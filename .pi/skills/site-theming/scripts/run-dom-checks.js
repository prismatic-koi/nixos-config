// Runs the DOM checks (5a, 5b, 5c, 6a, 6b) on a site, for each theme, view and page.
//
// Usage:
//   SITE_CSS=<site style file> node run-dom-checks.js sites/<site>.js
//
// Environment:
//   SITE_CSS   Required. The site style, for example .../greasemonkey/metservice.css.js.
//   THEMES     Comma list. Default: everforest-light,catppuccin-mocha.
//   VIEWS      Comma list of <width>x<scale>. Default: 1440x1,1100x1,800x1,1100x1.25.
//   PAGES      Comma list of page ids from the site config. Default: all pages of the site.
//   OUT        Output directory. Default: ./site-theming-out. It holds results/, unthemed/,
//              themes/ and cache/. Do not commit it.
//   PAR        Parallel runs. Default: 4.
// Other variables: see lib.js.
//
// Each run writes OUT/results/<theme>_<view>_<page>.json and prints one line.
// A theme file is built with theme-variables.sh when it is missing.
const fs = require('fs');
const path = require('path');
const { launch, newCtx, OUT } = require('./lib');
const pixel = require('./pixel');
const { compare } = require('./compare');

const siteFile = process.argv[2];
if (!siteFile || !process.env.SITE_CSS) { console.error('usage: SITE_CSS=<style> node run-dom-checks.js sites/<site>.js'); process.exit(2); }
const site = require(path.resolve(siteFile));
const CSS = path.resolve(process.env.SITE_CSS);
const CHECKS = fs.readFileSync(path.join(__dirname, 'dom-checks.js'), 'utf8');
const SNAP = fs.readFileSync(path.join(__dirname, 'snapshot.js'), 'utf8');
const checksCfg = JSON.stringify(site.checks || {});
const snapCfg = JSON.stringify(site.snapshot || {});
const runChecks = (p) => p.evaluate(`(${CHECKS})(${checksCfg})`);
const runSnap = (p, full) => p.evaluate(`(${SNAP})(${full}, ${snapCfg})`);

const PAGES = process.env.PAGES ? process.env.PAGES.split(',') : site.pages;
const VIEWS = (process.env.VIEWS || '1440x1,1100x1,800x1,1100x1.25').split(',').map((v) => { const [w, d] = v.split('x'); return { w: +w, d: +d }; });
const THEMES = (process.env.THEMES || 'everforest-light,catppuccin-mocha').split(',');
const RES = path.join(OUT, 'results'); const UDIR = path.join(OUT, 'unthemed');
fs.mkdirSync(RES, { recursive: true }); fs.mkdirSync(UDIR, { recursive: true });

async function load(p, name, page) {
  for (let attempt = 0; attempt < 3; attempt++) {
    const resp = await p.goto(site.pageUrl(page), { waitUntil: 'load', timeout: 90000 });
    const ok = await p.waitForSelector(site.readySelector, { timeout: 30000 }).then(() => true).catch(() => false);
    if (ok) break;
    console.log(name, 'retry', attempt, resp && resp.status());
    await p.waitForTimeout(5000 * (attempt + 1));
  }
  await p.waitForLoadState('networkidle', { timeout: 20000 }).catch(() => { });
  // Scroll to load lazy content.
  await p.evaluate(async () => { for (let y = 0; y < document.body.scrollHeight; y += 600) { scrollTo(0, y); await new Promise((r) => setTimeout(r, 120)); } scrollTo(0, 0); });
}

// The unthemed reference for check 6a: same page, view, cache and expand steps, no injection.
const pending = new Map();
function unthemed(browser, v, page) {
  const name = `unthemed_${v.w}x${v.d}_${site.pageName(page)}`;
  const f = path.join(UDIR, name + '.json');
  if (fs.existsSync(f)) return Promise.resolve(JSON.parse(fs.readFileSync(f)));
  if (pending.has(f)) return pending.get(f);
  const job = (async () => {
    const ctx = await newCtx(browser, { site, width: v.w, dsf: v.d });
    const p = await ctx.newPage();
    await load(p, name, page);
    await site.expand(p);
    await p.waitForTimeout(1000);
    const snap = await runSnap(p, true);
    snap.empty = (await runChecks(p)).empty;
    fs.writeFileSync(f, JSON.stringify(snap));
    await ctx.close();
    return snap;
  })();
  pending.set(f, job);
  return job;
}

async function one(browser, theme, v, page) {
  const name = `${theme}_${v.w}x${v.d}_${site.pageName(page)}`;
  const U = await unthemed(browser, v, page);
  const ctx = await newCtx(browser, { site, theme, css: CSS, width: v.w, dsf: v.d });
  const p = await ctx.newPage(); const helper = await ctx.newPage();
  try {
    const r = { name };
    await load(p, name, page);
    if (site.before) await site.before({ p, r });
    await site.expand(p);
    await p.waitForTimeout(1000);
    r.injected = await p.evaluate(() => document.querySelectorAll('style[data-gm]').length);
    Object.assign(r, await runChecks(p));
    for (const [i, item] of r.c.entries()) {
      if (item.rect.w < 2 || item.rect.h < 2) continue;
      let png;
      try { png = await p.locator(`[data-st5c="${i}"]`).screenshot({ timeout: 10000 }); } catch (e) { item.error = e.message.split('\n')[0]; item.fail = true; continue; }
      Object.assign(item, await pixel.analyseAsset(helper, png, item.bg));
      // A white asset on a light background fails. An icon in a warning element needs 3.0 (WCAG non-text).
      item.fail = item.dominant === null || (item.domLum > 0.8 && item.bgLum > 0.5) || (item.warn && item.ratio < 3);
      // A sprite with a fixed fill inside an external <use> cannot be recoloured by page CSS.
      if (item.fail && item.warn && /^warning-/.test(item.icon.replace(/.*#/, ''))) { item.fail = false; item.exception = 'fixed-fill warning sprite'; }
    }
    r.cmp = compare(U, await runSnap(p, false), site.exempt);
    r.unthemedEmpty = U.empty;
    if (site.extras) await site.extras({ p, helper, r, U, pixel });
    r.extraFails = site.extraFails ? site.extraFails(r) : [];
    fs.writeFileSync(path.join(RES, name + '.json'), JSON.stringify(r, null, 1));
    console.log(name, 'injected', r.injected, '5a', `${r.a.length}/${r.aCount}`, '5b', r.b.filter((x) => x.visible).length, '5c', `${r.c.filter((x) => x.fail).length}/${r.c.length}`,
      '6a', `${r.cmp.fails.length}/${r.cmp.checked}`, '6b', r.empty.length, 'extra', JSON.stringify(r.extraFails));
  } catch (e) { console.log(name, 'ERROR', e.message); }
  await ctx.close();
}

(async () => {
  const browser = await launch();
  const jobs = [];
  for (const t of THEMES) for (const v of VIEWS) for (const pg of PAGES) jobs.push([t, v, pg]);
  // Build the theme files one at a time before the parallel runs start.
  const { themeSource } = require('./lib');
  THEMES.forEach(themeSource);
  let i = 0;
  await Promise.all(Array.from({ length: +(process.env.PAR || 4) }, async () => { while (i < jobs.length) await one(browser, ...jobs[i++]); }));
  await browser.close();
})();
