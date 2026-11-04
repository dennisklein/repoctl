#!/usr/bin/env python3
"""Build the repoctl design site from HTML fragments.

Usage: python3 tools/site/build.py [--src docs] [--out _site] [--strict]

Each page in tools/site/nav.json and the home page are fragments: the build
takes their <title>, <meta name=description> and <body>, and wraps the body in
tools/site/template.html. All other files under --src are copied verbatim.
The build adds heading ids and anchors, the "On this page" list, table
wrappers, image sizes, prev/next links, the diagram markers and a search index,
and it checks all relative links. Python 3 standard library only.
"""

import argparse
import html
import json
import os
import posixpath
import re
import shutil
import struct
import subprocess
import sys
from html.parser import HTMLParser
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import glossary as gloss  # noqa: E402  (tools/site/glossary.py)

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
MARKER = ".repoctl-site"

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "source", "track", "wbr"}
LINK_ATTRS = ("href", "src", "xlink:href", "poster")
# Elements that do not separate words in the search text.
INLINE = {"a", "abbr", "b", "code", "dfn", "em", "i", "kbd", "mark", "q", "s", "samp", "small",
          "strong", "sub", "sup", "time", "u", "var", "wbr"}
EXTERNAL_RE = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")
CALLOUT_LABELS = {"note": "Note", "rule": "Rule", "why": "Why", "warn": "Warning", "tip": "Tip"}
CODE_LABELS = {"yaml": "YAML", "json": "JSON", "bash": "Bash", "shell": "Shell",
               "ini": "INI", "plaintext": "Text", "text": "Text", "toml": "TOML",
               "go": "Go", "diff": "Diff", "xml": "XML", "html": "HTML", "jsonl": "JSONL",
               "systemd": "systemd", "spec": "Spec"}

# Classes of the authoring contract (see README.md, "Write a page").
CONTRACT_CLASSES = set("""
lede keypoints callout note rule why warn tip callout-title term synopsis tag read
staging local shared req info ok muted compact steps facts cmd diagram wide
explainer shot no-gl glossary gl-index gl-icon gl-term gl-kind gl-see box box-accent box-soft box-ok box-warn box-err box-info box-muted
zone dot dot-accent edge edge-accent edge-muted edge-ok edge-err t t-title t-small
t-muted t-mono t-accent t-ok t-warn t-err t-num t-on
""".split())

SVG_DEFS = (
    '<svg class="svg-defs" aria-hidden="true" focusable="false" width="0" height="0">'
    "<defs>"
    '<marker id="arrow" class="marker" viewBox="0 0 10 10" refX="8" refY="5" '
    'markerWidth="9" markerHeight="9" markerUnits="userSpaceOnUse" orient="auto-start-reverse">'
    '<path d="M0.5,0.8 L10,5 L0.5,9.2 L2.6,5 Z"/></marker>'
    '<marker id="arrow-accent" class="marker marker-accent" viewBox="0 0 10 10" refX="8" refY="5" '
    'markerWidth="9" markerHeight="9" markerUnits="userSpaceOnUse" orient="auto-start-reverse">'
    '<path d="M0.5,0.8 L10,5 L0.5,9.2 L2.6,5 Z"/></marker>'
    '<marker id="arrow-muted" class="marker marker-muted" viewBox="0 0 10 10" refX="8" refY="5" '
    'markerWidth="9" markerHeight="9" markerUnits="userSpaceOnUse" orient="auto-start-reverse">'
    '<path d="M0.5,0.8 L10,5 L0.5,9.2 L2.6,5 Z"/></marker>'
    "</defs>{symbols}</svg>"
)
SYN_TOKEN = re.compile(r"\[[^\]\n]*\](?:\.\.\.)?|[^\s\[]+")
SYN_LONG = 28
# A short inline code (a flag, a name) does not break at its hyphens or spaces.
SHORT_CODE = re.compile(r"<code>([^<\n]{3,40})</code>")
SHORT_CODE_LEN = 22


def short_code(m):
    t = html.unescape(m.group(1))
    if len(t) > SHORT_CODE_LEN or not re.search(r"[-\s/]", t):
        return m.group(0)
    return f'<code class="nw">{m.group(1)}</code>'


def syn_group(m):
    """One option group of a synopsis that does not break; a long list of
    alternatives breaks after a "|"."""
    g = m.group(0)
    if len(g) <= SYN_LONG or " | " not in g:
        return f'<span class="syn">{g}</span>'
    return " ".join(f'<span class="syn">{p}</span>' for p in g.replace(" | ", " |\x00").split("\x00"))
# Longest line (characters) that fits the text column; longer blocks use the wide measure.
WIDE_CODE_COLS = 74
WIDE_TERM_COLS = 76
TEMPLATE_IDS = {"content", "sidebar", "sidebar-nav", "toc", "toc-label", "search", "search-input",
                "search-results", "arrow", "arrow-accent", "arrow-muted", "top"}
# Icons of the pages without a number (nav.json "icon"; the default is "gallery").
PAGE_ICONS = {
    "gallery": ('<svg viewBox="0 0 16 16" width="14" height="14">'
                '<rect x="2" y="2.5" width="5" height="5" rx="1.2"/><rect x="9" y="2.5" width="5" height="5" rx="1.2"/>'
                '<rect x="2" y="9" width="5" height="5" rx="1.2"/><rect x="9" y="9" width="5" height="5" rx="1.2"/></svg>'),
    "glossary": ('<svg viewBox="0 0 16 16" width="14" height="14">'
                 '<path d="M3 3.2c0-.94.76-1.7 1.7-1.7H13v10.3H4.9a.9.9 0 0 0 0 1.8H13V15H4.8A1.8 1.8 0 0 1 3 13.2Z"/></svg>'),
}
# The page of the glossary: its body has <div data-build="glossary"></div>.
GLOSSARY_BLOCK = re.compile(r'<div\s+data-build="glossary"\s*>\s*</div>')


# --------------------------------------------------------------------------
# small helpers

def esc(s):
    return html.escape(s, quote=True)


def slugify(text):
    s = html.unescape(text).lower()
    s = s.replace("@", " at ").replace("&", " and ")
    s = re.sub(r"[^a-z0-9]+", "-", s).strip("-")
    return s or "section"


def norm_space(s):
    return re.sub(r"\s+", " ", s).strip()


def rel(from_page, to_path):
    """Relative URL from page `from_page` (site path) to site path `to_path`."""
    base = posixpath.dirname(from_page) or "."
    return posixpath.relpath(to_path, base)


def insert_attrs(tag_text, extra):
    """Insert attribute text before the end of a raw start tag."""
    if tag_text.endswith("/>"):
        return tag_text[:-2].rstrip() + " " + extra + " />"
    return tag_text[:-1].rstrip() + " " + extra + ">"


def add_class(tag_text, cls):
    m = re.search(r'\sclass\s*=\s*("([^"]*)"|\'([^\']*)\')', tag_text)
    if not m:
        return insert_attrs(tag_text, f'class="{cls}"')
    old = m.group(2) if m.group(2) is not None else m.group(3)
    return tag_text[:m.start()] + f' class="{old} {cls}"' + tag_text[m.end():]


