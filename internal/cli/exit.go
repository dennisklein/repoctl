package cli

import "github.com/dennisklein/repoctl/internal/api"

// The exit codes of repoctl (cli.html#exit). The values live in package
// api so that the core, the MCP server and the UI share them.
const (
	ExitOK                = api.ExitOK
	ExitFailed            = api.ExitFailed
	ExitUsage             = api.ExitUsage
	ExitBuildFailed       = api.ExitBuildFailed
	ExitNothingToPublish  = api.ExitNothingToPublish
	ExitLockDirty         = api.ExitLockDirty
	ExitNeedsConfirmation = api.ExitNeedsConfirmation
	ExitBusy              = api.ExitBusy
	ExitDoctorWarning     = api.ExitDoctorWarning
	ExitHostNotReady      = api.ExitHostNotReady
	ExitVerifyFailed      = api.ExitVerifyFailed
	ExitDifferences       = api.ExitDifferences
	ExitNotPublishable    = api.ExitNotPublishable
	ExitStateConflict     = api.ExitStateConflict
	ExitInterrupted       = api.ExitInterrupted
)
