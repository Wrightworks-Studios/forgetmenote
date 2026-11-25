import {timeElement} from './time.mjs';
import {confirmAction,feedback} from './feedback.mjs';
import {$,element as node,button as control,downloadBlob,busy,isBusy} from './dom.mjs';
import {initIntegrations} from './integrations.mjs';
const roles={read:'Reader',comment:'Commenter',edit:'Editor',owner:'Owner'};
const label=user=>user.displayName||user.id;

function select(options,value=''){const e=node('select');for(const [id,text] of options)e.append(new Option(text,id));e.value=value;return e;}
function checks(container,users,selected=[]){container.replaceChildren();for(const user of users){const text=node('label');const input=node('input');input.type='checkbox';input.value=user.id;input.checked=selected.includes(user.id);text.append(input,document.createTextNode(' '+label(user)+(user.disabled?' (disabled)':'')));container.append(text);}if(!users.length)container.textContent='None';}
const checked=id=>[...$(id).querySelectorAll('input:checked')].map(input=>input.value);
export function initOrganization({post,getProject,getUser,refresh:reload,openNotebook}){
 let fieldID=0,secretAccount;
 let directory,org,status,groupRevision=0,selectedPerson='',selectedService='',personVersion=0,sharingVersion=0,activityCount=50,pageActive=()=>false;
 const call=(action,body={})=>post('/api/organization/action/'+action,body);
 const report=error=>feedback($('admin-status'),error.message);
 const integrations=initIntegrations({post,report});
 // A control saved by its action stays disabled until its form changes again (`unchanged`).
 // A control whose action is still running ignores the event and keeps its state.
 const act=(fn,getActive=()=>pageActive)=>async event=>{
  event?.preventDefault();const button=event?.submitter||event?.currentTarget;if(button&&isBusy(button))return;
  let active=getActive();if(!active())return;const account=getUser()?.id;
  const owns=()=>active();
  const refresh=async()=>{if(!owns())return false;const next=await reload();if(!next)return false;active=next;return owns();};
  await busy(button||{},()=>fn(refresh,owns,value=>secret(value,account)),{controls:button,report,current:owns});
  if(button)button.disabled=!!button.unchanged?.();
 };
 $('work-notebook-form').onsubmit=act(async(refresh,active)=>{
  if(!selectedPerson)throw Error('Select a person first.');
  await call('provision_work_notebook',{user:selectedPerson,id:$('work-notebook-id').value,name:$('work-notebook-name').value});
  if(!await refresh())return;feedback($('admin-status'),'Work notebook ready. No instance privileges were added.',{level:'success'});
 });
 function secret(value,account){if(getUser()?.id!==account||!getUser()?.admin)return;secretAccount=account;$('organization-secret-value').value=value;$('organization-secret').hidden=false;$('organization-secret-value').focus();$('organization-secret-value').select();}
 function clearSecret(){secretAccount=undefined;$('organization-secret-value').value='';$('organization-secret').hidden=true;}
 $('organization-secret-close').onclick=clearSecret;
 $('organization-secret-copy').onclick=act(async(refresh,active)=>{await navigator.clipboard.writeText($('organization-secret-value').value);feedback($('admin-status'),'Copied.',{level:'success'});});
 function closed(){clearSecret();personVersion++;selectedPerson='';}
 function checkAuthority(){if(secretAccount&&(secretAccount!==getUser()?.id||!getUser()?.admin))clearSecret();}
 function group(id=''){
  const g=org.groups.find(g=>g.id===id);groupRevision=g?.revision||0;
  $('group-id').value=g?.id||'';$('group-id').readOnly=!!g;$('group-name').value=g?.name||'';$('group-delete').hidden=!g;
  checks($('group-members'),directory.users,g?.members||[]);
 }
 $('group-select').onchange=()=>group($('group-select').value);
 $('group-form').onsubmit=act(async(refresh,active)=>{const g=await call('set_group',{id:$('group-id').value,name:$('group-name').value,members:checked('group-members'),revision:groupRevision});if(!await refresh())return;$('group-select').value=g.id;group(g.id);feedback($('admin-status'),'Group saved.',{level:'success'});});
 $('group-delete').onclick=act(async(refresh,active)=>{const id=$('group-id').value,revision=groupRevision;if(!await confirmAction('Delete this group and remove its notebook access?'))return;await call('delete_group',{id,revision});if(!await refresh())return;});
 $('invitation-create-form').onsubmit=act(async(refresh,active,receipt)=>{
  const projects=Object.fromEntries([...$('invitation-projects').selectedOptions].map(o=>[o.value,$('invitation-role').value]));
  const result=await call('create_invitation',{name:$('invitation-name').value,groups:checked('invitation-groups'),projects});
  try{if(active()){$('invitation-create-form').reset();await refresh();}}finally{receipt(result.url);}
 });
 $('service-create-form').onsubmit=act(async(refresh,active)=>{const service=await call('create_service_account',{name:$('service-name').value,steward:$('service-steward').value});if(!active())return;selectedService=service.id;$('service-create-form').reset();if(!await refresh())return;});
 let pendingLogo,logoGeneration=0;
 $('instance-logo').onchange=act(async(refresh,active)=>{
  const file=$('instance-logo').files[0],generation=++logoGeneration;
  pendingLogo=undefined;$('instance-logo-preview').src=status.branding.icon;
  if(!file)return;
  if(file.size>1048576){$('instance-logo').value='';throw Error('Choose a logo up to 1 MiB.');}
  const raw=await new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(reader.result);reader.onerror=()=>reject(Error('Could not read the logo.'));reader.readAsDataURL(file);});
  if(generation===logoGeneration){pendingLogo=raw.slice(raw.indexOf(',')+1);$('instance-logo-preview').src=raw;}
 });
 $('instance-logo-reset').onclick=()=>{logoGeneration++;pendingLogo='';$('instance-logo').value='';$('instance-logo-preview').src='/static/brand/flower.svg';};
 $('instance-form').onsubmit=act(async(refresh,active)=>{
  if($('instance-logo').disabled)throw Error('Wait for the logo to finish loading.');
  await call('set_instance_branding',{name:$('instance-name').value,...(pendingLogo===undefined?{}:{logo:pendingLogo})});
  if(!await refresh())return;
  const brand=status.branding,previous=document.documentElement.dataset.instanceName;
  document.documentElement.dataset.instanceName=brand.name;
  document.querySelectorAll('[data-instance-name]:not(html)').forEach(e=>{e.textContent=brand.name;});
  document.querySelector('.brand img').src=brand.icon;
  if(document.title.endsWith(previous))document.title=document.title.slice(0,-previous.length)+brand.name;
  for(const link of document.querySelectorAll('link[rel=icon],link[rel=apple-touch-icon]')){link.href=brand.icon;link.removeAttribute('type');}
  feedback($('admin-status'),'Branding saved.',{level:'success'});
 });
 $('activity-export').onclick=()=>downloadBlob(new Blob([JSON.stringify(org.activity,null,2)],{type:'application/json'}),'forgetmenote-activity.json');
 function activity(){const items=org.activity.slice(0,activityCount).map(event=>{const row=node('div',undefined,'activity-row');const person=directory.users.find(user=>user.id===event.actor),name=person?.displayName || (event.actor==='@local-admin'?'Recovery administrator':'Former contributor');row.append(timeElement(event.time),node('strong',event.action),node('span',name+' · '+(directory.users.find(user=>user.id===event.target)?.displayName || directory.projects.find(project=>project.id===event.target)?.name || event.target)+' · '+event.outcome,'small'));return row;});$('activity-list').replaceChildren(...items);if(!items.length)$('activity-list').textContent='No activity.';$('activity-more').hidden=activityCount>=org.activity.length;}
 $('activity-more').onclick=()=>{activityCount+=50;activity();};
 const button=(text,fn,getActive)=>control(text,act(fn,getActive),{class:'quiet'});
 // Saves a role or steward choice; available only while the choice differs from the saved one.
 function changes(save,select){let saved=select.value;save.unchanged=()=>select.value===saved;save.disabled=true;select.addEventListener('change',()=>{save.disabled=save.unchanged();});return value=>{saved=value;};}
 function saveRole(select,fn,getActive){const save=control('Save',undefined,{class:'primary'});const saved=changes(save,select);save.onclick=act(async(...args)=>{const value=select.value;await fn(...args);saved(value);},getActive);return save;}
 function field(form,text,input){const label=node('label',text);input.id='organization-field-'+(++fieldID);label.htmlFor=input.id;form.append(label,input);return input;}
 async function load(data,active){
  const nextStatus=await call('organization_status');if(!active())return;
  pageActive=active;
  directory={...data,users:data.users.map(user=>({...user,displayName:user.displayName||data.usernames?.[user.id]||user.id}))};status=nextStatus;org=status.organization;
  const selected=$('group-select').value;$('group-select').replaceChildren(new Option('New group',''),...org.groups.map(g=>new Option(g.name,g.id)));$('group-select').value=org.groups.some(g=>g.id===selected)?selected:'';group($('group-select').value);
  checks($('invitation-groups'),org.groups.map(g=>({id:g.id,displayName:g.name})));
  $('invitation-projects').replaceChildren(...directory.projects.filter(p=>!p.protectedInbox&&!p.assistantOwner).map(p=>new Option(p.name,p.id)));
  $('invitation-list').replaceChildren();
  for(const invite of org.invitations.filter(i=>i.status==='pending'&&Date.parse(i.expires)>Date.now())){const row=node('div',undefined,'person-row');row.append(node('span',invite.name+(invite.reset?' · password reset':'')+' · expires '+new Date(invite.expires).toLocaleDateString()),button('Revoke',async(refresh,active)=>{await call('revoke_invitation',{id:invite.id});if(!await refresh())return;}));$('invitation-list').append(row);}
  const humans=directory.users.filter(u=>u.kind!=='service'&&!u.disabled);$('service-steward').replaceChildren(...humans.map(u=>new Option(label(u),u.id)));
  $('service-list').replaceChildren(...directory.users.filter(u=>u.kind==='service').map(u=>button(label(u)+(u.disabled?' · disabled':''),async(refresh,active)=>{selectedService=u.id;await service(u.id);})));
  if(selectedService)await service(selectedService,active);if(!active())return;
  $('instance-name').value=org.name||'ForgetMeNote';$('instance-status').replaceChildren();
  logoGeneration++;pendingLogo=undefined;$('instance-logo').value='';$('instance-logo-preview').src=status.branding.icon;
  $('instance-status').append(node('dt','Authentication'),node('dd',status.authentication));
  const services=[['chat','AI conversations'],['decisions','AI checks'],['embeddings','Semantic search'],['dataConnections','Data connections'],['personalConnections','Personal connections'],['slack','Slack'],['repositorySources','Repository sources'],['managedBackups','Managed backups'],['automations','Automation definitions'],['automationPython','Automation runtime'],['trustedProxy','Trusted proxy'],['oidc','Identity provider'],['githubIdentity','GitHub account linking']];
  for(const [key,name] of services)$('instance-status').append(node('dt',name),node('dd',status.instance[key]?'Configured':'Not configured'));
  $('instance-status').append(node('dt','Browser interface'),node('dd',status.instance.headless?'Disabled':'Enabled'));
  $('instance-status').append(node('dt','Outgoing actions'),node('dd',status.instance.outgoingPaused?'Paused':'Enabled'));
  activity();
  await integrations.load(directory,org.groups,active);
 }
 async function person(id){
  $('work-notebook-form').reset();$('work-notebook-provision').open=false;
  const selected=directory.users.find(user=>user.id===id);
  $('work-notebook-provision').hidden=!selected||selected.disabled||selected.kind==='service'||id==='@local-admin';
  selectedPerson=id;const version=++personVersion;$('user-offboarding').replaceChildren();
  const data=await call('offboarding_status',{user:id});if(version!==personVersion||selectedPerson!==id)return;
  const box=$('user-offboarding'),account=data.account;
  if(account.groups.length)box.append(node('p','Groups: '+account.groups.map(id=>org?.groups.find(g=>g.id===id)?.name||id).join(', '),'small'));
  if(account.services.length)box.append(node('p','Automation to reassign: '+account.services.map(label).join(', '),'small'));
  if(account.owned.length){
   const form=node('form');form.append(node('h4','Reassign shared notebooks'));
   const choices=node('div',undefined,'organization-choices');checks(choices,account.owned.map(id=>({id,displayName:directory.projects.find(p=>p.id===id)?.name||id})),account.owned);form.append(choices);
   const replacement=field(form,'New owner',select(directory.users.filter(u=>u.id!==id&&u.kind!=='service'&&!u.disabled).map(u=>[u.id,label(u)])));replacement.required=true;
   const submit=node('button','Transfer selected notebooks');submit.type='submit';form.append(submit);
   form.onsubmit=act(async(refresh,active)=>{await call('reassign_notebook_ownership',{user:id,replacement:replacement.value,projects:[...choices.querySelectorAll('input:checked')].map(i=>i.value)});if(!await refresh())return;});box.append(form);
  }
  credentialList(box,data.credentials,id,async()=>person(id));
  if(data.connections.length){box.append(node('h4','Integration authorizations'));for(const connection of data.connections){const p=directory.projects.find(p=>p.id===connection.project);box.append(button((p?.name||connection.project)+' · '+connection.kind+(connection.reauthorize?' · reconnect required':''),()=>openNotebook(connection.project)));}}
 }
 function credentialList(box,credentials,user,reload){
  if(!credentials.length)return;box.append(node('h4','Credentials'));
  for(const credential of credentials){const row=node('div',undefined,'person-row');row.append(node('span',credential.name+' · '+credential.scopes.join(', ')+' · expires '+new Date(credential.expires*1000).toLocaleDateString()),button('Revoke',async(refresh,active)=>{await call('revoke_user_credential',{user,id:credential.id});if(active())await reload();}));box.append(row);}
 }
 async function service(id,active=pageActive){
  const data=await call('offboarding_status',{user:id});if(id!==selectedService||!active())return;
  const u=data.account.user,box=$('service-details');box.replaceChildren(node('h4',label(u)),node('p',u.id,'small'));
  const form=node('form');const steward=field(form,'Steward',select(directory.users.filter(u=>u.kind!=='service'&&!u.disabled).map(u=>[u.id,label(u)]),u.steward));steward.required=true;
  const submit=node('button','Save steward','primary');form.append(submit);const stewardSaved=changes(submit,steward);form.onsubmit=act(async(refresh,active)=>{const value=steward.value;await call('set_service_steward',{user:id,steward:value});stewardSaved(value);if(!await refresh())return;});box.append(form,button(u.disabled?'Enable account':'Disable account',async(refresh,active)=>{await post('/api/users',{id,disabled:!u.disabled});if(!await refresh())return;}));
  const grants=node('div');grants.append(node('h4','Notebook access'));
  for(const p of directory.projects.filter(p=>!p.protectedInbox&&!p.assistantOwner)){const row=node('div',undefined,'person-row');const role=select([['','No direct access'],['read','Reader'],['comment','Commenter'],['edit','Editor']],p.members?.[id]||'');role.setAttribute('aria-label','Role for '+p.name);row.append(node('span',p.name),role,saveRole(role,async(refresh,active)=>{await post('/api/members',{project:p.id,user:id,role:role.value});if(!await refresh())return;}));grants.append(row);}box.append(grants);
  if(!u.disabled){
   const form=node('form');form.append(node('h4','New credential'));
   const name=field(form,'Name',node('input'));name.required=true;name.maxLength=100;
   const projects=field(form,'Notebooks',select(directory.projects.filter(p=>data.account.access[p.id]?.role&&!p.protectedInbox).map(p=>[p.id,p.name])));projects.multiple=true;projects.required=true;projects.size=3;
   const scope=field(form,'Permission',select([['notes:read','Read'],['notes:write','Read and write']],'notes:read'));
   const available=await post('/integrations/actions/list_data_connections',{});if(id!==selectedService||!active())return;
   let dataScope,connections;
   if(available.length){
    dataScope=field(form,'Data access',select([['','None'],['data:read','Read'],['data:write','Read and write']]));
    connections=field(form,'Connections',select(available.map(c=>[c.id,c.label])));connections.multiple=true;connections.size=Math.min(4,available.length);connections.disabled=true;
    dataScope.onchange=()=>{connections.disabled=!dataScope.value;connections.required=!!dataScope.value;};
   }
   const days=field(form,'Expires in days',node('input'));days.type='number';days.min=1;days.max=365;days.value=30;days.required=true;
   const submit=node('button','Create credential');form.append(submit);form.onsubmit=act(async(refresh,active,receipt)=>{const scopes=scope.value==='notes:read'?['notes:read']:['notes:read','notes:write'];if(dataScope?.value)scopes.push('data:read',...(dataScope.value==='data:write'?['data:write']:[]));const result=await call('issue_service_credential',{user:id,name:name.value,projects:[...projects.selectedOptions].map(o=>o.value),connections:dataScope?.value?[...connections.selectedOptions].map(o=>o.value):[],scopes,audiences:['api','mcp'],days:Number(days.value)});try{if(active())await service(id);}finally{receipt(result.token);}});box.append(form);
  }
  credentialList(box,data.credentials,id,()=>service(id));
 }
 function clearSharing(){sharingVersion++;$('notebook-group-access')?.remove();}
 async function sharing(project,people,active){
  const version=++sharingVersion;const groups=await call('project_groups',{project:project.id});if(version!==sharingVersion||getProject()?.id!==project.id||!active())return;
  const box=node('div');box.id='notebook-group-access';box.append(node('h4','Group access'));
  for(const g of groups){const row=node('div',undefined,'person-row');const role=select([['','No access'],['read','Reader'],['comment','Commenter'],['edit','Editor']],project.groupRoles?.[g.id]||'');role.setAttribute('aria-label','Role for group '+g.name);row.append(node('span',g.name),role,saveRole(role,async(refresh,active)=>{await call('set_project_group',{project:project.id,group:g.id,role:role.value});if(active())await openNotebook(project.id);},()=>active));box.append(row);}
  if(!groups.length)box.append(node('p','No groups.','small'));
  for(const [id,access] of Object.entries(project.effectiveMembers||{})){if(!access.groups?.length)continue;box.append(node('p',(people.users.find(u=>u.id===id)?.displayName||people.usernames?.[id]||id)+' · '+(roles[access.role]||'Disabled')+' through '+access.groups.map(id=>groups.find(g=>g.id===id)?.name||id).join(', '),'small'));}
  $('member-list').after(box);
 }
 return {load,person,sharing,clearSharing,closed,checkAuthority,reset:async user=>{const account=getUser()?.id,result=await call('create_password_reset',{user});secret(result.url,account);}};
}
