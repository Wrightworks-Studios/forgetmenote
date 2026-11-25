import {element as node,button as control,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';

const button=(text,action)=>control(text,action,{class:'quiet'});
const list=value=>value.split(',').map(v=>v.trim()).filter(Boolean);

export function initChatClients({post,getUser,getActive}) {
 const root=document.getElementById('chat-clients-panel');let version=0;
 async function open(){
  const active=getActive(),current=++version;root.replaceChildren(node('p','Loading chat clients…'));
  try{
   const data=await post('/api/organization/action/chat_clients',{});
   if(current!==version||!getUser()?.admin||!active())return;
   const config=structuredClone(data.config);
   const controls=node('div');controls.className='actions settings-heading';
   controls.append(node('h3','Chat clients'),button('Refresh',open));root.replaceChildren(controls);
   if(!data.available.length){root.append(node('p','Chat clients connect messaging applications such as Slack to notebooks. None is set up on this server yet: add the client’s credentials to the server configuration (for Slack, SLACK_CONFIG_FILE), restart, then refresh this page. Credentials never reach the browser.','small'));return;}
   for(const id of data.available){
    const client=config.clients[id]||{workspace:'',disabled:true,users:{},channels:{},directMessages:{}};
    const form=node('form');form.className='settings-form chat-client-form';form.append(node('h3',id==='slack'?'Slack':id));
    const status=node('p');status.setAttribute('role','status');
    function field(parent,label,value='',type='text'){
     const wrap=node('label',type==='checkbox'?undefined:label),input=node(type==='textarea'?'textarea':'input');input.setAttribute('aria-label',label);
     if(type==='checkbox'){input.type='checkbox';input.checked=!!value;wrap.className='check-row';wrap.append(input,node('span',label));}
     else{if(type==='textarea'){input.rows=5;input.spellcheck=false;}else input.type=type;input.value=value;wrap.append(input);}
     parent.append(wrap);return input;
    }
    const enabled=field(form,'Enable '+id,!client.disabled,'checkbox');
    const workspace=field(form,'Workspace ID',client.workspace);workspace.readOnly=true;
    if(id==='slack'){const endpoint=field(form,'Slack event URL',data.eventURL);endpoint.readOnly=true;const interactions=field(form,'Slack interactivity URL',data.interactionURL);interactions.readOnly=true;const command=field(form,'Slack /task command URL',data.commandURL);command.readOnly=true;}
    const links=node('fieldset');links.append(node('legend','Linked accounts'));form.append(links);
    const accountRows=[];
    function account(external='',user=''){
     const row=node('div');row.className='form-columns';const externalInput=field(row,'Chat user ID',external);const userInput=field(row,'ForgetMeNote user ID',user);
     const state={row,externalInput,userInput};accountRows.push(state);row.append(button('Remove account',()=>{accountRows.splice(accountRows.indexOf(state),1);row.remove();}));links.append(row);
    }
    for(const [external,user] of Object.entries(client.users||{}))account(external,user);
    links.append(button('Link account',()=>account()));
    const destinations=node('div');form.append(destinations);const rows=[];
    function destination(kind='channels',id='',binding={project:'',externalProcessing:true}){
     const box=node('fieldset');box.className='chat-destination';box.append(node('legend',kind==='channels'?'Channel':'Direct messages'));
     const key=field(box,kind==='channels'?'Channel ID':'DM user ID',id);
     const project=field(box,'Primary notebook',binding.project||'');
     const notebooks=field(box,'Additional notebooks', (binding.readProjects||[]).join(', '));
     const connections=field(box,'Data connections',(binding.connections||[]).join(', '));
     const actors=field(box,'Allowed members',(binding.actors||[]).join(', '));
     const shared=field(box,'Shared replies',!!binding.shared,'checkbox');const approved=field(box,'Approve this channel audience for the selected notebooks and data',!!binding.audienceApproved,'checkbox');
     const context=field(box,'Include thread context',!!binding.threadHistory,'checkbox');const attachments=field(box,'Process attached files',!!binding.attachments,'checkbox');
     const consent=field(box,'Allow processing by the configured AI providers',!!binding.externalProcessing,'checkbox');
     if(kind==='directMessages'){shared.closest('label').hidden=true;approved.closest('label').hidden=true;actors.closest('label').hidden=true;}
     const state={box,read:()=>({kind,key:key.value.trim(),binding:{...binding,project:project.value.trim(),readProjects:list(notebooks.value),connections:list(connections.value),actors:kind==='channels'?list(actors.value):[],shared:kind==='channels'&&shared.checked,audienceApproved:kind==='channels'&&approved.checked,threadHistory:context.checked,attachments:attachments.checked,externalProcessing:consent.checked}})};
     rows.push(state);box.append(button('Remove destination',()=>{rows.splice(rows.indexOf(state),1);box.remove();}));destinations.append(box);
    }
    for(const kind of ['channels','directMessages'])for(const [key,binding]of Object.entries(client[kind]||{}))destination(kind,key,binding);
    const add=node('div');add.className='actions';add.append(button('Add channel',()=>destination()),button('Add direct messages',()=>destination('directMessages')));form.append(add);
    const advanced=node('details');advanced.append(node('summary','JSON configuration'));const json=field(advanced,'Chat client JSON',JSON.stringify(client,null,2),'textarea');let jsonChanged=false;json.oninput=()=>{jsonChanged=true;};form.append(advanced);
    const submit=node('button','Save chat client','primary');submit.type='submit';form.append(submit,status);root.append(form);
    form.onsubmit=event=>{
     event.preventDefault();if(current!==version||!active())return;status.textContent='';
     return busy(form,async()=>{
      let value;
      if(jsonChanged)value=JSON.parse(json.value);
      else{
       value={...client,disabled:!enabled.checked,users:{},channels:{},directMessages:{}};
       for(const r of accountRows){const external=r.externalInput.value.trim(),user=r.userInput.value.trim();if(!external||!user)throw Error('Enter both account IDs.');if(value.users[external])throw Error('Each chat account can be linked once.');value.users[external]=user;}
       for(const r of rows){const v=r.read();if(!v.key)throw Error('Enter a destination ID.');if(value[v.kind][v.key])throw Error('Each destination can be configured once.');value[v.kind][v.key]=v.binding;}
      }
      const next={...config,clients:{...config.clients,[id]:value}};
      const result=await post('/api/organization/action/set_chat_clients',{config:next});if(current!==version||!active())return;if(jsonChanged){await open();return;}Object.assign(config,result.config);Object.assign(client,result.config.clients[id]);json.value=JSON.stringify(client,null,2);jsonChanged=false;feedback(status,'Chat client saved.',{level:'success'});
     },{controls:submit,report:status,current:()=>current===version&&active()});
    };
   }
  }catch(error){if(current===version&&active())root.replaceChildren(node('p',error.message));}
 }
 return {open};
}
