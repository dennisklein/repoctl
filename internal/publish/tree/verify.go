package tree

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/records"
)

// VerifyOptions say what Verify checks besides the signatures and the
// checksums.
type VerifyOptions struct {
	Layout Layout
	// Keyring holds the keys that may sign: sign.keys for publish step 4
	// and pull, the client trust set for verify-el.
	Keyring *Keyring
	// Keyfile is sign.release.keyfile, the root key file, for example
	// RPM-GPG-KEY-slurm; "" skips the check of the root files.
	Keyfile string
	// Release is the name of the release package, for example
	// slurm-release; "" skips the check of the root files.
	Release string
	// Base gives the sha256 of each file of the base by its path. Each file
	// of the base must be in the tree with the same sha256 (keep all
	// versions), except the paths that Changed allows and repodata.
	Base    map[string]string
	Changed func(path string) bool
	// Records checks the supply chain records of the binary repository.
	Records bool
}

// VerifyResult is the result of Verify.
type VerifyResult struct {
	RPMs  int
	Repos int
	// Signers counts the RPMs per signing key fingerprint.
	Signers map[string]int
	// Repomd counts the repomd.xml.asc files that verify.
	Repomd   int
	Records  *records.CheckResult
	Problems []api.Problem
	Warnings []api.Problem
}

// Err gives the first problem as verify.failed (exit 10), or
// records.mismatch when only records fail; nil when the tree verifies.
func (r *VerifyResult) Err() error {
	if len(r.Problems) == 0 {
		return nil
	}
	code := api.PVerifyFailed
	for _, p := range r.Problems {
		if p.Code != api.PRecordsMismatch {
			code = api.PVerifyFailed
			break
		}
		code = api.PRecordsMismatch
	}
	msg := r.Problems[0].Message
	if n := len(r.Problems) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more)", n)
	}
	return api.Errorf(code, "%s", msg)
}

func (r *VerifyResult) fail(format string, args ...any) {
	r.Problems = append(r.Problems, api.Problem{Code: api.PVerifyFailed, Message: fmt.Sprintf(format, args...)})
}

// Verify checks a target tree: per repository the checksums of
// repomd.xml, exactly one valid signature in repomd.xml.asc, the files
// against primary.xml and rpmkeys --checksig on each RPM; then the root
// files, the files of the base and the records.
func Verify(ctx context.Context, dir string, o VerifyOptions) (*VerifyResult, error) {
	r := &VerifyResult{Signers: map[string]int{}}
	files, err := walk(dir)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	sumOf := func(rel string) (string, error) {
		if s, ok := sums[rel]; ok {
			return s, nil
		}
		s, err := SHA256File(filepath.Join(dir, filepath.FromSlash(rel)))
		if err == nil {
			sums[rel] = s
		}
		return s, err
	}
	var rpms []string
	for _, repo := range o.Layout.Repos() {
		rdir := filepath.Join(dir, repo.Dir)
		if _, err := os.Stat(rdir); err != nil {
			continue
		}
		r.Repos++
		md, pkgs, err := ReadPrimary(ctx, rdir)
		if err != nil {
			if os.IsNotExist(err) {
				r.fail("%s has no repodata/repomd.xml", repo.Dir)
				continue
			}
			r.fail("%s: %v", repo.Dir, err)
			continue
		}
		if o.Keyring != nil {
			asc := filepath.Join(rdir, "repodata", "repomd.xml.asc")
			if _, err := os.Stat(asc); err != nil {
				r.fail("%s/repodata/repomd.xml.asc is missing", repo.Dir)
			} else if _, err := o.Keyring.VerifyDetached(ctx, asc, filepath.Join(rdir, "repodata", "repomd.xml")); err != nil {
				r.fail("%s: %v", repo.Dir, err)
			} else {
				r.Repomd++
			}
		}
		for _, d := range md.Data {
			rel := path.Join(repo.Dir, d.Location.Href)
			got, err := sumOf(rel)
			switch {
			case err != nil:
				r.fail("%s is missing", rel)
			case d.Checksum.Type != "sha256":
				r.fail("%s has the checksum type %s, not sha256", rel, d.Checksum.Type)
			case got != d.Checksum.Value:
				r.fail("%s does not have the checksum of repomd.xml", rel)
			}
		}
		listed := map[string]bool{}
		for _, p := range pkgs {
			rel := path.Join(repo.Dir, p.Location.Href)
			listed[rel] = true
			got, err := sumOf(rel)
			switch {
			case err != nil:
				r.fail("%s is in primary.xml but not in the tree", rel)
			case p.Checksum.Type == "sha256" && got != p.Checksum.Value:
				r.fail("%s does not have the checksum of primary.xml", rel)
			}
		}
		for _, f := range files {
			if strings.HasPrefix(f, repo.Dir+"/") && strings.HasSuffix(f, ".rpm") && !listed[f] {
				r.fail("%s is not in primary.xml", f)
			}
		}
		for f := range listed {
			rpms = append(rpms, f)
		}
	}
	sort.Strings(rpms)
	r.RPMs = len(rpms)
	if o.Keyring != nil && len(rpms) > 0 {
		abs := make([]string, len(rpms))
		for i, f := range rpms {
			abs[i] = filepath.Join(dir, filepath.FromSlash(f))
		}
		bad, err := o.Keyring.CheckRPMs(ctx, abs)
		if err != nil {
			return nil, err
		}
		for i, f := range abs {
			if why, ok := bad[f]; ok {
				r.fail("%s: %s", rpms[i], why)
			}
		}
		hs, err := QueryHeaders(ctx, abs)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			if key, ok := o.Keyring.ByKeyID(h.KeyID); ok {
				r.Signers[key.Fingerprint]++
			}
		}
	}
	if o.Keyfile != "" && o.Release != "" {
		verifyRoot(ctx, dir, o, r)
	}
	for p, want := range o.Base {
		if strings.Contains(p, "/repodata/") || (o.Changed != nil && o.Changed(p)) {
			continue
		}
		got, err := sumOf(p)
		switch {
		case err != nil:
			r.fail("%s of the base is missing (keep all versions)", p)
		case got != want:
			r.fail("%s differs from the base (keep all versions)", p)
		}
	}
	if o.Records {
		bin := filepath.Join(dir, o.Layout.Name)
		m, err := manifest.ReadRepo(bin)
		if err != nil {
			r.fail("%s: %v", o.Layout.Name, err)
		} else if m != nil {
			idx := map[string]string{}
			for _, f := range files {
				if strings.HasSuffix(f, ".rpm") && strings.Contains(f, "/") {
					idx[path.Base(f)] = f
				}
			}
			cr := records.Check(bin, m, func(base string) (string, bool) {
				rel, ok := idx[base]
				if !ok {
					return "", false
				}
				s, err := sumOf(rel)
				return s, err == nil
			})
			r.Records = cr
			r.Problems = append(r.Problems, cr.Problems...)
			r.Warnings = append(r.Warnings, cr.Warnings...)
		}
	}
	return r, nil
}

