# repoctl

`repoctl` builds signed RPM repositories from a package set: a git directory
with `repoctl.yaml` and `repoctl.lock`. It calls `git`, `mock` (with the
rpmautospec plugin), `rpmsign` and `createrepo_c`. It has a CLI, a local web
UI and an MCP server for agents. It is built on
[GSI-HPC/go-clikit](https://github.com/GSI-HPC/go-clikit).

Status: design. No code yet.

## Design record

Read the design record at **<https://dennisklein.github.io/repoctl/>**.
The source is in [`docs/`](docs/index.html):

| Part | Chapters |
|---|---|
| I Foundations | [Why a Go tool](docs/design/decision.html) · [Core model](docs/design/model.html) · [Configuration](docs/design/config.html) |
| II How it works | [Build](docs/design/build.html) · [Advisories](docs/design/advisories.html) · [Publish and snapshots](docs/design/publish.html) · [Signing and trust](docs/design/signing.html) · [Supply chain records](docs/design/supply-chain.html) · [Sets and packages](docs/design/lifecycle.html) |
| III Interfaces | [CLI reference](docs/design/cli.html) · [Terminal output](docs/design/terminal.html) · [Machine contract](docs/design/contract.html) · [Agents](docs/design/agents.html) · [Web UI](docs/design/ui.html) · [UI: overview, build, runs](docs/design/ui-build.html) · [UI: packages, publish, signing](docs/design/ui-publish.html) · [Mockup gallery](docs/design/mockups/index.html) |
| IV Operations | [Import runbook](docs/runbooks/import.html) · [Delivery and verification](docs/design/delivery.html) |

The JSON Schemas and examples of the machine contract are in
[`docs/design/contract/`](docs/design/contract/).

## Branches in this repository

| Branch | Content |
|---|---|
| `main` | repoctl and its design record (this branch) |
| `builder/slurm-<version>[-testing]` | one package set |
| `<pkg>/slurm-<version>[-<variant>]` | one package: spec file and its history |

The branches do not share history. Today the builder branches hold the
build scripts of the bash era. The [import runbook](docs/runbooks/import.html)
moves a set to `repoctl.yaml` and `repoctl.lock`.

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
