import {request,notebookPath} from './request.mjs';

// A fenced `view` block in a note runs as an interactive page in a sandboxed
// frame. The frame's document has an opaque origin and a policy without
// network access, so the app page is its only channel: the page answers a
// fixed list of read calls about the notebook that holds the note, through the
// ordinary API with the reader's own session. Nothing else is handed over.
const frames=new WeakMap();
const maxHeight=4000,minHeight=48,budget=120,budgetWindow=60000;
let serial=0;
// A frame starts when its block is first on screen, so a note's preview can
// be rebuilt while it is hidden (as it is during editing) without running it.
const visible=new IntersectionObserver(entries=>{
  for(const entry of entries) {
    if(!entry.isIntersecting)continue;
    visible.unobserve(entry.target);
    const state=pendingStart.get(entry.target);
    pendingStart.delete(entry.target);
    if(state && entry.target.isConnected)start(state);
  }
});
const pendingStart=new WeakMap();

function element(tag,props={},children=[]) {
  const node=Object.assign(document.createElement(tag),props);
  node.append(...children);
  return node;
}
const notePath=path=>typeof path==='string' && path.endsWith('.md') && !path.startsWith('/') && !path.split('/').includes('..');
const summary=entry=>({path:entry.path,title:entry.title || '',excerpt:entry.excerpt || '',kind:entry.metadata?.kind || '',status:entry.metadata?.status || '',tags:[...(entry.metadata?.tags || [])]});

async function answer(state,name,args) {
  const base=notebookPath(state.project);
  if(name==='notes.list') {
    const folder=String(args?.folder ?? '').replace(/^\/+|\/+$/g,'');
    const entries=await request(base+'/api/notes');
    return entries.filter(entry=>entry.path.endsWith('.md') && (!folder || entry.path.startsWith(folder+'/'))).map(summary);
  }
  if(name==='notes.read') {
    if(!notePath(args?.path))throw Error('A note path ending in .md is required');
    const note=await request(base+'/api/note?path='+encodeURIComponent(args.path));
    return {path:note.path,title:note.title,content:note.content,revision:note.revision};
  }
  if(name==='notes.search') {
    const query=String(args?.query ?? '').trim().slice(0,200);
    if(!query)return [];
    return (await request(base+'/api/notes?q='+encodeURIComponent(query))).filter(entry=>entry.path.endsWith('.md')).map(summary);
  }
  if(name==='view.theme')return {scheme:document.documentElement.dataset.theme==='dark'?'dark':'light'};
  throw Error('Unknown view call: '+name);
}

window.addEventListener('message',event=>{
  const state=event.source && frames.get(event.source),data=event.data;
  if(!state || !data || data.fmn!==1)return;
  // A second document in the same frame is not the view this page started.
  if(data.hello) {
    if(state.started)stop(state,'This view left its page and was stopped.');
    state.started=true;
    return;
  }
  if(!state.started || state.stopped)return;
  if(Number.isFinite(data.resize)) {
    state.frame.style.height=Math.min(maxHeight,Math.max(minHeight,Math.ceil(data.resize)))+'px';
    return;
  }
  if(!Number.isInteger(data.id) || typeof data.call!=='string')return;
  const reply=message=>event.source.postMessage({fmn:1,id:data.id,...message},'*');
  const now=Date.now();
  if(now-state.windowStart>budgetWindow){state.windowStart=now;state.calls=0;}
  if(++state.calls>budget)return reply({ok:false,error:'Too many calls; wait a minute and try again.'});
  answer(state,data.call,data.args).then(result=>{if(!state.stopped)reply({ok:true,result});},error=>{if(!state.stopped)reply({ok:false,error:error.message || 'The view call failed'});});
});

function stop(state,message) {
  state.stopped=true;
  frames.delete(state.window);
  state.frame.remove();
  state.status.textContent=message;
  state.run.textContent='Run';
  state.run.setAttribute('aria-pressed','false');
}

function start(state) {
  state.frame?.remove();
  const name='fmn-view-'+(++serial);
  const frame=element('iframe',{name,title:'Interactive view',className:'note-view-frame',referrerPolicy:'no-referrer'});
  frame.setAttribute('sandbox','allow-scripts');
  Object.assign(state,{frame,started:false,stopped:false,calls:0,windowStart:Date.now()});
  state.status.textContent='';
  state.run.textContent='Stop';
  state.run.setAttribute('aria-pressed','true');
  if(state.url)frame.src=state.url;
  state.figure.insertBefore(frame,state.code);
  state.window=frame.contentWindow;
  frames.set(state.window,state);
  if(!state.url) {
    // A draft is posted into the frame, which answers with the same policy.
    const form=element('form',{method:'post',action:notebookPath(state.project)+'/api/view/preview',target:name,hidden:true},[element('textarea',{name:'source',value:state.source})]);
    document.body.append(form);
    form.submit();
    form.remove();
  }
}

// Replaces each view code block in rendered note HTML with its frame. `source`
// names the saved revision the HTML shows; without it the HTML is a draft.
// Private drafts never leave the browser, so their views do not run.
export function renderViews(root,{project,path,revision,privateDraft=false}) {
  const blocks=[...root.querySelectorAll('pre > code.language-view')];
  blocks.forEach((code,block)=>{
    const pre=code.parentElement;
    const status=element('span',{className:'note-view-status'});
    status.setAttribute('role','status');
    const bar=element('div',{className:'note-view-bar'},[element('span',{className:'note-view-label',textContent:'Interactive view'}),status]);
    const figure=element('div',{className:'note-view',contentEditable:'false'},[bar]);
    pre.replaceWith(figure);
    pre.hidden=true;
    figure.append(pre);
    const show=element('button',{type:'button',className:'quiet',textContent:'Code'});
    show.setAttribute('aria-expanded','false');
    show.onclick=()=>{pre.hidden=!pre.hidden;show.setAttribute('aria-expanded',String(!pre.hidden));};
    if(privateDraft) {
      status.textContent='Save the note to run this view.';
      pre.hidden=false;show.setAttribute('aria-expanded','true');
      bar.append(show);
      return;
    }
    const url=revision!==undefined?notebookPath(project)+'/api/view?'+new URLSearchParams({path,revision,block}):'';
    const state={project,url,source:code.textContent,figure,code:pre,status};
    state.run=element('button',{type:'button',className:'quiet'});
    state.run.onclick=()=>{pendingStart.delete(figure);visible.unobserve(figure);if(state.stopped || !state.frame?.isConnected)start(state);else stop(state,'Stopped.');};
    bar.append(state.run,show);
    if(url)bar.append(element('a',{className:'button quiet',href:url,target:'_blank',rel:'noopener noreferrer',textContent:'Open'}));
    state.run.textContent='Run';
    state.run.setAttribute('aria-pressed','false');
    pendingStart.set(figure,state);
    visible.observe(figure);
  });
}