# Screenshots taken at deviceScaleFactor 2 (tools/site/shots.mjs): shown at half size.
HIDPI_RE = re.compile(r"(?:^term-.+|^ui-.+|.+@2x)\.(?:webp|png|jpe?g)$")


def css_size(path, size):
    """The CSS size of an image: half the pixels for a 2x screenshot."""
    if size and HIDPI_RE.match(posixpath.basename(path)) and size[0] >= 600:
        return (size[0] + 1) // 2, (size[1] + 1) // 2
    return size


def image_size(path):
    """Return (width, height) of a PNG, WebP, GIF, JPEG or SVG file, or None."""
    try:
        with open(path, "rb") as f:
            head = f.read(64 * 1024)
    except OSError:
        return None
    if head[:8] == b"\x89PNG\r\n\x1a\n" and head[12:16] == b"IHDR":
        return struct.unpack(">II", head[16:24])
    if head[:4] == b"RIFF" and head[8:12] == b"WEBP":
        kind = head[12:16]
        if kind == b"VP8X":
            w = 1 + int.from_bytes(head[24:27], "little")
            h = 1 + int.from_bytes(head[27:30], "little")
            return w, h
        if kind == b"VP8 ":
            # frame tag (3 bytes) + start code 9d 01 2a, then 14-bit sizes
            if head[23:26] == b"\x9d\x01\x2a":
                w, h = struct.unpack("<HH", head[26:30])
                return w & 0x3FFF, h & 0x3FFF
            return None
        if kind == b"VP8L":
            if head[20] != 0x2F:
                return None
            b = int.from_bytes(head[21:25], "little")
            return (b & 0x3FFF) + 1, ((b >> 14) & 0x3FFF) + 1
        return None
    if head[:6] in (b"GIF87a", b"GIF89a"):
        return struct.unpack("<HH", head[6:10])
    if head[:2] == b"\xff\xd8":
        i = 2
        while i + 9 < len(head):
            if head[i] != 0xFF:
                i += 1
                continue
            marker = head[i + 1]
            if marker in (0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF):
                h, w = struct.unpack(">HH", head[i + 5:i + 9])
                return w, h
            seg = struct.unpack(">H", head[i + 2:i + 4])[0]
            i += 2 + seg
        return None
    text = head.decode("utf-8", "replace")
    m = re.search(r"<svg\b[^>]*>", text)
    if m:
        tag = m.group(0)
        wm = re.search(r'\swidth="([\d.]+)(px)?"', tag)
        hm = re.search(r'\sheight="([\d.]+)(px)?"', tag)
        if wm and hm:
            return round(float(wm.group(1))), round(float(hm.group(1)))
        vb = re.search(r'viewBox="\s*[-\d.]+[\s,]+[-\d.]+[\s,]+([\d.]+)[\s,]+([\d.]+)', tag)
        if vb:
            return round(float(vb.group(1))), round(float(vb.group(2)))
    return None


def parse_frames(spec):
    """'0' | '1-3' | '0,2,5' | '0,2-4' -> set of ints."""
    out = set()
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            a, b = part.split("-", 1)
            out.update(range(int(a), int(b) + 1))
        else:
            out.add(int(part))
    return out


def git_commit():
    sha = os.environ.get("GITHUB_SHA", "").strip()
    if sha:
        return sha[:7]
    try:
        r = subprocess.run(["git", "rev-parse", "--short=7", "HEAD"], cwd=REPO,
                           capture_output=True, text=True, timeout=10)
        if r.returncode == 0:
            return r.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return ""


# --------------------------------------------------------------------------
# fragment reading

class FragmentReader(HTMLParser):
    """Find <title>, <meta name=description>, and the raw <body> content."""

    def __init__(self, text):
        super().__init__(convert_charrefs=True)
        self.text = text
        self.starts = [0]
        for m in re.finditer("\n", text):
            self.starts.append(m.end())
        self.title = None
        self.description = None
        self.body_attrs = {}
        self.body_start = None
        self.body_end = None
        self.body_line = 1
        self._in_title = False
        self._title_buf = []

    def _offset(self):
        line, col = self.getpos()
        return self.starts[line - 1] + col

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "body" and self.body_start is None:
            self.body_start = self._offset() + len(self.get_starttag_text())
            self.body_line = self.getpos()[0]
            self.body_attrs = a
        elif self.body_start is None and tag == "title" and self.title is None:
            self._in_title = True
        elif self.body_start is None and tag == "meta" and (a.get("name") or "").lower() == "description":
            self.description = a.get("content") or ""

    def handle_endtag(self, tag):
        if tag == "title" and self._in_title:
            self._in_title = False
            self.title = norm_space("".join(self._title_buf))
        elif tag == "body" and self.body_start is not None and self.body_end is None:
            self.body_end = self._offset()

    def handle_data(self, data):
        if self._in_title:
            self._title_buf.append(data)

    def body(self):
        if self.body_start is None:
            return self.text, 1
        end = self.body_end
        if end is None:
            m = re.search(r"</html\s*>", self.text[self.body_start:], re.I)
            end = self.body_start + m.start() if m else len(self.text)
        return self.text[self.body_start:end], self.body_line


# --------------------------------------------------------------------------
# body rewriting

