// Site config for youtube.com. Read by run-shadow-checks.js.
// Run: SITE_CSS=<path to youtube.css.js> node run-shadow-checks.js sites/youtube.js
// Dark mode needs the cookie PREF=f6=400. The prefers-color-scheme media query alone keeps
// headless Chromium in light mode.
module.exports = {
  name: 'youtube',
  cacheRe: null, // live site: no disk cache
  // Rewrites of hashed names apply to these URLs when RENAME=1.
  renameRe: /youtube\.com\//,
  defaultPages: ['home', 'browse', 'search', 'watch'],
  pages: {
    home: 'https://www.youtube.com/',
    // The signed-out home has no videos and /feed/trending redirects to home.
    browse: 'https://www.youtube.com/hashtag/nixos',
    search: 'https://www.youtube.com/results?search_query=nixos',
    live: 'https://www.youtube.com/results?search_query=nixos&sp=EgJAAQ%253D%253D',
    watch: 'https://www.youtube.com/watch?v=dQw4w9WgXcQ',
  },
  cookies: (mode) => [{ name: 'SOCS', value: 'CAI', domain: '.youtube.com', path: '/' }]
    .concat(mode === 'dark' ? [{ name: 'PREF', value: 'f6=400&tz=Pacific.Auckland', domain: '.youtube.com', path: '/' }] : []),
  // Page state that youtube.css.js writes for the token mapping.
  state: () => ({ stats: window.__ytThemeStats, hashed: ((document.getElementById('yt-theme-tokens') || {}).textContent || '').match(/--t[0-9a-f]{16}/g) || [] }),
  async prepare(p, pg) {
    if (pg === 'watch') {
      // Expand the description, load the comments, and move the video.
      await p.evaluate(async () => { for (let y = 0; y < 2400; y += 400) { scrollTo(0, y); await new Promise((r) => setTimeout(r, 700)); } });
      await p.waitForTimeout(2000);
      await p.evaluate(() => { const m = document.querySelector('#description-inline-expander #expand'); if (m) m.click(); });
      for (let i = 0; i < 6; i++) { await p.evaluate(() => { const c = document.querySelector('ytd-comments, #comments'); if (c) c.scrollIntoView(); else scrollBy(0, 800); }); await p.waitForTimeout(2500); }
      await p.evaluate(() => { const v = document.querySelector('video'); if (v) { v.pause(); v.currentTime = 75; } scrollTo(0, 0); });
      await p.waitForTimeout(2500);
    } else {
      await p.evaluate(async () => { for (let y = 0; y < 1200; y += 400) { scrollTo(0, y); await new Promise((r) => setTimeout(r, 500)); } scrollTo(0, 0); });
      await p.waitForTimeout(1500);
    }
  },
};
