package sign

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
)

// DeployedFile is the file of the deployment records in the set.
const DeployedFile = "keys/deployed.yaml"

// KindKeyDeployments is the kind of keys/deployed.yaml.
const KindKeyDeployments = "KeyDeployments"

// Deployments is keys/deployed.yaml (signing.html#deployed-file): one
// deployment record per key and target, and the removal records.
type Deployments struct {
	APIVersion  string       `yaml:"apiVersion" json:"apiVersion"`
	Kind        string       `yaml:"kind" json:"kind"`
	Deployments []Deployment `yaml:"deployments" json:"deployments"`
	Removals    []Removal    `yaml:"removals,omitempty" json:"removals,omitempty"`
}

// Deployment is one entry of deployments: a release record (release) or a
// key file record (keyfile and commit).
type Deployment struct {
	Key         string `yaml:"key" json:"key"`
	Target      string `yaml:"target" json:"target"`
	Fingerprint string `yaml:"fingerprint" json:"fingerprint"`
	// Release is the NVR of the release package with the dist tag.
	Release string `yaml:"release,omitempty" json:"release,omitempty"`
	// Keyfile and Commit are a key file record: the sha256 of the key file
	// that config management installed, and the set commit of that file.
	Keyfile     string `yaml:"keyfile,omitempty" json:"keyfile,omitempty"`
	Commit      string `yaml:"commit,omitempty" json:"commit,omitempty"`
	ConfirmedBy string `yaml:"confirmedBy" json:"confirmedBy"`
	At          string `yaml:"at" json:"at"`
	Note        string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Removal is one entry of removals: config management removed the key from
// all clients of the target.
type Removal struct {
	Key         string `yaml:"key" json:"key"`
	Target      string `yaml:"target" json:"target"`
	Fingerprint string `yaml:"fingerprint" json:"fingerprint"`
	ConfirmedBy string `yaml:"confirmedBy" json:"confirmedBy"`
	At          string `yaml:"at" json:"at"`
	Note        string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Time gives the time of the record.
func (d Deployment) Time() time.Time { return parseAt(d.At) }

// Time gives the time of the record.
func (r Removal) Time() time.Time { return parseAt(r.At) }

func parseAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// FormatAt gives the time of a record as the file writes it:
// 2026-11-05T09:12:00Z.
func FormatAt(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// KeyfileRecord reports a key file record.
func (d Deployment) KeyfileRecord() bool { return d.Release == "" && d.Keyfile != "" }

// API gives the record in the form of the output schema.
func (d Deployment) API() api.KeyDeployment {
	return api.KeyDeployment{
		Release: d.Release, Keyfile: d.Keyfile, Commit: d.Commit,
		ConfirmedBy: d.ConfirmedBy, At: api.FormatTime(d.Time()), Note: d.Note,
	}
}

// API gives the record in the form of the output schema.
func (r Removal) API() api.KeyRemoval {
	return api.KeyRemoval{ConfirmedBy: r.ConfirmedBy, At: api.FormatTime(r.Time()), Note: r.Note}
}

// NewDeployments gives an empty file.
func NewDeployments() *Deployments {
	return &Deployments{APIVersion: api.APIVersion, Kind: KindKeyDeployments, Deployments: []Deployment{}}
}

// ParseDeployments reads keys/deployed.yaml. Two entries of one key and
// target (after a git merge) keep the newest at.
func ParseDeployments(data []byte, path string) (*Deployments, error) {
	d := NewDeployments()
	if len(bytes.TrimSpace(data)) == 0 {
		return d, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(d); err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", path, strings.TrimPrefix(err.Error(), "yaml: ")).WithPath(path).
			WithHint("correct the file; after a git conflict keep the entries of both sides", "repoctl schema deployments")
	}
	if d.Kind != KindKeyDeployments {
		return nil, api.Errorf(api.PConfigInvalid, "%s: kind %q, want %s", path, d.Kind, KindKeyDeployments).WithPath(path)
	}
	known := false
	for _, v := range api.ReadAPIVersions {
		known = known || v == d.APIVersion
	}
	if !known {
		return nil, api.Errorf(api.PContractNewerFile, "%s: apiVersion %s is newer than this repoctl", path, d.APIVersion).WithPath(path).
			WithHint("upgrade repoctl on this host", "repoctl help upgrade")
	}
	for i, e := range d.Deployments {
		where := fmt.Sprintf("%s: deployments[%d]", path, i)
		switch {
		case e.Key == "" || e.Target == "":
			return nil, api.Errorf(api.PConfigInvalid, "%s: key and target are required", where).WithPath(path)
		case !IsFingerprint(e.Fingerprint):
			return nil, api.Errorf(api.PConfigInvalid, "%s: fingerprint %q is not a full fingerprint", where, e.Fingerprint).WithPath(path)
		case (e.Release == "") == (e.Keyfile == ""):
			return nil, api.Errorf(api.PConfigInvalid, "%s: give exactly one of release and keyfile", where).WithPath(path)
		case e.Keyfile != "" && e.Commit == "":
			return nil, api.Errorf(api.PConfigInvalid, "%s: a key file record needs commit", where).WithPath(path)
		case e.Time().IsZero():
			return nil, api.Errorf(api.PConfigInvalid, "%s: at %q is not an RFC 3339 time", where, e.At).WithPath(path)
		}
	}
	for i, e := range d.Removals {
		if e.Key == "" || e.Target == "" || !IsFingerprint(e.Fingerprint) || e.Time().IsZero() {
			return nil, api.Errorf(api.PConfigInvalid, "%s: removals[%d]: key, target, a full fingerprint and at are required", path, i).WithPath(path)
		}
	}
	d.normalize()
	return d, nil
}

// ReadDeployments reads keys/deployed.yaml of the set directory. A missing
// file is an empty file.
func ReadDeployments(dir string) (*Deployments, error) {
	p := filepath.Join(dir, DeployedFile)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewDeployments(), nil
		}
		return nil, err
	}
	return ParseDeployments(data, DeployedFile)
}

// normalize keeps the newest entry of each key and target and sorts the
// entries by key, then target.
func (d *Deployments) normalize() {
	type kt struct{ k, t string }
	keepD := map[kt]Deployment{}
	for _, e := range d.Deployments {
		if old, ok := keepD[kt{e.Key, e.Target}]; !ok || !e.Time().Before(old.Time()) {
			keepD[kt{e.Key, e.Target}] = e
		}
	}
	d.Deployments = d.Deployments[:0]
	for _, e := range keepD {
		d.Deployments = append(d.Deployments, e)
	}
	sort.Slice(d.Deployments, func(i, j int) bool {
		a, b := d.Deployments[i], d.Deployments[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Target < b.Target
	})
	keepR := map[kt]Removal{}
	for _, e := range d.Removals {
		if old, ok := keepR[kt{e.Key, e.Target}]; !ok || !e.Time().Before(old.Time()) {
			keepR[kt{e.Key, e.Target}] = e
		}
	}
	d.Removals = d.Removals[:0]
	for _, e := range keepR {
		d.Removals = append(d.Removals, e)
	}
	sort.Slice(d.Removals, func(i, j int) bool {
		a, b := d.Removals[i], d.Removals[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Target < b.Target
	})
	if d.Deployments == nil {
		d.Deployments = []Deployment{}
	}
	if len(d.Removals) == 0 {
		d.Removals = nil
	}
}

// Find gives the record of the key and the target.
func (d *Deployments) Find(key, target string) *Deployment {
	for i := range d.Deployments {
		if d.Deployments[i].Key == key && d.Deployments[i].Target == target {
			return &d.Deployments[i]
		}
	}
	return nil
}

// FindRemoval gives the removal record of the key and the target.
func (d *Deployments) FindRemoval(key, target string) *Removal {
	for i := range d.Removals {
		if d.Removals[i].Key == key && d.Removals[i].Target == target {
			return &d.Removals[i]
		}
	}
	return nil
}

// Put adds a record; it replaces the entry of the same key and target.
func (d *Deployments) Put(e Deployment) {
	d.Withdraw(e.Key, e.Target)
	d.Deployments = append(d.Deployments, e)
	d.normalize()
}

// Withdraw removes the record of the key and the target, and reports
// whether there was one.
func (d *Deployments) Withdraw(key, target string) bool {
	found := false
	out := d.Deployments[:0]
	for _, e := range d.Deployments {
		if e.Key == key && e.Target == target {
			found = true
			continue
		}
		out = append(out, e)
	}
	d.Deployments = out
	return found
}

// PutRemoval adds a removal record; it replaces the entry of the same key
// and target.
func (d *Deployments) PutRemoval(r Removal) {
	out := d.Removals[:0]
	for _, e := range d.Removals {
		if e.Key != r.Key || e.Target != r.Target {
			out = append(out, e)
		}
	}
	d.Removals = append(out, r)
	d.normalize()
}

// Marshal gives the file text: the header comment, then the entries sorted
// by key and target.
func (d *Deployments) Marshal() ([]byte, error) {
	d.normalize()
	if d.APIVersion == "" {
		d.APIVersion = api.APIVersion
	}
	d.Kind = KindKeyDeployments
	var b bytes.Buffer
	b.WriteString("# Written by repoctl sign deployed and sign removed. One entry per key and\n# target. After a git conflict, keep the entries of both sides.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Write writes keys/deployed.yaml into the set directory.
func (d *Deployments) Write(dir string) error {
	data, err := d.Marshal()
	if err != nil {
		return err
	}
	p := filepath.Join(dir, DeployedFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// API gives the file in the form of the output schema (KeyDeployments):
// a release record with its release, a key file record with its keyfile
// and commit.
func (d *Deployments) API() api.KeyDeployments {
	out := api.KeyDeployments{APIVersion: d.APIVersion, Kind: d.Kind}
	for _, e := range d.Deployments {
		out.Deployments = append(out.Deployments, api.KeyDeploymentEntry{
			Key: e.Key, Target: e.Target, Fingerprint: e.Fingerprint, Release: e.Release,
			Keyfile: e.Keyfile, Commit: e.Commit, ConfirmedBy: e.ConfirmedBy, At: e.At, Note: e.Note,
		})
	}
	for _, r := range d.Removals {
		out.Removals = append(out.Removals, api.KeyRemovalEntry{
			Key: r.Key, Target: r.Target, Fingerprint: r.Fingerprint, ConfirmedBy: r.ConfirmedBy, At: r.At, Note: r.Note,
		})
	}
	return out
}
