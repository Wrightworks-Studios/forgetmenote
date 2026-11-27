import {element as node,button as control,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';
import {choiceSelect,chooseOption} from './pickers.mjs';
import {moduleOn} from './state.mjs';

const button=(text,action)=>control(text,action,{class:'quiet'});
// Phrases are typed one per line; an unchanged field keeps the saved list.
const lines=text=>text.split('\n').map(line=>line.trim()).filter(Boolean);

// Task queue settings: the messaging client whose members have queues, the
// group of accounts that may use tasks, away statuses, holiday titles and a
// pause. They are read and saved with task_queues and set_task_queues, the
// tools MCP and API clients use; the server checks every value and refuses a
// save made from an older revision.
export function initTaskQueues({post,getUser,getActive}){
 const root=document.getElementById('task-queues-panel');let version=0;
 const call=(name,input={})=>post('/integrations/actions/'+name,input);
 async function open(){
  const active=getActive(),current=++version;root.replaceChildren(node('p','Loading task queues…'));
  const live=()=>current===version&&!!getUser()?.admin&&active();
  try{
   const [status,chat]=await Promise.all([post('/api/organization/action/organization_status',{}),moduleOn(getUser(),'messaging')?post('/api/organization/action/chat_clients',{}).catch(()=>({config:{}})):{config:{}}]);
   if(!live())return;
   const heading=node('div');heading.className='actions settings-heading';heading.append(node('h3','Task queues'),button('Refresh',open));
   if((status.modules||[]).some(module=>module.name==='tasks'&&!module.enabled)){root.replaceChildren(heading,node('p','Task queues are switched off for this instance. An operator turns them on by removing "tasks" from modules.off in instance.json.','small'));return;}
   const settings=await call('task_queues');if(!live())return;
   // Only a client saved under Chat clients can hold queues; until the first
   // save that page shows the server's file configuration, which does not count.
   const clients=chat.config?.revision>0?Object.keys(chat.config.clients||{}):[];
   render(heading,settings,status.organization?.groups||[],clients,!clients.length&&!!chat.available?.length);
  }catch(error){if(live())root.replaceChildren(node('p',error.message));}
 }
 function render(heading,settings,groups,clients,unsaved){
  const current=version,enabled=settings.revision>0;
  const form=node('form');form.className='settings-form task-queue-form';
  const state=node('p',enabled?settings.disabled?'Paused: every task action is refused until you resume them.':'On. Queues live in the task-queues notebook.':'Off. Turning them on creates the task-queues notebook, where every queue is kept.','small');
  const status=node('p');status.setAttribute('role','status');
  function labelled(text,control,hint){const wrap=node('label',text);control.setAttribute('aria-label',text);wrap.append(control);if(hint)wrap.append(node('small',hint));form.append(wrap);return control;}
  const client=labelled('Messaging client',choiceSelect('Messaging client',clients.map(id=>[id,id==='slack'?'Slack':id]),{placeholder:'None'}),!moduleOn(getUser(),'messaging')?'Slack messaging is turned off on this instance, so queues work without a client: nobody gets reminders or notices.':unsaved?'Save the client under Chat clients first; then choose it here.':'Members of this client’s workspace have queues and get reminders. Clients are set up under Chat clients.');
  // A first setup starts from the only configured client.
  chooseOption(client,settings.client||(!enabled&&clients.length===1?clients[0]:''),value=>`${value} (not configured)`);
  const group=labelled('Accounts that may use tasks',choiceSelect('Accounts that may use tasks',groups.map(g=>[g.id,g.name||g.id]),{placeholder:'Everyone with an account'}),'Workspace members can use tasks either way.');
  chooseOption(group,settings.group||'',value=>`${value} (deleted group)`);
  function phrases(text,saved,hint){const area=node('textarea');area.rows=3;area.spellcheck=false;area.value=(saved||[]).join('\n');const initial=area.value;labelled(text,area,hint);return ()=>area.value===initial?saved:lines(area.value);}
  const away=phrases('Away statuses (one per line)',settings.awayStatuses,'Words in a Slack status that mean someone is away that day. Blank uses the built-in list: vacationing, out sick, OOO, out of office, PTO, on leave.');
  const holidays=phrases('Holiday titles (one per line)',settings.holidayTitles,'All-day events on the reminder’s holiday calendars whose titles begin with these are days off. Blank means “holiday”.');
  let paused=null;
  if(enabled){const wrap=node('label');wrap.className='check-row';paused=node('input');paused.type='checkbox';paused.checked=!!settings.disabled;wrap.append(paused,node('span','Pause every task action'));form.append(wrap);}
  // The server names the reminder jobs, and warns while there is none.
  const reminders=settings.reminders||[];
  form.append(node('p',enabled&&reminders.length?`Weekday reminders come from the ${reminders.join(', ')} automation under Automation.`:'Weekday reminders come from a task-reminder automation under Automation.','small'));
  const submit=node('button',enabled?'Save task queues':'Turn on task queues','primary');submit.type='submit';form.append(submit,status);
  const warnings=(settings.warnings||[]).map(text=>node('p',text,'settings-warning'));
  root.replaceChildren(heading,state,...warnings,form);
  form.onsubmit=event=>{
   event.preventDefault();if(current!==version||!getActive()())return;status.textContent='';
   return busy(form,async()=>{
    const value={...settings,client:client.value,group:group.value,awayStatuses:away(),holidayTitles:holidays(),disabled:paused?.checked||false};
    for(const key of ['client','group','awayStatuses','holidayTitles','disabled'])if(value[key]===''||value[key]===false||Array.isArray(value[key])&&!value[key].length||value[key]==null)delete value[key];
    // The revision this form was drawn from: a change saved elsewhere since is refused.
    let saved;
    try{saved=await call('set_task_queues',value);}catch(error){throw error.status===409?Error('Task queue settings changed since this page loaded. Refresh to see them, then save again.'):error;}
    if(current!==version)return;
    render(heading,saved,groups,clients,unsaved);
    feedback(root.querySelector('.task-queue-form [role=status]'),enabled?'Task queues saved.':'Task queues are on.',{level:'success'});
   },{controls:submit,report:status,current:()=>current===version});
  };
 }
 return {open};
}
