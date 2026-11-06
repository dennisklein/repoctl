package publish

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadRunSkipsUnpinned checks that a run from worktrees is never
// staged: readRun refuses it, as the staged index of the plan does.
func TestReadRunSkipsUnpinned(t *testing.T) {
	dir := t.TempDir()
	write := func(name, from string) string {
		p := filepath.Join(dir, name)
		line := `{"v":1,"seq":1,"type":"run.started","run":"r-20261102-1750-a3f","kind":"build","origin":{"via":"cli","user":"u","host":"h"},"selection":{"from":"` + from + `"}}` + "\n"
		if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := readRun(write("pinned.jsonl", "")); err != nil {
		t.Errorf("pinned run: %v", err)
	}
	if _, err := readRun(write("worktree.jsonl", "worktree")); err == nil {
		t.Error("worktree run: readRun gave no error")
	}
}
