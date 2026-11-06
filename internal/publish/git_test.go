package publish

import (
	"slices"
	"testing"
)

func TestPorcelainFiles(t *testing.T) {
	// The first line starts with a space: " M" is a change in the
	// worktree.
	out := " M repoctl.lock\nM  repoctl.yaml\n?? keys/a.asc\nR  old -> new"
	want := []string{"repoctl.lock", "repoctl.yaml", "keys/a.asc", "old -> new"}
	if got := porcelainFiles(out); !slices.Equal(got, want) {
		t.Errorf("porcelainFiles: %q, want %q", got, want)
	}
	if got := porcelainFiles(""); len(got) != 0 {
		t.Errorf("porcelainFiles of nothing: %q", got)
	}
}
