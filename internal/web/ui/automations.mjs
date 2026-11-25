import {confirmAction,feedback} from './feedback.mjs';
import {element as node,busy} from './dom.mjs';
import {requestSequence} from './state.mjs';

const kinds = {native:'Instance maintenance', checks:'Independent checks', digest:'Data digest', monitor:'Condition monitor', recipe:'Check and repair a recipe', learning:'Improve data-access profiles', task:'Maintain a note with AI', 'drive-sync':'Import Google Drive', script:'Repository package'};
const nativeTasks = ['notebook-health','storage-health','automation-health','profile-health','profile-compaction','activity-summary','repository-refresh','email-pairing','task-reminder'];
// These native tasks act through a messaging client and publish no output.
const clientTasks = ['email-pairing','task-reminder'];
const usesAI = draft => !!draft.repair || !!draft.aiConnection || ['task','learning'].includes(draft.kind) || (draft.script?.capabilities||[]).some(c=>['ai.structured', 'ai.decisions','decision.evaluate'].includes(c));
const checkStates = {healthy:'Healthy',investigating:'Investigating failures',repaired:'Automatically repaired',rolled_back:'Repair reverted',repair_validated:'Repair validated · preview',waiting_upstream:'Waiting on upstream data',upstream_unavailable:'Source unavailable',repair_failed:'Repair could not be validated',failed:'Checks failed'};
const learningStates = {published:'Profiles published',deferred:'Deferred',unchanged:'No changes',previewed:'Preview finished',failed:'Learning failed'};

