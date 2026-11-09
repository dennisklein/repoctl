package ui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/dennisklein/repoctl/internal/config"
)

// Watch polls the files of every set on the list until ctx ends. When the
// files of a set change, it drops the cached command results of the set
// and tells the streams (topic set:NAME). Polling needs no inotify and
// sees files on network file systems too.
func (s *Server) Watch(ctx context.Context) {
	t := time.NewTicker(s.o.Poll)
	defer t.Stop()
	sigs := map[string][32]byte{}
	for {
		for _, e := range s.sets.all() {
			sig := s.signature(e)
			if old, ok := sigs[e.Name]; ok && old != sig {
				s.src.Invalidate(e.Path)
				s.notify.Notify("set:" + e.Name)
			}
			sigs[e.Name] = sig
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// signature hashes the size and time of each file that a page of the set
// shows: the set files, the git state, the package worktrees, the
// journals, the check result and the agent results.
func (s *Server) signature(e SetEntry) [32]byte {
	h := sha256.New()
	stat := func(p string) {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "%s %d %d\n", p, st.Size(), st.ModTime().UnixNano())
		} else {
			fmt.Fprintf(h, "%s -\n", p)
		}
	}
	for _, f := range []string{"repoctl.yaml", "repoctl.lock", ".repoctl/advisories.yaml", "keys/deployed.yaml",
		".git/HEAD", ".git/index", ".git/packed-refs", ".git/refs/heads", ".git/logs/HEAD"} {
		stat(filepath.Join(e.Path, f))
	}
	if set, err := config.LoadSet(e.Path); err == nil {
		for _, p := range set.Packages {
			if p.Path != "" {
				stat(filepath.Join(e.Path, p.Path))
			} else {
				stat(filepath.Join(e.Path, "packages", p.ID))
			}
		}
	}
	dirEntries(h, s.o.Paths.Runs(e.Name))
	dirEntries(h, s.o.Paths.Agent(e.Name))
	stat(s.o.Paths.CheckJSON(e.Name))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// dirEntries hashes the name, size and time of each file in dir.
func dirEntries(h hash.Hash, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(h, "%s %d %d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
}
