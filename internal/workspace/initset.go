package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GSI-HPC/go-clikit/fsutil"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
	"github.com/dennisklein/repoctl/internal/tools/git"
)

// DefaultTarget is the target of a new set without --add-target.
var DefaultTarget = config.Target{ID: "el9", Mock: "rocky+epel-9-x86_64", Plugins: []string{"rpmautospec"}}

// InitOptions are the flags of init (cli.html#cmd-init).
type InitOptions struct {
	// Dir is the start directory.
	Dir string
	// Name is --name; "" takes the set branch set/NAME, else the base name
	// of the directory.
	Name string
	// Repo is --repo; "" is the first part of the set name.
	Repo string
	// Targets are the --add-target values; none is DefaultTarget.
	Targets []config.Target
	// From is --from SET.
	From  string
	Force bool
}

// InitReport is the result of init.
type InitReport struct {
	Set   *config.Set
	Files []api.File
	// The facts of init --from (lifecycle.html#init-from).
	From    string
	OldName string
	Fork    bool
	Renamed int
	Shared  []string
	Dropped []config.Key
	Deleted []string
	Copied  []string
	Skipped []string
}

// CheckWorkTree gives config.invalid when dir is not in a git work tree.
func CheckWorkTree(ctx context.Context, dir string) error {
	if _, err := git.New(dir).Toplevel(ctx); err != nil {
		return api.Errorf(api.PConfigInvalid, "%s is not in a git work tree", dir).WithPath(dir).
			WithHint("make the directory a git work tree", "git init")
	}
	return nil
}

// DefaultName gives the set name of a new set: NAME of the branch
// set/NAME, else the base name of the directory.
func DefaultName(ctx context.Context, dir string) string {
	if b, err := git.New(dir).CurrentBranch(ctx); err == nil {
		if n, ok := strings.CutPrefix(b, "set/"); ok && n != "" {
			return n
		}
	}
	return strings.ToLower(filepath.Base(dir))
}

// DefaultRepo gives the repository name of a set: the first part of the
// set name, for example slurm of slurm-26.05.
func DefaultRepo(name string) string {
	first, _, _ := strings.Cut(name, "-")
	return first
}

func existsError(dir, file string) error {
	return api.Errorf(api.PConfigInvalid, "%s exists in %s", file, dir).WithPath(filepath.Join(dir, file)).
		WithHint("write over it", "repoctl init --force")
}

// writeFile writes a file of the set atomically and gives its File item.
func writeFile(dir, rel string, data []byte, kind string) (api.File, error) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	action := "created"
	if old, err := os.ReadFile(p); err == nil {
		action = "updated"
		if bytes.Equal(old, data) {
			action = "unchanged"
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return api.File{}, err
	}
	if action != "unchanged" {
		if err := fsutil.WriteAtomic(p, data, 0o644); err != nil {
			return api.File{}, api.Errorf(api.PConfigInvalid, "write %s: %v", p, err).WithPath(p)
		}
	}
	return api.File{Path: rel, Action: action, Kind: kind, Bytes: len(data)}, nil
}

// writeLock writes an empty repoctl.lock through the lock writer.
func writeLock(ctx context.Context, dir string, lf *lock.File, order []string) (api.File, error) {
	p := filepath.Join(dir, lock.FileName)
	action := "created"
	old, err := os.ReadFile(p)
	if err == nil {
		action = "updated"
	}
	if err := lf.WriteContext(ctx, dir, order); err != nil {
		return api.File{}, err
	}
	if action == "updated" && bytes.Equal(old, lf.Raw) {
		action = "unchanged"
	}
	_ = Exclude(ctx, dir, excludeLockFile())
	return api.File{Path: lock.FileName, Action: action, Kind: "lock", Bytes: len(lf.Raw)}, nil
}

// Init writes a starter repoctl.yaml and an empty repoctl.lock in the start
// directory (cli.html#cmd-init). It makes no branch and no worktree.
func Init(ctx context.Context, o InitOptions) (*InitReport, error) {
	if err := CheckWorkTree(ctx, o.Dir); err != nil {
		return nil, err
	}
	if o.From != "" {
		return initFrom(ctx, o)
	}
	if !o.Force {
		for _, f := range []string{config.SetFile, lock.FileName} {
			if _, err := os.Stat(filepath.Join(o.Dir, f)); err == nil {
				return nil, existsError(o.Dir, f)
			}
		}
	}
	name := o.Name
	if name == "" {
		name = DefaultName(ctx, o.Dir)
	}
	repo := o.Repo
	if repo == "" {
		repo = DefaultRepo(name)
	}
	targets := o.Targets
	if len(targets) == 0 {
		targets = []config.Target{DefaultTarget}
	}
	d := Draft{Name: name, Repo: repo, Targets: targets}
	data := d.Render()
	set, err := config.ParseSet(data, filepath.Join(o.Dir, config.SetFile))
	if err != nil {
		return nil, err
	}
	rep := &InitReport{Set: set}
	f, err := writeFile(o.Dir, config.SetFile, data, "config")
	if err != nil {
		return nil, err
	}
	rep.Files = append(rep.Files, f)
	lf, err := writeLock(ctx, o.Dir, lock.Empty(), nil)
	if err != nil {
		return nil, err
	}
	rep.Files = append(rep.Files, lf)
	return rep, nil
}

