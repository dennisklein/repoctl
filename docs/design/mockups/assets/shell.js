// shell.js: the only page script of repoctl ui besides htmx and its SSE extension.
// Each part works alone. Pages render and work without it (menus are <details>, links are links).
(function () {
  "use strict";
  var doc = document, body = doc.body, root = doc.documentElement;

  // 1. Theme: auto -> light -> dark. The product keeps it in the cookie repoctl_theme,
  //    so the server renders <html data-theme> with no flash.
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
  // An open popover stays inside the viewport: shift it when it crosses an edge.
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

  // 5. Keyboard help and toasts.
  function help() { var h = doc.getElementById("help"); if (h && h.showModal && !h.open) h.showModal(); }
  doc.addEventListener("click", function (e) {
    if (e.target.closest("[data-help]")) { e.preventDefault(); help(); }
    var x = e.target.closest(".toast .x");
    if (x) x.closest(".toast").remove();
  });

  // 6. Keys: g + letter goes to the sidebar link with that data-key; / jump; ? help; t next target.
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

  // 7. Live updates. htmx-ext-sse fires htmx:sseOpen, htmx:sseMessage and htmx:sseError.
  //    After 30 s without a message (2 missed heartbeats) or an error, the page is marked offline.
  var last = Date.now(), down = false;
  function mark(d, why) {
    if (d === down) return;
    down = d;
    if (d) body.dataset.sse = "down"; else delete body.dataset.sse;
    var w = doc.querySelector(".offline [data-why]");
    if (w && why) w.textContent = why;
    if (!d && window.htmx) doc.querySelectorAll("[data-live][hx-get]").forEach(function (el) { window.htmx.trigger(el, "refresh"); });
  }
  body.addEventListener("htmx:sseOpen", function () { last = Date.now(); mark(false); });
  body.addEventListener("htmx:sseMessage", function () { last = Date.now(); });
  body.addEventListener("htmx:sseError", function () {
    // One ping tells "server stopped" from "network problem" and from "session ended".
    fetch("/-/ping", { credentials: "same-origin" }).then(function (r) {
      if (r.status === 401) location.href = "/-/session?next=" + encodeURIComponent(location.pathname);
      else mark(true, "The connection to repoctl ui was lost. The page shows old data.");
    }).catch(function () {
      mark(true, "The repoctl ui server does not answer. It stopped, or the machine is not reachable.");
    });
  });
  if (window.htmx && body.hasAttribute("sse-connect")) {
    setInterval(function () { if (Date.now() - last > 30000) mark(true, "No update for 30 s. The page shows old data."); }, 5000);
    // Elapsed timers tick in the browser; the server sends only state changes.
    setInterval(function () {
      doc.querySelectorAll("[data-since]").forEach(function (t) {
        var s = Math.floor((Date.now() - Date.parse(t.dataset.since)) / 1000);
        if (s >= 0) t.textContent = Math.floor(s / 60) + "m" + String(s % 60).padStart(2, "0") + "s";
      });
    }, 1000);
  }
})();