// verifyRoot checks the root files: the root release RPM equals the release
// RPM with the highest NVR in the binary repository, and the root key file
// equals the key file in it.
func verifyRoot(ctx context.Context, dir string, o VerifyOptions, r *VerifyResult) {
	rootRPM := filepath.Join(dir, o.Release+".rpm")
	rootKey := filepath.Join(dir, o.Keyfile)
	if _, err := os.Stat(rootRPM); err != nil {
		r.fail("the root file %s.rpm is missing", o.Release)
		return
	}
	newest, err := NewestRelease(ctx, filepath.Join(dir, o.Layout.Name), o.Release)
	if err != nil {
		r.fail("%v", err)
		return
	}
	if newest != "" {
		a, _ := SHA256File(rootRPM)
		b, _ := SHA256File(newest)
		if a != b {
			r.fail("the root file %s.rpm is not the release RPM with the highest NVR", o.Release)
		}
	}
	key, ok, err := ExtractFile(ctx, rootRPM, o.Keyfile)
	if err != nil || !ok {
		r.fail("the release RPM has no key file %s", o.Keyfile)
		return
	}
	got, err := SHA256File(rootKey)
	if err != nil {
		r.fail("the root key file %s is missing", o.Keyfile)
		return
	}
	if got != SHA256Bytes(key) {
		r.fail("the root key file %s differs from the key file of the release RPM", o.Keyfile)
	}
}

// NewestRelease gives the release RPM with the highest NVR in a binary
// repository directory, or "" when it has none.
func NewestRelease(ctx context.Context, repoDir, name string) (string, error) {
	var found []string
	_ = filepath.WalkDir(filepath.Join(repoDir, "Packages"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), name+"-") && strings.HasSuffix(d.Name(), ".rpm") && !strings.HasSuffix(d.Name(), ".src.rpm") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) == 0 {
		return "", nil
	}
	hs, err := QueryHeaders(ctx, found)
	if err != nil {
		return "", err
	}
	best := -1
	for i, h := range hs {
		if h.Name != name {
			continue
		}
		if best < 0 || manifest.CompareEVR(h.Epoch, h.Version, h.Release, hs[best].Epoch, hs[best].Version, hs[best].Release) > 0 {
			best = i
		}
	}
	if best < 0 {
		return "", nil
	}
	return found[best], nil
}

// walk lists the files of a tree, relative, with slashes, sorted.
func walk(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}