class Rewriter(HTMLParser):
    """Stream the body HTML and rewrite it for the site template."""

    def __init__(self, page, ctx):
        super().__init__(convert_charrefs=False)
        self.page = page            # site path of the page, e.g. design/build.html
        self.ctx = ctx              # build context (src root, nav, generated blocks)
        self.line_offset = 0
        self.out = []
        self.stack = []
        self.capture = None
        self.ids = []
        self.links = []             # (tag, attr, value, line)
        self.resources = []         # external resources (tag, attr, value, line)
        self.lint = []
        self.toc = []               # (level, id, text)
        self.h1 = []
        self.sections = []
        self.cur = {"heading": "", "anchor": "", "text": []}
        self.sections.append(self.cur)
        self.svg_depth = 0
        self.table_depth = 0
        self.skip_depth = 0         # inside a data-build placeholder
        self.classes = set()
        self.frames = []            # explainer checks
        self.counts = {"tables": 0, "images": 0, "diagrams": 0, "explainers": 0, "code": 0, "term": 0}
        self.has_code = False

    # -- output helpers
    def emit(self, s):
        if self.skip_depth:
            return
        if self.capture is not None:
            self.capture["inner"].append(s)
        else:
            self.out.append(s)

    def text(self, s):
        if self.skip_depth:
            return
        if self.capture is not None:
            self.capture["text"].append(s)
            return
        for e in self.stack:
            if e["tag"] in ("script", "style", "svg", "template", "h1"):
                return
        self.cur["text"].append(s)

    def line(self):
        return self.getpos()[0] + self.line_offset

    def parent(self, tag=None, cls=None):
        for e in reversed(self.stack):
            if (tag is None or e["tag"] == tag) and (cls is None or cls in e["cls"]):
                return e
        return None

    def new_section(self, heading, anchor):
        self.cur = {"heading": heading, "anchor": anchor, "text": []}
        self.sections.append(self.cur)

    # -- parser callbacks
    def handle_starttag(self, tag, attrs):
        self.start(tag, attrs, self.get_starttag_text(), tag in VOID)

    def handle_startendtag(self, tag, attrs):
        self.start(tag, attrs, self.get_starttag_text(), True)

    def start(self, tag, attrs, raw, closed):
        if self.skip_depth:
            if not closed:
                self.skip_depth += 1
            return
        a = {k: (v if v is not None else "") for k, v in attrs}
        cls = a.get("class", "").split()
        line = self.line()
        in_svg = self.svg_depth > 0
        entry = {"tag": tag, "cls": cls, "attrs": a, "line": line}

        if in_svg or tag == "svg":
            self.classes.update(c for c in cls)
        else:
            self.classes.update(cls)

        # lint: contract rules
        if "style" in a and not self.ctx.get("custom"):
            self.lint.append((line, f"inline style attribute on <{tag}>"))
        if tag in ("style", "link") and not self.ctx.get("custom"):
            self.lint.append((line, f"<{tag}> element in the body"))
        if tag == "script" and not (a.get("type") == "application/json" and "steps" in cls):
            self.lint.append((line, "<script> other than the explainer steps JSON"))
        if in_svg:
            for attr in ("fill", "stroke", "color", "stop-color"):
                v = (a.get(attr) or "").strip().lower()
                if v and v not in ("none", "currentcolor", "transparent", "inherit") and not v.startswith("url("):
                    self.lint.append((line, f"hard-coded colour {attr}={v!r} on <{tag}> in an SVG"))
        if tag == "img" and "alt" not in a:
            self.lint.append((line, "<img> without alt"))
        if a.get("id"):
            self.ids.append((a["id"], line))
        for attr in LINK_ATTRS:
            if attr in a and a[attr] != "":
                self.links.append((tag, attr, a[attr], line))
                loads = attr in ("src", "poster") or tag in ("link", "image", "use")
                if loads and re.match(r"^(https?:)?//", a[attr]):
                    self.resources.append((tag, attr, a[attr], line))

        # generated block placeholder
        if a.get("data-build"):
            block = self.ctx["blocks"].get(a["data-build"])
            if block is None:
                self.lint.append((line, f"unknown data-build block {a['data-build']!r}"))
            else:
                self.emit(raw)
                self.emit(block)
                if not closed:
                    self.skip_depth = 1
                    self.stack.append(dict(entry, generated=True))
                return

        # explainer step data
        if tag == "script" and "steps" in cls:
            entry["buf"] = []

        if tag == "svg" and not in_svg:
            self.svg_depth = 1
            fig = self.stack[-1] if self.stack else None
            if fig and fig["tag"] == "figure" and "diagram" in fig["cls"]:
                entry["wrap"] = True
                self.counts["diagrams"] += 1
                self.emit('<div class="diagram-body">')
            if "viewbox" in a and "width" not in a and "style" not in a:
                vb = re.split(r"[\s,]+", a["viewbox"].strip())
                if len(vb) == 4:
                    try:
                        w, h = float(vb[2]), float(vb[3])
                        raw = insert_attrs(raw, f'width="{w:g}" height="{h:g}" style="--vbw:{w:g}px"')
                    except ValueError:
                        self.lint.append((line, "bad viewBox"))
            if "role" not in a:
                self.lint.append((line, "<svg> without role=img"))
            self.emit(raw)
            self.stack.append(entry)
            if closed:
                self.end_entry(self.stack.pop())
            return
        if in_svg:
            if "data-f" in a:
                fig = self.parent("figure", "explainer")
                if fig is not None:
                    try:
                        fig.setdefault("frames", set()).update(parse_frames(a["data-f"]))
                    except ValueError:
                        self.lint.append((line, f"bad data-f {a['data-f']!r}"))
            if tag == "svg":
                self.svg_depth += 1
            self.emit(raw)
            if not closed:
                self.stack.append(entry)
            return

        # headings
        if tag == "h1":
            self.h1.append(line)
            if self.ctx.get("eyebrow") and len(self.h1) == 1:
                self.emit(self.ctx["eyebrow"])
        if tag in ("h2", "h3") or (tag == "h4"):
            cmd = self.parent("section", "cmd")
            if tag == "h4" and cmd is None and not self.ctx.get("custom"):
                self.lint.append((line, "<h4> outside section.cmd"))
            self.capture = {"tag": tag, "raw": raw, "attrs": a, "inner": [], "text": [],
                            "cmd": cmd, "line": line}
            self.stack.append(entry)
            return

        if tag == "table" and not closed:
            self.table_depth += 1
            if self.table_depth == 1:
                entry["wrap"] = True
                self.counts["tables"] += 1
                self.emit('<div class="table-wrap">')
            else:
                self.lint.append((line, "table inside a table"))
        elif tag == "img":
            raw = self.fix_img(raw, a, line)
        elif tag == "pre":
            entry["pre_at"] = len(self.out) + (1 if "term" in cls else 0)
            if "term" in cls:
                entry["term"] = True
                self.counts["term"] += 1
                self.emit('<div class="term-window">'
                          '<div class="term-bar"><span class="term-dots" aria-hidden="true"><i></i><i></i><i></i></span>'
                          '<button class="copy-button" type="button" data-copy="term" aria-label="Copy commands">Copy</button>'
                          '</div>')
            elif "synopsis" not in cls:
                entry["code_at"] = len(self.out)
                entry["lang"] = ""
                self.counts["code"] += 1
        elif tag == "code":
            pre = self.stack[-1] if self.stack else None
            if pre and pre["tag"] == "pre" and "code_at" in pre:
                for c in cls:
                    if c.startswith("language-"):
                        pre["lang"] = c[len("language-"):]
                        if pre["lang"] not in ("plaintext", "text"):
                            self.has_code = True
        elif tag == "figure":
            if "shot" in cls:
                entry["shot_at"] = len(self.out)
            if "explainer" in cls:
                self.counts["explainers"] += 1
                entry["frames"] = set()
        elif tag == "a":
            fig = self.stack[-1] if self.stack else None
            if fig and fig["tag"] == "figure" and "shot" in fig["cls"]:
                raw = add_class(raw, "shot-link")
                self.emit(raw)
                if "term" not in fig["cls"]:
                    self.emit('<span class="shot-bar" aria-hidden="true"><span class="shot-dots"><i></i><i></i><i></i></span>'
                              '<span class="shot-open">Open mockup</span></span>')
                if not closed:
                    self.stack.append(entry)
                return
        elif tag == "aside" and "callout" in cls:
            entry["callout_at"] = len(self.out) + 1
            entry["first_child"] = None
            kinds = [c for c in cls if c in CALLOUT_LABELS]
            if len(kinds) != 1:
                self.lint.append((line, "aside.callout needs exactly one of note, rule, why, warn, tip"))
            entry["kind"] = kinds[0] if kinds else "note"

        # first element child of a callout
        par = self.stack[-1] if self.stack else None
        if par is not None and par["tag"] == "aside" and "callout_at" in par and par["first_child"] is None:
            par["first_child"] = "title" if (tag == "p" and "callout-title" in cls) else tag

        self.emit(raw)
        if not closed:
            self.stack.append(entry)

    def fix_img(self, raw, a, line):
        self.counts["images"] += 1
        src = a.get("src", "")
        if not src or EXTERNAL_RE.match(src) or src.startswith("/"):
            return raw
        target = posixpath.normpath(posixpath.join(posixpath.dirname(self.page), src.split("#")[0].split("?")[0]))
        size = css_size(target, image_size(self.ctx["src_root"] / target))
        extra = []
        if size and "width" not in a and "height" not in a:
            extra.append(f'width="{size[0]}" height="{size[1]}"')
        if "decoding" not in a:
            extra.append('decoding="async"')
        if extra:
            raw = insert_attrs(raw, " ".join(extra))
        fig = self.parent("figure", "shot")
        if fig is not None and size and "shot_at" in fig:
            w, h = size
            if w < h and w <= 900:
                i = fig["shot_at"]
                self.out[i] = add_class(self.out[i], "shot-narrow")
        return raw

    def handle_endtag(self, tag):
        if self.skip_depth:
            self.skip_depth -= 1
            if self.skip_depth:
                return
            # closing tag of the placeholder
            while self.stack:
                e = self.stack.pop()
                if e.get("generated"):
                    break
            self.emit(f"</{tag}>")
            return
        if not any(e["tag"] == tag for e in self.stack):
            if tag not in VOID:
                self.lint.append((self.line(), f"stray </{tag}>"))
            self.emit(f"</{tag}>")
            return
        while self.stack:
            e = self.stack.pop()
            if e["tag"] == tag:
                self.end_entry(e)
                return
            # implicitly closed element (for example <p> or <li>)
            self.end_entry(e, implicit=True)

    def end_entry(self, e, implicit=False):
        tag = e["tag"]
        if tag not in INLINE and not (tag == "span" and not e["cls"]):
            self.text(" ")
        if self.capture is not None and tag == self.capture["tag"]:
            self.finish_heading()
            return
        close = "" if (implicit or tag in VOID) else f"</{tag}>"
        if tag == "svg":
            self.svg_depth -= 1
            self.emit(close)
            if e.get("wrap"):
                self.emit("</div>")
            return
        if self.svg_depth:
            self.emit(close)
            return
        if tag == "script" and "buf" in e:
            self.emit(close)
            try:
                steps = json.loads("".join(e["buf"]))
                if not isinstance(steps, list):
                    raise ValueError
                fig = self.parent("figure", "explainer")
                if fig is not None:
                    fig["steps"] = len(steps)
                self.cur["text"].append(" " + " ".join(str(s) for s in steps) + " ")
            except ValueError:
                self.lint.append((e["line"], "explainer steps are not a JSON list"))
            return
        if tag == "table" and not implicit:
            self.table_depth -= 1
            self.emit(close)
            if e.get("wrap"):
                self.emit("</div>")
            return
        if tag == "pre" and "synopsis" in e["cls"] and "pre_at" in e and self.capture is None:
            # keep each option group together: lines break only between groups
            i = e["pre_at"] + 1
            inner = "".join(self.out[i:])
            if "<" not in inner:
                del self.out[i:]
                self.out.append(SYN_TOKEN.sub(syn_group, inner))
            self.emit(close)
            return
        if tag == "pre":
            self.emit(close)
            longest = 0
            if "pre_at" in e and self.capture is None:
                text = html.unescape(re.sub(r"<[^>]*>", "", "".join(self.out[e["pre_at"]:])))
                longest = max((len(ln.rstrip()) for ln in text.split("\n")), default=0)
            if e.get("term"):
                self.emit("</div>")
                if longest > WIDE_TERM_COLS and self.out[e["pre_at"] - 1].startswith('<div class="term-window"'):
                    self.out[e["pre_at"] - 1] = self.out[e["pre_at"] - 1].replace(
                        '<div class="term-window"', '<div class="term-window is-wide"', 1)
            elif "code_at" in e:
                lang = e["lang"]
                label = CODE_LABELS.get(lang, lang.upper() if lang else "")
                wide = " is-wide" if longest > WIDE_CODE_COLS else ""
                head = (f'<div class="codeblock{wide}" data-lang="{esc(lang or "none")}">'
                        f'<div class="codeblock-head">'
                        + (f'<span class="codeblock-lang">{esc(label)}</span>' if label else "")
                        + '<button class="copy-button" type="button" data-copy="code" aria-label="Copy code">Copy</button></div>')
                self.out.insert(e["code_at"], head)
                self.emit("</div>")
            return
        if tag == "aside" and "callout_at" in e:
            if e["first_child"] != "title":
                label = CALLOUT_LABELS.get(e["kind"], "Note")
                self.out.insert(e["callout_at"], f'<p class="callout-title">{label}</p>')
            self.emit(close)
            return
        if tag == "figure" and "frames" in e:
            n = e.get("steps")
            if n is None:
                self.lint.append((e["line"], "explainer without <script class=\"steps\">"))
            elif e["frames"] and max(e["frames"]) >= n:
                self.lint.append((e["line"], f"explainer uses frame {max(e['frames'])} but has {n} captions"))
        if tag == "section" and "cmd" in e["cls"]:
            prev = e.get("prev_section")
            if prev is not None:
                self.cur = prev
        self.emit(close)

    def finish_heading(self):
        c = self.capture
        self.capture = None
        tag = c["tag"]
        inner = "".join(c["inner"])
        # the heading text without tags (a safety class or a level is not part of the title)
        plain = re.sub(r'<span\s+class="tag\b[^"]*"[^>]*>.*?</span>', "", inner, flags=re.S)
        text = norm_space(html.unescape(re.sub(r"<[^>]*>", "", plain)))
        raw = c["raw"]
        hid = c["attrs"].get("id", "")
        if tag == "h4":
            cmd = c["cmd"]
            anchor = cmd["attrs"].get("id", "") if cmd is not None else hid
            if anchor:
                inner += f'<a class="heading-anchor" href="#{esc(anchor)}" aria-label="Link to {esc(text)}">#</a>'
                prev = self.cur
                self.new_section(text, anchor)
                if cmd is not None:
                    cmd["prev_section"] = prev
            self.out.append(raw + inner + "</h4>")
            return
        if not hid:
            base = slugify(text)
            hid = base
            used = self.ctx["used_ids"]
            n = 2
            while hid in used:
                hid = f"{base}-{n}"
                n += 1
            raw = insert_attrs(raw, f'id="{hid}"')
            self.ids.append((hid, c["line"]))
        self.ctx["used_ids"].add(hid)
        inner += f'<a class="heading-anchor" href="#{esc(hid)}" aria-label="Link to this section">#</a>'
        self.out.append(raw + inner + f"</{tag}>")
        if self.ctx.get("toc_marker") and tag == "h2" and not any(t[0] == 2 for t in self.toc):
            # the inline "On this page" list goes before the first h2
            self.out.insert(len(self.out) - 1, "\x00TOC\x00")
        self.toc.append((int(tag[1]), hid, text))
        self.new_section(text, hid)

    def handle_data(self, data):
        if self.skip_depth:
            return
        top = self.stack[-1] if self.stack else None
        if top is not None and "buf" in top:
            top["buf"].append(data)
        self.emit(data)
        self.text(data)

    def handle_entityref(self, name):
        self.emit(f"&{name};")
        self.text(html.unescape(f"&{name};"))

    def handle_charref(self, name):
        self.emit(f"&#{name};")
        self.text(html.unescape(f"&#{name};"))

    def handle_comment(self, data):
        self.emit(f"<!--{data}-->")

    def handle_decl(self, decl):
        self.emit(f"<!{decl}>")

    def unknown_decl(self, data):
        self.emit(f"<![{data}]>")

    def handle_pi(self, data):
        self.emit(f"<?{data}>")


