package advisory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/fsutil"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/lock"
)

// DismissalPath is the dismissal file below the set directory.
const DismissalPath = ".repoctl/advisories.yaml"

// MinNote is the shortest --note of dismiss and --reason of an acceptance.
const MinNote = 8

const dismissalHead = `# .repoctl/advisories.yaml
# Written by repoctl advisories dismiss. Commit and push it.
`

// Dismissal is one entry of the dismissal file.
type Dismissal struct {
	ID    string     `yaml:"id"`
	Facts api.Digest `yaml:"facts"`
	Note  string     `yaml:"note"`
	By    string     `yaml:"by"`
	At    time.Time  `yaml:"at"`
}

// Dismissals is the dismissal file of a set: one committed file for the
// team.
type Dismissals struct {
	// Path is the file.
	Path string
	// Entries are the entries of the file in file order. Two entries with
	// the same id are valid after a git conflict; Get gives the newest.
	Entries []Dismissal
	// Exists reports that the file exists.
	Exists bool
}

type dismissalFile struct {
	APIVersion string      `yaml:"apiVersion"`
	Dismissed  []Dismissal `yaml:"dismissed"`
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// invalid gives the problem advisories.invalid: the file is invalid, and
// repoctl ignores it.
func invalid(path, format string, args ...any) *api.Error {
	return api.Errorf(api.PAdvisoriesInvalid, "%s: %s; repoctl ignores the file, so no advisory is dismissed", path, fmt.Sprintf(format, args...)).
		WithPath(path).WithHint("keep the entries of both sides of a conflict, remove the markers, and commit", "")
}

// LoadDismissals reads the dismissal file of the set in setDir. A missing
// file gives an empty list. An invalid file gives the error
// advisories.invalid together with an empty list: the caller warns and
// dismisses nothing.
func LoadDismissals(setDir string) (*Dismissals, error) {
	d := &Dismissals{Path: filepath.Join(setDir, filepath.FromSlash(DismissalPath))}
	data, err := os.ReadFile(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, invalid(d.Path, "%v", err)
	}
	d.Exists = true
	entries, perr := ParseDismissals(data, d.Path)
	if perr != nil {
		return d, perr
	}
	d.Entries = entries
	return d, nil
}

// ParseDismissals reads the entries of a dismissal file.
func ParseDismissals(data []byte, path string) ([]Dismissal, error) {
	for i, line := range strings.Split(string(data), "\n") {
		for _, m := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
			if strings.HasPrefix(line, m) {
				return nil, invalid(path, "line %d has the git conflict marker %s", i+1, m)
			}
		}
	}
	var f dismissalFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, invalid(path, "%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if f.APIVersion == "" && len(f.Dismissed) == 0 && len(bytes.TrimSpace(stripComments(data))) == 0 {
		return nil, nil
	}
	if f.APIVersion != api.APIVersion {
		return nil, invalid(path, "unknown apiVersion %q", f.APIVersion)
	}
	for i, e := range f.Dismissed {
		switch {
		case !ValidID(e.ID):
			return nil, invalid(path, "dismissed[%d]: id %q is not CODE/SUBJECT", i, e.ID)
		case !digestRE.MatchString(e.Facts):
			return nil, invalid(path, "dismissed[%d] (%s): facts %q is not a sha256 digest", i, e.ID, e.Facts)
		case len(strings.TrimSpace(e.Note)) < MinNote:
			return nil, invalid(path, "dismissed[%d] (%s): the note has fewer than %d characters", i, e.ID, MinNote)
		case e.At.IsZero():
			return nil, invalid(path, "dismissed[%d] (%s): at is missing", i, e.ID)
		}
	}
	return f.Dismissed, nil
}

