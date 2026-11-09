---
name: release-notes
description: Use to write or check the changelog of repoctl (CHANGELOG.md) from the commits, to prepare a release (the section of the version, the tag), or to preview the notes of a GitHub Release. Keeps the entries short, in ASD-STE100, and linked to the user manual.
---

# Release notes and changelog

`CHANGELOG.md` is the one source of the changes. The release job copies the
section of the tag into the notes of the GitHub Release. Do not write the
notes of a GitHub Release by hand, and do not edit a published release:
correct `CHANGELOG.md` and let the next release carry the correction.

```
commits on main ──(this skill)──▶ CHANGELOG.md  "## [Unreleased]"
                                       │  rename at release time
                                       ▼
tag vX.Y.Z ──▶ release.yml ──▶ packaging/release-notes.sh ──▶ GitHub Release notes
               build: "check" stops a tag without a section
               sign:  "notes" = changes + compare link + manual links + check values
```

## 1. Know the parts

| Part | File | Rule |
|---|---|---|
| Changelog | `CHANGELOG.md` | [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Newest section first. |
| Notes script | `packaging/release-notes.sh` | `check TAG`, `changes TAG`, `notes TAG RPM KEYS SIGNER`. |
| Release job | `.github/workflows/release.yml` | Runs `check` in the job build and `notes` in the job sign. |
| Install and upgrade steps | `docs/manual/install.html` | The notes link to them. Do not copy the steps into the changelog. |
| Release steps | `packaging/README` ("Each release") | The tag, the approval, the sha256 of a new release package. |

## 2. Find the commits

```sh
prev=$(git tag --list 'v[0-9]*' --sort=-v:refname | grep -v -- - | head -n 1)
git log --no-merges --format='%h %s%n%b' "$prev"..origin/main
```

Read the subject and the body of each commit. Read the diff when the
subject does not tell what an admin sees.

## 3. Sort each commit

| Commit | Section | Note |
|---|---|---|
| A change that an admin must act on: a new key, a new `apiVersion`, a new `minRepoctl`, a renamed flag or file | `### Upgrade` | Always first. The upgrade steps of the manual tell the admin to read it. |
| `feat` | `### Added` | A new command, flag, page, check or package. |
| `fix` | `### Fixed` | Write what was wrong for the admin, not the code fix. |
| `feat`/`refactor`/`perf`/`docs` that changes behaviour, output or a URL | `### Changed` | |
| A deprecated command, flag or field | `### Deprecated` | Name the replacement. |
| A removed command, flag, field, platform | `### Removed` | Also add an `### Upgrade` entry. |
| A fix for a vulnerability; a dependency update for a CVE | `### Security` | Name the CVE. |
| `ci`, `test`, `build(deps)` of the workflows, `style`, `chore` | none | Skip. |
| `build(deps)` of a Go module | none | Only when it fixes a bug or a CVE that an admin sees. |
| `docs` | none | Only when a page moves, or a new page or runbook comes. |

Several commits of one change make one entry. One commit can make two
entries (for example `### Removed` and `### Upgrade`).

## 4. Write the entries

- Write in ASD-STE100: short sentences, active voice, present tense, one
  idea in one sentence, at most 25 words in a sentence. Say "admin" or
  "human", never "user" or "people".
- Write what the admin sees or must do, not what the code does. Start with
  the command, the flag, the page or the file: "`repoctl plan` shows ...".
- One entry is one bullet, at most two sentences. No commit hashes, no PR
  numbers, no names of authors. The notes add the compare link.
- Put commands, flags, files and problem codes in backticks.
- Link the manual page that tells more, with its full URL
  (`https://dennisklein.github.io/repoctl/manual/...`): the same text goes
  to GitHub, where a relative link breaks. Link the design record only when
  the manual has no page for it.
- Use the words of the glossary (`docs/assets/glossary-data.js`).
- Use only the sections of step 3, in that order: Upgrade, Added, Changed,
  Deprecated, Removed, Fixed, Security. Leave out an empty section.

Example:

```markdown
### Upgrade

- Raise `minRepoctl` to `0.3` in `repoctl.yaml` when all admins have 0.3.
  The steps are in [Upgrade](https://dennisklein.github.io/repoctl/manual/install.html#upgrade).

### Added

- `repoctl plan --why NODE` tells why a node builds.
```

## 5. Add the entries

- Between releases, add each entry to `## [Unreleased]`. The best time is in
  the pull request of the change: a `feat` or `fix` pull request adds its
  entry in the same commit.
- Before a release, run step 2 to find commits without an entry. Add them.

## 6. Prepare a release

1. Pick the version (Semantic Versioning; before 1.0.0, a change in
   `### Upgrade` or `### Removed` raises the minor version, else the patch
   version).
2. Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`. Add a new empty
   `## [Unreleased]` above it.
3. Update the link references at the end of the file:

   ```markdown
   [Unreleased]: https://github.com/dennisklein/repoctl/compare/vX.Y.Z...HEAD
   [X.Y.Z]: https://github.com/dennisklein/repoctl/compare/vPREV...vX.Y.Z
   ```

4. Check and preview:

   ```sh
   packaging/release-notes.sh check vX.Y.Z
   packaging/release-notes.sh changes vX.Y.Z
   ```

   For the full notes without the release job, use any file as the RPM and a
   keys file from `go run ./packaging/rpmtree keys`:

   ```sh
   go run ./packaging/rpmtree keys >/tmp/keys
   packaging/release-notes.sh notes vX.Y.Z packaging/repoctl.repo /tmp/keys FPR
   ```

5. Commit as `docs(changelog): release X.Y.Z` in a pull request. A human
   merges it, then tags on main (`packaging/README`, "Each release"). An
   agent does not tag and does not push a tag.

A pre-release tag (`vX.Y.Z-rc.N`) takes the section `[X.Y.Z]` when it is
there, else `[Unreleased]`. It needs no rename.

## 7. Commit

- Conventional Commits. The changelog entry goes into the commit of its
  change. A release commit is `docs(changelog): release X.Y.Z`.
- No session links, no temporal context in the entries.
