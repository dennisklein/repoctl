package localcopy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

func TestInstall(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "local", "el9")
	for i, want := range []string{"one", "two"} {
		tmp, err := Temp(dir, "pull")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "f"), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Install(tmp, dir); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "f"))
		if err != nil || string(b) != want {
			t.Fatalf("install %d: %q %v", i, b, err)
		}
		if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("install %d left %s", i, tmp)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(dir))
	if len(ents) != 1 {
		t.Errorf("the parent holds %d entries, want only the local copy", len(ents))
	}
}

func TestSnapshotFile(t *testing.T) {
	dir := t.TempDir()
	if s, err := ReadSnapshot(dir); s != nil || err != nil {
		t.Fatalf("no local copy: %v %v", s, err)
	}
	in := api.Snapshot{Target: "el9", Publish: "p-20260912-1403-5e1", Digest: "sha256:" + strings.Repeat("a", 64), Kind: "publish", Local: "same", GC: "keep"}
	if err := WriteSnapshot(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSnapshot(dir)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Publish != in.Publish || got.Local != "" || got.GC != "" {
		t.Errorf("snapshot.json keeps local and gc: %+v", got)
	}
}

func TestFreshnessErrors(t *testing.T) {
	cases := map[string]string{
		FreshMissing:     api.PSnapshotMissing,
		FreshUnpushed:    api.PStateUnpushed,
		FreshNotCached:   api.PStateBehind,
		FreshBehind:      api.PStateBehind,
		FreshUnreachable: api.PRegistryUnreachable,
	}
	for result, code := range cases {
		f := Freshness{Target: "el9", Result: result, Publish: "p-20260912-1403-5e1", Err: errors.New("dial tcp: refused")}
		e, ok := api.AsError(f.PublishErr())
		if !ok || e.Code != code {
			t.Errorf("%s: %v, want %s", result, f.PublishErr(), code)
		}
	}
	for _, ok := range []string{FreshSame, FreshFirst} {
		if err := (Freshness{Result: ok}).PublishErr(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if (Freshness{Result: FreshSame}).Warning() != nil || (Freshness{Result: FreshMissing}).Warning() == nil {
		t.Error("Warning")
	}
}
