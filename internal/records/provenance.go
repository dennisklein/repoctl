// Package records writes and checks the supply chain records of a
// snapshot (docs/design/supply-chain.html): one SLSA provenance record (an
// in-toto Statement v1) and one CycloneDX 1.6 SBOM per build, and one SBOM
// per target repository. The records are files of the binary repository;
// the RepoManifest lists their sha256, and the signed repomd.xml covers the
// RepoManifest. Each subject and each SBOM hash is the sha256 of the
// signed file.
package records

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// The paths of the records in the binary repository.
const (
	Dir                = "records"
	RepositorySBOMPath = "records/repository.cdx.json"
	BuildrootDir       = "buildroot"
)

// ProvenancePath gives the path of the provenance record of a build NVR.
func ProvenancePath(nvr string) string { return path.Join(Dir, nvr, "provenance.json") }

// SBOMPath gives the path of the SBOM of a build NVR.
func SBOMPath(nvr string) string { return path.Join(Dir, nvr, "sbom.cdx.json") }

// BuildrootPath gives the path of the buildroot file of a build NVR.
func BuildrootPath(nvr string) string { return path.Join(BuildrootDir, nvr+".json") }

// The identifiers of the record (supply-chain.html#build-type, #builder).
const (
	StatementType  = "https://in-toto.io/Statement/v1"
	PredicateType  = "https://slsa.dev/provenance/v1"
	BuildType      = "https://dennisklein.github.io/repoctl/design/supply-chain.html#build-type"
	BuilderID      = "https://dennisklein.github.io/repoctl/design/supply-chain.html#builder"
	headerDigest   = "SHA256HEADER"
	digestSHA256   = "sha256"
	digestGitCommt = "gitCommit"
)

// Statement is an in-toto Statement v1 with the SLSA provenance v1
// predicate.
type Statement struct {
	Type          string     `json:"_type"`
	Subject       []Resource `json:"subject"`
	PredicateType string     `json:"predicateType"`
	Predicate     Predicate  `json:"predicate"`
}

// Resource is an in-toto ResourceDescriptor.
type Resource struct {
	Name        string            `json:"name,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Digest      map[string]string `json:"digest,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Predicate is the SLSA provenance v1 predicate.
type Predicate struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

// BuildDefinition is predicate.buildDefinition.
type BuildDefinition struct {
	BuildType            string            `json:"buildType"`
	ExternalParameters   map[string]string `json:"externalParameters"`
	InternalParameters   map[string]string `json:"internalParameters"`
	ResolvedDependencies []Resource        `json:"resolvedDependencies"`
}

// RunDetails is predicate.runDetails.
type RunDetails struct {
	Builder    Builder    `json:"builder"`
	Metadata   RunMeta    `json:"metadata"`
	Byproducts []Resource `json:"byproducts,omitempty"`
}

// Builder is runDetails.builder.
type Builder struct {
	ID      string            `json:"id"`
	Version map[string]string `json:"version,omitempty"`
}

// RunMeta is runDetails.metadata.
type RunMeta struct {
	InvocationID string `json:"invocationId"`
	StartedOn    string `json:"startedOn,omitempty"`
	FinishedOn   string `json:"finishedOn,omitempty"`
}

// Facts are the run facts of one node build (supply-chain.html#capture).
type Facts struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Mock is the mock version.
	Mock string `json:"mock,omitempty"`
	// MockConfig is the name of the mock config of the target.
	MockConfig string `json:"mockConfig,omitempty"`
	Repoctl    string `json:"repoctl,omitempty"`
	User       string `json:"user,omitempty"`
	Host       string `json:"host,omitempty"`
}

// Subject is one signed file of a build: its file name and sha256.
type Subject struct {
	Name   string
	SHA256 string
}

// BuildInput is what the records of one build come from.
type BuildInput struct {
	Set    string
	Target string
	// Namespace is the purl namespace and the supplier: the owner in
	// store.oci, for example gsi-hpc.
	Namespace string
	// Repo is the git repository of the source: the set repository or
	// git.url of an external package.
	Repo  string
	Build manifest.Build
	Facts Facts
	// Subjects are the SRPM, the binary RPMs and the debuginfo RPMs of the
	// build with the sha256 of the signed files.
	Subjects []Subject
	// SourceURIs gives the URL of each source file, by file name.
	SourceURIs map[string]string
	// Buildroot is the buildroot file of the build, if any, and its sha256.
	Buildroot       *BuildrootFile
	BuildrootSHA256 string
	// SetRPM gives, for a buildroot RPM that comes from the set, where it
	// comes from ("latest p-…" or "staging r-…"); "" for others.
	SetRPM func(nevra string) string
	// Distro is the vendor of the EL buildroot RPMs in a purl, for
	// example rocky.
	Distro string
}

// BuildrootFile is the buildroot file of a build: buildroot_lock.json of
// mock with the header digest and the signer of each RPM
// (build.html#buildroot-facts).
type BuildrootFile struct {
	Version   string `json:"version"`
	Buildroot struct {
		RPMs []BuildrootRPM `json:"rpms"`
	} `json:"buildroot"`
	Bootstrap struct {
		ImageDigest string `json:"image_digest"`
	} `json:"bootstrap"`
}

