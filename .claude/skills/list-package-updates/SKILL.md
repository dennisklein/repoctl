---
name: list-package-updates
description: List packages with current and latest upstream versions.
allowed-tools:
  - "Bash(./scripts/list)"
  - "WebSearch"
  - "WebFetch"
  - "Skill"
---

# List Package Updates

## Arguments

Optional: package names to filter (e.g., `/list-package-updates ucx slurm`)

## Workflow

**CRITICAL: Follow these exact steps. Do not deviate or invent custom commands.**

### 1. Get current versions

Run `./scripts/list` and extract package names and versions.

### 2. Research packages in parallel

Spawn one Task agent per package (MUST USE THE SAME MODEL AS TOP-LEVEL SKILL AGENT).

Each agent MUST use the `/package-info` skill to get release information:

```
/package-info <package>
```

The skill returns JSON with:
- `upstream_url` - URL from spec file
- `github_tag` - Latest stable GitHub release tag
- `github_date` - Release date
- `fedora_version` - Version in Fedora rawhide

Use WebSearch and/or WebFetch to read the Release Notes and Changelogs.

### 3. Output table

| Package | Current | Upstream | Released | Fedora | URL |

Notes below table: anomalies, EOL warnings, major version jumps.

For each package summarize the Release Notes / Changelogs - include everything from the current to the upstream version.
