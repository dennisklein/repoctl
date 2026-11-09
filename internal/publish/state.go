package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/localcopy"
)

// The steps of a publish run, in order.
var Steps = []string{"check", "sign", "repodata", "verify", "verify-el", "push", "tag", "cache"}

// The results of a step and of a target.
const (
	StepOK       = "ok"
	StepFailed   = "failed"
	StepCanceled = "canceled"
	StepSkipped  = "skipped"
)

// StateFile gives publishes/ID/state.json.
func StateFile(p config.Paths, set, id string) string {
	return filepath.Join(p.Publish(set, id), "state.json")
}

// TreeDir gives the new tree of a target: publishes/ID/tree/TARGET.
func TreeDir(p config.Paths, set, id, target string) string {
	return filepath.Join(p.Publish(set, id), "tree", target)
}

// ReadState reads the state file of a publish.
func ReadState(p config.Paths, set, id string) (*api.Run, error) {
	b, err := os.ReadFile(StateFile(p, set, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, api.Errorf(api.PSnapshotNotFound, "%s has no state file", id).
				WithHint("list the kept trees", "repoctl status")
		}
		return nil, err
	}
	var r api.Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// WriteState writes the state file of a publish in one rename.
func WriteState(p config.Paths, set, id string, r *api.Run) error {
	path := StateFile(p, set, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StepState gives the result of a step of a target in a state, or "".
func StepState(v api.RunPublishValue, step string) string {
	for _, s := range v.Steps {
		if s.Step == step {
			return s.State
		}
	}
	return ""
}

// IsPushable reports the rule "pushable tree": verify and verify-el are ok,
// and step 8 is not done.
func IsPushable(v api.RunPublishValue) bool {
	return StepState(v, "verify") == StepOK && StepState(v, "verify-el") == StepOK && StepState(v, "cache") != StepOK
}

// Kept is one kept tree of a publish: its id and its state.
type Kept struct {
	ID  string
	Run *api.Run
}

// KeptTrees lists the publishes with a state file, newest first.
func KeptTrees(p config.Paths, set string) ([]Kept, error) {
	dir := filepath.Join(p.SetData(set), "publishes")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Kept
	for _, e := range ents {
		r, err := ReadState(p, set, e.Name())
		if err != nil {
			continue
		}
		out = append(out, Kept{ID: e.Name(), Run: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// PushableFor gives the pushable trees of a target for the freshness
// check.
func PushableFor(p config.Paths, set, target string) []localcopy.Pushable {
	kept, _ := KeptTrees(p, set)
	var out []localcopy.Pushable
	for _, k := range kept {
		v, ok := k.Run.Publish[target]
		if !ok || !IsPushable(v) || v.Snapshot == nil {
			continue
		}
		out = append(out, localcopy.Pushable{Publish: k.ID, Base: v.Base, New: v.Snapshot.Digest})
	}
	return out
}
