#!/usr/bin/env python3
"""The glossary of the design record: load it, link terms in HTML.

Usage: python3 tools/site/glossary.py mockups [--check]
       python3 tools/site/glossary.py report [--site DIR] [KEY ...]

The data is docs/assets/glossary-data.js. tools/site/build.py imports this
module: it makes design/glossary.html and links the first use of each term in
each section of a chapter. It also links the first use of the other projects in
tools/site/projects.json (a tool name such as brig, or a repo such as
GSI-HPC/go-clikit) to the project on GitHub. The command "mockups" does the same for the web UI
mockups in docs/design/mockups/ui/ and writes the files in place (--check only
tells which files change). The rules are in the profiles DOCS and MOCKUPS.
The command "report" reads a built site (default _site/) and gives the number
of links per term id or repo; for each KEY it also gives each link with the
text around it. Use it to find a word that links where it has an other meaning.
Python 3 standard library only.
"""

import argparse
import html
import json
import posixpath
import re
import sys
from html.parser import HTMLParser
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
DATA = REPO / "docs" / "assets" / "glossary-data.js"
PROJECTS = HERE / "projects.json"
PAGE = "design/glossary.html"          # site path of the glossary page

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "source", "track", "wbr"}


def word_regex(forms):
    """A word of its own: not part of a name, a path, an id or a hyphenated word."""
    alts = "|".join(re.escape(f) for f in sorted(forms, key=len, reverse=True))
    return re.compile(r"(?<![\w@./$-])(?:" + alts + r")(?![\w@/-]|\.[\w])")


def compile_rules(entry):
    return {k: re.compile(entry[k]) for k in ("only_after", "not_after", "not_before") if k in entry}


def rules_ok(r, text, start, end):
    before, after = text[:start], text[end:]
    if "only_after" in r and not r["only_after"].search(before):
        return False
    if "not_after" in r and r["not_after"].search(before):
        return False
    if "not_before" in r and r["not_before"].search(after):
        return False
    return True


class Glossary:
    def __init__(self, path=DATA):
        text = Path(path).read_text(encoding="utf-8")
        m = re.search(r"=\s*(\{.*\})\s*;\s*$", text, re.S)
        if not m:
            raise ValueError(f"{path}: no 'window.REPOCTL_GLOSSARY = {{...}};'")
        data = json.loads(m.group(1))
        self.domains = data["domains"]
        self.terms = data["terms"]
        self.by_id = {t["id"]: t for t in self.terms}
        forms = {}
        for t in self.terms:
            if t["domain"] not in self.domains:
                raise ValueError(f"glossary: term {t['term']!r} has unknown domain {t['domain']!r}")
            for f in t["forms"]:
                for v in {f, f[:1].upper() + f[1:]}:
                    if v in forms and forms[v] != t["id"]:
                        raise ValueError(f"glossary: form {v!r} is in {forms[v]!r} and {t['id']!r}")
                    forms[v] = t["id"]
        self.forms = forms
        self.rules = {}
        for t in self.terms:
            r = compile_rules(t)
            if r:
                self.rules[t["id"]] = r
        self.regex = word_regex(forms)

    def find(self, text):
        for m in self.regex.finditer(text):
            tid = self.forms[m.group(0)]
            r = self.rules.get(tid)
            if r and not rules_ok(r, text, m.start(), m.end()):
                continue
            yield m.start(), m.end(), tid


class Projects:
    """Other projects: a form (a tool name, or owner/name) -> the repo on GitHub."""

    def __init__(self, path=PROJECTS):
        data = json.loads(Path(path).read_text(encoding="utf-8"))
        self.forms, self.rules = {}, {}
        for p in data["projects"]:
            repo = p["repo"]
            if not re.fullmatch(r"[\w.-]+/[\w.-]+", repo):
                raise ValueError(f"projects: bad repo {repo!r}")
            for f in [repo] + p["forms"]:
                if f in self.forms:
                    raise ValueError(f"projects: form {f!r} is in {self.forms[f]!r} and {repo!r}")
                self.forms[f] = repo
            r = compile_rules(p)
            if r:
                self.rules[repo] = r
        self.regex = word_regex(self.forms)

    @staticmethod
    def url(repo):
        return "https://github.com/" + repo

    def find(self, text):
        for m in self.regex.finditer(text):
            repo = self.forms[m.group(0)]
            r = self.rules.get(repo)
            if r and not rules_ok(r, text, m.start(), m.end()):
                continue
            yield m.start(), m.end(), repo


