package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// Exclude adds patterns to .git/info/exclude of the set repository, each
// once. checkout adds each worktree path, and the writers of repoctl.lock
// add the file of its file lock.
func Exclude(ctx context.Context, setDir string, patterns ...string) error {
	p, err := git.New(setDir).GitPath(ctx, "info/exclude")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add bytes.Buffer
	for _, pat := range patterns {
		if !have[pat] {
			add.WriteString(pat + "\n")
			have[pat] = true
		}
	}
	if add.Len() == 0 {
		return nil
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(data, add.Bytes()...), 0o644)
}

// excludeWorktree gives the exclude pattern of packages/PKG.
func excludeWorktree(pkg string) string { return "/" + PackagesDir + "/" + pkg + "/" }

// excludeLockFile gives the exclude pattern of the file lock of
// repoctl.lock.
func excludeLockFile() string { return "/" + lock.LockFileName }
