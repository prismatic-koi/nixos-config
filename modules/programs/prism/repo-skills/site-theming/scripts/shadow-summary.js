// Groups the low-contrast text of shadow-DOM runs by colour pair.
// Usage: node shadow-summary.js [results-dir]   Default: ./site-theming-out/results.
const fs=require('fs');const dir=process.argv[2]||require('path').join(process.env.OUT||'site-theming-out','results');
for(const f of fs.readdirSync(dir).filter(f=>f.endsWith('.json'))){const r=JSON.parse(fs.readFileSync(dir+'/'+f));if(!r.low)continue;
 const g={};r.low.forEach(l=>{const k=`${l.fg} on ${l.bg} ${l.video?'(video)':''}`;(g[k]=g[k]||[]).push(l)});
 console.log('##',f,'low',r.low.length,'leftover',r.leftover.map(l=>l.el+' '+l.bg+' '+l.text).join(' | '));
 for(const [k,v] of Object.entries(g))console.log('  ',k,v.length,'min',Math.min(...v.map(x=>x.ratio)),v.slice(0,3).map(x=>x.el+':'+x.text).join(' | '));}
