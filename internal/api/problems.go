// Code generated from the problem registry of docs/design/contract.html#problems.
// Keep it equal to that table: a test compares the codes.

package api

// ProblemInfo is the registry entry of a problem code: the exit code that an
// error with the code gives (0 for a code that is only a warning or a note),
// the text of the Exit column, the meaning and the hint.
type ProblemInfo struct {
	Exit     int
	ExitText string
	Meaning  string
	Hint     string
}

// The problem codes of the registry.
const (
	PAdvisoriesInvalid         ProblemCode = "advisories.invalid"
	PAdvisoryGateUnavailable   ProblemCode = "advisory.gate-unavailable"
	PAdvisoryNotBlocking       ProblemCode = "advisory.not-blocking"
	PAdvisoryNotDismissable    ProblemCode = "advisory.not-dismissable"
	PAdvisoryOpen              ProblemCode = "advisory.open"
	PAdvisoryUnknown           ProblemCode = "advisory.unknown"
	PBranchExistsRemote        ProblemCode = "branch.exists-remote"
	PBranchStartMissing        ProblemCode = "branch.start-missing"
	PCancelForeignRun          ProblemCode = "cancel.foreign-run"
	PCancelNotHolder           ProblemCode = "cancel.not-holder"
	PCheckFailed               ProblemCode = "check.failed"
	PCheckIncomplete           ProblemCode = "check.incomplete"
	PCheckNever                ProblemCode = "check.never"
	PCheckNoLinger             ProblemCode = "check.no-linger"
	PCheckOutdated             ProblemCode = "check.outdated"
	PCheckStale                ProblemCode = "check.stale"
	PCheckTimerOff             ProblemCode = "check.timer-off"
	PCheckTimerOrphan          ProblemCode = "check.timer-orphan"
	PCheckTimerUnit            ProblemCode = "check.timer-unit"
	PConfigInvalid             ProblemCode = "config.invalid"
	PConfigUnknownKey          ProblemCode = "config.unknown-key"
	PConfigUnknownSet          ProblemCode = "config.unknown-set"
	PConfigUnsupported         ProblemCode = "config.unsupported"
	PContractNewerFile         ProblemCode = "contract.newer-file"
	PContractOldFile           ProblemCode = "contract.old-file"
	PDepNotForTarget           ProblemCode = "dep.not-for-target"
	PDeprecated                ProblemCode = "deprecated"
	PGCFailed                  ProblemCode = "gc.failed"
	PGitBusy                   ProblemCode = "git.busy"
	PGitFetchFailed            ProblemCode = "git.fetch-failed"
	PGitMirrorUnpushed         ProblemCode = "git.mirror-unpushed"
	PGitProtection             ProblemCode = "git.protection"
	PGitSigning                ProblemCode = "git.signing"
	PGitUnreachable            ProblemCode = "git.unreachable"
	PGraphAmbiguousProvider    ProblemCode = "graph.ambiguous-provider"
	PGraphCycle                ProblemCode = "graph.cycle"
	PGraphDynamicBuildrequires ProblemCode = "graph.dynamic-buildrequires"
	PGraphSRPMFailed           ProblemCode = "graph.srpm-failed"
	PHostNotReady              ProblemCode = "host.not-ready"
	PHostStaleServer           ProblemCode = "host.stale-server"
	PImportNotEmpty            ProblemCode = "import.not-empty"
	PJournalSamplingStopped    ProblemCode = "journal.sampling-stopped"
	PLockBehind                ProblemCode = "lock.behind"
	PLockDirty                 ProblemCode = "lock.dirty"
	PLockDiverged              ProblemCode = "lock.diverged"
	PLockMissingPackage        ProblemCode = "lock.missing-package"
	PLockOrphaned              ProblemCode = "lock.orphaned"
	PLockUnpushed              ProblemCode = "lock.unpushed"
	PManifestInvalid           ProblemCode = "manifest.invalid"
	PMCPToolNotAllowed         ProblemCode = "mcp.tool-not-allowed"
	PMockBusy                  ProblemCode = "mock.busy"
	PMockConfigDiffers         ProblemCode = "mock.config-differs"
	PMockConfigMissing         ProblemCode = "mock.config-missing"
	PMockUnavailable           ProblemCode = "mock.unavailable"
	PNodeBlocked               ProblemCode = "node.blocked"
	PNodeFailed                ProblemCode = "node.failed"
	PNVROlder                  ProblemCode = "nvr.older"
	PNVRRemoved                ProblemCode = "nvr.removed"
	PNVRReused                 ProblemCode = "nvr.reused"
	PPruneBreaksClosure        ProblemCode = "prune.breaks-closure"
	PPruneNotRetired           ProblemCode = "prune.not-retired"
	PPruneUnsafe               ProblemCode = "prune.unsafe"
	PPublishNeedsConfirmation  ProblemCode = "publish.needs-confirmation"
	PPublishNotReady           ProblemCode = "publish.not-ready"
	PPublishNotVerified        ProblemCode = "publish.not-verified"
	PPublishNothingToPublish   ProblemCode = "publish.nothing-to-publish"
	PPublishPlanChanged        ProblemCode = "publish.plan-changed"
	PPublishServedExists       ProblemCode = "publish.served-exists"
	PPublishUnattendedRefused  ProblemCode = "publish.unattended-refused"
	PRebuildInTree             ProblemCode = "rebuild.in-tree"
	PRecordsMismatch           ProblemCode = "records.mismatch"
	PRecordsMissing            ProblemCode = "records.missing"
	PRegistryDenied            ProblemCode = "registry.denied"
	PRegistryUnreachable       ProblemCode = "registry.unreachable"
	PRunAbandoned              ProblemCode = "run.abandoned"
	PRunNotFinished            ProblemCode = "run.not-finished"
	PRunNotFound               ProblemCode = "run.not-found"
	PRunStagingMissing         ProblemCode = "run.staging-missing"
	PRunUnpinned               ProblemCode = "run.unpinned"
	PServedStale               ProblemCode = "served.stale"
	PSetBusy                   ProblemCode = "set.busy"
	PSetLockLost               ProblemCode = "set.lock-lost"
	PSetRenamed                ProblemCode = "set.renamed"
	PSetRetired                ProblemCode = "set.retired"
	PSignCardAbsent            ProblemCode = "sign.card-absent"
	PSignExpiredSigner         ProblemCode = "sign.expired-signer"
	PSignFailed                ProblemCode = "sign.failed"
	PSignKeyExists             ProblemCode = "sign.key-exists"
	PSignKeyExpiring           ProblemCode = "sign.key-expiring"
	PSignKeyFormat             ProblemCode = "sign.key-format"
	PSignKeyInUse              ProblemCode = "sign.key-in-use"
	PSignKeyfileStale          ProblemCode = "sign.keyfile-stale"
	PSignLastSigner            ProblemCode = "sign.last-signer"
	PSignLocked                ProblemCode = "sign.locked"
	PSignNoKey                 ProblemCode = "sign.no-key"
	PSignNoPinentry            ProblemCode = "sign.no-pinentry"
	PSignNotActive             ProblemCode = "sign.not-active"
	PSignNotDeployed           ProblemCode = "sign.not-deployed"
	PSignNotRemoved            ProblemCode = "sign.not-removed"
	PSignRemovedSigner         ProblemCode = "sign.removed-signer"
	PSignRepofileStale         ProblemCode = "sign.repofile-stale"
	PSignRetiring              ProblemCode = "sign.retiring"
	PSignSelfConfirm           ProblemCode = "sign.self-confirm"
	PSignTrustBehind           ProblemCode = "sign.trust-behind"
	PSignTrustChange           ProblemCode = "sign.trust-change"
	PSignTrustUnverified       ProblemCode = "sign.trust-unverified"
	PSignUnknownSigner         ProblemCode = "sign.unknown-signer"
	PSignWrongState            ProblemCode = "sign.wrong-state"
	PSkillsGuard               ProblemCode = "skills.guard"
	PSkillsVersionMismatch     ProblemCode = "skills.version-mismatch"
	PSnapshotDiverged          ProblemCode = "snapshot.diverged"
	PSnapshotExists            ProblemCode = "snapshot.exists"
	PSnapshotMissing           ProblemCode = "snapshot.missing"
	PSnapshotNotFound          ProblemCode = "snapshot.not-found"
	PSnapshotOrphaned          ProblemCode = "snapshot.orphaned"
	PSnapshotPushFailed        ProblemCode = "snapshot.push-failed"
	PSourceInTreeAutorelease   ProblemCode = "source.in-tree-autorelease"
	PSourceMismatch            ProblemCode = "source.mismatch"
	PSourceUnlisted            ProblemCode = "source.unlisted"
	PSourceUnreachable         ProblemCode = "source.unreachable"
	PStateBehind               ProblemCode = "state.behind"
	PStateUnpushed             ProblemCode = "state.unpushed"
	PUpstreamNoMatch           ProblemCode = "upstream.no-match"
	PUpstreamRateLimited       ProblemCode = "upstream.rate-limited"
	PUpstreamUnreachable       ProblemCode = "upstream.unreachable"
	PVerifyELFailed            ProblemCode = "verify.el-failed"
	PVerifyFailed              ProblemCode = "verify.failed"
	PWorktreeDirty             ProblemCode = "worktree.dirty"
	PWorktreeMissing           ProblemCode = "worktree.missing"
)