class VerbatimScanner(HTMLParser):
    """Collect links and external resources of a verbatim HTML page."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.links = []
        self.resources = []
        self.styles = []
        self._in_style = False

    def handle_starttag(self, tag, attrs):
        a = {k: (v or "") for k, v in attrs}
        for attr in LINK_ATTRS:
            v = a.get(attr, "")
            if not v:
                continue
            self.links.append((tag, attr, v, self.getpos()[0]))
            loads = attr != "href" or (tag == "link" and a.get("rel", "") not in ("canonical",))
            if loads and re.match(r"^(https?:)?//", v):
                self.resources.append((tag, attr, v, self.getpos()[0]))
        if "style" in a:
            self.styles.append(a["style"])
        self._in_style = tag == "style"

    handle_startendtag = handle_starttag

    def handle_endtag(self, tag):
        if tag == "style":
            self._in_style = False

    def handle_data(self, data):
        if self._in_style:
            self.styles.append(data)


CSS_URL_RE = re.compile(r"""@import\s+(?:url\()?\s*['"]?([^'")\s;]+)|url\(\s*['"]?([^'")]+)['"]?\s*\)""")


def css_urls(text):
    out = []
    for m in CSS_URL_RE.finditer(text):
        out.append(m.group(1) or m.group(2))
    return out


# --------------------------------------------------------------------------
# site generation

class Site:
    def __init__(self, src, out, strict):
        self.src = src
        self.out = out
        self.strict = strict
        self.errors = []      # fatal with --strict
        self.warnings = []    # never fatal
        self.nav = json.loads((HERE / "nav.json").read_text(encoding="utf-8"))
        self.template = (HERE / "template.html").read_text(encoding="utf-8")
        self.commit = git_commit()
        self.site = self.nav["site"]
        self.pages = []       # ordered nav pages with part info
        for part in self.nav["parts"]:
            for p in part["pages"]:
                self.pages.append(dict(p, part=part))
        self.home = self.site.get("home", "index.html")
        self.wrapped = {}     # path -> result dict
        self.files = set()    # all output paths
        # the glossary: data, the domain icons as symbols, the ids of the symbols
        data = self.src / "assets" / "glossary-data.js"
        self.glossary = gloss.Glossary(data) if data.is_file() else None
        symbols = ""
        self.symbol_ids = set()
        if self.glossary:
            for key, d in self.glossary.domains.items():
                symbols += (f'<symbol id="gl-i-{key}" viewBox="0 0 16 16">'
                            f'<path d="{esc(d["icon"])}"/></symbol>')
                self.symbol_ids.add(f"gl-i-{key}")
        self.svg_defs = SVG_DEFS.replace("{symbols}", symbols)

    def problem(self, msg, kind="other"):
        """A problem that fails the build with --strict."""
        self.errors.append((kind, msg))

    def warn(self, msg):
        self.warnings.append(msg)

    # -- nav html
    def sidebar(self, page):
        parts = []
        home_cls = ' aria-current="page"' if page == self.home else ""
        parts.append(f'<a class="sidebar-home" href="{rel(page, self.home)}"{home_cls}>'
                     f'<span class="nav-num" aria-hidden="true">'
                     f'<svg viewBox="0 0 16 16" width="14" height="14"><path d="M2.5 7.2 8 2.8l5.5 4.4V13a.5.5 0 0 1-.5.5H9.6V10H6.4v3.5H3a.5.5 0 0 1-.5-.5Z"/></svg>'
                     f'</span><span class="nav-title">Overview</span></a>')
        for part in self.nav["parts"]:
            items = []
            for p in part["pages"]:
                num = p.get("number")
                num_html = (f'<span class="nav-num">{num}</span>' if num is not None else
                            f'<span class="nav-num" aria-hidden="true">{PAGE_ICONS[p.get("icon", "gallery")]}</span>')
                title = esc(p["short"])
                if p["path"] not in self.available:
                    items.append(f'<li><span class="nav-link is-missing" title="Not written yet">{num_html}'
                                 f'<span class="nav-title">{title}</span></span></li>')
                    continue
                cur = ' aria-current="page"' if p["path"] == page else ""
                items.append(f'<li><a class="nav-link" href="{rel(page, p["path"])}"{cur}>{num_html}'
                             f'<span class="nav-title">{title}</span></a></li>')
            parts.append(f'<div class="nav-part"><p class="nav-part-title"><span class="nav-part-num">{part["number"]}</span>'
                         f'{esc(part["title"])}</p><ol class="nav-list">{"".join(items)}</ol></div>')
        return "".join(parts)

    def header_links(self, page, section):
        out = []
        for h in self.nav["header_links"]:
            href = rel(page, h["path"]) + (f'#{h["anchor"]}' if h.get("anchor") else "")
            attrs = ""
            if h["path"] == page and not h.get("anchor"):
                attrs = ' aria-current="page"'
            elif h["section"] == section:
                attrs = ' class="is-active"'
            if h["path"] not in self.available and h["path"] != self.home:
                continue
            out.append(f'<a href="{href}"{attrs}>{esc(h["title"])}</a>')
        return "".join(out)

    def toc_html(self, toc, inline=False):
        if not toc:
            return ""
        items = []
        open_sub = False
        for level, hid, text in toc:
            if level == 2:
                if open_sub:
                    items.append("</ol></li>")
                    open_sub = False
                elif items:
                    items.append("</li>")
                items.append(f'<li><a href="#{esc(hid)}">{esc(text)}</a>')
            else:
                if not items:
                    items.append('<li class="toc-orphan">')
                if not open_sub:
                    items.append("<ol>")
                    open_sub = True
                items.append(f'<li><a href="#{esc(hid)}">{esc(text)}</a></li>')
        if open_sub:
            items.append("</ol>")
        items.append("</li>")
        lst = "<ol>" + "".join(items) + "</ol>"
        if inline:
            n = sum(1 for t in toc if t[0] == 2)
            return (f'<details class="toc-inline"><summary><span>On this page</span>'
                    f'<span class="toc-count">{n} sections</span></summary>{lst}</details>')
        return lst

    def pager(self, page):
        order = [self.home] + [p["path"] for p in self.pages if p["path"] in self.available]
        if page not in order:
            return ""
        i = order.index(page)
        info = {p["path"]: p for p in self.pages}

        def card(path, kind):
            p = info.get(path)
            if p is None:
                title, num = "Overview", ""
            else:
                title = p["title"]
                num = f'<span class="pager-num">{p["number"]}</span>' if p.get("number") is not None else ""
            label = "Previous" if kind == "prev" else "Next"
            return (f'<a class="pager-card pager-{kind}" href="{rel(page, path)}" rel="{kind}">'
                    f'<span class="pager-label">{label}</span>'
                    f'<span class="pager-title">{num}{esc(title)}</span></a>')

        prev = card(order[i - 1], "prev") if i > 0 else '<span class="pager-card pager-empty"></span>'
        nxt = card(order[i + 1], "next") if i + 1 < len(order) else '<span class="pager-card pager-empty"></span>'
        return f'<nav class="pager" aria-label="Chapters">{prev}{nxt}</nav>'

    def chapter_cards(self, page):
        out = []
        for part in self.nav["parts"]:
            cards = []
            for p in part["pages"]:
                num = p.get("number")
                num_html = (f'<span class="card-num">{num}</span>' if num is not None
                            else '<span class="card-num card-num-icon" aria-hidden="true">'
                            + PAGE_ICONS[p.get("icon", "gallery")].replace('width="14" height="14"', 'width="20" height="20"')
                            + '</span>')
                body = (f'{num_html}<span class="card-body"><span class="card-title">{esc(p["title"])}</span>'
                        f'<span class="card-desc">{esc(p["description"])}</span></span>')
                if p["path"] in self.available:
                    cards.append(f'<li><a class="chapter-card" href="{rel(page, p["path"])}">{body}</a></li>')
                else:
                    cards.append(f'<li><span class="chapter-card is-missing">{body}</span></li>')
            out.append(f'<div class="part-block"><p class="part-label"><span class="part-num">Part {part["number"]}</span>'
                       f'<span class="part-name">{esc(part["title"])}</span></p>'
                       f'<ul class="chapter-grid">{"".join(cards)}</ul></div>')
        return "".join(out)

    # -- the glossary page
    def glossary_block(self, page):
        """The body of the glossary page: an A-Z index, then one section per domain."""
        g = self.glossary
        titles = {p["path"]: p["short"] for p in self.pages}
        index = " ".join(f'<a href="#{t["id"]}">{esc(t["term"])}</a>' for t in g.terms)
        out = [f'<p class="gl-index">{index}</p>']
        for key, d in g.domains.items():
            terms = [t for t in g.terms if t["domain"] == key]
            out.append(f'<h2 id="d-{key}"><svg class="gl-icon" data-domain="{key}" viewBox="0 0 16 16" role="img" '
                       f'aria-label="Domain"><use href="#gl-i-{key}"/></svg>{esc(d["name"])}</h2>')
            out.append(f'<dl class="glossary" data-domain="{key}">')
            for t in terms:
                see = t["see"]
                target = posixpath.normpath(posixpath.join("design", see.split("#")[0]))
                label = titles.get(target, see)
                out.append(f'<dt id="{t["id"]}"><span class="gl-term">{esc(t["term"])}</span>'
                           f'<span class="gl-kind">{esc(t["kind"])}</span></dt>'
                           f'<dd>{t["def"]} <span class="gl-see"><a href="{esc(rel(page, posixpath.join("design", see)))}">'
                           f'{esc(label)}</a></span></dd>')
            out.append("</dl>")
        return "\n".join(out)

    # -- one wrapped page
    def wrap(self, page, meta):
        src_file = self.src / page
        text = src_file.read_text(encoding="utf-8")
        fr = FragmentReader(text)
        fr.feed(text)
        fr.close()
        body, body_line = fr.body()
        if self.glossary and GLOSSARY_BLOCK.search(body):
            body = GLOSSARY_BLOCK.sub(lambda m: self.glossary_block(page), body, count=1)
        title = fr.title or ""
        desc = fr.description or (meta or {}).get("description", "")
        if not fr.title:
            self.warn(f"{page}: no <title>")
        if fr.description is None:
            self.warn(f"{page}: no <meta name=description>")
        body_cls = (fr.body_attrs.get("class") or "").split()
        is_home = page == self.home
        part = (meta or {}).get("part")
        eyebrow = ""
        if part is not None:
            num = meta.get("number")
            eyebrow = (f'<p class="eyebrow"><span class="eyebrow-part">Part {part["number"]} · {esc(part["title"])}</span>'
                       + (f'<span class="eyebrow-num">Chapter {num}</span>' if num is not None else "")
                       + "</p>")
        ctx = {
            "src_root": self.src,
            "eyebrow": eyebrow,
            "custom": is_home or (meta or {}).get("custom", False),
            "blocks": {"chapters": self.chapter_cards(page)},
            "used_ids": set(TEMPLATE_IDS) | self.symbol_ids,
            "toc_marker": not is_home,
        }
        rw = Rewriter(page, ctx)
        # ids that the author wrote are reserved before slugs are made
        for m in re.finditer(r'\sid\s*=\s*["\']([^"\']+)["\']', body):
            ctx["used_ids"].add(m.group(1))
        rw.line_offset = body_line - 1
        rw.feed(body)
        rw.close()
        while rw.stack:
            e = rw.stack.pop()
            if e["tag"] not in VOID and e["tag"] not in ("p", "li", "dd", "dt", "td", "th", "tr"):
                rw.lint.append((e["line"], f"unclosed <{e['tag']}>"))
            rw.end_entry(e, implicit=True)
        content = "".join(rw.out)
        inline_toc = self.toc_html(rw.toc, inline=True) if sum(1 for t in rw.toc if t[0] == 2) >= 3 else ""
        content = content.replace("\x00TOC\x00", inline_toc, 1).replace("\x00TOC\x00", "")
        content = SHORT_CODE.sub(short_code, content)
        gl_used = []
        if self.glossary and not ctx["custom"] and page != gloss.PAGE:
            to = rel(page, gloss.PAGE)
            content, gl_used = gloss.link(content, self.glossary, gloss.DOCS, lambda tid: f"{to}#{tid}")
            for tid in gl_used:
                rw.links.append(("a", "href", f"{to}#{tid}", body_line))

        # lint
        if len(rw.h1) != 1:
            rw.lint.append((rw.h1[1] if len(rw.h1) > 1 else body_line, f"{len(rw.h1)} <h1> elements (one expected)"))
        seen = {}
        for i, line in rw.ids:
            if i in seen:
                rw.lint.append((line, f"duplicate id {i!r} (first on line {seen[i]})"))
            else:
                seen[i] = line
        if not ctx["custom"]:
            extra = sorted(c for c in rw.classes - CONTRACT_CLASSES if not c.startswith("language-"))
            if extra:
                rw.lint.append((body_line, "classes outside the contract: " + ", ".join(extra)))
        for line, msg in rw.lint:
            self.warn(f"{page}:{line}: {msg}")
        for tag, attr, v, line in rw.resources:
            self.problem(f"{page}:{line}: external resource <{tag} {attr}={v!r}>", "external")

        ids = set(seen) | TEMPLATE_IDS | self.symbol_ids
        absent = [a for a in (meta or {}).get("anchors", []) if a not in ids]
        if absent:
            self.problem(f"{page}: " + " ".join("#" + a for a in absent), "anchors")

        section = "design"
        if page == "design/mockups/index.html":
            section = "mockups"
        elif page == "design/contract.html":
            section = "contract"
        elif page == gloss.PAGE:
            section = "glossary"
        elif is_home:
            section = ""
        classes = ["page"] + (["page-home"] if is_home else ["page-doc"]) + [f"page-{c}" for c in body_cls]
        toc = self.toc_html(rw.toc)
        rail = ""
        if toc and not is_home:
            rail = (f'<aside class="toc" id="toc" aria-labelledby="toc-label"><div class="toc-inner">'
                    f'<p class="toc-label" id="toc-label">On this page</p><nav aria-labelledby="toc-label">{toc}</nav>'
                    f'<a class="toc-top" href="#top">Back to top</a></div></aside>')
        else:
            classes.append("no-toc")
        root = rel(page, "index.html")[:-len("index.html")]
        doc_title = title if not is_home else f'{self.site["title"]} · {self.site["label"]}'
        if not is_home:
            doc_title = f'{title} · {self.site["title"]} {self.site["label"].lower()}'
        scripts = []
        if rw.has_code:
            scripts.append(f'<script src="{root}assets/vendor/highlight.min.js" defer></script>')
        if gl_used or page == gloss.PAGE:
            scripts.append(f'<script src="{root}assets/glossary-data.js" defer></script>')
            scripts.append(f'<script src="{root}assets/glossary.js" defer></script>')
        scripts.append(f'<script src="{root}assets/site.js" defer></script>')
        commit = self.commit
        commit_html = (f'<a class="footer-commit" href="{esc(self.site["repo"])}/commit/{esc(commit)}">'
                       f'<code>{esc(commit)}</code></a>') if commit else ""
        src_rel = posixpath.join(posixpath.relpath(str(self.src.resolve()), str(REPO)) if self.src.resolve().is_relative_to(REPO) else "docs", page)
        values = {
            "root": root,
            "doc_title": esc(doc_title),
            "description": esc(desc),
            "canonical": esc(self.site["pages_url"] + ("" if page == self.home else page)),
            "site_title": esc(self.site["title"]),
            "site_label": esc(self.site["label"]),
            "record": esc(self.site["record"]),
            "og_title": esc(title or self.site["title"]),
            "body_class": " ".join(classes),
            "home": rel(page, self.home),
            "header_links": self.header_links(page, section),
            "repo": esc(self.site["repo"]),
            "sidebar": self.sidebar(page),
            "content": content,
            "pager": self.pager(page) if not is_home else "",
            "toc": rail,
            "svg_defs": self.svg_defs,
            "scripts": "\n".join(scripts),
            "commit": commit_html,
            "source_link": esc(f'{self.site["repo"]}/blob/{self.site.get("branch", "main")}/{src_rel}'),
            "source_path": esc(src_rel),
        }

        def sub(m):
            k = m.group(1)
            if k not in values:
                raise KeyError(f"template placeholder {{{{{k}}}}} has no value")
            return values[k]

        out_html = re.sub(r"\{\{(\w+)\}\}", sub, self.template)
        dest = self.out / page
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(out_html, encoding="utf-8")

        # search sections
        entries = []
        merged = {}
        for s in rw.sections:
            key = (s["heading"], s["anchor"])
            if key in merged:
                continue
            merged[key] = True
            txt = norm_space("".join(s["text"]))
            if not txt and not s["heading"]:
                continue
            entries.append({
                "title": title or self.site["title"],
                "section": s["heading"],
                "url": page + (f'#{s["anchor"]}' if s["anchor"] else ""),
                "text": txt[:1200],
            })
        self.wrapped[page] = {"ids": ids, "links": rw.links, "search": entries, "counts": rw.counts}

    # -- verbatim pages
    def check_verbatim(self, path):
        text = (self.out / path).read_text(encoding="utf-8", errors="replace")
        if path.endswith(".css"):
            for u in css_urls(text):
                if re.match(r"^(https?:)?//", u):
                    self.problem(f"{path}: external resource url({u})", "external")
                else:
                    self.check_link(path, u, "css", product_paths=True)
            return
        sc = VerbatimScanner()
        sc.feed(text)
        sc.close()
        for tag, attr, v, line in sc.resources:
            self.problem(f"{path}:{line}: external resource <{tag} {attr}={v!r}>", "external")
        for tag, attr, v, line in sc.links:
            self.check_link(path, v, f"{path}:{line}", product_paths=True)
        for style in sc.styles:
            for u in css_urls(style):
                if re.match(r"^(https?:)?//", u):
                    self.problem(f"{path}: external resource url({u}) in a style", "external")

    # -- link checking
    def check_link(self, page, url, where, product_paths=False):
        self.link_count += 1
        url = url.strip()
        if not url or url == "#":
            return
        if url.startswith("//"):
            self.problem(f"{where}: protocol-relative URL {url!r}", "links")
            return
        m = EXTERNAL_RE.match(url)
        if m:
            scheme = m.group(0).lower()
            if scheme in ("http:", "https:", "mailto:", "data:", "tel:"):
                self.external_count += 1
                return
            self.problem(f"{where}: unsupported URL scheme in {url!r}", "links")
            return
        if url.startswith("/"):
            if product_paths:
                self.product_count += 1
                return
            self.problem(f"{where}: absolute path {url!r} (use a relative link)", "links")
            return
        path, _, frag = url.partition("#")
        path = path.split("?")[0]
        if not path:
            target = page
        else:
            target = posixpath.normpath(posixpath.join(posixpath.dirname(page), path))
            if path.endswith("/"):
                target = posixpath.join(target, "index.html")
        if target.startswith("../") or target == "..":
            self.problem(f"{where}: link {url!r} leaves the site", "links")
            return
        if target not in self.files:
            if posixpath.join(target, "index.html") in self.files:
                target = posixpath.join(target, "index.html")
            else:
                self.broken += 1
                if target in self.missing_nav:
                    self.problem(f"{where.split(':')[0]} → {target}", "to-missing")
                elif re.match(r"design/mockups/(ui|term)/[^/]+\.html$", target):
                    self.problem(target, "mockups")
                elif target.endswith((".webp", ".png")) and "/img/" in "/" + target:
                    self.problem(target, "images")
                else:
                    self.problem(f"{where}: broken link {url!r} (no file {target})", "links")
                return
        if frag and target in self.wrapped and frag not in self.wrapped[target]["ids"]:
            self.bad_anchor += 1
            self.problem(f"{where}: link {url!r}: no id #{frag} in {target}", "anchors-bad")

    # -- main
    def build(self):
        src, out = self.src, self.out
        if not src.is_dir():
            sys.exit(f"build: source directory {src} not found")
        self.prepare_out()
        nav_paths = [p["path"] for p in self.pages]
        self.available = {p for p in nav_paths if (src / p).is_file()}
        missing = [p for p in nav_paths if p not in self.available]
        self.missing_nav = set(missing)
        for p in missing:
            self.problem(p, "missing")
        if not (src / self.home).is_file():
            self.problem(self.home, "missing")
        wrapped_paths = [self.home] if (src / self.home).is_file() else []
        wrapped_paths += [p for p in nav_paths if p in self.available]
        self.available.add(self.home)

        # copy everything else verbatim
        copied, copied_bytes = 0, 0
        skip_dirs = {"tools"}
        out_res = out.resolve()
        for dirpath, dirnames, filenames in os.walk(src):
            d = Path(dirpath)
            rel_dir = d.relative_to(src).as_posix()
            dirnames[:] = sorted(n for n in dirnames
                                 if not (rel_dir == "." and n in skip_dirs) and (d / n).resolve() != out_res)
            for name in sorted(filenames):
                rp = (d / name).relative_to(src).as_posix()
                self.files.add(rp)
                if rp in wrapped_paths:
                    continue
                dest = out / rp
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(d / name, dest)
                copied += 1
                copied_bytes += (d / name).stat().st_size
        self.files.update(["search-index.json", "search-index.js", ".nojekyll"])

        info = {p["path"]: p for p in self.pages}
        for page in wrapped_paths:
            self.wrap(page, info.get(page))

        # search index, in nav order
        index = []
        for page in wrapped_paths:
            index.extend(self.wrapped[page]["search"])
        data = json.dumps(index, ensure_ascii=False, separators=(",", ":"))
        (out / "search-index.json").write_text(data + "\n", encoding="utf-8")
        (out / "search-index.js").write_text("window.REPOCTL_SEARCH=" + data + ";\n", encoding="utf-8")
        (out / ".nojekyll").write_text("", encoding="utf-8")

        # link check
        self.link_count = self.external_count = self.product_count = 0
        self.broken = self.bad_anchor = 0
        for page in wrapped_paths:
            for tag, attr, v, line in self.wrapped[page]["links"]:
                self.check_link(page, v, f"{page}:{line}")
        verbatim_checked = 0
        prefixes = tuple(self.nav.get("check_verbatim", []))
        for f in sorted(self.files):
            if f in self.wrapped or not f.startswith(prefixes):
                continue
            if f.endswith((".html", ".css")):
                self.check_verbatim(f)
                verbatim_checked += 1

        totals = {k: sum(self.wrapped[p]["counts"][k] for p in self.wrapped) for k in
                  ("diagrams", "explainers", "tables", "code", "term", "images")}
        return {
            "wrapped": len(wrapped_paths), "missing": missing, "copied": copied,
            "copied_bytes": copied_bytes, "search": len(index), "verbatim": verbatim_checked,
            "totals": totals,
        }

    def prepare_out(self):
        out = self.out.resolve()
        src = self.src.resolve()
        bad = {Path("/"), Path.home().resolve(), REPO, src}
        if out in bad or src.is_relative_to(out):
            sys.exit(f"build: refusing to use {self.out} as output directory")
        if out.exists():
            if not out.is_dir():
                sys.exit(f"build: {self.out} is not a directory")
            if any(out.iterdir()) and not (out / MARKER).exists():
                sys.exit(f"build: {self.out} is not empty and was not made by this build; remove it first")
            shutil.rmtree(out)
        out.mkdir(parents=True)
        (out / MARKER).write_text("output of tools/site/build.py\n", encoding="utf-8")
        self.files.add(MARKER)


def human_bytes(n):
    for unit in ("B", "KiB", "MiB", "GiB"):
        if n < 1024 or unit == "GiB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n /= 1024


def main(argv=None):
    ap = argparse.ArgumentParser(description="Build the repoctl design site.")
    ap.add_argument("--src", default=None, help="source directory (default: docs/ of the repo)")
    ap.add_argument("--out", default=None, help="output directory (default: _site/ of the repo)")
    ap.add_argument("--strict", action="store_true",
                    help="fail on missing nav pages, broken links, missing anchors and external resources")
    args = ap.parse_args(argv)
    src = Path(args.src) if args.src else REPO / "docs"
    out = Path(args.out) if args.out else REPO / "_site"
    site = Site(src, out, args.strict)
    r = site.build()

    t = r["totals"]
    print("repoctl site build")
    print(f"  source    {src}")
    print(f"  output    {out}")
    print(f"  commit    {site.commit or '(unknown)'}")
    print(f"  pages     {r['wrapped']} wrapped"
          + (f", {len(r['missing'])} missing" if r["missing"] else ""))
    print(f"  content   {t['diagrams']} diagrams ({t['explainers']} explainers), {t['tables']} tables, "
          f"{t['code']} code blocks, {t['term']} terminal blocks, {t['images']} images")
    print(f"  copied    {r['copied']} files ({human_bytes(r['copied_bytes'])})")
    print(f"  search    {r['search']} entries")
    print(f"  links     {site.link_count} checked ({site.external_count} external, "
          f"{site.product_count} product paths ignored), {site.broken} broken, {site.bad_anchor} bad anchors; "
          f"{r['verbatim']} mockup files scanned")
    if site.warnings:
        print(f"\nwarnings ({len(site.warnings)}):")
        for w in site.warnings:
            print(f"  - {w}")
    if site.errors:
        label = "errors" if args.strict else "problems (errors with --strict)"
        print(f"\n{label} ({len(site.errors)}):")
        groups = {}
        for kind, msg in site.errors:
            groups.setdefault(kind, []).append(msg)
        titles = [("missing", "missing nav pages"), ("anchors", "missing fixed anchors"),
                  ("links", "broken links"), ("anchors-bad", "links to ids that do not exist"),
                  ("to-missing", "links to nav pages that do not exist yet"),
                  ("mockups", "missing mockup pages"),
                  ("images", "missing images (run tools/site/shots.mjs)"),
                  ("external", "external resources"), ("other", "other")]
        for kind, title in titles:
            msgs = groups.get(kind)
            if not msgs:
                continue
            print(f"  {title} ({len(msgs)}):")
            if kind in ("missing", "mockups", "images"):
                names = sorted(set(msgs), key=msgs.index)
                if kind != "missing":
                    names = [posixpath.basename(n) if kind == "images" else n.replace("design/mockups/", "") for n in names]
                for i in range(0, len(names), 6):
                    print("    " + ", ".join(names[i:i + 6]))
            elif kind == "to-missing":
                per = {}
                for m in msgs:
                    src_page, target = m.split(" → ")
                    per.setdefault(src_page, set()).add(target)
                for src_page in sorted(per):
                    print(f"    {src_page} → " + ", ".join(sorted(per[src_page])))
            else:
                for m in msgs:
                    print(f"    {m}")
    print()
    if site.errors and args.strict:
        print(f"FAILED: {len(site.errors)} errors")
        return 1
    n = len(site.errors) + len(site.warnings)
    print("OK" + (f" ({len(site.errors)} problems, {len(site.warnings)} warnings)" if n else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
