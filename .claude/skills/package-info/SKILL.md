---
name: package-info
description: Get release information for a package (upstream URL, GitHub tag, Fedora version).
allowed-tools:
  - "Bash(.claude/skills/package-info/scripts/get-package-info*)"
---

# Package Info

Get release information for a single package.

## Arguments

Required: `<package> [version-filter]`

Example: `/package-info ucx`

## Usage

```bash
.claude/skills/package-info/scripts/get-package-info <package> [version-filter]
```

The GitHub owner/repo is derived automatically from:
1. Built-in mapping for special cases (pmix_v4, slurm, rocm-smi, slurm-singularity-exec)
2. The `URL:` field in the package's spec file

### Parameters

- `package` - Package name (must match directory in `packages/`)
- `version-filter` - Optional regex to filter tags (overrides default)

Default filters are applied automatically:
- `pmix_v4`: `^v4` (PMIx v4 series only)
- `slurm`: `^slurm-` (official Slurm tags)

### Output (JSON)

```json
{
  "package": "ucx",
  "upstream_url": "https://github.com/openucx/ucx",
  "github_tag": "v1.18.0",
  "github_date": "2024-12-01T12:00:00Z",
  "fedora_version": "1.18.0"
}
```
