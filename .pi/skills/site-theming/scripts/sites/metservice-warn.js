// MetService only. In-page report: warning bar colour, text ratios, and the highest chroma of any other background.
() => {
  const cv = document.createElement('canvas'); cv.width = cv.height = 1;
  const cx = cv.getContext('2d', { willReadFrequently: true });
  const parse = (s) => { cx.clearRect(0, 0, 1, 1); cx.fillStyle = 'rgba(0,0,0,0)'; cx.fillStyle = s; cx.fillRect(0, 0, 1, 1); const d = cx.getImageData(0, 0, 1, 1).data; return [d[0], d[1], d[2], d[3] / 255]; };
  const lin = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
  const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
  const ratio = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const chroma = ([r, g, b]) => { [r, g, b] = [r, g, b].map(lin);
    const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b), m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b), s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
    const A = 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s, B = 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s; return Math.hypot(A, B); };
  const hex = (c) => '#' + c.slice(0, 3).map(v => v.toString(16).padStart(2, '0')).join('');
  const vis = (e) => { const r = e.getBoundingClientRect(); if (r.width <= 1 || r.height <= 1) return false; for (let x = e; x; x = x.parentElement) { const s = getComputedStyle(x); if (s.display === 'none' || s.visibility === 'hidden' || +s.opacity === 0) return false; } return true; };
  const bars = [...document.querySelectorAll('.WeatherWarning .WeatherWarning-bar, .WeatherWarning .WeatherWarning-div')].filter(vis);
  const out = { bars: [] };
  for (const b of bars) {
    const w = b.closest('.WeatherWarning');
    const bg = parse(getComputedStyle(b).backgroundColor);
    const texts = [];
    const tw = document.createTreeWalker(b, NodeFilter.SHOW_TEXT); let n;
    while ((n = tw.nextNode())) { if (!n.textContent.trim()) continue; const e = n.parentElement; if (!vis(e)) continue; const fg = parse(getComputedStyle(e).color); texts.push({ t: n.textContent.trim().slice(0, 30), fg: hex(fg), ratio: +ratio(fg, bg).toFixed(2) }); }
    out.bars.push({ cls: w.className, bg: hex(bg), chroma: +chroma(bg).toFixed(3), minRatio: Math.min(...texts.map(t => t.ratio)), texts, expanded: !!w.querySelector('.WeatherWarning-body') && w.querySelector('.WeatherWarning-body').getBoundingClientRect().height > 20 });
  }
  let maxOther = { chroma: 0 };
  for (const e of document.querySelectorAll('body *')) {
    if (e.closest('.WeatherWarning, .leaflet-container, img, svg, .Map')) continue;
    const c = parse(getComputedStyle(e).backgroundColor); if (c[3] < 0.99) continue;
    const ch = chroma(c); if (ch > maxOther.chroma && vis(e)) maxOther = { chroma: +ch.toFixed(3), bg: hex(c), el: e.tagName + '.' + [...e.classList].slice(0, 3).join('.') };
  }
  out.maxOther = maxOther;
  return out;
}

