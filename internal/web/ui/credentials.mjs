import {$,element as node,button as control,busy} from './dom.mjs';
import {confirmAction,feedback} from './feedback.mjs';
import {openDialog} from './overlay.mjs';
import {moduleOn} from './state.mjs';

const permissions={read:['notes:read'],write:['notes:read','notes:write'],manage:['notes:read','notes:write','notes:manage']};
const permissionLabel=scopes=>scopes.includes('notes:manage')?'Read, write and manage settings':scopes.includes('notes:write')?'Read and write':'Read';
const audienceLabel=audiences=>audiences.length>1?'API and MCP':audiences[0]==='api'?'API only':'MCP only';

// Your own API credentials, on your settings page: created, listed and revoked
// with create_api_credential, list_api_credentials and revoke_api_credential,
// the actions API and MCP clients use; the server checks every limit. The
// secret is answered once. It lives only in the one-time dialog's field, which
// is removed when the dialog closes, and is never stored or logged.
export function initCredentials({post,getUser,getProjects,getActive}){
 const call=(name,input={})=>post('/integrations/actions/'+name,input);
 const status=$('credential-status'),form=$('credential-form'),secret=$('credential-secret-dialog');
 let version=0,available=[],shownFor;
 function sync(){
  $('credential-projects').disabled=$('credential-all').checked;
  $('credential-connections').disabled=!$('credential-data-scope').value;
  $('credential-connections').required=!!$('credential-data-scope').value;
 }
 $('credential-all').onchange=sync;$('credential-data-scope').onchange=sync;
 function choices(){
  const selected=new Set([...$('credential-projects').selectedOptions].map(option=>option.value));
  $('credential-projects').replaceChildren(...getProjects().map(project=>{const option=new Option(project.name,project.id);option.selected=selected.has(project.id);return option;}));
  $('credential-data').hidden=!available.length;
  $('credential-connections').replaceChildren(...available.map(connection=>new Option(connection.label||connection.id,connection.id)));
  $('credential-connections').size=Math.min(4,Math.max(available.length,1));
  sync();
 }
 function summary(credential){
  const names=new Map(getProjects().map(project=>[project.id,project.name]));
  const scopes=credential.scopes||[];
  const parts=[credential.name,permissionLabel(scopes),credential.allProjects?'All notebooks':(credential.projects||[]).map(id=>names.get(id)||id).join(', ')];
  if(scopes.includes('data:read'))parts.push((scopes.includes('data:write')?'data read and write':'data read')+' · '+(credential.connections||[]).length+' connections');
  parts.push(audienceLabel(credential.audiences||[]));
  if(credential.familyDigest)parts.push('ends with its AI connection');
  parts.push('expires '+new Date(credential.expires*1000).toLocaleDateString());
  return parts.join(' · ');
 }
 function render(credentials,live){
  const rows=credentials.map(credential=>{
   const row=node('div',undefined,'person-row');
   const revoke=control('Revoke',()=>busy(revoke,async()=>{
    if(!await confirmAction(`Revoke “${credential.name}”? Clients that use it stop working at once.`)||!live())return;
    await call('revoke_api_credential',{id:credential.id});
    if(!live())return;
    feedback(status,'Credential revoked.',{level:'success'});
    await load(live);
   },{report:status,current:live}),{class:'quiet'});
   row.append(node('span',summary(credential)),revoke);
   return row;
  });
  $('credential-list').replaceChildren(...rows);
  if(!rows.length)$('credential-list').textContent='No API credentials.';
 }
 async function load(live){
  const credentials=await call('list_api_credentials');
  if(live())render(credentials,live);
 }
 async function open(){
  const active=getActive(),current=++version,account=getUser()?.id;
  const live=()=>current===version&&getUser()?.id===account&&active();
  status.textContent='';$('credential-list').textContent='Loading credentials…';
  try{
   const [credentials,connections]=await Promise.all([call('list_api_credentials'),moduleOn(getUser(),'data-connections')?call('list_data_connections').catch(()=>[]):[]]);
   if(!live())return;
   available=connections;choices();render(credentials,live);
  }catch(error){if(live()){$('credential-list').textContent='';feedback(status,error.message);}}
 }
 function show(token){
  const field=node('input',undefined,{id:'credential-secret-value',readonly:'',autocomplete:'off',spellcheck:'false'});
  field.value=token;
  $('credential-secret-field').replaceChildren(node('label','Secret',{for:field.id}),field);
  $('credential-secret-status').textContent='';shownFor=getUser()?.id;
  openDialog(secret,{fallback:$('credential-name')});
  field.focus();field.select();
 }
 secret.addEventListener('close',()=>{shownFor=undefined;$('credential-secret-field').replaceChildren();$('credential-secret-status').textContent='';});
 $('credential-secret-done').onclick=()=>secret.close();
 $('credential-secret-copy').onclick=async()=>{
  const field=$('credential-secret-value');if(!field)return;
  try{await navigator.clipboard.writeText(field.value);feedback($('credential-secret-status'),'Copied.',{level:'success'});}
  catch{feedback($('credential-secret-status'),'Copying failed. Select the secret and copy it.');}
 };
 form.onsubmit=event=>{
  event.preventDefault();
  const active=getActive(),current=version,account=getUser()?.id;
  const live=()=>current===version&&getUser()?.id===account&&active();
  status.textContent='';
  return busy(form,async()=>{
   const data=$('credential-data-scope').value;
   const scopes=[...permissions[$('credential-permission').value]];
   if(data)scopes.push('data:read',...(data==='write'?['data:write']:[]));
   const all=$('credential-all').checked;
   const issued=await call('create_api_credential',{
    name:$('credential-name').value,
    projects:all?[]:[...$('credential-projects').selectedOptions].map(option=>option.value),allProjects:all,
    ...(data?{connections:[...$('credential-connections').selectedOptions].map(option=>option.value)}:{}),
    scopes,audiences:$('credential-audience').value.split(' '),days:Number($('credential-days').value)});
   // The receipt is shown first: a failed refresh must not lose the secret.
   if(getUser()?.id===account)show(issued.token);
   if(!live())return;
   form.reset();sync();
   await load(live);
  },{controls:event.submitter,report:status,current:live});
 };
 // A secret is shown only to the account it was issued to.
 return {open,checkAuthority(){if(secret.open&&getUser()?.id!==shownFor)secret.close();}};
}