export function initAutomations({manage, report, people}) {
  const root = document.getElementById('integration-jobs');
  let pageActive=()=>false;
  const views=requestSequence();
  const button = (label, action) => {
    const active=pageActive,version=views.current(),owns=()=>active()&&views.owns(version);
    const b = node('button', label); b.type = 'button'; b.className = 'quiet';
    b.onclick = () => {if(owns())return busy(b, action, {report, current: owns});};
    return b;
  };
  const download = spec => {
    const value = {...spec}; delete value.generation;
    const url = URL.createObjectURL(new Blob([JSON.stringify(value,null,2)+'\n'], {type:'application/json'}));
    const a = node('a'); a.href = url; a.download = (spec.id || 'automation')+'.json'; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  async function load(active=pageActive) {
    pageActive=active;const view=views.begin(active);
    const reload=async()=>{if(view())await load(active);};
    let data;try{data=await manage('automation_status');}catch(error){if(!view())return;readFailure(error,()=>load(active));throw error;}
    if(!view())return;
    const controls = node('div'); controls.className = 'dialog-actions';
    controls.append(button('New automation', () => edit()), button('Import JSON', () => edit(null,true)), button('Repository packages', () => packages()), button('Refresh runs', reload));
    root.replaceChildren(controls);
    if (!data.jobs.length) root.append(node('p','No automations configured.'));
    for (const job of data.jobs) {
      const card = node('section'); card.className = 'automation-card'; card.dataset.automation = job.id;
      const trigger = job.spec.trigger?.kind || (job.spec.schedule?'Calendar':job.intervalSeconds?`Every ${job.intervalSeconds}s`:job.kind==='learning'?'On usage':'Manual');
      card.append(node('h4',job.spec.name || job.id),node('p',`${kinds[job.kind] || job.kind} · ${job.project} · ${job.spec.disabled?'Paused':trigger}${job.managedBy==='file'?' · Managed by file':''}`));
      if(job.spec.script){const p=job.spec.script,update=node('p');update.hidden=true;card.append(node('p',`${p.sourceProject} / ${p.packagePath} · ${p.revision.slice(0,12)} · ${p.followPrimaryBranch?'Follows primary branch':'Pinned'}${p.sandbox===false?' · Not sandboxed':' · Sandboxed'}`),update);card.append(button('Inspect / update package',()=>packages(job)));
        // A newer source digest is reported, never adopted, unless this installation opted in.
        manage('discover_automation_packages',{project:p.sourceProject,path:p.packagePath}).then(([latest])=>{if(view()&&latest&&latest.digest!==p.digest){update.textContent=`Update available: ${latest.revision.slice(0,12)} · review it with Inspect / update package`;update.hidden=false;}},()=>{});}
      if (job.nextRun) card.append(node('p','Next: '+new Date(job.nextRun).toLocaleString()));
      const latest = data.runs.find(run => run.job===job.id && !run.dryRun && !['queued','running'].includes(run.state) && (run.checks || run.learning));
      if (latest?.checks) card.append(node('p',`${checkStates[latest.checks.state] || latest.checks.state} · ${latest.checks.passed} passed · ${latest.checks.failed} unresolved`));
      if (latest?.learning) card.append(node('p',latest.error || latest.learning.summary));
      const actions = node('div'); actions.className = 'dialog-actions';
      actions.append(button(job.managedBy==='file'?'View definition':'Edit', () => edit(job)),button('Duplicate', () => edit({...job,id:'',revision:'',managedBy:'instance',spec:{...job.spec,id:'',generation:undefined,disabled:true}})),button('Export JSON', () => download(job.spec)));
      if (job.kind!=='learning' && !job.spec.trigger) {
        actions.append(button('Test', async () => {await manage('test_automation',{id:job.id,requestId:crypto.randomUUID()});await reload();}));
        if (!job.spec.disabled) actions.append(button('Run now',async () => {await manage('run_automation',{id:job.id,requestId:crypto.randomUUID()});await reload();}));
      }
      if (job.managedBy==='instance') actions.append(button(job.spec.disabled?'Enable':'Pause',async () => {await manage('save_automation',{spec:{...job.spec,disabled:!job.spec.disabled},revision:job.revision});await reload();}),button('Delete',async () => {if (await confirmAction(`Delete automation “${job.spec.name || job.id}”? Run history will remain.`)){await manage('delete_automation',{id:job.id,revision:job.revision});await reload();}}));
      card.append(actions); root.append(card);
    }
    const runs = node('div'); runs.append(node('h4','Recent runs'));
    for (const run of data.runs) {
      const row = node('div'); row.className = 'activity-row';
      row.append(node('time',new Date(run.updated).toLocaleString()),node('strong',run.job+' · '+(checkStates[run.checks?.state] || learningStates[run.learning?.state] || run.state)),node('span',run.error || run.checks?.summary || run.learning?.summary || `${run.receipts} receipts · ${run.inputTokens+run.outputTokens} tokens`),button('Details', () => details(run.id)));
      if (run.state==='action-required') row.append(button('Reconciled · cancel run',async () => {if(await confirmAction('Confirm you have reconciled this retained submission with the upstream service. Cancelling permits new runs of this job.')){await manage('cancel_automation',{id:run.id});await reload();}}));
      if (['queued','running'].includes(run.state)) row.append(button('Cancel',async () => {await manage('cancel_automation',{id:run.id});await reload();}));
      runs.append(row);
    }
    if (!data.runs.length) runs.append(node('p','No runs yet.','small'));
    root.append(runs);
  }
  function readFailure(error,retry){const message=node('p');feedback(message,error.message);root.replaceChildren(message,button('Retry loading automations',retry));}
  async function packages(job=null) {
    const view=views.begin(pageActive);
    const section=node('section');section.className='automation-card';section.append(node('h4','Repository packages'));
    const project=node('input');project.setAttribute('aria-label','Package notebook');project.placeholder='Repository notebook ID';project.value=job?.spec.script?.sourceProject||'';
    const revision=node('input');revision.setAttribute('aria-label','Source commit');revision.placeholder='Latest available commit (or exact commit for rollback)';
    const found=node('div');
    section.append(project,revision,button('Discover',async()=>{
      const entries=await manage('discover_automation_packages',{project:project.value,revision:revision.value,path:job?.spec.script?.packagePath||''});if(!view())return;found.replaceChildren();
      if(!entries.length)found.append(node('p','No valid packages found.'));
      for(const p of entries){const card=node('section');card.className='automation-card';card.append(node('h4',p.manifest.name),node('p',`Available: ${p.revision}`));if(job)card.append(node('p',`Active: ${job.spec.script.revision}`),node('p',p.digest===job.spec.script.digest?'The installed package is current.':'Update available. Review the manifest and source changes, then prepare a paused update.'));card.append(node('pre',JSON.stringify(p.manifest,null,2)));
        card.append(button(job?'Prepare paused update':'Prepare paused installation',async()=>{
          const status=await manage('automation_status');if(!view())return; if(!status.pythonConfigured)throw Error('Configure AUTOMATION_PYTHON before installing packages.');
          const spec=job?structuredClone(job.spec):{id:p.manifest.id,name:p.manifest.name,kind:'script',actor:'',project:'',disabled:true,timeoutSeconds:180,connections:[],operations:{},readProjects:[p.project],externalProcessing:false};
          const prior=spec.script||{};spec.disabled=true;spec.script={sourceProject:p.project,sourceID:p.source,packagePath:p.path,revision:p.revision,digest:p.digest,runtime:p.manifest.runtime,entrypoint:p.manifest.entrypoint,environment:'',retentionHours:prior.retentionHours||0,...(prior.followPrimaryBranch?{followPrimaryBranch:true}:{}),...(prior.sandbox===false?{sandbox:false}:{}),capabilities:[...p.manifest.capabilities,...(prior.capabilities||[]).filter(c=>(p.manifest.optionalCapabilities||[]).includes(c))],connections:prior.connections||Object.fromEntries(Object.keys(p.manifest.connections||{}).map(k=>[k,''])),notebooks:prior.notebooks||Object.fromEntries(Object.keys(p.manifest.notebooks||{}).map(k=>[k,''])),parameters:prior.parameters||{}};
          edit({id:job?.id||'',revision:job?.revision||'',managedBy:job?.managedBy||'instance',spec},true);
        }));found.append(card);
      }
    }),button('Back',load),found);root.replaceChildren(section);
  }
  async function details(id) {
    const active=pageActive,view=views.begin(active);let run;try{run=await manage('automation_run',{id});}catch(error){if(!view())return;readFailure(error,()=>load(active));throw error;}if(!view())return; const box = node('section'); box.className = 'automation-card';
    box.append(node('h4',run.spec.name || run.spec.id),node('pre',JSON.stringify(run,null,2)),button('Back',load));
    if (run.spec.kind==='learning' || run.spec.trigger) box.append(button('Test with this evidence',async () => {await manage('test_automation',{id:run.spec.id,requestId:crypto.randomUUID(),evidenceRun:id});if(view())await load();}));
    root.replaceChildren(box);
  }
  function edit(job=null, importing=false) {
    const view=views.begin(pageActive);
    let draft = structuredClone(job?.spec || {id:'',name:'',kind:'native',actor:'',project:'',disabled:true,native:{task:'notebook-health'},timeoutSeconds:180});
    let jsonDirty = false;
    const readOnly = job?.managedBy==='file';
    const form = node('form'); form.className = 'automation-editor';
    const fields = node('div'); fields.className = 'automation-fields'; const inputs = {}, labels = {};
    function field(key,label,type='text',values) {
      const wrap = node('label',label), input = values?node('select'):node(type==='textarea'?'textarea':'input');
      input.dataset.field = key; input.setAttribute("aria-label",label);
      if (values) for (const [value,name] of values) input.append(new Option(name,value)); else if (type==='textarea') input.rows=4; else input.type=type;
      inputs[key]=input; labels[key]=wrap; wrap.append(input); fields.append(wrap); return input;
    }
    field('id','ID').required=true; field('name','Name'); field('kind','Task','text',Object.entries(kinds));
    field('actor','Service account','text',[['','Select an account'],...people().users.filter(u=>u.kind==='service'&&!u.disabled).map(u=>[u.id,u.displayName||u.id])]).required=true;
    field('project','Notebook ID').required=true;
    field('nativeTask','Maintenance task','text',nativeTasks.map(v=>[v,v.replaceAll('-',' ')]));
    field('triggerMode','Trigger','text',[['manual','Manual'],['interval','Interval'],['calendar','Calendar'],['once','Once'],['usage','MCP usage'],['event','Source event']]);
    field('intervalSeconds','Interval (seconds)','number');
    field('timezone','Timezone'); field('hours','Hours (0–23, comma separated)'); field('minutes','Minutes (0–59, comma separated)'); field('weekdays','Weekdays (0 Sunday–6 Saturday)'); field('once','Run at (RFC3339)');
    field('output','Output note'); field('recipe','Recipe note'); field('aiConnection','AI connection (blank uses instance default)');
    field('connections','Connections (comma separated)'); field('readProjects','Input notebooks (comma separated)');
    field('timeoutSeconds','Maximum runtime (seconds)','number'); field('maxTokens','Maximum tokens (0 uses provider limits)','number'); field('instruction','Instructions','textarea');
    field('repairCooldownSeconds','Repair cooldown (seconds)','number');
    const checks = node('div'); checks.className='automation-checks';
    for (const [key,label] of [['enabled','Enabled'],['externalProcessing','Allow configured AI processing'],['repairEnabled','Automatically diagnose and repair checks'],['followPrimaryBranch','Adopt compatible primary-branch package changes without review'],['sandbox','Sandbox package code: no instance files or network (requires Linux Landlock; unchecked runs as server code)']]) {const wrap=node('label');labels[key]=wrap;inputs[key]=node('input');inputs[key].type='checkbox';wrap.append(inputs[key],document.createTextNode(label));checks.append(wrap);}
    const advanced=node('details');advanced.open=importing;advanced.append(node('summary','JSON definition'));
    const json=node('textarea');json.rows=18;json.spellcheck=false;json.setAttribute('aria-label','Automation JSON');advanced.append(json);
    const status=node('p');status.setAttribute('role','status');
    const split = value => value.split(',').map(v=>v.trim()).filter(Boolean);
    function visibility() {
      const mode=inputs.triggerMode.value;
      for(const key of ['timezone','hours','minutes','weekdays']) labels[key].hidden=mode!=='calendar';
      labels.once.hidden=mode!=='once'; labels.intervalSeconds.hidden=mode!=='interval'; labels.nativeTask.hidden=draft.kind!=='native';
      labels.output.hidden=!!draft.repair||!['native','checks','digest','task'].includes(draft.kind)||(draft.kind==='native'&&(draft.native?.task.endsWith('-health')||clientTasks.includes(draft.native?.task))); labels.recipe.hidden=draft.kind!=='recipe'&&!draft.repair;
      labels.repairEnabled.hidden=draft.kind!=='checks'; labels.repairCooldownSeconds.hidden=!draft.repair; labels.followPrimaryBranch.hidden=!draft.script; labels.sandbox.hidden=!draft.script;
      labels.instruction.hidden=!usesAI(draft); labels.maxTokens.hidden=!usesAI(draft);
      inputs.triggerMode.disabled=readOnly||jsonDirty||draft.kind==='learning';
      inputs.triggerMode.querySelector('option[value="usage"]').disabled=draft.kind!=='learning';
    }
    function draw(triggerMode) {
      triggerMode??=draft.kind==='learning'?'usage':draft.trigger?'event':draft.schedule?.once?'once':draft.schedule?'calendar':draft.intervalSeconds?'interval':'manual';
      for (const [key,input] of Object.entries(inputs)) {
        if(key==='enabled') input.checked=!draft.disabled;
        else if(key==='repairEnabled') input.checked=!!draft.repair;
        else if(key==='followPrimaryBranch') input.checked=!!draft.script?.followPrimaryBranch;
        else if(key==='sandbox') input.checked=draft.script?.sandbox!==false;
        else if(key==='repairCooldownSeconds') input.value=draft.repair?.cooldownSeconds??3600;
        else if(key==='externalProcessing') input.checked=!!draft[key];
        else if(['connections','readProjects'].includes(key)) input.value=(draft[key]||[]).join(', ');
        else if(['timezone','hours','minutes','weekdays','once'].includes(key)) {const value=draft.schedule?.[key];input.value=Array.isArray(value)?value.join(', '):value||'';}
        else if(key==='triggerMode') input.value=triggerMode;
        else if(key==='nativeTask') input.value=draft.native?.task||'notebook-health';
        else input.value=draft[key]??'';
        input.disabled=readOnly;
      }
      inputs.id.disabled=readOnly||!!job?.id;json.value=JSON.stringify(draft,null,2);json.readOnly=readOnly;jsonDirty=false;visibility();
    }
    function capture() {
      if(jsonDirty) throw Error('Apply JSON changes first.');
      const previousKind=draft.kind;
      for(const [key,input] of Object.entries(inputs)) {
        if(key==='enabled') draft.disabled=!input.checked;
        else if(key==='repairEnabled'||key==='repairCooldownSeconds') continue;
        else if(key==='followPrimaryBranch') {if(draft.script){if(input.checked)draft.script.followPrimaryBranch=true;else delete draft.script.followPrimaryBranch;}}
        else if(key==='sandbox') {if(draft.script){if(input.checked)delete draft.script.sandbox;else draft.script.sandbox=false;}}
        else if(key==='externalProcessing') draft[key]=input.checked;
        else if(['connections','readProjects'].includes(key)) draft[key]=split(input.value);
        else if(!['triggerMode','nativeTask','timezone','hours','minutes','weekdays','once'].includes(key)) draft[key]=input.type==='number'?Number(input.value):input.value;
      }
      if(previousKind!==draft.kind) {for(const key of ['repair','native','drive','shadow','script','learning','condition','checks','calls','trigger','alert','retry'])delete draft[key];draft.aiConnection='';draft.recipe='';draft.output='';draft.instruction='';draft.externalProcessing=false;}
      if(draft.kind==='checks') {
        if(inputs.repairEnabled.checked) {draft.repair={...draft.repair,cooldownSeconds:Number(inputs.repairCooldownSeconds.value)||3600};delete draft.output;delete draft.retry;}
        else if(draft.repair) {delete draft.repair;draft.aiConnection='';}
      }
      if(draft.kind==='native') {draft.native={...draft.native,task:inputs.nativeTask.value};if(draft.native.task.endsWith('-health'))delete draft.output;if(clientTasks.includes(draft.native.task)){draft.native.client||='slack';delete draft.output;}else delete draft.native.client;}
      if(draft.kind==='learning') {draft.learning??={captureInputs:true,folder:'learned',debounceSeconds:120,maxWaitSeconds:900,minExamples:2,retentionHours:24};inputs.triggerMode.value='usage';}
      else if(inputs.triggerMode.value==='usage') inputs.triggerMode.value='manual';
      const mode=inputs.triggerMode.value;
      delete draft.schedule;
      if(mode==='calendar') draft.schedule={timezone:inputs.timezone.value||Intl.DateTimeFormat().resolvedOptions().timeZone,hours:split(inputs.hours.value).map(Number),minutes:split(inputs.minutes.value||'0').map(Number),weekdays:split(inputs.weekdays.value).map(Number),catchUpSeconds:job?.spec.schedule?.catchUpSeconds??3600};
      if(mode==='once') draft.schedule={once:inputs.once.value,catchUpSeconds:3600};
      if(mode!=='interval') delete draft.intervalSeconds;
      if(mode==='event') draft.trigger??={}; else delete draft.trigger;
      if(['checks','digest','monitor','drive-sync','script','recipe'].includes(draft.kind)||mode==='event') advanced.open=true;
      // An incomplete date or interval must not change the trigger the user selected.
      draw(mode);
    }
    function applyJSON() {
      const parsed=JSON.parse(json.value);if(!parsed||Array.isArray(parsed)||typeof parsed!=='object')throw Error('Enter one job definition.');
      if(job?.id&&parsed.id!==job.id)throw Error('Duplicate the job to change its ID.');draft=parsed;draw();
    }
    fields.onchange=()=>{try{capture();}catch(e){report(e);}};checks.onchange=fields.onchange;
    json.oninput=()=>{jsonDirty=true;for(const input of Object.values(inputs))input.disabled=true;};
    if(!readOnly)advanced.append(button('Apply JSON',applyJSON));
    const actions=node('div');actions.className='dialog-actions';actions.append(button('Back',load),button('Validate',async()=>{if(jsonDirty)applyJSON();else capture();await manage('validate_automation',{spec:draft});status.textContent='Definition is valid.';}));
    if(!readOnly) {
      const save=node('button','Save automation','primary');save.type='submit';actions.append(save);
      form.onsubmit=e=>{e.preventDefault();if(!view())return;return busy(form,async()=>{if(jsonDirty)applyJSON();else capture();await manage(draft.kind==='script'&&draft.disabled?'install_automation_package':'save_automation',{spec:draft,revision:job?.revision||''});if(view())await load();},{controls:save,report,current:view});};
    }
    form.append(node('h4',readOnly?'Mounted automation':job?.id?'Edit automation':'New automation'),fields,checks,advanced,status,actions);root.replaceChildren(form);draw();
  }
  return {load};
}
