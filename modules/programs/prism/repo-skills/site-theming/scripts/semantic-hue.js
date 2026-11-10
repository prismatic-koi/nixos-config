// 6a for shadow-DOM sites: semantic hue comparison against the unthemed run.
// Usage: node semantic-hue.js <unthemed-dir> <themed-dir>
// Both directories hold <theme>_<mode>_<page>.json from run-shadow-checks.js. The unthemed
// files start with "none_". A site colour with chroma 0.08 or more must keep a themed
// chroma of 0.06 or more, and a hue within 40 degrees of its theme family. Blue is exempt:
// links and call-to-action text carry no status meaning.
const fs=require('fs');const {oklch,hex}=require('./compare');
const dh=(a,b)=>{const d=Math.abs(a-b)%360;return Math.min(d,360-d)};
const {THEME_DIR}=require('./lib');
const [,,U,T]=process.argv;
const hueOf=th=>{const t=fs.readFileSync(`${THEME_DIR}/theme-${th}.js`,'utf8');const o={};for(const h of ['red','orange','yellow','green','aqua','blue','purple']){const m=t.match(new RegExp('--system-theme-'+h+': (#[0-9a-f]{6})'));if(m){const c=[1,3,5].map(i=>parseInt(m[1].slice(i,i+2),16));o[h]=c}}return o};
const fam=(c,H)=>{const o=oklch(c);let b=null,bd=999;for(const [n,h] of Object.entries(H)){const d=dh(o.H,oklch(h).H);if(d<bd){bd=d;b=n}}return b};
for(const f of fs.readdirSync(T).filter(f=>f.endsWith('.json'))){
 const [th,mode,pg]=f.replace('.json','').split('_');const uf=`${U}/none_${mode}_${pg}.json`;if(!fs.existsSync(uf))continue;
 const u=JSON.parse(fs.readFileSync(uf)).col,t=Object.fromEntries(JSON.parse(fs.readFileSync(T+'/'+f)).col.map(r=>[r.key,r]));const H=hueOf(th);
 let checked=0,pass=0,exempt=0;const fails=[];const miss=[];
 for(const r of u){if(/^(svg|path|circle|rect|g)\|/.test(r.key))continue;for(const p of ['color','bg']){if(!r[p])continue;const o=oklch(r[p]);if(o.C<0.08)continue;checked++;const tr=t[r.key];if(!tr||!tr[p]){miss.push(r.key+' '+p);continue}
   const fm=fam(r[p],H);if(fm==='blue'){exempt++;continue}const to=oklch(tr[p]);const ok=to.C>=0.06&&dh(to.H,oklch(H[fm]).H)<=40;
   if(ok)pass++;else fails.push({path:r.path,key:r.key,p,fam:fm,site:hex(r[p]),themed:hex(tr[p])})}}
 console.log(f,'checked',checked,'pass',pass,'fail',fails.length,'unmatched',miss.length,'exempt-blue',exempt);
 for(const x of fails.slice(0,8))console.log('   FAIL',JSON.stringify(x));
 if(process.env.V)for(const x of miss.slice(0,6))console.log('   miss',x);
}
