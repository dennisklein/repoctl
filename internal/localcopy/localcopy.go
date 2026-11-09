// Package localcopy keeps the local copy of latest of each target
// (publish.html#pull): $XDG_DATA_HOME/repoctl/SET/repo/TARGET/ with the
// three repositories, the root files and snapshot.json, the cache of the
// annotations of the snapshot. It also gives the freshness check, which
// compares the local copy with latest (publish.html#freshness).
package localcopy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/snapshot"
)

// SnapshotFile is the name of the cache of the annotations in a local
// copy.
const SnapshotFile = "snapshot.json"

// Dir gives the local copy of a target.
func Dir(p config.Paths, set, target string) string { return p.LocalCopy(set, target) }

// ReadSnapshot reads snapshot.json of a local copy. It gives nil without an
// error when the target has no local copy.
func ReadSnapshot(dir string) (*api.Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, SnapshotFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s api.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", filepath.Join(dir, SnapshotFile), err)
	}
	return &s, nil
}

// WriteSnapshot writes snapshot.json into a tree: the Snapshot without
// local, served and gc.
func WriteSnapshot(dir string, s api.Snapshot) error {
	s.Local, s.Served, s.GC = "", "", ""
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, SnapshotFile), append(b, '\n'))
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Have indexes the files of trees by their digest, for Download: a file
// that a tree holds is linked, not downloaded. A tree that does not exist
// adds nothing.
func Have(trees ...string) (func(digest.Digest) string, error) {
	idx := map[digest.Digest]string{}
	for _, t := range trees {
		if _, err := os.Stat(t); err != nil {
			continue
		}
		files, err := snapshot.Scan(t)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			idx[f.Digest] = f.Local
		}
	}
	return func(d digest.Digest) string { return idx[d] }, nil
}

// Install makes newTree the local copy dir in one rename: it exchanges the
// two directories (renameat2 RENAME_EXCHANGE) and removes the old tree. A
// file system without the exchange gets two renames. newTree must be on
// the file system of dir.
func Install(newTree, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return os.Rename(newTree, dir)
	}
	err := unix.Renameat2(unix.AT_FDCWD, newTree, unix.AT_FDCWD, dir, unix.RENAME_EXCHANGE)
	if err == nil {
		// newTree now holds the old local copy.
		return os.RemoveAll(newTree)
	}
	old := dir + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(dir, old); err != nil {
		return err
	}
	if err := os.Rename(newTree, dir); err != nil {
		_ = os.Rename(old, dir)
		return err
	}
	return os.RemoveAll(old)
}

// Temp makes a new empty directory next to the local copy dir, on the same
// file system, for a tree that Install takes.
func Temp(dir, prefix string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"."+prefix+"-")
}
