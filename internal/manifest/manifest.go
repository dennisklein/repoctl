// Package manifest is the RepoManifest: the build state of one target repo
// (docs/design/model.html#manifest). It is in the repodata of the binary
// repository of each snapshot, added with modifyrepo_c --mdtype=repoctl, and
// lists each published build with its commit.
package manifest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/config/schema"
)

// Kind is the kind of the file.
const Kind = "RepoManifest"

// MDType is the repomd type of the manifest (modifyrepo_c --mdtype).
const MDType = "repoctl"

// Manifest is one RepoManifest.
type Manifest struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Set        string    `json:"set"`
	Target     string    `json:"target"`
	Publish    string    `json:"publish"`
	Builds     []Build   `json:"builds"`
	Removed    []Removed `json:"removed"`
	History    []History `json:"history"`
	Records    *Records  `json:"records,omitempty"`
}

// Build is one node build in the manifest.
type Build struct {
	Package string `json:"package"`
	Branch  string `json:"branch,omitempty"`
	// URL is set only for an external package.
	URL    *string `json:"url"`
	Commit string  `json:"commit"`
	// Reason is the plan reason: commit-changed, new-package, new-target,
	// rebuild: TEXT, adopted or adopted-unmapped.
	Reason   string   `json:"reason"`
	SRPM     string   `json:"srpm"`
	RPMs     []string `json:"rpms"`
	Provides []string `json:"provides,omitempty"`
	Requires []string `json:"requires,omitempty"`
	// MockConfig, Macros and Buildroot are "sha256:HEX".
	MockConfig    string   `json:"mockConfig,omitempty"`
	Macros        string   `json:"macros,omitempty"`
	Buildroot     string   `json:"buildroot,omitempty"`
	BuildrootFile string   `json:"buildrootFile,omitempty"`
	Run           string   `json:"run,omitempty"`
	Built         string   `json:"built,omitempty"`
	Publish       string   `json:"publish,omitempty"`
	Signer        string   `json:"signer,omitempty"`
	Records       *Records `json:"records,omitempty"`
	// Sources are the sha256 of the source files of the build (record
	// draft), by file name.
	Sources map[string]string `json:"sources,omitempty"`
}

// ReasonAdoptedUnmapped is the reason of an imported build that no commit
// gives. Its commit is null, and its package is null for a retired name.
const ReasonAdoptedUnmapped = "adopted-unmapped"

// MarshalJSON writes an empty package or commit as null, as an
// adopted-unmapped build has them.
func (b Build) MarshalJSON() ([]byte, error) {
	null := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	// The fields of Build in the same order, with package and commit as
	// pointers.
	return json.Marshal(struct {
		Package       *string           `json:"package"`
		Branch        string            `json:"branch,omitempty"`
		URL           *string           `json:"url"`
		Commit        *string           `json:"commit"`
		Reason        string            `json:"reason"`
		SRPM          string            `json:"srpm"`
		RPMs          []string          `json:"rpms"`
		Provides      []string          `json:"provides,omitempty"`
		Requires      []string          `json:"requires,omitempty"`
		MockConfig    string            `json:"mockConfig,omitempty"`
		Macros        string            `json:"macros,omitempty"`
		Buildroot     string            `json:"buildroot,omitempty"`
		BuildrootFile string            `json:"buildrootFile,omitempty"`
		Run           string            `json:"run,omitempty"`
		Built         string            `json:"built,omitempty"`
		Publish       string            `json:"publish,omitempty"`
		Signer        string            `json:"signer,omitempty"`
		Records       *Records          `json:"records,omitempty"`
		Sources       map[string]string `json:"sources,omitempty"`
	}{null(b.Package), b.Branch, b.URL, null(b.Commit), b.Reason, b.SRPM, b.RPMs, b.Provides, b.Requires,
		b.MockConfig, b.Macros, b.Buildroot, b.BuildrootFile, b.Run, b.Built, b.Publish, b.Signer, b.Records, b.Sources})
}

// Records are the supply chain records of a build or the repository.
type Records struct {
	Provenance *Record `json:"provenance,omitempty"`
	SBOM       *Record `json:"sbom,omitempty"`
}

// Record is one record file.
type Record struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Removed is an NVR that a prune or a restore removed.
type Removed struct {
	NVR     string            `json:"nvr"`
	SHA256  map[string]string `json:"sha256,omitempty"`
	Publish string            `json:"publish"`
	Reason  string            `json:"reason"`
}

// History is one publish of the chain.
type History struct {
	Publish   string     `json:"publish"`
	Kind      string     `json:"kind"`
	Parent    string     `json:"parent,omitempty"`
	User      string     `json:"user"`
	Host      string     `json:"host"`
	Signer    string     `json:"signer"`
	SetCommit string     `json:"setCommit,omitempty"`
	Created   string     `json:"created"`
	Accepted  []Accepted `json:"accepted,omitempty"`
	Held      []Held     `json:"held,omitempty"`
	Trust     *Trust     `json:"trust,omitempty"`
}

// Accepted is an accepted advisory of a publish.
type Accepted struct {
	ID          string `json:"id"`
	FactsDigest string `json:"factsDigest"`
	Reason      string `json:"reason"`
}

// Held is a held node of a publish.
type Held struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// Trust is the trust change of a publish.
type Trust struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// New gives an empty manifest of a target.
func New(set, target string) *Manifest {
	return &Manifest{APIVersion: api.APIVersion, Kind: Kind, Set: set, Target: target, Builds: []Build{}, Removed: []Removed{}, History: []History{}}
}

