---
name: docs-glossary
description: Use to add, change or remove a project word in the glossary of the design record (docs/assets/glossary-data.js), or a tool or repository name that links to GitHub (tools/site/projects.json), and to fix a word that links where it has an other meaning.
---

# Glossary and project links

The build links two kinds of words in the chapters, each one only at its first
use in a section (the section starts at each `h2`, `h3` and `h4`):

| Kind | Data | Link | Look |
|---|---|---|---|
| Project word | `docs/assets/glossary-data.js` | `design/glossary.html#<id>` | dotted underline, hover card with domain icon and colour |
| Other project | `tools/site/projects.json` | `https://github.com/<owner>/<name>` | normal link |

The code is `tools/site/glossary.py`. `tools/site/build.py` uses it for the
chapters, and `python3 tools/site/glossary.py mockups` uses it for the web UI
mockups (project words only).

## Add or change a project word

1. Find the section that explains the word. The glossary entry points to it.
2. Add one line to `terms` in `docs/assets/glossary-data.js`, in alphabetical
   order. The part after `=` is strict JSON.

   ```js
   {"id": "set-lock", "term": "set lock", "kind": "noun", "domain": "build",
    "forms": ["set lock"], "see": "config.html#lock",
    "def": "The file lock that allows one writer per set for one user on one host."},
   ```

   - `id`: the anchor on the glossary page. Do not change an existing id: other
     pages can link to it.
   - `domain`: one key of `domains` (build, publish, trust, adv, agents,
     contract). It gives the icon and the colour. Do not add a domain without a
     change of the site CSS, the mockup CSS (`--d-*` tokens) and the colour
     table in `docs/design/ui.html`.
   - `forms`: the exact words, case-sensitive. A capital first letter also
     matches. Give the plural and the other forms that the text uses. The
     longest form wins, and two terms cannot share a form.
   - `see`: a chapter path relative to `docs/design/` with an anchor that is
     in `tools/site/nav.json` or is an `id` on that page.
   - `def`: one or two sentences in ASD-STE100. Inline `<code>` only.
3. When the word is also a common English word (run, set, check, base, store,
   kind, package), add a rule, else it links in the wrong places:
   - `only_after`: a regex on the text before the form; the form links only
     when it matches (for example a determiner: `\b(?:a|the|each|this)\s$`).
   - `not_after`: a regex on the text before; no link when it matches.
   - `not_before`: a regex on the text after; no link when it matches (for
     example `^\s+(?:up|of|to)\b` for the verb "set up").
   Or limit the forms (for example only `advisory check`, not `check`).
4. Build and look at each link of the word in context:

   ```sh
   python3 tools/site/build.py --strict --out _site
   python3 tools/site/glossary.py report set-lock
   ```

   Each line must use the glossary meaning. Fix a wrong link with a rule, or
   with `<span class="no-gl">` around that one word in the page.
5. Link the mockups again and make their screenshots again (skill
   `docs-mockups`):

   ```sh
   python3 tools/site/glossary.py mockups
   ```

6. Look at `design/glossary.html`: the entry, its domain, the filter (type a
   part of the definition) and the link `#<id>`.

Remove a word only when no page uses it. Search for its forms first, and keep
the chapter text in the same change.

## Add a tool or a repository

1. Add one line to `projects` in `tools/site/projects.json`:

   ```json
   {"repo": "rpm-software-management/mock", "forms": ["mock"]}
   ```

   - `repo`: `owner/name` on GitHub. It is always a form: in prose, and as the
     whole text of a `<code>` element (`<code>GSI-HPC/go-clikit</code>`).
   - `forms`: the tool names in prose, case-sensitive. A tool name in `<code>`
     stays plain (it is a command or a value).
   - The rules `only_after`, `not_after`, `not_before` work as for the
     glossary. Use them when the name is also a step, a group or a word (for
     example `mock` in "the group mock", `rpm` as the build step).
2. Make sure that the URL is correct: open `https://github.com/<repo>`.
3. Build and check: `python3 tools/site/glossary.py report <owner/name>`.

A tool without a GitHub repository (for example GnuPG) stays plain. Do not add
a mirror.
