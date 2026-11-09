package rpmmd

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Flags of a versioned capability, as primary.xml writes them.
const (
	FlagEQ = "EQ"
	FlagLT = "LT"
	FlagGT = "GT"
	FlagLE = "LE"
	FlagGE = "GE"
)

// Cap is one capability: a provide or a requirement.
type Cap struct {
	Name  string
	Flags string // "" for an unversioned capability
	EVR   EVR
}

// String gives the rpm form of the capability: NAME, or NAME OP EVR.
func (c Cap) String() string {
	if c.Flags == "" {
		return c.Name
	}
	return c.Name + " " + opOf(c.Flags) + " " + c.EVR.String()
}

func opOf(flags string) string {
	switch flags {
	case FlagEQ:
		return "="
	case FlagLT:
		return "<"
	case FlagGT:
		return ">"
	case FlagLE:
		return "<="
	case FlagGE:
		return ">="
	}
	return flags
}

// ParseCap reads the rpm form NAME or NAME OP EVR.
func ParseCap(s string) Cap {
	f := strings.Fields(s)
	if len(f) != 3 || strings.HasPrefix(s, "(") {
		return Cap{Name: strings.TrimSpace(s)}
	}
	flags := ""
	switch f[1] {
	case "=", "==":
		flags = FlagEQ
	case "<":
		flags = FlagLT
	case ">":
		flags = FlagGT
	case "<=", "=<":
		flags = FlagLE
	case ">=", "=>":
		flags = FlagGE
	default:
		return Cap{Name: strings.TrimSpace(s)}
	}
	return Cap{Name: f[0], Flags: flags, EVR: ParseEVR(f[2])}
}

func has(flags string, bit byte) bool {
	switch flags {
	case FlagEQ:
		return bit == 'e'
	case FlagLT:
		return bit == 'l'
	case FlagGT:
		return bit == 'g'
	case FlagLE:
		return bit == 'l' || bit == 'e'
	case FlagGE:
		return bit == 'g' || bit == 'e'
	}
	return false
}

// Satisfies reports whether the provide p satisfies the requirement r, as
// rpm compares two dependency ranges. An unversioned side matches each
// version.
func Satisfies(p, r Cap) bool {
	if p.Name != r.Name {
		return false
	}
	if p.Flags == "" || r.Flags == "" {
		return true
	}
	sense := CompareEVR(p.EVR, r.EVR)
	switch {
	case sense < 0:
		return has(p.Flags, 'g') || has(r.Flags, 'l')
	case sense > 0:
		return has(p.Flags, 'l') || has(r.Flags, 'g')
	}
	return (has(p.Flags, 'e') && has(r.Flags, 'e')) || (has(p.Flags, 'l') && has(r.Flags, 'l')) ||
		(has(p.Flags, 'g') && has(r.Flags, 'g'))
}

// Package is one binary or source RPM of a repository.
type Package struct {
	Name string
	Arch string
	EVR  EVR
	// SourceRPM is the file name of the source RPM, "" for a source RPM.
	SourceRPM string
	License   string
	Size      int64
	Location  string
	Provides  []Cap
	Requires  []Cap
	// Files are the files that primary.xml lists (bin, sbin, etc).
	Files []string
	// Repo is the id of the repository that holds the package.
	Repo string
}

// NVR gives NAME-VERSION-RELEASE.
func (p *Package) NVR() string { return p.Name + "-" + p.EVR.Version + "-" + p.EVR.Release }

// NVRA gives NAME-VERSION-RELEASE.ARCH.
func (p *Package) NVRA() string { return p.NVR() + "." + p.Arch }

// Key is the key of the newest rule: the name and the architecture.
func (p *Package) Key() string { return p.Name + "." + p.Arch }

// Newer reports whether p is newer than q.
func (p *Package) Newer(q *Package) bool { return CompareEVR(p.EVR, q.EVR) > 0 }

