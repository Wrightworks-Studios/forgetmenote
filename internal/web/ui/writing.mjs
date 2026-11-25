import {overlays} from './overlay.mjs';
import {$,element as node,button as domButton} from './dom.mjs';
import {parseTerms,parseGlossary,glossaryMarkdown} from './writing-terms.mjs';
import {splitMarkdown} from './markdown-model.mjs';
import {projectDOM,findAnchor} from './annotations.mjs';
import {notebookPath} from './request.mjs';

const button=(label,run)=>domButton(label,run,{class:'quiet'});
const normalize=text=>text.replace(/\s+/gu,' ').trim();

// This panel owns disposable local analysis, never the draft or save pipeline.
export function initWriting({getDocument,getProject,editor,api,post,canFix,openDocument,onOpen,notice}) {
 const panel=$('writing-panel'),mobile=matchMedia('(max-width: 900px)');
 let worker,timer,generation=0,busy=false,source='',analysis=null,analysisID=0,lastRequest=null,renderState='',opener,tab='overview',termPage=0,outlineLimit=100,scanID=0,scanAbort,glossary=null,projectID='',fullDocument=null;
 let parsedTerms=[];
 const active=()=>getDocument();
 const identity=c=>c && [c.project,c.origin,c.sourceID||'',c.path,c.local?'':c.revision].join(':');
 function overlay(){panel.setAttribute('role',mobile.matches?'dialog':'complementary');panel.setAttribute('aria-modal',String(mobile.matches&&!panel.hidden));document.body.classList.toggle('writing-open',!panel.hidden);overlays().panel(panel,{active:!panel.hidden,modal:mobile.matches,group:'note-panels',close,covers:[$('main'),$('sidebar'),$('mobile-navigation')]});}
 function close(focus=true){panel.hidden=true;overlay();if(focus&&opener?.isConnected)opener.focus();}
 function stopScan(){scanID++;scanAbort?.abort();scanAbort=null;$('writing-mentions-more').hidden=true;$('writing-mentions-previous').hidden=true;$('writing-scan').disabled=false;}
 function reset(){clearTimeout(timer);worker?.terminate();worker=null;busy=false;generation++;analysis=null;lastRequest=null;fullDocument=null;source='';forgetCount();stopScan();close(false);for(const id of ['writing-stats','writing-findings','writing-outline','writing-mentions'])$(id).replaceChildren();}
 function select(expected,from,to,quote,target,section){
  const ctx=active();if(!ctx)return;
  if(identity(ctx)!==target || source!==expected || ctx.local && editor.value!==expected){notice('The document changed. Run the check again.');return;}
  if(ctx.origin==='repository'&&(ctx.partial||$('source-markdown').hidden)){
   const startLine=expected.slice(0,from).split('\n').length;
   openDocument({project:ctx.project,origin:ctx.origin,source:ctx.sourceID,revision:ctx.revision,path:ctx.path,startLine,endLine:startLine+199}).catch(e=>notice(e.message));return;
  }
  const anchor=typeof quote==='object'?quote:{quote:normalize(quote||expected.slice(from,to)),prefix:'',suffix:'',section};
  if(ctx.local && ctx.editing){
   if(mobile.matches)close(false);
   if(!editor.selectRange(source,from,to,anchor))notice('The document changed. Run the check again.');
  }else {
   // Read-only navigation retains the displayed Markdown and reader mode.
   const root=$(ctx.origin==='repository'?'source-markdown':'preview');
   if(section){
    const element=section.kind==='opening'?root.firstElementChild:root.querySelectorAll(section.kind==='heading'?'h1,h2,h3,h4,h5,h6':'hr')[section.index];
    if(element){if(mobile.matches)close(false);root.tabIndex=-1;root.focus({preventScroll:true});element.scrollIntoView({block:'center'});return;}
   }
   const projection=projectDOM(root),found=findAnchor(projection.text,anchor);
   if(mobile.matches)close(false);
   const start=found&&projection.points[found.from],end=found&&projection.points[found.to-1];
   if(start&&end){const range=document.createRange();range.setStart(start.node,start.offset);range.setEnd(end.node,end.offset+1);root.tabIndex=-1;root.focus({preventScroll:true});const selection=window.getSelection();selection.removeAllRanges();selection.addRange(range);start.node.parentElement.scrollIntoView({block:'center'});}
   else notice('Open Source to locate this passage precisely.');
  }
 }
 function render(){
  if(!analysis || analysisID!==generation)return;
  const ctx=active();if(!ctx)return;
  const expected=source,target=identity(ctx);
  renderState=String(ctx.editing)+String(canFix())+getProject()?.role;
  $('writing-scope').textContent=ctx.local?(ctx.editing?'Current draft':'Displayed note'):'Repository · read only';
  const stats=$('writing-stats');stats.replaceChildren();
  for(const [title,value] of [['Words',analysis.stats.words],['Characters',analysis.stats.characters],['Paragraphs',analysis.stats.paragraphs],['Sentences ≈',analysis.stats.sentences],['Reading time',analysis.stats.words?Math.max(1,Math.round(analysis.stats.readingMinutes))+' min':'0 min']]){const term=node('dt',title),count=node('dd',typeof value==='number'?value.toLocaleString():value);stats.append(term,count);}
  $('writing-status').textContent=analysis.warning||'';
  const outline=$('writing-outline');outline.replaceChildren();
  const ordinals={heading:0,scene:0,opening:0};
  for(const section of analysis.sections.slice(0,outlineLimit)){const location={kind:section.kind,index:ordinals[section.kind]++},li=node('li');li.dataset.level=String(section.level);const jump=button(section.title,()=>select(expected,section.from,section.markerTo,section.title,target,location));li.append(jump,node('small',section.words+' words'+(section.subtreeWords!==section.words?' · '+section.subtreeWords+' with subsections':'')));outline.append(li);}
  $('writing-outline-more').hidden=outlineLimit>=analysis.sections.length;
  $('writing-glossary-save').disabled=getProject()?.kind==='repository'||!['edit','owner'].includes(getProject()?.role);
  const checks=$('writing-findings');checks.replaceChildren();
  const allowed=new Set([...panel.querySelectorAll('[data-writing-rule]:checked')].flatMap(el=>el.dataset.writingRule.split(',')));
  for(const finding of analysis.findings.filter(f=>allowed.has(f.rule))){
   const li=node('li');li.append(node('strong',finding.message),button(finding.text.trim().slice(0,160)||'Show spacing',()=>select(expected,finding.from,finding.to,finding.anchor,target)));
   if(finding.replacement!==undefined && canFix()){
    const expected=source,fix=button('Fix',()=>{
     const sheet=mobile.matches;
     if(sheet)close(false);
     const applied=identity(active())===target&&canFix()&&editor.replaceRange(expected,finding.from,finding.to,finding.replacement);
     if(sheet){panel.hidden=false;overlay();}
     if(!applied)notice('The draft changed or is read only. Run the check again.');
     update(true);$('writing-tab-checks').focus();
    });fix.classList.add('writing-fix');li.append(fix);
   }
   checks.append(li);
  }
  $('writing-check-status').textContent=(analysis.omitted?'Showing the first 200 findings. ':checks.children.length?'':'No findings. ')+'English heuristics · not grammar judgments';
  if($('writing-term-scope').value==='document')renderMentions(analysis.matches,analysis.next,false);
 }
 function renderMentions(matches,next,remote,append=false){
  const expected=source,target=identity(active());
  const list=$('writing-mentions');if(!append)list.replaceChildren();
  for(const match of matches){const li=node('li');li.append(node('strong',match.term));
   if(remote){const a=node('a',match.path+' · line '+match.startLine);a.href=match.citation;a.target='_blank';a.rel='noopener';li.append(a);}
   else li.append(button(match.text,()=>select(expected,match.from,match.to,match.anchor,target)));
   li.append(node('p',match.excerpt));list.append(li);
  }
  const more=$('writing-mentions-more');more.hidden=!next;more.disabled=false;more.textContent=remote?'More matches':'Next matches';
  $('writing-mentions-previous').hidden=remote||termPage===0;
  if(!remote)more.onclick=()=>{termPage=next;update(true);};
 }
 function ensureWorker(){
  if(worker)return;
  worker=new Worker(typeof FMN_WRITING_WORKER_URL==='string' ? FMN_WRITING_WORKER_URL : new URL('./dist/writing-worker.js',import.meta.url),{type:'module'});
  // data-words names the count's document (data-for), so focus mode can count words added per note.
  worker.onmessage=({data})=>{if(data.id!==generation)return;busy=false;if(data.error){$('writing-status').textContent=data.error;countUnavailable();return;}analysis=data.result;analysisID=data.id;$('word-count').textContent=analysis.stats.words+' words';$('word-count').setAttribute('aria-label',analysis.stats.words+' words · open writing tools');Object.assign($('word-count').dataset,{words:analysis.stats.words,for:lastRequest.key});if(!panel.hidden)render();};
  worker.onerror=()=>{busy=false;countUnavailable();$('writing-status').textContent='Writing analysis unavailable. Reload to retry.';};
 }
 function countUnavailable(){$('word-count').textContent='Count unavailable';forgetCount();}
 function forgetCount(){delete $('word-count').dataset.words;delete $('word-count').dataset.for;}
 function update(immediate=false){
  const ctx=active();if(!ctx)return;
  if(projectID!==ctx.project){projectID=ctx.project;parsedTerms=[];glossary=null;$('writing-term-input').value='';$('writing-glossary-path').value='';$('writing-mentions').replaceChildren();}
  if(ctx.partial && (!fullDocument || fullDocument.key!==identity(ctx)))return;
  const next=ctx.partial?fullDocument.content:ctx.content;
  const options={terms:$('writing-term-scope').value==='document'?parsedTerms:[],after:termPage,checks:!panel.hidden};
  const key=identity(ctx),optionsKey=JSON.stringify(options);
  if(lastRequest?.key===key && lastRequest.source===next && lastRequest.options===optionsKey){
   if(analysis && !panel.hidden && renderState!==String(ctx.editing)+String(canFix())+getProject()?.role)render();
   return;
  }
  clearTimeout(timer);generation++;if(next!==source){termPage=0;outlineLimit=100;if(busy){worker?.terminate();worker=null;busy=false;}source=next;}
  options.after=termPage;lastRequest={key,source,options:JSON.stringify(options)};
  const version=generation;
  timer=setTimeout(()=>{if(version!==generation)return;ensureWorker();busy=true;worker.postMessage({id:version,source,options});},immediate?0:220);
 }
 async function open(event){
  const ctx=active();if(!ctx)return;
  opener=event?.currentTarget||document.activeElement;onOpen();panel.hidden=false;overlay();$('writing-close').focus();$('writing-status').textContent='Analyzing…';
  if(ctx.partial){
   const key=identity(ctx),version=++generation;
   try{const ref={origin:ctx.origin,source:ctx.sourceID,revision:ctx.revision,path:ctx.path};const doc=await api(notebookPath(ctx.project)+'/api/document?'+new URLSearchParams(ref));if(version!==generation||identity(active())!==key)return;if(doc.more)throw Error('Writing tools require the complete Markdown file.');fullDocument={key,content:doc.content};}
   catch(e){if(identity(active())===key)$('writing-status').textContent=e.message;return;}
  }
  if(analysis)render();update(true);
 }
 function changeTab(name){tab=name;for(const b of panel.querySelectorAll('[data-writing-tab]')){const selected=b.dataset.writingTab===name;b.setAttribute('aria-selected',String(selected));b.tabIndex=selected?0:-1;}for(const content of panel.querySelectorAll('[data-writing-content]'))content.hidden=content.dataset.writingContent!==name;}
 for(const b of panel.querySelectorAll('[data-writing-tab]'))b.onclick=()=>changeTab(b.dataset.writingTab);
 panel.addEventListener('keydown',event=>{
  const tabs=[...panel.querySelectorAll('[data-writing-tab]')];
  if(tabs.includes(event.target)&&['ArrowLeft','ArrowRight','Home','End'].includes(event.key)){event.preventDefault();const i=event.key==='Home'?0:event.key==='End'?tabs.length-1:(tabs.indexOf(event.target)+(event.key==='ArrowRight'?1:-1)+tabs.length)%tabs.length;tabs[i].click();tabs[i].focus();}
 });
 $('writing-close').onclick=()=>close();mobile.addEventListener('change',overlay);
 $('writing-outline-more').onclick=()=>{outlineLimit+=100;render();};
 $('writing-mentions-previous').onclick=()=>{termPage=Math.max(0,termPage-100);update(true);};
 for(const id of ['writing-open','writing-menu-open','word-count','focus-words','source-writing-open'])$(id).onclick=open;
 for(const control of panel.querySelectorAll('[data-writing-rule]'))control.onchange=render;
 $('writing-term-input').oninput=()=>{stopScan();termPage=0;try{parsedTerms=parseTerms($('writing-term-input').value);$('writing-term-status').textContent='';}catch(e){parsedTerms=[];$('writing-term-status').textContent=e.message;}$('writing-mentions').replaceChildren();update();};
 $('writing-term-scope').onchange=()=>{stopScan();termPage=0;$('writing-mentions').replaceChildren();$('writing-term-status').textContent='';$('writing-scan').hidden=$('writing-term-scope').value==='document';if($('writing-scan').hidden)update(true);};
 async function scan(after='',request=null,append=false){
  const ctx=active();if(!ctx)return;
  stopScan();const id=scanID;scanAbort=new AbortController();const key=identity(ctx);
  try{
   const terms=parseTerms($('writing-term-input').value);if(!terms.length)throw Error('Add a term first.');
   request ||= {origin:ctx.origin,source:ctx.sourceID||'',terms,collection:'notes'};
   $('writing-scan').disabled=true;$('writing-term-status').textContent='Searching saved notes…';
   const result=await post(notebookPath(ctx.project)+'/api/writing/terms',{...request,after},{signal:scanAbort.signal});if(id!==scanID||identity(active())!==key)return;
   renderMentions(result.matches,result.next,true,append);
   $('writing-term-status').textContent=(result.complete?'Search complete':'More pages available')+' · saved '+result.revision.slice(0,8);
   $('writing-mentions-more').onclick=()=>scan(result.next,{...request,revision:result.revision,source:result.source||''},true);
  }catch(e){if(id===scanID && e.name!=='AbortError')$('writing-term-status').textContent=e.message;}
  finally{if(id===scanID)$('writing-scan').disabled=false;}
 }
 $('writing-scan').onclick=()=>scan();
 $('writing-glossary-path').oninput=()=>glossary=null;
 async function glossaryAction(save){
  const ctx=active(),path=$('writing-glossary-path').value.trim();if(!ctx)return;
  const key=identity(ctx);
  try{
   if(!path.endsWith('.md'))throw Error('Choose a .md glossary path.');
   if(save && (getProject()?.kind==='repository'||!['edit','owner'].includes(getProject()?.role)))throw Error('This notebook is read only.');
   if(save && path===ctx.path)throw Error('Choose a separate glossary note to preserve the open draft.');
   const terms=parseTerms($('writing-term-input').value);
   if(!save || !glossary || glossary.path!==path){
    try{const route=ctx.origin==='repository'?'/api/document?'+new URLSearchParams({origin:'repository',path,source:ctx.sourceID,revision:ctx.revision}):'/api/note?'+new URLSearchParams({path});const doc=await api(notebookPath(ctx.project)+route);parseGlossary(doc.content);glossary={path,revision:doc.revision,content:doc.content};}
    catch(e){if(!save||e.status!==404)throw e;glossary={path,revision:'',content:''};}
   }
   if(identity(active())!==key||path!==$('writing-glossary-path').value.trim())return;
   if(save){const result=await post(notebookPath(ctx.project)+'/api/note',{path,revision:glossary.revision,requireCheckpoint:true,content:splitMarkdown(glossary.content).prefix+glossaryMarkdown(terms)});if(identity(active())!==key)return;glossary={path,revision:result.revision,content:result.content};$('writing-term-status').textContent='Glossary saved.';}
   else {parsedTerms=parseGlossary(glossary.content);$('writing-term-input').value=parsedTerms.map(t=>[t.name,...t.aliases].join(' | ')).join('\n');termPage=0;stopScan();update(true);$('writing-term-status').textContent='Glossary loaded.';}
  }catch(e){if(identity(active())===key)$('writing-term-status').textContent=e.message;}
 }
 $('writing-glossary-load').onclick=()=>glossaryAction(false);$('writing-glossary-save').onclick=()=>glossaryAction(true);
 changeTab(tab);
 return {update,reset,close};
}
