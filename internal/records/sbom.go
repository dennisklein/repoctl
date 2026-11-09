package records

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/manifest"
)

// SpecVersion is the CycloneDX version of the SBOMs.
const SpecVersion = "1.6"

// The property names of repoctl in an SBOM.
const (
	PropType = "repoctl:type" // upstream, srpm, rpm, debuginfo, bundled
	PropFile = "repoctl:file" // the file name of an RPM in the tree
)

// BOM is a CycloneDX 1.6 document, with the fields that repoctl writes.
type BOM struct {
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	SerialNumber string       `json:"serialNumber"`
	Version      int          `json:"version"`
	Metadata     BOMMetadata  `json:"metadata"`
	Components   []Component  `json:"components"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// BOMMetadata is metadata of a BOM.
type BOMMetadata struct {
	Timestamp string           `json:"timestamp,omitempty"`
	Tools     *Tools           `json:"tools,omitempty"`
	Supplier  *Organization    `json:"supplier,omitempty"`
	Component *Component       `json:"component,omitempty"`
	Props     []Property       `json:"properties,omitempty"`
	Lifecycle []map[string]any `json:"lifecycles,omitempty"`
}

// Tools is metadata.tools.
type Tools struct {
	Components []Component `json:"components"`
}

// Organization is a supplier.
type Organization struct {
	Name string `json:"name"`
}

// Component is one component.
type Component struct {
	Type         string        `json:"type"`
	BOMRef       string        `json:"bom-ref,omitempty"`
	Supplier     *Organization `json:"supplier,omitempty"`
	Name         string        `json:"name"`
	Version      string        `json:"version,omitempty"`
	Purl         string        `json:"purl,omitempty"`
	Hashes       []Hash        `json:"hashes,omitempty"`
	Licenses     []License     `json:"licenses,omitempty"`
	ExtRefs      []ExtRef      `json:"externalReferences,omitempty"`
	Pedigree     *Pedigree     `json:"pedigree,omitempty"`
	Properties   []Property    `json:"properties,omitempty"`
	Components   []Component   `json:"components,omitempty"`
	Descriptions string        `json:"description,omitempty"`
}

// Hash is one hash of a component.
type Hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

// License is one entry of licenses[]: an SPDX expression or a name.
type License struct {
	Expression string       `json:"expression,omitempty"`
	License    *LicenseName `json:"license,omitempty"`
}

// LicenseName is a license that is not an SPDX expression.
type LicenseName struct {
	Name string `json:"name"`
}

// ExtRef is one external reference.
type ExtRef struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Hashes []Hash `json:"hashes,omitempty"`
}

// Pedigree gives the upstream of a component.
type Pedigree struct {
	Ancestors []Component `json:"ancestors,omitempty"`
}

// Property is a name-value pair.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Dependency is one entry of dependencies[].
type Dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Prop gives the value of a property, or "".
func (c Component) Prop(name string) string {
	for _, p := range c.Properties {
		if p.Name == name {
			return p.Value
		}
	}
	return ""
}

// SHA256 gives the SHA-256 hash of a component, or "".
func (c Component) SHA256() string {
	for _, h := range c.Hashes {
		if h.Alg == "SHA-256" {
			return h.Content
		}
	}
	return ""
}

// LicenseText gives the license as one string.
func (c Component) LicenseText() string {
	var out []string
	for _, l := range c.Licenses {
		if l.Expression != "" {
			out = append(out, l.Expression)
		} else if l.License != nil {
			out = append(out, l.License.Name)
		}
	}
	return strings.Join(out, " AND ")
}

// spdxRE accepts the common form of an SPDX license expression: ids
// joined with AND, OR and WITH, and parentheses.
var (
	spdxRE = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)
	// fedoraRE matches the old Fedora short names, for example GPLv2+ or
	// LGPLv3: SPDX ids write the version as -2.0.
	fedoraRE = regexp.MustCompile(`[A-Za-z]v[0-9]`)
)

// IsSPDX reports whether a License tag is an SPDX expression in the
// common form.
func IsSPDX(s string) bool {
	s = strings.NewReplacer("(", " ", ")", " ").Replace(s)
	words := strings.Fields(s)
	if len(words) == 0 {
		return false
	}
	op := false
	for _, w := range words {
		if op {
			if w != "AND" && w != "OR" && w != "WITH" {
				return false
			}
		} else if !spdxRE.MatchString(w) || fedoraRE.MatchString(w) || w == "AND" || w == "OR" || w == "WITH" {
			return false
		}
		op = !op
	}
	return op
}

// Licenses gives licenses[] of a License tag: an SPDX expression goes to
// expression, any other value to license.name (supply-chain.html#sbom-record).
func Licenses(tag string) []License {
	if tag == "" {
		return nil
	}
	if IsSPDX(tag) {
		return []License{{Expression: tag}}
	}
	return []License{{License: &LicenseName{Name: tag}}}
}

// serial gives a stable urn:uuid from a key.
func serial(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("urn:uuid:%s-%s-5%s-a%s-%s", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
}

// RPMInfo is what the build SBOM needs of one RPM of a build.
type RPMInfo struct {
	File    string
	SHA256  string
	Name    string
	Version string
	Release string
	Arch    string
	License string
	// Kind is srpm, rpm, debuginfo or debugsource.
	Kind     string
	Provides []string
	Requires []string
}

// SBOMInput is what the SBOM of one build comes from.
type SBOMInput struct {
	Set, Target string
	Namespace   string
	Publish     string
	Created     time.Time
	Repoctl     string
	Build       manifest.Build
	SRPM        RPMInfo
	RPMs        []RPMInfo
	SourceURIs  map[string]string
	// SetProvides gives the purl of the RPM of the set that provides a
	// requirement, or "".
	SetProvides func(req string) string
}

var bundledRE = regexp.MustCompile(`^bundled\(([^)]+)\)\s*(?:=\s*(\S+))?`)

// NewBuildSBOM gives the CycloneDX SBOM of a build, after the Red Hat
// guidance for RPMs.
func NewBuildSBOM(in SBOMInput) BOM {
	nsup := &Organization{Name: in.Namespace}
	srpmPurl := Purl(in.Namespace, in.SRPM.Name, in.SRPM.Version, in.SRPM.Release, "src", in.Target)
	main := Component{
		Type: "library", BOMRef: srpmPurl, Supplier: nsup, Name: in.SRPM.Name,
		Version: in.SRPM.Version + "-" + in.SRPM.Release, Purl: srpmPurl,
		Licenses:   Licenses(in.SRPM.License),
		Properties: []Property{{PropType, "srpm"}, {PropFile, in.SRPM.File}},
	}
	if in.SRPM.SHA256 != "" {
		main.Hashes = []Hash{{"SHA-256", in.SRPM.SHA256}}
	}
	var anc []Component
	names := make([]string, 0, len(in.Build.Sources))
	for n := range in.Build.Sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		anc = append(anc, upstream(n, in.SourceURIs[n], in.Build.Sources[n], in.SRPM.Name, in.SRPM.Version))
	}
	if len(anc) > 0 {
		main.Pedigree = &Pedigree{Ancestors: anc}
	}
	bom := BOM{
		BOMFormat: "CycloneDX", SpecVersion: SpecVersion, Version: 1,
		SerialNumber: serial("build/" + in.Set + "/" + in.Target + "/" + manifest.NVR(in.SRPM.File) + "/" + in.Publish),
		Metadata: BOMMetadata{
			Timestamp: in.Created.UTC().Format(time.RFC3339),
			Tools:     &Tools{Components: []Component{{Type: "application", Name: "repoctl", Version: in.Repoctl}}},
			Supplier:  nsup,
			Component: &main,
		},
		Components: []Component{},
	}
	deps := []Dependency{}
	for _, r := range in.RPMs {
		p := Purl(in.Namespace, r.Name, r.Version, r.Release, r.Arch, in.Target)
		c := Component{
			Type: "library", BOMRef: p, Supplier: nsup, Name: r.Name, Version: r.Version + "-" + r.Release, Purl: p,
			Licenses:   Licenses(r.License),
			Properties: []Property{{PropType, sbomType(r.Kind)}, {PropFile, r.File}},
		}
		if r.SHA256 != "" {
			c.Hashes = []Hash{{"SHA-256", r.SHA256}}
		}
		var setDeps []string
		for _, prov := range r.Provides {
			if m := bundledRE.FindStringSubmatch(prov); m != nil {
				bp := "pkg:generic/" + url.PathEscape(m[1])
				if m[2] != "" {
					bp += "@" + url.PathEscape(m[2])
				}
				c.Components = append(c.Components, Component{Type: "library", BOMRef: p + "#bundled-" + m[1], Name: m[1], Version: m[2], Purl: bp,
					Properties: []Property{{PropType, "bundled"}}})
			}
		}
		for _, req := range r.Requires {
			if strings.HasPrefix(req, "rpmlib(") || strings.HasPrefix(req, "/") {
				continue
			}
			if in.SetProvides != nil {
				if dp := in.SetProvides(req); dp != "" && dp != p {
					setDeps = append(setDeps, dp)
					continue
				}
			}
			c.Properties = append(c.Properties, Property{"repoctl:requires", req})
		}
		sort.Strings(setDeps)
		setDeps = uniq(setDeps)
		if len(setDeps) > 0 {
			deps = append(deps, Dependency{Ref: p, DependsOn: setDeps})
		}
		bom.Components = append(bom.Components, c)
	}
	if len(deps) > 0 {
		bom.Dependencies = deps
	}
	return bom
}

func uniq(s []string) []string {
	var out []string
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func sbomType(kind string) string {
	switch kind {
	case "srpm":
		return "srpm"
	case "debuginfo", "debugsource":
		return "debuginfo"
	}
	return "rpm"
}

var githubRE = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/`)

