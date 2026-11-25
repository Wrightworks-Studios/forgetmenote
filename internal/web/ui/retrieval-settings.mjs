import {notebookPath} from './request.mjs';
import {$,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';
const labels={include:'Include',lower:'Lower priority',exclude:'Exclude'};
const fields=['archives','drafts'];
const purposes=['search','ai'];

// Shared defaults live at the gateway; only explicit personal search choices
// live on this browser. Neither owner changes nor reloads overwrite those choices.
export function initRetrievalSettings({api,post,getProject,getUser,onChange}) {
 let state=null,selectedProject=null,loadVersion=0;
 const key=()=>`gitnotes:search-containers:${getUser()?.id}:${getProject()?.id}`;
 function renderDefaults(){
  for(const field of fields){
   const select=$('search-'+field),value=select.value;
   select.replaceChildren(new Option(state?'Default: '+labels[state.settings.search[field]]:'Notebook default',''),...Object.entries(labels).map(([id,label])=>new Option(label,id)));
   select.value=value;
  }
 }
 function preferences(){return Object.fromEntries(fields.filter(field=>labels[$('search-'+field).value]).map(field=>[field,$('search-'+field).value]));}
 function remember(){try{localStorage.setItem(key(),JSON.stringify(preferences()))}catch{}}
 for(const field of fields)$('search-'+field).onchange=()=>{remember();onChange()};
 async function load(){
  const project=getProject()?.id,version=++loadVersion;
  if(!project)return;
  const next=await api(notebookPath(project)+'/api/knowledge/retrieval-settings');
  if(project!==getProject()?.id || version!==loadVersion)return;
  state=next;selectedProject=project;renderDefaults();
 }
 async function projectChanged(){
  ++loadVersion;state=null;selectedProject=null;
  let values={};try{values=JSON.parse(localStorage.getItem(key())) || {}}catch{}
  for(const field of fields)$('search-'+field).value=labels[values[field]]?values[field]:'';
  renderDefaults();
  await load();
 }
 function chips(add){
  for(const field of fields){const value=$('search-'+field).value;if(value)add((field==='archives'?'Archives':'Drafts')+': '+labels[value],()=>{$('search-'+field).value='';remember()})}
 }
 async function openSettings(){
  $('retrieval-defaults-status').textContent='';$('retrieval-defaults-save').disabled=true;
  for(const purpose of purposes)for(const field of fields)$('default-'+purpose+'-'+field).disabled=true;
  try{
   await load();if(selectedProject!==getProject()?.id || !state)return;
   const writable=getUser()?.admin || getProject()?.role==='owner';
   for(const purpose of purposes)for(const field of fields){const select=$('default-'+purpose+'-'+field);select.value=state.settings[purpose][field];select.disabled=!writable}
   $('retrieval-defaults-save').hidden=!writable;$('retrieval-defaults-save').disabled=!writable;
  }catch(error){$('retrieval-defaults-status').textContent=error.message}
 }
 $('retrieval-defaults-form').onsubmit=event=>{
  event.preventDefault();const project=getProject()?.id,base=state;
  if(!base || selectedProject!==project)return;
  const settings=structuredClone(base.settings);
  for(const purpose of purposes)for(const field of fields)settings[purpose][field]=$('default-'+purpose+'-'+field).value;
  const status=$('retrieval-defaults-status');status.textContent='';
  // The save button stays retired once another notebook is open.
  return busy($('retrieval-defaults-form'),async()=>{
   const saved=await post(notebookPath(project)+'/api/knowledge/retrieval-settings',{settings,expectedId:base.id});
   if(project!==getProject()?.id)return;
   state=saved;renderDefaults();feedback(status,'Defaults saved.',{level:'success'});onChange();
  },{controls:$('retrieval-defaults-save'),report:error=>feedback(status,error.message+' Reopen notebook settings to reload.'),current:()=>project===getProject()?.id,retire:true});
 };
 renderDefaults();
 return {preferences,projectChanged,openSettings,chips};
}
