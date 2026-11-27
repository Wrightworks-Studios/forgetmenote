import {notebookPath} from './request.mjs';
import {$,busy,isBusy} from './dom.mjs';
import {moduleOn} from './state.mjs';

export function initAuthority({api,post,getProject,getUser,remember}) {
 let version=0,state=null,preview=null;
 const label=type=>type==='github'?'GitHub':'ForgetMeNote';
 function render(){
  const owner=getUser()?.admin||getProject()?.role==='owner';
  $('authority-current').textContent=label(state.authority.type)+(state.changing?' · Handoff in progress':state.authority.type==='github'?' · Read only':' · Editable');
  $('authority-revision').textContent=state.source?.connected&&state.authority.type==='github'?`${state.source.repository.full_name} · ${state.source.repository.default_branch} · ${(state.source.downloaded||'').slice(0,12)}`:(state.revision||'').slice(0,12);
  // A handoff moves a notebook to or from its repository source.
  $('authority-form').hidden=!owner||!!getProject()?.protectedInbox||state.changing||!moduleOn(getUser(),'repositories');
  $('authority-to').textContent=label(state.authority.type==='github'?'forgetmenote':'github');
  $('authority-writers-label').hidden=state.authority.type==='github';
  $('authority-writers').required=state.authority.type!=='github';
  $('authority-preview-panel').hidden=!preview;
  $('authority-preview-detail').replaceChildren();
  if(preview){
   for(const [name,value] of [['Authority',`${label(preview.from)} → ${label(preview.to)}`],['Repository',`${preview.repository} · ${preview.sourceBranch}`],['Pinned commit',preview.sourceRevision],['Files',`${preview.files} · ${preview.notes} notes · ${preview.attachments} attachments`],['History',preview.to==='github'?'Keep the original notebook history; stop outgoing backups.':'Import this snapshot; keep notebook history and pinned source references.']]){
    const dt=document.createElement('dt'),dd=document.createElement('dd');dt.textContent=name;dd.textContent=value;$('authority-preview-detail').append(dt,dd);
   }
  }
  $('authority-apply').hidden=!preview||['complete','failed'].includes(preview.phase);
  $('authority-apply').textContent=preview?.phase==='preparing'?'Continue handoff':preview?.phase==='publishing'?'Complete handoff':'Apply change';
  $('authority-cancel').hidden=!preview||['publishing','complete','failed'].includes(preview.phase);
  $('authority-status').textContent=preview?.error||(preview?.phase==='preparing'?'Waiting for backup worker acknowledgement.':preview?.phase==='publishing'?'Publication started. Complete the handoff to restore access.':'');
 }
 async function open(){
  const v=++version,project=getProject()?.id;preview=null;$('authority-error').textContent='';$('authority-preview-panel').hidden=true;$('authority-current').textContent='Loading…';$('authority-writers').checked=false;
  try{const next=await api('/api/authority/status?'+new URLSearchParams({project}));if(v!==version||project!==getProject()?.id)return;state=next;preview=next.transfer&&!['complete','failed'].includes(next.transfer.phase)?next.transfer:null;render();return next;}
  catch(error){if(v===version)$('authority-error').textContent=error.message;}
 }
 // One handoff step runs at a time.
 function perform(fn){
  const form=$('authority-form');if(isBusy(form))return;
  const v=version;$('authority-error').textContent='';
  return busy(form,()=>fn(v),{controls:['authority-preview','authority-apply','authority-cancel'].map(id=>$(id)),report:$('authority-error'),current:()=>v===version});
 }
 $('authority-form').onsubmit=event=>{event.preventDefault();perform(async v=>{
  const result=await post(notebookPath(getProject().id)+'/api/authority/preview',{to:state.authority.type==='github'?'forgetmenote':'github',operatorWritersStopped:$('authority-writers').checked});if(v!==version)return;preview=result;render();
 });};
 $('authority-apply').onclick=()=>perform(async v=>{
  remember();const result=await post('/api/authority/apply',{project:getProject().id,preview:preview.id});if(v!==version)return;
  if(result.phase==='complete'){location.reload();return;}preview=result;state.changing=true;render();
 });
 $('authority-cancel').onclick=()=>perform(async()=>{await post('/api/authority/cancel',{project:getProject().id,preview:preview.id});await open();});
 $('settings-dialog').addEventListener('close',()=>{version++;});
 return {open};
}
