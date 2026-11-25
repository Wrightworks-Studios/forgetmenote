import {fromMarkdown} from 'mdast-util-from-markdown';
import {splitMarkdown} from '../internal/web/ui/markdown-model.mjs';
import {validateTerms} from '../internal/web/ui/writing-terms.mjs';
export {parseTerms} from '../internal/web/ui/writing-terms.mjs';

// Prose is CommonMark without GFM, as remark-parse (which wraps this parser)
// reads it, so a table stays one paragraph of prose.
const writingVersion=1;
const wordPattern=/[\p{L}\p{N}][\p{L}\p{N}\p{M}]*(?:['’][\p{L}\p{N}\p{M}]+)*/gu;
const stopWords=new Set('a an and are as at be but by for from had has have he her his i in is it its of on or she that the their they this to was we were with you your'.split(' '));
const words=text=>[...text.matchAll(wordPattern)];
const sentenceCount=text=>text.trim()?Math.max(1,(text.match(/[.!?]+(?:["'”’)]*)(?=\s|$)/g)||[]).length):0;

// Each visible character retains its original UTF-16 range. Entity/escape
// expansion can be navigated, but fixes are only offered for literal source.
function mappedText(raw,value,start) {
 const points=[];let r=0,v=0;
 while(v<value.length){
  let size=1,visible=1;
  if(raw[r]==='\\' && /[!"#$%&'()*+,\-./:;<=>?@[\]\\^_`{|}~]/.test(raw[r+1]||''))size=2;
  else if(raw[r]==='&') {const m=/^&(?:#[xX][\da-fA-F]+|#\d+|[a-zA-Z][a-zA-Z\d]+);/.exec(raw.slice(r));if(m){size=m[0].length;const decoded=fromMarkdown(m[0]).children[0]?.children?.[0]?.value;if(decoded){visible=decoded.length;}}}
  else if(raw[r]==='\r' && raw[r+1]==='\n')size=2;
  for(let j=0;j<visible && v<value.length;j++,v++)points.push({from:start+r,to:start+r+size});
  r+=size;
 }
 return points;
}
function proseBlock(node,source,offset) {
 let text='',points=[];
 function visit(n){
  const from=offset+n.position.start.offset,to=offset+n.position.end.offset;
  if(n.type==='html')return;
  if(n.type==='link' && source[from]==='<'){text+=' ';points.push({from,to,synthetic:true});return;}
  if(n.type==='text') {
   const raw=source.slice(from,to),value=n.value;
   const mapped=raw===value?null:mappedText(raw,value,from);
   for(let i=0;i<value.length;i++){text+=value[i];points.push(mapped?mapped[i]:{from:from+i,to:from+i+1});}
  }else if(n.type==='break'){text+='\n';points.push({from,to});}
  else if(['emphasis','strong','delete','link','linkReference','paragraph','heading'].includes(n.type))for(const child of n.children||[])visit(child);
  else {text+=' ';points.push({from,to,synthetic:true});}
 }
 visit(node);
 // Bare URLs and GFM task markers are not prose. Keep ranges stable.
 text=text.replace(/(?:https?:\/\/|www\.)[^\s<>]+|^\[[ xX]\]\s/g,m=>' '.repeat(m.length));
 return {text,points,from:offset+node.position.start.offset,to:offset+node.position.end.offset,heading:node.type==='heading',level:node.depth||0};
}
export function projectWriting(source) {
 const parts=splitMarkdown(source),offset=parts.prefix.length;
 if(parts.error)return {blocks:[],sections:[],warning:parts.error};
 const tree=fromMarkdown(parts.body),blocks=[],sections=[];
 function visit(node){
  if(node.type==='heading'||node.type==='paragraph'){
   const block=proseBlock(node,source,offset);blocks.push(block);
   if(block.heading)sections.push({title:block.text.trim(),level:block.level,kind:'heading',from:block.from,markerTo:block.to,to:source.length,words:0,subtreeWords:0});
  }else if(node.type==='thematicBreak'){
   sections.push({title:'Scene break',level:7,kind:'scene',from:offset+node.position.start.offset,markerTo:offset+node.position.end.offset,to:source.length,words:0,subtreeWords:0});
  }else if(!['code','html','definition','image','imageReference'].includes(node.type))for(const child of node.children||[])visit(child);
 }
 visit(tree);
 if(blocks.length && (!sections.length||blocks[0].from<sections[0].from))sections.unshift({title:'Opening',level:0,kind:'opening',from:offset,markerTo:blocks[0].to,to:source.length,words:0,subtreeWords:0});
 let index=0;
 for(const block of blocks){while(index+1<sections.length && sections[index+1].from<=block.from)index++;if(sections[index])sections[index].words+=words(block.text).length;}
 const stack=[];
 for(let i=0;i<sections.length;i++){
  const section=sections[i];section.to=sections[i+1]?.from??source.length;section.subtreeWords=section.words;
  while(stack.length && sections[stack.at(-1)].level>=section.level)stack.pop();
  section.parent=stack.at(-1)??-1;stack.push(i);
 }
 for(let i=sections.length-1;i>=0;i--){const section=sections[i];if(section.parent>=0)sections[section.parent].subtreeWords+=section.subtreeWords;}
 return {blocks,sections,warning:''};
}
function range(block,start,end){return {from:block.points[start]?.from??block.from,to:block.points[end-1]?.to??block.to};}
function anchor(block,start,end){
 const selected=block.text.slice(start,end),leading=selected.length-selected.trimStart().length,trailing=selected.length-selected.trimEnd().length;
 const fold=text=>text.replace(/\s+/gu,' ');
 return {quote:fold(selected.trim()),prefix:fold(block.text.slice(0,start+leading)).slice(-64),suffix:fold(block.text.slice(end-trailing)).slice(0,64)};
}
function findMentions(projection,terms,after=0,limit=100) {
 terms=validateTerms(terms);const matches=[];let seen=0,more=false;
 const patterns=terms.map(term=>({term:term.name,pattern:new RegExp('(?<![\\p{L}\\p{N}\\p{M}])(?:'+[term.name,...term.aliases].sort((a,b)=>b.length-a.length).map(s=>s.replace(/[.*+?^${}()|[\]\\]/g,'\\$&')).join('|')+')(?![\\p{L}\\p{N}\\p{M}])','giu')}));
 outer:for(const block of projection.blocks){
  const found=[];
  for(const {term,pattern} of patterns)for(const m of block.text.matchAll(pattern)){
   const start=m.index,end=start+m[0].length;
   found.push({term,text:m[0],...range(block,start,end),anchor:anchor(block,start,end),excerpt:block.text.slice(Math.max(0,start-45),Math.min(block.text.length,end+65))});
  }
  found.sort((a,b)=>a.from-b.from||a.term.localeCompare(b.term));
  for(const match of found){if(seen++<after)continue;if(matches.length===limit){more=true;break outer;}matches.push(match);}
 }
 return {matches,next:more?after+matches.length:0};
}
export function analyzeWriting(source,{terms=[],after=0,limit=100,checks=true}={},projection=projectWriting(source)) {
 const stats={words:0,characters:0,sentences:0,paragraphs:0,readingMinutes:0},findings=[];let omitted=0;
 function add(block,start,end,rule,message,replacement){
  if(block.points.slice(start,end).some(point=>point.synthetic))return;
  const span=range(block,start,end),text=source.slice(span.from,span.to);
  if(findings.length===200){omitted++;return;}
  const finding={...span,rule,message,text,anchor:anchor(block,start,end)};
  if(replacement!==undefined && text===block.text.slice(start,end))finding.replacement=replacement;
  findings.push(finding);
 }
 for(const block of projection.blocks){
  const tokens=words(block.text);stats.words+=tokens.length;stats.characters+=[...block.text.trim()].length;
  if(!block.heading && tokens.length){stats.paragraphs++;stats.sentences+=sentenceCount(block.text);}
  if(!checks||block.heading)continue;
  // Tables are valid Markdown, but spacing and sentence heuristics do not fit.
  if(/^\s*\|/m.test(block.text)&&/^\s*\|?[ :|-]+\|[ :|-]*$/m.test(block.text))continue;
  for(const m of block.text.matchAll(/[\t ]{2,}(?![\t ])/g)){
   if(block.text[m.index+m[0].length]==='\n'||m.index+m[0].length===block.text.length)continue;
   add(block,m.index,m.index+m[0].length,'spacing','Repeated spacing',' ');
  }
  for(const m of block.text.matchAll(/[\t ]+[,.!?;:]/g))add(block,m.index,m.index+m[0].length,'punctuation','Space before punctuation',m[0].trimStart());
  const recent=new Map(),phrases=new Map();
  for(let i=0;i<tokens.length;i++){
   const token=tokens[i],key=token[0].toLowerCase(),previous=tokens[i-1];
   if(previous && previous[0].toLowerCase()===key && /^[\t ]+$/.test(block.text.slice(previous.index+previous[0].length,token.index))){
    add(block,previous.index+previous[0].length,token.index+token[0].length,'duplicate','Adjacent repeated word','');
   }else if(!stopWords.has(key)&&key.length>=4 && recent.has(key)&&i-recent.get(key)<=35)add(block,token.index,token.index+token[0].length,'repetition','Nearby repeated word');
   recent.set(key,i);
   if(i>=2){const phrase=tokens.slice(i-2,i+1).map(t=>t[0].toLowerCase()).join(' ');if(phrases.has(phrase)&&i-phrases.get(phrase)>=3&&i-phrases.get(phrase)<=60)add(block,tokens[i-2].index,token.index+token[0].length,'phrase','Nearby repeated phrase');phrases.set(phrase,i);}
  }
  for(const m of block.text.matchAll(/[^.!?]+(?:[.!?]+|$)/g))if(words(m[0]).length>35)add(block,m.index,m.index+m[0].length,'sentence','Long sentence · over 35 words');
  if(tokens.length>150)add(block,0,Math.min(block.text.length,120),'paragraph','Dense paragraph · over 150 words');
 }
 stats.readingMinutes=stats.words/225;
 return {version:writingVersion,stats,sections:projection.sections,findings,omitted,warning:projection.warning,...findMentions(projection,terms,after,limit)};
}
