package publish

import (
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

func TestNotReady(t *testing.T) {
	nr := []NotReady{{Node: "slurm@el10", State: "failed"}, {Node: "slurm-singularity-exec@el10", State: "blocked"}}
	if got, want := NotReadyText(nr), "slurm@el10 failed, slurm-singularity-exec@el10 is blocked"; got != want {
		t.Errorf("NotReadyText: %q, want %q", got, want)
	}
	e := notReadyHints(api.Errorf(api.PPublishNotReady, "el10 is not publishable"), "el10", nr)
	if len(e.Hints) != 2 || e.Hints[0].String() != "read the log. run: repoctl logs slurm@el10" ||
		e.Hints[1].String() != "fix slurm.spec, commit and push it. run: repoctl pin slurm" {
		t.Errorf("hints of a failed node: %v", e.Hints)
	}
	if e.ExitCode != 12 {
		t.Errorf("exit %d, want 12", e.ExitCode)
	}
	e = notReadyHints(api.Errorf(api.PPublishNotReady, "x"), "el10", []NotReady{{Node: "hello@el10", State: "build"}})
	if len(e.Hints) != 1 || e.Hints[0].Run != "repoctl plan --target el10" {
		t.Errorf("hints of a node to build: %v", e.Hints)
	}
	if got := NotReadyText([]NotReady{{Node: "hello@el10", State: "build"}, {Node: "x@el10", State: "skip"}}); got != "hello@el10 is not built, x@el10 has no lock line" {
		t.Errorf("NotReadyText: %q", got)
	}
}
