package upstream

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/tools/git"
	"github.com/dennisklein/repoctl/internal/tools/run"
	"github.com/dennisklein/repoctl/internal/workspace"
)

// SpecAt gives the path and the text of the spec file of a package at its
// commit: git show COMMIT:SPEC in the repository of the source (the set
// repository or the bare clone of an external package). The spec is the
// file PKG.spec, else the only *.spec file, below the in-tree path or at
// the root of the package branch.
func SpecAt(ctx context.Context, src workspace.Source) (string, []byte, error) {
	if _, err := os.Stat(src.Repo); err != nil {
		return "", nil, api.Errorf(api.PWorktreeMissing, "package %s has no clone on this host", src.Package).
			WithPackage(src.Package).WithHint("clone it", "repoctl checkout "+src.Package)
	}
	g := git.New(src.Repo)
	dir := strings.Trim(src.Path, "/")
	args := []string{"ls-tree", "--name-only", src.Commit}
	if dir != "" {
		args = append(args, dir+"/")
	}
	names, err := g.Lines(ctx, args...)
	if err != nil {
		return "", nil, api.Errorf(api.PSourceUnreachable, "package %s: commit %s is not in %s", src.Package, api.ShortCommit(src.Commit), src.Repo).
			WithPackage(src.Package).WithCause(err).WithHint("fetch the package", "repoctl checkout "+src.Package)
	}
	var specs []string
	for _, n := range names {
		if strings.HasSuffix(n, ".spec") {
			specs = append(specs, n)
		}
	}
	spec := ""
	for _, s := range specs {
		if path.Base(s) == src.Package+".spec" {
			spec = s
		}
	}
	if spec == "" && len(specs) > 0 {
		spec = specs[0]
	}
	if spec == "" {
		return "", nil, api.Errorf(api.PSourceMismatch, "package %s has no spec file at %s", src.Package, api.ShortCommit(src.Commit)).WithPackage(src.Package)
	}
	text, err := g.Run(ctx, "show", src.Commit+":"+spec)
	if err != nil {
		return "", nil, err
	}
	return spec, []byte(text + "\n"), nil
}

// SpecQuery runs rpmspec -q --srpm --qf FORMAT on the spec of a package at
// its commit. %autorelease and %autochangelog are defined, so a spec of
// rpmautospec parses without the plugin.
func SpecQuery(ctx context.Context, src workspace.Source, format string) (string, error) {
	name, text, err := SpecAt(ctx, src)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "repoctl-spec-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, path.Base(name))
	if err := os.WriteFile(file, text, 0o600); err != nil {
		return "", err
	}
	out, err := (&run.Cmd{Name: "rpmspec", Args: []string{"-q", "--srpm",
		"--define", "autorelease 1", "--define", "autochangelog %{nil}",
		"--define", "_sourcedir " + dir, "--qf", format, file}}).Output(ctx)
	if err != nil {
		return "", api.Errorf(api.PGraphSRPMFailed, "rpmspec -q %s of %s: %v", path.Base(name), src.Package, err).WithPackage(src.Package).WithCause(err)
	}
	return strings.TrimSpace(string(out)), nil
}
