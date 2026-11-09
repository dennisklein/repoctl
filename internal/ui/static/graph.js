// repoctl graph page client. No framework. The server renders all state; this file only adds the elapsed timers,
// and in the DAG view the hover and selection highlight, zoom and the edge state mirror after SSE swaps.
// In a static mockup (no htmx) the timers do not run, so screenshots stay at the story time.
(function () {
  'use strict';
  const live = typeof window.htmx !== 'undefined' && document.body.hasAttribute('sse-connect');
  // Elapsed timers tick on the client; the server sends only state and step changes.
  if (live) setInterval(() => {
    document.querySelectorAll('time.tick').forEach((t) => {
      const s = Math.floor((Date.now() - Date.parse(t.dateTime)) / 1000);
      if (s >= 0) t.textContent = s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s` : `${Math.floor(s / 3600)}h${String(Math.floor(s / 60) % 60).padStart(2, '0')}m`;
    });
  }, 1000);

  // The rest is for the DAG view only (the matrix view has no SVG).
  const svg = document.querySelector('svg.graph');
  if (!svg) return;
  // The server cannot set the width as a style attribute (Content-Security-Policy), so the script does.
  svg.style.setProperty('--gw', svg.getAttribute('width') + 'px');
  // Enter on a focused card opens its drawer as a click does.
  svg.addEventListener('keyup', (e) => {
    const nd = e.key === 'Enter' && e.target.closest && e.target.closest('.nd');
    if (nd) nd.click();
  });
  const q = (s) => [...svg.querySelectorAll(s)];
  const list = (g, k) => (g.dataset[k] || '').split(' ').filter(Boolean);

  // Highlight: every .gnode carries data-up and data-down (the transitive closure, computed on the server).
  function mark(id) {
    const g = svg.querySelector(`.gnode[data-node="${id}"]`);
    if (!g) return;
    const up = new Set(list(g, 'up')), down = new Set(list(g, 'down'));
    svg.classList.add('has-sel');
    q('.gnode').forEach((n) => {
      const k = n.dataset.node;
      n.classList.toggle('is-sel', k === id);
      n.classList.toggle('is-up', up.has(k));
      n.classList.toggle('is-down', down.has(k));
      n.classList.toggle('is-dim', k !== id && !up.has(k) && !down.has(k));
    });
    q('.edge').forEach((e) => {
      const f = e.dataset.from, t = e.dataset.to;
      const isDown = (f === id || down.has(f)) && down.has(t);
      const isUp = (t === id || up.has(t)) && up.has(f);
      e.classList.toggle('is-down', isDown);
      e.classList.toggle('is-up', isUp);
      if (isDown || isUp) e.parentNode.appendChild(e); // draw on top
    });
  }
  function clear() {
    svg.classList.remove('has-sel');
    q('.gnode,.edge').forEach((n) => n.classList.remove('is-sel', 'is-up', 'is-down', 'is-dim'));
  }
  const pinned = () => svg.dataset.sel || '';
  // A page rendered with a selected node (node drawer open) starts with that highlight.
  const pre = svg.querySelector('.gnode.is-sel');
  if (pre) { svg.dataset.sel = pre.dataset.node; mark(pre.dataset.node); }

  svg.addEventListener('mouseover', (e) => {
    const g = e.target.closest('.gnode');
    if (g && !pinned()) mark(g.dataset.node);
  });
  svg.addEventListener('mouseleave', () => { if (!pinned()) clear(); });
  // Keyboard focus shows the same highlight as hover.
  svg.addEventListener('focusin', (e) => {
    const g = e.target.closest('.gnode');
    if (g && !pinned()) mark(g.dataset.node);
  });
  // A click opens the drawer through hx-get on the card; the highlight stays pinned.
  document.body.addEventListener('htmx:afterRequest', (e) => {
    const g = e.target.closest && e.target.closest('.gnode');
    if (g) { svg.dataset.sel = g.dataset.node; mark(g.dataset.node); }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && pinned()) { delete svg.dataset.sel; clear(); }
  });

  // SSE swaps replace the card (div.nd). Mirror its state to the <g> and to the outgoing edges.
  document.body.addEventListener('htmx:afterSwap', (e) => {
    const nd = e.detail.elt.closest && e.detail.elt.closest('.nd');
    if (!nd) return;
    const g = nd.closest('.gnode');
    if (!g) return;
    g.dataset.state = nd.dataset.state;
    q(`.edge[data-from="${g.dataset.node}"]`).forEach((ed) => (ed.dataset.srcState = nd.dataset.state));
  });

  // Zoom. CSS lets the graph shrink to fit the canvas (to 86 % at most). A zoom button sets an explicit width;
  // the viewBox keeps the layout.
  const base = +svg.getAttribute('width');
  const scroller = svg.parentNode;
  const label = document.querySelector('.zoom .zl');
  let z = 0; // 0: fit to the canvas by CSS
  function showScale() {
    if (label) label.textContent = Math.round((z || svg.getBoundingClientRect().width / base) * 100) + '%';
  }
  function zoom(v) {
    z = Math.min(2, Math.max(0.4, v));
    svg.style.width = Math.round(base * z) + 'px';
    svg.style.minWidth = svg.style.maxWidth = 'none';
    showScale();
  }
  document.querySelectorAll('.zoom [data-zoom]').forEach((b) => b.addEventListener('click', () => {
    const a = b.dataset.zoom, cur = z || svg.getBoundingClientRect().width / base;
    if (a === 'in') zoom(cur + 0.1);
    else if (a === 'out') zoom(cur - 0.1);
    else if (a === 'reset') zoom(1);
    else zoom(scroller.clientWidth / base); // fit
  }));
  showScale();
  window.addEventListener('resize', () => { if (!z) showScale(); });

  // Keep the selected node in view while the drawer is open: scroll the canvas (not the page) only as far as needed.
  if (pre) {
    const r = pre.getBoundingClientRect(), s = scroller.getBoundingClientRect(), m = 24;
    if (r.right > s.right - m) scroller.scrollLeft += r.right - s.right + m;
    else if (r.left < s.left + m) scroller.scrollLeft -= s.left + m - r.left;
  }

})();
