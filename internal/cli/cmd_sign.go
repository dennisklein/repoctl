package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// signCommands: sign and release (cli.html#group-sign).
func (a *App) signCommands() []*cobra.Command {
	sign := command("sign", "sign", "Show and change the trusted keys and their deployment record", "", "read")
	// The group has no output of its own; its subcommands have --json.
	delete(sign.Annotations, annoNoJSON)

	ls := sub("ls", "List the keys, their deployment record and their RPMs", api.KindSignerList, "read")
	ls.Args = cobra.NoArgs
	ls.RunE = a.run(hSignLs)
	onboard := sub("onboard ID", "Check the onboarding steps of a key", api.KindOnboardReport, "read")
	onboard.Args = cobra.ExactArgs(1)
	onboard.RunE = a.run(hSignOnboard)
	offboard := sub("offboard ID", "Check the offboarding steps of a key", api.KindOnboardReport, "read")
	offboard.Args = cobra.ExactArgs(1)
	offboard.RunE = a.run(hSignOffboard)
	add := sub("add ID", "Add a trusted key", api.KindSignerList, "local write")
	add.Args = cobra.ExactArgs(1)
	add.Flags().String("fingerprint", "", "the full fingerprint that the key must have (required)")
	add.Flags().Bool("from-gpg", false, "export the key from the local gpg or card (default)")
	add.Flags().String("file", "", "read the key from FILE")
	add.RunE = a.run(hSignAdd)
	signLockWait(add)
	var lifecycle []*cobra.Command
	for _, x := range []struct{ use, short string }{
		{"retire ID", "Set an active key to retiring"},
		{"activate ID", "Set a retiring key back to active"},
		{"remove ID", "Set a retiring key to removed"},
		{"revoke ID", "End the trust in a compromised key"},
	} {
		c := sub(x.use, x.short, api.KindSignerList, "local write")
		c.Args = cobra.ExactArgs(1)
		c.RunE = a.run(hSignLifecycle)
		signLockWait(c)
		lifecycle = append(lifecycle, c)
	}
	deployed := sub("deployed ID", "Record that the clients have the release with the key", api.KindSignerList, "shared state")
	deployed.Args = cobra.ExactArgs(1)
	deployed.Flags().String("release", "", "the release NVR in latest of each target")
	deployed.Flags().String("keyfile", "", "a key file record: the sha256 of the key file")
	deployed.Flags().String("path", "", "for an import: the signed tree in place of latest")
	deployed.Flags().Bool("withdraw", false, "remove the entry of the key and each target")
	deployed.Flags().String("note", "", "how the human checked the clients")
	deployed.Flags().Bool("dry-run", false, "run the checks and write nothing")
	// --confirm-fd: the UI confirmation of a child of the web UI
	// (cli.html#ui-confirmation); the help and the completion hide it.
	deployed.Flags().Int("confirm-fd", -1, "")
	_ = deployed.Flags().MarkHidden("confirm-fd")
	deployed.RunE = a.run(hSignDeployed)
	signLockWait(deployed)
	removed := sub("removed ID", "Record that the clients no longer have the key", api.KindSignerList, "shared state")
	removed.Args = cobra.ExactArgs(1)
	removed.Flags().String("note", "", "how the human checked the clients")
	removed.Flags().Bool("dry-run", false, "run the checks and write nothing")
	removed.RunE = a.run(hSignRemoved)
	signLockWait(removed)
	keyfile := sub("keyfile", "Print the key file in latest of each target", api.KindFile, "read")
	keyfile.Args = cobra.NoArgs
	keyfile.Flags().Bool("tree", false, "read the key file of the in-tree release package")
	keyfile.Flags().Bool("checksum", false, "print the sha256 in the form of sha256sum")
	keyfile.RunE = a.run(hSignKeyfile)
	sign.AddCommand(append([]*cobra.Command{ls, onboard, offboard, add}, append(lifecycle, deployed, removed, keyfile)...)...)

	release := command("sign", "release", "Write the key file of the release package", "", "local write")
	delete(release.Annotations, annoNoJSON)
	write := sub("write", "Write the key file and the .repo files of the release package", api.KindFile, "local write")
	write.Args = cobra.NoArgs
	write.RunE = a.run(hReleaseWrite)
	signLockWait(write)
	release.AddCommand(write)

	return []*cobra.Command{sign, release}
}

// signLockWait adds --lock-wait to a command that takes the set lock
// (cli.html#global: each command that takes the set lock).
func signLockWait(c *cobra.Command) {
	c.Flags().Duration("lock-wait", 0, "wait this long for the set lock\n(default: REPOCTL_LOCK_WAIT, else 0: do not wait)")
}
