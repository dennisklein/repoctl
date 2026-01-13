# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

RPM package builder for Slurm and HPC components at GSI. Builds signed RPM packages for Rocky Linux 9 (EL9) with EPEL dependencies using Mock for isolated, reproducible builds.

## Build Commands

```bash
# Initial setup (install dependencies, configure GPG)
./scripts/setup

# Build all packages in dependency order
./scripts/build

# To build specific packages, comment out unwanted builds in scripts/build
# Build order: ucx → pmix → pmix_v4 → prrte → munge → rocm-smi → slurm → slurm-singularity-exec → openmpi → slurm-release

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

- **packages/**: Git submodules with package sources and spec files
  - `ucx`, `pmix`, `pmix_v4`, `prrte`, `munge`, `rocm-smi`, `slurm`, `slurm-singularity-exec`, `openmpi`
- **packages/slurm-release/**: Package sources and spec files for the repo pkg that can be used to configure yum/dnf on a target system pointing to the rpm repo
- **scripts/**: Build tooling
  - `build`: Main build script with `build()` function for each package
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

```bash
# Modify a package
cd packages/<package>
# make changes, commit within submodule
cd ../..
git add packages/<package> && git commit

# Add new package
git submodule add <repo-url> packages/<name>
# Add "build <name>" line to scripts/build in dependency order
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

- **Main branch**: `virgo-3-el8` (base for PRs)
- Each major Slurm version has its own branch (e.g., `slurm-25.05`, `slurm-25.11`)
