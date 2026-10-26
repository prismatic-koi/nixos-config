// Prints a pass and fail table from the result files.
//
// Usage: node summary.js [results-dir]      Default: ./site-theming-out/results.
// One row per page and theme. The counts add up over all views.
// A run fails when 5a, 5b, 5c, 6a, 6b or an extra check has a failure, or when the run has no result (ERROR).
const fs = require('fs');
const path = require('path');
const dir = process.argv[2] || path.join(process.env.OUT || 'site-theming-out', 'results');
const rows = {};
for (const f of fs.readdirSync(dir).filter((f) => f.endsWith('.json'))) {
  const r = JSON.parse(fs.readFileSync(path.join(dir, f)));
  if (!r.name) continue;
  const [theme, , page] = r.name.split('_');
  const R = rows[page + '|' + theme] || (rows[page + '|' + theme] = { runs: 0, a: 0, aN: 0, b: 0, c: 0, cN: 0, f6a: 0, n6a: 0, ex6a: 0, e6b: 0, extra: 0 });
  R.runs++;
  if (r.a) { R.a += r.a.length; R.aN += r.aCount; }
  if (r.b) R.b += r.b.filter((x) => x.visible).length;
  if (r.c) { R.c += r.c.filter((x) => x.fail).length; R.cN += r.c.length; }
  if (r.cmp) { R.f6a += r.cmp.fails.length; R.n6a += r.cmp.checked; R.ex6a += r.cmp.exempt.length; }
  if (r.empty) R.e6b += r.empty.length;
  R.extra += (r.extraFails || []).length;
}
console.log('| Page | Theme | Runs | 5a fails / text | 5b fails | 5c fails / assets | 6a fails / checked (exempt) | 6b boxes | Extra fails | Result |');
console.log('|---|---|---|---|---|---|---|---|---|---|');
for (const k of Object.keys(rows).sort()) {
  const R = rows[k]; const [page, theme] = k.split('|');
  const ok = !(R.a || R.b || R.c || R.f6a || R.e6b || R.extra);
  console.log(`| ${page} | ${theme} | ${R.runs} | ${R.a} / ${R.aN} | ${R.b} | ${R.c} / ${R.cN} | ${R.f6a} / ${R.n6a} (${R.ex6a}) | ${R.e6b} | ${R.extra} | ${ok ? 'pass' : 'FAIL'} |`);
}