// Problems is the registry of the problem codes, sorted by code.
var Problems = map[ProblemCode]ProblemInfo{
	PAdvisoriesInvalid:         {Exit: 1, ExitText: "1 in dismiss, undismiss; warning", Meaning: ".repoctl/advisories.yaml is invalid or has conflict markers. repoctl ignores the file, so no advisory is dismissed.", Hint: "Keep the entries of both sides, remove the markers, commit (dismissals)."},
	PAdvisoryGateUnavailable:   {Exit: 13, ExitText: "13", Meaning: "The advisory gate cannot read the repodata of the result tree or of a closure repo. It fails closed.", Hint: "Try again when the repo answers. The message names the repo."},
	PAdvisoryNotBlocking:       {Exit: 12, ExitText: "12", Meaning: "--accept-advisory names an id that does not block this publish.", Hint: "Remove the flag for this id; repoctl publish --dry-run shows the gate rows."},
	PAdvisoryNotDismissable:    {Exit: 1, ExitText: "1", Meaning: "A required advisory cannot be dismissed.", Hint: "repoctl publish --accept-advisory ID --reason TEXT"},
	PAdvisoryOpen:              {Exit: 12, ExitText: "12", Meaning: "A required advisory is open, and the publish changes its closure.", Hint: "Fix it, or accept it for one publish (accept-advisory)."},
	PAdvisoryUnknown:           {Exit: 1, ExitText: "1", Meaning: "No advisory has this id.", Hint: "repoctl advisories ls"},
	PBranchExistsRemote:        {Exit: 1, ExitText: "1", Meaning: "checkout --create: the branch is on the remote.", Hint: "repoctl checkout PKG"},
	PBranchStartMissing:        {Exit: 1, ExitText: "1", Meaning: "checkout --create --from-set SET: SET has no such package.", Hint: "Name another start point."},
	PCancelForeignRun:          {Exit: 1, ExitText: "1", Meaning: "MCP: the server cancels only the runs that it started (its session id is in origin.session).", Hint: "The human runs repoctl cancel RUN (cancel-foreign-run)."},
	PCancelNotHolder:           {Exit: 1, ExitText: "1", Meaning: "CLI, web UI: the process of the run is on another host or belongs to another user.", Hint: "Cancel the run on its host, as its user."},
	PCheckFailed:               {Exit: 0, ExitText: "warning", Meaning: "The last check failed.", Hint: "repoctl check"},
	PCheckIncomplete:           {Exit: 1, ExitText: "1", Meaning: "advisories undismiss --stale: a code or target of the last check has no good result.", Hint: "repoctl check"},
	PCheckNever:                {Exit: 0, ExitText: "warning", Meaning: "No check ran for this set on this host.", Hint: "repoctl check --install-timer"},
	PCheckNoLinger:             {Exit: 0, ExitText: "note", Meaning: "Lingering is off, so the timer runs only while the user is logged in.", Hint: "loginctl enable-linger USER"},
	PCheckOutdated:             {Exit: 0, ExitText: "warning", Meaning: "latest of a target differs from the digest that the last check used.", Hint: "repoctl check"},
	PCheckStale:                {Exit: 0, ExitText: "warning", Meaning: "The last check is older than 36 hours, or older than latest of a target.", Hint: "repoctl check"},
	PCheckTimerOff:             {Exit: 0, ExitText: "warning", Meaning: "The timer of this set is not active.", Hint: "repoctl check --install-timer"},
	PCheckTimerOrphan:          {Exit: 0, ExitText: "warning", Meaning: "A timer instance points to a set directory that is gone or holds no set.", Hint: "systemctl --user disable --now repoctl-check@INSTANCE.timer"},
	PCheckTimerUnit:            {Exit: 0, ExitText: "warning", Meaning: "The installed timer units differ from the units of this binary.", Hint: "repoctl check --install-timer"},
	PConfigInvalid:             {Exit: 1, ExitText: "1", Meaning: "A configuration file does not agree with its schema or with a rule.", Hint: "Correct the file; repoctl schema config."},
	PConfigUnknownKey:          {Exit: 1, ExitText: "1", Meaning: "A key is unknown, or the user config sets a key that changes the build.", Hint: "Remove the key, or upgrade repoctl when a newer release knows it; repoctl config show."},
	PConfigUnknownSet:          {Exit: 0, ExitText: "warning", Meaning: "config show: the user config has sets.NAME for a set that this host does not know.", Hint: "Correct NAME, or remove the key."},
	PConfigUnsupported:         {Exit: 1, ExitText: "1", Meaning: "This release does not support the key or its value, for example a target that is not x86_64.", Hint: "Remove the key, or change the value."},
	PContractNewerFile:         {Exit: 1, ExitText: "1; warning in a command that only reads the file", Meaning: "Its apiVersion, a field, a closed enum value or minRepoctl of a stored file is newer than this binary.", Hint: "Upgrade repoctl on this host (upgrade)."},
	PContractOldFile:           {Exit: 1, ExitText: "1", Meaning: "The command must write a stored file in an apiVersion that this binary only reads.", Hint: "repoctl migrate --api, then commit."},
	PDepNotForTarget:           {Exit: 0, ExitText: "warning", Meaning: "Plan: only a package of another target provides a BuildRequires of the node.", Hint: "Guard the BuildRequires with %if, or add the target."},
	PDeprecated:                {Exit: 0, ExitText: "warning", Meaning: "The input uses a deprecated name, for example sign.gpg_name.", Hint: "Use the name in the message."},
	PGCFailed:                  {Exit: 13, ExitText: "13", Meaning: "repo gc --apply: the GitHub API failed one delete, and gc stopped.", Hint: "Run repoctl repo gc --apply again. The dry run shows what is left."},
	PGitBusy:                   {Exit: 7, ExitText: "7", Meaning: "Another command holds the clone lock of an external package. The message names the clone and the holder.", Hint: "Try again, or set --lock-wait."},
	PGitFetchFailed:            {Exit: 1, ExitText: "1", Meaning: "git fetch failed.", Hint: "Examine the remote and the credentials."},
	PGitMirrorUnpushed:         {Exit: 0, ExitText: "warning", Meaning: "clean --git keeps a bare clone with commits that are not on the remote.", Hint: "Push the commits."},
	PGitProtection:             {Exit: 0, ExitText: "warning in doctor", Meaning: "A branch of the set repository does not have the rules of its GitHub ruleset, or doctor cannot read them.", Hint: "An organization owner sets the ruleset (GitHub rulesets)."},
	PGitSigning:                {Exit: 0, ExitText: "warning in doctor", Meaning: "git in the set directory does not sign commits with the key of sign.key.", Hint: "git config commit.gpgsign true and git config user.signingkey FPR (Signed set inputs)."},
	PGitUnreachable:            {Exit: 1, ExitText: "1", Meaning: "git ls-remote of an external package failed.", Hint: "Make the project on the git server."},
	PGraphAmbiguousProvider:    {Exit: 0, ExitText: "warning", Meaning: "Two set packages provide one capability.", Hint: "Make the Provides unique."},
	PGraphCycle:                {Exit: 1, ExitText: "1", Meaning: "The BuildRequires make a cycle. The message names it.", Hint: "Break the cycle in one spec."},
	PGraphDynamicBuildrequires: {Exit: 0, ExitText: "warning", Meaning: "The spec uses %generate_buildrequires, so edges can be missing.", Hint: "Add the BuildRequires to the spec."},
	PGraphSRPMFailed:           {Exit: 0, ExitText: "warning in plan, graph; the node fails in build (class srpm)", Meaning: "The SRPM of a node failed at plan time, so the node has no known dependencies.", Hint: "Fix the spec. repoctl graph -v shows the mock output."},
	PHostNotReady:              {Exit: 9, ExitText: "9", Meaning: "A required host check failed.", Hint: "repoctl doctor (install-tool)"},
	PHostStaleServer:           {Exit: 9, ExitText: "9; warning in doctor", Meaning: "A UI or MCP server runs a binary that an upgrade replaced. It starts no child.", Hint: "Restart the server: repoctl ui, or a new agent session."},
	PImportNotEmpty:            {Exit: 13, ExitText: "13", Meaning: "repo push --from (an import): the registry has a snapshot of the target.", Hint: "repoctl repo snapshots --target ID; publish on top of latest, or import into a new target."},
	PJournalSamplingStopped:    {Exit: 0, ExitText: "warning", Meaning: "The journal is 32 MiB. No more node.log lines come.", Hint: "The node log has the full text."},
	PLockBehind:                {Exit: 5, ExitText: "5 in pin PKG; warning in pin", Meaning: "The worktree HEAD is an ancestor of the lock commit. pin does not write the older commit back.", Hint: "repoctl checkout PKG; repoctl pin PKG --older pins the older commit on purpose."},
	PLockDirty:                 {Exit: 5, ExitText: "5", Meaning: "A worktree is not in-sync with the lock, or a set input is not committed.", Hint: "The exact git commands (commit-worktree, commit-lock)."},
	PLockDiverged:              {Exit: 5, ExitText: "5", Meaning: "pin: the worktree and the lock commit both have commits that the other does not have.", Hint: "Rebase packages/PKG on the lock commit, then repoctl pin PKG."},
	PLockMissingPackage:        {Exit: 5, ExitText: "5", Meaning: "build --from lock selects a package without a lock line, or publish takes a target that has one.", Hint: "repoctl pin PKG"},
	PLockOrphaned:              {Exit: 5, ExitText: "5", Meaning: "The lock has a line for a package that repoctl.yaml does not have.", Hint: "repoctl pin --orphans"},
	PLockUnpushed:              {Exit: 5, ExitText: "5", Meaning: "The set commit or a pinned commit is not on its remote branch.", Hint: "The exact git push commands (push)."},
	PManifestInvalid:           {Exit: 1, ExitText: "1", Meaning: "The RepoManifest of a snapshot does not agree with the schema manifest.", Hint: "The message names the field."},
	PMCPToolNotAllowed:         {Exit: 0, ExitText: "MCP only", Meaning: "The tool is not in the mode of this server.", Hint: "The human runs the command."},
	PMockBusy:                  {Exit: 7, ExitText: "7", Meaning: "repoctl mock update or mock clean holds the host mock lock, or a repoctl mock runs while one of them waits for it. The problem names the holder.", Hint: "Wait, or use --lock-wait (Mock roots)."},
	PMockConfigDiffers:         {Exit: 0, ExitText: "warning in doctor", Meaning: "The mock config of a target on this host differs from the config of the newest build in latest. mock.drift and the default closure repos depend on it.", Hint: "Install the mock config of the build host, or rebuild against the new config (data)."},
	PMockConfigMissing:         {Exit: 9, ExitText: "9", Meaning: "The mock config of a target is not on this host.", Hint: "Install it, or correct targets[].mock."},
	PMockUnavailable:           {Exit: 9, ExitText: "9", Meaning: "mock or podman is not installed, or the user is not in the group mock. mock makes the bootstrap root from a podman image; without podman, the buildroot file has no image digest.", Hint: "Install mock (install-tool)."},
	PNodeBlocked:               {Exit: 3, ExitText: "3", Meaning: "A dependency failed. The node did not run.", Hint: "Repair the failed dependency."},
	PNodeFailed:                {Exit: 3, ExitText: "3", Meaning: "The node failed in a step.", Hint: "repoctl logs RUN NODE, or the skill triage-build."},
	PNVROlder:                  {Exit: 12, ExitText: "12", Meaning: "A new NVR is not newer than the newest NVR of its source package name in the base or in removed[].", Hint: "Raise Epoch or the version (Restore and fix forward)."},
	PNVRRemoved:                {Exit: 12, ExitText: "12", Meaning: "A prune or a restore removed this NVR, and the new build has other content.", Hint: "repoctl rebuild PKG --reason TEXT"},
	PNVRReused:                 {Exit: 12, ExitText: "12", Meaning: "latest holds this NVR with other content.", Hint: "repoctl rebuild PKG --reason TEXT"},
	PPruneBreaksClosure:        {Exit: 12, ExitText: "12", Meaning: "The prune gives a broken requirement that the base does not have.", Hint: "Keep the NVR that the message names; prune the requirer first."},
	PPruneNotRetired:           {Exit: 1, ExitText: "1", Meaning: "repo prune --package NAME --all: NAME is not retired.", Hint: "Retire it first (Retire)."},
	PPruneUnsafe:               {Exit: 12, ExitText: "12", Meaning: "A prune selects a protected NVR: the newest NVR of a kept package or a release NVR of a deployment record.", Hint: "The message names the rule. Select other NVRs."},
	PPublishNeedsConfirmation:  {Exit: 6, ExitText: "6", Meaning: "No terminal for the confirmation, or the human answers no.", Hint: "Publish in a terminal or in the UI (confirm)."},
	PPublishNotReady:           {Exit: 12, ExitText: "12", Meaning: "A node of a selected target is not current or staged.", Hint: "Build it, or publish the other targets. For an urgent fix, hold the node for one publish: repoctl publish --hold NODE --reason TEXT (urgent fix)."},
	PPublishNotVerified:        {Exit: 10, ExitText: "10", Meaning: "repo push: the state file of the tree does not record verify and verify-el ok for the target.", Hint: "Discard the tree: repoctl clean --publish p-\u2026, then publish again."},
	PPublishNothingToPublish:   {Exit: 4, ExitText: "4", Meaning: "No node is staged, or prune finds nothing.", Hint: "-"},
	PPublishPlanChanged:        {Exit: 6, ExitText: "6", Meaning: "The plan digest under the set lock differs from the plan digest of the UI confirmation. Nothing is signed.", Hint: "Review the publish again (confirm, UI confirmation)."},
	PPublishServedExists:       {Exit: 12, ExitText: "12", Meaning: "A first publish of a target: repo.url serves a repo or does not answer with 404.", Hint: "Import the repo (import runbook), or correct store.oci."},
	PPublishUnattendedRefused:  {Exit: 6, ExitText: "6", Meaning: "--yes without a terminal and without REPOCTL_ALLOW_UNATTENDED_PUBLISH=1, or --accept-advisory without a terminal.", Hint: "Shared-state safety"},
	PRebuildInTree:             {Exit: 1, ExitText: "1; warning for a glob", Meaning: "repoctl rebuild: the package is in-tree, so an empty commit does not change its commit.", Hint: "Raise the Release in the spec of the package, and commit it to the set branch."},
	PRecordsMismatch:           {Exit: 10, ExitText: "10", Meaning: "A supply chain record does not have the sha256 in the RepoManifest, or a subject does not have the sha256 of its file.", Hint: "The message names the record. Publish keeps the tree; the mirror job keeps the old tree."},
	PRecordsMissing:            {Exit: 1, ExitText: "1; warning in repo verify", Meaning: "The build has no record of this type, for example an adopted build without provenance.", Hint: "-"},
	PRegistryDenied:            {Exit: 13, ExitText: "13", Meaning: "The token has no write scope for the OCI package.", Hint: "The message names the scope."},
	PRegistryUnreachable:       {Exit: 13, ExitText: "13; warning in plan, build", Meaning: "The registry does not answer.", Hint: "Plan and build use the local copy."},
	PRunAbandoned:              {Exit: 1, ExitText: "1", Meaning: "runs --follow, repoctl_run_wait: the process of the run ended without run.finished.", Hint: "repoctl mock clean"},
	PRunNotFinished:            {Exit: 1, ExitText: "1", Meaning: "The run is still active.", Hint: "repoctl runs RUN --follow"},
	PRunNotFound:               {Exit: 1, ExitText: "1", Meaning: "No run has this id.", Hint: "repoctl runs"},
	PRunStagingMissing:         {Exit: 1, ExitText: "1 in publish; warning in plan, build", Meaning: "A file of a staged node is not in the staging of its run, or has another size or sha256.", Hint: "repoctl plan, then repoctl build"},
	PRunUnpinned:               {Exit: 12, ExitText: "12", Meaning: "The run built worktree commits.", Hint: "Pin, then build from the lock."},
	PServedStale:               {Exit: 0, ExitText: "warning", Meaning: "The served tree is behind latest for more than 15 minutes, or the mirror job wrote no heartbeat for 15 minutes.", Hint: "Read the journal of the mirror job on the HTTP server (Freshness)."},
	PSetBusy:                   {Exit: 7, ExitText: "7", Meaning: "Another command of this user on this host holds the set lock.", Hint: "repoctl runs RUN --follow"},
	PSetLockLost:               {Exit: 0, ExitText: "warning; the run ends canceled (130)", Meaning: "The lock file of the set lock was removed during the run, and another process now holds the set lock.", Hint: "Turn on lingering: loginctl enable-linger USER (Set lock)."},
	PSetRenamed:                {Exit: 12, ExitText: "12", Meaning: "A first publish of a target: the served tree at repo.url names another set or target.", Hint: "Give the set its own repo.url. A set name or a target id never changes (Rename)."},
	PSetRetired:                {Exit: 12, ExitText: "12; warning in plan", Meaning: "Each target of the set has state: retired.", Hint: "Work in the next set (End a set)."},
	PSignCardAbsent:            {Exit: 9, ExitText: "9", Meaning: "No card for sign.key.", Hint: "Insert the card (unlock)."},
	PSignExpiredSigner:         {Exit: 0, ExitText: "warning", Meaning: "A kept RPM has a signature that its key made before the key expired. EL10 clients refuse the RPM.", Hint: "repoctl repo resign --signer ID (After the expiry)"},
	PSignFailed:                {Exit: 1, ExitText: "1", Meaning: "rpmsign failed. repoctl deletes the new tree.", Hint: "The message names the file."},
	PSignKeyExists:             {Exit: 1, ExitText: "1", Meaning: "sign add: sign.keys has the id or the fingerprint, in any state.", Hint: "Use a new id for a new key (Key lifecycle)."},
	PSignKeyExpiring:           {Exit: 12, ExitText: "12", Meaning: "Gate 1: the signer key expires within sign.checks.key_min_validity.", Hint: "Add a new key, then retire this key (Key expiry)."},
	PSignKeyFormat:             {Exit: 9, ExitText: "9", Meaning: "Gate 3: the key format is not allowed.", Hint: "Gates"},
	PSignKeyInUse:              {Exit: 12, ExitText: "12", Meaning: "sign remove: latest holds RPMs of the key.", Hint: "repoctl repo resign --signer ID"},
	PSignKeyfileStale:          {Exit: 5, ExitText: "5", Meaning: "The key file of the in-tree release package differs from sign.keys.", Hint: "repoctl release write, then commit."},
	PSignLastSigner:            {Exit: 12, ExitText: "12", Meaning: "sign retire: for a target with a latest, no other active key has a deployment record.", Hint: "Deploy a second key first (Onboarding)."},
	PSignLocked:                {Exit: 9, ExitText: "9", Meaning: "The card or the key is locked.", Hint: "gpg --card-status (unlock). A blocked PIN: Card and key failures."},
	PSignNoKey:                 {Exit: 9, ExitText: "9; warning in build", Meaning: "The secret key of sign.key is not on this host.", Hint: "repoctl doctor"},
	PSignNoPinentry:            {Exit: 9, ExitText: "9", Meaning: "No pinentry works.", Hint: "Install a pinentry."},
	PSignNotActive:             {Exit: 12, ExitText: "12", Meaning: "Gate 1: the key is not active in sign.keys.", Hint: "repoctl sign onboard ID"},
	PSignNotDeployed:           {Exit: 12, ExitText: "12", Meaning: "Gate 2: keys/deployed.yaml has no record of the key for a selected target.", Hint: "repoctl sign onboard ID (sign-deployed)"},
	PSignNotRemoved:            {Exit: 12, ExitText: "12", Meaning: "sign removed: the key is not in state removed, or the key file in latest of the target still holds it.", Hint: "repoctl sign offboard ID"},
	PSignRemovedSigner:         {Exit: 12, ExitText: "12; warning in repo pull", Meaning: "A kept RPM has the signature of a removed or revoked key.", Hint: "repoctl repo resign --signer ID"},
	PSignRepofileStale:         {Exit: 5, ExitText: "5", Meaning: "A .repo file of the in-tree release package differs from the file that repoctl release write writes.", Hint: "repoctl release write, then commit."},
	PSignRetiring:              {Exit: 0, ExitText: "warning in doctor", Meaning: "RPMs in latest have the signature of a retiring key.", Hint: "repoctl repo resign --signer ID"},
	PSignSelfConfirm:           {Exit: 12, ExitText: "12", Meaning: "sign deployed: the key of sign.key is the key of the record, and another key is active.", Hint: "An admin with another active key confirms the deployment."},
	PSignTrustBehind:           {Exit: 13, ExitText: "13", Meaning: "The tip of the remote branch has another repoctl.yaml or keys/ than the set commit of the publish.", Hint: "git pull --ff-only, then publish again (Trust records on the remote branch)."},
	PSignTrustChange:           {Exit: 6, ExitText: "6", Meaning: "The publish changes the trust set, and the human must type the key id.", Hint: "Use a terminal or the UI (trust-change)."},
	PSignTrustUnverified:       {Exit: 12, ExitText: "12", Meaning: "A set input changed without a valid signature of an allowed key, or a new deployment record names another key than its commit signer. The message names the commit.", Hint: "Review the commit, then sign a review commit (Review commit)."},
	PSignUnknownSigner:         {Exit: 10, ExitText: "10", Meaning: "An RPM has the signature of a key outside the trust set.", Hint: "The message names the files. In repo pull, when the set checkout does not hold the set commit of the snapshot: git pull origin set/SET, then pull again."},
	PSignWrongState:            {Exit: 1, ExitText: "1", Meaning: "A sign command does not allow the current state of the key, for example sign remove of an active key.", Hint: "The message gives the allowed command (Key lifecycle)."},
	PSkillsGuard:               {Exit: 0, ExitText: "warning in doctor", Meaning: "A skill of repoctl is installed or the plugin is on, and no guard hook covers the set directory; or a deny list differs.", Hint: "repoctl skills install with the same scope (Guard hook)."},
	PSkillsVersionMismatch:     {Exit: 0, ExitText: "warning", Meaning: "An installed copy of a skill has another version than the binary: older or newer. The message names the copy.", Hint: "repoctl skills install for a user or project copy; update the plugin or the RPM for the others."},
	PSnapshotDiverged:          {Exit: 13, ExitText: "13", Meaning: "latest moved between the publish steps check and tag.", Hint: "repoctl repo pull (repo-pull), then plan and publish again. Discard the tree: repoctl clean --publish p-\u2026."},
	PSnapshotExists:            {Exit: 13, ExitText: "13", Meaning: "repo push --reseed, also with --from: the registry has a snapshot of the target.", Hint: "Use publish. A re-seed works only on an empty package."},
	PSnapshotMissing:           {Exit: 13, ExitText: "13; warning in plan, build, status", Meaning: "The registry has no snapshot of the target, but the local copy or keys/deployed.yaml says that it was published.", Hint: "Re-seed it: repoctl repo push --reseed --target ID (repo-push, Registry outage and loss)."},
	PSnapshotNotFound:          {Exit: 1, ExitText: "1", Meaning: "REF names no snapshot of the target: no tag, publish id or digest has this value.", Hint: "repoctl repo snapshots --all"},
	PSnapshotOrphaned:          {Exit: 0, ExitText: "warning", Meaning: "The local copy holds a publish of this user that is not on the parent chain of latest.", Hint: "repoctl repo pull, then plan and publish again. The staged nodes of that publish are staged again."},
	PSnapshotPushFailed:        {Exit: 13, ExitText: "13", Meaning: "An upload failed. The signed tree stays.", Hint: "repoctl repo push p-\u2026 (repo-push)"},
	PSourceInTreeAutorelease:   {Exit: 0, ExitText: "warning in plan, graph; the node fails in build (class srpm)", Meaning: "The spec of an in-tree package uses %autorelease or %autochangelog. The rpmautospec plugin needs a package repository, so the release would stay 1.", Hint: "Write a plain Release and %changelog."},
	PSourceMismatch:            {Exit: 0, ExitText: "warning in plan, graph; the node fails in build (class srpm)", Meaning: "A source archive does not have the SHA-512 of its line in the sources file.", Hint: "Find out why the archive changed. Then correct the archive or the line, and commit."},
	PSourceUnlisted:            {Exit: 0, ExitText: "warning in plan, graph; the node fails in build (class srpm)", Meaning: "A Source line has a URL, and the sources file has no SHA512 line for its file.", Hint: "Add the line, for example with sha512sum --tag FILE, and commit (Sources)."},
	PSourceUnreachable:         {Exit: 0, ExitText: "warning in plan, graph; the node fails in build (class srpm)", Meaning: "No source of the file answers, and the source cache does not have it.", Hint: "Try again with network access."},
	PStateBehind:               {Exit: 13, ExitText: "13", Meaning: "latest is newer than the local copy.", Hint: "repoctl repo pull (repo-pull). When latest is the new snapshot of a pushable tree of this user: repoctl repo push p-\u2026, which runs only the cache step."},
	PStateUnpushed:             {Exit: 13, ExitText: "13; warning in plan, build", Meaning: "This user has a pushable tree, and latest is still its base.", Hint: "repoctl repo push p-\u2026 (repo-push), or discard the tree: repoctl clean --publish p-\u2026."},
	PUpstreamNoMatch:           {Exit: 0, ExitText: "warning", Meaning: "The tag filter matches no upstream tag.", Hint: "Correct upstream.tags."},
	PUpstreamRateLimited:       {Exit: 0, ExitText: "warning", Meaning: "The GitHub API limit is reached.", Hint: "Set GH_TOKEN."},
	PUpstreamUnreachable:       {Exit: 1, ExitText: "1 in check; warning", Meaning: "An upstream does not answer. The other checks continue.", Hint: "Try again later."},
	PVerifyELFailed:            {Exit: 10, ExitText: "10", Meaning: "The check in the EL chroot with the client trust set fails.", Hint: "The message names the files."},
	PVerifyFailed:              {Exit: 10, ExitText: "10", Meaning: "A signature, a checksum or the repodata is wrong.", Hint: "The message names the files."},
	PWorktreeDirty:             {Exit: 0, ExitText: "warning", Meaning: "A worktree is not in-sync with the lock (status, checkout, and plan for a worktree that no selected node uses).", Hint: "Commit and pin."},
	PWorktreeMissing:           {Exit: 0, ExitText: "warning", Meaning: "The worktree of a package is not there.", Hint: "repoctl checkout PKG"},
}