func stripComments(data []byte) []byte {
	var b bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Get gives the entry of an id: the one with the newest at.
func (d *Dismissals) Get(id string) (Dismissal, bool) {
	var best Dismissal
	ok := false
	for _, e := range d.Entries {
		if e.ID == id && (!ok || e.At.After(best.At)) {
			best, ok = e, true
		}
	}
	return best, ok
}

// IDs gives the ids of the file, sorted, each once.
func (d *Dismissals) IDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range d.Entries {
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Format gives the file with one entry per id (the newest), sorted by id.
func (d *Dismissals) Format() ([]byte, error) {
	f := dismissalFile{APIVersion: api.APIVersion, Dismissed: []Dismissal{}}
	for _, id := range d.IDs() {
		e, _ := d.Get(id)
		e.At = e.At.UTC().Truncate(time.Second)
		f.Dismissed = append(f.Dismissed, e)
	}
	var b bytes.Buffer
	b.WriteString(dismissalHead)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Save writes the file atomically.
func (d *Dismissals) Save() error {
	data, err := d.Format()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.Path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.Path, data, 0o644)
}

// Set adds or replaces the entry of an id.
func (d *Dismissals) Set(e Dismissal) {
	d.Remove(e.ID)
	d.Entries = append(d.Entries, e)
}

// Remove removes all entries of an id. It reports whether one existed.
func (d *Dismissals) Remove(id string) bool {
	out := d.Entries[:0]
	found := false
	for _, e := range d.Entries {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	d.Entries = out
	return found
}

// LockDismissals takes the short file lock of the dismissal file,
// $XDG_RUNTIME_DIR/repoctl/SET.advisories.lock, for the read, the change
// and the rename. It takes no set lock: a build can hold that for an hour.
func LockDismissals(ctx context.Context, p config.Paths, set string, h lock.Holder) (*lock.SetLock, error) {
	path := filepath.Join(p.Runtime(), set+".advisories.lock")
	return lock.Acquire(ctx, path, h, 10*time.Second, false, lock.What{
		Code: api.PSetBusy, Name: "the dismissal file of " + set, Lock: "the advisories lock",
	})
}

// Stale reports whether a dismissal no longer matches the current
// advisories: its id is gone, its digest differs, or its advisory is
// required now. A mock.drift entry whose id this host does not find is not
// stale, because another host can still find it.
func Stale(e Dismissal, current map[string]api.Advisory) bool {
	a, ok := current[e.ID]
	if !ok {
		code, _ := SplitID(e.ID)
		return code != api.AdvisoryCodeMockDrift
	}
	return a.FactsDigest != e.Facts || a.Level == api.AdvisoryLevelRequired
}

// ApplyDismissals sets the state and the dismissal of each advisory from
// the file. A dismissal matches when it has the same id and the same
// factsDigest and the advisory is not required. A pending advisory stays
// pending.
func ApplyDismissals(list []api.Advisory, d *Dismissals) {
	for i := range list {
		a := &list[i]
		if a.State == api.AdvisoryStatePending {
			continue
		}
		a.State = api.AdvisoryStateOpen
		a.Dismissal = nil
		if d == nil {
			continue
		}
		e, ok := d.Get(a.ID)
		if !ok {
			continue
		}
		a.Dismissal = &api.AdvisoryDismissal{By: e.By, At: api.FormatTime(e.At), Note: e.Note}
		if e.Facts == a.FactsDigest && a.Level != api.AdvisoryLevelRequired {
			a.State = api.AdvisoryStateDismissed
			continue
		}
		a.Dismissal.Stale = true
	}
}

// DismissResult is what Dismiss did for one id.
type DismissResult struct {
	ID    string
	Facts api.Digest
}

// Dismiss adds a dismissal for each open id: the note is required, with at
// least MinNote characters; only info and warn advisories can be
// dismissed. It changes nothing when one id fails.
func Dismiss(d *Dismissals, current []api.Advisory, ids []string, note, by string, now time.Time) ([]DismissResult, error) {
	if len(strings.TrimSpace(note)) < MinNote {
		return nil, api.Errorf(api.PUsage, "--note needs at least %d characters: it tells the team why", MinNote).WithExit(api.ExitUsage)
	}
	byID := map[string]api.Advisory{}
	for _, a := range current {
		byID[a.ID] = a
	}
	var out []DismissResult
	var entries []Dismissal
	for _, id := range ids {
		a, ok := byID[id]
		if !ok {
			return nil, api.Errorf(api.PAdvisoryUnknown, "no open advisory has the id %s", id).
				WithHint("list the advisories", "repoctl advisories ls")
		}
		if a.Level == api.AdvisoryLevelRequired {
			return nil, api.Errorf(api.PAdvisoryNotDismissable, "%s is required, and a required advisory cannot be dismissed", id).
				WithHint("fix its cause, or accept it for one publish", "repoctl publish --accept-advisory "+id+" --reason TEXT")
		}
		if a.State != api.AdvisoryStateOpen {
			return nil, api.Errorf(api.PAdvisoryUnknown, "%s is not open (%s)", id, a.State).
				WithHint("list the advisories", "repoctl advisories ls --dismissed")
		}
		entries = append(entries, Dismissal{ID: id, Facts: a.FactsDigest, Note: strings.TrimSpace(note), By: by, At: now.UTC().Truncate(time.Second)})
		out = append(out, DismissResult{ID: id, Facts: a.FactsDigest})
	}
	for _, e := range entries {
		d.Set(e)
	}
	return out, nil
}

// Undismiss removes the entries of the ids. Each id must have an entry.
func Undismiss(d *Dismissals, ids []string) error {
	for _, id := range ids {
		if _, ok := d.Get(id); !ok {
			return api.Errorf(api.PAdvisoryUnknown, "%s has no dismissal in %s", id, DismissalPath).
				WithHint("list the dismissed advisories", "repoctl advisories ls --dismissed")
		}
	}
	for _, id := range ids {
		d.Remove(id)
	}
	return nil
}

// UndismissStale removes each stale entry and gives their ids. complete
// tells that each code of the last check ran ok on each target; else it
// stops with check.incomplete, so a failed input never removes a valid
// dismissal.
func UndismissStale(d *Dismissals, current []api.Advisory, complete bool) ([]string, error) {
	if !complete {
		return nil, api.Errorf(api.PCheckIncomplete, "the last check did not run each code ok on each target, so repoctl cannot tell a stale dismissal").
			WithHint("run the check, then try again", "repoctl check")
	}
	byID := map[string]api.Advisory{}
	for _, a := range current {
		byID[a.ID] = a
	}
	var removed []string
	for _, id := range d.IDs() {
		e, _ := d.Get(id)
		if Stale(e, byID) {
			d.Remove(id)
			removed = append(removed, id)
		}
	}
	return removed, nil
}
