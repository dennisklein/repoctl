package journal

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/dennisklein/repoctl/internal/api"
	"github.com/dennisklein/repoctl/internal/config"
)

// CancelRequest is what repoctl cancel, the web UI or the MCP server asks
// of the process of a run (build.html#cancel). Only the process of the run
// writes its journal: the canceler writes the request next to the journal
// and sends SIGINT to the holder of the set lock, which then writes
// run.cancel-requested with these keys.
type CancelRequest struct {
	By     api.RunOrigin `json:"by"`
	User   string        `json:"user,omitempty"`
	Reason string        `json:"reason,omitempty"`
	Force  bool          `json:"force,omitempty"`
}

// CancelPath gives the file of the cancel request of a run.
func CancelPath(p config.Paths, set, run string) string {
	return filepath.Join(p.Runs(set), run+".cancel")
}

// WriteCancel writes the cancel request of a run.
func WriteCancel(p config.Paths, set, run string, c CancelRequest) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := CancelPath(p, set, run)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadCancel reads the cancel request of a run, if there is one.
func ReadCancel(p config.Paths, set, run string) (CancelRequest, bool) {
	var c CancelRequest
	b, err := os.ReadFile(CancelPath(p, set, run))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, false
	}
	return c, true
}
