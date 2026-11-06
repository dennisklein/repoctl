package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// SetSource is a set that init --from and checkout --create --from-set
// fork: a set directory, or a set branch of the set repository, for
// example set/slurm-26.05 (lifecycle.html#init-from).
type SetSource struct {
	// Ref is SET as the human gave it.
	Ref string
	// Dir is the set directory; "" for a branch.
	Dir string
	// Rev is the revision of the branch, for example origin/set/slurm-26.05.
	Rev  string
	repo *git.Git
}

// OpenSetSource finds SET: a directory with repoctl.yaml, else a branch of
// the repository of dir, local or on the remote.
func OpenSetSource(ctx context.Context, dir, remote, ref string) (*SetSource, error) {
	cand := ref
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(dir, ref)
	}
	if fi, err := os.Stat(filepath.Join(cand, config.SetFile)); err == nil && fi.Mode().IsRegular() {
		abs, _ := filepath.Abs(cand)
		return &SetSource{Ref: ref, Dir: abs}, nil
	}
	g := git.New(dir)
	for _, rev := range []string{ref, "refs/remotes/" + remote + "/" + ref, "refs/heads/" + ref} {
		c, err := g.RevParse(ctx, rev)
		if err == nil && c != "" {
			if _, err := g.Show(ctx, rev, config.SetFile); err == nil {
				return &SetSource{Ref: ref, Rev: rev, repo: g}, nil
			}
		}
	}
	return nil, api.Errorf(api.PConfigInvalid, "%s is neither a set directory nor a set branch with %s", ref, config.SetFile).
		WithHint("give a set branch such as set/slurm-26.05, or a set directory", "git branch -a --list '*set/*'")
}

// ReadFile gives a file of the set, by its path in the set.
func (s *SetSource) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if s.Dir != "" {
		return os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(path)))
	}
	return s.repo.Show(ctx, s.Rev, path)
}

// Files gives the files below a path of the set, as paths in the set.
func (s *SetSource) Files(ctx context.Context, path string) ([]string, error) {
	if s.Dir != "" {
		var out []string
		root := filepath.Join(s.Dir, filepath.FromSlash(path))
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if d.Type().IsRegular() {
				rel, _ := filepath.Rel(s.Dir, p)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		return out, err
	}
	return s.repo.Lines(ctx, "ls-tree", "-r", "--name-only", s.Rev, "--", strings.TrimSuffix(path, "/")+"/")
}

// Config gives repoctl.yaml of the set: the parsed set and its bytes.
func (s *SetSource) Config(ctx context.Context) (*config.Set, []byte, error) {
	data, err := s.ReadFile(ctx, config.SetFile)
	if err != nil {
		return nil, nil, api.Errorf(api.PConfigInvalid, "%s: read %s: %v", s.Ref, config.SetFile, err)
	}
	set, err := config.ParseSet(data, s.Ref+":"+config.SetFile)
	return set, data, err
}

// Lock gives repoctl.lock of the set; a set without one has an empty lock.
func (s *SetSource) Lock(ctx context.Context) (*lock.File, error) {
	data, err := s.ReadFile(ctx, lock.FileName)
	if err != nil {
		return lock.Empty(), nil
	}
	return lock.Parse(data, s.Ref+":"+lock.FileName)
}
