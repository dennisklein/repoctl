#!/usr/bin/env python3
"""Check the text image of design record pages.

Reports, per page:
  SENT     a sentence above 25 words (code words count, but a sentence that
           is 40 % or more code is exempt: it is mostly one command)
  PARA     a paragraph above 60 words or 6 sentences
  CELL     a table cell above 40 words
  SECTION  a heading followed by more than 400 words of running text
           (tables, code, terminal frames and figures do not count)

Usage:
  python3 tools/site/textlint.py docs/design/build.html [more pages]
  python3 tools/site/textlint.py docs            # all pages under docs/

Exit status 1 when a page has a finding. Standard library only.
"""
import html
import re
import sys
from pathlib import Path

SENT_MAX, STEP_MAX, PARA_MAX, PARA_SENTS, CELL_MAX, SECTION_MAX = 25, 20, 60, 6, 40, 400
CODE_SHARE = 0.4
SPLIT = re.compile(r"(?<!\be\.g\.)(?<!\bi\.e\.)(?<!\betc\.)(?<!\bvs\.)(?<=[.!?])\s+(?=[^\s.,;:)\]])")
WORD = re.compile(r"[A-Za-z0-9][\w'./@:+-]*")
CODE = re.compile(r"<code\b[^>]*>(.*?)</code>", re.S)
TAG = re.compile(r"<[^>]+>")


def text(fragment):
    return " ".join(html.unescape(TAG.sub(" ", fragment)).split())


def words(s):
    return WORD.findall(s)


def skip_blocks(src):
    for tag in ("svg", "pre", "script", "table"):
        src = re.sub(rf"<{tag}\b.*?</{tag}>", f" <{tag}-skipped/> ", src, flags=re.S)
    return src


def lint(path):
    src = Path(path).read_text(encoding="utf-8")
    out = []
    cells_src = re.sub(r"<(svg|pre|script)\b.*?</\1>", " ", src, flags=re.S)
    for m in re.finditer(r"<td\b[^>]*>(.*?)</td>", cells_src, re.S):
        n = len(words(text(m.group(1))))
        if n > CELL_MAX:
            out.append(f"CELL {n}w: {text(m.group(1))[:100]}…")
    body = skip_blocks(src)
    for m in re.finditer(r"<(p|li|dd|figcaption)\b([^>]*)>(.*?)</\1>", body, re.S):
        inner = re.sub(r"<(ul|ol)\b.*?</\1>", " ", m.group(3), flags=re.S)
        plain = text(inner)
        codes = " ".join(text(c) for c in CODE.findall(inner))
        sents = [s for s in SPLIT.split(plain) if s.strip()]
        limit = STEP_MAX if m.group(1) == "li" and "steps" in _parent_class(body, m.start()) else SENT_MAX
        if m.group(1) == "p" and (len(words(plain)) > PARA_MAX or len(sents) > PARA_SENTS):
            out.append(f"PARA {len(words(plain))}w/{len(sents)}s: {plain[:100]}…")
        for s in sents:
            n = len(words(s))
            if n <= limit:
                continue
            code_words = sum(len(words(c)) for c in re.findall(r"\S+", codes) if c in s)
            if code_words / n >= CODE_SHARE:
                continue
            out.append(f"SENT {n}w: {s[:120]}")
    heads = list(re.finditer(r"<h([1-4])\b[^>]*>(.*?)</h\1>", body, re.S))
    for i, h in enumerate(heads):
        end = heads[i + 1].start() if i + 1 < len(heads) else len(body)
        chunk = re.sub(r"<figure\b.*?</figure>", " ", body[h.end():end], flags=re.S)
        n = len(words(text(chunk)))
        if n > SECTION_MAX:
            out.append(f"SECTION {n}w of running text: {text(h.group(2))}")
    return out


def _parent_class(src, pos):
    """The class of the nearest open <ol> or <ul> before pos."""
    opens = [m for m in re.finditer(r"<(ol|ul)\b([^>]*)>|</(ol|ul)>", src[:pos])]
    depth = []
    for m in opens:
        if m.group(1):
            depth.append(m.group(2))
        elif depth:
            depth.pop()
    return depth[-1] if depth else ""


def main(argv):
    if not argv:
        print(__doc__.strip())
        return 2
    pages = []
    for a in argv:
        p = Path(a)
        if p.is_dir():
            pages += sorted(x for x in p.rglob("*.html") if "mockups" not in x.parts and x.name != "glossary.html")
        else:
            pages.append(p)
    total = 0
    for p in pages:
        found = lint(p)
        total += len(found)
        if found:
            print(f"{p}: {len(found)}")
            for f in found:
                print(f"  {f}")
    print(f"{len(pages)} pages, {total} findings")
    return 1 if total else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
