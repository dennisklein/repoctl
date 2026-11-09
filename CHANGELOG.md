# Changelog

This file lists the changes of each release of repoctl that an admin sees.
The release job copies the section of a release into the notes of its
[GitHub Release](https://github.com/dennisklein/repoctl/releases). To use
repoctl, read the [user manual](https://dennisklein.github.io/repoctl/manual/).

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The versions follow [Semantic Versioning](https://semver.org/). This file
starts after 0.1.0, the first release. Read the section "Upgrade" of a
release before you upgrade.

## [Unreleased]

### Added

- `sign.key` can be a user ID of gpg, for example your mail address, when
  exactly one secret key in gpg matches it. Before, it had to be an ID of
  `sign.keys` or a full fingerprint.

### Changed

- `repoctl doctor --only registry` checks the login and the token of ghcr.io
  also outside a set directory. Before, it gave no check there.
- The hints of the registry login show
  `podman login ghcr.io --authfile ~/.config/containers/auth.json`. podman
  comes with repoctl; oras is not necessary
  ([Registry login](https://dennisklein.github.io/repoctl/manual/install.html#registry-login)).

### Fixed

- The hint of the doctor check `sign.key` names the user config file and the
  values that you can set
  ([Signing key](https://dennisklein.github.io/repoctl/manual/install.html#signing-key)).
- Import of a bash-era repo
  ([runbook](https://dennisklein.github.io/repoctl/manual/runbooks/import.html)):
  - `repoctl sign deployed --path` finds the release package in the signed
    tree. Before, it stopped with `sign.not-deployed`, because the tree has
    no RepoManifest.
  - `repoctl repo push --from` takes only the RPMs that the RepoManifest
    lists. Before, it also took old RPMs and the link `slurm-release.rpm`,
    and the push stopped.
  - The RepoManifest of an import can have `commit: null`
    (`adopted-unmapped`), `package: null` (a retired name) and null build
    facts, as the design tells. Before, the schema refused them.

## [0.1.1] - 2026-11-09

### Changed

- The notes of a GitHub Release are shorter. They list the changes and the
  check values, and link to the manual for the install and the upgrade.
- `repoctl help team` and the hint of the doctor check `tool.repo-key` link to
  [First install](https://dennisklein.github.io/repoctl/manual/install.html#first-install)
  in the manual.
- The runbooks are now part IV of the
  [user manual](https://dennisklein.github.io/repoctl/manual/), at
  `manual/runbooks/`. The old URLs redirect to the new pages.

[Unreleased]: https://github.com/dennisklein/repoctl/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/dennisklein/repoctl/compare/v0.1.0...v0.1.1
