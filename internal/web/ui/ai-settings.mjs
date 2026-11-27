import {$,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';
const features = ['embeddings','mapClassification','rerank','citations','reviews','writing'];

// A disabled feature says why next to its label.
function hint(input,text) {
 const id=input.id+'-hint';let note=document.getElementById(id);
 if(!note&&text){note=document.createElement('span');note.id=id;note.className='check-hint';input.closest('label').after(note);}
 if(note){note.textContent=text;note.hidden=!text;}
 if(text)input.setAttribute('aria-describedby',id);else input.removeAttribute('aria-describedby');
}

export function initAISettings({post,getUser,getActive}) {
 let state, version=0;
 const form=$('instance-ai-form'), status=$('instance-ai-status');
 function render(data) {
  state=data;
  const providers=data.providers, config=data.config;
  $('instance-ai-providers').textContent=`Embeddings: ${providers.embeddings?'connected':'not configured'} · Decisions: ${providers.decisions?'connected':'not configured'}`;
  for(const feature of features) {
   const input=$('instance-ai-'+feature);input.checked=!!config[feature];
   const unavailable=feature==='embeddings'?!providers.embeddings:feature==='writing'?false:!providers.decisions||providers.localOnly;
   input.disabled=unavailable&&!input.checked;
   hint(input,input.disabled?feature==='embeddings'?'Requires a configured embeddings provider.':providers.localOnly&&providers.decisions?'Unavailable while AI is limited to local processing.':'Requires a configured decision provider.':'');
  }
  $('instance-ai-model').value=config.model||'';$('instance-ai-dimensions').value=config.dimensions||1536;
  $('instance-ai-rebuild').checked=false;
  const notebooks=$('instance-ai-notebooks');notebooks.replaceChildren();
  for(const item of data.notebooks) {
   const row=document.createElement('div');row.className='ai-notebook-status';
   const name=document.createElement('strong');name.textContent=item.name;
   const detail=document.createElement('span');detail.className='small';
   const index=item.index, semantic=item.semantic;
   detail.textContent=item.error || (index ? `${index.embedded||0}/${index.chunks||0} passages · ${index.mapClassified||0} map decisions${item.paused?' · Paused':''}${[index.error,index.embeddingError,index.mapError].filter(Boolean).map(e=>' · '+e).join('')}` : `${semantic?.indexed||0}/${semantic?.total||0} passages${semantic?.message?' · '+semantic.message:''}`);
   row.append(name,detail);notebooks.append(row);
  }
  if(!data.notebooks.length)notebooks.textContent='No notebooks.';
  update();
 }
 function update() {
  const enabled=$('instance-ai-embeddings').checked;
  for(const id of ['model','dimensions']) { $('instance-ai-'+id).disabled=!enabled;$('instance-ai-'+id).required=enabled; }
  $('instance-ai-embedding-options').hidden=!enabled;
 }
 form.addEventListener('change',update);
 async function open() {
  if(!getUser()?.admin)return;
  const active=getActive(),request=++version;state=null;form.inert=true;feedback(status,'Loading…',{level:'info'});
  try {const data=await post('/api/organization/action/ai_settings',{});if(request!==version||!getUser()?.admin||!active())return;render(data);form.inert=false;status.textContent='';}
  catch(error){if(request===version&&active())feedback(status,error.message);}
 }
 $('instance-ai-refresh').onclick=open;
 form.onsubmit=event=>{
  event.preventDefault();if(!state||!getUser()?.admin)return;
  const request=++version,saved=text=>feedback(status,text,{level:'success'});saved('Saving…');
  const config={revision:state.config.revision,model:$('instance-ai-model').value.trim(),dimensions:Number($('instance-ai-dimensions').value)};
  for(const feature of features)config[feature]=$('instance-ai-'+feature).checked;
  return busy(form,async()=>{const data=await post('/api/organization/action/set_ai_settings',{config,rebuild:$('instance-ai-rebuild').checked});if(request!==version||!getUser()?.admin)return;render(data);saved('AI settings saved for all notebooks.');},
   {controls:event.submitter,report:status,current:()=>request===version});
 };
 return {open};
}
