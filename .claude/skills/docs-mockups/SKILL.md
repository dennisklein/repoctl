---
name: docs-mockups
description: Use to change the web UI or terminal mockups in docs/design/mockups/, their CSS or JS, or the screenshots in docs/design/img/ that the chapters show. Keeps the sidebar, the domain colours, the glossary links and the screenshots in step with the design record.
---

# Mockups and screenshots

The mockups are complete HTML pages. The build copies them verbatim (it does not
wrap them) and only checks their links. The chapters `ui.html`, `ui-build.html`
and `ui-publish.html` describe them and show screenshots of them.

| Part | Path |
|---|---|
| Web UI pages | `docs/design/mockups/ui/*.html` |
| Terminal frames | `docs/design/mockups/term/*.html` |
| Shared CSS and JS | `docs/design/mockups/assets/` (`repoctl.css` tokens, `shell.css` frame and sidebar, `shell.js` keys and pop-ups) |
| Gallery | `docs/design/mockups/index.html` |
| Screenshots | `docs/design/img/ui-*.webp`, `term-*.webp` |
| Screenshot tool | `tools/site/shots.mjs` |

## Rules

- Each web UI page has its own copy of the sidebar. A change to the sidebar
  goes into all pages in `ui/`. Compare them after the change, for example
  with `grep -c 'nav-item' docs/design/mockups/ui/*.html`.
- The sidebar uses the glossary domains: a group label has `data-domain` and
  the domain icon (`svg.nav-dom`), an entry has `data-domain` and an icon. The
  active entry (`aria-current="page"`) gets the tint and the left bar from
  `shell.css`. Overview and the Host entries stay neutral. The domain icons are
  the `icon` paths of `domains` in `docs/assets/glossary-data.js`, in a 16 px
  viewBox with `class="dom"`.
- Colours come only from the tokens in `repoctl.css` (`--d-build`,
  `--d-publish`, `--d-trust`, `--d-adv`, `--d-agents`, `--d-contract` and the
  state tokens), in light and in both dark blocks. A domain colour marks a
  domain, never a state. A new token goes into all three blocks and into the
  colour table of `docs/design/ui.html`.
- Use the project words of the glossary in the UI text. Do not write glossary
  links by hand: the tool writes them (see below). Text in `.no-gl` stays plain.
  Buttons, tables, the sidebar, the top bar, the stepper, code, ids and paths
  never get links (profile `MOCKUPS` in `tools/site/glossary.py`).
- A page that the chapters show must be in the list `UI` (or `MOBILE`, `TERM`)
  of `tools/site/shots.mjs`, and the chapter has a `figure.shot` with an `alt`
  text and a `figcaption` that tell what the screenshot shows.
- When the UI changes, change the text of the chapter in the same change: the
  chapter is the design, the mockup shows it.

## After a change

```sh
python3 tools/site/glossary.py mockups          # glossary links and scripts
node tools/site/shots.mjs <page> ...            # or no name: all pages
python3 tools/site/build.py --strict --out _site
```

- `glossary.py mockups` is idempotent: run it after each change to a page or
  to the glossary. `--check` must then say `0 files to change`.
- `shots.mjs` needs Node with Playwright and Chromium, and Python 3 with
  Pillow. It takes each web UI page at 1440x900 CSS px and some at 390x844,
  at device scale 2. Make the shots of each page that you changed, and of all
  pages after a change to the shared CSS, to the sidebar or to the glossary.
- Look at each new screenshot before you commit it: no hover card open, no
  focus ring that is not part of the design, the same data as the text.
- Open one page in a browser and check: light and dark mode, the hover card on
  a dotted word, the keys of `shell.js` (`g` plus a letter, `/`, `?`), and a
  phone width for the pages with a mobile shot.
- Commit the changed pages, the screenshots and the chapter text together.
