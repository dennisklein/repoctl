package cli

import (
	"github.com/spf13/cobra"

	"github.com/dennisklein/repoctl/internal/api"
)

// repoCommands: publish and repo (cli.html#group-repository).
func (a *App) repoCommands() []*cobra.Command {
	publish := command("repository", "publish", `Sign the staged nodes and make them the new "latest"`, api.KindRun, "shared state")
	publish.Args = cobra.NoArgs
	publish.Flags().String("run", "", "take only the staged nodes of this build run")
	publish.Flags().Bool("dry-run", false, "show the summary and run the checks; sign nothing")
	publish.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	publish.Flags().StringArray("accept-advisory", nil, "accept a required advisory that blocks this publish")
	publish.Flags().StringArray("hold", nil, "keep the build of the base for this node")
	publish.Flags().String("reason", "", "the reason of --accept-advisory or --hold, 8 characters or more")
	publish.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	publish.Flags().Int("confirm-fd", -1, "")
	_ = publish.Flags().MarkHidden("confirm-fd")
	publish.RunE = a.run(hPublish)

	repo := command("repository", "repo", "List, compare, verify, pull, push, restore and prune snapshots", "", "read")

	ls := sub("ls", "List the RPMs of a repository", api.KindRepoContents, "read")
	ls.Args = cobra.NoArgs
	ls.Flags().String("run", "", "list the staging of the run")
	ls.Flags().String("path", "", "list any rpm-md directory")
	ls.Flags().String("repo", "binary", "the repository: binary, source, debuginfo")
	ls.Flags().String("name", "", "only the RPMs whose name matches the glob")
	ls.Flags().Bool("removed", false, "list removed[] of the RepoManifest")
	ls.RunE = a.run(hRepoLs)

	diff := sub("diff [SELECTOR...]", "Compare the staging of a run, or two snapshots", api.KindRepoDiff, "read")
	diff.Flags().String("run", "", "the staging of this run")
	diff.Flags().String("from", "", "the first snapshot")
	diff.Flags().String("to", "", "the second snapshot")
	diff.Flags().Bool("rpms", false, "show each binary RPM")
	diff.Flags().Bool("check", false, "exit 11 on a difference")
	diff.RunE = a.run(hRepoDiff)

	verify := sub("verify", "Check the signatures, checksums and records", api.KindDoctorReport, "read")
	verify.Args = cobra.NoArgs
	verify.Flags().String("snapshot", "", "check a registry snapshot")
	verify.Flags().String("run", "", "check the staging of a run")
	verify.Flags().String("path", "", "check a directory")
	verify.Flags().Bool("served", false, "check the served tree over HTTP")
	verify.Flags().Bool("el", false, "add the check in the verify root")
	verify.Flags().StringArray("key", nil, "use these keys in place of the client trust set")
	verify.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	verify.RunE = a.run(hRepoVerify)

	prov := sub("provenance NVR|PKG@TARGET", "Show the provenance record of one build", api.KindProvenance, "read")
	prov.Args = cobra.ExactArgs(1)
	prov.Flags().String("snapshot", "", "read the record from this snapshot")
	prov.Flags().Bool("raw", false, "write the stored in-toto Statement")
	prov.RunE = a.run(hRepoProvenance)

	sbom := sub("sbom", "Show the SBOM of each target", api.KindSBOM, "read")
	sbom.Args = cobra.NoArgs
	sbom.Flags().String("build", "", "the SBOM of one build")
	sbom.Flags().String("snapshot", "", "read the SBOM from this snapshot")
	sbom.Flags().Bool("raw", false, "write the stored CycloneDX file")
	sbom.RunE = a.run(hRepoSBOM)

	pull := sub("pull [REF]", "Make the local copy equal to a snapshot", api.KindSnapshot, "local write")
	pull.Args = cobra.MaximumNArgs(1)
	pull.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	pull.RunE = a.run(hRepoPull)

	push := sub("push [PUBLISH]", "Complete a publish that stopped, import or re-seed a target", api.KindRun, "shared state")
	push.Args = cobra.MaximumNArgs(1)
	push.Flags().String("from", "", "a signed tree")
	push.Flags().String("manifest", "", "the RepoManifest of the import")
	push.Flags().Bool("reseed", false, "make the snapshot of a target again after its loss")
	push.Flags().Bool("dry-run", false, "show the summary; push nothing")
	push.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	push.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	push.RunE = a.run(hRepoPush)

	snaps := sub("snapshots", "List the snapshots of each target", api.KindSnapshotList, "read")
	snaps.Args = cobra.NoArgs
	snaps.Flags().Int("limit", 0, "at most N snapshots")
	snaps.Flags().Bool("all", false, "add the tags that are not on the chain")
	snaps.Flags().Bool("history", false, "list the publish history of latest")
	snaps.RunE = a.run(hRepoSnapshots)

	restore := sub("restore REF", "Make a new snapshot with the files of an older one", api.KindRun, "shared state")
	restore.Args = cobra.ExactArgs(1)
	restore.Flags().Bool("dry-run", false, "show the summary; sign nothing")
	restore.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	restore.Flags().StringArray("accept-advisory", nil, "accept a blocking advisory")
	restore.Flags().String("reason", "", "the reason, 8 characters or more")
	restore.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	restore.RunE = a.run(hRepoRestore)

	prune := sub("prune", "Remove NVRs in a new snapshot", api.KindRun, "shared state")
	prune.Args = cobra.NoArgs
	prune.Flags().String("repo", "", "only the binary, source or debuginfo repository")
	prune.Flags().String("package", "", "only this source package")
	prune.Flags().Bool("all", false, "with --package: every NVR of a retired package")
	prune.Flags().Int("keep-last", 0, "keep the newest N NVRs of each package name")
	prune.Flags().String("older-than", "", "only NVRs older than D")
	prune.Flags().StringArray("nvr", nil, "one build with all its subpackages")
	prune.Flags().Bool("apply", false, "start a publish run of kind prune")
	prune.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	prune.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	prune.RunE = a.run(hRepoPrune)

	gc := sub("gc", "Delete old snapshots in the registry", api.KindSnapshotList, "shared state")
	gc.Args = cobra.NoArgs
	gc.Flags().Int("keep", 10, "keep the newest N snapshots of the chain")
	gc.Flags().String("keep-days", "90d", "keep the snapshots younger than D")
	gc.Flags().String("keep-removed", "1y", "keep the newest snapshot of each removed entry until D")
	gc.Flags().Bool("apply", false, "delete, oldest first")
	gc.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	gc.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	gc.RunE = a.run(hRepoGC)

	resign := sub("resign", "Re-sign the RPMs of a key with the key of this human", api.KindRun, "shared state")
	resign.Args = cobra.NoArgs
	resign.Flags().String("signer", "", "the key whose RPMs get a new signature (required)")
	resign.Flags().String("repo", "", "only the binary, source or debuginfo repository")
	resign.Flags().Bool("dry-run", false, "show the summary; sign nothing")
	resign.Flags().BoolP("yes", "y", false, "do not ask on a terminal")
	resign.Flags().Duration("lock-wait", 0, "wait this long for the set lock")
	resign.RunE = a.run(hRepoResign)

	repo.AddCommand(ls, diff, verify, prov, sbom, pull, push, snaps, restore, prune, gc, resign)
	return []*cobra.Command{publish, repo}
}
