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

## Install

repoctl comes as signed RPMs from its RPM repo on GitHub Pages
([Tool releases](docs/design/tool.html)). The first install on a host:

1. Download `https://dennisklein.github.io/repoctl/rpm/repoctl-release-1-1.noarch.rpm`.
2. Compare its sha256 with the value in the notes of the newest
   [GitHub Release](https://github.com/dennisklein/repoctl/releases).
3. `sudo dnf install ./repoctl-release-1-1.noarch.rpm`
4. Compare the fingerprint of `gpg --show-keys /etc/pki/rpm-gpg/RPM-GPG-KEY-repoctl`
   with the tool repository key of the release notes.
5. On an admin host (Fedora): `sudo dnf install repoctl`, then
   `repoctl doctor --only host`. On the HTTP server: `sudo dnf install repoctl-mirror`.

The fingerprint and the sha256 come here with the first release.

License: [Apache-2.0](LICENSE), for the code and the design record on this
branch. The fonts and the vendored script in `docs/assets/` keep their own
licenses ([details](docs/design/decision.html#license)).

## Design record

Read the design record at **<https://dennisklein.github.io/repoctl/>**.
The source is in [`docs/`](docs/index.html):

| Part | Chapters |
|---|---|
| I Foundations | [Why a Go tool](docs/design/decision.html) · [Core model](docs/design/model.html) · [Configuration](docs/design/config.html) |
| II Build and publish | [Build](docs/design/build.html) · [Advisories](docs/design/advisories.html) · [Publish and snapshots](docs/design/publish.html) |
| III Trust and security | [Signing and trust](docs/design/signing.html) · [Security model](docs/design/security.html) · [Supply chain records](docs/design/supply-chain.html) · [Tool releases](docs/design/tool.html) |
| IV Interfaces | [CLI reference](docs/design/cli.html) · [Terminal output](docs/design/terminal.html) · [Machine contract](docs/design/contract.html) · [Agents](docs/design/agents.html) · [Web UI](docs/design/ui.html) · [UI: overview, build, runs](docs/design/ui-build.html) · [UI: packages, publish, signing](docs/design/ui-publish.html) · [Mockup gallery](docs/design/mockups/index.html) |
| V Operations | [Sets and packages](docs/design/lifecycle.html) · [Import runbook](docs/runbooks/import.html) · [Key compromise runbook](docs/runbooks/key-compromise.html) · [Bad publish runbook](docs/runbooks/bad-publish.html) · [Registry loss runbook](docs/runbooks/registry-loss.html) · [Mirror job runbook](docs/runbooks/mirror-job.html) · [Host loss runbook](docs/runbooks/host-loss.html) |
| VI Delivery | [Delivery and verification](docs/design/delivery.html) |

The JSON Schemas and examples of the machine contract are in
[`docs/design/contract/`](docs/design/contract/).

## Branches in this repository

| Branch | Content |
|---|---|
| `main` | repoctl and its design record (this branch) |
| `set/slurm-<version>[-testing]` | one package set |
| `<project>/slurm-<version>[-<variant>]` | one package: spec file and its history |

The branches do not share history. Today the set branches hold the
build scripts of the bash era. Their old name is `builder/<set>`. The [import runbook](docs/runbooks/import.html)
moves a set to `repoctl.yaml` and `repoctl.lock`.

## Build and test repoctl

repoctl needs Go 1.26. The tests also need `rpmbuild`, `createrepo_c`,
`modifyrepo_c`, `rpmsign` and `gpg` (on Ubuntu: `apt-get install rpm
createrepo-c gnupg`). The tests never call the real mock: a fake mock in
`internal/testkit/fakemock` builds with `rpmbuild` on the host.

```sh
make build          # bin/repoctl
make test           # unit tests
make e2e            # end-to-end tests on the story fixture (build tag e2e)
make lint           # golangci-lint
make generate       # the contract types from docs/design/contract/schema
```

| Path | Content |
|---|---|
| `cmd/repoctl` | the binary |
| `internal/api` | the machine contract: types generated from the JSON Schemas, problem codes, exit codes |
| `internal/cli` | the command tree, the help topics and the terminal output |
| `internal/config`, `internal/lock`, `internal/workspace` | repoctl.yaml, repoctl.lock, the user config, worktrees and lock states |
| `internal/mock`, `internal/manifest`, `internal/tools` | the mock adapter, the RepoManifest, child processes and git |
| `internal/srpm`, `internal/graph`, `internal/plan` | sources, SRPMs, the graph per target and the plan |
| `internal/build`, `internal/journal`, `internal/staging` | build runs, the event journal and the staging |
| `internal/publish`, `internal/snapshot`, `internal/localcopy`, `internal/records` | publish, snapshots in the OCI registry, the local copy of latest, provenance and SBOM |
| `internal/sign`, `internal/release` | keys, gates, the deployment record and the release package |
| `internal/advisory`, `internal/upstream`, `internal/doctor` | advisories, upstream versions and host checks |
| `internal/mcp`, `internal/skills`, `internal/ui` | the MCP server, the agent skills and the web UI |
| `internal/testkit` | the story fixture, the fake mock and the contract validator |
| `contrib/mirror` | the mirror job of the HTTP server (shell, curl, jq, gpgv, rpmkeys) and its systemd units |
| `packaging` | the spec files, the RPM repo on GitHub Pages and the scripts of the release job ([README](packaging/README)) |

## Build the docs site

The site generator needs only Python 3 (standard library).

```sh
python3 tools/site/build.py --strict      # writes _site/
python3 -m http.server -d _site 8000      # open http://localhost:8000/
```

`--strict` fails on a broken link, a missing anchor, a page that is not in
`tools/site/nav.json` and an external resource.

The [docs workflow](.github/workflows/docs.yml) builds the site for each pull
request. A push to `main` also deploys it to GitHub Pages. In the repository
settings, *Pages → Source* must be *GitHub Actions*.

### Screenshots

The screenshots in `docs/design/img/` come from the mockups. Make them again
after a change to a mockup:

```sh
node tools/site/shots.mjs                 # all pages
node tools/site/shots.mjs graph run-detail
```

This needs Node with Playwright and Chromium, and Python 3 with Pillow (for
WebP).

### Write a page

A page in `docs/` is an HTML fragment: one `<h1>`, then the content. The
generator adds the frame (header, chapter list, "On this page", search,
previous and next). The chapter list and the fixed anchors are in
`tools/site/nav.json`. Use the classes of the authoring contract
(`CONTRACT_CLASSES` in `tools/site/build.py`), for example:

- `p.lede` for the first paragraph, `ul.keypoints` for the key points;
- `aside.callout` with `note`, `rule`, `why`, `warn` or `tip`;
- `pre.term` for terminal output, `pre > code.language-*` for code;
- `span.tag` with `read`, `staging`, `local` or `shared` for the safety class;
- `figure.diagram` for an inline SVG diagram (classes `box`, `edge`, `t` and
  their variants; markers `#arrow`, `#arrow-accent`, `#arrow-muted`);
- `figure.shot` for a screenshot of a mockup.

Do not use inline styles or colours. The site CSS gives light and dark mode.
Write in ASD-STE100 (Simplified Technical English).

### Glossary

The project words are in `docs/assets/glossary-data.js`: the word, its forms,
its domain and its definition. The build makes `design/glossary.html` from it.
In each section of a chapter, it links the first use of each word to its entry
(dotted underline, a hover card with the domain icon and colour). Headings,
code, links, definitions (`<dfn>`) and table heads stay plain. Put text in
`<span class="no-gl">` to keep it plain.

The web UI mockups have the same links. Make them again after a change to the
glossary or to a mockup, then make the screenshots again:

```sh
python3 tools/site/glossary.py mockups
```

The other projects that the text names (a tool such as `brig` or `mock`, a
repository such as `GSI-HPC/go-clikit`) are in `tools/site/projects.json`. The
build links the first use of each one in a section to its GitHub repository.

To see each link of a word in context, build the site, then:

```sh
python3 tools/site/glossary.py report             # links per term and repo
python3 tools/site/glossary.py report set-lock sigstore/cosign
```

### Skills for agents

`.claude/skills/` has the skills for work on the docs: `docs-steward` (the
checklist for each change), `docs-glossary` (project words and project links)
and `docs-mockups` (mockups and screenshots).
