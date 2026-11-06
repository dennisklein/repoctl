package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dennisklein/repoctl/internal/api"
)

// ErrNoSet is the cause of a discovery that finds no repoctl.yaml.
var ErrNoSet = errors.New("no set")

// Discover finds the set directory: the first directory with a repoctl.yaml,
// from start up to the file system root or a mount point (config.html#discovery).
// It does not stop at a .git file, so a command in a package worktree finds
// the set above it.
func Discover(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", api.Errorf(api.PConfigInvalid, "%s is not a directory", start).
			WithHint("give the set directory", "repoctl -C DIR status")
	}
	startDev := device(dir)
	for {
		if fi, err := os.Stat(filepath.Join(dir, SetFile)); err == nil && fi.Mode().IsRegular() {
			if !inGitWorkTree(dir) {
				return "", api.Errorf(api.PConfigInvalid, "set %s is not in a git work tree", dir).WithPath(dir).
					WithHint("make the set directory a git work tree", "git init")
			}
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir || device(parent) != startDev {
			break
		}
		dir = parent
	}
	return "", NoSet(start, "status")
}

// NoSet is the error of a command that needs a set and found none from
// start. The hint runs command (for example "plan") again with -C.
func NoSet(start, command string) error {
	abs, _ := filepath.Abs(start)
	return api.Errorf(api.PConfigInvalid, "no set: no repoctl.yaml in %s or in a parent directory", abs).
		WithCause(ErrNoSet).
		WithHint("give the set directory", "repoctl -C ~/src/slurm-repo "+command).
		WithHint("start a new set here", "repoctl init")
}

// IsNoSet reports an error of Discover that found no set.
func IsNoSet(err error) bool { return errors.Is(err, ErrNoSet) }

func device(dir string) uint64 {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert // the type differs per platform
	}
	return 0
}

// inGitWorkTree reports a .git (directory or file) in dir or a parent.
func inGitWorkTree(dir string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
