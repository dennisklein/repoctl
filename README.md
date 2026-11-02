# repoctl

`repoctl` builds signed RPM repositories from a package set: a git directory
with `repoctl.yaml` and `repoctl.lock`. It calls `git`, `mock` (with the
rpmautospec plugin), `rpmsign` and `createrepo_c`. It has a CLI, a local web
UI and an MCP server for agents. It is built on
[GSI-HPC/go-clikit](https://github.com/GSI-HPC/go-clikit).

Status: design. No code yet.

## Design

The design record is in [`docs/design/`](docs/design/index.html):

1. [Tooling decision](docs/design/01-tooling-decision.html)
2. [repoctl core: config, XDG, targets, skills](docs/design/02-repoctl.html)
3. [Build state, rebuilds, snapshots, LLM in the UI](docs/design/03-build-state.html)
4. [CLI, contract and web UI](docs/design/04-cli-ui.html), with [mockups](docs/design/mockups/index.html)

Review of the design from six user journeys: [UX review](docs/review/ux-review.html).

## Branches in this repository

| Branch | Content |
|---|---|
| `main` | repoctl (this branch) |
| `builder/slurm-<version>[-testing]` | one package set: `packages.lock`, build scripts, release package |
| `<pkg>/slurm-<version>[-<variant>]` | one package: spec file and its history |

The branches do not share history.
