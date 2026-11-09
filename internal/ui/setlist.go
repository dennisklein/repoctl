package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"
)

// SetEntry is one set of the set list: its name and its directory.
type SetEntry struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type setListFile struct {
	Sets []SetEntry `yaml:"sets"`
}

// setList is the set list of the UI, $XDG_STATE_HOME/repoctl/-ui/sets.yaml.
// The server keeps it in memory and writes it on each change.
type setList struct {
	path string
	mu   sync.Mutex
	sets []SetEntry
}

func loadSetList(path string) (*setList, error) {
	l := &setList{path: path}
	sets, err := ReadSetList(path)
	if err != nil {
		return nil, err
	}
	l.sets = sets
	return l, nil
}

// ReadSetList reads the set list at path; a missing file is an empty list.
func ReadSetList(path string) ([]SetEntry, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the user's own state file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f setListFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Sets, nil
}

// WriteSetList writes the set list at path, through a temporary file.
func WriteSetList(path string, sets []SetEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(setListFile{Sets: sets})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveFromSetList removes the set name from the set list at path. It
// reports whether the set was on the list.
func RemoveFromSetList(path, name string) (bool, error) {
	sets, err := ReadSetList(path)
	if err != nil {
		return false, err
	}
	n := len(sets)
	sets = slices.DeleteFunc(sets, func(e SetEntry) bool { return e.Name == name })
	if len(sets) == n {
		return false, nil
	}
	return true, WriteSetList(path, sets)
}

// all gives a copy of the list.
func (l *setList) all() []SetEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sets)
}

// get gives the set name.
func (l *setList) get(name string) (SetEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.sets {
		if e.Name == name {
			return e, true
		}
	}
	return SetEntry{}, false
}

// add puts the set on the list, or moves its directory, and reports
// whether the list changed.
func (l *setList) add(e SetEntry) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.sets {
		if s.Name == e.Name {
			if s.Path == e.Path {
				return false, nil
			}
			l.sets[i].Path = e.Path
			return true, WriteSetList(l.path, l.sets)
		}
	}
	l.sets = append(l.sets, e)
	return true, WriteSetList(l.path, l.sets)
}

// remove takes the set off the list and reports whether it was on it.
func (l *setList) remove(name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.sets)
	l.sets = slices.DeleteFunc(l.sets, func(e SetEntry) bool { return e.Name == name })
	if len(l.sets) == n {
		return false, nil
	}
	return true, WriteSetList(l.path, l.sets)
}
