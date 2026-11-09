package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
	"github.com/dennisklein/repoctl/internal/manifest"
	"github.com/dennisklein/repoctl/internal/publish"
	"github.com/dennisklein/repoctl/internal/ui"
)

// confirmPipe gives a file descriptor that reads the UI confirmation c,
// as the UI server passes it to its child.
func confirmPipe(t *testing.T, c any) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return fd
}

// uiServer writes ui.json with pid and holds its flock, as a running
// server does. It gives a func that writes another pid.
func uiServer(t *testing.T, p config.Paths, pid int) func(pid int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.UIJSON()), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(pid int) {
		b, _ := json.Marshal(ui.Record{PID: pid, Session: "0123456789abcdef"})
		if err := os.WriteFile(p.UIJSON(), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pid)
	f, err := os.Open(p.UIJSON())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return write
}

func problemCode(err error) api.ProblemCode {
	if e, ok := api.AsError(err); ok {
		return e.Code
	}
	return ""
}

func TestReadConfirmation(t *testing.T) {
	dir := t.TempDir()
	a := &App{paths: config.NewPaths(func(k string) string {
		switch k {
		case "HOME":
			return dir
		case "XDG_RUNTIME_DIR":
			return filepath.Join(dir, "run")
		}
		return ""
	})}
	good := ui.Confirmation{V: 1, Command: "publish", Set: "slurm-26.05", Targets: []string{"el9"}, PlanDigest: "sha256:abc"}
	old := parentPID
	t.Cleanup(func() { parentPID = old })
	parentPID = os.Getpid

	// No server: refused, and the descriptor is read and closed.
	fd := confirmPipe(t, good)
	if _, err := a.readConfirmation(fd); problemCode(err) != api.PPublishNeedsConfirmation {
		t.Errorf("without a UI server: %v", err)
	}
	if _, err := syscall.Read(fd, make([]byte, 1)); err == nil {
		t.Errorf("the descriptor %d stays open", fd)
	}

	// A server that is not the parent: refused.
	setPID := uiServer(t, a.paths, os.Getpid()+1)
	if _, err := a.readConfirmation(confirmPipe(t, good)); problemCode(err) != api.PPublishNeedsConfirmation {
		t.Errorf("from another process: %v", err)
	}

	// The parent is the server.
	setPID(os.Getpid())
	c, err := a.readConfirmation(confirmPipe(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Set != good.Set || c.PlanDigest != good.PlanDigest || !slices.Equal(c.Targets, good.Targets) {
		t.Errorf("confirmation %+v, want %+v", c, good)
	}
	for _, bad := range []any{"not an object", ui.Confirmation{V: 1, Command: "sign deployed"}, ui.Confirmation{V: 2, Command: "publish"}} {
		if _, err := a.readConfirmation(confirmPipe(t, bad)); problemCode(err) != api.PPublishNeedsConfirmation {
			t.Errorf("confirmation %v: %v", bad, err)
		}
	}
}

func TestMatchConfirmation(t *testing.T) {
	req := publish.Request{Run: "r-20261102-1750-a3f", Accept: []string{"repoclosure/x"}, Reason: "needed for the fix"}
	base := ui.Confirmation{V: 1, Command: "publish", Set: "slurm-26.05", Targets: []string{"el9"}, Run: req.Run,
		Accept: []string{"repoclosure/x"}, Reason: req.Reason, PlanDigest: "sha256:abc"}
	if err := matchConfirmation(&base, "slurm-26.05", []string{"el9"}, req); err != nil {
		t.Fatalf("a matching confirmation: %v", err)
	}
	for name, change := range map[string]func(c *ui.Confirmation){
		"set":     func(c *ui.Confirmation) { c.Set = "slurm" },
		"targets": func(c *ui.Confirmation) { c.Targets = []string{"el10"} },
		"run":     func(c *ui.Confirmation) { c.Run = "" },
		"accept":  func(c *ui.Confirmation) { c.Accept = nil },
		"reason":  func(c *ui.Confirmation) { c.Reason = "another reason" },
		"digest":  func(c *ui.Confirmation) { c.PlanDigest = "" },
	} {
		c := base
		change(&c)
		err := matchConfirmation(&c, "slurm-26.05", []string{"el9"}, req)
		if problemCode(err) != api.PPublishNeedsConfirmation {
			t.Errorf("%s: %v", name, err)
		}
		if e, ok := api.AsError(err); ok && e.ExitCode != api.ExitNeedsConfirmation {
			t.Errorf("%s: exit %d", name, e.ExitCode)
		}
	}
	c := base
	c.Set = "slurm"
	if err := matchConfirmation(&c, "slurm-26.05", []string{"el9"}, req); err == nil || err.Error() != `publish: "slurm" is not the set name; nothing is signed or pushed` {
		t.Errorf("a wrong set name: %v", err)
	}
}

func TestCheckConfirmedPlan(t *testing.T) {
	p := &publish.Plan{Digest: "sha256:abc", Targets: []*publish.TargetPlan{
		{Target: "el9", Gate: &publish.GateResult{Trust: &manifest.Trust{Added: []string{"a.admin"}}}},
	}}
	c := ui.Confirmation{V: 1, Command: "publish", Set: "slurm-26.05", PlanDigest: "sha256:abc", Keys: []string{"a.admin"}}
	if err := checkConfirmedPlan(&c, p); err != nil {
		t.Fatalf("a matching plan: %v", err)
	}
	c.PlanDigest = "sha256:def"
	err := checkConfirmedPlan(&c, p)
	if e, ok := api.AsError(err); !ok || e.Code != api.PPublishPlanChanged || e.ExitCode != api.ExitNeedsConfirmation {
		t.Errorf("another plan digest: %v", err)
	}
	c.PlanDigest, c.Keys = "sha256:abc", nil
	if err := checkConfirmedPlan(&c, p); problemCode(err) != api.PPublishNeedsConfirmation {
		t.Errorf("without the typed key id: %v", err)
	}
}
