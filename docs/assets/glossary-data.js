// The glossary of the repoctl design record: the one source of the project words.
// tools/site/build.py reads it to make design/glossary.html and to link the first use
// of each term in each section. The mockups and site.js read it for the hover card.
// Keep the part after "=" strict JSON: double quotes, no trailing commas, no comments.
//   id      anchor on the glossary page (design/glossary.html#ID)
//   kind    noun, verb, or noun and verb
//   domain  key of "domains"; gives the icon and the colour
//   forms   the exact words that link to the term (case-sensitive; a capital first
//           letter also matches). The longest form wins.
//   see     the chapter section that explains the term, relative to design/
//   def     one sentence or two, ASD-STE100; inline HTML (code) only
//   only_after, not_after, not_before (optional): Python regular expressions on the text before and after a form in the same text node.
//           A form links only when the text before matches only_after, the text
//           before does not match not_after and the text after does not match not_before.
//           They keep the English verb ("run the command", "is not set") plain.
window.REPOCTL_GLOSSARY = {
 "domains": {
  "build": {
   "name": "Build",
   "icon": "M8 1.8 13.6 5v6L8 14.2 2.4 11V5Z M2.4 5 8 8.2 13.6 5 M8 8.2v6"
  },
  "publish": {
   "name": "Publish and records",
   "icon": "M8 2 14.2 5 8 8 1.8 5Z M1.8 8.1 8 11.1l6.2-3 M1.8 11.1 8 14.1l6.2-3"
  },
  "trust": {
   "name": "Signing and trust",
   "icon": "M7.6 8.4 13.6 2.4 M11.4 4.6l1.6 1.6 M9.9 6.1l1.3 1.3 M8.1 10.5a3.1 3.1 0 1 1-6.2 0 3.1 3.1 0 0 1 6.2 0Z"
  },
  "adv": {
   "name": "Advisories",
   "icon": "M3.6 14.4V2 M3.6 2.6h8.6l-2 3 2 3H3.6"
  },
  "agents": {
   "name": "Agents",
   "icon": "M8 1.6c.7 3.6 2 5 6.4 6.4C10 9.4 8.7 10.8 8 14.4 7.3 10.8 6 9.4 1.6 8 6 6.6 7.3 5.2 8 1.6Z"
  },
  "contract": {
   "name": "Machine contract",
   "icon": "M5.6 2.4c-1.6 0-2.1.7-2.1 2v1.7c0 .9-.5 1.6-1.5 1.9 1 .3 1.5 1 1.5 1.9v1.7c0 1.3.5 2 2.1 2 M10.4 2.4c1.6 0 2.1.7 2.1 2v1.7c0 .9.5 1.6 1.5 1.9-1 .3-1.5 1-1.5 1.9v1.7c0 1.3-.5 2-2.1 2"
  }
 },
 "terms": [
  {"id": "accept", "term": "accept", "kind": "verb", "domain": "adv", "forms": ["accept", "accepts", "accepted"], "see": "advisories.html#gate", "def": "To publish over a required advisory, with a reason."},
  {"id": "acceptance", "term": "acceptance", "kind": "noun", "domain": "adv", "forms": ["acceptance", "acceptances"], "see": "advisories.html#gate", "def": "The override of a required advisory for one publish, with a reason."},
  {"id": "advisory", "term": "advisory", "kind": "noun", "domain": "adv", "forms": ["advisory", "advisories"], "see": "advisories.html#model", "def": "The result of a deterministic check that suggests a step. Id <code>CODE/SUBJECT</code>."},
  {"id": "agent", "term": "agent", "kind": "noun", "domain": "agents", "forms": ["agent", "agents"], "see": "agents.html#principles", "def": "A local LLM program, for example Claude Code, that runs a skill and calls repoctl."},
  {"id": "agent-button", "term": "agent button", "kind": "noun", "domain": "agents", "forms": ["agent button", "agent buttons"], "see": "agents.html#agent-buttons", "def": "A web UI button that runs the local agent with one fixed skill and a read-only MCP server."},
  {"id": "agent-panel", "term": "agent panel", "kind": "noun", "domain": "agents", "forms": ["agent panel"], "see": "agents.html#agent-panel", "def": "The web UI drawer that shows the progress and the result of an agent button."},
  {"id": "base", "term": "base", "kind": "noun", "domain": "publish", "forms": ["base"], "see": "publish.html#order", "def": "The snapshot that a publish starts from: <code>latest</code> at the check step.", "not_before": "^\\s+(?:or|skill|repos?)\\b"},
  {"id": "build", "term": "build", "kind": "noun and verb", "domain": "build", "forms": ["build", "builds"], "see": "build.html#run", "def": "To build the nodes with action build in mock. A build makes a run and its staging."},
  {"id": "build-identity", "term": "build identity", "kind": "noun", "domain": "build", "forms": ["build identity"], "see": "model.html#identity", "def": "The package commit plus the target."},
  {"id": "builder-branch", "term": "builder branch", "kind": "noun", "domain": "build", "forms": ["builder branch", "builder branches"], "see": "model.html#set", "def": "The branch <code>builder/&lt;set&gt;</code> that holds the data of one set."},
  {"id": "check", "term": "check", "kind": "noun and verb", "domain": "adv", "forms": ["advisory check", "advisory checks"], "see": "advisories.html#check", "def": "To run all advisory checks of a set. <code>repoctl check</code>."},
  {"id": "clone-lock", "term": "clone lock", "kind": "noun", "domain": "build", "forms": ["clone lock", "clone locks"], "see": "lifecycle.html#clones", "def": "The file lock <code>$XDG_RUNTIME_DIR/repoctl/git-HASH.lock</code> of one bare clone. Each git step on the clone takes it, because all sets of the user share the clone."},
  {"id": "closure", "term": "closure", "kind": "noun", "domain": "adv", "forms": ["closure"], "see": "advisories.html#gate", "def": "The set packages that an advisory concerns."},
  {"id": "deployment-record", "term": "deployment record", "kind": "noun", "domain": "trust", "forms": ["deployment record", "deployment records"], "see": "signing.html#gates", "def": "An entry in <code>keys/deployed.yaml</code>: all clients have a release package with the key, or a key file with the key that config management installed."},
  {"id": "dismiss", "term": "dismiss", "kind": "verb", "domain": "adv", "forms": ["dismiss", "dismisses", "dismissed"], "see": "advisories.html#dismissals", "def": "To hide an info or warn advisory with a committed dismissal."},
  {"id": "dismissal", "term": "dismissal", "kind": "noun", "domain": "adv", "forms": ["dismissal", "dismissals"], "see": "advisories.html#dismissals", "def": "A committed entry that hides an info or warn advisory with the same id and facts."},
  {"id": "exit-code", "term": "exit code", "kind": "noun", "domain": "contract", "forms": ["exit code", "exit codes"], "see": "cli.html#exit", "def": "The exit status of a command. A code can be added but never renumbered."},
  {"id": "gate", "term": "gate", "kind": "noun", "domain": "trust", "forms": ["gate 1", "gate 2", "gate 3", "gates 1 to 3", "gate", "gates"], "see": "signing.html#gates", "def": "Gate 1, 2 and 3: the signer checks before the first signature."},
  {"id": "golden-file", "term": "golden file", "kind": "noun", "domain": "contract", "forms": ["golden file", "golden files"], "see": "delivery.html#tests", "def": "The expected output of one command or one UI fragment in the story state. A golden test compares the output with it."},
  {"id": "guard-hook", "term": "guard hook", "kind": "noun", "domain": "agents", "forms": ["guard hook"], "see": "agents.html#guard", "def": "<code>repoctl skills guard</code>: a hook of Claude Code that checks each Bash, Edit and Write call of an agent in a set directory. It denies push, the shared-state commands and each change of a set input."},
  {"id": "held-node", "term": "held node", "kind": "noun", "domain": "publish", "forms": ["held node", "held nodes"], "see": "publish.html#urgent", "def": "A node that one publish keeps at its build in the base, with <code>--hold NODE --reason TEXT</code>, so that an urgent fix of the target can go out."},
  {"id": "humanaction", "term": "humanAction", "kind": "noun", "domain": "agents", "forms": ["humanAction"], "see": "agents.html#human-action", "def": "The field of an MCP result that names the command that a human must run."},
  {"id": "journal", "term": "journal", "kind": "noun", "domain": "contract", "forms": ["journal", "journals"], "see": "contract.html#journal", "def": "The event file of one run or publish. All surfaces read it."},
  {"id": "key-file", "term": "key file", "kind": "noun", "domain": "trust", "forms": ["key file"], "see": "signing.html#release", "def": "The file in the release package with the public keys of all trusted signers."},
  {"id": "key-state", "term": "key state", "kind": "noun", "domain": "trust", "forms": ["key state", "key states"], "see": "signing.html#lifecycle", "def": "<code>active</code>, <code>retiring</code>, <code>removed</code> or <code>revoked</code>."},
  {"id": "kind", "term": "kind", "kind": "noun", "domain": "contract", "forms": ["kind", "kinds"], "see": "contract.html#kinds", "def": "The type of the elements of <code>items</code> in a <code>--json</code> result.", "not_before": "^\\s+of\\b", "not_after": "\\b(?:upstream|source|two|three|Fold)\\s$"},
  {"id": "latest", "term": "latest", "kind": "noun", "domain": "publish", "forms": ["latest"], "see": "model.html#records", "def": "The GHCR tag of the published snapshot of one set and target. The only authority for what is published.", "not_before": "^\\s+versions?\\b"},
  {"id": "local-branch", "term": "local branch", "kind": "noun", "domain": "build", "forms": ["local branch", "local branches"], "see": "cli.html#local-branch", "def": "The package branch in the repository of a package worktree. <code>repoctl checkout</code> attaches the worktree to it and moves it only forward."},
  {"id": "local-copy", "term": "local copy", "kind": "noun", "domain": "publish", "forms": ["local copy", "local copies"], "see": "model.html#records", "def": "A copy of one snapshot on the admin host. The build input and the base of the next publish."},
  {"id": "lock-state", "term": "lock state", "kind": "noun", "domain": "build", "forms": ["lock state", "lock states"], "see": "lifecycle.html#lock-states", "def": "in-sync, in-tree, ahead, behind, diverged, dirty, missing, unpinned or orphaned."},
  {"id": "match-key", "term": "match key", "kind": "noun", "domain": "build", "forms": ["match key"], "see": "model.html#manifest", "def": "The commit, the target and the source package name of a build. plan finds the build of a node in the RepoManifest by this key."},
  {"id": "mirror-job", "term": "mirror job", "kind": "noun", "domain": "publish", "forms": ["mirror job"], "see": "publish.html#mirror", "def": "A shell script with systemd units on the HTTP server. It pulls <code>latest</code>, verifies it and swaps the served tree."},
  {"id": "mock-lock", "term": "host mock lock", "kind": "noun", "domain": "build", "forms": ["host mock lock"], "see": "build.html#mock", "def": "The file lock <code>/var/lib/mock/.repoctl.lock</code> of the host. A command that runs mock takes it shared; <code>mock update</code> and <code>mock clean</code> take it exclusive."},
  {"id": "node", "term": "node", "kind": "noun", "domain": "build", "forms": ["node", "nodes"], "see": "model.html#nodes", "def": "One package on one target, <code>DIR@TARGET</code>."},
  {"id": "node-state", "term": "node state", "kind": "noun", "domain": "build", "forms": ["node state", "node states"], "see": "model.html#words", "def": "In a run: waiting, queued, running, ok, failed, blocked, canceled. In a plan: current, staged, skip."},
  {"id": "offboard", "term": "offboard", "kind": "verb", "domain": "trust", "forms": ["offboard", "offboarding"], "see": "signing.html#offboarding", "def": "To remove a human's key from signing and, after a release, from the key file."},
  {"id": "onboard", "term": "onboard", "kind": "verb", "domain": "trust", "forms": ["onboard", "onboarding"], "see": "signing.html#onboarding", "def": "To add a human and their key to the key file and the deployment records."},
  {"id": "package", "term": "package", "kind": "noun", "domain": "build", "forms": ["package", "packages"], "see": "model.html#set", "def": "One <code>dir</code> entry in <code>repoctl.yaml</code>.", "not_after": "\\b(?:OCI|GitHub|Go|RPM|EL|EPEL|Fedora|buildroot|distribution|old)\\s$", "not_before": "^\\s+URLs?\\b"},
  {"id": "package-branch", "term": "package branch", "kind": "noun", "domain": "build", "forms": ["package branch", "package branches"], "see": "model.html#set", "def": "The branch <code>&lt;pkg&gt;/&lt;set&gt;[-&lt;variant&gt;]</code> that is the source of one package."},
  {"id": "pin", "term": "pin", "kind": "verb", "domain": "build", "forms": ["pin", "pins", "pinned"], "see": "cli.html#cmd-pin", "def": "To write the commit of a package into <code>repoctl.lock</code>.", "not_after": "\\b(?:scripts|keys|this)\\s$"},
  {"id": "plan", "term": "plan", "kind": "noun and verb", "domain": "build", "forms": ["plan", "plans"], "see": "build.html#plan", "def": "The plan action of each node before a build. To plan is to make this list and build nothing."},
  {"id": "plan-action", "term": "plan action", "kind": "noun", "domain": "build", "forms": ["plan action", "plan actions"], "see": "build.html#actions", "def": "<code>build</code>, <code>current</code>, <code>staged</code> or <code>skip</code>."},
  {"id": "plan-reason", "term": "plan reason", "kind": "noun", "domain": "build", "forms": ["plan reason", "plan reasons"], "see": "build.html#actions", "def": "Why a node builds: <code>commit-changed</code>, <code>rebuild</code>, <code>new-package</code>, <code>new-target</code>, <code>forced</code> or <code>staging-missing</code>."},
  {"id": "problem-code", "term": "problem code", "kind": "noun", "domain": "contract", "forms": ["problem code", "problem codes"], "see": "contract.html#problems", "def": "The dotted id of a problem, for example <code>set.busy</code>, with its exit code and hint."},
  {"id": "provenance", "term": "provenance", "kind": "noun", "domain": "publish", "forms": ["provenance"], "see": "supply-chain.html#provenance", "def": "The SLSA provenance record of one build: an in-toto Statement with the signed RPMs as subjects."},
  {"id": "prune", "term": "prune", "kind": "verb", "domain": "publish", "forms": ["prune", "prunes"], "see": "publish.html#retention", "def": "To make a new snapshot without old NVRs of a package."},
  {"id": "publish", "term": "publish", "kind": "noun and verb", "domain": "publish", "forms": ["publish", "publishes", "published"], "see": "publish.html#order", "def": "To sign the staged nodes, push them as a new snapshot and move <code>latest</code> to it."},
  {"id": "publish-history", "term": "publish history", "kind": "noun", "domain": "publish", "forms": ["publish history"], "see": "model.html#manifest", "def": "The list <code>history[]</code> in the RepoManifest: one entry per publish of the chain, with the human, the signer, the acceptances and the trust changes. gc never deletes it."},
  {"id": "publish-id", "term": "publish id", "kind": "noun", "domain": "publish", "forms": ["publish id", "publish ids"], "see": "publish.html#order", "def": "The id of one publish and the tag of its snapshot: <code>p-YYYYMMDD-HHMM-xxx</code>."},
  {"id": "re-sign", "term": "re-sign", "kind": "verb", "domain": "trust", "forms": ["re-sign", "re-signs"], "see": "signing.html#resign", "def": "To sign the files of a snapshot again with the current signer."},
  {"id": "rebuild", "term": "rebuild", "kind": "noun and verb", "domain": "build", "forms": ["rebuild", "rebuilds"], "see": "model.html#rebuild", "def": "An empty <code>Rebuild: TEXT</code> commit. It gives the node a new build with a new NVR."},
  {"id": "registry", "term": "registry", "kind": "noun", "domain": "publish", "forms": ["registry"], "see": "publish.html#registry", "def": "The OCI registry that holds the snapshots: GHCR."},
  {"id": "release-package", "term": "release package", "kind": "noun", "domain": "trust", "forms": ["release package", "release packages"], "see": "signing.html#release", "def": "The in-tree package with <code>role: release</code> that installs the key file and the <code>.repo</code> file on the clients."},
  {"id": "repomanifest", "term": "RepoManifest", "kind": "noun", "domain": "publish", "forms": ["RepoManifest", "RepoManifests"], "see": "model.html#manifest", "def": "The signed file in the repodata that lists every build of a target repo with its commit."},
  {"id": "restore", "term": "restore", "kind": "verb", "domain": "publish", "forms": ["restore", "restores"], "see": "publish.html#restore", "def": "To move <code>latest</code> back to an earlier snapshot."},
  {"id": "review-commit", "term": "review commit", "kind": "noun", "domain": "trust", "forms": ["review commit", "review commits"], "see": "security.html#review", "def": "An empty, signed set commit with a <code>Trust-Reviewed</code> trailer. It covers a change of a set input that has no valid signature."},
  {"id": "retire", "term": "retire", "kind": "verb", "domain": "trust", "forms": ["retire", "retires", "retired"], "see": "signing.html#lifecycle", "def": "To stop signing with a key that the clients still trust. A package retires from a target the same way."},
  {"id": "revoke", "term": "revoke", "kind": "verb", "domain": "trust", "forms": ["revoke", "revokes"], "see": "signing.html#compromise", "def": "To end the trust in a compromised key at once: the key gets state <code>revoked</code> and leaves the key file before the re-sign."},
  {"id": "run", "term": "run", "kind": "noun", "domain": "build", "forms": ["run", "runs"], "see": "build.html#run", "def": "One execution of <code>repoctl build</code>, id <code>r-YYYYMMDD-HHMM-xxx</code>.", "only_after": "(?i)\\b(?:the|a|an|its|this|each|every|one|same|own|story|build|publish|unpublished|running|finished|failed|canceled|abandoned|current|newest|oldest|last|earlier|foreign|second|source|of|per|no|active|later|next|new|first)\\s$"},
  {"id": "run-state", "term": "run state", "kind": "noun", "domain": "build", "forms": ["run state", "run states"], "see": "model.html#words", "def": "planning, running, ok, failed, canceled or abandoned."},
  {"id": "safety-class", "term": "safety class", "kind": "noun", "domain": "contract", "forms": ["safety class", "safety classes"], "see": "model.html#safety", "def": "read, staging, local write or shared state."},
  {"id": "sbom", "term": "SBOM", "kind": "noun", "domain": "publish", "forms": ["SBOM", "SBOMs"], "see": "supply-chain.html#sbom-record", "def": "Software bill of materials: the components, versions, licenses and hashes of a build or a repository, in CycloneDX 1.6."},
  {"id": "selector", "term": "selector", "kind": "noun", "domain": "build", "forms": ["selector", "selectors"], "see": "cli.html#selectors", "def": "A command argument that selects nodes: a package, <code>DIR@TARGET</code> or a group."},
  {"id": "served-tree", "term": "served tree", "kind": "noun", "domain": "publish", "forms": ["served tree"], "see": "publish.html#mirror", "def": "The directory on the HTTP server that the clients read. Only the mirror job writes it."},
  {"id": "set", "term": "set", "kind": "noun", "domain": "build", "forms": ["set", "sets"], "see": "model.html#set", "def": "A group of packages that one <code>repoctl.yaml</code> describes. Also: package set.", "only_after": "(?i)\\b(?:the|a|an|this|that|each|every|one|new|old|its|their|all|per|of|package|own|slurm|story|foreign|first|previous|same|which|wrong|known|other|any|no|in)\\s$", "not_before": "^\\s+(?:up|of|to|the)\\b"},
  {"id": "set-directory", "term": "set directory", "kind": "noun", "domain": "build", "forms": ["set directory"], "see": "model.html#set", "def": "The git work tree of a builder branch, with the package worktrees below <code>packages/</code>."},
  {"id": "set-input", "term": "set input", "kind": "noun", "domain": "trust", "forms": ["set input", "set inputs"], "see": "security.html#set-inputs", "def": "A file of the builder branch that decides what a publish builds or which keys the clients trust: <code>repoctl.yaml</code>, <code>repoctl.lock</code>, the macros files, the in-tree packages and <code>keys/</code>. Each change needs a signed commit."},
  {"id": "set-lock", "term": "set lock", "kind": "noun", "domain": "build", "forms": ["set lock"], "see": "config.html#lock", "def": "The file lock that allows one writer per set for one user on one host."},
  {"id": "signer", "term": "signer", "kind": "noun", "domain": "trust", "forms": ["signer", "signers"], "see": "signing.html#probe", "def": "The key that signs a publish, selected by <code>sign.key</code> in the user config."},
  {"id": "skill", "term": "skill", "kind": "noun", "domain": "agents", "forms": ["skill", "skills"], "see": "agents.html#skills", "def": "Instructions for an agent that ship with repoctl."},
  {"id": "snapshot", "term": "snapshot", "kind": "noun", "domain": "publish", "forms": ["snapshot", "snapshots"], "see": "model.html#snapshots", "def": "One OCI artifact with all files of one set and target."},
  {"id": "sources-file", "term": "sources file", "kind": "noun", "domain": "build", "forms": ["sources file", "sources files"], "see": "build.html#sources", "def": "The file <code>sources</code> in a package branch. It gives the SHA-512 of each source archive that is not in git."},
  {"id": "staged-node", "term": "staged node", "kind": "noun", "domain": "build", "forms": ["staged node", "staged nodes"], "see": "build.html#staging", "def": "A node built for the current lock in an unpublished run from the lock of this user. A run from worktree never stages a node."},
  {"id": "staged-view", "term": "staged view", "kind": "noun", "domain": "build", "forms": ["staged view", "staged views"], "see": "build.html#staging", "def": "A repository per target in the work directory of a run. It links the RPMs of the staged nodes of the plan, and mock reads it."},
  {"id": "staging", "term": "staging", "kind": "noun", "domain": "build", "forms": ["staging"], "see": "build.html#staging", "def": "The RPMs that a run built and that are not published."},
  {"id": "store", "term": "store", "kind": "noun", "domain": "publish", "forms": ["store"], "see": "config.html#set-config", "def": "The registry path of a set, <code>store.oci</code>.", "only_after": "\\b(?:the|its)\\s$", "not_after": "\\bcredential\\s$"},
  {"id": "story-fixture", "term": "story fixture", "kind": "noun", "domain": "contract", "forms": ["story fixture"], "see": "delivery.html#tests", "def": "The data story as test data: the set <code>slurm-26.05</code> with small specs, and the recorded tool output of the story."},
  {"id": "surface", "term": "surface", "kind": "noun", "domain": "contract", "forms": ["surface", "surfaces"], "see": "model.html#surfaces", "def": "One of the four ways to use the core: CLI, web UI, MCP server, skills."},
  {"id": "target", "term": "target", "kind": "noun", "domain": "build", "forms": ["target", "targets"], "see": "model.html#targets", "def": "One build platform, one mock chroot with one architecture: <code>el9</code>, <code>el10</code>."},
  {"id": "trust-set", "term": "trust set", "kind": "noun", "domain": "trust", "forms": ["trust set"], "see": "signing.html#verify-el", "def": "The keys that a signature check uses."},
  {"id": "verify-root", "term": "verify root", "kind": "noun", "domain": "build", "forms": ["verify root", "verify roots"], "see": "build.html#mock", "def": "A mock root per target with the client rpm and dnf. The publish step verify-el runs in it."}
 ]
};
