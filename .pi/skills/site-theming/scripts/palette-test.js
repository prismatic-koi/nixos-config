// Computes the contrast of a CSS colour expression against a background, on every palette.
//
// Usage:
//   node palette-test.js --fg '<css colour>' --bg '<css colour>' [--css <site style>] [--min 4.5]
// Options:
//   --fg, --bg   CSS colour expressions. They can use var(--system-theme-*), color-mix(),
//                and relative colour syntax. With --css, they can also use the variables
//                that the site style defines.
//   --css        A site style file. It is injected after the theme, so its variables resolve.
//   --min        The pass ratio. Default: 4.5.
// Environment: THEMES (comma list; default: all themes from theme-variables.sh --list),
//   OUT, and the variables in lib.js.
//
// Examples:
//   Black or white warning text on the orange hue:
//     --fg 'oklch(from var(--system-theme-orange) clamp(0, (0.565 - l) * 1000, 1) 0 0)' --bg 'var(--system-theme-orange)'
//   Status text of metservice.css.js on bg0:
//     --css <path>/metservice.css.js --fg 'var(--ms-orange-text)' --bg 'var(--system-theme-bg0)'
// Prints one line for each palette: both colours, the ratio, and pass or FAIL. Exits 1 on any FAIL.
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');
const { launch, wrap, themeSource } = require('./lib');

const args = process.argv.slice(2);
const opt = (k, d) => { const i = args.indexOf('--' + k); return i >= 0 ? args[i + 1] : d; };
const fg = opt('fg'), bg = opt('bg'), min = +opt('min', 4.5), css = opt('css');
if (!fg || !bg) { console.error("usage: node palette-test.js --fg '<css colour>' --bg '<css colour>' [--css file] [--min 4.5]"); process.exit(2); }
const themes = process.env.THEMES ? process.env.THEMES.split(',')
  : execFileSync(path.join(__dirname, 'theme-variables.sh'), ['--list'], { encoding: 'utf8' }).split('\n').filter(Boolean);

const measure = ({ fg, bg }) => {
  const cv = document.createElement('canvas'); cv.width = cv.height = 1;
  const cx = cv.getContext('2d', { willReadFrequently: true });
  const px = (v, prop) => { const d = document.createElement('div'); d.style[prop] = v; document.body.appendChild(d); const s = getComputedStyle(d)[prop]; d.remove(); cx.clearRect(0, 0, 1, 1); cx.fillStyle = s; cx.fillRect(0, 0, 1, 1); return [...cx.getImageData(0, 0, 1, 1).data].slice(0, 3); };
  const lin = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
  const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
  const hex = (c) => '#' + c.map((v) => v.toString(16).padStart(2, '0')).join('');
  const f = px(fg, 'color'), b = px(bg, 'backgroundColor');
  const x = lum(f), y = lum(b);
  return { fg: hex(f), bg: hex(b), ratio: +((Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)).toFixed(2) };
};

(async () => {
  const browser = await launch();
  const page = await browser.newPage();
  let bad = 0;
  for (const t of themes) {
    await page.setContent('<html><head></head><body></body></html>');
    await page.evaluate(wrap(themeSource(t)).replace(/\)\(\);$/, ')();') + (css ? ';' + wrap(fs.readFileSync(css, 'utf8')) : ''));
    await page.waitForTimeout(50);
    const r = await page.evaluate(measure, { fg, bg });
    const ok = r.ratio >= min; if (!ok) bad++;
    console.log(t.padEnd(18), 'fg', r.fg, 'bg', r.bg, 'ratio', r.ratio, ok ? 'pass' : 'FAIL');
  }
  await browser.close();
  process.exit(bad ? 1 : 0);
})();
