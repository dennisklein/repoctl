# repoctl

`repoctl` builds signed RPM repositories from a package set: a git directory
with `repoctl.yaml` and `repoctl.lock`. It calls `git`, `mock` (with the
rpmautospec plugin), `rpmsign` and `createrepo_c`. It has a CLI, a local web
UI and an MCP server for agents. It is built on
[GSI-HPC/go-clikit](https://github.com/GSI-HPC/go-clikit).

Status: first implementation of the design (phases 1 to 7 of the
[delivery plan](docs/design/delivery.html)), not released yet. The release
job is ready; the first release needs the tool repository key
([packaging/README](packaging/README)).

## Documentation

Read the docs at **<https://dennisklein.github.io/repoctl/>**.

- [User manual](https://dennisklein.github.io/repoctl/manual/): install,
  concepts, a tutorial that starts a new set, the CLI and the web UI.
- [Design record](https://dennisklein.github.io/repoctl/design/): why repoctl
  works as it does, the decisions, the mockups and the machine contract.

The source is in [`docs/`](docs/index.html). To build the docs or the code, read
[CONTRIBUTING](CONTRIBUTING.md).

## Install

repoctl comes as signed RPMs from its RPM repo on GitHub Pages. Check the key
of the repo by hand at the first install. The steps, with the checks, are in
[Install repoctl](https://dennisklein.github.io/repoctl/manual/install.html).

```sh
sudo dnf install ./repoctl-release-1-1.noarch.rpm   # after the checks
sudo dnf install repoctl
repoctl doctor --only host
```

The fingerprint of the tool repository key:

```
F4EBF057DF65EF8D26B1E44128A5286144DD6792
```

The sha256 of the release RPM comes here with the first release.

## Branches in this repository

| Branch | Content |
|---|---|
| `main` | repoctl and its docs (this branch) |
| `set/slurm-<version>[-testing]` | one package set |
| `<project>/slurm-<version>[-<variant>]` | one package: spec file and its history |

The branches do not share history. Today the set branches hold the build
scripts of the bash era (old name `builder/<set>`). The
[import runbook](docs/runbooks/import.html) moves a set to `repoctl.yaml` and
`repoctl.lock`.

## License

[Apache-2.0](LICENSE), for the code and the design record on this branch. The
fonts and the vendored script in `docs/assets/` keep their own licenses
([details](docs/design/decision.html#license)).
