// ==UserScript==
// @name    Userstyle (wikipedia.css)
// @match        *://en.wikipedia.org/*
// @run-at       document-start
// ==/UserScript==

// The cookie name is per-wiki (`enwiki...` is English Wikipedia). This
// script is scoped to en.wikipedia.org only, so the cookie prefix is fixed.
// Another language wiki (de.wikipedia.org, etc.) or a sister project
// (wiktionary.org, commons.wikimedia.org, ...) is untouched by this script:
// it does not match, so neither the cookie nor the class nor the CSS below
// apply there. Extending coverage would need each wiki's own cookie prefix
// (MediaWiki exposes it to page scripts as `wgDBname`), which is left as a
// follow-up rather than guessed at here.
document.cookie =
  'enwikimwclientpreferences=skin-theme-clientpref-os; expires=' +
  new Date('3099').toUTCString() +
  '; path=/';

// The cookie above only changes what the server renders on the *next*
// request. On the current page load, MediaWiki has already server-rendered
// `html` with whatever `skin-theme-clientpref-*` class the cookie held at
// request time (`-day` by default for a first visit). The dark palette
// below is scoped to `html.skin-theme-clientpref-os`, so without the class
// the CSS does not apply and the first paint uses Wikipedia's own light
// colours.
//
// MediaWiki's own inline bootstrap script (emitted at the top of every
// page, before any stylesheet) does exactly this: it reads the
// `enwikimwclientpreferences` cookie and swaps the server-rendered
// `<prefix>-clientpref-<value>` class for whichever value the cookie holds.
// Replicate that swap here, at document-start, so it runs before the page's
// own CSS paints.
function applyThemeClass() {
  const html = document.documentElement;
  if (!html) return false;
  if (html.classList.contains('skin-theme-clientpref-os')) return true;
  const existing = Array.from(html.classList).find((c) =>
    c.startsWith('skin-theme-clientpref-')
  );
  if (existing) {
    html.classList.replace(existing, 'skin-theme-clientpref-os');
  } else {
    html.classList.add('skin-theme-clientpref-os');
  }
  return true;
}

// `GM_addStyle` (see the qutebrowser wrapper,
// qutebrowser/javascript/greasemonkey_wrapper.js) appends a <style> element
// to `document.head`, falling back to `document.documentElement` only when
// `head` does not exist yet:
//
//   const head = document.getElementsByTagName("head")[0];
//   if (head === undefined) {
//       document.documentElement.appendChild(oStyle);
//   } else {
//       head.appendChild(oStyle);
//   }
//
// At document-start `document.documentElement` itself can still be null for
// an instant -- that fallback then dereferences null, which is exactly the
// "Cannot read properties of null (reading 'appendChild')" error this fixes.
// The wrapper's own fallback already proves an appended <style> works from
// `documentElement` just as well as from `head`, so the fix is simply to
// defer the whole call (class swap + GM_addStyle) until `documentElement`
// exists, using the same retry gate. `documentElement` appears at the very
// start of parsing, before any of the page's own stylesheets, so the
// !important overrides above still win the cascade.
function injectStyle() {
  GM_addStyle(`
  /* Remap Wikipedia's own custom properties onto --system-theme-* with
     !important, the same pattern github.css.js uses. Unlike GitHub, this
     override is not gated behind a media query: --system-theme-* already
     reflects a single active palette (it does not itself change with the
     OS colour-scheme preference), so one unconditional block covers both
     the "OS is light" and "OS is dark" cases that skin-theme-clientpref-os
     can select between.

     This does not touch .notheme content. .notheme carries its own
     explicit values for every one of these variables (that is why
     upstream's own light block lists .notheme alongside :root: to give it
     fixed light values rather than letting it inherit the page's active
     palette). An explicit declaration on an element always wins over an
     inherited one, with or without !important, so .notheme keeps its own
     colours automatically -- no extra selector is needed to protect it. */
  html.skin-theme-clientpref-os {
    /* body text, subtle/muted text, placeholders */
    --color-base: var(--system-theme-fg) !important;
    --color-emphasized: var(--system-theme-fg) !important;
    --color-subtle: var(--system-theme-grey1) !important;
    --color-placeholder: var(--system-theme-grey2) !important;

    /* links and visited links -- --color-link* already aliases these, so
       remapping the base variables carries the links for free */
    --color-progressive: var(--system-theme-primary) !important;
    --color-progressive--hover: var(--system-theme-blue) !important;
    --color-progressive--active: var(--system-theme-blue) !important;
    --color-visited: var(--system-theme-purple) !important;
    --color-visited--hover: var(--system-theme-purple) !important;
    --color-visited--active: var(--system-theme-purple) !important;

    /* destructive accent (e.g. "new page" links) */
    --color-destructive: var(--system-theme-red) !important;
    --color-destructive--hover: var(--system-theme-red) !important;

    /* diff views -- keep added/removed distinct and legible */
    --color-content-added: var(--system-theme-green) !important;
    --color-content-removed: var(--system-theme-red) !important;
    --background-color-content-added: var(--system-theme-bg_green) !important;
    --background-color-content-removed: var(--system-theme-bg_red) !important;

    /* page and surface backgrounds -- covers infobox/table backgrounds,
       which use --background-color-neutral(-subtle) and
       --background-color-interactive */
    --background-color-base: var(--system-theme-bg0) !important;
    --background-color-neutral: var(--system-theme-bg1) !important;
    --background-color-neutral-subtle: var(--system-theme-bg_dim) !important;
    --background-color-interactive: var(--system-theme-bg1) !important;

    /* borders, e.g. infobox/table rules and the titlebar underline */
    --border-color-base: var(--system-theme-bg5) !important;
    --border-color-subtle: var(--system-theme-bg4) !important;
    --border-color-muted: var(--system-theme-bg3) !important;
  }
`);
}

if (document.documentElement) {
  // Common case: `documentElement` already exists by the time this script
  // runs. Apply both the class swap and the style injection immediately, in
  // the same tick, before first paint.
  applyThemeClass();
  injectStyle();
} else {
  // `document.documentElement` can be missing for an instant at
  // document-start. Retry once it exists, then run both steps together so
  // neither one can dereference a null node.
  const observer = new MutationObserver(() => {
    if (document.documentElement) {
      observer.disconnect();
      applyThemeClass();
      injectStyle();
    }
  });
  observer.observe(document, { childList: true, subtree: true });
}
