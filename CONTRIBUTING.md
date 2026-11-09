# Contributing to repoctl

This file is for people who change the code or the docs. To use repoctl, read the
[user manual](https://dennisklein.github.io/repoctl/manual/).

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

## Screenshots

The screenshots in `docs/design/img/` come from the mockups. Make them again
after a change to a mockup:

```sh
node tools/site/shots.mjs                 # all pages
node tools/site/shots.mjs graph run-detail
```

This needs Node with Playwright and Chromium, and Python 3 with Pillow (for
WebP).

## Write a page

A page in `docs/` is an HTML fragment: one `<h1>`, then the content. The
generator adds the frame (header, chapter list, "On this page", search,
previous and next). The docs have two books: the manual (`docs/manual/`) and
the design record (`docs/design/`). The page lists of both books and the fixed
anchors are in `tools/site/nav.json`. Use the classes of the authoring contract
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

## Glossary

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

## Skills for agents

`.claude/skills/` has the skills for work on the docs: `docs-steward` (the
checklist for each change), `docs-glossary` (project words and project links)
and `docs-mockups` (mockups and screenshots).
