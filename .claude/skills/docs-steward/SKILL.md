---
name: docs-steward
description: Use for any change to the design record in docs/ or to the site generator in tools/site/ (a new page, a new section, an edit of text, a diagram, CSS or JS). Gives the checklist that keeps the pages, the glossary, the project links, the mockups and the screenshots consistent.
---

# Docs steward

The design record is a static site. `tools/site/build.py` (Python 3, standard
library only) wraps the HTML fragments in `docs/` with `tools/site/template.html`
and writes `_site/`. The docs workflow (`.github/workflows/docs.yml`) builds it
for each pull request and deploys `main` to GitHub Pages.

Use this checklist for each change. Do the steps that apply, and tell in the
pull request which steps you did.

## 1. Know the parts

| Part | File | Rule |
|---|---|---|
| Page | `docs/**/*.html` | An HTML fragment: one `<h1>`, then the content. No frame, no inline style, no colour. |
| Chapter list, anchors | `tools/site/nav.json` | Each page is in `parts[].pages[]`. Each `id` that other pages or the "On this page" list need is in `anchors`. |
| Authoring classes | `CONTRACT_CLASSES` in `tools/site/build.py` | Use only these classes. A new class goes into this set and into `docs/assets/site.css`. |
| Glossary | `docs/assets/glossary-data.js` | The one source of the project words. Use the skill `docs-glossary`. |
| Other projects | `tools/site/projects.json` | A tool name or `owner/name` that links to GitHub. Use the skill `docs-glossary`. |
| Web UI mockups | `docs/design/mockups/ui/*.html` | Verbatim pages, not wrapped. Use the skill `docs-mockups`. |
| Screenshots | `docs/design/img/*.webp` | Made from the mockups with `tools/site/shots.mjs`. Never edit by hand. |
| Site CSS and JS | `docs/assets/` | The build adds `?v=<hash>` to their URLs. Do not add a version by hand. |

## 2. Write the text

- Write in ASD-STE100 (Simplified Technical English): short sentences, one
  instruction per sentence, active voice, approved words. Use one word for one
  concept: the word of the glossary.
- Say "human" (plural "humans") for a user of repoctl, never "person" or
  "people". The contract uses the same word (`humanAction`, `human: true`).
- When you use a project concept (set, node, run, publish, advisory, signer
  and so on), use the exact glossary word. The build links its first use in each
  section. Do not write the link or the dotted underline by hand.
- A new concept that a reader must know: add it to the glossary in the same
  change (skill `docs-glossary`).
- An other tool or git repository by name (`brig`, `mock`,
  `GSI-HPC/go-clikit`): the build links its first use in each section to
  `https://github.com/<owner>/<name>`. A name that is not in
  `tools/site/projects.json` stays plain: add it.
- Do not write a link by hand to a word that the build links. Write a link by
  hand only for a specific page (a release, an issue, a file).
- Put text in `<span class="no-gl">` when a word must stay plain (for example
  the English verb "run" where the build wrongly links the glossary term).
- Headings, code, links, `<dfn>`, `<th>`, `<dt>` and `.callout-title` never get
  links. Thus a definition list or a table head stays plain.
- The authoring classes are in the README ("Write a page"): `p.lede`,
  `ul.keypoints`, `aside.callout` (`note`, `rule`, `why`, `warn`, `tip`),
  `pre.term`, `pre > code.language-*`, `span.tag`, `figure.diagram`,
  `figure.shot`.

## 3. Diagrams

- Use `figure.diagram` with inline SVG and the diagram classes (`box`, `edge`,
  `t` and their variants; markers `#arrow`, `#arrow-accent`, `#arrow-muted`).
- No colour values in the SVG. The classes give light and dark mode.
- Add a `<figcaption>`. The build links glossary words in the caption, not in
  the SVG text.

## 4. Build and check

```sh
python3 tools/site/build.py --strict --out _site
python3 tools/site/glossary.py mockups --check
python3 tools/site/glossary.py report            # links per term and repo
```

- `--strict` must say `OK`: no broken link, no missing anchor, no page outside
  `nav.json`, no external resource, no class outside the contract.
- `mockups --check` must say `0 files to change`. If not, use the skill
  `docs-mockups`.
- After a change of the text, look at `report KEY` for each new or changed
  glossary word and for each project name in the changed sections. Each link
  must have the glossary meaning.

## 5. Look at the result

Open the changed pages in a browser (`python3 -m http.server -d _site 8000`) or
take screenshots with Playwright. Look at:

- light and dark mode (`prefers-color-scheme` and the theme button);
- a phone width (390 CSS px): no horizontal scroll, the tables scroll in their
  box;
- the hover card on two or three term links (domain icon, colour, definition);
- the glossary page `design/glossary.html` when the glossary changed: the
  filter, the domain buttons, a link to `#<id>`;
- print preview when you changed the layout.

## 6. Mockups and screenshots

A change to a mockup, to `docs/design/mockups/assets/` or to the glossary can
change the screenshots. Use the skill `docs-mockups`. Commit the new `.webp`
files in the same pull request.

## 7. Commit

- Conventional Commits (`docs(design): ...`, `feat(site): ...`,
  `fix(site): ...`), with a short body of bullet points.
- One logical change per commit: for example the generator change, the glossary
  data and the page text in separate commits when each one is complete.
- The generated mockup links and the screenshots go into the commit of the
  change that caused them.