class Profile:
    """Where terms link: inside an element of `link_in`, never inside an element
    of `never_in` (a tag or a class); the first use per block of `reset_on`."""

    def __init__(self, link_in, never_in, reset_on):
        self.link_in = set(link_in)
        self.never_in = set(never_in)
        self.reset_on = set(reset_on)

    @staticmethod
    def hit(names, tag, classes):
        return tag in names or any("." + c in names for c in classes)


DOCS = Profile(
    link_in=["p", "li", "td", "dd", "figcaption"],
    never_in=["a", "code", "pre", "kbd", "samp", "var", "dfn", "abbr", "h1", "h2", "h3", "h4", "h5", "h6",
              "svg", "script", "style", "template", "button", "summary", "label", "th", "dt", "title",
              "nav", "header", "footer", "select", "option", "textarea", "details",
              ".no-gl", ".callout-title", ".tag", ".eyebrow", ".synopsis", ".toc-inline", ".shot"],
    reset_on=["h2", "h3", "h4"],
)

MOCKUPS = Profile(
    link_in=["p", ".s", ".help-pop", ".hint", ".foot", ".note", ".lead", ".desc", "dd"],
    never_in=["a", "code", "pre", "kbd", "samp", "var", "h1", "h2", "h3", "h4", "svg", "script", "style",
              "template", "button", "summary", "label", "th", "dt", "title", "select", "option", "textarea",
              "input", "nav", "header", "table", "time",
              ".no-gl", ".btn", ".pill", ".mono", ".code-sub", ".kind", ".node-chip", ".sidebar", ".topbar",
              ".cli-pop", ".more-pop", ".set-pop", ".keys", ".errline", ".log", ".count", ".qs", ".tag",
              ".sr-only", ".path", ".id", ".stepper"],
    reset_on=["main", "section", "dialog", "aside", ".card", "h2", "h3"],
)


class Linker(HTMLParser):
    """Copy HTML and wrap the first use of each term per block in <a class="gl">,
    and the first use of each other project in <a href="https://github.com/...">."""

    def __init__(self, glossary, profile, href, projects=None):
        super().__init__(convert_charrefs=False)
        self.g = glossary
        self.p = profile
        self.href = href            # id -> URL
        self.out = []
        self.stack = []             # (tag, link_in, never_in)
        self.seen = set()
        self.used = []              # ids in order of first link on the page
        self.projects = projects
        self.seen_repo = set()
        self.code_at = None         # index in out of a <code> that can be a repo link

    def _state(self):
        ok = any(e[1] for e in self.stack)
        bad = any(e[2] for e in self.stack)
        return ok and not bad

    def handle_starttag(self, tag, attrs):
        self._start(tag, attrs, tag in VOID)

    def handle_startendtag(self, tag, attrs):
        self._start(tag, attrs, True)

    def _start(self, tag, attrs, closed):
        raw = self.get_starttag_text()
        self.out.append(raw)
        if tag == "svg" and closed:
            return
        classes = (dict(attrs).get("class") or "").split()
        if Profile.hit(self.p.reset_on, tag, classes):
            self.seen = set()
            self.seen_repo = set()
        if tag == "code" and not closed and self.projects and self.stack and self._state():
            self.code_at = len(self.out) - 1
        if not closed:
            self.stack.append((tag, Profile.hit(self.p.link_in, tag, classes),
                               Profile.hit(self.p.never_in, tag, classes)))

    def handle_endtag(self, tag):
        if tag == "code" and self.code_at is not None:
            i, self.code_at = self.code_at, None
            inner = "".join(self.out[i + 1:])
            repo = self.projects.forms.get(inner) if "/" in inner else None   # owner/name only
            if repo and repo not in self.seen_repo:
                self.seen_repo.add(repo)
                self.out.insert(i, f'<a href="{Projects.url(repo)}">')
                self.out.append("</code></a>")
                self.stack_pop(tag)
                return
        self.out.append(f"</{tag}>")
        self.stack_pop(tag)

    def stack_pop(self, tag):
        for i in range(len(self.stack) - 1, -1, -1):
            if self.stack[i][0] == tag:
                del self.stack[i:]
                return

    def handle_data(self, data):
        if not self.stack or not self._state():
            self.out.append(data)
            return
        hits = [(s, e, "gl", tid) for s, e, tid in self.g.find(data)]
        if self.projects:
            hits += [(s, e, "repo", repo) for s, e, repo in self.projects.find(data)]
        hits.sort(key=lambda h: (h[0], h[0] - h[1]))
        pos = 0
        for start, end, kind, key in hits:
            if start < pos:
                continue
            if kind == "gl":
                if key in self.seen:
                    continue
                self.seen.add(key)
                if key not in self.used:
                    self.used.append(key)
                t = self.g.by_id[key]
                a = (f'<a class="gl" href="{html.escape(self.href(key))}" data-gl="{key}" '
                     f'data-domain="{t["domain"]}">')
            else:
                if key in self.seen_repo:
                    continue
                self.seen_repo.add(key)
                a = f'<a href="{Projects.url(key)}">'
            self.out.append(data[pos:start])
            self.out.append(f'{a}{data[start:end]}</a>')
            pos = end
        self.out.append(data[pos:])

    def handle_entityref(self, name):
        self.out.append(f"&{name};")

    def handle_charref(self, name):
        self.out.append(f"&#{name};")

    def handle_comment(self, data):
        self.out.append(f"<!--{data}-->")

    def handle_decl(self, decl):
        self.out.append(f"<!{decl}>")

    def unknown_decl(self, data):
        self.out.append(f"<![{data}]>")

    def handle_pi(self, data):
        self.out.append(f"<?{data}>")


