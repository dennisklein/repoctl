package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/ui"
)

// TestCheckUIConfirmation: sign deployed takes a UI confirmation only from
// the server of ui.json, and only when it names the command, the set, the
// key and the targets (cli.html#ui-confirmation).
func TestCheckUIConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.json")
	const server = 4242
	b, _ := json.Marshal(ui.Record{PID: server, Listen: "127.0.0.1:8421"})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	body := func(c ui.Confirmation) []byte {
		b, _ := json.Marshal(c)
		return b
	}
	good := ui.Confirmation{V: 1, Command: "sign deployed", Set: "slurm-26.05", Targets: []string{"el9"}, Keys: []string{"a.admin"}}
	check := func(b []byte, ppid int) error {
		return checkUIConfirmation(b, path, ppid, "sign deployed", "slurm-26.05", []string{"el9"}, "a.admin")
	}
	// ui.json without a server that holds it.
	if err := check(body(good), server); err == nil {
		t.Fatal("a ui.json that no server holds was accepted")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := check(body(good), server); err != nil {
		t.Fatalf("a valid confirmation: %v", err)
	}
	wrongSet, wrongKey, wrongCmd, wrongTarget, noTargets := good, good, good, good, good
	wrongSet.Set = "slurm-25.11"
	wrongKey.Keys = []string{"d.klein"}
	wrongCmd.Command = "publish"
	wrongTarget.Targets = []string{"el10"}
	noTargets.Targets = nil
	for name, c := range map[string]struct {
		b    []byte
		ppid int
	}{
		"other parent": {body(good), server + 1},
		"set":          {body(wrongSet), server},
		"key":          {body(wrongKey), server},
		"command":      {body(wrongCmd), server},
		"target":       {body(wrongTarget), server},
		"empty":        {nil, server},
		"not JSON":     {[]byte("a.admin"), server},
	} {
		err := check(c.b, c.ppid)
		ae, ok := api.AsError(err)
		if !ok || ae.Code != api.PPublishNeedsConfirmation || ae.ExitCode != api.ExitNeedsConfirmation || !strings.Contains(ae.Message, "nothing changed") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The targets are optional in the object.
	if err := check(body(noTargets), server); err != nil {
		t.Errorf("without targets: %v", err)
	}
}