// upstream gives the ancestor of one source archive: pkg:github for a
// GitHub URL, else pkg:generic with download_url and checksum.
func upstream(file, uri, sum, name, version string) Component {
	c := Component{Type: "library", Name: name, Version: version, Properties: []Property{{PropType, "upstream"}, {PropFile, file}}}
	if sum != "" {
		c.Hashes = []Hash{{"SHA-256", sum}}
	}
	if uri != "" {
		c.ExtRefs = []ExtRef{{Type: "distribution", URL: uri}}
	}
	if m := githubRE.FindStringSubmatch(uri); m != nil {
		c.Purl = fmt.Sprintf("pkg:github/%s/%s@v%s", strings.ToLower(m[1]), strings.ToLower(m[2]), version)
	} else {
		q := url.Values{}
		if uri != "" {
			q.Set("download_url", uri)
		}
		if sum != "" {
			q.Set("checksum", "sha256:"+sum)
		}
		c.Purl = "pkg:generic/" + url.PathEscape(name) + "@" + url.PathEscape(version)
		if len(q) > 0 {
			c.Purl += "?" + q.Encode()
		}
	}
	c.BOMRef = c.Purl
	return c
}

// BuildSBOMRef is one build SBOM in the repository SBOM.
type BuildSBOMRef struct {
	NVR    string
	Path   string
	SHA256 string
}

