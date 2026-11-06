package sign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// SignerItem is one key of SignerList: the Signer of the contract with its
// expiry, its deployment records, its RPMs in latest and its removal
// records.
type SignerItem struct {
	UID         string `json:"uid"`
	Fingerprint string `json:"fingerprint"`
	KeyID       string `json:"keyId"`
	Backend     string `json:"backend,omitempty"`
	State       string `json:"state,omitempty"`
	// Deployed is true when gate 2 passes for each target in scope that
	// has a latest.
	Deployed    bool                         `json:"deployed"`
	Expires     api.Time                     `json:"expires,omitempty"`
	Deployments map[string]api.KeyDeployment `json:"deployments"`
	RPMs        map[string]int               `json:"rpms"`
	Removals    map[string]api.KeyRemoval    `json:"removals,omitempty"`
}

// View is what the sign reports read: the trust set, the deployment
// records and the latest of each target in scope.
type View struct {
	Set         *config.Set
	Dir         string
	Trust       *Trust
	Deployments *Deployments
	// Targets is the scope: the selected active targets.
	Targets []string
	// Trees gives latest of each target (the local copy). A tree that does
	// not exist is a target without a latest.
	Trees map[string]*Tree
	// Current is the key of sign.key, nil when it is not set.
	Current   *Signer
	KeyfileAt KeyfileAt
	Now       time.Time
	// MockDir is the directory of the mock roots (sign.verify-el).
	MockDir string
}

// NewView reads the trust set, keys/deployed.yaml and the local copies of
// the targets.
func NewView(set *config.Set, dir string, paths config.Paths, targets []string) (*View, error) {
	d, err := ReadDeployments(dir)
	if err != nil {
		return nil, err
	}
	v := &View{Set: set, Dir: dir, Trust: LoadTrust(set, dir), Deployments: d, Targets: targets,
		Trees: map[string]*Tree{}, KeyfileAt: KeyfileAtCommit(dir, set), Now: time.Now(), MockDir: paths.MockDir}
	for _, t := range targets {
		v.Trees[t] = LocalTree(paths, set, t)
	}
	return v, nil
}

// latest gives the tree of a target with a latest, else nil.
func (v *View) latest(target string) *Tree {
	if t := v.Trees[target]; t != nil && t.Exists() {
		return t
	}
	return nil
}

// HasLatest reports whether any target in scope has a latest.
func (v *View) HasLatest() bool {
	for _, t := range v.Targets {
		if v.latest(t) != nil {
			return true
		}
	}
	return false
}

// RecordPasses checks the record of the key for one target against its
// latest.
func (v *View) RecordPasses(ctx context.Context, k *TrustedKey, target string) (bool, string) {
	var base TargetBase
	if t := v.latest(target); t != nil {
		base = t
	}
	return RecordPasses(ctx, v.Deployments.Find(k.ID, target), k.Fingerprint, base, v.KeyfileAt)
}

// Deployed reports gate 2 of the key for the targets in scope that have a
// latest.
func (v *View) Deployed(ctx context.Context, k *TrustedKey) bool {
	for _, t := range v.Targets {
		if v.latest(t) == nil {
			continue
		}
		if ok, _ := v.RecordPasses(ctx, k, t); !ok {
			return false
		}
	}
	return true
}

// RPMs gives the RPMs in latest of a target that the key signed.
func (v *View) RPMs(k *TrustedKey, target string) int {
	t := v.latest(target)
	if t == nil {
		return 0
	}
	return t.SignerCounts()[k.Fingerprint]
}

// Item gives the SignerList entry of a key.
func (v *View) Item(ctx context.Context, k *TrustedKey) SignerItem {
	it := SignerItem{
		UID: k.UID(), Fingerprint: k.Fingerprint, KeyID: k.ID, State: k.State,
		Deployed:    v.Deployed(ctx, k),
		Deployments: map[string]api.KeyDeployment{}, RPMs: map[string]int{},
	}
	if v.Current != nil && v.Current.FPR == k.Fingerprint {
		it.Backend = v.Current.Backend
	}
	if k.Cert != nil && !k.Cert.Expires.IsZero() {
		it.Expires = api.FormatTime(k.Cert.Expires)
	}
	for _, t := range v.Targets {
		if d := v.Deployments.Find(k.ID, t); d != nil {
			it.Deployments[t] = d.API()
		}
		it.RPMs[t] = v.RPMs(k, t)
		if k.State == config.KeyRemoved || k.State == config.KeyRevoked {
			if r := v.Deployments.FindRemoval(k.ID, t); r != nil {
				if it.Removals == nil {
					it.Removals = map[string]api.KeyRemoval{}
				}
				it.Removals[t] = r.API()
			}
		}
	}
	return it
}

