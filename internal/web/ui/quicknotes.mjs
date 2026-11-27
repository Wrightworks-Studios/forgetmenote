import {openDialog,dialogShell} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {poll,moduleOn} from './state.mjs';
import {element as el,actionButton,routeLink,keepTyped} from './dom.mjs';

function destinationKey(destination){return JSON.stringify([destination.project,destination.folder || '']);}
function approvalKey(destination){return JSON.stringify([destination.project,destination.folder || '',!!destination.shared]);}
export function captureURL(capture){return '/?'+new URLSearchParams({project:capture.project,note:capture.path});}
export function initQuicknotes({api,post,getUser,getDraftStorage,onCapture=()=>{},openInbox=()=>{},openCaptureNote}) {
 const opener=document.getElementById('quicknotes-open'),count=document.getElementById('count-inbox'),dialog=dialogShell('Quick note',{id:'quicknotes-dialog',className:'judgments-dialog quicknotes-dialog'});
 const error=el('p',undefined,'judgments-error');error.hidden=true;error.setAttribute('role','alert');const status=el('p',undefined,'small');status.setAttribute('role','status');
 const form=el('form'),label=el('label','Quick note'),input=keepTyped(el('textarea'));input.id='quicknotes-text';input.rows=6;input.maxLength=65536;input.setAttribute('aria-label','Quick note');label.append(input);
 const save=el('button','Save to Inbox','primary');save.type='submit';const recovered=el('div',undefined,'quicknotes-recovery');form.append(label,save,recovered);
 const toolbar=el('div',undefined,'judgments-actions'),body=el('div'),recent=el('div');dialog.append(error,status,form);const inbox=document.getElementById('inbox-content'),inboxError=el('p',undefined,'judgments-error');inboxError.setAttribute('role','alert');inboxError.hidden=true;const inboxStatus=el('p',undefined,'small');inboxStatus.setAttribute('role','status');inbox.append(inboxError,inboxStatus,toolbar,body,recent);function report(message){status.textContent=message;inboxStatus.textContent=message;}const visible=()=>!document.getElementById('inbox-view').hidden || dialog.open;
 let returnFocus=document.getElementById('create-toggle');
 let account='',generation=0,controller=new AbortController(),draft={content:'',revision:'',operationId:''},busy=false,view=0,refreshID=0,destinations=[],settings=null,recentSignature='',timer=null;
 function scope(){return account+'/quicknotes/capture';}
 function token(){return {account,generation};}function valid(t){return t.account===getUser()?.id && t.account===account && t.generation===generation;}
 function options(){return {signal:controller.signal};}
 function fail(problem){if(problem.name==='AbortError')return;const message=problem.message || 'Quicknotes could not complete that request.';feedback(error,message);feedback(inboxError,message);if(problem.status===401){reset();}}
 function button(text,action){return actionButton(text,current=>action(current),{owner:()=>{const t=token();error.hidden=true;return ()=>valid(t);},report:fail});}
 function persist(){getDraftStorage().write('draft',scope(),draft);}
 function controls(){input.readOnly=busy||!!draft.operationId;save.disabled=busy||!input.value.trim();save.textContent=busy?'Saving…':draft.operationId?'Retry save':'Save to Inbox';}
 function restore(){const raw=getDraftStorage()?.read('draft',scope());try{const saved=JSON.parse(raw);draft=saved && typeof saved.content==='string'?{content:saved.content,revision:'',operationId:saved.operationId || ''}:{content:'',revision:'',operationId:''};}catch{draft={content:'',revision:'',operationId:''};}input.value=draft.content;controls();renderRecovered();}
 function renderRecovered(){
  recovered.replaceChildren();for(const candidate of getDraftStorage()?.candidates(scope()) || []) {
   if(candidate.owner===getDraftStorage().owner)continue;
   const details=el('details');details.append(el('summary','Saved draft from another tab · '+new Date(candidate.draft.savedAt).toLocaleString()));const preview=el('pre',candidate.draft.content);details.append(preview);
   details.append(button('Restore this draft',()=>{if(busy||draft.operationId)throw Error('Finish the pending save before restoring another draft.');getDraftStorage().adopt(scope(),candidate);restore();}));recovered.append(details);
  }
 }
 function reset(){generation++;view++;refreshID++;timer?.stop();recentSignature='';controller.abort();controller=new AbortController();dialog.close();input.value='';count.textContent='';body.replaceChildren();recent.replaceChildren();recovered.replaceChildren();report('');error.hidden=true;draft={content:'',revision:'',operationId:''};busy=false;destinations=[];settings=null;}
 function userChanged(){const user=getUser()?.id || '';if(account===user)return;reset();account=user;opener.hidden=!user||user==='@local-admin';document.getElementById('inbox-open').hidden=opener.hidden;if(!opener.hidden){restore();refresh().catch(fail);}}
 input.oninput=()=>{draft={content:input.value,revision:'',operationId:''};try{persist();error.hidden=true;}catch(e){fail(Error('This browser could not preserve the draft. Keep this window open and copy the text before leaving. '+e.message));}controls();};
 form.onsubmit=async event=>{
  event.preventDefault();if(busy||!input.value.trim())return;const t=token(),savedScope=scope();
  draft={...draft,content:input.value,operationId:draft.operationId||crypto.randomUUID()};
  try{persist();}catch(e){fail(Error('The recovery record could not be saved in this browser. Copy your text before leaving. '+e.message));controls();return;}
  const submission={operationId:draft.operationId,content:draft.content};busy=true;controls();error.hidden=true;
  try {
   const capture=await post('/api/quicknotes',submission,options());if(!valid(t))return;
   // Resolve only this tab's acknowledged immutable submission. Foreign/newer
   // browser drafts keep their own version-guarded recovery records.
   getDraftStorage().resolve(savedScope,null);draft={content:'',revision:'',operationId:''};input.value='';report('Saved privately in your Inbox.');onCapture(capture);renderRecovered();await refresh();
  }catch(e){if(valid(t)){if(e.status && e.status<500 && ![408,409,429].includes(e.status)){draft.operationId='';try{persist();}catch{}}fail(e);}}
  finally{if(valid(t)){busy=false;controls();}}
 };
 async function refresh(){
  if(!account||account==='@local-admin')return;
  const t=token(),request=++refreshID,withCaptures=visible();timer?.stop();
  let data;
  try{data=await api('/api/quicknotes'+(withCaptures?'':'?summary=1'),options());}
  catch(e){if(!valid(t)||request!==refreshID||e.name==='AbortError')return;count.textContent='';throw e;}
  if(!valid(t)||request!==refreshID)return;
  count.textContent=Number.isInteger(data.inboxCount)?String(data.inboxCount):'';count.hidden=!data.inboxCount;
  if(!withCaptures||!visible())return;
  const signature=JSON.stringify(data.captures);timer=poll(()=>{refresh().catch(fail);return false;},{every:3000,active:()=>valid(t)&&visible()});if(signature===recentSignature)return;recentSignature=signature;
  recent.replaceChildren(el('h3','Captured notes'));if(!data.captures?.length)recent.append(el('p','Your Inbox is empty.','small'));
  for(const capture of data.captures || [])recent.append(captureCard(capture));
 }
 function captureCard(capture){
  const card=el('article',undefined,'judgments-card');card.dataset.capture=capture.id;card.append(el('h4',capture.title || capture.path?.split('/').at(-1) || 'Quick note'),el('p',capture.status.replaceAll('_',' '),'small'));
  if(capture.excerpt)card.append(el('p',capture.excerpt));if(capture.error)card.append(el('p',capture.error,'small'));
  const actions=el('div',undefined,'judgments-actions'),link=el('a','Open note','button quiet');link.href=captureURL(capture);routeLink(link,()=>openCaptureNote?.(capture).catch(fail));actions.append(link);
  if(capture.suggestion){const destination=capture.suggestion.destination || capture.suggestion;card.append(el('p','Suggested: '+(destination.name || destination.project)+(destination.folder?' / '+destination.folder:''),'small'));if(destination.project)actions.append(button('Review suggested destination',()=>chooseDestination(capture,destination)));}
  if(['inbox','suggested'].includes(capture.status)){actions.append(button('Choose destination',()=>chooseDestination(capture)));actions.append(button('Suggest destination',async(current)=>{await post('/api/quicknotes/sort',{id:capture.id},options());if(current())await refresh();}));}
  if(capture.canUndo)actions.append(button('Undo filing',async(current)=>{await post('/api/quicknotes/undo',{id:capture.id},options());if(current()){report('Returned to your private Inbox.');await refresh();}}));
  if(['saving','filing','undoing'].includes(capture.status))actions.append(button('Recover interrupted action',async(current)=>{await post('/api/quicknotes/recover',{id:capture.id},options());if(current())await refresh();}));
  card.append(actions);return card;
 }
 async function chooseDestination(capture,suggestion){
  const t=token(),version=++view;body.replaceChildren(el('p','Loading destinations…'));const data=await api('/api/quicknotes/destinations',options());if(!valid(t)||version!==view||!visible())return;destinations=data.destinations || [];
  const section=el('section');section.append(el('h3','File '+(capture.title || 'quick note')));const select=el('select');select.setAttribute('aria-label','Destination');select.append(el('option','Choose a destination…'));select.firstChild.value='';
  destinations.forEach((destination,index)=>{const option=el('option',destinationLabel(destination));option.value=String(index);select.append(option);if(suggestion && destinationKey(suggestion)===destinationKey(destination))select.value=String(index);});
  const explanation=el('p',undefined,'small');const update=()=>{const target=destinations[Number(select.value)];explanation.textContent=select.value===''?'Private until filed.':target.shared?'Visible to this notebook’s members.':'Private to you.';};select.onchange=update;update();
  section.append(select,explanation,button('File note',async(current)=>{if(select.value==='')throw Error('Choose a destination.');const destination=destinations[Number(select.value)];await post('/api/quicknotes/file',{id:capture.id,destination:{project:destination.project,folder:destination.folder || ''},version:capture.version},options());if(current()&&version===view){body.replaceChildren();report('Filed in '+destinationLabel(destination)+'.');await refresh();}}),button('Keep in Inbox',()=>{view++;body.replaceChildren();}));body.replaceChildren(section);
 }
 function destinationLabel(destination){return (destination.name || destination.project)+(destination.folder?' / '+destination.folder:' / root')+(destination.shared?' · shared':'');}
 async function openSettings(){
  const t=token(),version=++view;body.replaceChildren(el('p','Loading preferences…'));const [saved,available,policy]=await Promise.all([api('/api/quicknotes/settings',options()),api('/api/quicknotes/destinations',options()),moduleOn(getUser(),'judgments')?api('/api/judgments/settings',options()):{available:false,off:true}]);if(!valid(t)||version!==view||!visible())return;settings=saved;destinations=available.destinations || [];
  const section=el('section');section.append(el('h3','Quicknotes preferences'));const label=el('label','Sorting mode'),mode=el('select');mode.setAttribute('aria-label','Sorting mode');for(const [value,text] of [['manual','Manual'],['suggest','Suggest destinations'],['automatic','File automatically to approved destinations']]){const option=el('option',text);option.value=value;mode.append(option);}mode.value=settings.mode;label.append(mode);section.append(label);
  const consent=check('Allow TypeSafe to read saved Quicknotes for sorting',settings.cloudConsent),notify=check('Notify me when a destination is chosen',settings.notifyDestination);
  const remoteDisabled=!policy.available || policy.localOnly;const updateConsent=()=>{consent.input.disabled=remoteDisabled&&!consent.input.checked;};consent.input.onchange=updateConsent;updateConsent();for(const option of mode.options)if(option.value!=='manual')option.disabled=remoteDisabled;
  if(remoteDisabled)section.append(el('p',policy.off?'Remote sorting needs the judgments module, which is turned off on this instance.':policy.localOnly?'Remote sorting is disabled by local-only policy.':'Remote sorting is unavailable.','small'));
  section.append(consent.row,notify.row);
  const approved=new Set((settings.approvedDestinations || []).map(approvalKey)),choices=el('fieldset');choices.append(el('legend','Approved automatic destinations'));
  for(const destination of destinations){const control=check(destinationLabel(destination),approved.has(approvalKey(destination)));control.input.onchange=()=>control.input.checked?approved.add(approvalKey(destination)):approved.delete(approvalKey(destination));choices.append(control.row);}
  const warning=el('p','Notes filed in shared destinations are visible to their members.','small');section.append(choices,warning);
  section.append(button('Save Quicknotes preferences',async(current)=>{const chosen=destinations.filter(destination=>approved.has(approvalKey(destination))).map(({project,folder,shared})=>({project,folder:folder || '',shared:!!shared}));if(mode.value==='automatic'&&remoteDisabled)throw Error('Remote sorting is unavailable. Choose Manual to save these preferences.');if(mode.value==='automatic'&&(!consent.input.checked||!chosen.length))throw Error('Automatic filing requires remote sorting consent and at least one approved destination.');const next=await post('/api/quicknotes/settings',{mode:mode.value,cloudConsent:consent.input.checked,notifyDestination:notify.input.checked,approvedDestinations:chosen,generation:settings.generation},options());if(current()&&version===view){settings=next;body.replaceChildren();report('Quicknotes preferences saved.');}}),button('Close preferences',()=>{view++;body.replaceChildren();}));body.replaceChildren(section);
 }
 function check(text,checked){const row=el('label',undefined,'judgments-check'),input=el('input');input.type='checkbox';input.checked=!!checked;row.append(input,document.createTextNode(text));return {row,input};}
 toolbar.append(button('Refresh captures',refresh),button('Preferences',openSettings));
 function openCapture(trigger){returnFocus=trigger;error.hidden=true;report('');body.replaceChildren();view++;restore();if(!dialog.open)openDialog(dialog);input.focus();refresh().catch(fail);}
 opener.onclick=()=>openCapture(document.getElementById('create-toggle'));
 dialog.addEventListener('close',()=>{view++;if(!visible())timer?.stop();if(returnFocus?.getClientRects().length)returnFocus.focus();else document.getElementById('menu').focus();});
 window.addEventListener('storage',()=>{if(dialog.open&&account===getUser()?.id)renderRecovered();});
 document.addEventListener('visibilitychange',()=>{if(!document.hidden)refresh().catch(fail);});
 window.addEventListener('online',()=>refresh().catch(fail));
 document.getElementById('inbox-capture').onclick=event=>openCapture(event.currentTarget);dialog.append(button('Open Inbox',()=>{dialog.close();return openInbox();}));
 return {userChanged,open:()=>opener.click(),show:()=>{inboxError.hidden=true;recentSignature='';return refresh().catch(fail);},hide:()=>{view++;timer?.stop();}};
}
