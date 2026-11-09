package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/repoctl/internal/api"
)

// TestSignRPMsOutput runs an rpmsign that writes to stdout and stderr at
// once and fails: the error holds its first lines (and -race finds no
// race on the shared buffer).
func TestSignRPMsOutput(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nfor i in 1 2 3 4 5 6 7 8; do echo \"out $i\"; echo \"err $i\" >&2; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "rpmsign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := &GPGSigner{Fingerprint: strings.Repeat("A", 40)}
	err := s.SignRPMs(context.Background(), []string{"a.rpm", "b.rpm"})
	e, ok := api.AsError(err)
	if !ok || e.Code != api.PSignFailed || !strings.Contains(e.Error(), "rpmsign failed: ") {
		t.Fatalf("SignRPMs: %v", err)
	}
	if n := strings.Count(e.Error(), "; "); n != 2 {
		t.Errorf("SignRPMs gives %d lines, want 3: %v", n+1, err)
	}
}
