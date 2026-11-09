# repoctl

`repoctl` builds signed RPM repositories from a package set: a git directory
with `repoctl.yaml` and `repoctl.lock`. It calls `git`, `mock` (with the
rpmautospec plugin), `rpmsign` and `createrepo_c`. It has a CLI, a local web
UI and an MCP server for agents. It is built on
[GSI-HPC/go-clikit](https://github.com/GSI-HPC/go-clikit).

Status: first implementation of the design (phases 0 to 7 of the
[delivery plan](https://dennisklein.github.io/repoctl/design/delivery.html#phases)). The
[releases](https://github.com/dennisklein/repoctl/releases) have the RPMs; the
[changelog](CHANGELOG.md) lists the changes of each release.

## Start here

Read the **[user manual](https://dennisklein.github.io/repoctl/manual/)**. Read its pages in this order:

1. [Install repoctl](https://dennisklein.github.io/repoctl/manual/install.html): the first install, the host
   setup and the upgrade.
2. [Concepts](https://dennisklein.github.io/repoctl/manual/concepts.html): set, target, node, lock, plan, run,
   publish and key.
3. [Tutorial: start a new set](https://dennisklein.github.io/repoctl/manual/tutorial.html).

Then look up a command in the [CLI reference](https://dennisklein.github.io/repoctl/manual/cli.html) or
`repoctl help`. The [design record](https://dennisklein.github.io/repoctl/design/) tells why repoctl works as it
does. To change the code or the docs, read [CONTRIBUTING](CONTRIBUTING.md).

## Install

repoctl comes as signed RPMs from its RPM repo on GitHub Pages. Check the key
of the repo by hand at the first install. The steps, with the checks, are in
[First install](https://dennisklein.github.io/repoctl/manual/install.html#first-install). In short:

```sh
sudo dnf install ./repoctl-release-1-1.noarch.rpm   # after the checks
sudo dnf install repoctl
repoctl doctor --only host
```

The fingerprint of the tool repository key:

```
F4EBF057DF65EF8D26B1E44128A5286144DD6792
```

The sha256 of `repoctl-release-1-1.noarch.rpm`:

```
ad8eebea41eef412fd15cb7cf09e04a49c17622bd6dcaae0d2fd855e38c7e054
```

## Branches in this repository

| Branch | Content |
|---|---|
| `main` | repoctl and its docs (this branch) |
| `set/slurm-<version>[-testing]` | one package set |
| `<project>/slurm-<version>[-<variant>]` | one package: spec file and its history |

The branches do not share history. Today the set branches hold the build
scripts of the bash era (old name `builder/<set>`). The
[import runbook](https://dennisklein.github.io/repoctl/runbooks/import.html) moves a set to `repoctl.yaml` and
`repoctl.lock`.

## License

[Apache-2.0](LICENSE), for the code and the design record on this branch. The
fonts and the vendored script in `docs/assets/` keep their own licenses
([details](https://dennisklein.github.io/repoctl/design/decision.html#license)).
