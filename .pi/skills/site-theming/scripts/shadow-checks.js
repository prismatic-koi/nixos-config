// In-page checks for sites with shadow DOM (YouTube). Walks open shadow roots.
// page.evaluate(`(${source})()`) returns:
//   low       text with contrast below 4.5 on its effective background
//   leftover  text on a site base colour (BASES) outside media
//   col       colour records for the semantic comparison (see semantic-hue.js)
// BASES lists the site base colours. Edit it for a new site.
() => {
  const cv=document.createElement('canvas');cv.width=cv.height=1;const cx=cv.getContext('2d',{willReadFrequently:true});const cache=new Map();
  const parse=s=>{if(cache.has(s))return cache.get(s);let o;if(!s||s==='transparent'||s==='rgba(0, 0, 0, 0)')o=[0,0,0,0];else{cx.clearRect(0,0,1,1);cx.fillStyle='rgba(0,0,0,0)';cx.fillStyle=s;cx.fillRect(0,0,1,1);const d=cx.getImageData(0,0,1,1).data;o=[d[0],d[1],d[2],d[3]/255]}cache.set(s,o);return o};
  const lin=c=>{c/=255;return c<=0.04045?c/12.92:((c+0.055)/1.055)**2.4};const lum=([r,g,b])=>0.2126*lin(r)+0.7152*lin(g)+0.0722*lin(b);
  const ratio=(a,b)=>{const x=lum(a),y=lum(b);return (Math.max(x,y)+0.05)/(Math.min(x,y)+0.05)};
  const over=(t,b)=>{const a=t[3];return [0,1,2].map(i=>t[i]*a+b[i]*(1-a)).concat(1)};
  const hex=c=>'#'+c.slice(0,3).map(v=>Math.round(v).toString(16).padStart(2,'0')).join('');
  const BASES=['#ffffff','#0f0f0f','#212121','#282828','#f9f9f9','#181818','#f2f2f2','#f1f1f1','#030303','#e5e5e5','#3f3f3f','#272727'];
  const isVideo=e=>e.closest('#movie_player, .html5-video-player, ytd-thumbnail, yt-thumbnail-view-model, .yt-thumbnail-view-model, yt-image, ytd-video-preview, .ytp-chrome-bottom, ytd-moving-thumbnail-renderer, yt-collection-thumbnail-view-model, ytd-playlist-thumbnail, .badge-shape-wiz, yt-thumbnail-badge-view-model, yt-thumbnail-overlay-badge-view-model, yt-avatar-shape, .ytp-ce-element, #player, ytd-engagement-panel-section-list-renderer:not([visibility="ENGAGEMENT_PANEL_VISIBILITY_EXPANDED"]), ytd-miniplayer');
  const visible=e=>{const r=e.getBoundingClientRect();if(r.width<=1||r.height<=1)return false;for(let x=e;x;x=x.parentElement){const s=getComputedStyle(x);if(s.display==='none'||s.visibility==='hidden'||+s.opacity===0)return false}return true};
  // effective bg walking up through shadow roots
  const parentOf=n=>n.parentElement||(n.parentNode&&n.parentNode.host)||null;
  const effBg=e=>{const layers=[];let img=null;for(let x=e;x;x=parentOf(x)){const s=getComputedStyle(x);if(s.backgroundImage!=='none'&&!img)img=(x.tagName.toLowerCase()+' '+s.backgroundImage).slice(0,70);const c=parse(s.backgroundColor);if(c[3]>0){layers.push(c);if(c[3]>=0.999)break}}let bg=[255,255,255,1];for(let i=layers.length-1;i>=0;i--)bg=over(layers[i],bg);return {bg,img}};
  const low=[],leftover=[];let count=0;const seen=new Set();
  const walk=root=>{for(const e of root.querySelectorAll('*')){if(e.shadowRoot)walk(e.shadowRoot);
    if(/^(SCRIPT|STYLE|NOSCRIPT|SVG|PATH)$/i.test(e.tagName))continue;
    const own=[...e.childNodes].some(n=>n.nodeType===3&&n.textContent.trim());if(!own)continue;
    if(!visible(e))continue;count++;
    const s=getComputedStyle(e);const fg=parse(s.color);const {bg,img}=effBg(e);const f=over(fg,bg);const cr=ratio(f,bg);
    const rec={el:e.tagName.toLowerCase()+(e.id?'#'+e.id:'')+(e.classList.length?'.'+[...e.classList].slice(0,2).join('.'):''),text:e.textContent.trim().slice(0,30),fg:hex(f),bg:hex(bg),ratio:+cr.toFixed(2),video:!!isVideo(e),img};
    if(cr<4.5)low.push(rec);
    if(BASES.includes(hex(bg))&&!isVideo(e))leftover.push(rec);
  }};
  walk(document);
  // other check: background colours of big visible blocks
  const big=[];const walk2=root=>{for(const e of root.querySelectorAll('*')){if(e.shadowRoot)walk2(e.shadowRoot);const s=getComputedStyle(e);const c=parse(s.backgroundColor);if(c[3]<0.5)continue;const r=e.getBoundingClientRect();if(r.width*r.height<20000||!visible(e)||isVideo(e))continue;big.push({el:e.tagName.toLowerCase()+(e.id?'#'+e.id:''),bg:hex(c),a:+c[3].toFixed(2)})}};walk2(document);
  const tally={};big.forEach(b=>{const k=b.el+' '+b.bg;tally[k]=(tally[k]||0)+1});
  const thumbs=e=>e.closest('ytd-thumbnail, yt-thumbnail-view-model, .yt-thumbnail-view-model, yt-image, img, ytd-moving-thumbnail-renderer, yt-collection-thumbnail-view-model, yt-avatar-shape, video, canvas');
  const col=[];const cnt={};const walk3=root=>{for(const e of root.querySelectorAll('*')){if(e.shadowRoot)walk3(e.shadowRoot);if(/^(SCRIPT|STYLE)$/i.test(e.tagName)||(thumbs(e)&&!e.closest('badge-shape, .badge-shape-wiz, yt-thumbnail-badge-view-model, yt-thumbnail-overlay-badge-view-model, ytd-thumbnail-overlay-time-status-renderer, .yt-badge-shape'))||!visible(e))continue;
    const s=getComputedStyle(e);const own=[...e.childNodes].some(n=>n.nodeType===3&&n.textContent.trim());
    const sig=e.tagName.toLowerCase()+(e.id?'#'+e.id:'')+(e.classList.length?'.'+[...e.classList].filter(c=>!/^style-scope$/.test(c)).join('.'):'');
    const rec={sig,text:own?e.textContent.trim().slice(0,20):''};
    if(own)rec.color=parse(s.color);const bg=parse(s.backgroundColor);if(bg[3]>=0.5)rec.bg=bg;
    if(/^(svg|path|circle|rect|g)$/i.test(e.tagName)){const f=parse(s.fill);if(f[3]>0.5)rec.fill=f}
    const sk=sig+'|'+rec.text;cnt[sk]=(cnt[sk]||0)+1;rec.key=sk+'#'+cnt[sk];
    if(rec.color||rec.bg||rec.fill){rec.path=(function(){let o=[];for(let x=e;x&&o.length<5;x=x.parentElement||(x.getRootNode&&x.getRootNode().host))o.push(x.tagName.toLowerCase()+(x.id?'#'+x.id:''));return o.join('<')})();col.push(rec)}}};walk3(document);
  const html=getComputedStyle(document.documentElement);
  return {col,textCount:count,low,leftover,big:tally,htmlBg:html.backgroundColor,bodyBg:getComputedStyle(document.body).backgroundColor,appBg:getComputedStyle(document.querySelector('ytd-app')||document.body).backgroundColor};
}
