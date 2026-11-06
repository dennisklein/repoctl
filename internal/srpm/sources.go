package srpm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dennisklein/repoctl/internal/api"
)

// SourcesFile is the name of the sources file of a package branch: one
// line SHA512 (FILE) = HEX per source archive that is not in git.
const SourcesFile = "sources"

// FedoraLookaside is the Fedora lookaside cache.
const FedoraLookaside = "https://src.fedoraproject.org/repo/pkgs"

var reSHA512 = regexp.MustCompile(`^SHA512 \(([^)]+)\) = ([0-9a-fA-F]{128})$`)

// ParseSourcesFile reads the SHA512 lines of a sources file and gives the
// hex digest of each file. Lines of another form (the old MD5 form) are not
// used: repoctl accepts only the SHA512 form (build.html#sources).
func ParseSourcesFile(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		if m := reSHA512.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			out[m[1]] = strings.ToLower(m[2])
		}
	}
	return out
}

// SpecSource is one Source or Patch line of a spec, after rpmspec -P.
type SpecSource struct {
	// Tag is Source0, Patch1 and so on; "sourcelist" or "patchlist" for a
	// line of those sections.
	Tag string
	// Value is the value of the line.
	Value string
	// URL is the value when it has a scheme, else "".
	URL string
	// File is the file name, as spectool gives it: the last part of the
	// URL path, or of the fragment #/NAME.
	File string
}

var reSourceTag = regexp.MustCompile(`(?i)^((?:source|patch)[0-9]*)\s*:\s*(\S+)`)

// ParseSpecSources reads the Source and Patch lines and the %sourcelist and
// %patchlist sections of a parsed spec (rpmspec -P).
func ParseSpecSources(parsed string) []SpecSource {
	var out []SpecSource
	section := ""
	for _, line := range strings.Split(parsed, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "%") {
			f := strings.Fields(t)[0]
			switch f {
			case "%sourcelist", "%patchlist":
				section = strings.TrimPrefix(f, "%")
			default:
				section = ""
			}
			continue
		}
		if section != "" {
			if t != "" && !strings.HasPrefix(t, "#") {
				out = append(out, newSource(section, t))
			}
			continue
		}
		if m := reSourceTag.FindStringSubmatch(t); m != nil {
			out = append(out, newSource(m[1], m[2]))
		}
	}
	return out
}

func newSource(tag, value string) SpecSource {
	s := SpecSource{Tag: tag, Value: value, File: path.Base(value)}
	if strings.Contains(value, "://") {
		s.URL = value
		if u, err := url.Parse(value); err == nil {
			switch {
			case strings.HasPrefix(u.Fragment, "/"):
				s.File = path.Base(u.Fragment)
			case u.Path != "":
				s.File = path.Base(u.Path)
			}
		}
	}
	return s
}

// Fetcher gets source archives into the source cache
// $XDG_CACHE_HOME/repoctl/sources/sha512/HEX/FILE: from the cache, the
// Fedora lookaside cache or the URL of the Source line, in this order.
type Fetcher struct {
	// Cache is the source cache directory.
	Cache string
	// Lookaside is the base URL of the Fedora lookaside cache.
	Lookaside string
	// Client makes the downloads.
	Client *http.Client
	// Offline uses only the source cache.
	Offline bool
}

// NewFetcher gives a Fetcher on a cache directory.
func NewFetcher(cache string) *Fetcher {
	return &Fetcher{Cache: cache, Lookaside: FedoraLookaside, Client: &http.Client{Timeout: 30 * time.Minute}}
}

// CachePath gives the path of a file in the source cache.
func (f *Fetcher) CachePath(sha512hex, file string) string {
	return filepath.Join(f.Cache, "sha512", strings.ToLower(sha512hex), file)
}

// LookasideURL gives the URL of a file in the Fedora lookaside cache:
// BASE/NAME/FILE/sha512/HEX/FILE.
func (f *Fetcher) LookasideURL(name, file, sha512hex string) string {
	base := strings.TrimSuffix(f.Lookaside, "/")
	return fmt.Sprintf("%s/%s/%s/sha512/%s/%s", base, url.PathEscape(name), url.PathEscape(file), strings.ToLower(sha512hex), url.PathEscape(file))
}