// The codes that repoctl gives beyond the registry of the contract table.
// The contract keeps its list open (contract.html#problems): a consumer
// treats an unknown code as a generic error or warning. Problems stays
// equal to the contract table; these codes are in Unlisted, and Lookup
// finds both.
const (
	PInternal               ProblemCode = "internal"
	PInternalNotImplemented ProblemCode = "internal.not-implemented"
	PUsage                  ProblemCode = "usage"
	PCleanConfirm           ProblemCode = "clean.confirm"
	PCleanNotImplemented    ProblemCode = "clean.not-implemented"
	PDoctorWarning          ProblemCode = "doctor.warning"
	PMCPInvalidInput        ProblemCode = "mcp.invalid-input"
	PMCPResultTooLarge      ProblemCode = "mcp.result-too-large"
	PMigrateKey             ProblemCode = "migrate.key"
	PMigrateNotes           ProblemCode = "migrate.notes"
	PMigrateOrder           ProblemCode = "migrate.order"
	PNodeCanceled           ProblemCode = "node.canceled"
	PNodeUnknown            ProblemCode = "node.unknown"
	PRunCanceled            ProblemCode = "run.canceled"
	PRunFailed              ProblemCode = "run.failed"
	PSetNone                ProblemCode = "set.none"
	PSkillsChanged          ProblemCode = "skills.changed"
	PUpstreamRelease        ProblemCode = "upstream.release"
)

