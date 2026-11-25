import {openDialog} from './overlay.mjs';
import {confirmAction} from './feedback.mjs';
import {notebookPath} from './request.mjs';
import {$,downloadBlob,keepTyped} from './dom.mjs';
// The retained-draft chooser and the Compare dialog. The chooser picks which
// of a note's retained drafts this tab opens; Compare owns the reviewed base
// until the user explicitly adopts or discards it. `scope(path)` names a note's
// drafts in the browser's draft storage (`getDrafts()`).
export function initDraftRecovery({api,getNote,getProject,getContent,getDrafts,scope,openNote,applyDraft,notice,beforeReview}) {
 const dialog=$('reconcile-dialog'),chooser=$('draft-candidates-dialog');
 const readScratch=()=>getDrafts().read('reconcile',scope()),writeScratch=value=>getDrafts().write('reconcile',scope(),value);
 let choice=null;
 function cancelChoice(){if(choice){const finish=choice;choice=null;chooser.close();finish(false)}}
 // Resolves to the retained draft to open, null for the shared version, or
 // false when the person cancelled or `active()` no longer holds.
 async function choose(path,active,force=false){
  const drafts=getDrafts(),candidates=drafts.candidates(scope(path)),own=candidates.find(c=>c.owner===drafts.owner);
  // A durable explicit resolution owns reopening this tab. Other retained
  // branches stay available through the chooser, not automatic re-adoption.
  if(!force && !own && drafts.resolved(scope(path)))return null;
  if(!force && (own || candidates.length<2))return own || candidates[0] || null;
  return new Promise(resolve=>{
   const finish=value=>{choice=null;chooser.close();resolve(active()?value:false)};
   choice=resolve;$("draft-candidates-list").replaceChildren();
   for(const candidate of candidates){
    const section=document.createElement('section');
    const button=document.createElement('button');button.type='button';
    button.textContent=(candidate.owner===drafts.owner?'This tab':candidate.owner?'Another retained draft':'Legacy draft')+' · '+(candidate.draft.content.trim().slice(0,120)||'(empty draft)');
    button.onclick=()=>finish(candidate);
    const details=document.createElement('details'),summary=document.createElement('summary'),text=keepTyped(document.createElement('textarea'));
    summary.textContent='Preview full draft'+(candidate.draft.savedAt?' · '+new Date(candidate.draft.savedAt).toLocaleString():'');
    text.readOnly=true;text.rows=8;text.value=candidate.draft.content;text.setAttribute('aria-label','Retained draft content');
    details.append(summary,text);
    if(candidate.scratch!==null){
     const label=document.createElement('p'),scratch=keepTyped(document.createElement('textarea'));label.textContent='Unapplied reconciliation text';
     scratch.readOnly=true;scratch.rows=8;scratch.value=candidate.scratch;scratch.setAttribute('aria-label','Retained reconciliation text');details.append(label,scratch);
    }
    section.append(button,details);$("draft-candidates-list").append(section);
   }
   $("draft-candidates-shared").onclick=()=>finish(null);openDialog(chooser);
  });
 }
 function refreshCandidates(){
  try{const drafts=getDrafts(),others=getNote() && drafts.candidates(scope()).filter(c=>c.owner!==drafts.owner);$("draft-candidates-open").hidden=!others?.length;$("draft-candidates-open").textContent='Other retained drafts ('+(others?.length||0)+')'}catch{$("draft-candidates-open").hidden=true}
 }
 $("draft-candidates-close").onclick=cancelChoice;
 chooser.addEventListener('close',()=>{if(choice){const finish=choice;choice=null;finish(false)}});
 $("draft-candidates-open").onclick=()=>{if(getNote())openNote(getNote().path,'',true)};
 window.addEventListener('storage',event=>{if(/^gitnotes:(draft|reconcile):/.test(event.key||''))refreshCandidates()});
 dialog.addEventListener('close',refreshCandidates);

 let review=null,busy=false;
 const matches=()=>review && getNote()?.path===review.path && getProject()?.id===review.project;
 function setBusy(value){
  busy=value;$('reconcile-content').readOnly=value;
  for(const id of ['reconcile-apply','reconcile-discard','reconcile-close'])$(id).disabled=value;
 }
 function preserveCombined(){
  if(!matches())return;
  try{writeScratch($('reconcile-content').value)}catch{notice('This browser cannot preserve reconciliation edits. Keep this dialog open and copy your text before leaving.')}
 }
 $('reconcile-content').oninput=preserveCombined;
 const latest=async()=>{
  const path=encodeURIComponent(getNote().path),saved=await api(notebookPath(getProject().id)+'/api/note?path='+path);
  // Private intent does not prove live eligibility. Decide from the owner's
  // current committed note; eligible notes also include accepted pending text.
  if(saved.path==='AGENTS.md' || !saved.path.endsWith('.md') || new TextEncoder().encode(saved.content).byteLength>(1<<20))return saved;
  return {...saved,...await api(notebookPath(getProject().id)+'/api/live?path='+path)};
 };
 const version=note=>note.liveRevision??note.version;
 $('draft-download').onclick=()=>{
  downloadBlob(new Blob([getContent()],{type:'text/markdown'}),getNote().path.split('/').pop()+'.draft.md');
 };
 $('draft-compare').onclick=async()=>{
  if(busy)return;setBusy(true);
  review={path:getNote().path,project:getProject().id};
  try {
   await beforeReview?.();if(!matches())return;
   const saved=await latest();if(!matches())return;review.latest=saved;
   $('reconcile-latest').value=saved.content;
   let combined=null;try{combined=readScratch()}catch{}
   $('reconcile-content').value=combined??getContent();$('reconcile-error').textContent=combined!==null?'Recovered your combined draft. Review it against the latest saved version.':'';openDialog(dialog);
  }catch(error){notice(error.message)}finally{setBusy(false)}
 };
 $('reconcile-close').onclick=()=>{if(busy)return;preserveCombined();dialog.close()};
 dialog.addEventListener('cancel',event=>{if(busy)event.preventDefault()});
 dialog.addEventListener('close',()=>{review=null});
 async function finish(discard) {
  if(busy || !matches())return;
  if(discard && !await confirmAction('Discard this local draft and use the latest shared version? Changes already accepted by the server remain shared.'))return;
  if(!matches())return;
  setBusy(true);
  try {
   const saved=await latest();
   if(!matches())return;
   if(version(saved)!==version(review.latest)){
    $('reconcile-latest').value=saved.content;
    review.latest=saved;
    throw new Error('The shared note changed again. Review the updated version before continuing.');
   }
   const content=discard?saved.content:$('reconcile-content').value;
   if(await applyDraft(saved,content,discard)!==true)return;
   dialog.close();
  }catch(error){
   $('reconcile-error').textContent=error.message;
  }finally{
   setBusy(false);
  }
 }
 $('reconcile-apply').onclick=()=>finish(false);
 $('reconcile-discard').onclick=()=>finish(true);
 return {choose,cancelChoice,refreshCandidates};
}
