// Takes named screenshots of a site page, for one theme and one view.
//
// Usage:
//   SITE_CSS=<site style> node screenshot.js sites/<site>.js [options]
// Options:
//   --theme <name>     A theme name, or "unthemed" for the reference. Default: everforest-light.
//   --width <px>       View width. Default: 1440.
//   --page <id>        Page id from the site config. Default: the first page.
//   --mode <mode>      Site colour mode (light or dark), for sites that have one. Default: light.
//   --regions <list>   Comma list of region names from the site config. Default: all regions.
// Environment: SITE_CSS (not needed for "unthemed"), OUT, and the variables in lib.js.
//
// A region in the site config is one of:
//   "full"                          the whole page
//   "<css selector>"                the first matching element
//   { clipTo: "<selector>", pad: n }  the top of the page down to the bottom of the element, plus n px
// Files: OUT/shots/<site>_<theme>_<width>_<page>_<region>.png. Do not commit them.
// Push them to a branch named <issue>-screenshots, and link that branch in the PR.
const fs = require('fs');
const path = require('path');
const { launch, newCtx, OUT, themeSource } = require('./lib');

const args = process.argv.slice(2);
const siteFile = args.shift();
const opt = (k, d) => { const i = args.indexOf('--' + k); return i >= 0 ? args[i + 1] : d; };
if (!siteFile) { console.error('usage: SITE_CSS=<style> node screenshot.js sites/<site>.js [--theme t] [--width px] [--page id] [--mode m] [--regions a,b]'); process.exit(2); }
const site = require(path.resolve(siteFile));
const theme = opt('theme', 'everforest-light');
const unthemed = theme === 'unthemed';
const width = +opt('width', 1440);
const mode = opt('mode', 'light');
const pages = site.pages;
const pageId = opt('page', Array.isArray(pages) ? pages[0] : Object.keys(pages)[0]);
const regions = site.regions || { 'full-page': 'full' };
const names = opt('regions', Object.keys(regions).join(',')).split(',');
if (!unthemed && !process.env.SITE_CSS) { console.error('SITE_CSS is required for a themed run'); process.exit(2); }
if (!unthemed) themeSource(theme);

(async () => {
  const b = await launch();
  const ctx = await newCtx(b, { site, theme: unthemed ? null : theme, css: process.env.SITE_CSS && path.resolve(process.env.SITE_CSS), width, colorScheme: mode, cookies: site.cookies ? site.cookies(mode) : undefined });
  const p = await ctx.newPage();
  if (site.pageUrl) {
    await p.goto(site.pageUrl(pageId), { waitUntil: 'load', timeout: 90000 });
    await p.waitForSelector(site.readySelector, { timeout: 60000 });
    await p.waitForLoadState('networkidle', { timeout: 20000 }).catch(() => { });
    await p.evaluate(async () => { for (let y = 0; y < document.body.scrollHeight; y += 600) { scrollTo(0, y); await new Promise((r) => setTimeout(r, 120)); } scrollTo(0, 0); });
  } else {
    await p.goto(pages[pageId], { waitUntil: 'load', timeout: 60000 });
    await p.waitForTimeout(5000);
    if (site.prepare) await site.prepare(p, pageId);
  }
  await p.waitForTimeout(800);
  const dir = path.join(OUT, 'shots'); fs.mkdirSync(dir, { recursive: true });
  for (const n of names) {
    const r = regions[n];
    if (r === undefined) { console.log(n, 'unknown region'); continue; }
    const file = path.join(dir, `${site.name}_${theme}_${width}_${site.pageName ? site.pageName(pageId) : pageId}_${n}.png`);
    try {
      if (r === 'full') await p.screenshot({ path: file, fullPage: true });
      else if (typeof r === 'string') { const l = p.locator(r).first(); await l.scrollIntoViewIfNeeded(); await l.screenshot({ path: file }); }
      else {
        await p.evaluate(() => scrollTo(0, 0));
        const h = await p.evaluate(({ s, pad }) => Math.round(document.querySelector(s).getBoundingClientRect().bottom + pad), { s: r.clipTo, pad: r.pad || 0 });
        await p.screenshot({ path: file, clip: { x: 0, y: 0, width, height: h } });
      }
      console.log(n, '->', file);
    } catch (e) { console.log(n, 'ERR', e.message.split('\n')[0]); }
  }
  await b.close();
})();
