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
1. Built-in `REPO_MAP` in the script (covers every package; needed wherever the
   spec `URL:` is a project homepage rather than a GitHub repo)
2. The `URL:` field in the package's spec file

### Parameters

- `package` - Package name (must match directory in `packages/`)
- `version-filter` - Optional regex to filter tags (overrides default)

Default filters are applied automatically:
- `openmpi`: `^v[0-9]` (skips vendor forks such as `v5.0.9amzn1`)
- `pmix`: `^v6` (PMIx 6.x series)
- `pmix_v4`: `^v4` (PMIx 4.x series only — frozen upstream at 4.2.9)
- `pmix_v5`: `^v5` (PMIx 5.x series)
- `rocm-smi`: `^therock-[0-9]` (TheRock stream, which the spec follows via
  `%{pkg_src}`; the parallel production `rocm-X.Y.Z` stream is frozen at 7.2.4)
- `slurm`: `^slurm-` (official Slurm tags)

Note: the series-pinned filters deliberately hide newer major series. When
upstream opens a new series that this repo should follow (e.g. PMIx v7), the
filter must be widened or the package will silently report no update.

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
