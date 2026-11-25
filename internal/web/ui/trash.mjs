import {openDialog} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {notebookPath} from './request.mjs';
import {$} from './dom.mjs';
export function initTrash({api,post,getProject,canManage,beforeOpen,beforeMove,afterMove,refresh,notice}) {
 const dialog=$('trash-dialog');
 let listing=null,selected=null,busy=false,loading=false,loadVersion=0,generation=0,preview=null,undo=null;
 const context=()=>{const project=getProject();if(!project)throw Error('Choose a notebook first.');return {id:project.id,generation,prefix:notebookPath(project.id)}};
 const active=c=>c.id===getProject()?.id && c.generation===generation;
 const request=(c,path,body)=>body===undefined?api(c.prefix+'/api/'+path):post(c.prefix+'/api/'+path,body);
 function error(message){feedback($('trash-error'),message);}
 function controls(){
  $('trash-undo').disabled=busy;
  for(const b of dialog.querySelectorAll('button'))b.disabled=busy || loading;
  $('trash-close').disabled=busy;
  $('trash-empty').hidden=!canManage();$('trash-empty').disabled=busy || loading || !listing?.entries.length;
  $('trash-reload').hidden=!!listing && !listing.entries.length;
  $('trash-restore').disabled=busy || loading || !selected;$('trash-restore-fields').hidden=!selected || !canManage();
  $('trash-destination').disabled=busy || loading;
 }
 async function select(entry,c=context()){
  selected=entry;$('trash-destination').value=entry.originalPath;$('trash-preview').textContent='Loading…';controls();
  try{const note=await request(c,'note?path='+encodeURIComponent(entry.path));if(active(c)&&selected===entry)$('trash-preview').textContent=note.content}
  catch(e){if(active(c)&&selected===entry){$('trash-preview').textContent='';error(e.message)}}
 }
 async function load(id){
  const c=context(),version=++loadVersion;loading=true;controls();error('');
  try{
  const value=await request(c,'trash');if(!active(c) || version!==loadVersion)return;
  listing=value;selected=null;$('trash-preview').textContent='';$('trash-list').replaceChildren();
  for(const entry of value.entries){
   const row=document.createElement('li'),button=document.createElement('button'),detail=document.createElement('small');
   button.className='quiet';button.textContent=entry.title || entry.originalPath;button.onclick=()=>select(entry,c);
   detail.textContent=entry.originalPath+' · '+new Date(entry.deletedAt).toLocaleString();row.append(button,detail);$('trash-list').append(row);
  }
  if(!value.entries.length){const empty=document.createElement('li');empty.className='trash-empty-state';empty.textContent='Trash is empty.';$('trash-list').append(empty)}
  controls();if(id){const entry=value.entries.find(e=>e.id===id);if(entry)await select(entry,c)}
  }finally{if(active(c) && version===loadVersion){loading=false;controls()}}
 }
 async function open(id){
  if(!getProject() || !await beforeOpen())return;
  if(!dialog.open)openDialog(dialog);
  $('trash-list').textContent='Loading…';listing=null;selected=null;controls();
  try{await load(id)}catch(e){error(e.message)}
 }
 // One note, or a folder's notes as one transaction; Undo restores them all.
 async function move(selection,onError=notice){
  if(busy)return;
  const c=context(),paths=[].concat(selection);let finish;
  try{
   finish=await beforeMove(paths);busy=true;controls();
   const status=await request(c,'status');if(!active(c))return;
   const result=await request(c,'trash',{paths,revision:status.revision});if(!active(c))return;
   undo={entries:result.entries,project:c.id};$('trash-undo').hidden=false;
   await afterMove(paths);await refresh();notice(paths.length===1?'Moved to Trash.':`Moved ${paths.length} notes to Trash.`,true);
  }catch(e){if(active(c))onError(e.message)}finally{busy=false;finish?.();controls()}
 }
 async function restore(entry,to,c=context()){
  const result=await request(c,'trash/restore',{id:entry.id,to,revision:listing.revision});
  if(!active(c))return;
  if(undo?.entries.some(e=>e.id===entry.id)){undo.entries=undo.entries.filter(e=>e.id!==entry.id);if(!undo.entries.length){undo=null;$('trash-undo').hidden=true}}
  await refresh();await load();notice('Restored '+result.entries[0].path+'.',true);
 }
 $('trash-open').onclick=()=>open();
 $('trash-close').onclick=()=>dialog.close();
 $('trash-reload').onclick=()=>load(selected?.id).catch(e=>error(e.message));
 $('trash-restore').onclick=async()=>{
  if(busy || !selected)return;const entry=selected,c=context();busy=true;error('');controls();
  try{await restore(entry,$('trash-destination').value.trim(),c)}catch(e){if(active(c))error(e.message+' Choose another destination if occupied, or refresh Trash if the notebook changed.')}
  finally{busy=false;controls()}
 };
 $('trash-undo').onclick=async()=>{
  if(busy || !undo)return;const c=context();if(undo.project!==c.id)return;busy=true;controls();
  try{for(const entry of [...undo.entries]){listing=await request(c,'trash');if(!active(c))break;await restore(entry,'',c)}}catch(e){if(active(c)){notice(e.message);await open(undo?.entries[0]?.id)}}finally{busy=false;controls()}
 };
 $('trash-empty').onclick=async()=>{
  if(busy)return;const c=context();busy=true;controls();error('');
  try{
   const value=await request(c,'trash');if(!active(c))return;
   if(!value.entries.length){await load();return}
   preview={...value,context:c};$('trash-empty-error').textContent='';
   const n=value.entries.length;$('trash-empty-question').textContent=`Empty Trash and remove ${n} ${n===1?'note':'notes'}? You cannot restore these from Trash afterward.`;
   openDialog($('trash-empty-dialog'));$('trash-empty-cancel').focus();
  }catch(e){if(active(c))error(e.message)}finally{busy=false;controls()}
 };
 $('trash-empty-cancel').onclick=()=>$('trash-empty-dialog').close();
 $('trash-empty-dialog').addEventListener('close',()=>{preview=null});
 for(const modal of [dialog,$('trash-empty-dialog')])modal.addEventListener('cancel',e=>{if(busy)e.preventDefault()});
 $('trash-empty-confirm').onclick=async()=>{
  if(busy || !preview)return;const p=preview;busy=true;$('trash-empty-confirm').disabled=true;$('trash-empty-cancel').disabled=true;
  try{
   await request(p.context,'trash/empty',{revision:p.revision,confirmation:p.confirmation});if(!active(p.context))return;
   $('trash-empty-dialog').close();undo=null;$('trash-undo').hidden=true;await refresh();await load();notice('Trash emptied.',true);
  }catch(e){if(active(p.context))$('trash-empty-error').textContent=e.message+' Cancel and review Trash again before retrying.'}
  finally{busy=false;$('trash-empty-confirm').disabled=false;$('trash-empty-cancel').disabled=false;controls()}
 };
 function reset(){generation++;loadVersion++;loading=false;listing=null;selected=null;preview=null;undo=null;dialog.close();$('trash-empty-dialog').close();$('trash-undo').hidden=true;$('trash-preview').textContent=''}
 return {open,move,reset};
}
