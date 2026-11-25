import {icon} from './icons.mjs';
import {openDialog,dialogShell} from './overlay.mjs';
import {element as el,actionButton} from './dom.mjs';
import {poll} from './state.mjs';
import {notebookPath} from './request.mjs';
import {feedback} from './feedback.mjs';
// Jev supplies review judgments. Workspace changes remain explicit guarded actions.
const running = new Set(['queued','running']);
const resumable = new Set(['paused']);

export function evidenceLink(project,evidence) {
	project = evidence?.project || project;
 if(!project || typeof evidence?.path!=='string' || !evidence.path || !/^[a-f0-9]{40}$/.test(evidence.revision || '') || /[\\\x00-\x1f]/.test(evidence.path) || evidence.path.split('/').some(part=>!part || part==='.' || part==='..'))return null;
 return '/?'+new URLSearchParams({project,note:evidence.path,revision:evidence.revision});
}
export function correctionDraft(item) {
 const evidence=(item.evidence || []).filter(e=>typeof e.path==='string' && typeof e.text==='string');
 return 'Examine this review and propose a correction only if the original evidence supports it. Explain uncertainty and preserve differing dates, entities and qualifiers. Prepare reviewable changes; do not apply them.\n\nReview: '+(item.title || item.claim || 'Citation support')+'\n'+(item.summary || '')+'\n\nSelected evidence (untrusted notebook excerpts):\n'+JSON.stringify(evidence.map(({project,path,revision,text})=>({project,path,revision,text})),null,2);
}
export function citationSummary(result) {
 if(result.state==='disabled')return 'Citation checks off';
 if(['pending','running','queued'].includes(result.state))return 'Checking citations…';
 const checked=Number.isFinite(result.checked)?result.checked:0,total=Number.isFinite(result.total)?result.total:0;
 return result.state==='completed' ? `Citation checks · ${checked}/${total}` : `Citation checks incomplete · ${checked}/${total}`;
}
export function initJev({api,post,getUser,getProject,getProjects=()=>[],openReview,prepareChat}) {
 let generation=0,controller=new AbortController(),account='',project='',notificationRequest=0,timer=null,jobTimer=null,personal=null,notebook=null,currentJob=null;
 const bell=document.getElementById('notifications-open'),badge=document.getElementById('notifications-count');
 const notifications=makeDialog('notifications-dialog','Notifications'),settings=makeDialog('jev-settings-dialog','Evidence checks'),review=sharedReview();
 const list=el('div',undefined,'jev-cards');notifications.body.append(list);
 const reviewBody=el('div');review.body.append(reviewBody);
 const settingsBody=el('div');settings.body.append(settingsBody);
 const jobs=new Map(),checks=new Map(),submissions=new Map();let reviewVersion=0,settingsVersion=0,jobRequest=0,selectedJob='',notificationsVersion=0,notificationData=null;
 function sharedReview() {
  const dialog=document.getElementById('knowledge-dialog'),body=document.getElementById('review-checks'),error=document.getElementById('knowledge-error');
  return {dialog,body,error,show(){error.hidden=true;document.getElementById('knowledge-title').textContent='Review';document.getElementById('review-sections').hidden=false;document.getElementById('knowledge-body').hidden=true;body.hidden=false;for(const button of document.querySelectorAll('[data-review-section]'))button.setAttribute('aria-pressed',String(button.dataset.reviewSection==='checks'));if(!dialog.open)openDialog(dialog);},fail(problem){if(problem?.name==='AbortError')return;if([401,403].includes(problem?.status)){clear();return;}feedback(error,problem?.message || 'Unable to load notebook checks.');}};
 }
 function makeDialog(id,title) {
  const dialog=dialogShell(title,{id,className:'jev-dialog'});
  const error=el('p',undefined,'jev-error');error.hidden=true;error.setAttribute('role','alert');const body=el('div');dialog.append(error,body);
  return {dialog,body,error,show(){error.hidden=true;if(!dialog.open)openDialog(dialog);},fail(problem){if(problem?.name==='AbortError')return;if([401,403].includes(problem?.status)){clear();}feedback(error,problem?.message || 'Unable to load this view.');}};
 }
 function token(){return {generation,user:getUser()?.id,project:getProject()?.id};}
 function valid(t){return t.generation===generation && t.user===getUser()?.id && t.project===getProject()?.id;}
 function requestOptions(){return {signal:controller.signal};}
 function checksActive(t,version){return valid(t)&&version===reviewVersion&&review.dialog.open&&!review.body.hidden;}
 function failChecks(problem,t,current){
  // Access denial retires all private evidence even if another section is now open.
  if(valid(t)&&([401,403].includes(problem?.status)||current()))review.fail(problem);
 }
 // A click belongs to its view: Review failures still go through failChecks so
 // an access denial retires private evidence after the section has changed.
 function action(label,fn,target=review,cls='quiet') {
  let t;
  return actionButton(label,async current=>{try{await fn(t,current);}catch(e){if(target===review)failChecks(e,t,current);else if(current())target.fail(e);}},{class:cls,owner(){
   t=token();const version=reviewVersion,current=()=>valid(t)&&target.dialog.open&&(target!==review||checksActive(t,version));
   if(!current())return;target.error.hidden=true;return current;
  }});
 }
 function clear() {
  generation++;reviewVersion++;settingsVersion++;jobRequest++;selectedJob='';notificationsVersion++;notificationData=null;controller.abort();controller=new AbortController();notificationRequest++;timer?.stop();jobTimer?.stop();jobs.clear();checks.clear();currentJob=null;personal=null;notebook=null;
  for(const view of [notifications,settings,review]){view.dialog.close();view.error.hidden=true;}
  list.replaceChildren();reviewBody.replaceChildren();document.getElementById('knowledge-body').replaceChildren();settingsBody.replaceChildren();badge.hidden=true;badge.textContent='';bell.title='Notifications';
 }
 function member(id){return getProjects().find(p=>p.id===id) || (getProject()?.id===id?getProject():null);}
 function canComment(id){return !!getUser()?.admin || ['comment','edit','owner'].includes(member(id)?.role);}
 function canEdit(id){return !!getUser()?.admin || ['edit','owner'].includes(member(id)?.role);}
 function canManage(id){return !!getUser()?.admin || member(id)?.role==='owner';}
 async function refreshNotifications(more=false) {
  if(!account || account==='@local-admin')return;
  const t=token(),request=++notificationRequest,version=notificationsVersion,previous=notificationData;
  const current=()=>valid(t)&&request===notificationRequest&&version===notificationsVersion;
  const append=more===true&&previous&&Number.isInteger(previous.next);
  try {
   const offset=append?previous.next:0;
   let data=await api('/api/notifications'+(offset?'?offset='+offset+'&limit=50':''),requestOptions());if(!current())return;
   if(append&&data.total!==undefined&&data.total!==previous.total)return refreshNotifications();
   const total=data.total;
   let items=append?[...previous.items,...data.items || []]:data.items || [];
   // Refresh only the range the user already opened; new pages require a click.
   const wanted=notifications.dialog.open?previous?.items.length || 0:0;
   while(!append&&items.length<wanted&&Number.isInteger(data.next)) {
    data=await api('/api/notifications?'+new URLSearchParams({offset:data.next,limit:Math.min(50,wanted-items.length)}),requestOptions());if(!current())return;
    if(data.total!==total)return refreshNotifications();
    items.push(...data.items || []);
   }
   const combined={...data,items:[...new Map(items.map(item=>[item.id,item])).values()]};
   badge.textContent=String(data.unreadCount || 0);badge.hidden=!data.unreadCount;bell.title=data.unreadCount?`${data.unreadCount} unread notifications`:'Notifications';
   if(notifications.dialog.open)await renderNotifications(combined,current);
  }catch(e){
   if(!current()||e.name==='AbortError')return;
   if(notifications.dialog.open){
    if(!notificationData)list.replaceChildren(action('Try again',()=>refreshNotifications(),notifications));
    notifications.fail(e);
   }else if([401,403].includes(e.status))clear();
  }
 }
 function evidenceView(items,projectID,target) {
  const wrap=el('div',undefined,'jev-evidence');
  for(const evidence of items || []) {
   const block=el('figure');const label=el('figcaption',evidence.path || 'Source');const url=evidenceLink(projectID,evidence);
   if(url){const link=el('a','Inspect · '+evidence.revision.slice(0,7));link.href=url;link.target='_blank';link.rel='noopener noreferrer';label.append(document.createTextNode(' · '),link);}
   block.append(label,el('blockquote',evidence.text || 'No excerpt supplied.'));wrap.append(block);
  }
  return wrap;
 }
 async function renderNotifications(data,current) {
  const reviews=new Map();
  for(const id of new Set(data.items.filter(item=>item.findingId).map(item=>item.project))) {
   try{reviews.set(id,await api(notebookPath(id)+'/api/knowledge/review',requestOptions()));}catch(e){if([401,403].includes(e.status)){if(current())notifications.fail(e);return;}}
   if(!current()||!notifications.dialog.open)return;
  }
  if(!current()||!notifications.dialog.open)return;
  notificationData=data;notifications.error.hidden=true;list.replaceChildren();
  if(!data.items.length)list.append(el('p','No notifications.'));
  for(const item of data.items) {
   const card=resultCard(item,item.project,notifications,reviews.get(item.project));card.dataset.notification=item.id;card.classList.toggle('jev-unread',!item.read);
   if(!item.read)card.append(action('Mark read',async(t)=>{await post('/api/notifications/read',{ids:[item.id]},requestOptions());if(valid(t))await refreshNotifications();},notifications));
   list.append(card);
  }
  if(data.items.length)list.append(el('p',`Showing ${data.items.length} of ${data.total ?? data.items.length} notifications`,'small'));
  if(Number.isInteger(data.next))list.append(action('Load more notifications',()=>refreshNotifications(true),notifications));
 }
 function resultCard(item,projectID,target,reviewData) {
  const card=el('article',undefined,'jev-card');card.append(el('h3',item.title || item.claim || 'Review result'));
  if(item.summary)card.append(el('p',item.summary));if(item.status)card.append(el('p',item.status.replaceAll('_',' '),'small'));
  card.append(evidenceView(item.evidence,projectID,target));
  if(item.details?.evidenceTotal>item.details?.evidenceShown)card.append(el('p',`Showing ${item.details.evidenceShown} of ${item.details.evidenceTotal} evidence excerpts · ${item.details.destinationPassages || 0} destination passages checked`,'small'));
  const actions=el('div',undefined,'jev-actions');
  actions.append(action('Open Review',async()=>{if(target!==review)target.dialog.close();await openReview(projectID,item.findingId);},target));
  if(canComment(projectID))actions.append(action('Ask chat to propose',async()=>{target.dialog.close();await prepareChat(projectID,correctionDraft(item));},target));
  const finding=reviewData?.findings?.find(f=>f.comment?.id===item.findingId && (!item.findingPath || f.path===item.findingPath));
  if(finding && !finding.comment.resolved && (canEdit(projectID)||finding.comment.author===getUser()?.id)) {
   for(const [label,decision] of [['Resolve','resolve'],['Dismiss','dismiss'],['Intentional difference','dismiss']]){const button=action(label,async(t,current)=>{
    await post(notebookPath(projectID)+'/api/knowledge/findings/decide',{path:finding.path,id:finding.comment.id,revision:finding.revision,decision},requestOptions());
    if(valid(t)){await refreshNotifications();if(current()&&currentJob)await loadJob(currentJob.id);}
   },target);if(label==='Intentional difference')button.title='Dismiss this finding because the difference is intentional; the original notes stay unchanged.';actions.append(button);}
  }
  if(item.proposalId)actions.append(action('Inspect proposed changes',async()=>{if(target!==review)target.dialog.close();await openReview(projectID,undefined,'proposals');},target));
  card.append(actions);return card;
 }
 async function openSettings() {
  const t=token(),version=++settingsVersion;settingsBody.replaceChildren(el('p','Loading…'));settings.show();
  try{const data=await api('/api/jev/settings',requestOptions());if(!valid(t)||version!==settingsVersion||!settings.dialog.open)return;personal=data;renderSettings();}catch(e){if(valid(t))settings.fail(e);}
 }
 function checkbox(label,checked=false){const row=el('label',undefined,'jev-check');const input=el('input');input.type='checkbox';input.checked=checked;row.append(input,document.createTextNode(label));return {row,input};}
 function renderSettings() {
  settingsBody.replaceChildren(el('p','AI settings are managed in Administration → AI features.','small'));
  if(personal.localOnly)settingsBody.append(el('p','Remote checks are disabled by local-only policy.'));
  else if(!personal.available)settingsBody.append(el('p','Decision provider unavailable.'));
  for(const [key,label] of [['rerank','Search reranking'],['citations','Citation checks']])settingsBody.append(el('p',label+': '+(personal.settings?.consent&&personal.settings?.[key]?'Enabled':'Disabled')));
 }

 async function openChecks() {
  const t=token(),id=getProject()?.id,version=++reviewVersion;if(!id)return;selectedJob='';currentJob=null;jobRequest++;jobTimer?.stop();
  reviewBody.replaceChildren(el('p','Loading…'));review.show();
  try{const values=await Promise.all([api('/api/jev/settings',requestOptions()),api(notebookPath(id)+'/api/jev/settings',requestOptions()),api(notebookPath(id)+'/api/jev/jobs',requestOptions())]);if(!checksActive(t,version))return;[personal,notebook]=values;renderChecks(values[2].jobs || []);}catch(e){failChecks(e,t,()=>checksActive(t,version));}
 }
 function renderChecks(savedJobs) {
  reviewBody.replaceChildren();const id=getProject().id,enabled=personal.available&&!personal.localOnly&&(personal.reviews??personal.settings?.consent);
  if(!enabled){reviewBody.append(el('p',personal.localOnly?'Remote checks are disabled by local-only policy.':'Ask an administrator to enable notebook reviews.'));reviewBody.append(action('AI feature status',()=>{review.dialog.close();return openSettings();}));}
  const consistency=el('section');consistency.append(el('h3','Consistency'),el('p','Checks selected passages in saved Notes.','small'));
  const check=action('Check now',()=>startJob({kind:'consistency'}));check.disabled=!enabled||!canComment(id);consistency.append(check);
  consistency.append(el('p','Automatic checks: '+(notebook.settings?.autoChecks?'Enabled':'Disabled')+' · Managed in Administration.','small'));
  reviewBody.append(consistency);
  const organizer=el('section');organizer.append(el('h3','Organize Notes'));
  organizer.append(action('Organize existing notes',()=>organizationForm(),review,''));reviewBody.append(organizer);
  for(const [kind,title,description] of [['audit','Extraction audit','Compare selected source revisions with destination notes.'],['organization','Organization suggestions','Suggest tags, links, and duplicates for review.']]) {
   const section=el('section');section.append(el('h3',title),el('p',description,'small'));const button=action('Choose '+(kind==='audit'?'sources and destinations':'notes and tags'),()=>selection(kind));button.disabled=!enabled||!canComment(id);section.append(button);reviewBody.append(section);
  }
  const section=el('section');section.append(el('h3','Recent checks'));if(!savedJobs.length)section.append(el('p','No checks yet.','small'));
  for(const job of savedJobs){jobs.set(job.id,job);section.append(action(`${job.kind} · ${job.state} · ${job.examined || 0}/${job.total || 0}`,()=>loadJob(job.id)));}reviewBody.append(section);
  reviewBody.append(action('Open findings and proposed changes',async()=>{await openReview(id);}));
 }
 async function startJob(body) {
  const t=token(),id=getProject()?.id,version=reviewVersion,key=id+'/'+JSON.stringify(body);
  const request=submissions.get(key) || {...body,operationId:crypto.randomUUID()};submissions.set(key,request);
  try {
   const job=await post(notebookPath(id)+'/api/jev/jobs',request,requestOptions());submissions.delete(key);
   if(valid(t)&&version===reviewVersion&&(review.dialog.open && !review.body.hidden))await loadJob(job.id);
  }catch(e){if(e.status && e.status<500 && ![408,429].includes(e.status))submissions.delete(key);throw e;}
 }
 async function organizationForm() {
  const t=token(),id=project,version=++reviewVersion;jobTimer?.stop();selectedJob='';currentJob=null;jobRequest++;
  const current=()=>checksActive(t,version);reviewBody.replaceChildren(el('p','Loading organization settings…'));
  try {
   const data=await api(notebookPath(id)+'/api/jev/organization',requestOptions());if(!current())return;
   const permitted=data.available&&data.consent&&!data.localOnly,policy={tags:true};
   const form=el('div',undefined,'organization-form');reviewBody.replaceChildren(el('h3','Organize existing notes'),form);
   const footer=el('footer',undefined,'organization-footer');
   if(!permitted)form.append(el('p',data.localOnly?'Remote processing is disabled by local-only policy.':!data.available?'The operator has not configured Jev.':'Ask an administrator to enable notebook reviews.','jev-error'));
   if(!data.canManage)form.append(el('p','Only Owners and Admins can apply changes.','small'));
   const field=(label,input)=>{const row=el('label',label);input.setAttribute('aria-label',label);row.append(input);form.append(row);return input;};
   const scope=el('select');for(const [value,label] of [['all','All eligible Notes'],['folder','One folder and its subfolders'],['selected','Selected Notes']]){const option=el('option',label);option.value=value;scope.append(option);}
   const folder=el('select');for(const p of data.folders){const option=el('option',p||'Notebook root');option.value=p;folder.append(option);}
   const selected=new Set(),selection=el('fieldset',undefined,'jev-selection');selection.append(el('legend','Notes to organize'));for(const note of data.notes){const c=checkbox(note.path);c.input.onchange=()=>c.input.checked?selected.add(note.path):selected.delete(note.path);selection.append(c.row);}
   field('Scope',scope);field('Scope folder',folder);form.append(selection);const update=()=>{folder.parentElement.hidden=scope.value!=='folder';selection.hidden=scope.value!=='selected';};scope.onchange=update;update();
   const actions=el('fieldset');actions.append(el('legend','Allowed actions'));const controls={};
   for(const [key,label] of [['tags','Add tags'],['links','Add related-note links'],['moves','Move notes into approved folders'],['duplicates','Move verified duplicates to Trash']]){const c=checkbox(label,!!policy[key]);controls[key]=c.input;actions.append(c.row);}
   form.append(actions);
   const duplicateHelp=el('p','Uncertain matches remain suggestions.','small');controls.duplicates.parentElement.after(duplicateHelp);
   const tags=el('textarea');tags.rows=3;tags.value=(policy.vocabulary||data.tags).join('\n');field('Tag vocabulary',tags);const tagHelp=el('p','One per line; leave blank to use notebook tags.','small');form.append(tagHelp);
   const destinations=el('fieldset',undefined,'jev-selection'),approved=new Set(policy.folders||[]);destinations.append(el('legend','Approved existing destination folders'));for(const p of data.folders){const c=checkbox(p||'Notebook root',approved.has(p));c.input.onchange=()=>c.input.checked?approved.add(p):approved.delete(p);destinations.append(c.row);}form.append(destinations);
   const moveHelp=el('p','Moves stay in this notebook.','small');form.append(moveHelp);
   const showOptions=()=>{tags.parentElement.hidden=tagHelp.hidden=!controls.tags.checked;destinations.hidden=moveHelp.hidden=!controls.moves.checked;duplicateHelp.hidden=!controls.duplicates.checked;};
   for(const control of Object.values(controls))control.onchange=showOptions;showOptions();
   const getPolicy=()=>({tags:controls.tags.checked,links:controls.links.checked,moves:controls.moves.checked,duplicates:controls.duplicates.checked,vocabulary:[...new Set(tags.value.split('\n').map(t=>t.trim()).filter(Boolean))],folders:[...approved]});

    const reconsider=checkbox('Reconsider suppressed work'),advanced=el('details');advanced.append(el('summary','Previous decisions'),reconsider.row,el('p','Includes previously undone or dismissed suggestions.','small'));form.append(advanced);
    const run=async(mode)=>{if(scope.value==='selected'&&!selected.size)throw Error('Select at least one note.');await startJob({kind:'organize',organization:{mode,scope:scope.value,folder:scope.value==='folder'?folder.value:'',paths:scope.value==='selected'?[...selected]:[],policy:getPolicy(),reconsider:reconsider.input.checked}});};
    const buttons=el('div',undefined,'jev-actions'),preview=action('Preview suggestions',()=>run('preview')),apply=action('Apply eligible changes',()=>run('apply'),review,'');preview.disabled=!permitted||!canComment(id);apply.disabled=!permitted||!data.canManage;buttons.append(preview,apply);footer.append(buttons);
   footer.append(action('Back to checks',openChecks));form.append(footer);scope.focus();
  }catch(e){failChecks(e,t,current);}
 }
 async function organizationCommand(job,command,index) {
  const t=token(),version=reviewVersion,response=await post(notebookPath(project)+'/api/jev/jobs/'+encodeURIComponent(job.id)+'/organization/'+command,index===undefined?{}:{index},requestOptions());
  if(!checksActive(t,version)||selectedJob!==job.id)return;
  await loadJob(job.id);if(!checksActive(t,version))return;
  const failures=(response.outcomes||[]).filter(o=>o.error);if(failures.length)review.fail(Error(failures.map(o=>`Action ${o.index+1}: ${o.error}`).join('\n')));
 }
 function organizationCard(item,index,job) {
  const a=item.details||{},card=el('article',undefined,'jev-card');card.append(el('h4',item.title),el('p',item.status),el('p',a.reason||item.summary));
  const link=(label,p,revision)=>{const url=revision?evidenceLink(project,{path:p,revision}):'/?'+new URLSearchParams({project,note:p});if(!url)return;const anchor=el('a',label);anchor.href=url;anchor.target='_blank';anchor.rel='noopener noreferrer';card.append(anchor,document.createTextNode(' '));};
  if(a.request?.target)link((a.kind==='duplicate'?'Retained note: ':'Destination: ')+a.request.target,a.request.target);
  for(const f of a.result?.files||[]){const details=el('details');details.append(el('summary',f.path));for(const [label,value] of [['Before',f.before],['After',f.after]])details.append(el('h5',label),el('pre',value===undefined||value===null?'(absent)':value));card.append(details);if(f.path.endsWith('.md')&&!f.path.startsWith('Trash/')){if(f.after!==null&&f.after!==undefined)link('Open '+f.path,f.path);const revision=f.after===null?a.request?.sources?.find(s=>s.path===f.path)?.revision:a.result.revision;if(revision)link('Saved version',f.path,revision);}}
  if(a.result?.trash&&job.organization.input.mode==='apply'){ const trash=el('a','Inspect Trash entry');trash.href='/?'+new URLSearchParams({project,trash:a.result.trash.id});trash.target='_blank';trash.rel='noopener noreferrer';card.append(trash);}
  if(a.undoRevision)card.append(el('p','Restored in commit '+a.undoRevision.slice(0,7),'small'));
  if(canManage(project)&&!running.has(job.state)){const buttons=el('div',undefined,'jev-actions');if(a.status==='applied')buttons.append(action('Undo action',()=>organizationCommand(job,'undo',index)));else if(!['undone','dismissed','skipped'].includes(a.status))buttons.append(action('Dismiss action',()=>organizationCommand(job,'dismiss',index)));card.append(buttons);}
  return card;
 }
 async function loadJob(id,more=false) {
  if(selectedJob!==id){reviewVersion++;selectedJob=id;currentJob=null;}
  jobTimer?.stop();
  const t=token(),projectID=getProject()?.id,version=reviewVersion,request=++jobRequest,previous=currentJob;
  const current=()=>valid(t)&&version===reviewVersion&&request===jobRequest&&selectedJob===id&&(review.dialog.open && !review.body.hidden);
  const append=more&&previous&&Number.isInteger(previous.resultNext),endpoint=notebookPath(projectID)+'/api/jev/jobs/'+encodeURIComponent(id);
  try {
   let job=await api(endpoint+(append?'?offset='+previous.resultNext+'&limit=50':''),requestOptions());if(!current())return;
   let results=append&&job.resultTotal!==0?[...previous.results,...job.results || []]:job.results || [];
   const wanted=previous?.results.length || 0;
   while(!append&&results.length<wanted&&Number.isInteger(job.resultNext)) {
    job=await api(endpoint+'?'+new URLSearchParams({offset:job.resultNext,limit:Math.min(50,wanted-results.length)}),requestOptions());if(!current())return;
    if(job.resultTotal===0){results=[];break;}
    results.push(...job.results || []);
   }
   renderJob({...job,results:[...new Map(results.map(item=>[item.id,item])).values()]});
  }catch(error){failChecks(error,t,current);}
 }
 function renderJob(job) {
  jobTimer?.stop();currentJob=job;jobs.set(job.id,job);reviewBody.replaceChildren(el('h3',job.kind+' · '+job.state),el('p',`${job.examined || 0} of ${job.total || 0} examined · ${job.remaining || 0} remaining`,'small'));
  if(job.message)reviewBody.append(el('p',job.message));const actions=el('div',undefined,'jev-actions');
  if(job.organization){const input=job.organization.input;reviewBody.append(el('p',`${job.automatic?'Automatic':'Manual '+input.mode} · ${job.owner} · ${input.scope}${input.folder?' / '+input.folder:''} · Allowed: ${['tags','links','moves','duplicates'].filter(k=>input.policy[k]).join(', ')}`,'small'));if(input.reconsider)reviewBody.append(el('p','This manual run explicitly reconsiders suppressed work.','small'));if(canManage(project)&&!running.has(job.state)&&job.results?.some(r=>r.status==='applied'))actions.append(action('Undo run',()=>organizationCommand(job,'undo')));}
  if(running.has(job.state))actions.append(action('Stop check',async(t)=>{const version=reviewVersion;await post(notebookPath(project)+'/api/jev/jobs/'+encodeURIComponent(job.id)+'/cancel',{},requestOptions());if(valid(t)&&version===reviewVersion&&selectedJob===job.id)await loadJob(job.id);}));
  if(resumable.has(job.state))actions.append(action('Continue check',async(t)=>{const version=reviewVersion;await post(notebookPath(project)+'/api/jev/jobs/'+encodeURIComponent(job.id)+'/continue',{},requestOptions());if(valid(t)&&version===reviewVersion&&selectedJob===job.id)await loadJob(job.id);}));
  const terminal=job.state==='cancelled'||job.state==='failed';
  if(terminal)reviewBody.append(el('p',`This check was ${job.state==='cancelled'?'cancelled':'not completed'}. Start a new review to check the notebook again.`,'small'));
  actions.append(action('Refresh',()=>loadJob(job.id)),action(terminal?'Start new review':'All checks',openChecks));reviewBody.append(actions);
  for(const [index,result] of (job.results || []).entries())reviewBody.append(job.organization?organizationCard(result,index,job):resultCard(result,project,review));
  reviewBody.append(el('p',`Showing ${job.results?.length || 0} of ${job.resultTotal ?? job.results?.length ?? 0} results`,'small'));
  if(Number.isInteger(job.resultNext))reviewBody.append(action('Load more results',()=>loadJob(job.id,true)));
  if(!job.results?.length)reviewBody.append(el('p',running.has(job.state)?'Checking selected evidence…':'No findings.','small'));
  if(running.has(job.state)){const t=token(),version=reviewVersion;jobTimer=poll(()=>{loadJob(job.id);return false;},{every:2000,active:()=>valid(t)&&version===reviewVersion&&selectedJob===job.id&&review.dialog.open && !review.body.hidden});}
 }
 async function selection(kind) {
  const t=token(),id=project,version=++reviewVersion;const selected=()=>valid(t)&&version===reviewVersion&&(review.dialog.open && !review.body.hidden);jobTimer?.stop();currentJob=null;selectedJob='';jobRequest++;reviewBody.replaceChildren(el('h3',kind==='audit'?'Select audit evidence':'Select notes and vocabulary'));
  try {
  const sources=new Map(),destinations=new Set();const chosen=el('div',undefined,'jev-selection'),destinationList=el('div',undefined,'jev-selection');
  const picker=el('section'),collection=el('select');collection.setAttribute('aria-label','Source collection');for(const [value,label] of [['notes','Current Notes'],['archives','Archives (explicit selection)']]){const option=el('option',label);option.value=value;collection.append(option);}
  const entries=el('div',undefined,'jev-selection');let browseRequest=0;
  const renderChosen=()=>{chosen.replaceChildren();for(const [key,source] of sources)chosen.append(action(`${source.path} · ${source.revision.slice(0,7)} · Remove`,()=>{sources.delete(key);renderChosen();}));};
  const addSource=async(path,revision)=>{let actual=revision;if(!actual){const doc=await api(notebookPath(id)+'/api/note?path='+encodeURIComponent(path),requestOptions());if(!selected())return;actual=doc.revision;}sources.set(path+'@'+actual,{path,revision:actual});renderChosen();};
  const browse=async(folder='')=>{
   const request=++browseRequest;entries.replaceChildren(el('p','Loading…'));const data=await api(notebookPath(id)+'/api/browse?'+new URLSearchParams({collection:collection.value,path:folder}),requestOptions());if(!selected()||request!==browseRequest||!(review.dialog.open && !review.body.hidden))return;
   entries.replaceChildren();if(folder){const up=action('Parent folder',()=>browse(folder.split('/').slice(0,-1).join('/')));up.prepend(icon('back'));entries.append(up);}
   for(const item of data.items || []){
    if(item.kind==='folder'){entries.append(action('Folder · '+item.path,()=>browse(item.path)));continue;}
    if(item.kind!=='note')continue;const row=el('div',undefined,'jev-picker-row');row.append(action('Select · '+item.path,()=>addSource(item.path,data.revision)));
    if(kind==='audit')row.append(action('Previous versions · '+item.path,async()=>{const history=await api(notebookPath(id)+'/api/history?path='+encodeURIComponent(item.path),requestOptions());if(!selected()||request!==browseRequest)return;const versions=el('div');for(const version of history)versions.append(action(`${version.revision.slice(0,7)} · ${version.message || 'Saved version'}`,()=>addSource(item.path,version.revision)));row.append(versions);}));entries.append(row);
   }
  };
  collection.onchange=()=>browse().catch(e=>failChecks(e,t,selected));picker.append(el('h4','Source notes'),collection,entries,chosen);reviewBody.append(picker);
  let tags;
  if(kind==='audit') {
   reviewBody.append(el('h4','Destination Notes'),destinationList);
   const notes=await api(notebookPath(id)+'/api/notes?collection=notes',requestOptions());if(!selected())return;
   for(const note of notes){if(note.kind==='folder'||!note.path?.endsWith('.md'))continue;const control=checkbox(note.path);control.input.onchange=()=>control.input.checked?destinations.add(note.path):destinations.delete(note.path);destinationList.append(control.row);}
  }else{
   const label=el('label','Allowed tags (one per line)');tags=el('textarea');tags.rows=4;tags.setAttribute('aria-label','Allowed tags');label.append(tags);reviewBody.append(label);
   const notes=await api(notebookPath(id)+'/api/notes?collection=notes',requestOptions());if(!selected())return;tags.value=[...new Set(notes.flatMap(note=>note.metadata?.tags || []))].sort().join('\n');
  }
  reviewBody.append(action('Start '+(kind==='audit'?'audit':'organization review'),async()=>{if(!sources.size)throw Error('Select at least one source note.');if(kind==='audit'&&!destinations.size)throw Error('Select at least one destination Note.');const body={kind,sources:[...sources.values()]};if(kind==='audit')body.destinations=[...destinations];else body.tags=[...new Set(tags.value.split('\n').map(tag=>tag.trim()).filter(Boolean))];await startJob(body);},review,''),action('Back to checks',openChecks));
  await browse();
  }catch(e){failChecks(e,t,selected);}
 }
 function mountCitationChecks(host,scope) {
  const t=token();if(scope.project!==project || !scope.valid())return;
  const key=scope.project+'/'+scope.chat+'/'+scope.run;const endpoint=notebookPath(scope.project)+'/api/chat/'+encodeURIComponent(scope.chat)+'/runs/'+encodeURIComponent(scope.run)+'/citations';
  const current=()=>valid(t)&&scope.valid()&&host.isConnected;
  async function load(recheck=false) {
   try{const result=await (recheck?post(endpoint,{},requestOptions()):api(endpoint,requestOptions()));if(!current())return;
    if(result.state==='disabled' && !personal){const prefs=await api('/api/jev/settings',requestOptions());if(!current())return;personal=prefs;}
    checks.set(key,result);render(result);
    if(['pending','running','queued'].includes(result.state))poll(()=>{load();return false;},{every:1500,active:current});
   }catch(e){if(!current()||e.name==='AbortError')return;if([401,403].includes(e.status)){clear();return;}if(e.status===404)return;host.replaceChildren(el('span','Citation checks unavailable','small'));}
  }
  function render(result) {
   host.replaceChildren();if(result.state==='disabled' && (!personal?.available || personal.localOnly || !personal.settings?.consent || !personal.settings?.citations))return;const details=el('details',undefined,'jev-citations');details.append(el('summary',result.state==='disabled'?'Citation checks not run':citationSummary(result)));
   if(result.message)details.append(el('p',result.message,'small'));
   for(const item of result.items || []) {
    const section=el('section');
    section.append(el('strong',item.status?.replaceAll('_',' ') || 'Unchecked'),el('p',item.claim),evidenceView(item.evidence,scope.project,review));
    if(item.evidence?.length) {
     const correction=el('button','Ask chat to propose','quiet');correction.type='button';
     correction.onclick=()=>{if(current())prepareChat(scope.project,correctionDraft(item)).catch(e=>{if(current())host.append(el('p',e.message,'small'));});};
     section.append(correction);
    }
    details.append(section);
   }
   const recheck=el('button',result.state==='disabled'?'Check citations':'Recheck citations','quiet');recheck.type='button';recheck.disabled=['pending','running','queued'].includes(result.state);recheck.onclick=()=>{if(current()){recheck.disabled=true;load(true);}};details.append(recheck);host.append(details);
  }
  if(checks.has(key))render(checks.get(key));load();
 }
 function projectChanged() {
  const user=getUser()?.id || '',next=getProject()?.id || '';if(user!==account)submissions.clear();clear();account=user;project=next;bell.hidden=!user||user==='@local-admin';
  document.getElementById('jev-review-open').disabled=!next;
  refreshNotifications();const t=token();timer?.stop();timer=poll(()=>refreshNotifications(),{every:30000,active:()=>valid(t)});
 }
 bell.onclick=()=>{notificationsVersion++;notificationData=null;notifications.show();list.replaceChildren(el('p','Loading…'));refreshNotifications();};
 notifications.dialog.addEventListener('close',()=>{notificationsVersion++;notificationRequest++;bell.focus();});review.dialog.addEventListener('close',()=>{reviewVersion++;jobTimer?.stop();});settings.dialog.addEventListener('close',()=>{settingsVersion++;});
 document.getElementById('jev-settings-open').onclick=openSettings;document.getElementById('jev-review-open').onclick=openChecks;
 return {leaveChecks(){reviewVersion++;jobTimer?.stop();},projectChanged,openSettings,openChecks,mountCitationChecks,refreshNotifications};
}
