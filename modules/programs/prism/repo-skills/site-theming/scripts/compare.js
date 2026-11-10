// Node side of check 6a: compare the themed snapshot with the unthemed snapshot.
// Elements with a site chroma of 0.08 or more must keep a themed chroma of 0.06 or more,
// and a hue within 40 degrees of the theme hue of their family.
// exempt: list of { re, props, nav, why } from the site config. re and props are regex
// source strings. An exempt element is reported with its reason, not as a failure.
const lin = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
const oklch = ([r, g, b]) => { [r, g, b] = [r, g, b].map(lin); const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b), m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b), s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b); const L = 0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s, A = 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s, B = 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s; let h = Math.atan2(B, A) * 180 / Math.PI; if (h < 0) h += 360; return { L, C: Math.hypot(A, B), H: h }; };
const hex = (c) => '#' + c.slice(0, 3).map(v => Math.round(v).toString(16).padStart(2, '0')).join('');
const dh = (a, b) => { const d = Math.abs(a - b) % 360; return Math.min(d, 360 - d); };
const SITE_MIN = 0.08, THEME_MIN = 0.06, HUE_TOL = 40;
function family(c, hues) { const o = oklch(c); let best = null, bd = 999; for (const [n, h] of Object.entries(hues)) { const d = dh(o.H, oklch(h).H); if (d < bd) { bd = d; best = n; } } return best; }
function judge(u, t, hues, sev) {
  const fam = sev || family(u, hues); const th = oklch(hues[fam]); const o = oklch(t);
  const ok = o.C >= THEME_MIN && dh(o.H, th.H) <= HUE_TOL;
  return { fam, site: hex(u), siteC: +oklch(u).C.toFixed(3), themed: hex(t), C: +o.C.toFixed(3), dH: +dh(o.H, th.H).toFixed(1), ok };
}
function place(out, rec, exempt) {
  if (rec.ok) return out.passes.push(rec);
  const x = exempt.find(e => new RegExp(e.re).test(rec.el) && new RegExp(e.props).test(rec.prop) && (!e.nav || rec.nav));
  if (x) { rec.why = x.why; return out.exempt.push(rec); }
  out.fails.push(rec);
}
function compare(U, T, exempt = []) {
  const hues = T.hues; const out = { checked: 0, fails: [], passes: [], exempt: [], mismatch: 0, missing: 0, gradients: [] };
  for (const [k, u] of Object.entries(U.els)) {
    if (!u.vis) continue;
    const t = T.els[k];
    const props = [];
    if (u.color && oklch(u.color).C >= SITE_MIN) props.push('color');
    if (u.bg && oklch(u.bg).C >= SITE_MIN) props.push('bg');
    const stopIdx = (u.stops || []).map((s, i) => s[3] > 0 && oklch(s).C >= SITE_MIN ? i : -1).filter(i => i >= 0);
    if (!props.length && !stopIdx.length) continue;
    if (!t) { out.missing++; continue; }
    if (t.t !== u.t) { out.mismatch++; continue; }
    for (const p of props) {
      out.checked++;
      // themed element can lose its own background but sit on a themed parent; then its own bg is unset.
      const tv = p === 'bg' ? (t.bg || t.ebg) : t[p];
      const rec = { key: k, el: u.t.slice(0, 90), prop: p, text: u.text, nav: u.nav, ...(tv ? judge(u[p], tv, hues, p === 'bg' ? u.sev : undefined) : { fam: family(u[p], hues), site: hex(u[p]), themed: 'none', ok: false }) };
      place(out, rec, exempt);
    }
    if (stopIdx.length) {
      const g = { key: k, el: u.t, sitePos: u.pos, themedPos: t.pos, posEqual: JSON.stringify(u.pos) === JSON.stringify(t.pos), stops: [] };
      for (const i of stopIdx) {
        out.checked++;
        const tv = t.stops && t.stops[i];
        const rec = { key: k, el: u.t, prop: 'stop' + i, ...(tv ? judge(u.stops[i], tv, hues, u.sev) : { site: hex(u.stops[i]), themed: 'none', ok: false }) };
        place(out, rec, exempt);
      }
      g.siteStops = u.stops.map(hex); g.themedStops = (t.stops || []).map(hex);
      out.gradients.push(g);
    }
  }
  return out;
}
module.exports = { compare, oklch, hex };
