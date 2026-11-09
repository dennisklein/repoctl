// Package lock holds repoctl.lock, the lock states and the set lock
// (docs/design/config.html#lock, docs/design/build.html#lock).
package lock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
)

// FileName is the name of the lock file in the set directory.
const FileName = "repoctl.lock"

// Entry is the line of one package in repoctl.lock.
type Entry struct {
	Branch string `yaml:"branch"`
	Commit string `yaml:"commit"`
	URL    string `yaml:"url,omitempty"`
}

// File is repoctl.lock: the pinned commit of each package.
type File struct {
	APIVersion string
	Packages   map[string]Entry
	// Order is the order of the package ids in the file.
	Order []string
	// Raw holds the bytes that Read read, for the lock digest.
	Raw []byte
}

// Digest gives sha256: of the bytes of the file that Read read.
func (f *File) Digest() string {
	sum := sha256.Sum256(f.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Get gives the entry of a package.
func (f *File) Get(id string) (Entry, bool) {
	e, ok := f.Packages[id]
	return e, ok
}

// Set writes the entry of a package.
func (f *File) Set(id string, e Entry) {
	if f.Packages == nil {
		f.Packages = map[string]Entry{}
	}
	if _, ok := f.Packages[id]; !ok {
		f.Order = append(f.Order, id)
	}
	f.Packages[id] = e
}

// Delete removes the entry of a package.
func (f *File) Delete(id string) {
	delete(f.Packages, id)
	for i, x := range f.Order {
		if x == id {
			f.Order = append(f.Order[:i], f.Order[i+1:]...)
			break
		}
	}
}

// Empty gives an empty lock.
func Empty() *File {
	return &File{APIVersion: api.APIVersion, Packages: map[string]Entry{}}
}

// Read reads repoctl.lock in the set directory. A missing file is an empty
// lock.
func Read(setDir string) (*File, error) {
	p := filepath.Join(setDir, FileName)
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		f := Empty()
		return f, nil
	}
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", p, err).WithPath(p)
	}
	return Parse(data, p)
}

// Parse reads the bytes of a lock file. path names it in messages.
func Parse(data []byte, path string) (*File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		msg := strings.TrimPrefix(err.Error(), "yaml: ")
		if strings.Contains(msg, "already defined") {
			return nil, dupError(path, msg)
		}
		if bytes.Contains(data, []byte("<<<<<<<")) {
			return nil, api.Errorf(api.PConfigInvalid, "%s has git conflict markers", path).WithPath(path).
				WithHint("keep both lines when they name two packages; keep the descendant commit when both name one package, then run repoctl checkout", "repoctl help packages")
		}
		return nil, api.Errorf(api.PConfigInvalid, "%s: %s", path, msg).WithPath(path)
	}
	f := &File{Packages: map[string]Entry{}, Raw: data}
	if len(doc.Content) == 0 {
		f.APIVersion = api.APIVersion
		return f, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, api.Errorf(api.PConfigInvalid, "%s: want a map", path).WithPath(path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "apiVersion":
			f.APIVersion = v.Value
		case "packages":
			if v.Kind != yaml.MappingNode {
				return nil, api.Errorf(api.PConfigInvalid, "%s: packages must be a map", path).WithPath(path)
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				id := v.Content[j].Value
				if _, dup := f.Packages[id]; dup {
					return nil, dupError(path, fmt.Sprintf("package %s is two times in the file", id))
				}
				var e Entry
				if err := v.Content[j+1].Decode(&e); err != nil {
					return nil, api.Errorf(api.PConfigInvalid, "%s: packages.%s: %v", path, id, err).WithPath(path)
				}
				if e.Commit == "" || e.Branch == "" {
					return nil, api.Errorf(api.PConfigInvalid, "%s: packages.%s needs branch and commit", path, id).WithPath(path)
				}
				f.Packages[id] = e
				f.Order = append(f.Order, id)
			}
		default:
			return nil, api.Errorf(api.PConfigUnknownKey, "%s: unknown key %s", path, k.Value).WithPath(path)
		}
	}
	if f.APIVersion == "" {
		f.APIVersion = api.APIVersion
	}
	if f.APIVersion != api.APIVersion {
		return nil, api.Errorf(api.PContractNewerFile, "%s: apiVersion %s is newer than this repoctl", path, f.APIVersion).WithPath(path).
			WithHint("upgrade repoctl on this host", "repoctl help upgrade")
	}
	return f, nil
}

