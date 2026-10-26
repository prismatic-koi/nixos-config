---
name: site-theming
description: Load this skill when you write, change, or verify a qutebrowser greasemonkey site style (modules/programs/qutebrowser/greasemonkey/*.css.js) or a Firefox user-content.nix site block. Also load it before you prepare a spawn prompt or review a PR for such a style. It holds the pattern, the design rules, the verification method, and the check scripts. It works in this repository only.
---

# Site theming: style a website with the system theme

A site style recolours one website with the colours of the current theme.
This skill covers the qutebrowser greasemonkey styles and the Firefox
`user-content.nix` site blocks. The Firefox blocks use the same mappings as
`greasemonkey.nix`.

Five rounds of work (#3246, #3248, #3250, #3252, #3254) found the rules
below. Each rule exists because a check failed on a real site. Follow the
rules first. Then run the checks.

## The pattern

- `theme.css.js` in `greasemonkey.nix` sets the `--system-theme-*` variables.
- A site script uses only those variables. It has no hex literal and no Nix interpolation.
- One rule set serves light themes and dark themes.
- Use `color-mix()` and relative colour syntax to make other colours.
- Add one registration entry for each site in `greasemonkey.nix`.

## Design rules

1. **Colour can carry meaning.** Keep status colours, severity colours, and
   data gradients in the same hue family, with theme colours. Example: the
   MetService day and night row. Do not flatten them to one neutral colour.
2. **Set only the lightness to reach a contrast.** To make a hue reach a
   ratio of 4.5, keep its hue and chroma. Set only the OKLCH lightness.
   See `--ms-text-l` in `metservice.css.js` and `--yt-text-l` in
   `youtube.css.js`. Do not mix a hue toward the foreground colour. The hue
   is gone before the contrast is enough.
3. **Warning bars use a solid severity hue.** Take the text colour (black or
   white) from a lightness formula on the hue.
4. **Text and background go together.** If the script sets a text colour on
   an element, it must also set the background. If it sets a background, it
   must also set the text colour.
5. **No white asset on a light background.** Recolour a logo or an icon with
   `fill`, `mask-image`, or `filter`. Or keep a dark bar behind it.
6. **Hide only what the user asks you to hide.** The exception is an empty
   ad slot. Collapse a slot only when it has no `iframe`, `img`, or `video`.
7. **Remove backgrounds that the site does not have.** Examples: the square
   box behind a round search bar, and the layout columns.
8. **Prefer stable class selectors.** Add a comment to each positional
   selector.
9. **Map hashed custom properties at run time.** If the site uses obfuscated
   names, read the site stylesheets. Match each hashed name to a readable
   name by its (light value, dark value) pair. Do not write a hashed name in
   the style. See `youtube.css.js`. As the first step for a new site, run
   `scripts/dump-properties.js` to list the names that share a pair.
10. **Theme both modes.** In qutebrowser, `preferred_color_scheme` follows
    `config.theme.type`. If a site has a dark mode, theme it in both modes.

## Verification method

1. Inject the styles at document start through an init script, with a
   `GM_addStyle` shim. `scripts/lib.js` does this.
2. Use a disk cache of the site responses, so that themed and unthemed runs
   see the same data. Block ad requests. `scripts/lib.js` does both.
   A live site (YouTube) has no cache. Its counts change between runs.
3. Build the real `theme.css.js` values with `scripts/theme-variables.sh`.
   Do not write the values by hand. Do not commit a theme change.
4. Test `everforest-light` first. Then test one dark theme.
5. Test every route in the site navigation. Expand every accordion and drawer.
6. Test at 1440, 1100, and 800 px wide, and at 1100 px with a device scale
   factor of 1.25.
7. Run these checks:

| Check | What it finds | Pass |
|---|---|---|
| 5a | Text contrast against the effective background, including each gradient stop | 4.5 or more. No `url()` image behind text |
| 5b | Site base colours or site gradients left behind | None visible |
| 5c | White assets (logo, icons) on a light background. Icons in warning elements | No white asset on light. 3.0 for a warning icon |
| 6a | Colour with meaning that the theme flattened. It compares with the unthemed page | Chroma 0.06 or more, and hue within 40 degrees of the theme hue |
| 6b | Boxes 40 px high or more with no text or image and a different background | None |
| Header | More than one colour on a row of the top bar | One colour |
| Hidden text | Text that the unthemed page shows and the themed page hides | None |

8. The checks do not find small shape artefacts. Make screenshots of the
   header, the search bar, and the footer with `scripts/screenshot.js`, in
   the themed and the unthemed state. Compare them.
   Test the colour formulas on all palettes with `scripts/palette-test.js`.
   The two-theme runs cover only two palettes.
9. Record which runs used live data.

A check that you did not run is not a pass. Run a check against an empty
style file once, to make sure that the check can fail.

## Scripts

All scripts are in `scripts/`. Run them from the repository root. Each
script has usage text at the top. Results go to `OUT` (default
`./site-theming-out`). That directory is in `.gitignore`. Never commit it.

| Script | Use |
|---|---|
| `theme-variables.sh <theme> [file]` | Write the `theme.css.js` values for a theme. It uses `extendModules` and forces `nx.desktop.theme`. |
| `run-dom-checks.js sites/<site>.js` | Run 5a, 5b, 5c, 6a, and 6b for each theme, view, and page. |
| `summary.js` | Print the pass and fail table of the results. |
| `run-shadow-checks.js sites/<site>.js` | Run the contrast, base-colour, and token checks on a site with shadow DOM (YouTube). |
| `semantic-hue.js <unthemed-dir> <themed-dir>` | Run 6a on shadow-DOM results. |
| `shadow-summary.js` | Group the low-contrast text by colour pair. |
| `screenshot.js sites/<site>.js` | Write named screenshots (regions from the site file) for one theme and view. Use `--theme unthemed` for the reference. |
| `dump-properties.js sites/<site>.js` | Dump the custom properties of `html` in light and dark mode. Group the names by equal (light, dark) pair. |
| `palette-test.js --fg <css> --bg <css>` | Compute the contrast of a CSS colour expression on every palette. It exits 1 if a palette fails. |
| `theme-variables.sh --list` | Print the theme names. |
| `lib.js`, `pixel.js`, `compare.js`, `snapshot.js`, `dom-checks.js`, `shadow-checks.js` | Shared parts. Do not run them. |

Find Playwright and Chromium at run time. The scripts use the `playwright-cli`
package on `PATH`. To use another install, set `PLAYWRIGHT_MODULE` and
`CHROMIUM_PATH`.

Example for MetService (2 pages, 2 themes, 4 views):

```bash
SITE_CSS=modules/programs/qutebrowser/greasemonkey/metservice.css.js \
  PAGES=,extended node .pi/skills/site-theming/scripts/run-dom-checks.js \
  .pi/skills/site-theming/scripts/sites/metservice.js
node .pi/skills/site-theming/scripts/summary.js
```

Example for YouTube (one page, 2 themes, 2 modes):

```bash
S=.pi/skills/site-theming/scripts
UNTHEMED=1 PAGES=search node $S/run-shadow-checks.js $S/sites/youtube.js
SITE_CSS=modules/programs/qutebrowser/greasemonkey/youtube.css.js PAGES=search \
  node $S/run-shadow-checks.js $S/sites/youtube.js
node $S/semantic-hue.js site-theming-out/unthemed site-theming-out/results
```

The environment variables `THEMES`, `VIEWS`, `PAGES`, `MODES`, `PAR`, and
`OUT` change the run. The head of each runner lists them.

### Add a new site

1. Copy `scripts/sites/metservice.js` (light DOM) or `scripts/sites/youtube.js`
   (shadow DOM).
2. Set the page URLs, the ready selector, and `cacheRe` (the hosts to cache).
3. For a light-DOM site, set `checks.siteColours` to the base colours of the
   site as `r,g,b` strings. Set `assetRoots` and `warnRoots` to the selectors
   that hold white assets.
4. Write `expand` to open every collapsed control.
5. Put a site colour with no meaning in `exempt`, with a reason. The reason
   goes in the PR.
6. Set `regions` for the screenshots.
7. Keep site selectors in the site file. Do not put them in the shared checks.

## Practical notes

- The GitHub token cannot create gists. Push screenshots to a branch named
  `<issue>-screenshots`. Link them from the PR. The coordinator deletes the
  branch after merge.
- Workers cannot dispatch workflows. Do not try.
- YouTube dark mode needs the cookie `PREF=f6=400`. The media query alone
  keeps headless Chromium in light mode.
- The signed-out YouTube home has no videos. Use `/hashtag/nixos` as the feed
  page.
- A visibility rule that treats children of a 1 px absolute box as hidden
  skips the Leaflet map pills on MetService. The checks do not use that rule.
- `warning-*` sprite icons have fixed fills inside an external `<use>`. Page
  CSS cannot recolour them. The runner lists them as an exception.

## History

| PR | Result |
|---|---|
| #3246, #3248, #3249 | MetService logo, warning bars, accordions, and nav. First checks: 5a, 5b, 5c |
| #3250, #3251 | Day and night gradient, status colours, header, ad slots. Added 6a, 6b, and the header check |
| #3252, #3253 | Square box behind the search bar. Checks do not find shape artefacts. Hand-written theme files were not enough |
| #3254, #3255 | YouTube in both modes. Added the token mapping at run time and the shadow-DOM checks |