// RepoSBOMInput is what the repository SBOM of a target comes from.
type RepoSBOMInput struct {
	Set, Target string
	Namespace   string
	// Store is store.oci, the repository_url of the purl.
	Store   string
	Publish string
	Created time.Time
	Repoctl string
	Builds  []BuildSBOMRef
}

// NewRepositorySBOM gives the SBOM of the repository of a target: the
// snapshot as metadata.component and the build SBOMs as components. The
// snapshot is named by its publish id: the digest of the snapshot covers
// this file, so the file cannot hold it.
func NewRepositorySBOM(in RepoSBOMInput) BOM {
	q := url.Values{}
	if in.Store != "" {
		q.Set("repository_url", in.Store)
	}
	q.Set("tag", in.Publish)
	p := "pkg:oci/" + in.Set + "-" + in.Target + "?" + q.Encode()
	main := Component{Type: "container", BOMRef: p, Supplier: &Organization{Name: in.Namespace}, Name: in.Set + "-" + in.Target, Version: in.Publish, Purl: p}
	bom := BOM{
		BOMFormat: "CycloneDX", SpecVersion: SpecVersion, Version: 1,
		SerialNumber: serial("repository/" + in.Set + "/" + in.Target + "/" + in.Publish),
		Metadata: BOMMetadata{
			Timestamp: in.Created.UTC().Format(time.RFC3339),
			Tools:     &Tools{Components: []Component{{Type: "application", Name: "repoctl", Version: in.Repoctl}}},
			Supplier:  &Organization{Name: in.Namespace},
			Component: &main,
		},
		Components: []Component{},
	}
	bs := append([]BuildSBOMRef{}, in.Builds...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].Path < bs[j].Path })
	for _, b := range bs {
		bom.Components = append(bom.Components, Component{
			Type: "file", BOMRef: b.Path, Name: b.Path, Version: b.NVR,
			Hashes:     []Hash{{"SHA-256", b.SHA256}},
			ExtRefs:    []ExtRef{{Type: "bom", URL: b.Path, Hashes: []Hash{{"SHA-256", b.SHA256}}}},
			Properties: []Property{{"repoctl:build", b.NVR}},
		})
	}
	return bom
}

// ParseBOM reads a CycloneDX document.
func ParseBOM(b []byte) (*BOM, error) {
	var bom BOM
	if err := json.Unmarshal(b, &bom); err != nil {
		return nil, fmt.Errorf("the SBOM is not valid: %w", err)
	}
	if bom.BOMFormat != "CycloneDX" {
		return nil, fmt.Errorf("the SBOM is not CycloneDX")
	}
	return &bom, nil
}

// Namespace gives the purl namespace of store.oci: its owner, for example
// gsi-hpc of ghcr.io/gsi-hpc/slurm-repo.
func Namespace(storeOCI string) string {
	parts := strings.Split(strings.TrimSuffix(storeOCI, "/"), "/")
	if len(parts) >= 2 {
		return strings.ToLower(parts[1])
	}
	return "repoctl"
}
