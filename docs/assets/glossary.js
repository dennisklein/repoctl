/* Glossary hover card of the design record and the web UI mockups. No dependencies.
   Needs glossary-data.js first. A term link is <a class="gl" data-gl="ID" data-domain="KEY">:
   tools/site/build.py and tools/site/glossary.py write them. When the pointer or the focus is
   on a term, the card shows the domain (icon and colour), the word, its kind and the definition.
   Esc closes the card. The link itself goes to the glossary entry. */
(function () {
  "use strict";
  var G = window.REPOCTL_GLOSSARY;
  if (!G || !document.querySelector) return;
  var doc = document, terms = {};
  G.terms.forEach(function (t) { terms[t.id] = t; });

  var card = doc.createElement("div");
  card.className = "gl-card";
  card.id = "gl-card";
  card.setAttribute("role", "tooltip");
  card.hidden = true;
  doc.body.appendChild(card);

  var NS = "http://www.w3.org/2000/svg";
  function icon(path) {
    var svg = doc.createElementNS(NS, "svg"), p = doc.createElementNS(NS, "path");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("class", "gl-card-icon");
    p.setAttribute("d", path);
    svg.appendChild(p);
    return svg;
  }
  function el(tag, cls, text) {
    var e = doc.createElement(tag);
    e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  var cur = null, showT = 0, hideT = 0;
  function show(a) {
    var t = terms[a.getAttribute("data-gl")];
    if (!t) return;
    var d = G.domains[t.domain] || { name: t.domain, icon: "" };
    cur = a;
    card.setAttribute("data-domain", t.domain);
    card.textContent = "";
    var head = el("div", "gl-card-domain");
    head.appendChild(icon(d.icon));
    head.appendChild(el("span", "", d.name));
    var term = el("div", "gl-card-term", t.term);
    term.appendChild(el("span", "gl-card-kind", t.kind));
    var def = el("div", "gl-card-def");
    def.innerHTML = t.def;           // from glossary-data.js: inline <code> only
    card.appendChild(head);
    card.appendChild(term);
    card.appendChild(def);
    card.hidden = false;
    var r = a.getBoundingClientRect(), vw = doc.documentElement.clientWidth;
    var w = card.offsetWidth, h = card.offsetHeight;
    var x = Math.min(Math.max(8, r.left), vw - w - 8);
    var y = r.bottom + 8;
    if (y + h > window.innerHeight - 8 && r.top - h - 8 > 8) y = r.top - h - 8;
    card.style.left = Math.round(x + window.scrollX) + "px";
    card.style.top = Math.round(y + window.scrollY) + "px";
    a.setAttribute("aria-describedby", "gl-card");
  }
  function hide() {
    if (cur) cur.removeAttribute("aria-describedby");
    cur = null;
    card.hidden = true;
  }
  function termOf(e) { return e.target.closest ? e.target.closest("a.gl[data-gl]") : null; }

  doc.addEventListener("mouseover", function (e) {
    var a = termOf(e);
    clearTimeout(showT); clearTimeout(hideT);
    if (a) { if (a !== cur) showT = setTimeout(function () { show(a); }, 150); }
    else if (cur) hideT = setTimeout(hide, 120);
  });
  doc.addEventListener("focusin", function (e) { var a = termOf(e); if (a) show(a); else if (cur) hide(); });
  doc.addEventListener("focusout", function (e) { if (termOf(e) === cur) hide(); });
  doc.addEventListener("keydown", function (e) { if (e.key === "Escape" && cur) hide(); });
  window.addEventListener("resize", hide);
})();
