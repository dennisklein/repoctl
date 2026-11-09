package tree

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Header is what repoctl reads from the header of one RPM file.
type Header struct {
	File      string
	Name      string
	Epoch     int
	Version   string
	Release   string
	Arch      string
	Sourcerpm string
	License   string
	Summary   string
	URL       string
	// PayloadDigest is the payload digest: equal for the same content,
	// also after a new signature.
	PayloadDigest string
	// HeaderDigest is SHA256HEADER.
	HeaderDigest string
	Provides     []string
	Requires     []string
	// KeyID is the long key id of the signature (16 hex digits, lower
	// case), or "" for an unsigned RPM.
	KeyID string
}

// NVR gives name-version-release.
func (h Header) NVR() string { return h.Name + "-" + h.Version + "-" + h.Release }

// NVRA gives name-version-release.arch; an SRPM has the arch src.
func (h Header) NVRA() string {
	arch := h.Arch
	if h.Sourcerpm == "" {
		arch = "src"
	}
	return h.NVR() + "." + arch
}

const (
	fieldSep = "\x1f"
	listSep  = "\x1e"
	recSep   = "\x1d\n"
)

var headerFormat = strings.Join([]string{
	"%{NAME}", "%{EPOCHNUM}", "%{VERSION}", "%{RELEASE}", "%{ARCH}",
	"%|SOURCERPM?{%{SOURCERPM}}:{}|", "%{LICENSE}", "%{SUMMARY}", "%|URL?{%{URL}}:{}|",
	"%|PAYLOADDIGEST?{%{PAYLOADDIGEST}}:{}|", "%|SHA256HEADER?{%{SHA256HEADER}}:{}|",
	"[%{PROVIDENEVRS}" + listSep + "]", "[%{REQUIRENEVRS}" + listSep + "]",
	"%|DSAHEADER?{%{DSAHEADER:pgpsig}}:{%|RSAHEADER?{%{RSAHEADER:pgpsig}}:{%|SIGGPG?{%{SIGGPG:pgpsig}}:{%|SIGPGP?{%{SIGPGP:pgpsig}}:{}|}|}|}|",
}, fieldSep) + recSep

var keyIDRE = regexp.MustCompile(`Key ID ([0-9a-fA-F]+)`)

// QueryHeaders reads the headers of RPM files, in order.
func QueryHeaders(ctx context.Context, files []string) ([]Header, error) {
	var out []Header
	for start := 0; start < len(files); start += 200 {
		end := min(start+200, len(files))
		args := append([]string{"-qp", "--nosignature", "--nodigest", "--qf", headerFormat}, files[start:end]...)
		b, err := (&run.Cmd{Name: "rpm", Args: args}).Output(ctx)
		if err != nil {
			return nil, err
		}
		recs := strings.Split(string(b), recSep)
		if len(recs) > 0 && recs[len(recs)-1] == "" {
			recs = recs[:len(recs)-1]
		}
		if len(recs) != end-start {
			return nil, fmt.Errorf("rpm -qp gave %d headers for %d files", len(recs), end-start)
		}
		for i, r := range recs {
			f := strings.Split(r, fieldSep)
			if len(f) != 14 {
				return nil, fmt.Errorf("rpm -qp %s: unexpected output", files[start+i])
			}
			ep, _ := strconv.Atoi(f[1])
			h := Header{File: files[start+i], Name: f[0], Epoch: ep, Version: f[2], Release: f[3], Arch: f[4],
				Sourcerpm: f[5], License: f[6], Summary: f[7], URL: f[8], PayloadDigest: f[9], HeaderDigest: f[10],
				Provides: splitList(f[11]), Requires: splitList(f[12])}
			if m := keyIDRE.FindStringSubmatch(f[13]); m != nil {
				h.KeyID = strings.ToLower(m[1])
			}
			out = append(out, h)
		}
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, listSep) {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// QueryHeader reads the header of one RPM file.
func QueryHeader(ctx context.Context, file string) (Header, error) {
	hs, err := QueryHeaders(ctx, []string{file})
	if err != nil {
		return Header{}, err
	}
	return hs[0], nil
}

// ExtractFile extracts the file with the base name from the payload of an
// RPM and gives its content. It reads the cpio archive of rpm2cpio itself,
// so it needs no cpio program. ok is false when the RPM has no such file.
func ExtractFile(ctx context.Context, rpmFile, base string) ([]byte, bool, error) {
	list, err := (&run.Cmd{Name: "rpm", Args: []string{"-qlp", "--nosignature", "--nodigest", rpmFile}}).Output(ctx)
	if err != nil {
		return nil, false, err
	}
	var want string
	for _, l := range strings.Split(string(list), "\n") {
		if filepath.Base(l) == base && strings.HasPrefix(l, "/") {
			want = l
			break
		}
	}
	if want == "" {
		return nil, false, nil
	}
	archive, err := (&run.Cmd{Name: "rpm2cpio", Args: []string{rpmFile}}).Output(ctx)
	if err != nil {
		return nil, false, err
	}
	b, ok, err := cpioFile(archive, want)
	if err != nil {
		return nil, false, fmt.Errorf("rpm2cpio %s: %w", rpmFile, err)
	}
	return b, ok, nil
}

// cpioFile finds a file in a cpio archive of the newc format (magic 070701
// or 070702): each entry has a header of 110 bytes, the name and the data,
// each padded to 4 bytes.
func cpioFile(a []byte, want string) ([]byte, bool, error) {
	pad := func(n int) int { return (n + 3) &^ 3 }
	for off := 0; off+110 <= len(a); {
		h := a[off : off+110]
		if string(h[:5]) != "07070" {
			return nil, false, fmt.Errorf("not a newc cpio archive")
		}
		field := func(i int) (int, error) {
			v, err := strconv.ParseUint(string(h[6+8*i:6+8*i+8]), 16, 32)
			return int(v), err
		}
		size, err := field(6)
		if err != nil {
			return nil, false, err
		}
		nameSize, err := field(11)
		if err != nil {
			return nil, false, err
		}
		if off+110+nameSize > len(a) || nameSize == 0 {
			return nil, false, fmt.Errorf("truncated cpio archive")
		}
		name := string(a[off+110 : off+110+nameSize-1])
		if name == "TRAILER!!!" {
			return nil, false, nil
		}
		data := off + pad(110+nameSize)
		if data+size > len(a) {
			return nil, false, fmt.Errorf("truncated cpio archive")
		}
		if strings.TrimPrefix(name, ".") == want {
			return a[data : data+size], true, nil
		}
		off = data + pad(size)
	}
	return nil, false, nil
}