// Unlisted is the registry of the codes outside the contract table, in the
// form of Problems.
var Unlisted = map[ProblemCode]ProblemInfo{
	PInternal:               {Exit: 1, ExitText: "1", Meaning: "An error that repoctl does not expect, for example a failed system call.", Hint: "Run the command again with -vvv; report the output when it stays."},
	PInternalNotImplemented: {Exit: 1, ExitText: "1", Meaning: "The command is not in this release.", Hint: "Upgrade repoctl (upgrade)."},
	PUsage:                  {Exit: 2, ExitText: "2", Meaning: "A flag or an argument is wrong. The terminal shows a usage error, without an envelope.", Hint: "repoctl help COMMAND"},
	PCleanConfirm:           {Exit: 6, ExitText: "6", Meaning: "repoctl clean deletes a staging or a signed tree only after a confirmation: the answer yes on a terminal, or --yes.", Hint: "Run it in a terminal, or add --yes."},
	PCleanNotImplemented:    {Exit: 0, ExitText: "warning", Meaning: "repoctl clean --git is not in this release; it deletes nothing of the bare clones.", Hint: "-"},
	PDoctorWarning:          {Exit: 8, ExitText: "8", Meaning: "doctor --strict: a check gave a warning.", Hint: "repoctl doctor shows the warnings and their fixes."},
	PMCPInvalidInput:        {Exit: 2, ExitText: "MCP only", Meaning: "The MCP server refuses the input of a tool; the CLI gives exit 2 without an envelope.", Hint: "Correct the arguments; tools/list gives the input schema."},
	PMCPResultTooLarge:      {Exit: 1, ExitText: "MCP only", Meaning: "The result of a tool is larger than the limit of the MCP server.", Hint: "Narrow the call, for example with packages, targets, name, tail or grep."},
	PMigrateKey:             {Exit: 0, ExitText: "warning", Meaning: "migrate: gpg cannot read the key file of the release package, so sign.keys has no key.", Hint: "Add the key to sign.keys by hand: gpg --show-keys --with-fingerprint FILE (import runbook)."},
	PMigrateNotes:           {Exit: 0, ExitText: "warning", Meaning: "migrate: the upstream notes of the package-info skill are not migrated.", Hint: "Add notes: to these entries by hand: $EDITOR repoctl.yaml."},
	PMigrateOrder:           {Exit: 0, ExitText: "warning", Meaning: "migrate: the hand order of scripts/build differs from the graph. repoctl builds in the order of the graph.", Hint: "No error: write the difference in the report of the import (import runbook)."},
	PNodeCanceled:           {Exit: 3, ExitText: "3", Meaning: "A cancel stopped the node before its end.", Hint: "repoctl build builds the open nodes again."},
	PNodeUnknown:            {Exit: 1, ExitText: "1", Meaning: "Web UI: the run has no node with this id.", Hint: "Open the run page."},
	PRunCanceled:            {Exit: 130, ExitText: "130", Meaning: "The run was canceled (Ctrl-C, repoctl cancel).", Hint: "repoctl build builds the open nodes again."},
	PRunFailed:              {Exit: 3, ExitText: "the exit code of the run", Meaning: "runs --follow: the run ended failed.", Hint: "repoctl runs RUN"},
	PSetNone:                {Exit: 1, ExitText: "1", Meaning: "Web UI: no set is on the list of the server.", Hint: "repoctl ui -C DIR adds a set."},
	PSkillsChanged:          {Exit: 1, ExitText: "1", Meaning: "skills install or uninstall: a human changed a skill file, or skills install did not write a skill directory.", Hint: "Keep the change elsewhere and remove the file, then install again."},
	PUpstreamRelease:        {Exit: 11, ExitText: "11 with --check", Meaning: "upstream --check: a package has a newer upstream release that no dismissal hides.", Hint: "repoctl upstream PKG --releases"},
}

// Lookup gives the registry entry of a code: Problems, then Unlisted.
func Lookup(code ProblemCode) (ProblemInfo, bool) {
	if info, ok := Problems[code]; ok {
		return info, true
	}
	info, ok := Unlisted[code]
	return info, ok
}