// SignerList gives the result of sign ls: each key of sign.keys in file
// order.
func (v *View) SignerList(ctx context.Context) api.SignerList {
	l := api.SignerList{Keys: []any{}}
	if v.Current != nil {
		l.Current = v.Current.KeyID()
	}
	for _, k := range v.Trust.Keys {
		l.Keys = append(l.Keys, v.Item(ctx, k))
	}
	return l
}

// keyfileOfTree gives the root key file of latest of a target.
func (v *View) keyfileOfTree(target string) ([]byte, bool) {
	t := v.latest(target)
	if t == nil {
		return nil, false
	}
	data, err := t.RootKeyfile()
	return data, err == nil
}

// treeKeyfile gives the key file of the in-tree release package and its
// path.
func (v *View) treeKeyfile() ([]byte, string) {
	p, ok := v.Set.ReleasePackage()
	if !ok {
		return nil, ""
	}
	path := filepath.Join(p.Path, KeyfileName(v.Set))
	data, _ := os.ReadFile(filepath.Join(v.Dir, path))
	return data, path
}

var specVR = regexp.MustCompile(`(?m)^(Version|Release):[ \t]*(\S+)`)

// nextRelease gives N-V-R of the spec of the release package in the set,
// without the dist tag: the release that the next build gives.
func (v *View) nextRelease() string {
	p, ok := v.Set.ReleasePackage()
	if !ok {
		return "the release package"
	}
	m, _ := filepath.Glob(filepath.Join(v.Dir, p.Path, "*.spec"))
	if len(m) == 0 {
		return p.ID
	}
	data, err := os.ReadFile(m[0])
	if err != nil {
		return p.ID
	}
	ver, rel := "", ""
	for _, x := range specVR.FindAllSubmatch(data, -1) {
		if string(x[1]) == "Version" && ver == "" {
			ver = string(x[2])
		}
		if string(x[1]) == "Release" && rel == "" {
			rel = string(x[2])
		}
	}
	rel = strings.TrimSuffix(rel, "%{?dist}")
	if ver == "" || rel == "" {
		return p.ID
	}
	return p.ID + "-" + ver + "-" + rel
}

// StripDist gives an NVR without the dist tag: slurm-release-26.05-2.el9
// gives slurm-release-26.05-2.
func StripDist(nvr string) string {
	i := strings.LastIndex(nvr, "-")
	if i < 0 {
		return nvr
	}
	if j := strings.Index(nvr[i:], "."); j > 0 {
		return nvr[:i+j]
	}
	return nvr
}

