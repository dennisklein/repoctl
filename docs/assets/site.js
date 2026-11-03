/* repoctl design record: site script. No dependencies; highlight.js is optional. */
(function () {
  "use strict";

  var doc = document;
  var root = doc.documentElement;
  var base = root.getAttribute("data-root") || "";
  var reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var isMac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");

  function $(sel, ctx) { return (ctx || doc).querySelector(sel); }
  function $$(sel, ctx) { return Array.prototype.slice.call((ctx || doc).querySelectorAll(sel)); }
  function el(tag, attrs, text) {
    var e = doc.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function (k) { e.setAttribute(k, attrs[k]); });
    if (text != null) e.textContent = text;
    return e;
  }
  function store(key, value) {
    try {
      if (value === undefined) return localStorage.getItem(key);
      if (value === null) localStorage.removeItem(key); else localStorage.setItem(key, value);
    } catch (e) { /* storage blocked: the choice lasts for this page only */ }
    return null;
  }

  /* ---------- theme: auto, light, dark ---------- */
  var themeButton = $("[data-theme-toggle]");
  function applyTheme(choice) {
    if (choice === "light" || choice === "dark") root.setAttribute("data-theme", choice);
    else root.removeAttribute("data-theme");
    if (themeButton) {
      themeButton.setAttribute("data-choice", choice);
      var label = "Colour theme: " + choice;
      themeButton.setAttribute("aria-label", label);
      themeButton.setAttribute("title", label);
    }
  }
  var themeChoice = store("repoctl-theme") || "auto";
  if (["auto", "light", "dark"].indexOf(themeChoice) < 0) themeChoice = "auto";
  applyTheme(themeChoice);
  if (themeButton) {
    themeButton.addEventListener("click", function () {
      var order = ["auto", "light", "dark"];
      themeChoice = order[(order.indexOf(themeChoice) + 1) % order.length];
      applyTheme(themeChoice);
      store("repoctl-theme", themeChoice === "auto" ? null : themeChoice);
    });
  }

  /* ---------- sidebar drawer ---------- */
  var body = doc.body;
  var toggle = $(".nav-toggle");
  var sidebar = $("#sidebar");
  var scrim = $(".scrim");
  function setDrawer(open) {
    body.classList.toggle("nav-open", open);
    if (toggle) {
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
      toggle.setAttribute("aria-label", open ? "Close the chapter list" : "Open the chapter list");
    }
    if (scrim) scrim.hidden = !open;
    if (open) {
      var cur = $('[aria-current="page"]', sidebar) || $("a", sidebar);
      if (cur) setTimeout(function () { cur.focus(); }, 30);
    } else if (toggle && sidebar && sidebar.contains(doc.activeElement)) {
      toggle.focus();
    }
  }
  if (toggle && sidebar) {
    toggle.addEventListener("click", function () { setDrawer(!body.classList.contains("nav-open")); });
    $$("[data-nav-close]").forEach(function (b) { b.addEventListener("click", function () { setDrawer(false); }); });
    sidebar.addEventListener("click", function (e) { if (e.target.closest("a")) setDrawer(false); });
    doc.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && body.classList.contains("nav-open")) setDrawer(false);
    });
  }
  // keep the current chapter visible in a long sidebar
  if (sidebar) {
    var current = $('.nav-link[aria-current="page"]', sidebar);
    if (current) {
      var r = current.getBoundingClientRect(), s = sidebar.getBoundingClientRect();
      if (r.bottom > s.bottom - 40) sidebar.scrollTop += r.top - s.top - s.height / 3;
    }
  }

  /* ---------- "On this page" scrollspy ---------- */
  var tocLinks = $$(".toc nav a[href^='#']").filter(function (a) { return a.getAttribute("href").length > 1; });
  if (tocLinks.length) {
    var targets = tocLinks.map(function (a) {
      return doc.getElementById(decodeURIComponent(a.getAttribute("href").slice(1)));
    });
    var active = null, ticking = false;
    var spy = function () {
      ticking = false;
      var line = (parseFloat(getComputedStyle(root).getPropertyValue("--header-h")) || 60) + 90;
      var idx = -1;
      for (var i = 0; i < targets.length; i++) {
        if (targets[i] && targets[i].getBoundingClientRect().top <= line) idx = i;
      }
      // at the end of the page, mark the last heading that is on screen
      if (window.innerHeight + window.scrollY >= doc.documentElement.scrollHeight - 4) {
        for (var j = targets.length - 1; j >= 0; j--) {
          if (targets[j] && targets[j].getBoundingClientRect().top < window.innerHeight) { idx = Math.max(idx, j); break; }
        }
      }
      var link = idx >= 0 ? tocLinks[idx] : null;
      if (link === active) return;
      if (active) active.classList.remove("is-active");
      active = link;
      if (link) {
        link.classList.add("is-active");
        var box = link.closest(".toc");
        if (box) {
          var lr = link.getBoundingClientRect(), br = box.getBoundingClientRect();
          if (lr.top < br.top + 40 || lr.bottom > br.bottom - 40) box.scrollTop += lr.top - br.top - br.height / 2;
        }
      }
    };
    window.addEventListener("scroll", function () {
      if (!ticking) { ticking = true; requestAnimationFrame(spy); }
    }, { passive: true });
    window.addEventListener("resize", spy);
    spy();
  }

  /* ---------- copy buttons ---------- */
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    return new Promise(function (resolve, reject) {
      var ta = el("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      body.appendChild(ta);
      ta.select();
      try { doc.execCommand("copy") ? resolve() : reject(); } catch (e) { reject(e); }
      body.removeChild(ta);
    });
  }
  $$(".copy-button").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var box = btn.closest(".codeblock, .term-window");
      var pre = box && $("pre", box);
      if (!pre) return;
      var text = pre.textContent.replace(/\n$/, "");
      if (btn.getAttribute("data-copy") === "term") {
        var cmds = text.split("\n").filter(function (l) { return /^\$ /.test(l); })
          .map(function (l) { return l.slice(2); });
        if (cmds.length) text = cmds.join("\n");
      }
      copyText(text).then(function () {
        btn.textContent = "Copied";
        btn.classList.add("is-done");
        setTimeout(function () { btn.textContent = "Copy"; btn.classList.remove("is-done"); }, 1600);
      }, function () {
        btn.textContent = "Select and copy";
      });
    });
  });

  /* ---------- syntax colours (highlight.js, if present) ---------- */
  if (window.hljs) {
    try { window.hljs.configure({ ignoreUnescapedHTML: true }); } catch (e) { /* old version */ }
    $$(".codeblock pre code[class*='language-']").forEach(function (code) {
      if (/language-(plaintext|text|none)\b/.test(code.className)) return;
      var m = code.className.match(/language-([\w-]+)/);
      if (m && !window.hljs.getLanguage(m[1])) return;
      try { window.hljs.highlightElement(code); } catch (e) { /* leave plain */ }
    });
  }

  /* ---------- terminal colours ---------- */
  function termLine(line) {
    // returns a DocumentFragment with spans; text stays the same
    var frag = doc.createDocumentFragment();
    function add(text, cls) {
      if (!text) return;
      if (!cls) { frag.appendChild(doc.createTextNode(text)); return; }
      frag.appendChild(el("span", { "class": cls }, text));
    }
    var m;
    if ((m = line.match(/^(\s*)(\$)( .*)?$/))) {
      add(m[1]); add(m[2], "tl-prompt"); add(m[3] || "", "tl-cmd");
      return frag;
    }
    if ((m = line.match(/^(\s*)(#(?: .*)?)$/))) { add(m[1]); add(m[2], "tl-dim"); return frag; }
    if ((m = line.match(/^(.*?)(\berror:)(.*)$/))) { add(m[1]); add(m[2], "tl-err"); add(m[3]); return frag; }
    if ((m = line.match(/^(.*?)(\bhint:)(.*)$/))) { add(m[1]); add(m[2], "tl-hint"); add(m[3]); return frag; }
    if ((m = line.match(/^(\s*)(!)(\s.*|)$/))) { add(m[1]); add(m[2], "tl-warn"); add(m[3]); return frag; }
    var parts = line.split(/([✓✗])/);
    if (parts.length > 1) {
      parts.forEach(function (p) {
        if (p === "✓") add(p, "tl-ok"); else if (p === "✗") add(p, "tl-fail"); else add(p);
      });
      return frag;
    }
    add(line);
    return frag;
  }
  $$("pre.term").forEach(function (pre) {
    if (pre.children.length) return; // the author marked it up already
    var text = pre.textContent;
    var lines = text.split("\n");
    var out = doc.createDocumentFragment();
    lines.forEach(function (line, i) {
      out.appendChild(termLine(line));
      if (i < lines.length - 1) out.appendChild(doc.createTextNode("\n"));
    });
    pre.textContent = "";
    pre.appendChild(out);
  });

  /* ---------- scroll fades for tables, diagrams, code and terminals ---------- */
  var wraps = $$(".table-wrap, .diagram-body, .codeblock pre, pre.term");
  function fades(w) {
    var max = w.scrollWidth - w.clientWidth;
    var over = max > 2;
    w.classList.toggle("fade-l", over && w.scrollLeft > 2);
    w.classList.toggle("fade-r", over && w.scrollLeft < max - 2);
    if (w.classList.contains("diagram-body")) return;
    // a region that scrolls sideways must be reachable with the keyboard
    if (over) {
      if (!w.hasAttribute("tabindex")) {
        w.setAttribute("tabindex", "0"); w.setAttribute("role", "region");
        w.setAttribute("aria-label", (w.tagName === "PRE" ? "Code" : "Table") + ", scrolls sideways");
      }
    } else if (w.getAttribute("role") === "region") {
      w.removeAttribute("tabindex"); w.removeAttribute("role"); w.removeAttribute("aria-label");
    }
  }
  wraps.forEach(function (w) {
    fades(w);
    w.addEventListener("scroll", function () { fades(w); }, { passive: true });
  });

  /* ---------- code blocks: the language label never covers the first line ---------- */
  var codeBlocks = $$(".codeblock");
  function firstLineRight(pre) {
    var walker = doc.createTreeWalker(pre, NodeFilter.SHOW_TEXT, null);
    var range = doc.createRange(), started = false, n, right = 0;
    while ((n = walker.nextNode())) {
      if (!started) { range.setStart(n, 0); started = true; }
      var i = n.data.indexOf("\n");
      if (i >= 0) { range.setEnd(n, i); break; }
      range.setEnd(n, n.data.length);
    }
    if (!started) return 0;
    Array.prototype.forEach.call(range.getClientRects(), function (r) { right = Math.max(right, r.right); });
    return right;
  }
  function headRoom(b) {
    var pre = $("pre", b), head = $(".codeblock-head", b);
    if (!pre || !head) return;
    var left = Infinity;
    $$(".codeblock-lang, .copy-button", head).forEach(function (x) {
      var cs = getComputedStyle(x);
      // only what shows at rest: the label, or the copy button on a touch screen
      if (cs.display === "none" || (x.classList.contains("copy-button") && parseFloat(cs.opacity) < 0.5)) return;
      left = Math.min(left, x.getBoundingClientRect().left);
    });
    b.classList.toggle("has-head", left < Infinity && firstLineRight(pre) + pre.scrollLeft > left - 8);
  }
  function layoutCode() { codeBlocks.forEach(headRoom); wraps.forEach(fades); }
  var codeTick = false;
  if (codeBlocks.length || wraps.length) {
    layoutCode();
    if (doc.fonts && doc.fonts.ready) doc.fonts.ready.then(layoutCode);
    window.addEventListener("resize", function () {
      if (!codeTick) { codeTick = true; requestAnimationFrame(function () { codeTick = false; layoutCode(); }); }
    });
  }

  /* ---------- screenshots: mark images that are cropped ---------- */
  function cropCheck(img) {
    var a = img.closest(".shot-link");
    if (!a || !img.naturalWidth) return;
    var full = img.clientWidth * img.naturalHeight / img.naturalWidth;
    a.classList.toggle("is-cropped", full > img.clientHeight + 4);
  }
  function missing(img) {
    // a screenshot that is not generated yet: show a quiet placeholder
    var box = el("span", { "class": "shot-missing", role: "img", "aria-label": img.alt || "Screenshot" });
    box.appendChild(el("span", null, "Screenshot not generated yet"));
    img.replaceWith(box);
  }
  $$("figure.shot img, .gallery-thumb img").forEach(function (img) {
    if (img.complete && img.naturalWidth) cropCheck(img);
    else if (img.complete && img.getAttribute("src")) missing(img);
    else {
      img.addEventListener("load", function () { cropCheck(img); });
      img.addEventListener("error", function () { missing(img); });
    }
  });
  window.addEventListener("resize", function () { $$("figure.shot img").forEach(cropCheck); });

  /* ---------- step explainers ---------- */
  function parseFrames(spec) {
    var set = {};
    String(spec).split(",").forEach(function (part) {
      part = part.trim();
      if (!part) return;
      var r = part.split("-");
      var a = parseInt(r[0], 10), b = r.length > 1 ? parseInt(r[1], 10) : a;
      for (var i = a; i <= b; i++) set[i] = true;
    });
    return set;
  }
  var icons = {
    back: '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M7.8 2 3.6 6l4.2 4z"/></svg>',
    next: '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M4.2 2 8.4 6l-4.2 4z"/></svg>',
    play: '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M3 1.8v8.4L10 6z"/></svg>',
    pause: '<svg viewBox="0 0 12 12" aria-hidden="true"><path d="M2.8 2h2.3v8H2.8zM6.9 2h2.3v8H6.9z"/></svg>'
  };
  $$("figure.explainer").forEach(function (fig, n) {
    var data = $("script.steps", fig);
    var steps = [];
    try { steps = JSON.parse(data ? data.textContent : "[]"); } catch (e) { steps = []; }
    if (!Array.isArray(steps) || !steps.length) return;
    var total = steps.length;
    var items = $$("[data-f]", fig).map(function (node) { return { node: node, frames: parseFrames(node.getAttribute("data-f")) }; });
    var bodyEl = $(".diagram-body", fig) || $("svg", fig);
    var ui = el("div", { "class": "explainer-ui" });
    var caption = el("p", { "class": "explainer-caption", "aria-live": "polite", id: "ex-cap-" + n });
    var controls = el("div", { "class": "ex-controls" });
    var back = el("button", { type: "button", "class": "ex-btn ex-back", "aria-label": "Previous step" });
    back.innerHTML = icons.back + "<span>Back</span>";
    var play = el("button", { type: "button", "class": "ex-btn ex-play", "aria-label": "Play the steps" });
    play.innerHTML = icons.play + "<span>Play</span>";
    var next = el("button", { type: "button", "class": "ex-btn ex-next", "aria-label": "Next step" });
    next.innerHTML = "<span>Next</span>" + icons.next;
    controls.appendChild(back); controls.appendChild(play); controls.appendChild(next);
    var dots = el("div", { "class": "ex-dots", role: "group", "aria-label": "Steps" });
    var dotButtons = [];
    for (var i = 0; i < total; i++) {
      var d = el("button", { type: "button", "class": "ex-dot", "aria-label": "Step " + (i + 1) + " of " + total });
      d.setAttribute("data-i", String(i));
      dots.appendChild(d);
      dotButtons.push(d);
    }
    var count = el("span", { "class": "ex-count", "aria-hidden": "true" });
    ui.appendChild(caption); ui.appendChild(controls); ui.appendChild(dots); ui.appendChild(count);
    if (bodyEl && bodyEl.parentNode === fig) bodyEl.insertAdjacentElement("afterend", ui); else fig.insertBefore(ui, $("figcaption", fig));
    fig.setAttribute("tabindex", "0");
    fig.setAttribute("aria-describedby", caption.id);

    var frame = 0, timer = null;
    function show(f) {
      frame = Math.max(0, Math.min(total - 1, f));
      items.forEach(function (it) { it.node.classList.toggle("ex-on", !!it.frames[frame]); });
      caption.textContent = String(steps[frame]);
      count.textContent = (frame + 1) + " / " + total;
      back.disabled = frame === 0;
      next.disabled = frame === total - 1;
      dotButtons.forEach(function (b, k) {
        if (k === frame) b.setAttribute("aria-current", "step"); else b.removeAttribute("aria-current");
      });
    }
    function stop() {
      if (timer) { clearInterval(timer); timer = null; }
      play.innerHTML = icons.play + "<span>Play</span>";
      play.setAttribute("aria-label", "Play the steps");
    }
    function start() {
      if (frame >= total - 1) show(0);
      play.innerHTML = icons.pause + "<span>Pause</span>";
      play.setAttribute("aria-label", "Pause");
      timer = setInterval(function () {
        if (frame >= total - 1) { stop(); return; }
        show(frame + 1);
      }, reduceMotion ? 4200 : 3200);
    }
    back.addEventListener("click", function () { stop(); show(frame - 1); });
    next.addEventListener("click", function () { stop(); show(frame + 1); });
    play.addEventListener("click", function () { if (timer) stop(); else start(); });
    dots.addEventListener("click", function (e) {
      var b = e.target.closest(".ex-dot");
      if (b) { stop(); show(parseInt(b.getAttribute("data-i"), 10)); }
    });
    fig.addEventListener("keydown", function (e) {
      if (e.target.closest("input, textarea")) return;
      if (e.key === "ArrowRight") { e.preventDefault(); stop(); show(frame + 1); }
      else if (e.key === "ArrowLeft") { e.preventDefault(); stop(); show(frame - 1); }
      else if (e.key === "Home") { e.preventDefault(); stop(); show(0); }
      else if (e.key === "End") { e.preventDefault(); stop(); show(total - 1); }
    });
    show(0);
  });

  /* ---------- search ---------- */
  var dialog = $("#search");
  var input = $("#search-input");
  var list = $("#search-results");
  var openers = $$("[data-search-open]");
  $$("[data-kbd-mod]").forEach(function (k) { k.textContent = isMac ? "⌘K" : "Ctrl K"; });
  var index = null, loading = null, results = [], selected = -1;
  var suggestions = ["publish order", "exit codes", "advisory codes", "repoctl.yaml", "signer probe", "mirror job"];

  function loadIndex() {
    if (index) return Promise.resolve(index);
    if (loading) return loading;
    loading = new Promise(function (resolve) {
      function viaScript() {
        if (window.REPOCTL_SEARCH) { resolve(window.REPOCTL_SEARCH); return; }
        var s = el("script", { src: base + "search-index.js" });
        s.onload = function () { resolve(window.REPOCTL_SEARCH || []); };
        s.onerror = function () { resolve([]); };
        body.appendChild(s);
      }
      if (location.protocol === "file:" || !window.fetch) { viaScript(); return; }
      fetch(base + "search-index.json").then(function (r) { return r.ok ? r.json() : Promise.reject(); })
        .then(resolve, viaScript);
    }).then(function (data) {
      index = data.map(function (d) {
        return { d: d, title: (d.title || "").toLowerCase(), section: (d.section || "").toLowerCase(), text: (d.text || "").toLowerCase() };
      });
      return index;
    });
    return loading;
  }
  function escapeHtml(s) {
    return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }
  function highlight(text, words) {
    var html = escapeHtml(text);
    words.slice().sort(function (a, b) { return b.length - a.length; }).forEach(function (w) {
      if (w.length < 2) return;
      var re = new RegExp("(" + escapeHtml(w).replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
      html = html.replace(/(<[^>]*>)|([^<]+)/g, function (m, tag, txt) { return tag || txt.replace(re, "<mark>$1</mark>"); });
    });
    return html;
  }
  function snippet(text, words) {
    if (!text) return "";
    var low = text.toLowerCase(), at = -1;
    for (var i = 0; i < words.length && at < 0; i++) at = low.indexOf(words[i]);
    if (at < 0) return text.slice(0, 150) + (text.length > 150 ? "…" : "");
    var start = Math.max(0, at - 60);
    if (start > 0) { var sp = text.indexOf(" ", start); if (sp > 0 && sp < at) start = sp + 1; }
    var end = Math.min(text.length, start + 170);
    return (start > 0 ? "…" : "") + text.slice(start, end) + (end < text.length ? "…" : "");
  }
  function score(e, words, phrase) {
    var s = 0;
    for (var i = 0; i < words.length; i++) {
      var w = words[i], hit = false, k;
      k = e.section.indexOf(w);
      if (k >= 0) { s += 10 + (k === 0 ? 6 : (/\W/.test(e.section.charAt(k - 1)) ? 3 : 0)); hit = true; }
      k = e.title.indexOf(w);
      if (k >= 0) { s += 6 + (k === 0 ? 2 : 0); hit = true; }
      k = e.text.indexOf(w);
      if (k >= 0) {
        hit = true;
        s += 2;
        var c = 0, p = k;
        while (p >= 0 && c < 6) { c++; p = e.text.indexOf(w, p + w.length); }
        s += c * 0.5;
        if (k === 0 || /\W/.test(e.text.charAt(k - 1))) s += 1;
      }
      if (!hit) return 0;
    }
    if (words.length > 1) {
      if (e.section.indexOf(phrase) >= 0) s += 16;
      else if (e.text.indexOf(phrase) >= 0) s += 6;
    }
    if (e.section === phrase) s += 12;
    if (!e.d.section) s += 1;
    return s;
  }
  function render(q) {
    list.innerHTML = "";
    selected = -1;
    input.removeAttribute("aria-activedescendant");
    var phrase = q.trim().toLowerCase();
    if (!phrase) {
      var empty = el("li", { "class": "search-empty", role: "presentation" });
      empty.appendChild(el("p", null, "Search the chapters, commands and rules."));
      var sug = el("div", { "class": "search-suggest" });
      suggestions.forEach(function (t) {
        var b = el("button", { type: "button" }, t);
        b.addEventListener("click", function () { input.value = t; run(); input.focus(); });
        sug.appendChild(b);
      });
      empty.appendChild(sug);
      list.appendChild(empty);
      results = [];
      return;
    }
    var words = phrase.split(/\s+/).filter(Boolean);
    results = index.map(function (e) { return { e: e, s: score(e, words, phrase) }; })
      .filter(function (r) { return r.s > 0; })
      .sort(function (a, b) { return b.s - a.s; })
      .slice(0, 24);
    if (!results.length) {
      list.appendChild(el("li", { "class": "search-empty", role: "presentation" }, "No results for “" + q.trim() + "”."));
      return;
    }
    results.forEach(function (r, i) {
      var d = r.e.d;
      var li = el("li", { role: "option", id: "sr-" + i, "aria-selected": "false" });
      var a = el("a", { href: base + d.url, tabindex: "-1" });
      a.innerHTML = '<span class="sr-path">' + escapeHtml(d.title) + (d.section ? " › <b>" + highlight(d.section, words) + "</b>" : "") + "</span>" +
        '<span class="sr-snippet">' + highlight(snippet(d.text, words), words) + "</span>";
      a.addEventListener("click", function () { dialog.close(); });
      li.appendChild(a);
      li.addEventListener("mousemove", function () { select(i, false); });
      list.appendChild(li);
    });
    select(0, false);
  }
  function select(i, scroll) {
    if (!results.length) return;
    var items = $$("li[role=option]", list);
    if (selected >= 0 && items[selected]) items[selected].setAttribute("aria-selected", "false");
    selected = (i + results.length) % results.length;
    var cur = items[selected];
    cur.setAttribute("aria-selected", "true");
    input.setAttribute("aria-activedescendant", cur.id);
    if (scroll !== false) cur.scrollIntoView({ block: "nearest" });
  }
  var pending = null;
  function run() {
    var q = input.value;
    loadIndex().then(function () { render(q); });
  }
  function openSearch() {
    if (!dialog || dialog.open) return;
    if (typeof dialog.showModal === "function") dialog.showModal(); else dialog.setAttribute("open", "");
    input.select();
    run();
  }
  if (dialog && input) {
    openers.forEach(function (b) { b.addEventListener("click", openSearch); });
    input.addEventListener("input", function () { clearTimeout(pending); pending = setTimeout(run, 50); });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Escape") { e.preventDefault(); dialog.close(); }
      else if (e.key === "ArrowDown") { e.preventDefault(); select(selected + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); select(selected - 1); }
      else if (e.key === "Enter") {
        e.preventDefault();
        var cur = $$("li[role=option]", list)[selected];
        if (cur) { var a = $("a", cur); dialog.close(); location.href = a.href; }
      }
    });
    dialog.addEventListener("click", function (e) { if (e.target === dialog) dialog.close(); });
    doc.addEventListener("keydown", function (e) {
      var t = e.target;
      var typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey) && !e.altKey) {
        e.preventDefault();
        if (dialog.open) dialog.close(); else openSearch();
      } else if (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        openSearch();
      }
    });
    // warm the index when the pointer comes near the button
    openers.forEach(function (b) { b.addEventListener("pointerenter", function () { loadIndex(); }, { once: true }); });
  }
})();
