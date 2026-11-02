// repoctl shell.js: the only page script besides htmx + htmx-ext-sse.
// Served as /-/static/<hash>/shell.js. Each part works alone; pages render without it.
(function () {
  var doc = document, body = doc.body, root = doc.documentElement;
  var set = body.dataset.set || "";            // <body data-set="slurm-26.05">
  var base = set ? "/s/" + set + "/" : "/";

  // 1. Theme: auto -> light -> dark. Production stores it in the cookie
  //    repoctl_theme, so the server renders <html data-theme> without a flash.
  doc.addEventListener("click", function (e) {
    var t = e.target.closest("[data-theme-toggle]");
    if (!t) return;
    var cur = root.dataset.theme || "auto";
    var next = { auto: "light", light: "dark", dark: "auto" }[cur];
    if (next === "auto") delete root.dataset.theme; else root.dataset.theme = next;
    try { doc.cookie = "repoctl_theme=" + next + ";path=/;samesite=strict"; } catch (_) {}
    t.setAttribute("aria-label", "Theme: " + next);
  });

  // 2. Copy buttons: <button class="copy" data-copy="full text">
  doc.addEventListener("click", function (e) {
    var b = e.target.closest("[data-copy]");
    if (!b || !navigator.clipboard) return;
    navigator.clipboard.writeText(b.dataset.copy).then(function () {
      b.classList.add("done"); setTimeout(function () { b.classList.remove("done"); }, 1200);
    });
  });

  // 3. Keyboard. Ignored while focus is in an input, or with Ctrl/Alt/Meta.
  var go = { o: "", g: "graph", p: "packages", r: "runs", u: "upstream", b: "repo/", s: "/" };
  var pending = null;
  doc.addEventListener("keydown", function (e) {
    if (e.ctrlKey || e.altKey || e.metaKey) return;
    var tag = (e.target.tagName || "").toLowerCase();
    if (tag === "input" || tag === "textarea" || tag === "select" || e.target.isContentEditable) {
      if (e.key === "Escape") e.target.blur();
      return;
    }
    if (pending) {
      clearTimeout(pending); pending = null;
      if (e.key in go) {
        var p = go[e.key];
        if (e.key === "b") p += (body.dataset.target || "el9");
        location.href = p.charAt(0) === "/" ? p : base + p;
      }
      return;
    }
    if (e.key === "g") { pending = setTimeout(function () { pending = null; }, 1200); return; }
    if (e.key === "/") { var j = doc.querySelector(".jump input"); if (j) { e.preventDefault(); j.focus(); } return; }
    if (e.key === "?") { var h = doc.getElementById("help"); if (h && h.showModal) { e.preventDefault(); h.showModal(); } return; }
    if (e.key === "t") { var tt = doc.querySelector("[data-next-target]"); if (tt) location.href = tt.href; }
  });

  // 4. SSE state. htmx-ext-sse fires htmx:sseOpen / htmx:sseError / htmx:sseClose.
  //    After 2 missed heartbeats (30 s) or an error, the page is marked offline.
  var last = Date.now(), down = false, retry = 2;
  function mark(d, why) {
    if (d === down) return;
    down = d;
    if (d) body.dataset.sse = "down"; else delete body.dataset.sse;
    var w = doc.querySelector(".offline [data-why]");
    if (w && why) w.textContent = why;
    if (!d) refresh();
  }
  function refresh() { // regions that missed events reload their fragment
    if (window.htmx) doc.querySelectorAll("[data-live][hx-get]").forEach(function (el) { htmx.trigger(el, "refresh"); });
  }
  body.addEventListener("htmx:sseOpen", function () { last = Date.now(); retry = 2; mark(false); });
  body.addEventListener("htmx:sseMessage", function () { last = Date.now(); });
  body.addEventListener("htmx:sseError", function () {
    // Tell "server stopped" from "network hiccup" with one ping.
    fetch("/-/ping", { credentials: "same-origin" }).then(function (r) {
      if (r.status === 401) location.href = "/-/session?next=" + encodeURIComponent(location.pathname);
      else mark(true, "The connection to repoctl ui was lost. The page shows old data.");
    }).catch(function () {
      mark(true, "The repoctl ui server does not answer. It stopped, or the machine is not reachable.");
    });
  });
  if (window.htmx && body.hasAttribute("sse-connect")) setInterval(function () { if (Date.now() - last > 30000) mark(true, "No update for 30 s. The page shows old data."); }, 5000);
})();
