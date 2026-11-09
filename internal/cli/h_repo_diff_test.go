package cli

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dennisklein/repoctl/internal/manifest"
)

func TestCompareRPMs(t *testing.T) {
	rpm := func(file, src, srcV, srcR, sum string) diffRPM {
		r := diffRPM{File: file + ".rpm", Source: src, SourceV: srcV, SourceR: srcR, Sum: sum}
		nvra := file
		i := len(nvra) - 1
		for nvra[i] != '.' {
			i--
		}
		r.Arch = nvra[i+1:]
		r.Name, r.Version, r.Release = manifest.SplitNVR(nvra[:i])
		return r
	}
	local := []diffRPM{
		rpm("hello-1.0-1.el9.x86_64", "hello", "1.0", "1.el9", "p1"),
		rpm("hello-devel-1.0-1.el9.x86_64", "hello", "1.0", "1.el9", "p2"),
		rpm("munge-0.5-1.el9.x86_64", "munge", "0.5", "1.el9", "p3"),
		rpm("ucx-1.22-1.el9.x86_64", "ucx", "1.22", "1.el9", "p4"),
	}
	staging := []diffRPM{
		rpm("hello-1.1-1.el9.x86_64", "hello", "1.1", "1.el9", "q1"),
		rpm("hello-devel-1.1-1.el9.x86_64", "hello", "1.1", "1.el9", "q2"),
		rpm("slurm-26.05-1.el9.x86_64", "slurm", "26.05", "1.el9", "q3"),
		rpm("ucx-1.22-1.el9.x86_64", "ucx", "1.22", "1.el9", "other"),
	}
	d := compareRPMs(local, staging, false)
	if len(d.Rows) != 2 || d.Rows[0].Mark != "~" || d.Rows[0].Source != "hello" || d.Rows[0].From != "1.0-1.el9" || d.Rows[0].To != "1.1-1.el9" ||
		len(d.Rows[0].RPMs) != 2 || d.Rows[1].Mark != "+" || d.Rows[1].Source != "slurm" || d.Rows[1].From != "-" {
		t.Errorf("rows: %+v", d.Rows)
	}
	if d.New != 3 || d.Unchanged != 2 || len(d.Removed) != 0 || !slices.Equal(d.Rebuilt, []string{"ucx-1.22-1.el9.x86_64"}) {
		t.Errorf("new %d, unchanged %d, removed %v, rebuilt %v", d.New, d.Unchanged, d.Removed, d.Rebuilt)
	}
	if len(d.Changed) != 2 || d.Changed[0].Name != "hello" || d.Changed[0].From != "1.0-1.el9" || d.Changed[0].To != "1.1-1.el9" {
		t.Errorf("changed: %+v", d.Changed)
	}

	// Between snapshots, a prune removes RPMs: a - row.
	from := append(slices.Clone(local), rpm("hello-0.9-1.el9.x86_64", "hello", "0.9", "1.el9", "p0"))
	d = compareRPMs(from, local, true)
	if len(d.Rows) != 1 || d.Rows[0].Mark != "-" || d.Rows[0].Source != "hello" || d.Gone != 1 || d.Unchanged != 2 ||
		!slices.Equal(d.Removed, []string{"hello-0.9-1.el9.x86_64"}) {
		t.Errorf("prune: rows %+v, removed %v, unchanged %d", d.Rows, d.Removed, d.Unchanged)
	}
	b, err := json.Marshal(d.RepoDiff)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil || back["added"] == nil || back["rebuilt"] == nil {
		t.Errorf("RepoDiff JSON: %s", b)
	}
}
