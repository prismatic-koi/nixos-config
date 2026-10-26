// Dumps the custom properties (CSS variables) of a page, in light mode and in dark mode,
// and groups the names by equal (light, dark) value pair.
//
// Use it as the first step for a site with hashed or obfuscated property names. Names
// with the same (light, dark) pair are the same token. Map a hashed name to a readable
// name by this pair, as youtube.css.js does at run time.
//
// Usage:
//   node dump-properties.js sites/<site>.js [--page <id>] [--element <selector>]
// Options:
//   --page <id>        Page id from the site config. Default: the first page.
//   --element <sel>    Also dump the computed custom properties of this element. html is always dumped.
// Environment: OUT and the variables in lib.js.
//
// Output: OUT/properties/<site>_<page>.json, and a summary on standard output:
// the count of names for each element and mode, and each pair that more than one name shares and that differs between light and dark.
// The page runs unthemed. A live site changes between runs.
const fs = require('fs');
const path = require('path');
const { launch, newCtx, OUT } = require('./lib');

const args = process.argv.slice(2);
const siteFile = args.shift();
const opt = (k, d) => { const i = args.indexOf('--' + k); return i >= 0 ? args[i + 1] : d; };
if (!siteFile) { console.error('usage: node dump-properties.js sites/<site>.js [--page id] [--element selector]'); process.exit(2); }
const site = require(path.resolve(siteFile));
const pageId = opt('page', Array.isArray(site.pages) ? site.pages[0] : Object.keys(site.pages)[0]);
const selectors = ['html'].concat(opt('element') ? [opt('element')] : []);

const dump = (sels) => {
  const out = {};
  for (const sel of sels) {
    const e = document.querySelector(sel);
    if (!e) { out[sel] = null; continue; }
    const cs = getComputedStyle(e); out[sel] = {};
    for (const n of cs) if (n.startsWith('--')) out[sel][n] = cs.getPropertyValue(n).trim();
  }
  return out;
};

(async () => {
  const b = await launch();
  const res = {};
  for (const mode of ['light', 'dark']) {
    const ctx = await newCtx(b, { site, colorScheme: mode, cookies: site.cookies ? site.cookies(mode) : undefined });
    const p = await ctx.newPage();
    if (site.pageUrl) { await p.goto(site.pageUrl(pageId), { waitUntil: 'load', timeout: 90000 }); await p.waitForSelector(site.readySelector, { timeout: 60000 }); }
    else { await p.goto(site.pages[pageId], { waitUntil: 'load', timeout: 60000 }); }
    await p.waitForTimeout(5000);
    res[mode] = await p.evaluate(dump, selectors);
    await ctx.close();
  }
  await b.close();
  const pairs = {};
  for (const sel of selectors) {
    const L = res.light[sel] || {}, D = res.dark[sel] || {};
    console.log(sel, 'light', Object.keys(L).length, 'dark', Object.keys(D).length);
    const g = {};
    for (const n of Object.keys(L)) if (n in D) (g[L[n] + ' | ' + D[n]] = g[L[n] + ' | ' + D[n]] || []).push(n);
    pairs[sel] = g;
    for (const [k, v] of Object.entries(g).filter(([k, v]) => v.length > 1 && k.split(' | ')[0] !== k.split(' | ')[1]).sort((a, b) => b[1].length - a[1].length).slice(0, 15)) console.log('  ', v.length, 'names share', k, '->', v.slice(0, 4).join(' '));
  }
  const dir = path.join(OUT, 'properties'); fs.mkdirSync(dir, { recursive: true });
  const f = path.join(dir, `${site.name}_${pageId}.json`);
  fs.writeFileSync(f, JSON.stringify({ site: site.name, page: pageId, props: res, pairs }, null, 1));
  console.log('written', f);
})();
