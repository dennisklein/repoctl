package rpmmd

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cases of the rpm test suite (rpmvercmp.at).
func TestVercmp(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0}, {"1.0", "2.0", -1}, {"2.0", "1.0", 1},
		{"2.0.1", "2.0.1", 0}, {"2.0", "2.0.1", -1}, {"2.0.1a", "2.0.1", 1},
		{"5.5p1", "5.5p2", -1}, {"5.5p10", "5.5p1", 1}, {"10xyz", "10.1xyz", -1},
		{"xyz10", "xyz10.1", -1}, {"xyz.4", "8", -1}, {"1b.fc17", "1.fc17", -1},
		{"1.0~rc1", "1.0", -1}, {"1.0~rc1", "1.0~rc2", -1}, {"1.0~rc1~git123", "1.0~rc1", -1},
		{"1.0^", "1.0", 1}, {"1.0^git1", "1.0^git2", -1}, {"1.0^git1", "1.01", -1},
		{"1.0^20160101", "1.0.1", -1}, {"1.0~rc1^git1", "1.0~rc1", 1},
		{"26.05.5", "26.05.4", 1}, {"26.05.10", "26.05.9", 1}, {"005", "5", 0},
	} {
		if got := Vercmp(c.a, c.b); got != c.want {
			t.Errorf("Vercmp(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if CompareEVR(ParseEVR("1:1.0-1"), ParseEVR("2.0-1")) != 1 {
		t.Error("the epoch wins")
	}
	if CompareEVR(ParseEVR("1.0"), ParseEVR("1.0-5")) != 0 {
		t.Error("a missing release matches any release")
	}
}

func TestSatisfies(t *testing.T) {
	for _, c := range []struct {
		prov, req string
		want      bool
	}{
		{"libhwloc.so.15()(64bit)", "libhwloc.so.15()(64bit)", true},
		{"libhwloc.so.16()(64bit)", "libhwloc.so.15()(64bit)", false},
		{"glibc = 2.34-100.el9", "glibc >= 2.34", true},
		{"glibc = 2.34-100.el9", "glibc >= 2.40", false},
		{"glibc", "glibc >= 2.40", true},
		{"pmix = 6.1.0-1.el9", "pmix < 6.1.0", false},
		{"pmix = 6.1.0-1.el9", "pmix <= 6.1.0", true},
	} {
		if got := Satisfies(ParseCap(c.prov), ParseCap(c.req)); got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v", c.prov, c.req, got)
		}
	}
}

const primary = `<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="2">
<package type="rpm">
  <name>pmix</name><arch>x86_64</arch>
  <version epoch="0" ver="6.1.0" rel="1.el9"/>
  <size package="1234" installed="5000" archive="6000"/>
  <location href="Packages/p/pmix-6.1.0-1.el9.x86_64.rpm"/>
  <format>
    <rpm:license>BSD-3-Clause</rpm:license>
    <rpm:sourcerpm>pmix-6.1.0-1.el9.src.rpm</rpm:sourcerpm>
    <rpm:provides><rpm:entry name="libpmix.so.2()(64bit)"/><rpm:entry name="pmix" flags="EQ" epoch="0" ver="6.1.0" rel="1.el9"/></rpm:provides>
    <rpm:requires><rpm:entry name="libhwloc.so.15()(64bit)"/><rpm:entry name="glibc" flags="GE" ver="2.34"/></rpm:requires>
    <file>/usr/bin/pmix_info</file>
  </format>
</package>
<package type="rpm">
  <name>pmix</name><arch>src</arch>
  <version epoch="0" ver="6.1.0" rel="1.el9"/>
  <location href="Packages/p/pmix-6.1.0-1.el9.src.rpm"/>
  <format><rpm:sourcerpm></rpm:sourcerpm></format>
</package>
</metadata>`

func writeRepo(t *testing.T) string {
	dir := t.TempDir()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(primary))
	_ = w.Close()
	if err := os.MkdirAll(filepath.Join(dir, "repodata"), 0o755); err != nil {
		t.Fatal(err)
	}
	repomd := `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo"><data type="primary"><location href="repodata/abc-primary.xml.gz"/></data></repomd>`
	for name, data := range map[string][]byte{"repomd.xml": []byte(repomd), "abc-primary.xml.gz": gz.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, "repodata", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRead(t *testing.T) {
	dir := writeRepo(t)
	if !HasRepodata(dir) {
		t.Fatal("HasRepodata")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	for _, base := range []string{dir, "file://" + dir, srv.URL + "/"} {
		pkgs, err := (&Reader{}).Read(t.Context(), base, "local")
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if len(pkgs) != 2 {
			t.Fatalf("%s: %d packages", base, len(pkgs))
		}
		p := pkgs[0]
		if p.NVRA() != "pmix-6.1.0-1.el9.x86_64" || p.SourceRPM != "pmix-6.1.0-1.el9.src.rpm" || p.License != "BSD-3-Clause" || p.Repo != "local" {
			t.Errorf("%s: %+v", base, p)
		}
		var req []string
		for _, r := range p.Requires {
			req = append(req, r.String())
		}
		if strings.Join(req, ",") != "libhwloc.so.15()(64bit),glibc >= 2.34" {
			t.Errorf("requires: %v", req)
		}
		if len(p.Files) != 1 || p.Files[0] != "/usr/bin/pmix_info" {
			t.Errorf("files: %v", p.Files)
		}
	}
}

func TestResolveMirror(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mirrorlist":
			_, _ = w.Write([]byte("# comment\nhttps://mirror.example.org/epel/9/x86_64/\nhttps://other.example.org/\n"))
		case "/metalink":
			_, _ = w.Write([]byte(`<?xml version="1.0"?><metalink xmlns="http://www.metalinker.org/"><files><file name="repomd.xml"><resources>` +
				`<url protocol="https" type="https" preference="100">https://m.example.org/9/repodata/repomd.xml</url></resources></file></files></metalink>`))
		}
	}))
	defer srv.Close()
	got, err := ResolveMirror(t.Context(), srv.Client(), "mirrorlist="+srv.URL+"/mirrorlist")
	if err != nil || got != "https://mirror.example.org/epel/9/x86_64/" {
		t.Errorf("mirrorlist: %q %v", got, err)
	}
	got, err = ResolveMirror(t.Context(), srv.Client(), "metalink="+srv.URL+"/metalink")
	if err != nil || got != "https://m.example.org/9/" {
		t.Errorf("metalink: %q %v", got, err)
	}
}