def link(text, glossary, profile, href, projects=None):
    """Return (html, ids): the text with links, and the linked term ids."""
    lk = Linker(glossary, profile, href, projects)
    lk.feed(text)
    lk.close()
    return "".join(lk.out), lk.used


GL_LINK = re.compile(r'<a class="gl" href="[^"]*" data-gl="[^"]*" data-domain="[^"]*">([^<]*)</a>')
SCRIPTS = ('<script src="../../../assets/glossary-data.js"></script>\n'
           '<script src="../../../assets/glossary.js"></script>\n')


def link_mockup(text, glossary, page):
    """Link one mockup page again. The page loads the data and the hover card."""
    text = GL_LINK.sub(r"\1", text)
    text = text.replace(SCRIPTS, "")
    head, sep, rest = text.partition("<body")
    if not sep:
        return text
    href = lambda tid: posixpath.relpath(PAGE, posixpath.dirname(page)) + "#" + tid
    body, ids = link(sep + rest, glossary, MOCKUPS, href)
    if ids:
        i = body.rfind("</body>")
        body = body[:i] + SCRIPTS + body[i:]
    return head + body


REPORT_LINK = re.compile(r'<a (?:class="gl" [^>]*data-gl="([^"]+)"|href="https://github\.com/([^"]+)")[^>]*>(.*?)</a>', re.S)


def plain(text):
    return re.sub(r"\s+", " ", html.unescape(re.sub(r"<[^>]+>", "", text)))


def report(site, keys):
    projects = set(Projects().forms.values()) if PROJECTS.is_file() else set()
    count, ctx = {}, {}
    for f in sorted(Path(site).rglob("*.html")):
        rel = f.relative_to(site).as_posix()
        if rel.startswith("design/mockups/"):
            continue
        text = f.read_text(encoding="utf-8")
        for m in REPORT_LINK.finditer(text):
            key = m.group(1) or m.group(2)
            if m.group(2) and key not in projects:
                continue            # a link in the source, not one that the build made
            count[key] = count.get(key, 0) + 1
            if key in keys:
                before = plain(text[max(0, m.start() - 300):m.start()])[-45:]
                after = plain(text[m.end():m.end() + 300])[:45]
                ctx.setdefault(key, []).append(f"  {rel}: ...{before}[{plain(m.group(3))}]{after}...")
    for key, n in sorted(count.items(), key=lambda kv: (-kv[1], kv[0])):
        print(f"{n:5}  {key}")
    for key in keys:
        print(f"\n== {key}: {count.get(key, 0)} links")
        print("\n".join(ctx.get(key, [])))
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description="Link glossary terms and other projects.")
    ap.add_argument("what", choices=["mockups", "report"])
    ap.add_argument("keys", nargs="*", help="report: term ids or repos to show in context")
    ap.add_argument("--check", action="store_true", help="mockups: only tell which files change")
    ap.add_argument("--site", default=str(REPO / "_site"), help="report: the built site (default _site/)")
    args = ap.parse_args(argv)
    if args.what == "report":
        return report(args.site, args.keys)
    g = Glossary()
    changed = 0
    for f in sorted((REPO / "docs" / "design" / "mockups" / "ui").glob("*.html")):
        page = f.relative_to(REPO / "docs").as_posix()
        old = f.read_text(encoding="utf-8")
        new = link_mockup(old, g, page)
        if new != old:
            changed += 1
            print(("would change " if args.check else "changed ") + page)
            if not args.check:
                f.write_text(new, encoding="utf-8")
    print(f"{changed} files {'to change' if args.check else 'changed'}")
    return 1 if (args.check and changed) else 0


if __name__ == "__main__":
    sys.exit(main())
