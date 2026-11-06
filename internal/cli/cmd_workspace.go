package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// workspaceCommands: init, migrate, status, list, show, checkout, pin,
// rebuild (cli.html#group-workspace).
func (a *App) workspaceCommands() []*cobra.Command {
	initCmd := command("workspace", "init", "Write a starter repoctl.yaml, or fork a new set from another", api.KindFile, "local write")
	initCmd.Annotations[annoNoTarget] = "true"
	initCmd.Args = cobra.NoArgs
	initCmd.Flags().String("name", "", "the name of the new set")
	initCmd.Flags().String("repo", "", "the repository name (default: the first part of the set name)")
	initCmd.Flags().StringArray("add-target", nil, "add a target ID=MOCKCFG (default: el9=rocky+epel-9-x86_64)")
	initCmd.Flags().String("from", "", "fork the set directory or set branch SET")
	initCmd.Flags().Bool("force", false, "write over an existing file")
	initCmd.RunE = a.run(hInit)

	migrate := command("workspace", "migrate", "Convert packages.lock and scripts/, or move to a new apiVersion", api.KindFile, "local write")
	migrate.Args = cobra.NoArgs
	migrate.Flags().String("name", "", "the name of the set")
	migrate.Flags().Bool("dry-run", false, "print the files and write nothing")
	migrate.Flags().Bool("force", false, "write over existing files")
	migrate.Flags().Bool("api", false, "move the four set files to the newest apiVersion")
	migrate.Flags().Duration("lock-wait", 0, "with --api: wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
	migrate.RunE = a.run(hMigrate)

	status := command("workspace", "status", `Show the set, "latest", the run, the advisories and the packages`, api.KindStatus, "read")
	status.Args = cobra.NoArgs
	status.RunE = a.run(hStatus)

	list := command("workspace", "list [SELECTOR...]", "List the packages of the set", api.KindPackage, "read")
	list.Aliases = []string{"ls"}
	list.Flags().Bool("buildrequires", false, "add the BuildRequires per target")
	list.Flags().Bool("provides", false, "add the binary RPM names per target")
	list.RunE = a.run(hList)

	show := command("workspace", "show SELECTOR", "Show one package for each of its targets", api.KindPackage, "read")
	show.Args = cobra.ExactArgs(1)
	show.RunE = a.run(hShow)

	checkout := command("workspace", "checkout [PKG...]", "Create or update the package worktrees from repoctl.lock", api.KindLock, "local write")
	checkout.Flags().Bool("fetch", false, "fetch the remote first")
	checkout.Flags().Bool("force", false, "reset the local branch to the lock commit and discard the changes")
	checkout.Flags().IntP("jobs", "j", 0, "work on at most N packages at the same time")
	checkout.Flags().Bool("create", false, "make the branch of a new package from one start point")
	checkout.Flags().String("from-set", "", "with --create: the set branch SET to fork from")
	checkout.Flags().String("from", "", "with --create: REF, or fedora[:BRANCH]")
	checkout.Flags().Bool("orphan", false, "with --create: an orphan branch")
	checkout.Flags().Duration("lock-wait", 0, "wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
	checkout.RunE = a.run(hCheckout)

	pin := command("workspace", "pin [PKG...]", "Write the commits of the worktrees into repoctl.lock", api.KindLock, "local write")
	pin.Flags().Bool("fetch", false, "fetch first")
	pin.Flags().Bool("older", false, "with PKG: write a HEAD commit that is behind the lock")
	pin.Flags().Bool("orphans", false, "remove only the orphaned lines")
	pin.Flags().Duration("lock-wait", 0, "wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
	pin.RunE = a.run(hPin)

	rebuild := command("workspace", "rebuild PKG...", "Add a rebuild commit to package branches and pin it", api.KindLock, "local write")
	rebuild.Args = cobra.MinimumNArgs(1)
	rebuild.Flags().String("reason", "", "the reason, 8 characters or more (required)")
	rebuild.Flags().Bool("dry-run", false, "show the new NVRs")
	rebuild.Flags().Duration("lock-wait", 0, "wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
	rebuild.RunE = a.run(hRebuild)

	return []*cobra.Command{initCmd, migrate, status, list, show, checkout, pin, rebuild}
}