// Onboard checks the five onboarding steps of a key
// (signing.html#onboarding). A key that sign.keys does not have gives a
// report with step 1 open.
func (v *View) Onboard(ctx context.Context, id string) api.OnboardReport {
	r := api.OnboardReport{Key: id}
	k := v.Trust.Key(id)
	step := func(n int, name string, ok bool, detail string) {
		r.Steps = append(r.Steps, api.OnboardReportStep{N: n, Name: name, Ok: ok, Detail: detail})
	}
	next := func(cmd string) {
		if r.Next == "" {
			r.Next = cmd
		}
	}
	if k == nil {
		step(1, "key in sign.keys, state active", false, "sign.keys has no key "+id)
		step(2, "key in the key file", false, "")
		step(3, "release with the key is published", false, "")
		step(4, "deployed on all clients", false, "no record")
		step(5, "GHCR write access", false, "")
		r.Next = "repoctl sign add " + id + " --fingerprint FPR"
		return r
	}
	// 1: in sign.keys, active, the set commit pushed.
	commit := LastCommit(ctx, v.Dir, k.Path())
	d1 := ""
	ok1 := k.State == config.KeyActive
	switch {
	case !ok1:
		d1 = "state " + k.State
		if k.State == config.KeyRetiring {
			next("repoctl sign activate " + id)
		} else {
			next("repoctl sign add NEW-ID --fingerprint FPR")
		}
	case commit == "":
		ok1 = false
		d1 = k.Path() + " is not committed"
		next("repoctl sign add " + id + " --fingerprint " + k.Fingerprint)
	case !Pushed(ctx, v.Dir, v.Set, commit):
		ok1 = false
		d1 = "set commit " + api.ShortCommit(commit) + " is not pushed"
		next("git push " + v.Set.Remote() + " set/" + v.Set.Name)
	default:
		d1 = "set commit " + api.ShortCommit(commit) + " (pushed)"
	}
	step(1, "key in sign.keys, state active", ok1, d1)
	// 2: in the key file of the release package.
	kf, path := v.treeKeyfile()
	ok2 := path != "" && KeyfileHolds(kf, k.Fingerprint)
	d2 := path
	if !ok2 {
		if path == "" {
			d2 = "the set has no release package"
		} else {
			d2 = path + " does not hold the key"
		}
		next("repoctl release write")
	}
	step(2, "key in the key file", ok2, d2)
	// 3: a release with the key is published (or a key file record).
	var with, without []string
	releases := map[string][]string{}
	for _, t := range v.Targets {
		tree := v.latest(t)
		if tree == nil {
			continue
		}
		if d := v.Deployments.Find(k.ID, t); d != nil && d.KeyfileRecord() {
			if ok, _ := v.RecordPasses(ctx, k, t); ok {
				with = append(with, t)
				continue
			}
		}
		data, _ := v.keyfileOfTree(t)
		rel := StripDist(tree.NewestRelease())
		if KeyfileHolds(data, k.Fingerprint) {
			with = append(with, t)
		} else {
			without = append(without, t)
		}
		releases[rel] = append(releases[rel], t)
	}
	d3 := latestReleases(releases)
	if len(with)+len(without) == 0 {
		d3 = "no target has a latest; the first publish ships the key file"
	}
	ok3 := len(without) == 0
	if !ok3 {
		next("an admin with a deployed key builds and publishes " + v.nextRelease())
	}
	step(3, "release with the key is published", ok3, d3)
	// 4: deployed on all clients.
	var have, missing []string
	for _, t := range v.Targets {
		if v.latest(t) == nil {
			continue
		}
		if ok, _ := v.RecordPasses(ctx, k, t); ok {
			have = append(have, t)
		} else {
			missing = append(missing, t)
		}
	}
	ok4 := len(missing) == 0
	d4 := "no record"
	switch {
	case len(have) > 0 && ok4:
		d4 = "record for " + strings.Join(have, ", ")
	case len(have) > 0:
		d4 = "record for " + strings.Join(have, ", ") + "; none for " + strings.Join(missing, ", ")
	case len(have)+len(missing) == 0:
		d4 = "no target has a latest"
	}
	if !ok4 {
		rel := "NVR"
		if t := v.latest(missing[0]); t != nil && KeyfileHolds(mustKeyfile(t), k.Fingerprint) {
			rel = StripDist(t.NewestRelease())
		}
		next(fmt.Sprintf("repoctl sign deployed %s --release %s --target %s", id, rel, missing[0]))
	}
	step(4, "deployed on all clients", ok4, d4)
	// 5: GHCR write access: the key signed a snapshot of a latest.
	ok5 := false
	for _, t := range v.Targets {
		if tree := v.latest(t); tree != nil && tree.Signers()[k.Fingerprint] {
			ok5 = true
		}
	}
	d5 := id + " pushed a snapshot"
	if !ok5 {
		d5 = id + " has not pushed a snapshot yet; repoctl doctor checks registry.write on its host"
		next("on the host of " + id + ": repoctl doctor --only registry")
	}
	step(5, "GHCR write access", ok5, d5)
	return r
}

