// Runs the contrast, base-colour and semantic checks on a site with shadow DOM, for each
// theme, colour mode and page. Use it for YouTube.
//
// Usage:
//   SITE_CSS=<site style file> node run-shadow-checks.js sites/<site>.js
//
// Environment:
//   SITE_CSS   Required unless UNTHEMED=1. The site style, for example youtube.css.js.
//   THEMES     Comma list. Default: everforest-light,catppuccin-mocha.
//   MODES      Comma list of the site colour modes. Default: light,dark.
//   PAGES      Comma list of page ids from the site config. Default: the site's default pages.
//   UNTHEMED   1 = run with no injection. The results are the reference for semantic-hue.js.
//   RENAME     1 = rename every hashed custom property in the site responses to a random name.
//              This tests the run-time token mapping of the site style.
//   OUT        Output directory. Default: ./site-theming-out. Results go to OUT/results,
//              or OUT/unthemed when UNTHEMED=1.
// Other variables: see lib.js.
//
// Each run writes <theme>_<mode>_<page>.json and a screenshot. A live site changes
// between runs, so counts vary. Record that the run used live data.
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { launch, newCtx, OUT, themeSource } = require('./lib');

const siteFile = process.argv[2];
const unthemed = !!process.env.UNTHEMED;
if (!siteFile || (!unthemed && !process.env.SITE_CSS)) { console.error('usage: SITE_CSS=<style> node run-shadow-checks.js sites/<site>.js'); process.exit(2); }
const site = require(path.resolve(siteFile));
const CSS = process.env.SITE_CSS && path.resolve(process.env.SITE_CSS);
const CHECK = fs.readFileSync(path.join(__dirname, 'shadow-checks.js'), 'utf8');
const dir = path.join(OUT, unthemed ? 'unthemed' : 'results');
fs.mkdirSync(dir, { recursive: true });
const themes = unthemed ? ['none'] : (process.env.THEMES || 'everforest-light,catppuccin-mocha').split(',');
const modes = (process.env.MODES || 'light,dark').split(',');
const pages = (process.env.PAGES || site.defaultPages.join(',')).split(',');
if (!unthemed) themes.forEach(themeSource);

(async () => {
  const b = await launch();
  for (const th of themes) for (const mode of modes) for (const pg of pages) {
    const name = `${th}_${mode}_${pg}`;
    const ctx = await newCtx(b, { site, theme: unthemed ? null : th, css: CSS, width: 1440, colorScheme: mode, cookies: site.cookies(mode) });
    if (process.env.RENAME) {
      const map = {}; const re = /--t[0-9a-f]{16}\b/g;
      const rnd = (m) => map[m] || (map[m] = '--t' + crypto.randomBytes(8).toString('hex'));
      await ctx.route(site.renameRe || /./, async (route) => {
        if (route.request().method() !== 'GET') return route.continue();
        let r; try { r = await route.fetch(); } catch { return route.continue(); }
        if (!/text\/(css|html)|javascript/.test(r.headers()['content-type'] || '')) return route.fulfill({ response: r });
        const body = (await r.text()).replace(re, rnd); const h = r.headers(); delete h['content-length']; delete h['content-encoding'];
        return route.fulfill({ status: r.status(), headers: h, body });
      });
    }
    const p = await ctx.newPage();
    try {
      await p.goto(site.pages[pg], { waitUntil: 'load', timeout: 60000 });
      await p.waitForTimeout(5000);
      await site.prepare(p, pg);
      const r = await p.evaluate(`(${CHECK})()`);
      if (site.state) r.state = await p.evaluate(site.state);
      r.injected = await p.evaluate(() => document.querySelectorAll('style[data-gm]').length);
      fs.writeFileSync(path.join(dir, name + '.json'), JSON.stringify(r, null, 1));
      await p.screenshot({ path: path.join(dir, name + '.png') });
      console.log(name, 'injected', r.injected, 'text', r.textCount, 'low', r.low.length, 'leftover', r.leftover.length, 'state', JSON.stringify(r.state || null).slice(0, 80));
    } catch (e) { console.log(name, 'ERR', e.message.slice(0, 150)); }
    await ctx.close();
  }
  await b.close();
})();
