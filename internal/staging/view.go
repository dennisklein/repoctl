package staging

import (
	"context"
	"os"
	"path/filepath"
)

// MakeView makes the staged view of one target in dir: a repository with
// its own repodata and links to the binary RPMs of exactly the staged nodes
// of the plan (build.html#staging). mock gets the view, never the whole
// staging of an older run, so a build sees no RPM of a failed, canceled,
// older or abandoned node. It gives false when no node has a file for the
// view.
func MakeView(ctx context.Context, dir string, entries []Entry) (bool, error) {
	n := 0
	for _, e := range entries {
		for _, a := range e.Artifacts {
			if a.Kind != "rpm" || a.Path == "" {
				continue
			}
			dst := filepath.Join(dir, "Packages", filepath.Base(a.Path))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return false, err
			}
			os.Remove(dst)
			if err := os.Symlink(a.Path, dst); err != nil {
				return false, err
			}
			n++
		}
	}
	if n == 0 {
		return false, nil
	}
	_, err := Createrepo(ctx, dir)
	return err == nil, err
}
