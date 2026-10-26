// In-page snapshot for check 6a. Run on the unthemed page and on the themed page:
//   page.evaluate(`(${source})(${full}, ${JSON.stringify(cfg)})`)
// Returns { els: { key: rec }, hues }. The key is the child-index path from <body>.
// rec: tag and class, own-text colour, background colour, gradient stops.
// cfg (the "snapshot" object of the site config):
//   navSelector      Selector. Elements inside it get nav:true (used by exemptions).
//   severityClasses  { hue: selector }. An element inside a match is in that hue family.
(full, cfg) => {
  const cv = document.createElement('canvas'); cv.width = cv.height = 1;
  const cx = cv.getContext('2d', { willReadFrequently: true });
  const cache = new Map();
  const parse = (s) => { if (cache.has(s)) return cache.get(s); let o; if (!s || s === 'transparent' || s === 'rgba(0, 0, 0, 0)') o = [0, 0, 0, 0]; else { cx.clearRect(0, 0, 1, 1); cx.fillStyle = 'rgba(0,0,0,0)'; cx.fillStyle = s; cx.fillRect(0, 0, 1, 1); const d = cx.getImageData(0, 0, 1, 1).data; o = [d[0], d[1], d[2], d[3] / 255]; } cache.set(s, o); return o; };
  const gradStops = (bi) => (bi.match(/rgba?\([^)]*\)|color\([^)]*\)|oklch\([^)]*\)|oklab\([^)]*\)/g) || []).map(parse);
  const gradPos = (bi) => (bi.match(/\)\s+(-?[\d.]+px)/g) || []).map(x => x.replace(/^\)\s+/, ''));
  const visible = (e) => {
    const r = e.getBoundingClientRect(); if (r.width <= 1 || r.height <= 1) return false;
    if (e.closest('[aria-hidden="true"]')) return false;
    for (let x = e; x; x = x.parentElement) {
      const s = getComputedStyle(x);
      if (s.display === 'none' || s.visibility === 'hidden' || +s.opacity === 0) return false;
      if (s.clip === 'rect(0px, 0px, 0px, 0px)' || s.clipPath === 'inset(50%)') return false;
      
    }
    return true;
  };
  const els = {};
  const over = (t, b) => { const a = t[3]; return [0, 1, 2].map(i => t[i] * a + b[i] * (1 - a)).concat(1); };
  const walk = (e, key, peff) => {
    const kids = e.children;
    for (let i = 0; i < kids.length; i++) {
      const k = kids[i]; const kk = key + '.' + i;
      if (/^(SCRIPT|STYLE|NOSCRIPT|TEMPLATE|svg)$/i.test(k.tagName)) continue;
      const s = getComputedStyle(k);
      const ownText = [...k.childNodes].some(n => n.nodeType === 3 && n.textContent.trim());
      const rec = { t: k.tagName.toLowerCase() + (k.classList.length ? '.' + [...k.classList].join('.') : '') };
      if (full) {
        if (cfg.navSelector && k.closest(cfg.navSelector)) rec.nav = true;
        // A severity class on the element or an ancestor names the hue family.
        const sv = cfg.severityClasses && k.closest(Object.values(cfg.severityClasses).join(', '));
        if (sv) rec.sev = Object.keys(cfg.severityClasses).find(h => sv.matches(cfg.severityClasses[h]));
      }
      if (ownText) { rec.color = parse(s.color); rec.text = k.textContent.trim().slice(0, 40); }
      const bg = parse(s.backgroundColor); if (bg[3] >= 0.5) rec.bg = bg;
      const eff = bg[3] > 0 ? over(bg, peff) : peff; rec.ebg = eff;
      if (/gradient\(/.test(s.backgroundImage)) { rec.stops = gradStops(s.backgroundImage); rec.pos = gradPos(s.backgroundImage); }
      if (full && (rec.color || rec.bg || rec.stops)) rec.vis = visible(k);
      els[kk] = rec;
      walk(k, kk, eff);
    }
  };
  const bodyBg = over(parse(getComputedStyle(document.body).backgroundColor), over(parse(getComputedStyle(document.documentElement).backgroundColor), [255, 255, 255, 1]));
  walk(document.body, 'b', bodyBg);
  const hues = {};
  for (const h of ['red', 'orange', 'yellow', 'green', 'aqua', 'blue', 'purple']) {
    const v = getComputedStyle(document.documentElement).getPropertyValue('--system-theme-' + h).trim();
    if (v) hues[h] = parse(v);
  }
  return { els, hues };
}
