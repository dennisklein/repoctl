package records

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
)

// CheckResult is the result of the check of the records of one binary
// repository.
type CheckResult struct {
	// Records is the number of record files that the check read.
	Records int
	// Problems are records.mismatch problems: a record or a subject does not
	// equal its file.
	Problems []api.Problem
	// Warnings are records.missing warnings: a build without provenance
	// (adopted) or without an SBOM.
	Warnings []api.Problem
	// Adopted counts the builds with an SBOM and no provenance.
	Adopted int
}

// Err gives the first problem as an error (records.mismatch, exit 10), or
// nil.
func (r *CheckResult) Err() error {
	if len(r.Problems) == 0 {
		return nil
	}
	msg := r.Problems[0].Message
	if n := len(r.Problems) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more)", n)
	}
	return api.Errorf(api.PRecordsMismatch, "%s", msg)
}

// FileSums gives the sha256 of a file by its base name; Check uses it for
// the subjects and the SBOM components, which name files by base name.
type FileSums func(base string) (string, bool)

// Check checks the records of the binary repository dir against the
// RepoManifest m (supply-chain.html#verify): the sha256 of each record in
// the RepoManifest, each provenance subject and each SBOM hash against its
// file.
func Check(dir string, m *manifest.Manifest, files FileSums) *CheckResult {
	r := &CheckResult{}
	if m == nil {
		return r
	}
	mismatch := func(format string, args ...any) {
		r.Problems = append(r.Problems, api.Problem{Code: api.PRecordsMismatch, Message: fmt.Sprintf(format, args...), Target: m.Target})
	}
	read := func(rec *manifest.Record) ([]byte, bool) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rec.Path)))
		if err != nil {
			mismatch("the record %s is missing", rec.Path)
			return nil, false
		}
		r.Records++
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != rec.SHA256 {
			mismatch("the record %s does not have the sha256 of the RepoManifest", rec.Path)
			return nil, false
		}
		return b, true
	}
	subject := func(rec, name, sum string) {
		got, ok := files(name)
		switch {
		case !ok:
			mismatch("the subject %s of %s is not in the tree", name, rec)
		case got != sum:
			mismatch("the subject %s of %s does not have the sha256 of its file", name, rec)
		}
	}
	for _, b := range m.Builds {
		nvr := manifest.NVR(b.SRPM)
		var prov, sbom *manifest.Record
		if b.Records != nil {
			prov, sbom = b.Records.Provenance, b.Records.SBOM
		}
		if prov == nil {
			if sbom != nil {
				r.Adopted++
			} else {
				r.Warnings = append(r.Warnings, api.Problem{Code: api.PRecordsMissing, Target: m.Target, Message: fmt.Sprintf("%s has no provenance and no SBOM", nvr)})
			}
		} else if raw, ok := read(prov); ok {
			st, err := ParseStatement(raw)
			if err != nil {
				mismatch("%s: %v", prov.Path, err)
			} else {
				for _, s := range st.Subject {
					subject(prov.Path, s.Name, s.Digest[digestSHA256])
				}
			}
		}
		if sbom != nil {
			if raw, ok := read(sbom); ok {
				bom, err := ParseBOM(raw)
				if err != nil {
					mismatch("%s: %v", sbom.Path, err)
				} else {
					comps := bom.Components
					if bom.Metadata.Component != nil {
						comps = append([]Component{*bom.Metadata.Component}, comps...)
					}
					for _, c := range comps {
						if f, h := c.Prop(PropFile), c.SHA256(); f != "" && h != "" && c.Prop(PropType) != "upstream" {
							subject(sbom.Path, f, h)
						}
					}
				}
			}
		}
	}
	if r.Adopted > 0 {
		r.Warnings = append(r.Warnings, api.Problem{Code: api.PRecordsMissing, Target: m.Target,
			Message: fmt.Sprintf("%d builds have an SBOM and no provenance (adopted)", r.Adopted)})
	}
	if m.Records != nil && m.Records.SBOM != nil {
		if raw, ok := read(m.Records.SBOM); ok {
			if bom, err := ParseBOM(raw); err != nil {
				mismatch("%s: %v", m.Records.SBOM.Path, err)
			} else {
				for _, c := range bom.Components {
					if h := c.SHA256(); h != "" {
						got, err := fileSHA256(filepath.Join(dir, filepath.FromSlash(c.Name)))
						if err != nil || got != h {
							mismatch("the build SBOM %s of %s does not have its sha256", c.Name, m.Records.SBOM.Path)
						}
					}
				}
			}
		}
	}
	sort.SliceStable(r.Problems, func(i, j int) bool { return r.Problems[i].Message < r.Problems[j].Message })
	return r
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
