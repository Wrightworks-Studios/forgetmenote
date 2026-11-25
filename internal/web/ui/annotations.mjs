// Whitespace-normalized visible text with a map back to DOM or editor positions.
export function projectSegments(segments) {
  let text='';const points=[];let space=false,spacePoint=null;
  for(const segment of segments) for(let i=0;i<segment.text.length;i++) {
    if(/\s/u.test(segment.text[i])) {if(text && !space){space=true;spacePoint=segment.points?.[i] ?? null;}continue;}
    if(space){text+=' ';points.push(spacePoint);space=false;}
    text+=segment.text[i];points.push(segment.points?.[i] ?? null);
  }
  return {text,points};
}
// The context is 64 code points on each side; 128 code units hold them, so
// a note-length text is never copied.
export function anchorAt(text,from,to) {
  while(from<to && /\s/u.test(text[from]))from++;
  while(to>from && /\s/u.test(text[to-1]))to--;
  return {quote:text.slice(from,to),prefix:Array.from(text.slice(Math.max(0,from-128),from)).slice(-64).join(''),suffix:Array.from(text.slice(to,to+128)).slice(0,64).join('')};
}
export function findAnchor(text,a) {
  if(!a?.quote)return null;
  const candidates=[];
  for(let at=text.indexOf(a.quote);at>=0;at=text.indexOf(a.quote,at+1)) {
    const end=at+a.quote.length;
    const before=a.prefix || '',prefix=at>=before.length && text.startsWith(before,at-before.length),suffix=text.startsWith(a.suffix || '',end);
    candidates.push({from:at,to:end,prefix,suffix});
  }
  const full=candidates.filter(c=>c.prefix && c.suffix);
  let found=full.length===1?full[0]:null;
  // A changed context is acceptable only for a unique quote with other context intact.
  if(!found && candidates.length===1) {
    const c=candidates[0];if((a.prefix && c.prefix)||(a.suffix && c.suffix))found=c;
  }
  return found?{from:found.from,to:found.to}:null;
}
export function projectDOM(root) {
  const segments=[];
  function visit(node) {
    if(node.nodeType===3){segments.push({text:node.data,points:Array.from({length:node.data.length},(_,offset)=>({node,offset}))});return;}
    if(node.nodeType!==1 || node.matches('script,style,button,svg,[hidden],[aria-hidden="true"]'))return;
    const block=/^(P|H[1-6]|LI|TD|TH|PRE|BLOCKQUOTE|DIV|BR)$/.test(node.tagName);
    if(block)segments.push({text:' '});
    node.childNodes.forEach(visit);
    if(block)segments.push({text:' '});
  }
  visit(root);return projectSegments(segments);
}
export function selectionAnchor(root) {
  const selection=window.getSelection();
  if(!selection?.rangeCount || selection.isCollapsed)return null;
  const range=selection.getRangeAt(0);
  if(!root.contains(range.startContainer)||!root.contains(range.endContainer))return null;
  const projection=projectDOM(root);
  let from=-1,to=-1;
  projection.points.forEach((point,i)=>{
    if(point && range.comparePoint(point.node,point.offset)===0 && range.comparePoint(point.node,point.offset+1)===0){if(from<0)from=i;to=i+1;}
  });
  return from<0?null:anchorAt(projection.text,from,to);
}
export function readingRanges(root,comments) {
  const projection=projectDOM(root),ranges=new Map();
  for(const comment of comments) {
    if(comment.resolved || !comment.anchor)continue;
    const found=findAnchor(projection.text,comment.anchor);if(!found)continue;
    const start=projection.points[found.from],end=projection.points[found.to-1];if(!start || !end)continue;
    const range=document.createRange();range.setStart(start.node,start.offset);range.setEnd(end.node,end.offset+1);ranges.set(comment.id,range);
  }
  if(globalThis.CSS?.highlights && globalThis.Highlight)CSS.highlights.set('forgetmenote-comments',new Highlight(...ranges.values()));
  return ranges;
}
