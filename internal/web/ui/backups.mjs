import {poll,moduleOn} from './state.mjs';
import {notebookPath} from './request.mjs';
import {$,busy,isBusy} from './dom.mjs';
import {initDriveBackups} from './drive-backups.mjs';
// A notebook's managed backups: its GitHub binding and, below it, the Google
// Drive and Dropbox destinations the server configures (drive-backups.mjs).
export function initBackups({api,post,getProject,getUser,remember}) {
 let state=null, version=0, timer;
 // One backup action runs at a time, and status polling waits for it.
 const panel=$('managed-backup');
 const drives=initDriveBackups({api,post,getProject,remember,panel,refresh:()=>reload(version)});
 const query=new URLSearchParams(location.search);
 let connecting=query.get('backup')==='connect'?query.get('project'):null;
 const permitted=()=>moduleOn(getUser(),'managed-backups')&&getProject()?.kind!=='repository'&&(getUser()?.admin||getProject()?.role==='owner');
 function reset(){version++;timer?.stop();state=null;$('managed-backup').hidden=true;drives.reset();}
 function render(next){
  state=next;
  // An instance with drive destinations and no GitHub App shows no GitHub row.
  $('managed-backup-github').hidden=!next.configured&&!!next.drives?.length;
  drives.render(next.drives||[],next);
  const connectable=next.connectable!==false;
  $('managed-backup-status').textContent=!next.configured?'Not configured on this server.':!next.connected?'Not connected.':next.repository.full_name+' · '+next.branch+' · '+(!next.enabled?'Paused':next.blocked?'Stopped':next.pending?'Saved changes pending':'Up to date');
  $('managed-backup-connect').hidden=!next.configured||!connectable;
  $('managed-backup-connect').textContent=next.connected?'Reconnect GitHub':'Connect GitHub backup';
  $('managed-backup-install').hidden=!next.configured;
  if(next.installUrl)$('managed-backup-install').href=next.installUrl;
  $('managed-backup-controls').hidden=!next.connected||!connectable;
  $('managed-backup-personal').hidden=connectable;
  $('managed-backup-now').disabled=!next.enabled||next.blocked;
  $('managed-backup-pause').textContent=!next.enabled||next.blocked?'Resume':'Pause';
  $('managed-backup-error').textContent=next.error||'';
  $('managed-backup-receipt').textContent=next.lastRevision?'Verified '+next.lastRevision.slice(0,12)+' · '+new Date(next.lastSuccess).toLocaleString():'No upload verified yet.';
 }
 async function reload(v){const id=getProject()?.id;const next=await api(notebookPath(id)+'/api/managed-backup');if(v===version&&getProject()?.id===id)render(next);}
 function watch(v,id){timer?.stop();timer=poll(async()=>{
  if(isBusy(panel))return;
  try{const next=await api(notebookPath(id)+'/api/managed-backup');if(v===version)render(next);}catch(error){if(v===version)$('managed-backup-error').textContent=error.message;}
 },{every:5000,active:()=>v===version&&getProject()?.id===id&&$('settings-dialog').open});}
 async function choices(v){
  const entries=await api(notebookPath(getProject().id)+'/api/managed-backup/choices');if(v!==version)return;
  $('managed-backup-choice').replaceChildren(...entries.map(e=>{const option=document.createElement('option');option.textContent=e.full_name;option.value=JSON.stringify({repository:e.id,installation:e.installation});return option;}));
  $('managed-backup-select').hidden=false;$('managed-backup-enable').disabled=!entries.length;
  $('managed-backup-create').hidden=!state.allowCreate;
  if(!entries.length)$('managed-backup-error').textContent='No private repositories with write access are available. Install the Backups App on an empty private repository, then reconnect.';
 }
 async function open(){reset();if(!permitted())return;const v=version,id=getProject()?.id;
  $('managed-backup').hidden=false;$('managed-backup-select').hidden=true;$('managed-backup-create').hidden=true;
  try{const next=await api(notebookPath(id)+'/api/managed-backup');if(v!==version||getProject()?.id!==id)return;render(next);if(connecting===id&&next.configured)await choices(v);await drives.resume(()=>v===version&&getProject()?.id===id);watch(v,id);}catch(error){if(v===version)$('managed-backup-status').textContent=error.message;}
 }
 function perform(button,fn){const v=version;return busy(panel,()=>fn(v),{controls:button,report:$('managed-backup-error'),current:()=>v===version});}
 $('managed-backup-connect').onclick=e=>perform(e.currentTarget,async()=>{remember();const out=await post(notebookPath(getProject().id)+'/api/managed-backup/connect',{});location.assign(out.url);});
 $('managed-backup-select').onsubmit=e=>{e.preventDefault();perform(e.submitter,async v=>{
  await post(notebookPath(getProject().id)+'/api/managed-backup/select',{...JSON.parse($('managed-backup-choice').value),branch:$('managed-backup-branch').value.trim(),generation:state.generation||0,shareHistory:$('managed-backup-share').checked});
  if(v!==version)return;connecting=null;const u=new URL(location.href);u.searchParams.delete('backup');history.replaceState(history.state,'',u);await open();
 });};
 for(const [id,action] of [['now','now'],['pause',null],['disconnect','disconnect']])$('managed-backup-'+id).onclick=e=>perform(e.currentTarget,async v=>{
  await post(notebookPath(getProject().id)+'/api/managed-backup/control',{action:action||(!state.enabled||state.blocked?'resume':'pause'),generation:state.generation});if(v===version)await open();
 });
 $('managed-backup-create').onsubmit=e=>{e.preventDefault();perform(e.submitter,async v=>{
  const out=await post(notebookPath(getProject().id)+'/api/managed-backup/create',{owner:$('managed-backup-owner').value.trim(),name:$('managed-backup-name').value.trim(),generation:state.generation||0,confirm:$('managed-backup-create-confirm').checked});
  if(v!==version)return;await choices(v);$('managed-backup-error').textContent='Created '+out.full_name+'. Select it above to enable backups. If absent, grant the Backups App access and reconnect.';
 });};
 $('settings-dialog').addEventListener('close',()=>{version++;timer?.stop();drives.reset();});
 return{open,projectChanged:reset};
}