func mustKeyfile(t *Tree) []byte {
	data, _ := t.RootKeyfile()
	return data
}

// latestReleases gives "latest has slurm-release-26.05-2 (el9, el10)".
func latestReleases(m map[string][]string) string {
	var parts []string
	for _, rel := range sortedKeys(m) {
		name := rel
		if name == "" {
			name = "no release package"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, strings.Join(m[rel], ", ")))
	}
	return "latest has " + strings.Join(parts, ", ")
}

// ClientCommands gives the commands that config management runs on each
// client to remove the key (signing.html#offboarding).
func (v *View) ClientCommands(k *TrustedKey) []api.OnboardReportClientCommand {
	id := ""
	if k.Cert != nil {
		id = k.Cert.RPMKeyID()
	} else if len(k.Fingerprint) == 40 {
		id = strings.ToLower(k.Fingerprint[32:])
	}
	var out []api.OnboardReportClientCommand
	if k.State == config.KeyRevoked {
		kf, _ := v.treeKeyfile()
		out = append(out, api.OnboardReportClientCommand{OS: "all",
			Command: fmt.Sprintf("install %s with sha256 %s", KeyfileName(v.Set), Sha256Hex(kf)),
			Note:    "repoctl sign keyfile --tree --checksum"})
	}
	out = append(out,
		api.OnboardReportClientCommand{OS: "EL9", Command: "rpm -e gpg-pubkey-" + id},
		api.OnboardReportClientCommand{OS: "EL10", Command: "rpmkeys --erase " + id, Note: "EL10.1 and later; else use the EL9 line"},
		api.OnboardReportClientCommand{OS: "both", Command: "rm -rf /var/cache/dnf/" + v.Set.Repo.Name + "*/pubring"},
	)
	return out
}