func dupError(path, msg string) error {
	return api.Errorf(api.PConfigInvalid, "%s: %s", path, msg).WithPath(path).
		WithHint("keep the line whose commit is the descendant (git merge-base --is-ancestor OLD NEW), then run repoctl checkout", "repoctl help packages")
}

// Header is the first line of a lock file.
const Header = "# Written by repoctl pin. Do not edit."

// flowMax is the longest flow line of an entry; a longer entry is written
// as a block.
const flowMax = 100

// Format gives the bytes of the lock file. order gives the package order,
// usually the order of repoctl.yaml; entries of other packages follow, sorted.
// An entry without url that fits in 100 columns is one flow line; the flow
// lines align their key and branch columns.
func (f *File) Format(order []string) []byte {
	var ids []string
	seen := map[string]bool{}
	for _, id := range order {
		if _, ok := f.Packages[id]; ok && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	var rest []string
	for id := range f.Packages {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	ids = append(ids, rest...)

	flowLine := func(id string, e Entry, keyW, brW int) string {
		return fmt.Sprintf("  %-*s {branch: %-*s commit: %s}", keyW, id+":", brW, e.Branch+",", e.Commit)
	}
	flow := map[string]bool{}
	keyW, brW := 0, 0
	for _, id := range ids {
		if e := f.Packages[id]; e.URL == "" && len(flowLine(id, e, 0, 0)) <= flowMax {
			flow[id] = true
			keyW = max(keyW, len(id)+1)
			brW = max(brW, len(e.Branch)+1)
		}
	}
	var b bytes.Buffer
	b.WriteString(Header + "\n")
	b.WriteString("apiVersion: " + api.APIVersion + "\n")
	if len(ids) == 0 {
		b.WriteString("packages: {}\n")
		return b.Bytes()
	}
	b.WriteString("packages:\n")
	for _, id := range ids {
		e := f.Packages[id]
		if flow[id] {
			b.WriteString(strings.TrimRight(flowLine(id, e, keyW, brW), " ") + "\n")
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", id)
		if e.URL != "" {
			fmt.Fprintf(&b, "    url: %s\n", e.URL)
		}
		fmt.Fprintf(&b, "    branch: %s\n    commit: %s\n", e.Branch, e.Commit)
	}
	return b.Bytes()
}

// LockFileName is the file of the file lock beside repoctl.lock (the lock
// of go-clikit fsutil.Lock). A set directory excludes it from git.
const LockFileName = ".repoctl.lock.lock"

// fileLockWait is how long a writer waits for the file lock of repoctl.lock.
const fileLockWait = 30 * time.Second

// Write writes the lock file atomically under its file lock. It is the
// only writer of repoctl.lock: pin, rebuild, init and migrate call it.
func (f *File) Write(setDir string, order []string) error {
	return f.WriteContext(context.Background(), setDir, order)
}

// WriteContext is Write with a context for the wait for the file lock.
func (f *File) WriteContext(ctx context.Context, setDir string, order []string) error {
	p := filepath.Join(setDir, FileName)
	unlock, err := fsutil.Lock(ctx, p, fileLockWait)
	if err != nil {
		return api.Errorf(api.PConfigInvalid, "write %s: %v", p, err).WithPath(p)
	}
	defer unlock()
	return f.write(p, order)
}

func (f *File) write(p string, order []string) error {
	data := f.Format(order)
	if err := fsutil.WriteAtomic(p, data, 0o644); err != nil {
		return api.Errorf(api.PConfigInvalid, "write %s: %v", p, err).WithPath(p)
	}
	f.Raw = data
	return nil
}

// Update reads repoctl.lock under its file lock, gives it to change and
// writes the result atomically. When change returns an error, Update
// writes nothing and returns the error. It gives the written file.
func Update(ctx context.Context, setDir string, order []string, change func(f *File) error) (*File, error) {
	p := filepath.Join(setDir, FileName)
	unlock, err := fsutil.Lock(ctx, p, fileLockWait)
	if err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "write %s: %v", p, err).WithPath(p)
	}
	defer unlock()
	f, err := Read(setDir)
	if err != nil {
		return nil, err
	}
	if err := change(f); err != nil {
		return nil, err
	}
	if err := f.write(p, order); err != nil {
		return nil, err
	}
	return f, nil
}
