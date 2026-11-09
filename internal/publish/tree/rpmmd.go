package tree

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Repomd is repodata/repomd.xml of one repository.
type Repomd struct {
	Revision string       `xml:"revision"`
	Data     []RepomdData `xml:"data"`
}

// RepomdData is one data entry of repomd.xml.
type RepomdData struct {
	Type     string `xml:"type,attr"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Size int64 `xml:"size"`
}

// ReadRepomd reads repodata/repomd.xml of a repository directory. A
// directory without it gives an error that os.IsNotExist reports.
func ReadRepomd(dir string) (*Repomd, error) {
	b, err := os.ReadFile(filepath.Join(dir, "repodata", "repomd.xml"))
	if err != nil {
		return nil, err
	}
	var md Repomd
	if err := xml.Unmarshal(b, &md); err != nil {
		return nil, api.Errorf(api.PVerifyFailed, "%s: repomd.xml is not valid: %v", dir, err)
	}
	return &md, nil
}

// DataOf gives the entry of a type.
func (m *Repomd) DataOf(typ string) (RepomdData, bool) {
	for _, d := range m.Data {
		if d.Type == typ {
			return d, true
		}
	}
	return RepomdData{}, false
}

// Package is one package of primary.xml.
type Package struct {
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	} `xml:"version"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Summary string `xml:"summary"`
	Time    struct {
		Build int64 `xml:"build,attr"`
	} `xml:"time"`
	Size struct {
		Package int64 `xml:"package,attr"`
	} `xml:"size"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Format struct {
		License   string `xml:"license"`
		Sourcerpm string `xml:"sourcerpm"`
	} `xml:"format"`
}

// Epoch gives the epoch as a number.
func (p Package) Epoch() int {
	n, _ := strconv.Atoi(p.Version.Epoch)
	return n
}

// NVRA gives name-version-release.arch.
func (p Package) NVRA() string {
	return p.Name + "-" + p.Version.Ver + "-" + p.Version.Rel + "." + p.Arch
}

// EVRA gives epoch:version-release.arch, the form of repo ls.
func (p Package) EVRA() string {
	return fmt.Sprintf("%d:%s-%s.%s", p.Epoch(), p.Version.Ver, p.Version.Rel, p.Arch)
}

// File gives the file name of the package.
func (p Package) File() string { return filepath.Base(p.Location.Href) }

type primary struct {
	Packages []Package `xml:"package"`
}

// ReadPrimary reads the packages of primary.xml of a repository directory.
func ReadPrimary(ctx context.Context, dir string) (*Repomd, []Package, error) {
	md, err := ReadRepomd(dir)
	if err != nil {
		return nil, nil, err
	}
	d, ok := md.DataOf("primary")
	if !ok {
		return md, nil, api.Errorf(api.PVerifyFailed, "%s: repomd.xml lists no primary data", dir)
	}
	b, err := ReadData(ctx, filepath.Join(dir, filepath.FromSlash(d.Location.Href)))
	if err != nil {
		return md, nil, err
	}
	var p primary
	if err := xml.Unmarshal(b, &p); err != nil {
		return md, nil, api.Errorf(api.PVerifyFailed, "%s: primary.xml is not valid: %v", dir, err)
	}
	return md, p.Packages, nil
}

// ReadData reads a repodata file and decompresses it by its suffix: .gz,
// .bz2, .xz or .zst.
func ReadData(ctx context.Context, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	switch {
	case strings.HasSuffix(path, ".gz"):
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	case strings.HasSuffix(path, ".bz2"):
		return io.ReadAll(bzip2.NewReader(f))
	case strings.HasSuffix(path, ".xz"):
		return (&run.Cmd{Name: "xz", Args: []string{"-dc"}, Stdin: f}).Output(ctx)
	case strings.HasSuffix(path, ".zst"):
		return (&run.Cmd{Name: "zstd", Args: []string{"-dc"}, Stdin: f}).Output(ctx)
	}
	return io.ReadAll(f)
}

// SHA256File gives the sha256 of a file as 64 hex digits.
func SHA256File(path string) (string, error) {
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

// SHA256Bytes gives the sha256 of bytes as 64 hex digits.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Createrepo runs createrepo_c --update on a repository directory. The
// metadata is gzip, which the EL clients read.
func Createrepo(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var out bytes.Buffer
	c := &run.Cmd{Name: "createrepo_c", Args: []string{"--update", "--quiet", "--general-compress-type=gz", "--no-database", dir}, Stdout: &out}
	if err := c.Run(ctx); err != nil {
		return fmt.Errorf("createrepo_c %s: %w", dir, err)
	}
	return nil
}

// AddMetadata adds a file to the repodata of a repository with
// modifyrepo_c --mdtype=TYPE. The file goes in gzip compressed:
// modifyrepo_c 0.17 cannot detect the compression of a plain JSON file.
func AddMetadata(ctx context.Context, dir, file, mdtype string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	gz := file + ".gz"
	f, err := os.Create(gz)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	c := &run.Cmd{Name: "modifyrepo_c", Args: []string{"--mdtype=" + mdtype, gz, filepath.Join(dir, "repodata")}}
	if err := c.Run(ctx); err != nil {
		return fmt.Errorf("modifyrepo_c %s: %w", dir, err)
	}
	return nil
}
