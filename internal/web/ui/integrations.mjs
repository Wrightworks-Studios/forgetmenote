import {element as node,actionButton,busy} from './dom.mjs';
import {initAutomations} from './automations.mjs';

// dataConnections and automationsOn report whether the instance runs those
// modules; with one off its part is neither listed nor requested.
export function initIntegrations({post,report,dataConnections=()=>true,automationsOn=()=>true}){
 let directory,groups,connectionList=[],version=0,pageActive=()=>false;
 const call=(name,input={})=>post('/integrations/actions/'+name,input);
 const manage=(name,input={})=>post('/api/organization/action/'+name,input);
 const button=(label,fn)=>actionButton(label,()=>fn(),{report});
 async function connection(info,box){
  const active=pageActive;
  const previous=box.querySelector('select')?.value;
  const current=++version;
  const [discovery,grant]=await Promise.all([
   call('list_data_operations',{connection:info.id}).then(operations=>({operations}),()=>({unavailable:true})),
   manage('data_connection_access',{connection:info.id})
  ]);if(current!==version||!active())return;
  const operations=discovery.operations||[...new Set([...Object.values(grant.users||{}),...Object.values(grant.groups||{})].flat())].sort().map(name=>({name}));
  box.replaceChildren(node('h4',info.label));
  if(['email','sheets','calendar'].includes(info.kind)&&operations.some(op=>op.name==='status')){const status=node('pre');status.setAttribute('aria-live','polite');box.append(button('Test connection',async()=>{const result=await call('call_data_operation',{connection:info.id,operation:'status',arguments:{}});status.textContent=JSON.stringify({...result.data,permittedOperations:operations.map(op=>op.name)},null,2);}),status);}
  if(['email','sheets'].includes(info.kind)){const outcomes=node('div');outcomes.setAttribute('aria-label','Write outcomes');box.append(button('Inspect write outcomes',async()=>{
   const effects=await manage('data_connection_effects',{connection:info.id});if(current!==version||!active())return;outcomes.replaceChildren(node('pre',JSON.stringify(effects,null,2)));
   for(const effect of effects.filter(e=>['started','unknown'].includes(e.state))){const evidence=node('textarea');evidence.setAttribute('aria-label','Reconciliation evidence for '+effect.id);evidence.placeholder='Verified provider outcome and evidence';const actions=node('div');actions.className='dialog-actions';
    for(const state of ['accepted','rejected'])actions.append(button('Record '+state,async()=>{if(evidence.value.trim().length<10)throw Error('Describe the verified provider outcome. A negative search is inconclusive.');await manage('resolve_automation_effect',{id:effect.id,state,evidence:evidence.value});await connection(info,box);}));
    outcomes.append(node('p',effect.id),evidence,actions);
   }
  }),outcomes);}
  if(discovery.unavailable)box.append(node('p','Operation discovery unavailable. You can remove existing access.'));
  const form=node('form'),people=node('select');people.setAttribute('aria-label','Connection account or group');
  for(const u of directory.users.filter(u=>!u.disabled))people.append(new Option(u.displayName||u.id,'user:'+u.id));for(const g of groups)people.append(new Option('Group: '+g.name,'group:'+g.id));form.append(people);
  if(previous&&Array.from(people.options).some(o=>o.value===previous))people.value=previous;
  const choices=node('fieldset');choices.append(node('legend','Allowed operations'));const controls=[];
  for(const op of operations){const label=node('label'),input=node('input');input.type='checkbox';input.value=op.name;label.append(input,document.createTextNode(' '+op.name+(discovery.unavailable?'':op.readOnly?' · read':' · write')));choices.append(label);controls.push(input);}
  const selected=()=>{const [kind,id]=people.value.split(':');return {kind,id,key:kind==='group'?'groups':'users'};};
  const draw=()=>{const {key,id}=selected();for(const input of controls){input.checked=(grant[key]?.[id]||[]).includes(input.value);input.disabled=!!discovery.unavailable&&!input.checked;}};people.onchange=draw;draw();form.append(choices);
  const save=node('button','Save access','primary');save.type='submit';form.append(save);form.onsubmit=e=>{e.preventDefault();return busy(form,async()=>{const {key,id}=selected();if(!id)return;const updated={...grant,users:{...grant.users},groups:{...grant.groups}};updated[key][id]=controls.filter(c=>c.checked).map(c=>c.value);await manage('set_data_connection_access',updated);await connection(info,box);},{controls:save,report});};box.append(form);
  for(const op of discovery.operations||[]){const details=node('details');details.append(node('summary',op.name),node('p',op.description),node('pre',JSON.stringify(op.inputSchema,null,2)));box.append(details);}
 }
 const automations=initAutomations({manage,report,people:()=>directory,connections:()=>connectionList,operations:id=>call('list_data_operations',{connection:id})});
 async function load(people,orgGroups,active){pageActive=active;version++;const list=dataConnections()?await call('list_data_connections'):[];if(!active())return;directory=people;groups=orgGroups;connectionList=list;const box=document.getElementById('integration-connections'),details=node('div');box.replaceChildren();for(const info of list)box.append(button(info.label+' · '+(info.kind==='email'?'Email · '+(info.provider==='gmail'?'Gmail':info.provider):info.kind),()=>connection(info,details)));if(!list.length)box.append(node('p',dataConnections()?'No data connections configured.':'Data connections are turned off on this instance.'));box.append(details);document.getElementById('automation-jobs').hidden=!automationsOn();if(automationsOn())await automations.load(active);}
 return {load};
}
