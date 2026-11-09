// shell.js: the only page script of repoctl ui besides htmx, its SSE extension and graph.js.
// Each part works alone. Pages render and work without it (menus are <details>, links are links).
// The Content-Security-Policy of the server refuses inline scripts and handlers, so every
// behaviour hangs on a data attribute here.
(function () {
  "use strict";
  var doc = document, body = doc.body, root = doc.documentElement;

  // 1. Theme: auto -> light -> dark. The cookie repoctl_theme keeps it, so the server
  //    renders <html data-theme> with no flash.
  doc.addEventListener("click", function (e) {
    var t = e.target.closest("[data-theme-toggle]");
    if (!t) return;
    var cur = root.dataset.theme || "auto";
    var next = { auto: "light", light: "dark", dark: "auto" }[cur];
    if (next === "auto") delete root.dataset.theme; else root.dataset.theme = next;
    try { doc.cookie = "repoctl_theme=" + next + ";path=/;samesite=strict"; } catch (_) {}
    t.setAttribute("aria-label", "Theme: " + next);
  });

  // 2. Copy: <button class="copy" data-copy="full text">. The full id goes to the clipboard.
  doc.addEventListener("click", function (e) {
    var b = e.target.closest("[data-copy]");
    if (!b || !navigator.clipboard) return;
    e.preventDefault();
    navigator.clipboard.writeText(b.dataset.copy).then(function () {
      b.classList.add("done");
      setTimeout(function () { b.classList.remove("done"); }, 1200);
    });
  });

  // 3. Popovers (set menu, "more" menus, "?" help, CLI toggle) are <details>.
  //    One is open at a time. A click outside or Esc closes it.
  var POP = "details.set-menu,details.more,details.help,details.cli-toggle";
  function closePops(except) {
    doc.querySelectorAll(POP).forEach(function (d) { if (d !== except && d.open) d.open = false; });
  }
  function keepInView(d) {
    var pop = d.querySelector(":scope > .more-pop, :scope > .cli-pop, :scope > .help-pop, :scope > .set-pop");
    if (!pop) return;
    pop.style.translate = "";
    var r = pop.getBoundingClientRect(), vw = doc.documentElement.clientWidth, dx = 0;
    if (r.left < 8) dx = 8 - r.left;
    else if (r.right > vw - 8) dx = Math.max(8 - r.left, vw - 8 - r.right);
    if (dx) pop.style.translate = Math.round(dx) + "px 0";
  }
  doc.addEventListener("toggle", function (e) {
    if (e.target.matches && e.target.matches(POP) && e.target.open) {
      closePops(e.target);
      keepInView(e.target);
    }
  }, true);
  doc.addEventListener("click", function (e) {
    if (!e.target.closest(POP)) closePops(null);
  });

  // 4. Navigation drawer below 760 px. The toggle is a link to #nav, so :target opens it without script.
  var nav = doc.getElementById("nav"), tog = doc.querySelector(".nav-toggle");
  function setNav(open) {
    if (open) body.dataset.nav = "open"; else delete body.dataset.nav;
    if (tog) tog.setAttribute("aria-expanded", String(open));
    if (open && nav) { var a = nav.querySelector("[aria-current]") || nav.querySelector("a"); if (a) a.focus(); }
  }
  if (tog) tog.addEventListener("click", function (e) { e.preventDefault(); setNav(body.dataset.nav !== "open"); });
  doc.addEventListener("click", function (e) { if (e.target.closest("[data-nav-close]")) setNav(false); });

  // 5. Keyboard help, toasts, reload buttons.
  function help() { var h = doc.getElementById("help"); if (h && h.showModal && !h.open) h.showModal(); }
  doc.addEventListener("click", function (e) {
    if (e.target.closest("[data-help]")) { e.preventDefault(); help(); }
    if (e.target.closest("[data-reload]")) { e.preventDefault(); location.reload(); }
    var x = e.target.closest(".toast .x");
    if (x) x.closest(".toast").remove();
    var dc = e.target.closest("[data-drawer-close]");
    if (dc) { var dr = dc.closest("#drawer"); if (dr) { dr.innerHTML = ""; dr.hidden = true; } }
    var c = e.target.closest("[data-close-dialog]");
    if (c) { var d = c.closest("dialog"); if (d) d.close(); }
  });
  // Ok and info toasts close after 6 s; error toasts stay. At most 3 show.
  function tidyToasts() {
    var box = doc.getElementById("toasts");
    if (!box) return;
    var all = box.querySelectorAll(".toast");
    for (var i = 3; i < all.length; i++) all[i].remove();
    all.forEach(function (t) {
      if (t.dataset.timed || t.classList.contains("err")) return;
      t.dataset.timed = "1";
      setTimeout(function () { t.remove(); }, 6000);
    });
  }

  // 6. Dialogs: the server swaps a <dialog class="dlg"> into #modal; it opens as a modal, so the
  //    page behind is inert. Closing it empties #modal.
  function openDialogs() {
    var m = doc.getElementById("modal");
    if (!m) return;
    m.querySelectorAll("dialog").forEach(function (d) {
      if (!d.open && d.showModal) {
        d.showModal();
        d.addEventListener("close", function () { d.remove(); }, { once: true });
      }
    });
  }
  body.addEventListener("htmx:afterSettle", function () { openDialogs(); tidyToasts(); });
  // A typed confirmation: the submit button of a form with data-confirm-value stays off until
  // every [data-must] input holds its value.
  doc.addEventListener("input", function (e) {
    var f = e.target.closest("form[data-typed]");
    if (!f) return;
    var ok = true;
    f.querySelectorAll("[data-must]").forEach(function (i) { if (i.value !== i.dataset.must) ok = false; });
    f.querySelectorAll("[data-min]").forEach(function (i) { if (i.value.trim().length < +i.dataset.min) ok = false; });
    f.querySelectorAll("button[type=submit]").forEach(function (b) { b.disabled = !ok; });
  });

  // 7. Keys: g + letter goes to the sidebar link with that data-key; / jump; ? help; t next target.
  //    Ignored in inputs and with Ctrl, Alt or Meta. No key starts a build, a cancel or a publish.
  var pending = null;
  doc.addEventListener("keydown", function (e) {
    if (e.key === "Escape") { closePops(null); if (body.dataset.nav === "open") setNav(false); }
    if (e.ctrlKey || e.altKey || e.metaKey) return;
    var tag = (e.target.tagName || "").toLowerCase();
    if (tag === "input" || tag === "textarea" || tag === "select" || e.target.isContentEditable) {
      if (e.key === "Escape") e.target.blur();
      return;
    }
    if (pending) {
      clearTimeout(pending); pending = null;
      var link = doc.querySelector('.sidebar [data-key="' + e.key + '"]');
      if (link && link.getAttribute("href") !== "#") location.href = link.href;
      return;
    }
    if (e.key === "g") { pending = setTimeout(function () { pending = null; }, 1200); return; }
    if (e.key === "/") { var j = doc.querySelector(".jump input"); if (j) { e.preventDefault(); j.focus(); } return; }
    if (e.key === "?") { e.preventDefault(); help(); return; }
    if (e.key === "t") { var n = doc.querySelector("[data-next-target]"); if (n) location.href = n.href; }
  });

  // 8. Live updates. htmx-ext-sse fires htmx:sseOpen, htmx:sseMessage and htmx:sseError.
  //    After 30 s without a message (2 missed heartbeats) or an error, the page is marked offline.
  var last = Date.now(), down = false;
  function mark(d, why) {
    if (d === down) return;
    down = d;
    if (d) body.dataset.sse = "down"; else delete body.dataset.sse;
    var w = doc.querySelector(".offline [data-why]");
    if (w && why) w.textContent = why;
    var t = doc.querySelector(".live .t");
    if (t) t.textContent = d ? "offline" : "live";
  }
  body.addEventListener("htmx:sseOpen", function () { last = Date.now(); mark(false); });
  body.addEventListener("htmx:sseMessage", function (e) {
    last = Date.now();
    var type = e.detail && e.detail.type;
    if (type === "bye") mark(true, "The repoctl ui server does not answer. It stopped.");
    if (type === "reload") location.reload();
    if (type === "toast") tidyToasts();
  });
  body.addEventListener("htmx:sseError", function () {
    // One ping tells "server stopped" from "network problem" and from "session ended".
    fetch("/-/ping", { credentials: "same-origin" }).then(function (r) {
      if (r.status === 401) location.href = "/-/session?next=" + encodeURIComponent(location.pathname + location.search);
      else mark(true, "The connection to repoctl ui was lost. The page shows old data.");
    }).catch(function () {
      mark(true, "The repoctl ui server does not answer. It stopped.");
    });
  });
  if (window.htmx && body.hasAttribute("sse-connect")) {
    setInterval(function () { if (Date.now() - last > 30000) mark(true, "No update for 30 s. The page shows old data."); }, 5000);
    // Elapsed timers tick in the browser; the server sends only state changes.
    setInterval(function () {
      doc.querySelectorAll("[data-since]").forEach(function (t) {
        var s = Math.floor((Date.now() - Date.parse(t.dataset.since)) / 1000);
        if (s < 0) return;
        t.textContent = s < 3600 ? Math.floor(s / 60) + "m" + String(s % 60).padStart(2, "0") + "s"
          : Math.floor(s / 3600) + "h" + String(Math.floor(s / 60) % 60).padStart(2, "0") + "m";
      });
    }, 1000);
  }
  // A log view that follows stays at its end when lines arrive.
  body.addEventListener("htmx:sseMessage", function (e) {
    var v = doc.querySelector(".logview[data-follow]");
    if (v && e.detail && String(e.detail.type).indexOf("log:") === 0) v.scrollTop = v.scrollHeight;
  });
  openDialogs();
  tidyToasts();
})();
