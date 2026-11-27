import {icon} from './icons.mjs';
import {$,element as node,installHTML,contentElement,routeLink,busy} from './dom.mjs';
import {navigationURL} from './navigation.mjs';
import {resolveLink} from './editor.mjs';
import {poll} from './state.mjs';
import {notebookPath} from './request.mjs';

export function documentURL(ref) {return navigationURL({...ref,collection:'notes',hash:ref.heading});}

// The reader owns only displayed source bytes and navigation. Saving and draft
// retention remain with the application's existing editor/navigation owner,
// whose navigation requests (`beginNavigation`/`ownsNavigation`) a file or
// folder opens under.
export function initDocuments({api,post,getContext,notice,openNote,chooseNotebook,currentNotebook,beginNavigation,ownsNavigation,navigationSignal,retainForNavigation,clearDisplayedNote,updateRoute,state,setCollection,updateCreationControls,layout,repositoriesOn=()=>true}) {
 let generation=0,disposeCode,displayed,selection,outlineGeneration=0,downloadTimer,waiting=false,listedRevision='',listedFolder='';
 const href=ref=>documentURL(ref);
 function hide(){generation++;downloadTimer?.stop();waiting=false;disposeCode?.();disposeCode=null;displayed=null;$('source-browser').hidden=true;$('source-document').hidden=true;document.body.classList.remove('source-active');}
 function link(ref,title){const a=node('a',title);a.href=href(ref);return routeLink(a,()=>openDocument(ref).catch(e=>notice(e.message)));}
 async function show(doc,request){
  const version=++generation;displayed=doc;selection=null;document.body.classList.add('source-active');$('source-document').hidden=false;
  $('source-document-meta').textContent=`${doc.path} · ${doc.revision.slice(0,12)} · Read only${doc.startLine?' · Lines '+doc.startLine+'–'+doc.endLine:''}`;
  $('source-github').href=doc.citation;$('source-update').hidden=true;
  const markdown=!!doc.html;$('source-raw-toggle').hidden=!markdown;$('source-raw-toggle').textContent='Code';
  $('source-writing-open').hidden=!/\.(md|markdown)$/i.test(doc.path);
  $('source-markdown').hidden=!markdown;$('source-code').hidden=markdown;$('source-code').replaceChildren();
  installHTML($('source-markdown'),doc.html||'',{resolve:value=>resolveLink(doc.path,value),follow(a,target){
   if(target.project){a.removeAttribute('href');return;}
   const ref={project:doc.project,origin:'repository',source:doc.source,revision:doc.revision,path:target.path,heading:target.hash};
   a.href=href(ref);return ()=>{if(target.path===doc.path&&target.hash){contentElement($('source-markdown'),target.hash)?.scrollIntoView();return;}openDocument(ref).catch(e=>notice(e.message));};
  }});
  for(const image of $('source-markdown').querySelectorAll('img')){
   const target=resolveLink(doc.path,image.getAttribute('src')||'');
   if(!target||target.external||target.project){image.removeAttribute('src');image.alt=image.alt||'External image';continue;}
   image.src=notebookPath(doc.project)+'/api/document/asset?'+new URLSearchParams({source:doc.source,revision:doc.revision,path:target.path});
  }
  $('source-raw-toggle').onclick=()=>{const raw=$('source-code').hidden;$('source-code').hidden=!raw;$('source-markdown').hidden=raw;$('source-raw-toggle').textContent=raw?'Preview':'Code';};
  $('source-back').onclick=()=>openFolder(doc.path.split('/').slice(0,-1).join('/'));
  $('source-copy').onclick=async()=>{try{if(getContext().user?.admin||['owner','edit','comment'].includes(getContext().project?.role))await post(notebookPath(doc.project)+'/api/repository/capture',{source:doc.source,revision:doc.revision,path:doc.path});const url=new URL(doc.citation);if(selection)url.hash=`L${selection.startLine}-L${selection.endLine}`;await navigator.clipboard.writeText(`[${doc.path.replace(/[\\\[\]]/g,'\\$&')}](${url})`);notice('Citation copied.',true);}catch(e){notice(e.message);}};
  $('source-next').hidden=!doc.more&&!doc.cursor;
  $('source-next').onclick=()=>openDocument({...request,source:doc.source,revision:doc.revision,...(doc.cursor?{cursor:doc.cursor}:{startLine:doc.endLine+1,endLine:doc.endLine+200,cursor:''})}).catch(e=>notice(e.message));
  $('source-previous').hidden=(doc.startLine||1)<=1;
  $('source-previous').onclick=()=>openDocument({...request,startLine:Math.max(1,(doc.startLine||1)-200),endLine:(doc.startLine||1)-1,cursor:''}).catch(e=>notice(e.message));
  try{const {readCode}=await import('./dist/source-view.js');if(version!==generation)return;disposeCode=readCode($('source-code'),doc,range=>selection=range);}catch(e){if(version===generation){$('source-code').append(node('pre',doc.content));notice('Syntax highlighting unavailable: '+e.message);}}
  if(version===generation)contentElement($('source-markdown'),request.heading)?.scrollIntoView();
 }
 function browse(data,folder='',collection='notes'){
  downloadTimer?.stop();waiting=false;listedRevision=data.revision;listedFolder=folder;
  const version=++generation,project=getContext().project.id,seenFolders=new Set();document.body.classList.add('source-active');$('source-browser').hidden=false;
  $('source-browser-title').textContent=folder||getContext().project.name;
  $('source-browser-status').textContent=`${data.total} files · ${data.revision.slice(0,12)} · Read only`;
  $('source-file-query').value='';$('source-files').replaceChildren();
  const base={project,origin:'repository',source:data.source,revision:data.revision,pathPrefix:folder?folder+'/':'',collection};
  let queryID=0,timer;
  const ownsQuery=id=>version===generation&&id===queryID;
  function resetFiles(query){
   $('source-files').replaceChildren();seenFolders.clear();
   if(folder&&!query){const li=node('li'),up=node('button','Parent folder');up.prepend(icon('back'));up.className='quiet';up.onclick=()=>openFolder(folder.split('/').slice(0,-1).join('/'));li.append(up);$('source-files').append(li);}
  }
  function render(page,query='',id=queryID){
   const filtered=!!query;
   for(const entry of page.entries){
    const relative=entry.path.slice(base.pathPrefix.length),directory=relative.includes('/')?relative.split('/')[0]:'';
    if(directory&&!filtered){if(seenFolders.has(directory))continue;seenFolders.add(directory);const li=node('li'),a=node('a','▱ '+directory+'/');a.href=navigationURL({project,origin:'repository',folder:base.pathPrefix+directory,collection});routeLink(a,()=>openFolder(base.pathPrefix+directory));li.append(a);$('source-files').append(li);continue;}
    const li=node('li');li.append(link(entry,filtered?entry.path:relative));
    if(entry.coverage||!entry.supported)li.append(node('small',' — '+(entry.coverage||'Unindexed; bounded text preview if available')));
    $('source-files').append(li);
   }
   const more=$('source-files-more');more.hidden=!page.next;more.disabled=false;
   more.onclick=()=>{
    if(!ownsQuery(id)||more.disabled)return;
    return busy(more,async()=>{const next=await api(notebookPath(project)+'/api/documents?'+new URLSearchParams({...base,query,after:page.next}));if(ownsQuery(id))render(next,query,id);},
     {report:e=>notice(e.message),current:()=>ownsQuery(id),retire:true});
   };
  }
  resetFiles('');
  render(data);
  $('source-file-query').oninput=()=>{
   clearTimeout(timer);const id=++queryID,query=$('source-file-query').value;
   $('source-files-more').disabled=true;
   timer=setTimeout(async()=>{
    if(!ownsQuery(id))return;
    try{const result=await api(notebookPath(project)+'/api/documents?'+new URLSearchParams({...base,query}));if(!ownsQuery(id))return;resetFiles(query);render(result,query,id);}
    catch(e){if(ownsQuery(id))notice(e.message);}
   },180);
  };
 }
 async function check(refresh=false,readError=null){
  if(!repositoriesOn())return;
  const selected=getContext().project?.id,version=generation;downloadTimer?.stop();
  const current=()=>version===generation&&selected===getContext().project?.id;
  try{
   if(refresh&&(getContext().user?.admin||getContext().project?.role==='owner'))await post(notebookPath(selected)+'/api/repository/refresh',{});
   if(!current())return;
   const state=await api(notebookPath(selected)+'/api/repository');if(!current())return;
   if(!displayed&&state.connected&&!state.downloaded&&!state.error&&!state.paused){
    waiting=true;$('source-browser-status').textContent='Waiting for the first download. Files will appear here automatically.';
    downloadTimer=poll(()=>check().then(()=>false),{every:1500,active:current});return;
   }
   if(readError&&state.downloaded){$('source-browser-status').textContent=readError.message+' Check connection status in Notebook settings.';return;}
   if(!displayed&&state.connected&&state.downloaded&&(waiting||(!listedRevision&&refresh)||(listedRevision&&state.downloaded!==listedRevision))){await openFolder(listedFolder);return;}
   const text=state.connected?`${state.repository.full_name} · ${state.repository.default_branch} · Refreshed ${state.lastRefresh&&!state.lastRefresh.startsWith('0001')?new Date(state.lastRefresh).toLocaleString():'not yet'} · ${state.index?.embedded||0}/${state.index?.chunks||0} embedded${state.error?' · '+state.error:''}`:'Connection needs setup in Notebook settings.';
   if(displayed){$('source-update').textContent=state.connected&&state.downloaded&&state.downloaded!==displayed.revision?text+' · A newer commit is available. Return to Files to view it.':text;$('source-update').hidden=false;}else $('source-browser-status').textContent=text;
  }catch(e){if(current())notice(e.message);}
 }
 $('source-check').onclick=()=>check(true);
 window.addEventListener('focus',()=>{if(displayed)check();});
 async function outline(after=''){
  const version=++outlineGeneration,project=getContext().project?.id,query=$('outline-query').value;if(!project)return;
  $('outline-status').textContent='Loading map…';if(!after)$('outline-entries').replaceChildren();
  try{const data=await api(notebookPath(project)+'/api/map?'+new URLSearchParams({query,...(after?{after}:{} )}));if(version!==outlineGeneration||project!==getContext().project?.id)return;
   $('outline-status').textContent=`${data.total} pages · ${data.revision.slice(0,12)}`;
   for(const entry of data.entries){const li=node('li'),details=node('details'),summary=node('summary',entry.title||entry.path);details.append(summary);const a=entry.ref.origin==='repository'?link(entry.ref,'Open file'):node('button','Open note');if(entry.ref.origin!=='repository'){a.className='quiet';a.onclick=()=>openNote(entry.path);}details.append(a);for(const h of entry.headings||[]){const button=node('button',h.text);button.className='quiet';button.onclick=()=>openNote(entry.path,h.anchor);details.append(button);}if(entry.symbols?.length)details.append(node('p',entry.symbols.join(' · ')));if(entry.links?.length)details.append(node('p','Links: '+entry.links.join(', ')));li.append(details);$('outline-entries').append(li);}
   $('outline-more').hidden=!data.next;$('outline-more').onclick=()=>outline(data.next);
  }catch(e){if(version===outlineGeneration)$('outline-status').textContent=e.message;}
 }
 $('notebook-outline').ontoggle=()=>{if($('notebook-outline').open)outline();};$('outline-find').onclick=()=>outline();$('outline-query').onkeydown=e=>{if(e.key==='Enter')outline();};
 // Opens a repository file in place of the displayed note or folder, in its
 // own notebook.
 async function openDocument(ref) {
  const target=ref.project||getContext().project?.id;
  if(target!==getContext().project?.id){const selected=await chooseNotebook(target);if(!ownsNavigation(selected)||getContext().project?.id!==target)return;}
  if(getContext().saving||!getContext().project||!retainForNavigation())return;
  const request=beginNavigation(),selected=getContext().project,sidebarRequest=layout.version();
  const query={origin:'repository',source:ref.source||'',revision:ref.revision||'',path:ref.path,startLine:ref.startLine||1,endLine:ref.endLine||200,...(ref.cursor?{cursor:ref.cursor}:{})};
  const doc=await api(notebookPath(target)+'/api/document?'+new URLSearchParams(query));
  if(!ownsNavigation(request)||getContext().project!==selected||getContext().saving||!retainForNavigation())return;
  clearDisplayedNote();
  $('document').hidden=$('folder-browser').hidden=$('empty').hidden=true;
  $('path').textContent=doc.path;state('Read only · '+doc.revision.slice(0,12));
  updateRoute({origin:'repository',source:doc.source,path:doc.path,revision:doc.revision,startLine:query.startLine,endLine:query.endLine,cursor:query.cursor,hash:ref.heading});
  await show(doc,{...ref,...query,project:target});
  if(!ownsNavigation(request))return;updateCreationControls();check();layout.closeOnPhone(sidebarRequest);return request;
 }
 // Lists a repository folder in place of the displayed note or folder.
 async function openFolder(path='',closeSidebar=true,nextCollection=getContext().collection){
  if(getContext().saving||!retainForNavigation())return;
  const request=beginNavigation(),signal=navigationSignal(),selected=getContext().project,sidebarRequest=layout.version();
  let data,error;
  try{data=await api(currentNotebook()+'/api/documents?'+new URLSearchParams({origin:'repository',pathPrefix:path?path+'/':'',collection:nextCollection}),{signal});}catch(e){error=e;data={entries:[],total:0,revision:''};}
  if(!ownsNavigation(request)||getContext().project!==selected||getContext().saving||!retainForNavigation())return;
  clearDisplayedNote(path);
  $('document').hidden=$('folder-browser').hidden=$('empty').hidden=true;
  setCollection(nextCollection);
  const {project}=getContext();
  if(project.kind==='repository'){
   $('collection-drafts').hidden=$('collection-archives').hidden=!data.notebookLayout;
   $('browse-root').querySelector('span').textContent=data.notebookLayout?'Notes':'Files';
  }
  $('path').textContent=project.name;state('Read-only repository');browse(data,path,getContext().collection);
  // With the repositories module off, the notebook's files stay stored but
  // nothing reads or refreshes them.
  $('source-check').hidden=!repositoriesOn();
  if(!repositoriesOn())$('source-browser-status').textContent='The repositories module is turned off on this instance, so this notebook\'s files are not available.';
  else{if(error)$('source-browser-status').textContent=error.message+' Check connection status in Notebook settings.';check(false,error);}
  updateRoute({origin:'repository',folder:path});updateCreationControls();if(closeSidebar)layout.closeOnPhone(sidebarRequest);return request;
 }
 return {open:openDocument,openFolder,hide,get current(){return displayed;},projectChanged(){hide();outlineGeneration++;$('notebook-outline').open=false;$('outline-query').value='';$('outline-entries').replaceChildren();}};
}