// Offboard checks the six offboarding steps of a key
// (signing.html#offboarding), or for a revoked key the order after a key
// compromise (signing.html#compromise). It gives the client commands and,
// for a revoked key, the snapshots of latest with files of the key.
func (v *View) Offboard(ctx context.Context, id string) (api.OnboardReport, error) {
	r := api.OnboardReport{Key: id}
	k := v.Trust.Key(id)
	if k == nil {
		return r, api.Errorf(api.PSignWrongState, "sign offboard %s: sign.keys has no key %s", id, id).
			WithHint("list the keys", "repoctl sign ls")
	}
	step := func(n int, name string, ok bool, detail string) {
		r.Steps = append(r.Steps, api.OnboardReportStep{N: n, Name: name, Ok: ok, Detail: detail})
	}
	next := func(cmd string) {
		if r.Next == "" {
			r.Next = cmd
		}
	}
	// The facts of latest per target.
	var withRPMs []string
	rpmText := map[string]int{}
	var holds, noRemoval []string
	anyLatest := false
	for _, t := range v.Targets {
		tree := v.latest(t)
		if tree == nil {
			continue
		}
		anyLatest = true
		if n := tree.SignerCounts()[k.Fingerprint]; n > 0 {
			withRPMs = append(withRPMs, t)
			rpmText[t] = n
		}
		if data, ok := v.keyfileOfTree(t); ok && KeyfileHolds(data, k.Fingerprint) {
			holds = append(holds, t)
		}
		if v.Deployments.FindRemoval(k.ID, t) == nil {
			noRemoval = append(noRemoval, t)
		}
	}
	resignDetail := "no RPM in latest carries the key"
	if len(withRPMs) > 0 {
		var parts []string
		for _, t := range withRPMs {
			parts = append(parts, fmt.Sprintf("%s in latest of %s", plural(rpmText[t], "RPM"), t))
		}
		resignDetail = strings.Join(parts, ", ") + " " + Agree(len(parts)*rpmText[withRPMs[0]], "carries", "carry") + " " + id
	}
	holdsDetail := "the key file in latest does not hold the key"
	if len(holds) > 0 {
		holdsDetail = "the key file in latest of " + strings.Join(holds, ", ") + " holds the key"
	}
	removalDetail := "removal record for each target"
	if len(noRemoval) > 0 {
		removalDetail = "no removal record for " + strings.Join(noRemoval, ", ")
	}
	if !anyLatest {
		// A target without a latest has no clients.
		resignDetail = "no target has a latest"
		holdsDetail = "no target has a latest"
		removalDetail = "no target has a latest"
	}
	r.ClientCommands = v.ClientCommands(k)
	if k.State == config.KeyRevoked {
		kf, path := v.treeKeyfile()
		ok2 := !KeyfileHolds(kf, k.Fingerprint)
		step(1, "stop the access: GitHub team and registry tokens", true, "not checked by repoctl")
		step(2, "state revoked, key file without the key", ok2, path)
		step(3, "key file on the HTTP server and all clients", len(noRemoval) == 0, removalDetail)
		snaps := v.signedSnapshots(ctx, k)
		r.SignedSnapshots = snaps
		step(4, "check the publishes", len(withRPMs) == 0, fmt.Sprintf("%s of latest with files of %s", plural(len(snaps), "snapshot"), id))
		step(5, "re-sign its RPMs", len(withRPMs) == 0, resignDetail)
		step(6, "release package without the key", len(holds) == 0, holdsDetail)
		switch {
		case !ok2:
			next("repoctl release write")
		case len(withRPMs) > 0:
			next("check each publish, then repoctl repo resign --signer " + id + " --target " + withRPMs[0])
		case len(holds) > 0:
			next("publish the release package of " + holds[0] + " with the next publish")
		case len(noRemoval) > 0:
			next(fmt.Sprintf("repoctl sign removed %s --target %s", id, noRemoval[0]))
		}
		return r, nil
	}
	notActive := k.State != config.KeyActive
	step(1, "GitHub team and token", notActive, "not checked by repoctl: an organization owner removes the human from the team")
	step(2, "state retiring", notActive, "state "+k.State)
	if !notActive {
		next("repoctl sign retire " + id)
	}
	step(3, "re-sign its RPMs", len(withRPMs) == 0, resignDetail)
	if len(withRPMs) > 0 {
		next("repoctl repo resign --signer " + id + " --target " + withRPMs[0])
	}
	removed := k.State == config.KeyRemoved
	step(4, "remove the key", removed, "state "+k.State)
	if !removed {
		next("repoctl sign remove " + id)
	}
	step(5, "publish the new release", removed && len(holds) == 0, holdsDetail)
	if len(holds) > 0 {
		next("an admin with a deployed key builds and publishes " + v.nextRelease() + " for " + holds[0])
	}
	step(6, "remove the key on the clients", removed && len(noRemoval) == 0 && len(holds) == 0, removalDetail)
	if len(noRemoval) > 0 {
		next(fmt.Sprintf("run the client commands, then repoctl sign removed %s --target %s", id, noRemoval[0]))
	}
	return r, nil
}

// signedSnapshots lists latest of each target when it holds a file that the
// key signed. The registry holds more snapshots; repoctl reads latest here.
func (v *View) signedSnapshots(_ context.Context, k *TrustedKey) []api.OnboardReportSignedSnapshot {
	var out []api.OnboardReportSignedSnapshot
	for _, t := range v.Targets {
		tree := v.latest(t)
		if tree == nil {
			continue
		}
		n := tree.SignerCounts()[k.Fingerprint]
		repomd := false
		if data, err := os.ReadFile(filepath.Join(tree.Dir, v.Set.Repo.Name, "repodata", "repomd.xml.asc")); err == nil {
			if bin, err := Dearmor(data); err == nil {
				if sig, err := ParseSignature(bin); err == nil && k.Cert != nil && k.Cert.HasKey(sig.Issuer()) {
					repomd = true
				}
			}
		}
		if n == 0 && !repomd {
			continue
		}
		snap := tree.Snapshot()
		s := api.OnboardReportSignedSnapshot{Target: t, Publish: tree.PublishID(), RPMs: n, Repomd: repomd, Latest: true}
		if snap != nil {
			s.Digest, s.User, s.Host = snap.Digest, snap.User, snap.Host
		}
		if s.Digest == "" {
			// The contract needs the digest; without snapshot.json the
			// local copy cannot give it.
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Publish > out[j].Publish })
	return out
}
