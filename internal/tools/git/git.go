// Package git is the adapter of git: each call is one git child process.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dennisklein/repoctl/internal/tools/run"
)

// Git runs git in a directory.
type Git struct {
	// Dir is the directory of the git commands (git -C DIR).
	Dir string
	// Env is added to the environment of each git command.
	Env []string
}

// New gives a Git for the directory.
func New(dir string) *Git { return &Git{Dir: dir} }

func (g *Git) cmd(args []string) *run.Cmd {
	return &run.Cmd{Name: "git", Args: append([]string{"-C", g.Dir}, args...), Env: append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, g.Env...)}
}

// Run runs git with the arguments and gives its stdout without the final
// newline.
func (g *Git) Run(ctx context.Context, args ...string) (string, error) {
	out, err := g.cmd(args).Output(ctx)
	return strings.TrimRight(string(out), "\n"), err
}

// Output runs git with the arguments and gives its stdout as is.
func (g *Git) Output(ctx context.Context, args ...string) ([]byte, error) {
	return g.cmd(args).Output(ctx)
}

// Lines runs git and gives the lines of its stdout.
func (g *Git) Lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := g.Run(ctx, args...)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// RevParse gives the commit of a revision, or "" when it does not exist.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		if run.ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// HasCommit reports whether the commit object is in the repository.
func (g *Git) HasCommit(ctx context.Context, commit string) bool {
	if commit == "" {
		return false
	}
	_, err := g.Run(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// IsAncestor reports git merge-base --is-ancestor a b.
func (g *Git) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := g.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if run.ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// Count gives the commits that only a has and the commits that only b has
// (git rev-list --left-right --count a...b).
func (g *Git) Count(ctx context.Context, a, b string) (onlyA, onlyB int, err error) {
	out, err := g.Run(ctx, "rev-list", "--left-right", "--count", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("git rev-list --count: unexpected output %q", out)
	}
	onlyA, _ = strconv.Atoi(f[0])
	onlyB, _ = strconv.Atoi(f[1])
	return onlyA, onlyB, nil
}

// Subject gives the subject line of a commit.
func (g *Git) Subject(ctx context.Context, commit string) (string, error) {
	return g.Run(ctx, "log", "-1", "--format=%s", commit)
}

// Dirty reports uncommitted changes of tracked files.
func (g *Git) Dirty(ctx context.Context) (bool, error) {
	files, err := g.Changed(ctx)
	return len(files) > 0, err
}

// Changed gives the tracked files with uncommitted changes (staged or not),
// below the paths when paths are given.
func (g *Git) Changed(ctx context.Context, paths ...string) ([]string, error) {
	args := []string{"status", "--porcelain", "--untracked-files=no"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	out, err := g.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if len(l) > 3 {
			files = append(files, strings.TrimSpace(l[3:]))
		}
	}
	return files, nil
}

// Toplevel gives the root of the work tree of Dir.
func (g *Git) Toplevel(ctx context.Context) (string, error) {
	return g.Run(ctx, "rev-parse", "--show-toplevel")
}

// CommonDir gives the absolute common git directory of Dir: the .git
// directory of the main worktree, or the bare repository.
func (g *Git) CommonDir(ctx context.Context) (string, error) {
	return g.absPath(g.Run(ctx, "rev-parse", "--git-common-dir"))
}

// GitPath gives the absolute path of a file in the git directory, as git
// rev-parse --git-path gives it (for example info/exclude).
func (g *Git) GitPath(ctx context.Context, name string) (string, error) {
	return g.absPath(g.Run(ctx, "rev-parse", "--git-path", name))
}

func (g *Git) absPath(p string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.Dir, p)
	}
	return filepath.Clean(p), nil
}

// CurrentBranch gives the branch of HEAD, or "" for a detached HEAD.
func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if run.ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// Branch gives the commit of the local branch, or "" when it does not
// exist.
func (g *Git) Branch(ctx context.Context, branch string) (string, error) {
	return g.RevParse(ctx, "refs/heads/"+branch)
}

// Fetch runs git fetch from the remote with the refspecs.
func (g *Git) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	_, err := g.Run(ctx, append([]string{"fetch", "--quiet", remote}, refspecs...)...)
	return err
}

// LsRemote gives the commit of the branch on the remote (a remote name or
// a URL), or "" when the remote does not have the branch. An error means
// that the remote did not answer.
func (g *Git) LsRemote(ctx context.Context, remote, branch string) (string, error) {
	out, err := g.Run(ctx, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == "refs/heads/"+branch {
			return f[0], nil
		}
	}
	return "", nil
}

// ConfigSet sets a git config key of the repository.
func (g *Git) ConfigSet(ctx context.Context, key, value string) error {
	_, err := g.Run(ctx, "config", key, value)
	return err
}

// Show gives the bytes of a file at a revision (git show REV:PATH).
func (g *Git) Show(ctx context.Context, rev, path string) ([]byte, error) {
	return g.Output(ctx, "show", rev+":"+path)
}

// Worktree is one entry of git worktree list.
type Worktree struct {
	Path string
	Head string
	// Branch is the short branch name, or "" for a detached HEAD.
	Branch string
	Bare   bool
}

// Worktrees gives the worktrees of the repository of Dir.
func (g *Git) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := g.Output(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	for _, block := range bytes.Split(out, []byte("\n\n")) {
		var w Worktree
		for _, l := range strings.Split(string(block), "\n") {
			k, v, _ := strings.Cut(l, " ")
			switch k {
			case "worktree":
				w.Path = v
			case "HEAD":
				w.Head = v
			case "branch":
				w.Branch = strings.TrimPrefix(v, "refs/heads/")
			case "bare":
				w.Bare = true
			}
		}
		if w.Path != "" {
			list = append(list, w)
		}
	}
	return list, nil
}

// ErrNotFound is the error of a missing object.
var ErrNotFound = errors.New("not found")
