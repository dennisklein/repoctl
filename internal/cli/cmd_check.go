package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// checkCommands: check, advisories, upstream (cli.html#group-check).
func (a *App) checkCommands() []*cobra.Command {
	check := command("check", "check", "Compute the advisories that need no build", api.KindAdvisoryList, "read")
	check.Args = cobra.NoArgs
	check.Flags().StringArray("code", nil, "run only these codes")
	check.Flags().Bool("check", false, "exit 11 for an open advisory at --min-level or higher")
	check.Flags().String("min-level", "warn", "the level of --check: info, warn, required")
	check.Flags().Bool("install-timer", false, "write and start the user systemd timer of the set")
	check.Flags().Bool("remove-timer", false, "remove the user systemd timer of the set")
	check.RunE = a.run(hCheck)

	adv := command("check", "advisories", "List, show and dismiss advisories", api.KindAdvisoryList, "read")
	adv.Args = cobra.NoArgs
	adv.Flags().String("level", "", "only advisories of this level")
	adv.Flags().Bool("dismissed", false, "add the dismissed entries")
	adv.Flags().Bool("refresh", false, "run check first")
	adv.RunE = a.run(hAdvisories)
	advLs := sub("ls", "List the advisories", api.KindAdvisoryList, "read")
	advLs.Args = cobra.NoArgs
	advLs.Flags().String("level", "", "only advisories of this level")
	advLs.Flags().Bool("dismissed", false, "add the dismissed entries")
	advLs.Flags().Bool("refresh", false, "run check first")
	advLs.RunE = a.run(hAdvisories)
	advShow := sub("show ID", "Show one advisory", api.KindAdvisory, "read")
	advShow.Args = cobra.ExactArgs(1)
	advShow.RunE = a.run(hAdvShow)
	advDismiss := sub("dismiss ID...", "Dismiss info and warn advisories", api.KindAdvisoryList, "local write")
	advDismiss.Args = cobra.MinimumNArgs(1)
	advDismiss.Flags().String("note", "", "why, 8 characters or more (required)")
	advDismiss.RunE = a.run(hAdvDismiss)
	advUndismiss := sub("undismiss [ID...]", "Remove dismissals", api.KindAdvisoryList, "local write")
	advUndismiss.Flags().Bool("stale", false, "remove the entries whose id is gone or whose facts changed")
	advUndismiss.RunE = a.run(hAdvUndismiss)
	adv.AddCommand(advLs, advShow, advDismiss, advUndismiss)

	upstream := command("upstream", "upstream [PKG...]", "Compare package versions with upstream releases", api.KindUpstream, "read")
	upstream.Flags().Bool("check", false, "exit 11 for an update")
	upstream.Flags().Bool("releases", false, "list each newer release with its date and URL")
	upstream.Flags().Bool("refresh", false, "ignore the cache")
	upstream.Flags().IntP("jobs", "j", 4, "at most N queries at the same time")
	upstream.RunE = a.run(hUpstream)

	return []*cobra.Command{check, adv, upstream}
}