// initFrom forks the set config of SET (lifecycle.html#init-from).
func initFrom(ctx context.Context, o InitOptions) (*InitReport, error) {
	remote := "origin"
	here, hereErr := os.ReadFile(filepath.Join(o.Dir, config.SetFile))
	if hereErr == nil {
		if s, err := config.ParseSet(here, config.SetFile); err == nil {
			remote = s.Remote()
		}
	}
	src, err := OpenSetSource(ctx, o.Dir, remote, o.From)
	if err != nil {
		return nil, err
	}
	old, oldData, err := src.Config(ctx)
	if err != nil {
		return nil, err
	}
	rep := &InitReport{From: o.From, OldName: old.Name}
	// A start directory is a fork of SET when its repoctl.yaml has the old
	// set name: init then changes the file in place.
	data := oldData
	if hereErr == nil {
		var head struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal(here, &head) == nil && head.Name == old.Name {
			rep.Fork = true
			data = here
		}
	}
	if !rep.Fork && !o.Force {
		for _, f := range []string{config.SetFile, lock.FileName} {
			if _, err := os.Stat(filepath.Join(o.Dir, f)); err == nil {
				return nil, existsError(o.Dir, f)
			}
		}
	}
	out, err := rep.fork(data, old, o.Name)
	if err != nil {
		return nil, err
	}
	set, err := config.ParseSet(out, filepath.Join(o.Dir, config.SetFile))
	if err != nil {
		return nil, err
	}
	rep.Set = set
	f, err := writeFile(o.Dir, config.SetFile, out, "config")
	if err != nil {
		return nil, err
	}
	rep.Files = append(rep.Files, f)
	lf, err := writeLock(ctx, o.Dir, lock.Empty(), nil)
	if err != nil {
		return nil, err
	}
	rep.Files = append(rep.Files, lf)
	if rep.Fork {
		// The new set starts without the records of the old set.
		dropped := []string{".repoctl/advisories.yaml", "keys/deployed.yaml"}
		for _, k := range rep.Dropped {
			dropped = append(dropped, keyFile(k))
		}
		for _, rel := range dropped {
			if err := os.Remove(filepath.Join(o.Dir, filepath.FromSlash(rel))); err == nil {
				rep.Deleted = append(rep.Deleted, rel)
			}
		}
		return rep, nil
	}
	if err := rep.copyFiles(ctx, src, set, o); err != nil {
		return nil, err
	}
	return rep, nil
}

func keyFile(k config.Key) string {
	if k.File != "" {
		return k.File
	}
	return "keys/" + k.ID + ".asc"
}

// fork rewrites the YAML of the old set: the name, the branches of the
// packages and the keys. It keeps the comments and the key order.
func (rep *InitReport) fork(data []byte, old *config.Set, name string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, api.Errorf(api.PConfigInvalid, "%s: %v", rep.From, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, api.Errorf(api.PConfigInvalid, "%s: repoctl.yaml is not a map", rep.From)
	}
	root := doc.Content[0]
	if n := mapValue(root, "name"); n != nil {
		n.Value = name
	}
	if pkgs := mapValue(root, "packages"); pkgs != nil && pkgs.Kind == yaml.SequenceNode {
		for _, p := range pkgs.Content {
			b := mapValue(p, "branch")
			if b == nil {
				continue
			}
			id := ""
			if n := mapValue(p, "id"); n != nil {
				id = n.Value
			}
			if strings.Contains(b.Value, old.Name) {
				b.Value = strings.ReplaceAll(b.Value, old.Name, name)
				rep.Renamed++
			} else {
				rep.Shared = append(rep.Shared, id)
			}
		}
	}
	if sign := mapValue(root, "sign"); sign != nil {
		if keys := mapValue(sign, "keys"); keys != nil && keys.Kind == yaml.SequenceNode {
			var kept []*yaml.Node
			for _, k := range keys.Content {
				var key config.Key
				_ = k.Decode(&key)
				if key.State == config.KeyActive {
					kept = append(kept, k)
					continue
				}
				rep.Dropped = append(rep.Dropped, key)
			}
			keys.Content = kept
			if len(kept) == 0 {
				keys.Style = yaml.FlowStyle
			}
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return b.Bytes(), nil
}

// mapValue gives the value node of a key of a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// copyFiles copies the macro files, the release package and the key file of
// each kept key from SET into a start directory that is not a fork of it.
func (rep *InitReport) copyFiles(ctx context.Context, src *SetSource, set *config.Set, o InitOptions) error {
	var files []string
	seen := map[string]bool{}
	add := func(p string) {
		p = path.Clean(p)
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, t := range set.Targets {
		for _, m := range t.Macros {
			add(m)
		}
	}
	if rp, ok := set.ReleasePackage(); ok {
		list, err := src.Files(ctx, rp.Path)
		if err == nil {
			for _, f := range list {
				add(f)
			}
		}
	}
	for _, k := range set.Sign.Keys {
		add(keyFile(k))
	}
	for _, rel := range files {
		data, err := src.ReadFile(ctx, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || src.Dir == "" {
				continue
			}
			return err
		}
		dst := filepath.Join(o.Dir, filepath.FromSlash(rel))
		if _, err := os.Stat(dst); err == nil && !o.Force {
			rep.Skipped = append(rep.Skipped, rel)
			rep.Files = append(rep.Files, api.File{Path: rel, Action: "skipped", Kind: fileKind(rel, set), Bytes: len(data)})
			continue
		}
		f, err := writeFile(o.Dir, rel, data, fileKind(rel, set))
		if err != nil {
			return err
		}
		rep.Copied = append(rep.Copied, rel)
		rep.Files = append(rep.Files, f)
	}
	return nil
}

// fileKind gives the File kind of a set file.
func fileKind(rel string, set *config.Set) string {
	for _, t := range set.Targets {
		for _, m := range t.Macros {
			if path.Clean(m) == rel {
				return "macros"
			}
		}
	}
	for _, k := range set.Sign.Keys {
		if keyFile(k) == rel {
			return "keyfile"
		}
	}
	if strings.HasSuffix(rel, ".repo") {
		return "repofile"
	}
	return "other"
}
