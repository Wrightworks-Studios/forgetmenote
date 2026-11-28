import {notebookPath} from './request.mjs';
import {$,busy,button,element} from './dom.mjs';
import {feedback,confirmAction} from './feedback.mjs';

// A notebook's Google Drive and Dropbox backup destinations, one row per kind
// the server configures. Connecting goes to the provider and comes back with
// ?backup=<kind>; the setup screen then names the account and folder and asks
// for consent and the encryption choice. A new encrypted destination's
// password is shown here once and kept only in this page until dismissed.
const checkbox=(text,attributes={})=>{const input=element('input',undefined,{type:'checkbox',...attributes}),label=element('label');label.append(input,' '+text);return [label,input];};
export function initDriveBackups({api,post,getProject,remember,panel,refresh}) {
 const list=$('drive-backups'),rows=new Map();
 const query=new URLSearchParams(location.search);
 let setup=['gdrive','dropbox'].includes(query.get('backup'))?{project:query.get('project'),kind:query.get('backup')}:null;
 let state=[],connectable=false;
 const path=suffix=>notebookPath(getProject().id)+'/api/managed-backup'+suffix;
 function endSetup(){setup=null;const u=new URL(location.href);if(['gdrive','dropbox'].includes(u.searchParams.get('backup'))){u.searchParams.delete('backup');history.replaceState(history.state,'',u);}}
 function build(kind,name){
  const status=element('p','',{role:'status'}),receipt=element('p','',{class:'small'});
  const connect=button('Connect '+name,null,{class:'quiet'});
  const offer=element('p'),kept=element('p','',{class:'small'});
  const [encryptLabel,encrypt]=checkbox('Encrypt snapshots. ForgetMeNote makes a password and shows it once; without it nobody, including you, can read the snapshots.');
  const [consentLabel,consent]=checkbox('Anyone with access to this folder can read every snapshot of this notebook’s saved history, including earlier revisions, unless the snapshots are encrypted.',{required:''});
  const enable=element('button','Enable backup',{type:'submit'}),cancel=button('Cancel',null,{class:'quiet'});
  const form=element('form','',{hidden:'',class:'drive-backup-setup'});form.append(offer,kept,encryptLabel,consentLabel,enable,cancel);
  const secret=element('input','',{readonly:'','aria-label':name+' snapshot password',spellcheck:'false',autocomplete:'off',class:'drive-backup-secret'});
  const copy=button('Copy',null,{class:'quiet'}),saved=button('I saved the password');
  const reveal=element('div','',{hidden:'',class:'drive-backup-password',role:'group','aria-label':'Snapshot password'});
  reveal.append(element('p','Save this password now in a password manager. It is shown only this once, and without it these snapshots cannot be read. Restoring one needs it.'),secret,copy,saved);
  const now=button('Back up now',null,{class:'quiet'}),pause=button('Pause',null,{class:'quiet'}),disconnect=button('Disconnect',null,{class:'quiet'});
  const controls=element('div','',{hidden:'',class:'drive-backup-controls'});controls.append(now,pause,disconnect);
  const error=element('p','',{role:'alert'});
  const root=element('div','',{class:'drive-backup',id:'drive-backup-'+kind,'data-drive-kind':kind});
  root.append(element('h3',name+' backup'),status,receipt,connect,form,reveal,controls,error);
  const parts={root,status,receipt,connect,form,offer,kept,encryptLabel,encrypt,consent,cancel,reveal,secret,copy,saved,controls,now,pause,disconnect,error,name,kind};
  const perform=(control,fn)=>busy(panel,fn,{controls:control,report:error});
  connect.onclick=()=>perform(connect,async()=>{remember();const out=await post(path('/drive/connect'),{kind});location.assign(out.url);});
  cancel.onclick=()=>perform(cancel,async()=>{await post(path('/drive/cancel'),{kind});endSetup();form.hidden=true;await refresh();});
  form.onsubmit=event=>{event.preventDefault();perform(enable,async()=>{
   const out=await post(path('/drive/enable'),{kind,generation:Number(form.dataset.generation||0),encrypt:!encryptLabel.hidden&&encrypt.checked,consent:consent.checked});
   endSetup();form.hidden=true;
   if(out.password){secret.value=out.password;reveal.hidden=false;secret.focus();secret.select();}
   await refresh();
  });};
  copy.onclick=async()=>{try{await navigator.clipboard.writeText(secret.value);feedback(error,'Password copied.',{level:'success'});}catch{secret.select();feedback(error,'Select the password and copy it.');}};
  saved.onclick=()=>{secret.value='';reveal.hidden=true;};
  const control=(node,action,confirm)=>{node.onclick=async()=>{
   if(confirm&&!await confirmAction(confirm))return;
   return perform(node,async()=>{const d=state.find(entry=>entry.kind===kind);await post(path('/control'),{kind,generation:d.generation,action:action()});await refresh();});
  };};
  control(now,()=>'now');
  control(pause,()=>{const d=state.find(entry=>entry.kind===kind);return !d.enabled||d.blocked?'resume':'pause';});
  control(disconnect,()=>'disconnect',`Disconnect ${name}? ForgetMeNote stops these backups and removes its access to the account. Snapshots already uploaded stay in the folder.`);
  rows.set(kind,parts);list.append(root);return parts;
 }
 function describe(d){
  if(!d.connected)return 'Not connected.';
  const condition=d.handoff?'Stopped for a notebook authority change':!d.enabled?'Paused':d.blocked?'Stopped':d.pending?'Saved changes pending':'Up to date';
  return [d.account,d.path,d.encrypted?'Encrypted':'Not encrypted',condition].join(' · ');
 }
 function render(drives,next) {
  state=drives;connectable=next.connectable!==false;
  for(const d of drives){
   const row=rows.get(d.kind)||build(d.kind,d.name);
   row.status.textContent=describe(d);
   row.receipt.textContent=!d.connected?'':d.lastSnapshot?`Verified ${d.lastRevision.slice(0,12)} · ${new Date(d.lastSuccess).toLocaleString()} · ${d.kept} ${d.kept===1?'snapshot':'snapshots'} kept`:'No snapshot verified yet.';
   row.connect.hidden=!connectable||!row.form.hidden;
   row.connect.textContent=(d.connected?'Reconnect ':'Connect ')+d.name;
   row.controls.hidden=!connectable||!d.connected;
   row.now.disabled=!d.enabled||d.blocked||!!d.handoff;
   row.pause.textContent=!d.enabled||d.blocked?'Resume':'Pause';
   row.pause.disabled=!!d.handoff;
   // A destination's own error replaces the row's message only when it changes,
   // so a refused action stays reported until the next change.
   if((d.error||'')!==row.shown){row.shown=d.error||'';feedback(row.error,row.shown);}
  }
  for(const [kind,row] of rows)if(!drives.some(d=>d.kind===kind)){row.root.remove();rows.delete(kind);}
  list.hidden=!drives.length;
 }
 // The provider returned to this notebook: show what the Owner consents to.
 async function resume(current){
  if(!setup||setup.project!==getProject()?.id||!connectable)return;
  const row=rows.get(setup.kind);if(!row)return;
  try{
   const offer=await api(path('/drive/pending?kind='+encodeURIComponent(setup.kind)));if(!current())return;
   row.offer.textContent=`Snapshots of this notebook’s saved history will go to the folder ${offer.path} in the ${offer.name} account ${offer.account}.`;
   row.kept.textContent=offer.continuing?`This account already holds this notebook’s backups; reconnecting keeps the folder and ${offer.encrypted?'its encryption password':'its unencrypted snapshots'}.`:'Uploaded snapshots stay in the folder if you disconnect later.';
   row.encryptLabel.hidden=offer.continuing;row.encrypt.checked=false;row.consent.checked=false;
   row.form.dataset.generation=String(offer.generation||0);
   row.form.hidden=false;row.connect.hidden=true;
  }catch(error){if(current()){row.error.textContent=error.message;endSetup();}}
 }
 function reset(){for(const row of rows.values()){row.form.hidden=true;row.secret.value='';row.reveal.hidden=true;row.shown='';feedback(row.error,'');}}
 return {render,resume,reset};
}
