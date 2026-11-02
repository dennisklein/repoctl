// repoctl graph page client. No framework. Server renders all state; this file only adds
// hover/selection highlight, zoom, elapsed timers and the edge state mirror after SSE swaps.
(function () {
  'use strict';
  const svg = document.querySelector('svg.graph');
  if (!svg) return;
  const mock = document.documentElement.hasAttribute('data-mock');
  const q = (s) => [...svg.querySelectorAll(s)];
  const list = (g, k) => (g.dataset[k] || '').split(' ').filter(Boolean);

  // Highlight: every .gnode carries data-up / data-down (transitive closure, computed in Go).
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
  svg.addEventListener('mouseover', (e) => {
    const g = e.target.closest('.gnode');
    if (g && !pinned()) mark(g.dataset.node);
  });
  svg.addEventListener('mouseleave', () => { if (!pinned()) clear(); });
  // Click opens the drawer via hx-get on the card; keep the highlight pinned.
  document.body.addEventListener('htmx:afterRequest', (e) => {
    const g = e.target.closest && e.target.closest('.gnode');
    if (g) { svg.dataset.sel = g.dataset.node; mark(g.dataset.node); }
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && pinned()) { delete svg.dataset.sel; clear(); }
  });

  // SSE swaps replace the card (div.nd). Mirror its state to the <g> and to outgoing edges.
  document.body.addEventListener('htmx:afterSwap', (e) => {
    const nd = e.detail.elt.closest && e.detail.elt.closest('.nd');
    if (!nd) return;
    const g = nd.closest('.gnode');
    g.dataset.state = nd.dataset.state;
    q(`.edge[data-from="${g.dataset.node}"]`).forEach((ed) => (ed.dataset.srcState = nd.dataset.state));
  });

  // Zoom: scale the SVG width; the viewBox keeps the layout.
  const base = +svg.getAttribute('width');
  let z = 1;
  const label = document.querySelector('.zoom .zl');
  function zoom(v) {
    z = Math.min(2, Math.max(0.4, v));
    svg.setAttribute('width', Math.round(base * z));
    svg.setAttribute('height', Math.round(+svg.dataset.h * z));
    if (label) label.textContent = Math.round(z * 100) + '%';
  }
  document.querySelectorAll('.zoom [data-zoom]').forEach((b) => b.addEventListener('click', () => {
    const a = b.dataset.zoom;
    if (a === 'in') zoom(z + 0.1);
    else if (a === 'out') zoom(z - 0.1);
    else if (a === 'reset') zoom(1);
    else zoom(svg.parentNode.clientWidth / base); // fit
  }));

  // Selected node: keep it in view (drawer open).
  const sel = svg.querySelector('.gnode.is-sel');
  if (sel) sel.scrollIntoView({ block: 'nearest', inline: 'center' });

  // Elapsed timers tick on the client; the server sends only state and step changes.
  if (!mock) setInterval(() => {
    document.querySelectorAll('time.tick').forEach((t) => {
      const s = Math.floor((Date.now() - Date.parse(t.dateTime)) / 1000);
      if (s >= 0) t.textContent = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
    });
  }, 1000);
})();