// Newest gives the newest package of each name and architecture.
func Newest(pkgs []*Package) []*Package {
	best := map[string]*Package{}
	var order []string
	for _, p := range pkgs {
		k := p.Key()
		cur, ok := best[k]
		if !ok {
			order = append(order, k)
			best[k] = p
			continue
		}
		if p.Newer(cur) {
			best[k] = p
		}
	}
	out := make([]*Package, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

type xmlEntry struct {
	Name  string `xml:"name,attr"`
	Flags string `xml:"flags,attr"`
	Epoch string `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}

func (e xmlEntry) cap() Cap {
	c := Cap{Name: e.Name, Flags: e.Flags}
	if c.Flags != "" {
		c.EVR = EVR{Epoch: e.Epoch, Version: e.Ver, Release: e.Rel}
	}
	return c
}

type xmlPackage struct {
	Type    string `xml:"type,attr"`
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	} `xml:"version"`
	Size struct {
		Package int64 `xml:"package,attr"`
	} `xml:"size"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Format struct {
		License   string     `xml:"license"`
		SourceRPM string     `xml:"sourcerpm"`
		Provides  []xmlEntry `xml:"provides>entry"`
		Requires  []xmlEntry `xml:"requires>entry"`
		Files     []string   `xml:"file"`
	} `xml:"format"`
}

// ParsePrimary reads the packages of a primary.xml.
func ParsePrimary(r io.Reader, repo string) ([]*Package, error) {
	dec := xml.NewDecoder(r)
	var out []*Package
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("primary.xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "package" {
			continue
		}
		var xp xmlPackage
		if err := dec.DecodeElement(&xp, &se); err != nil {
			return nil, fmt.Errorf("primary.xml: %w", err)
		}
		p := &Package{
			Name: xp.Name, Arch: xp.Arch,
			EVR:       EVR{Epoch: xp.Version.Epoch, Version: xp.Version.Ver, Release: xp.Version.Rel},
			SourceRPM: xp.Format.SourceRPM, License: xp.Format.License,
			Size: xp.Size.Package, Location: xp.Location.Href, Files: xp.Format.Files, Repo: repo,
		}
		for _, e := range xp.Format.Provides {
			p.Provides = append(p.Provides, e.cap())
		}
		for _, e := range xp.Format.Requires {
			p.Requires = append(p.Requires, e.cap())
		}
		out = append(out, p)
	}
}

type repomd struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

// Reader reads repositories from a directory, a file:// URL or an http(s)
// URL.
type Reader struct {
	Client *http.Client
	// Timeout bounds one HTTP request; 0 is 2 minutes.
	Timeout time.Duration
}

// Read reads the packages of primary.xml of the repository at base: a
// directory, file://… or http(s)://…, with repodata/repomd.xml below it.
func (r *Reader) Read(ctx context.Context, base, repo string) ([]*Package, error) {
	md, err := r.fetch(ctx, base, "repodata/repomd.xml")
	if err != nil {
		return nil, err
	}
	var m repomd
	if err := xml.Unmarshal(md, &m); err != nil {
		return nil, fmt.Errorf("%s: repomd.xml: %w", base, err)
	}
	for _, d := range m.Data {
		if d.Type != "primary" {
			continue
		}
		raw, err := r.fetch(ctx, base, d.Location.Href)
		if err != nil {
			return nil, err
		}
		plain, err := decompress(ctx, d.Location.Href, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", base, d.Location.Href, err)
		}
		return ParsePrimary(bytes.NewReader(plain), repo)
	}
	return nil, fmt.Errorf("%s: repomd.xml has no primary data", base)
}

// HasRepodata reports whether dir holds repodata/repomd.xml.
func HasRepodata(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "repodata", "repomd.xml"))
	return err == nil
}

func (r *Reader) fetch(ctx context.Context, base, rel string) ([]byte, error) {
	if !strings.Contains(base, "://") {
		return os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "file" {
		return os.ReadFile(filepath.Join(u.Path, filepath.FromSlash(rel)))
	}
	u.Path = path.Join(u.Path, rel)
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u.String(), resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// decompress unpacks gz and bz2 with the standard library, xz and zst with
// the programs xz and zstd.
func decompress(ctx context.Context, name string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	case strings.HasSuffix(name, ".bz2"):
		return io.ReadAll(bzip2.NewReader(bytes.NewReader(data)))
	case strings.HasSuffix(name, ".xz"):
		return unpack(ctx, "xz", data)
	case strings.HasSuffix(name, ".zst"):
		return unpack(ctx, "zstd", data)
	}
	return data, nil
}

func unpack(ctx context.Context, prog string, data []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, prog, "-dc")
	cmd.Stdin = bytes.NewReader(data)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s -dc: %w: %s", prog, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
