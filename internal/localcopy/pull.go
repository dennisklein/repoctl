package localcopy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/publish/tree"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// Keys gives the keys of sign.keys (all states) with their files below the
// set directory.
func Keys(set *config.Set, setDir string) []tree.Key {
	var out []tree.Key
	for _, k := range set.Sign.Keys {
		f := k.File
		if !filepath.IsAbs(f) {
			f = filepath.Join(setDir, f)
		}
		out = append(out, tree.Key{ID: k.ID, Fingerprint: strings.ToUpper(strings.ReplaceAll(k.Fingerprint, " ", "")), State: k.State, File: f})
	}
	return out
}

// PullOptions say what Pull does.
type PullOptions struct {
	Store *snapshot.Store
	// Ref is latest (the default), a publish id or a digest.
	Ref    string
	Set    string
	Target string
	// Dir is the local copy.
	Dir    string
	Layout tree.Layout
	// Keys are sign.keys, all states.
	Keys []tree.Key
	// Keyfile and Release name the root files (sign.release.keyfile and
	// the release package).
	Keyfile, Release string
	// User is the user of this host, for snapshot.orphaned.
	User string
	// Also are more trees whose files Pull links, for example the trees of
	// the publishes.
	Also []string
}

// PullResult is the result of a pull.
type PullResult struct {
	Snapshot *snapshot.Snapshot
	// Same is true when the local copy already was the snapshot.
	Same     bool
	Stats    snapshot.PullStats
	Verify   *tree.VerifyResult
	Warnings []api.Problem
	// Before is snapshot.json of the local copy before the pull.
	Before *api.Snapshot
}

// Pull makes the local copy equal to a snapshot: it downloads the missing
// blobs, checks their sha256, verifies each RPM and each repomd.xml.asc
// with sign.keys and writes the local copy in one rename, with
// snapshot.json. A file that does not verify stops the pull, which writes
// nothing (exit 10).
func Pull(ctx context.Context, o PullOptions) (*PullResult, error) {
	res := &PullResult{}
	before, err := ReadSnapshot(o.Dir)
	if err != nil {
		return nil, err
	}
	res.Before = before
	d, err := o.Store.Resolve(ctx, o.Ref)
	if err != nil {
		return nil, err
	}
	snap, err := o.Store.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	res.Snapshot = snap
	if snap.Meta.Set != o.Set || snap.Meta.Target != o.Target {
		return nil, api.Errorf(api.PVerifyFailed, "the snapshot %s is of %s %s, not of %s %s", snap.Digest(), snap.Meta.Set, snap.Meta.Target, o.Set, o.Target)
	}
	if before != nil && before.User == o.User && before.Digest != snap.Digest() && before.Kind != snapshot.KindImport {
		on, err := o.Store.OnChain(ctx, d, before.Digest)
		if err == nil && !on {
			res.Warnings = append(res.Warnings, api.Problem{Code: api.PSnapshotOrphaned, Target: o.Target,
				Message: fmt.Sprintf("the local copy holds %s of %s, which is not on the chain of %s", before.Publish, before.User, snap.Meta.Publish)})
		}
	}
	if before != nil && before.Digest == snap.Digest() {
		if ok, _ := Intact(o.Dir, snap); ok {
			res.Same = true
			return res, nil
		}
	}
	have, err := Have(append([]string{o.Dir}, o.Also...)...)
	if err != nil {
		return nil, err
	}
	tmp, err := Temp(o.Dir, "pull")
	if err != nil {
		return nil, err
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(tmp)
		}
	}()
	st, err := o.Store.Download(ctx, snap, tmp, have)
	if err != nil {
		return nil, err
	}
	res.Stats = st
	kr, err := tree.NewKeyring(ctx, o.Keys)
	if err != nil {
		return nil, err
	}
	defer kr.Close()
	vr, err := tree.Verify(ctx, tmp, tree.VerifyOptions{Layout: o.Layout, Keyring: kr, Keyfile: o.Keyfile, Release: o.Release, Records: true})
	if err != nil {
		return nil, err
	}
	res.Verify = vr
	if err := vr.Err(); err != nil {
		e, _ := api.AsError(err)
		e.Target = o.Target
		return res, e.WithHint("the local copy did not change. Check the snapshot", "repoctl repo verify --snapshot "+snap.Meta.Publish+" --target "+o.Target)
	}
	res.Warnings = append(res.Warnings, vr.Warnings...)
	for fpr, n := range vr.Signers {
		if k, ok := kr.ByFingerprint(fpr); ok && (k.State == config.KeyRemoved || k.State == config.KeyRevoked) {
			res.Warnings = append(res.Warnings, api.Problem{Code: api.PSignRemovedSigner, Target: o.Target,
				Message: fmt.Sprintf("%d RPMs of %s are signed by %s, a key in state %s", n, snap.Meta.Publish, k.ID, k.State),
				Hint:    api.HintText("give them a new signature", "repoctl repo resign --signer "+k.ID+" --target "+o.Target)})
		}
	}
	if err := WriteSnapshot(tmp, snap.API(o.Store.Ref, o.Layout.Name, keyIDOf(kr))); err != nil {
		return nil, err
	}
	if err := Install(tmp, o.Dir); err != nil {
		return nil, err
	}
	done = true
	return res, nil
}

// keyIDOf gives the sign.keys id of a fingerprint.
func keyIDOf(kr *tree.Keyring) func(string) string {
	return func(fpr string) string {
		if k, ok := kr.ByFingerprint(fpr); ok {
			return k.ID
		}
		return ""
	}
}

// Intact reports whether a local copy holds each file of a snapshot with
// its size. It does not hash the files.
func Intact(dir string, snap *snapshot.Snapshot) (bool, error) {
	for _, f := range snap.Files {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil || fi.Size() != f.Size {
			return false, err
		}
	}
	return true, nil
}