// Parse reads a manifest and checks its apiVersion and kind.
func Parse(data []byte) (*Manifest, error) {
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, api.Errorf(api.PManifestInvalid, "the RepoManifest is not JSON: %v", err)
	}
	if head.Kind != Kind {
		return nil, api.Errorf(api.PManifestInvalid, "the RepoManifest has kind %q", head.Kind)
	}
	known := false
	for _, v := range api.ReadAPIVersions {
		if v == head.APIVersion {
			known = true
		}
	}
	if !known {
		return nil, api.Errorf(api.PContractNewerFile, "the RepoManifest has apiVersion %s, which this repoctl cannot read", head.APIVersion).
			WithHint("update repoctl", "sudo dnf upgrade repoctl")
	}
	if err := schema.Validate("manifest", data); err != nil {
		return nil, api.Errorf(api.PManifestInvalid, "the RepoManifest does not agree with the schema manifest: %v", err).
			WithHint("print the schema", "repoctl schema manifest")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, api.Errorf(api.PManifestInvalid, "the RepoManifest does not agree with the schema: %v", err)
	}
	if m.Builds == nil {
		m.Builds = []Build{}
	}
	if m.Removed == nil {
		m.Removed = []Removed{}
	}
	if m.History == nil {
		m.History = []History{}
	}
	return &m, nil
}

// Marshal writes the manifest as indented JSON.
func (m *Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Find gives the newest build of the match key: the commit and the source
// package name (the Name of the SRPM). The target is the manifest's.
func (m *Manifest) Find(commit, srpmName string) (*Build, bool) {
	for i := len(m.Builds) - 1; i >= 0; i-- {
		b := &m.Builds[i]
		if b.Commit == commit && (srpmName == "" || NameOfSRPM(b.SRPM) == srpmName) {
			return b, true
		}
	}
	return nil, false
}

// Newest gives the newest build of a package id, else of the source
// package name. It is the base of the reason commit-changed.
func (m *Manifest) Newest(pkg, srpmName string) (*Build, bool) {
	for i := len(m.Builds) - 1; i >= 0; i-- {
		if m.Builds[i].Package == pkg {
			return &m.Builds[i], true
		}
	}
	if srpmName != "" {
		for i := len(m.Builds) - 1; i >= 0; i-- {
			if NameOfSRPM(m.Builds[i].SRPM) == srpmName {
				return &m.Builds[i], true
			}
		}
	}
	return nil, false
}

// NVR gives the name-version-release of an RPM file name.
func NVR(file string) string {
	f := strings.TrimSuffix(filepath.Base(file), ".rpm")
	if i := strings.LastIndex(f, "."); i > 0 {
		return f[:i]
	}
	return f
}

// NameOfSRPM gives the Name of an SRPM file name N-V-R.src.rpm.
func NameOfSRPM(file string) string {
	n, _, _ := SplitNVR(NVR(file))
	return n
}

// SplitNVR splits N-V-R.
func SplitNVR(nvr string) (name, version, release string) {
	i := strings.LastIndex(nvr, "-")
	if i < 0 {
		return nvr, "", ""
	}
	release = nvr[i+1:]
	rest := nvr[:i]
	j := strings.LastIndex(rest, "-")
	if j < 0 {
		return rest, "", release
	}
	return rest[:j], rest[j+1:], release
}

type repomd struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

// ReadRepo reads the manifest of an rpm-md repository directory: the data
// of type repoctl in repodata/repomd.xml. It gives nil without an error
// when the repository has no repomd.xml or no manifest.
func ReadRepo(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "repodata", "repomd.xml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var md repomd
	if err := xml.Unmarshal(b, &md); err != nil {
		return nil, api.Errorf(api.PManifestInvalid, "%s: repomd.xml is not valid: %v", dir, err)
	}
	for _, d := range md.Data {
		if d.Type != MDType {
			continue
		}
		f, err := os.Open(filepath.Join(dir, d.Location.Href))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var r io.Reader = f
		if strings.HasSuffix(d.Location.Href, ".gz") {
			zr, err := gzip.NewReader(f)
			if err != nil {
				return nil, err
			}
			r = zr
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return Parse(data)
	}
	return nil, nil
}

// Validate checks the rules between the fields that the decoder does not
// check.
func (m *Manifest) Validate() error {
	if m.Set == "" || m.Target == "" {
		return fmt.Errorf("manifest.invalid: set and target are required")
	}
	for _, b := range m.Builds {
		unmapped := b.Reason == ReasonAdoptedUnmapped
		if (b.Package == "" && !unmapped) || (b.Commit == "" && !unmapped) || b.SRPM == "" {
			return api.Errorf(api.PManifestInvalid, "a build of the RepoManifest of %s misses package, commit or srpm", m.Target)
		}
	}
	return nil
}

// LocalRepo gives the binary repository of the local copy of latest of a
// target: $XDG_DATA_HOME/repoctl/SET/repo/TARGET/REPO.
func LocalRepo(p config.Paths, set *config.Set, target string) string {
	return filepath.Join(p.LocalCopy(set.Name, target), set.Repo.Name)
}

// ReadLocal reads the manifest of the local copy of latest of a target. It
// gives nil without an error when the target has no local copy.
func ReadLocal(p config.Paths, set *config.Set, target string) (*Manifest, error) {
	return ReadRepo(LocalRepo(p, set, target))
}
