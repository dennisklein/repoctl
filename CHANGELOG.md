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

### Fixed

- The hint of the doctor check `sign.key` names the user config file and the
  values that you can set
  ([Signing key](https://dennisklein.github.io/repoctl/manual/install.html#signing-key)).

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
