# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

RPM package builder for Slurm and HPC components at GSI. Builds signed RPM packages for Rocky Linux 9 (EL9) with EPEL dependencies using Mock for isolated, reproducible builds.

## Build Commands

```bash
# Initial setup (install dependencies, configure GPG)
./scripts/setup

# Check out the pinned package commits into packages/<dir>
./scripts/checkout

# Build all packages in dependency order
./scripts/build

# To build specific packages, comment out unwanted builds in scripts/build
# Build order: munge → ucx → pmix → pmix_v5 → pmix_v4 → prrte → rocm-smi → slurm → slurm-singularity-exec → openmpi → slurm-release

# List packages with dependencies and build requirements
./scripts/list

# Query built repository contents
./scripts/rpmlist result/el9/slurm/
./scripts/rpmlist --format json result/el9/slurm/

# Clean build artifacts
./scripts/clean

# Update mock environment
./scripts/update
```

## Repository Structure

- **packages.lock**: Pinned package commits, one line per package: `dir branch sha`
- **packages/**: Git worktrees with package sources and spec files, created by `scripts/checkout`
  - `ucx`, `pmix`, `pmix_v4`, `prrte`, `munge`, `rocm-smi`, `slurm`, `slurm-singularity-exec`, `openmpi`
- **packages/slurm-release/**: Package sources and spec files for the repo pkg that can be used to configure yum/dnf on a target system pointing to the rpm repo
- **scripts/**: Build tooling
  - `build`: Main build script with `build()` function for each package
  - `checkout`: Create or update the package worktrees from `packages.lock`
  - `pin`: Write the HEAD of the package worktrees into `packages.lock`
  - `common.sh`: Shared config (mock target, paths, macros)
  - `macros`: RPM macros passed to mock
  - `rpmlist`: Python tool to query repo contents (requires `python3-humanize python3-rich`)
- **build/el9/**: Temporary build artifacts (`SOURCES/`, `SRPMS/`, `RPMS/`)
- **result/el9/**: Final signed repositories
  - `slurm/`: Binary RPMs
  - `slurm-source/`: Source RPMs
  - `slurm-debuginfo/`: Debug packages

## Build Configuration

- **Mock target**: `rocky+epel-9-x86_64` (set in `scripts/common.sh`)
- **GPG signing**: Requires `%_gpg_name` in `~/.rpmmacros` pointing to your GPG key

## Working with Packages

Each package has its own branch `<project>/slurm-<version>[-<variant>]` with its own
history. The spec file is at the root of the branch, so rpmautospec reads the
package history unchanged.

```bash
# Modify a package
cd packages/<dir>
git switch <pkg>/slurm-<version>
# make changes, commit, push the package branch
cd ../..
./scripts/pin <dir>
git add packages.lock && git commit

# Add new package (new orphan branch for the package)
git worktree add --orphan -b <pkg>/slurm-<version> packages/<dir>
# add the spec file, commit, push, then add a line to packages.lock
# Add "build <dir>" line to scripts/build in dependency order
```

## Troubleshooting

```bash
# Check build logs
less build/el9/<package>/sources.log

# Debug in mock shell
mock -r rocky+epel-9-x86_64 --shell

# Clean corrupted mock cache
mock -r rocky+epel-9-x86_64 --clean

# Verify GPG setup
rpm --eval "%{_gpg_name}"
gpg --list-secret-keys
```

## Git Branching

Branch names: `<component>/slurm-<version>[-<variant>]`

- `set/slurm-<version>[-testing]`: build scripts and `packages.lock`
- `<project>/slurm-<version>[-<variant>]`: one package, e.g. `slurm/slurm-26.05`, `pmix/slurm-26.05-v5`
- Package branches do not share history with the set branch or with other packages