// BuildrootRPM is one RPM of the build root.
type BuildrootRPM struct {
	Name    string  `json:"name"`
	Epoch   *string `json:"epoch"`
	Version string  `json:"version"`
	Release string  `json:"release"`
	Arch    string  `json:"arch"`
	License string  `json:"license"`
	Sigmd5  string  `json:"sigmd5"`
	URL     string  `json:"url"`
	// HeaderDigest and Signer are the buildroot facts of repoctl. The other
	// spellings are read too.
	HeaderDigest string  `json:"sha256header,omitempty"`
	HeaderAlt    string  `json:"headerDigest,omitempty"`
	Signer       *string `json:"signer,omitempty"`
}

// NEVRA gives name-epoch:version-release.arch.
func (r BuildrootRPM) NEVRA() string {
	e := "0"
	if r.Epoch != nil && *r.Epoch != "" {
		e = *r.Epoch
	}
	return fmt.Sprintf("%s-%s:%s-%s.%s", r.Name, e, r.Version, r.Release, r.Arch)
}

// Header gives the header digest.
func (r BuildrootRPM) Header() string {
	if r.HeaderDigest != "" {
		return r.HeaderDigest
	}
	return r.HeaderAlt
}

// ParseBuildroot reads a buildroot file.
func ParseBuildroot(b []byte) (*BuildrootFile, error) {
	var f BuildrootFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("the buildroot file is not valid: %w", err)
	}
	return &f, nil
}

// Purl gives the purl of an RPM of the set: pkg:rpm/NS/NAME@V-R?arch=A&distro=T.
func Purl(ns, name, version, release, arch, distro string) string {
	p := fmt.Sprintf("pkg:rpm/%s/%s@%s-%s?arch=%s", ns, name, version, release, arch)
	if distro != "" {
		p += "&distro=" + distro
	}
	return p
}

// NewProvenance gives the provenance record of a build.
func NewProvenance(in BuildInput) Statement {
	b := in.Build
	src := sourceURI(in.Repo, b.Branch)
	ext := map[string]string{
		"set": in.Set, "target": in.Target, "package": b.Package, "source": src,
		"commit": b.Commit, "reason": b.Reason, "mockConfig": in.Facts.MockConfig,
	}
	intl := map[string]string{"mockConfig": b.MockConfig, "macros": b.Macros, "user": in.Facts.User, "host": in.Facts.Host}
	deps := []Resource{{URI: src, Digest: map[string]string{digestGitCommt: b.Commit}}}
	names := make([]string, 0, len(b.Sources))
	for n := range b.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		deps = append(deps, Resource{Name: n, URI: in.SourceURIs[n], Digest: map[string]string{digestSHA256: b.Sources[n]}})
	}
	if in.Buildroot != nil {
		for _, r := range in.Buildroot.Buildroot.RPMs {
			if r.Name == "gpg-pubkey" {
				continue
			}
			ann := map[string]string{"digest": headerDigest}
			from := ""
			if in.SetRPM != nil {
				from = in.SetRPM(r.NEVRA())
			}
			var uri string
			if from != "" {
				uri = Purl(in.Namespace, r.Name, r.Version, r.Release, r.Arch, in.Target)
				ann["from"] = from
			} else {
				uri = fmt.Sprintf("pkg:rpm/%s/%s@%s-%s?arch=%s", in.Distro, r.Name, r.Version, r.Release, r.Arch)
				if r.Signer != nil && *r.Signer != "" {
					ann["signer"] = *r.Signer
				}
			}
			res := Resource{URI: uri, Annotations: ann}
			if h := r.Header(); h != "" {
				res.Digest = map[string]string{digestSHA256: h}
			}
			deps = append(deps, res)
		}
	}
	st := Statement{
		Type:          StatementType,
		PredicateType: PredicateType,
		Predicate: Predicate{
			BuildDefinition: BuildDefinition{BuildType: BuildType, ExternalParameters: ext, InternalParameters: intl, ResolvedDependencies: deps},
			RunDetails: RunDetails{
				Builder: Builder{ID: BuilderID, Version: map[string]string{"repoctl": in.Facts.Repoctl, "mock": in.Facts.Mock}},
				Metadata: RunMeta{
					InvocationID: b.Run + "/" + api.Node(b.Package, in.Target),
					StartedOn:    api.FormatTime(in.Facts.Started),
					FinishedOn:   api.FormatTime(in.Facts.Finished),
				},
			},
		},
	}
	for _, s := range in.Subjects {
		st.Subject = append(st.Subject, Resource{Name: s.Name, Digest: map[string]string{digestSHA256: s.SHA256}})
	}
	if b.BuildrootFile != "" && in.BuildrootSHA256 != "" {
		st.Predicate.RunDetails.Byproducts = []Resource{{Name: b.BuildrootFile, Digest: map[string]string{digestSHA256: in.BuildrootSHA256}}}
	}
	return st
}

// sourceURI gives git+URL@refs/heads/BRANCH.
func sourceURI(repo, branch string) string {
	u := repo
	if !strings.HasPrefix(u, "git+") {
		u = "git+" + u
	}
	if branch != "" {
		u += "@refs/heads/" + branch
	}
	return u
}

// Marshal gives the indented JSON of a record, with a final newline.
func Marshal(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseStatement reads a provenance record.
func ParseStatement(b []byte) (*Statement, error) {
	var s Statement
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("the provenance record is not valid: %w", err)
	}
	if s.Type != StatementType || s.PredicateType != PredicateType {
		return nil, fmt.Errorf("the provenance record is not an in-toto Statement v1 with SLSA provenance v1")
	}
	return &s, nil
}
