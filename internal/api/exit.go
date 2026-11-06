package api

// The exit codes of repoctl (docs/design/cli.html#exit). They are a
// contract: repoctl can add a code, but it never renumbers one. internal/cli
// gives them the names of the CLI reference.
const (
	ExitOK                = 0
	ExitFailed            = 1
	ExitUsage             = 2
	ExitBuildFailed       = 3
	ExitNothingToPublish  = 4
	ExitLockDirty         = 5
	ExitNeedsConfirmation = 6
	ExitBusy              = 7
	ExitDoctorWarning     = 8
	ExitHostNotReady      = 9
	ExitVerifyFailed      = 10
	ExitDifferences       = 11
	ExitNotPublishable    = 12
	ExitStateConflict     = 13
	ExitInterrupted       = 130
)
