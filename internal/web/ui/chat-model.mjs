export {createTurnSubmission,attachmentSnapshot} from './chat-composer.mjs';
export const activeStates = new Set(['queued','running','awaiting_confirmation']);
const resumableStates = new Set(['interrupted','cancelled','failed']);
export function validCorpus(value) {
  return value?.version === 1 && ['include','lower','exclude'].includes(value.archives) && ['include','lower','exclude'].includes(value.drafts) && typeof value.history === 'boolean' && Object.keys(value).every(key => ['version','archives','drafts','history','sourceProjects','sourceBindings','noteProjects'].includes(key));
}
export const canContinueRun = run => validCorpus(run?.options?.corpus) && (resumableStates.has(run?.state) || run?.state === 'completed' && run.review && !run.review.complete);
function replaceByID(items, item) {
  const index = items.findIndex(entry => entry.id === item.id);
  return index < 0 ? [...items,item] : items.map((entry,i) => i === index ? {...entry,...item} : entry);
}
export function applyChatEvent(state, event) {
  if (!Number.isSafeInteger(event.sequence) || event.sequence <= (state.eventCursor || 0)) return state;
  if (event.sequence !== (state.eventCursor || 0) + 1) return {...state,gap:true};
  const next = {...state,eventCursor:event.sequence,gap:false,turns:state.turns || [],runs:state.runs || []};
  const data = event.data || {}, prior = next.runs.find(run => run.id === event.run);
  let run = {...prior,id:event.run,operations:prior?.operations || []};
  if (event.type === 'text_delta') {
    const turn = next.turns.find(turn => turn.id === data.turnId);
    next.turns = replaceByID(next.turns,{...turn,id:data.turnId,runId:event.run,role:'assistant',content:(turn?.content || '') + data.text,streaming:true});
  } else if (event.type === 'retracted') {
    next.turns = next.turns.filter(turn => turn.id !== data.turnId);
  } else if (event.type === 'assistant' || event.type === 'user') {
    next.turns = replaceByID(next.turns,{...data,runId:event.run,streaming:false});
  } else if (['tool_started','tool_result','approval_required'].includes(event.type)) {
    run.operations = replaceByID(run.operations,{...data,state:data.state || (event.type === 'approval_required' ? 'awaiting_confirmation' : 'running')});
    if (event.type === 'approval_required') run.state = 'awaiting_confirmation';
  } else if (event.type === 'usage') run.usage = data;
  else if (event.type === 'citations') run.citations = Array.isArray(data) ? data : data.citations || [];
  else if (event.type === 'review_coverage') run.review = data;
  else if (event.type === 'review') {run.reviewProgress = data;run.progress = data.message;}
  else if (event.type === 'progress') run.progress = data.message + (data.round ? ' · round ' + data.round : '');
  else if (event.type === 'state') {
    run = {...run,...data};
    if (!activeStates.has(data.state)) next.turns = next.turns.map(turn => turn.runId === event.run ? {...turn,streaming:false} : turn);
  }
  if (event.run) next.runs = replaceByID(next.runs,run);
  return next;
}
function validCitationPath(path,revision) {
  return typeof path==='string' && path.length>0 && !/[:\\?#\x00-\x1f]/.test(path) && !path.startsWith('/') && !path.split('/').some(part=>!part||part==='.'||part==='..') && /^[a-f0-9]{40}$/.test(revision||'');
}
export function chatCitation(citation, project) {
  if(citation?.kind==='data'){
    if(citation.project!==project||!/^#data-receipt-[a-f0-9]{64}$/.test(citation.url||'')||typeof citation.text!=='string')return null;
    return {kind:'data',text:citation.text,url:citation.url};
  }
  if(citation?.kind==='notebook'||citation?.kind==='repository'&&citation.project&&citation.source){
    if(!/^[a-z0-9][a-z0-9-]{0,63}$/.test(citation.project||'')||!validCitationPath(citation.path,citation.revision))return null;
    const ref={project:citation.project,origin:citation.kind==='repository'?'repository':'notes',path:citation.path,revision:citation.revision};
    const query=new URLSearchParams({project:ref.project,collection:'notes',revision:ref.revision});
    if(ref.origin==='repository'){
      if(!/^[a-f0-9]{32}$/.test(citation.source))return null;
      ref.source=citation.source;
      if(citation.startLine||citation.endLine){
        if(!Number.isInteger(citation.startLine)||!Number.isInteger(citation.endLine)||citation.startLine<1||citation.endLine<citation.startLine)return null;
        ref.startLine=citation.startLine;ref.endLine=citation.endLine;
        query.set('startLine',ref.startLine);query.set('endLine',ref.endLine);
      }
      query.set('origin','repository');query.set('source',ref.source);query.set('path',ref.path);
    }else{
      query.set('note',ref.path);
      try{
        const link=new URL(citation.url||'', 'https://notebook.invalid');
        if(link.origin==='https://notebook.invalid'&&link.pathname==='/'){
          const id=link.searchParams.get('noteId');
          if(/^[a-f0-9]{32}$/.test(id||'')){ref.noteId=id;query.set('noteId',id);}
          if(link.hash)ref.heading=decodeURIComponent(link.hash.slice(1));
        }
      }catch{return null;}
    }
    return {kind:'document',ref,url:'/?'+query+(ref.heading?'#'+encodeURIComponent(ref.heading):'')};
  }
  if(citation?.kind==='repository'){
    try{const url=new URL(citation.url);if(url.protocol!=='https:'||url.hostname!=='github.com'||url.username||url.password||!url.pathname.includes('/blob/'+citation.revision+'/')||!/^[a-f0-9]{40}$/.test(citation.revision))return null;return {kind:'repository',url:url.href};}catch{return null;}
  }
  if (!citation || citation.project && citation.project !== project) return null;
  if (citation.path) {
    const path = citation.path;
    if (!validCitationPath(path,citation.revision)) return null;
    return {kind:'note',path,revision:citation.revision,heading:citation.heading || ''};
  }
  if (citation.url) {
    try {
      const url = new URL(citation.url);
      if (!['https:','http:'].includes(url.protocol) || url.username || url.password) return null;
      return {kind:'source',url:url.href};
    } catch {}
  }
  return null;
}
