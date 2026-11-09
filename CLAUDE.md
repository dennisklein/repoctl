# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

RPM package set `slurm-26.05` for Slurm and HPC components at GSI. repoctl builds
the set with mock for Rocky Linux 9 (EL9) with EPEL and publishes signed repos.
Manual: https://dennisklein.github.io/repoctl/manual/

## Commands

```bash
repoctl status            # the set, the packages and the published state
repoctl checkout          # make the package worktrees from repoctl.lock
repoctl plan              # what to build, and why
repoctl build             # build the nodes of the plan in graph order
repoctl check             # advisories that need no build
repoctl publish           # sign the staged nodes, push a snapshot, move "latest"
repoctl doctor            # check the host: mock, gpg, registry login
```

Read `repoctl help packages` before you bump, add, remove or rename a package.
Read `repoctl help new-set` before you start the next set from this one.
Read `repoctl help safety` for the rules for agents: do not sign, publish or
push to the registry without the human.

## Repository Structure

- **repoctl.yaml**: the set: repo, store, signing keys, target `el9`
  (`rocky+epel-9-x86_64`), packages and their upstreams
- **repoctl.lock**: the pinned commit of each package branch (`repoctl pin` writes it)
- **rpm/macros**: RPM macros for mock
- **packages/**: git worktrees of the package branches (`repoctl checkout` makes them)
- **packages/slurm-release/**: the release package (in this branch); it holds
  the repo file and the key file `RPM-GPG-KEY-slurm`

The bash-era files `packages.lock`, `scripts/` and the skills `package-info`
and `list-package-updates` stay until the first publish with repoctl. Then
they are removed. Do not use them for new work.

## Git Branching

Branch names: `<component>/slurm-<version>[-<variant>]`

- `set/slurm-<version>[-testing]`: `repoctl.yaml`, `repoctl.lock` and the release package
- `<project>/slurm-<version>[-<variant>]`: one package, e.g. `slurm/slurm-26.05`, `pmix/slurm-26.05-v5`
- Package branches do not share history with the set branch or with other packages
