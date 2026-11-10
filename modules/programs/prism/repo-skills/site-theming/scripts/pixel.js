// Pixel analysis helpers. Each takes a helper page: a second page in the same context,
// used only to decode PNG data with a canvas.
async function analyseAsset(helper, png, bgHex) {
  return helper.evaluate(async ({ b64, bgHex }) => {
    const img = new Image(); img.src = 'data:image/png;base64,' + b64; await img.decode();
    const cv = document.createElement('canvas'); cv.width = img.width; cv.height = img.height;
    const cx = cv.getContext('2d'); cx.drawImage(img, 0, 0);
    const d = cx.getImageData(0, 0, cv.width, cv.height).data;
    const bg = [1, 3, 5].map(i => parseInt(bgHex.slice(i, i + 2), 16));
    const counts = new Map(); let total = 0, fgPix = 0;
    for (let i = 0; i < d.length; i += 4) {
      total++;
      const px = [d[i], d[i + 1], d[i + 2]];
      if (Math.hypot(px[0] - bg[0], px[1] - bg[1], px[2] - bg[2]) < 40) continue;
      fgPix++;
      const k = px.map(v => v >> 4).join(',');
      counts.set(k, (counts.get(k) || 0) + 1);
    }
    let best = null, bc = 0; for (const [k, v] of counts) if (v > bc) { best = k; bc = v; }
    const lin = c => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
    const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    if (!best) return { dominant: null, assetPixels: 0, total };
    const dom = best.split(',').map(v => (+v << 4) + 8);
    const L1 = lum(dom), L2 = lum(bg);
    return { dominant: '#' + dom.map(v => v.toString(16).padStart(2, '0')).join(''), domLum: +L1.toFixed(3), assetPixels: fgPix, total, ratio: +((Math.max(L1, L2) + .05) / (Math.min(L1, L2) + .05)).toFixed(2) };
  }, { b64: png.toString('base64'), bgHex });
}

// Header check : distinct pixel colours on rows near the top and bottom edge of a band.
async function band(p, helper, rect) {
  if (!rect || rect.w < 10 || rect.h < 8) return null;
  const png = await p.screenshot({ clip: { x: rect.x, y: rect.y, width: rect.w, height: rect.h } });
  return helper.evaluate(async ({ b64, rows, h }) => {
    const img = new Image(); img.src = 'data:image/png;base64,' + b64; await img.decode();
    const cv = document.createElement('canvas'); cv.width = img.width; cv.height = img.height;
    const cx = cv.getContext('2d'); cx.drawImage(img, 0, 0);
    const out = {};
    for (const fy of rows) {
      const y = Math.round(fy * img.height / h); const d = cx.getImageData(0, y, img.width, 1).data; const m = new Map();
      for (let i = 0; i < d.length; i += 4) { const k = '#' + [d[i], d[i + 1], d[i + 2]].map(v => v.toString(16).padStart(2, '0')).join(''); m.set(k, (m.get(k) || 0) + 1); }
      out['y' + fy] = [...m].filter(([, n]) => n / img.width > 0.01).sort((a, b) => b[1] - a[1]).map(([k, n]) => k + ' ' + (100 * n / img.width).toFixed(0) + '%');
    }
    return out;
  }, { b64: png.toString('base64'), rows: [3, rect.h - 4], h: rect.h });
}

module.exports = { analyseAsset, band };
