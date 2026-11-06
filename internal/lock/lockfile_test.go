package lock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

const (
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333"
)

func code(err error) api.ProblemCode {
	if e, ok := api.AsError(err); ok {
		return e.Code
	}
	return ""
}

func TestParseFormatRoundTrip(t *testing.T) {
	text := `# Written by repoctl pin. Do not edit.
apiVersion: repoctl/v1alpha1
packages:
  munge: {branch: munge/slurm-26.05, commit: ` + c1 + `}
  ucx:   {branch: ucx/slurm-26.05,   commit: ` + c2 + `}
  rocm-smi:
    url: https://git.gsi.de/hpc/rocm-smi.git
    branch: slurm-26.05
    commit: ` + c3 + `
`
	f, err := Parse([]byte(text), "repoctl.lock")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Order, " "); got != "munge ucx rocm-smi" {
		t.Errorf("order %s", got)
	}
	if e, _ := f.Get("rocm-smi"); e.URL != "https://git.gsi.de/hpc/rocm-smi.git" || e.Commit != c3 {
		t.Errorf("rocm-smi %+v", e)
	}
	if got := string(f.Format(f.Order)); got != text {
		t.Errorf("Format:\n%s\nwant:\n%s", got, text)
	}
	if f.Digest() != "sha256:"+sha([]byte(text)) {
		t.Errorf("digest %s", f.Digest())
	}
}

func TestFormatOrderAndLongLines(t *testing.T) {
	f := Empty()
	f.Set("zeta", Entry{Branch: "zeta/b", Commit: c1})
	f.Set("alpha", Entry{Branch: "alpha/b", Commit: c2})
	f.Set("slurm-singularity-exec", Entry{Branch: "slurm-singularity-exec/slurm-26.05", Commit: c3})
	out := string(f.Format([]string{"slurm-singularity-exec", "zeta"}))
	want := `# Written by repoctl pin. Do not edit.
apiVersion: repoctl/v1alpha1
packages:
  slurm-singularity-exec:
    branch: slurm-singularity-exec/slurm-26.05
    commit: ` + c3 + `
  zeta:  {branch: zeta/b,  commit: ` + c1 + `}
  alpha: {branch: alpha/b, commit: ` + c2 + `}
`
	if out != want {
		t.Errorf("Format:\n%s\nwant:\n%s", out, want)
	}
	for _, l := range strings.Split(out, "\n") {
		if len(l) > flowMax || strings.HasSuffix(l, " ") {
			t.Errorf("line %q", l)
		}
	}
	if _, err := Parse([]byte(out), "x"); err != nil {
		t.Fatal(err)
	}
	if got := string(Empty().Format(nil)); !strings.HasSuffix(got, "packages: {}\n") {
		t.Errorf("empty lock:\n%s", got)
	}
}

func TestParseRefuses(t *testing.T) {
	head := "apiVersion: repoctl/v1alpha1\npackages:\n"
	for name, tc := range map[string]struct {
		text string
		code api.ProblemCode
		msg  string
	}{
		"duplicate": {head + "  a: {branch: b, commit: " + c1 + "}\n  a: {branch: b, commit: " + c2 + "}\n", api.PConfigInvalid, ""},
		"conflict": {head + "<<<<<<< HEAD\n  a: {branch: b, commit: " + c1 + "}\n=======\n  a: {branch: b, commit: " + c2 + "}\n>>>>>>> x\n",
			api.PConfigInvalid, "conflict markers"},
		"no commit":   {head + "  a: {branch: b}\n", api.PConfigInvalid, "needs branch and commit"},
		"unknown key": {head + "  a: {branch: b, commit: " + c1 + "}\nextra: 1\n", api.PConfigUnknownKey, ""},
		"newer":       {"apiVersion: repoctl/v1alpha2\npackages: {}\n", api.PContractNewerFile, ""},
		"not a map":   {"- a\n", api.PConfigInvalid, ""},
	} {
		_, err := Parse([]byte(tc.text), "repoctl.lock")
		if code(err) != tc.code || !strings.Contains(errMsg(err), tc.msg) {
			t.Errorf("%s: %v (%s)", name, err, code(err))
		}
	}
	// An empty file is an empty lock.
	f, err := Parse(nil, "repoctl.lock")
	if err != nil || len(f.Packages) != 0 || f.APIVersion != api.APIVersion {
		t.Errorf("empty file: %v %+v", err, f)
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSetDelete(t *testing.T) {
	f := Empty()
	f.Set("a", Entry{Branch: "a", Commit: c1})
	f.Set("b", Entry{Branch: "b", Commit: c2})
	f.Set("a", Entry{Branch: "a", Commit: c3})
	if strings.Join(f.Order, " ") != "a b" {
		t.Errorf("order %v", f.Order)
	}
	f.Delete("a")
	if _, ok := f.Get("a"); ok || strings.Join(f.Order, " ") != "b" {
		t.Errorf("after Delete: %v %v", f.Packages, f.Order)
	}
}

func TestWriteAndUpdate(t *testing.T) {
	dir := t.TempDir()
	// A missing file reads as an empty lock.
	f, err := Read(dir)
	if err != nil || len(f.Packages) != 0 {
		t.Fatalf("Read of a missing file: %v", err)
	}
	f.Set("a", Entry{Branch: "a/b", Commit: c1})
	if err := f.Write(dir, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(f.Raw) || !strings.Contains(string(data), c1) {
		t.Errorf("written:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); err != nil {
		t.Errorf("the file lock is not beside repoctl.lock: %v", err)
	}

	// Update writes nothing when the change fails.
	stop := errors.New("stop")
	if _, err := Update(context.Background(), dir, nil, func(f *File) error {
		f.Set("b", Entry{Branch: "b", Commit: c2})
		return stop
	}); !errors.Is(err, stop) {
		t.Fatalf("Update: %v", err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, FileName)); string(again) != string(data) {
		t.Errorf("a failed Update wrote:\n%s", again)
	}

	// Parallel updates of two packages keep both lines.
	var wg sync.WaitGroup
	for i, id := range []string{"b", "c", "d", "e"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commit := []string{c1, c2, c3, c1}[i]
			if _, err := Update(context.Background(), dir, nil, func(f *File) error {
				f.Set(id, Entry{Branch: id, Commit: commit})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Packages) != 5 {
		t.Errorf("after parallel updates: %v", f.Order)
	}
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