// Get gives the path in the source cache of a file with the SHA-512, and
// downloads it first when it is not there. fedora is upstream.fedora ("" to
// skip the lookaside cache), rawURL the URL of the Source line ("" for
// none). A download that has another SHA-512 is never used: Get gives
// source.mismatch when no source gives the right file, else
// source.unreachable.
func (f *Fetcher) Get(ctx context.Context, file, sha512hex, fedora, rawURL string) (string, error) {
	dst := f.CachePath(sha512hex, file)
	if ok, _ := fileHasSHA512(dst, sha512hex); ok {
		return dst, nil
	}
	_ = os.Remove(dst) // a damaged entry
	if f.Offline {
		return "", api.Errorf(api.PSourceUnreachable, "source %s is not in the source cache, and repoctl is offline", file)
	}
	var urls []string
	if fedora != "" && f.Lookaside != "" {
		urls = append(urls, f.LookasideURL(fedora, file, sha512hex))
	}
	if rawURL != "" {
		urls = append(urls, rawURL)
	}
	var mismatch string
	var last error
	for _, u := range urls {
		got, err := f.download(ctx, u, dst, sha512hex)
		if err == nil {
			return dst, nil
		}
		if got != "" {
			mismatch = fmt.Sprintf("%s from %s has SHA-512 %s…, the sources file gives %s…", file, u, got[:12], strings.ToLower(sha512hex)[:12])
		}
		last = err
	}
	if mismatch != "" {
		return "", api.Errorf(api.PSourceMismatch, "%s", mismatch)
	}
	if last == nil {
		return "", api.Errorf(api.PSourceUnreachable, "source %s is not in the source cache, and no URL names it", file)
	}
	return "", api.Errorf(api.PSourceUnreachable, "source %s: %v", file, last).WithCause(last)
}

// download writes u to a temporary file next to dst, checks its SHA-512
// and renames it to dst. On a mismatch it gives the SHA-512 of the file.
func (f *Fetcher) download(ctx context.Context, u, dst, want string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(want) {
		return got, errors.New("checksum mismatch")
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	return "", os.Rename(tmp.Name(), dst)
}

func fileHasSHA512(p, want string) (bool, error) {
	fh, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	h := sha512.New()
	if _, err := io.Copy(h, fh); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(want), nil
}

// PutSources puts the sources of a spec into the package directory of a
// source clone (build.html#sources). A file that the commit holds stays as
// it is. A file with a SHA512 line in the sources file comes from the
// source cache, the lookaside cache (fedora) or its URL. A URL without a
// line gives source.unlisted. A plain name that is neither in git nor in
// the sources file is left to rpmbuild.
func (f *Fetcher) PutSources(ctx context.Context, pkgDir string, srcs []SpecSource, fedora string) error {
	sums := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(pkgDir, SourcesFile)); err == nil {
		sums = ParseSourcesFile(data)
	}
	for _, s := range srcs {
		dst := filepath.Join(pkgDir, s.File)
		if _, err := os.Lstat(dst); err == nil {
			continue // in git at the commit
		}
		sum, listed := sums[s.File]
		if !listed {
			if s.URL != "" {
				return api.Errorf(api.PSourceUnlisted, "%s has the URL %s, and the sources file has no SHA512 line for %s", s.Tag, s.URL, s.File).
					WithHint("add the line, for example with sha512sum --tag "+s.File+", and commit", "")
			}
			continue
		}
		cached, err := f.Get(ctx, s.File, sum, fedora, s.URL)
		if err != nil {
			return err
		}
		if err := linkOrCopy(cached, dst); err != nil {
			return err
		}
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sourceDigests gives the sha256 of each source file in the package
// directory (the record draft of the node).
func sourceDigests(pkgDir string, srcs []SpecSource) map[string]string {
	out := map[string]string{}
	for _, s := range srcs {
		fh, err := os.Open(filepath.Join(pkgDir, s.File))
		if err != nil {
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, fh)
		fh.Close()
		if err == nil {
			out[s.File] = hex.EncodeToString(h.Sum(nil))
		}
	}
	return out
}
