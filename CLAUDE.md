# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is an RPM package builder repository for Slurm and related HPC components at GSI. It builds RPM packages for Rocky Linux 9 (EL9) with EPEL dependencies. The repository contains git submodules for various HPC packages that are built into signed RPM repositories.

## Key Components

### Package Structure
- **packages/**: Git submodules containing package sources and spec files for:
  - `slurm`: Main Slurm workload manager (version 25.05.2)
  - `openmpi`: OpenMPI implementation
  - `pmix` & `pmix_v4`: Process Management Interface for Exascale
  - `prrte`: PMIx Reference RunTime Environment
  - `ucx`: Unified Communication X library
  - `munge`: Authentication service
  - `rocm-smi`: AMD ROCm System Management Interface
  - `slurm-release`: Repository configuration package
  - `fi-slurm-utils`: Rust-based CLI utilities for Slurm (fi-nodes, fi-slurm-limits)

### Build System
- **scripts/build**: Main build script that processes all packages in sequence
- **scripts/common.sh**: Shared configuration and variables
- **scripts/macros**: RPM macros for build customization
- **scripts/rpmlist**: Python utility to list RPM repository contents

### Directory Layout
- **build/el9/**: Build artifacts and temporary files
- **result/el9/**: Final RPM repositories (binary, source, debuginfo)

## Common Commands

### Initial Setup
```bash
# Install dependencies and configure GPG signing
./scripts/setup

# Configure GPG key for RPM signing (if not already done)
echo "%_gpg_name your.email@gsi.de" >> ~/.rpmmacros
```

### Building Packages
```bash
# Build all packages in dependency order
./scripts/build

# Build a specific package (edit scripts/build to uncomment desired package)
# Current build order: ucx → pmix → pmix_v4 → prrte → munge → rocm-smi → slurm → slurm-singularity-exec → openmpi → slurm-release → fi-slurm-utils
vim scripts/build  # Uncomment the desired "build <package>" line
./scripts/build
```

### Repository Management
```bash
# List all available packages with dependencies
./scripts/list

# Query built RPM repository contents
./scripts/rpmlist result/el9/slurm/
./scripts/rpmlist --format json result/el9/slurm/

# Clean all build artifacts
./scripts/clean

# Update mock environment
./scripts/update
```

### Verification
```bash
# Check build status and disk usage
tree -h build/ result/
du -hs build/ result/

# Verify RPM signatures
rpm -K result/el9/slurm/packages/*.rpm

# Check repository metadata signature
gpg --verify result/el9/slurm/repodata/repomd.xml.asc
```

## Build Configuration

- **Target**: `rocky+epel-9-x86_64` (defined in `scripts/common.sh`)
- **Mock root**: Configurable via `$mockroot` variable
- **Build directories**:
  - Sources: `build/el9/<package>/SOURCES/`
  - SRPMs: `build/el9/<package>/SRPMS/`
  - RPMs: `build/el9/<package>/RPMS/`
- **Repository structure**:
  - Binary: `result/el9/slurm/`
  - Source: `result/el9/slurm-source/`
  - Debug: `result/el9/slurm-debuginfo/`

## Development Workflow

### Adding New Packages
1. Add as git submodule: `git submodule add <repo-url> packages/<name>`
2. Ensure package has proper `.spec` file in `packages/<name>/`
3. Add build line to `scripts/build` in correct dependency order
4. Test build: `./scripts/build`
5. Commit submodule reference: `git add packages/<name> && git commit`

### Modifying Existing Packages
1. Navigate to submodule: `cd packages/<package>`
2. Make changes and commit within submodule
3. Return to main repo: `cd ../..`
4. Update submodule reference: `git add packages/<package> && git commit`
5. Rebuild: `./scripts/build`

### Working with Git Branches
- **Current branch**: `slurm-25.05` (for Slurm 25.05.x releases)
- **Main branch**: `virgo-3-el8` (used as base for pull requests)
- Each major Slurm version typically has its own branch

## Troubleshooting

### GPG Signing Issues
```bash
# Check configured GPG key
rpm --eval "%{_gpg_name}"

# List available keys
gpg --list-secret-keys

# If missing, generate a new key
gpg --gen-key
```

### Build Failures
```bash
# Check build logs
less build/el9/<package>/sources.log

# Mock shell for debugging
mock -r rocky+epel-9-x86_64 --shell

# Clean mock cache if corrupted
mock -r rocky+epel-9-x86_64 --clean
```

### Missing Dependencies
- Check EPEL is enabled in mock config
- Verify all submodules are initialized: `git submodule update --init`
- Ensure packages are built in correct order (dependency chain)

## Architecture Notes

This is a specialized RPM build system designed for HPC environments at GSI. Key features:

- **Mock-based builds**: Ensures reproducible, isolated build environments
- **Automatic GPG signing**: All RPMs and repository metadata are cryptographically signed
- **Dependency management**: Handles complex chains between Slurm and supporting libraries
- **Multi-repository output**: Separates binary, source, and debug packages
- **Git submodules**: Each package maintained in separate repository for version control

The build process uses Mock with RPM macros to maintain ABI compatibility and follow enterprise Linux packaging conventions. The `slurm-release` package provides repository configuration for client systems.