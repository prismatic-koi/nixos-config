// Shared helpers for the site-theming scripts.
//
// Finds Playwright and Chromium at run time, builds a browser context with a
// disk cache and an ad block list, and injects the theme and site styles.
//
// Environment (all optional):
//   PLAYWRIGHT_MODULE   Directory of the "playwright" npm package.
//   CHROMIUM_PATH       Chromium executable. PLAYWRIGHT_MCP_EXECUTABLE_PATH also works.
//   THEME_DIR           Directory with theme-<name>.js files. Default: <OUT>/themes.
//   CACHE_DIR           Disk cache for site responses. Default: <OUT>/cache.
//   OUT                 Output directory. Default: ./site-theming-out (git-ignored).
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const OUT = path.resolve(process.env.OUT || 'site-theming-out');
const THEME_DIR = path.resolve(process.env.THEME_DIR || path.join(OUT, 'themes'));
const CACHE_DIR = path.resolve(process.env.CACHE_DIR || path.join(OUT, 'cache'));

function which(cmd) {
  try { return execFileSync('which', [cmd], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim(); } catch { return null; }
}

// The playwright-cli package (pkgs/playwright-cli.nix) installs a wrapper script. The
// script names the Chromium path and the wrapped binary, and the wrapped binary sits
// next to the package's node_modules.
function wrapperText() {
  const bin = which('playwright-cli');
  if (!bin) return null;
  try { return fs.readFileSync(fs.realpathSync(bin), 'utf8'); } catch { return null; }
}

function findPlaywright() {
  const candidates = [];
  if (process.env.PLAYWRIGHT_MODULE) candidates.push(process.env.PLAYWRIGHT_MODULE);
  const w = wrapperText();
  const m = w && w.match(/"([^"]+)\/bin\/\.playwright-cli-wrapped"/);
  if (m) candidates.push(path.join(m[1], 'lib/node_modules/@playwright/cli/node_modules/playwright'));
  candidates.push('playwright');
  for (const c of candidates) {
    try { return require(c); } catch { /* try the next candidate */ }
  }
  throw new Error('Playwright not found. Set PLAYWRIGHT_MODULE, or put playwright-cli on PATH.');
}

function findChromium() {
  const w = wrapperText();
  const m = w && w.match(/PLAYWRIGHT_MCP_EXECUTABLE_PATH=\$\{[^}]*?-'([^']+)'/);
  const candidates = [process.env.CHROMIUM_PATH, process.env.PLAYWRIGHT_MCP_EXECUTABLE_PATH, m && m[1], which('chromium'), which('chromium-browser'), which('google-chrome')];
  for (const c of candidates) if (c && fs.existsSync(c)) return c;
  throw new Error('Chromium not found. Set CHROMIUM_PATH, or put playwright-cli or chromium on PATH.');
}

// Classic scrollbars show, as in qutebrowser, so --hide-scrollbars is off.
const launch = () => findPlaywright().chromium.launch({ executablePath: findChromium(), headless: true, ignoreDefaultArgs: ['--hide-scrollbars'] });

// A GM_addStyle shim. The script marks each style element with data-gm.
const SHIM = `window.GM_addStyle=function(css){const s=document.createElement('style');s.textContent=css;s.setAttribute('data-gm','1');(document.head||document.documentElement).appendChild(s);return s;};`;
// Run at document end, as Greasemonkey does by default.
const wrap = (src) => SHIM + `(function(){const run=()=>{${src}};if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',run);else run();})();`;

const BLOCK = /doubleclick|googlesyndication|googletagmanager|google-analytics|adservice|amazon-adsystem|facebook\.net|hotjar|newrelic|nr-data|adnxs|criteo|taboola|outbrain|pubmatic|rubicon|casalemedia|openx|smartadserver|moatads|scorecardresearch|chartbeat|permutive|sentry/;

// Serve cached GET responses for hosts that match cacheRe. Fetch and store the rest.
async function cacheRoutes(ctx, cacheRe) {
  fs.mkdirSync(CACHE_DIR, { recursive: true });
  await ctx.route('**/*', async (route) => {
    const req = route.request(); const url = req.url();
    if (BLOCK.test(url)) return route.abort();
    if (!cacheRe || req.method() !== 'GET' || !cacheRe.test(url)) return route.continue();
    const f = path.join(CACHE_DIR, crypto.createHash('sha1').update(url).digest('hex'));
    if (fs.existsSync(f + '.json')) {
      const m = JSON.parse(fs.readFileSync(f + '.json'));
      return route.fulfill({ status: m.status, headers: m.headers, body: fs.readFileSync(f) });
    }
    try {
      const r = await route.fetch(); const body = await r.body();
      if (r.status() === 200) {
        const h = r.headers(); delete h['content-encoding']; delete h['content-length'];
        fs.writeFileSync(f, body); fs.writeFileSync(f + '.json', JSON.stringify({ status: 200, headers: h, url }));
      }
      return route.fulfill({ response: r, body });
    } catch { return route.abort(); }
  });
}

// Return the theme.css.js text. Build it with theme-variables.sh when the file is missing.
function themeSource(theme) {
  const f = path.join(THEME_DIR, `theme-${theme}.js`);
  if (!fs.existsSync(f)) {
    fs.mkdirSync(THEME_DIR, { recursive: true });
    execFileSync(path.join(__dirname, 'theme-variables.sh'), [theme, f], { stdio: ['ignore', 'inherit', 'inherit'] });
  }
  return fs.readFileSync(f, 'utf8');
}

// opts: site (config object), theme (omit for an unthemed run), css (site style path),
// width, dsf, colorScheme, cookies.
async function newCtx(browser, opts) {
  const { site, theme, css, width = 1440, dsf = 1, colorScheme, cookies } = opts;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, deviceScaleFactor: dsf, locale: 'en-US', ...(colorScheme ? { colorScheme } : {}) });
  await cacheRoutes(ctx, site.cacheRe);
  if (cookies && cookies.length) await ctx.addCookies(cookies);
  if (theme) {
    await ctx.addInitScript(wrap(themeSource(theme)));
    await ctx.addInitScript(wrap(fs.readFileSync(css, 'utf8')));
  }
  return ctx;
}

module.exports = { launch, newCtx, wrap, OUT, THEME_DIR, themeSource };
