import {element as node,button} from './dom.mjs';
import {argumentFields} from './operation-arguments.mjs';
import {checkList,choiceSelect,chooseOption} from './pickers.mjs';

// The parts of an automation definition the basic editor fields do not cover:
// data calls, independent checks, a monitor's condition, incident alerts, Slack
// notices, recipe usage rechecks, Drive imports, repository package bindings and
// source events. Each part edits only its own keys of the draft, and a control
// left as it was rendered gives back the saved value unchanged, so saving an
// unchanged form keeps the definition the JSON had. The choices offer only what
// this server lists (connections, read-only operations, notebooks, configured
// Slack channels); validate_automation and save_automation check everything
// again, whichever way the definition was built.

const isObject=value=>!!value&&typeof value==='object'&&!Array.isArray(value);
const connectionText=info=>info.label&&info.label!==info.id?`${info.label} (${info.id})`:info.id;
const timeFormats=[['rfc3339','Date and time (RFC 3339)'],['date','Date (YYYYMMDD)'],['unix','Unix seconds'],['unixnano','Unix nanoseconds']];
const comparisons=[['equals','Equals'],['greaterThan','Greater than'],['lessThan','Less than']];
const eventKinds=[['repository.updated','Notebook changes'],['slack.knowledge','Slack knowledge channel']];
let lists=0;

function labelled(text,control){const wrap=node('label',text);control.setAttribute('aria-label',text);wrap.append(control);return wrap;}
function textInput(text,value,placeholder=''){const input=node('input');input.type='text';input.autocomplete='off';input.spellcheck=false;input.value=value??'';input.placeholder=placeholder;return {input,wrap:labelled(text,input)};}
function numberInput(text,value,placeholder=''){const input=node('input');input.type='number';input.step='1';input.inputMode='numeric';input.value=value??'';input.placeholder=placeholder;return {input,wrap:labelled(text,input)};}
function checkbox(text,checked){const input=node('input'),wrap=node('label',undefined,'check-row');input.type='checkbox';input.checked=!!checked;wrap.append(input,node('span',text));return {input,wrap};}
function part(legend,hint){const box=node('fieldset',undefined,'automation-part');box.append(node('legend',legend));if(hint)box.append(node('p',hint,'small'));return box;}
const put=(target,key,value)=>{if(value===undefined)delete target[key];else target[key]=value;return target;};
// A text control's value: the saved one while unchanged, else trimmed, and
// undefined when blank.
function typed(input,saved){const initial=saved??'';if(input.value===initial)return saved===''?undefined:saved;return input.value.trim()||undefined;}
// A whole number, or undefined when blank and allowed to be. The server checks
// the same ranges; this names the field to fix.
function whole(input,text,{min,max,blank=true}={}){
  if(input.validity?.badInput)throw Error(`${text} must be a whole number.`);
  if(input.value.trim()===''){if(blank)return undefined;throw Error(`${text} is required.`);}
  const n=Number(input.value);
  if(!Number.isInteger(n))throw Error(`${text} must be a whole number.`);
  if(min!==undefined&&n<min||max!==undefined&&n>max)throw Error(max===undefined?`${text} must be at least ${min}.`:`${text} must be from ${min} to ${max}.`);
  return n;
}
// A JSON value typed as text; an unchanged field gives back the saved value.
function jsonInput(text,present,value,placeholder='3, "ok", true or {"rows":1}'){
  const {input,wrap}=textInput(text,present?JSON.stringify(value):'',placeholder),initial=input.value;
  function read(){
    if(input.value===initial)return present?{value}:null;
    if(!input.value.trim())return null;
    try{return {value:JSON.parse(input.value)};}catch{throw Error(`${text} must be a JSON value, such as 3, "ok" or true.`);}
  }
  return {input,wrap,read};
}

// Rows of one kind, each with a Remove button. `make(value)` returns
// {element, read(strict), name(index, count)}; `name` renumbers the row's
// legend or labels after rows are added or removed.
function repeat(noun,values,make,{min=0,max=32,changed=()=>{}}={}){
  const element=node('div',undefined,'automation-repeat'),list=node('div',undefined,'automation-rows'),items=[];
  const add=button(`Add ${noun.toLowerCase()}`,()=>{push();changed();},{class:'quiet'});
  function number(){
    items.forEach((item,index)=>{item.name(index,items.length);item.remove.hidden=items.length<=min;item.remove.setAttribute('aria-label',`Remove ${noun.toLowerCase()}${max===1?'':' '+(index+1)}`);});
    add.hidden=items.length>=max;
  }
  function push(value){
    const item=make(value);
    item.remove=button('Remove',()=>{items.splice(items.indexOf(item),1);item.element.remove();number();changed();},{class:'quiet'});
    item.element.append(item.remove);items.push(item);list.append(item.element);number();
  }
  for(const value of values)push(value);
  while(items.length<min)push();
  element.append(list,add);
  return {element,items,read:strict=>items.map(item=>item.read(strict))};
}
const legendName=(box,noun,max)=>(index,count)=>{box.querySelector('legend').textContent=max===1&&count===1?noun:`${noun} ${index+1}`;};

// A configured ID chosen from a list the server returns later (for example a
// Slack channel named in the chat-client policy). Until the list arrives, or
// when it is empty, the ID is typed.
function laterChoice(text,saved,options,{placeholder='None',missing=value=>`${value} (not configured)`}={}){
  const wrap=node('label',text);let control=node('input');control.type='text';control.autocomplete='off';control.spellcheck=false;control.value=saved??'';control.setAttribute('aria-label',text);wrap.append(control);
  options.then(found=>{
    if(!found.length||!wrap.isConnected)return;
    const value=control.value.trim(),select=choiceSelect(text,found,{placeholder});chooseOption(select,value,missing);
    control.replaceWith(select);control=select;
  },()=>{});
  return {wrap,read:()=>typed(control,saved)};
}

// One data call: a connection, one of its read-only operations, argument fields
// from the operation's input schema, and time arguments filled in at run time.
function callEditor(ctx,saved,legend,where,max){
  const has=isObject(saved);saved=has?saved:{};
  const box=part(legend),connection=choiceSelect('Connection',ctx.connections.map(info=>[info.id,connectionText(info)]),{placeholder:'Choose a connection'});
  chooseOption(connection,saved.connection||'',value=>`${value} (not listed)`);
  const operation=node('select'),about=node('small'),args=node('div'),status=node('p','','small'),suggest=node('datalist');
  suggest.id='automation-time-names-'+(++lists);
  const grid=node('div',undefined,'automation-grid');grid.append(labelled('Connection',connection),labelled('Operation',operation));
  let found=[],fields=null,loading=false;
  const sameCall=()=>has&&connection.value===saved.connection&&(loading?true:operation.value===saved.operation);
  const times=repeat('Time argument',Object.entries(saved.time||{}),([name,binding]=[])=>{
    const row=part('Time argument');
    const argument=textInput('Argument',name),format=choiceSelect('Format',timeFormats);chooseOption(format,binding?.format||'rfc3339',value=>value);
    argument.input.setAttribute('list',suggest.id);
    const before=numberInput('Look back (seconds)',binding?.offsetSeconds?-binding.offsetSeconds:'','0');
    const fieldsRow=node('div',undefined,'automation-grid');fieldsRow.append(argument.wrap,labelled('Format',format),before.wrap);row.append(fieldsRow);
    return {element:row,name:legendName(row,'Time argument'),argument,read(strict){
      const key=argument.input.value.trim();
      if(strict&&!key)throw Error(`Name the argument of each time argument in ${where()}.`);
      const back=whole(before.input,`Look back of ${key||'a time argument'}`,{min:0,max:366*86400});
      const value={...(binding||{}),format:format.value};put(value,'offsetSeconds',back?-back:undefined);
      return [key,value];
    }};
  });
  const timeNames=()=>times.items.map(item=>item.argument.input.value.trim()).filter(Boolean);
  box.append(grid,about,status,args,times.element,suggest);
  function build(){
    const op=found.find(op=>op.name===operation.value),same=sameCall();
    about.textContent=op?.description||'';
    if(!op&&!(same&&operation.value)){fields=null;args.replaceChildren();suggest.replaceChildren();return;}
    // Without discovery the saved arguments stay editable as JSON.
    fields=argumentFields(op?op.inputSchema:null,same?saved.arguments:{},{optional:name=>timeNames().includes(name)});
    args.replaceChildren(fields.element);
    const properties=isObject(op?.inputSchema?.properties)?op.inputSchema.properties:{};
    suggest.replaceChildren(...Object.entries(properties).filter(([,p])=>p?.type==='string').map(([name])=>new Option(name)));
  }
  async function load(){
    const id=connection.value;fields=null;found=[];args.replaceChildren();about.textContent='';status.textContent='';
    operation.replaceChildren(new Option(id?'Loading operations…':'Choose a connection first',''));operation.disabled=true;
    if(!id)return;
    loading=true;let failure;
    try{found=(await ctx.operations(id)).filter(op=>op.readOnly);}catch(error){failure=error;found=[];}
    if(connection.value!==id)return;
    loading=false;operation.disabled=false;operation.replaceChildren(new Option('Choose an operation',''),...found.map(op=>new Option(op.name,op.name)));
    if(has&&saved.connection===id&&saved.operation)chooseOption(operation,saved.operation,value=>failure?value:`${value} (not listed as read-only)`);
    if(failure)status.textContent='Operations could not be loaded: '+failure.message;
    build();
  }
  connection.addEventListener('change',()=>{loading=false;load();});operation.addEventListener('change',build);
  load();
  return {element:box,name:legendName(box,legend,max),read(strict){
    const same=sameCall(),op=loading&&same?saved.operation:operation.value;
    if(strict&&!connection.value)throw Error(`Choose a connection for ${where()}.`);
    if(strict&&!op)throw Error(`Choose an operation for ${where()}.`);
    const value=same?{...saved}:{};value.connection=connection.value;value.operation=op;
    let args;
    try{args=fields?fields.read():same?saved.arguments:{};}catch(error){throw Error(`${where()}: ${error.message}`);}
    // Arguments saved as null stay null while none are set.
    if(same&&saved.arguments==null&&isObject(args)&&!Object.keys(args).length)args=saved.arguments;
    put(value,'arguments',args??undefined);
    const bound=times.read(strict).filter(([key])=>key);
    if(strict&&new Set(bound.map(([key])=>key)).size!==bound.length)throw Error(`${where()} names a time argument twice.`);
    put(value,'time',bound.length?Object.fromEntries(bound):undefined);
    if(!same)delete value.schemaHash;
    return value;
  }};
}
function callList(ctx,calls,noun,{min=0,max=32,where=index=>`${noun.toLowerCase()} ${index+1}`}={}){
  return repeat(noun,calls||[],call=>{
    let index=0;const editor=callEditor(ctx,call,noun,()=>max===1?noun.toLowerCase():where(index),max);
    return {...editor,name:(i,count)=>{index=i;editor.name(i,count);}};
  },{min,max});
}

// One independent check: a named call with assertions on its result or a
// reference call to compare with, and with repair, the SQL block it checks.
function checkEditor(ctx,saved,kind,refresh){
  const has=isObject(saved);saved=has?saved:{};let index=0;
  const box=part('Check'),where=()=>`check ${index+1}`;
  const name=textInput('Check name',saved.name),block=textInput('SQL block',saved.block,'Block name in the recipe note');
  name.input.addEventListener('input',()=>refresh());
  const top=node('div',undefined,'automation-grid');top.append(name.wrap,block.wrap);
  const call=callEditor(ctx,saved.call,'Call',()=>`the call of ${where()}`);
  const assertions=repeat('Assertion',saved.assertions||[],assertion=>{
    const row=part('Assertion'),pointer=textInput('Result pointer',assertion?.pointer,'/rows/0/count (blank for the whole result)');
    const equals=jsonInput('Expected value',!!assertion&&'equals' in assertion,assertion?.equals);
    const grid=node('div',undefined,'automation-grid');grid.append(pointer.wrap,equals.wrap);row.append(grid);let at=0;
    return {element:row,name:(i,count)=>{at=i;legendName(row,'Assertion')(i,count);},read(strict){
      const path=typed(pointer.input,assertion?.pointer)??'',expected=equals.read();
      if(path&&!path.startsWith('/'))throw Error(`Assertion ${at+1} of ${where()}: a result pointer starts with /.`);
      if(strict&&!expected)throw Error(`Enter the expected value of assertion ${at+1} of ${where()}.`);
      const value={...(assertion||{}),pointer:path};put(value,'equals',expected?.value);return value;
    }};
  },{max:64});
  const compare=checkbox('Compare with a reference call',!!saved.reference),reference=node('div');
  let referenceCall=null;
  function showReference(){if(compare.input.checked&&!referenceCall){referenceCall=callEditor(ctx,saved.reference,'Reference call',()=>`the reference call of ${where()}`);reference.replaceChildren(referenceCall.element);}reference.hidden=!compare.input.checked;}
  compare.input.addEventListener('change',showReference);showReference();
  const duration=numberInput('Slowest allowed query (milliseconds)',saved.maxDurationMs,'No limit');
  const repair=node('div',undefined,'automation-repair'),template=node('textarea');template.rows=3;template.spellcheck=false;template.value=saved.sqlTemplate??'';template.placeholder='SELECT … FROM ({{sql}}) AS example';
  const depends=node('div');let dependsOn=null;
  const evidence=callList(ctx,saved.evidence,'Evidence call',{max:4,where:i=>`evidence call ${i+1} of ${where()}`});
  repair.append(labelled('SQL template (one {{sql}} for the block)',template),depends,evidence.element);
  box.append(top,call.element,assertions.element,compare.wrap,reference,duration.wrap,repair);
  return {element:box,
    name:(i,count)=>{index=i;legendName(box,'Check')(i,count);},
    checkName:()=>name.input.value.trim(),
    names(others){const chosen=dependsOn?dependsOn.read():saved.dependsOn||[];dependsOn=checkList('Runs after these fixed checks',others.map(other=>[other,other]),{missing:value=>`${value} (not in this suite)`,empty:'No other checks.'});dependsOn.set(chosen);depends.replaceChildren(dependsOn.element);},
    sync(draft){block.wrap.hidden=kind!=='recipe'&&!draft.repair;repair.hidden=!draft.repair;},
    read(strict,draft){
      const value={...saved},label=typed(name.input,saved.name);
      if(strict&&!label)throw Error(`Name ${where()}.`);
      value.name=label??'';
      if(kind==='recipe'||draft.repair)put(value,'block',typed(block.input,saved.block));else delete value.block;
      value.call=call.read(strict);
      const list=assertions.read(strict);
      // A saved null or empty list stays as it was while no assertion is added.
      put(value,'assertions',list.length?list:saved.assertions===null||Array.isArray(saved.assertions)&&!saved.assertions.length?saved.assertions:undefined);
      if(compare.input.checked)value.reference=referenceCall.read(strict);else delete value.reference;
      put(value,'maxDurationMs',whole(duration.input,`Slowest allowed query of ${where()}`,{min:0}));
      // Repair-only settings go with the repair policy.
      if(draft.repair){put(value,'sqlTemplate',template.value===(saved.sqlTemplate??'')?saved.sqlTemplate:template.value||undefined);const after=dependsOn.read();put(value,'dependsOn',after.length?after:undefined);const calls=evidence.read(strict);put(value,'evidence',calls.length?calls:undefined);}
      else for(const key of ['sqlTemplate','dependsOn','evidence'])delete value[key];
      return value;
    }};
}

export function automationSections({manage,operations,connections,notebooks}){
  const element=node('div',undefined,'automation-sections');
  const cache=new Map();
  let parts=[],chat=null;
  const ctx={get connections(){return connections();},operations(id){if(!cache.has(id))cache.set(id,operations(id).catch(error=>{cache.delete(id);throw error;}));return cache.get(id);}};
  // Slack channels the chat-client policy already configures; none when Slack
  // is not set up or cannot be read.
  const slack=()=>chat??=manage('chat_clients').then(data=>data.config?.clients?.slack||{},()=>({}));

  function calls(draft,{min,max}){
    const box=part(max===1?'Data call':'Data calls','Calls run read-only operations of the connections checked under Data connections.');
    const list=callList(ctx,draft.calls,'Call',{min,max});box.append(list.element);
    return {element:box,read(draft,strict){draft.calls=list.read(strict);}};
  }
  function checks(draft){
    const box=part('Checks',draft.kind==='recipe'?'Each check runs the query operation on a named SQL block of the recipe note.':'Each check passes when its result matches every assertion, or its reference call.');
    let list;const refresh=()=>{for(const item of list?.items||[])item.names(list.items.filter(other=>other!==item).map(other=>other.checkName()).filter(Boolean));};
    list=repeat('Check',draft.checks||[],check=>checkEditor(ctx,check,draft.kind,refresh),{min:1,changed:refresh});
    refresh();box.append(list.element);
    return {element:box,sync(draft){for(const item of list.items)item.sync(draft);},read(draft,strict){
      const values=list.items.map(item=>item.read(strict,draft));
      if(strict&&new Set(values.map(check=>check.name)).size!==values.length)throw Error('Give each check its own name.');
      draft.checks=values;
    }};
  }
  function repairEvidence(draft){
    const box=part('Repair evidence','Read-only calls a repair sees, such as the current schema. They must not observe a clock or request ID.');
    const list=callList(ctx,draft.repair?.evidence,'Evidence call',{max:8});box.append(list.element);
    return {element:box,sync(draft){box.hidden=!draft.repair;},read(draft,strict){if(draft.repair){const calls=list.read(strict);put(draft.repair,'evidence',calls.length?calls:undefined);}}};
  }
  function condition(draft){
    const saved=isObject(draft.condition)?draft.condition:null,box=part('Condition','The monitor raises an incident while its call’s result matches this test.');
    const which=comparisons.find(([key])=>saved&&saved[key]!==undefined&&saved[key]!==null)?.[0]||'equals';
    const pointer=textInput('Result pointer',saved?.pointer,'/rows/0/count (blank for the whole result)'),test=choiceSelect('Test',comparisons);test.value=which;
    const value=textInput('Value',saved&&saved[which]!==undefined?which==='equals'?JSON.stringify(saved[which]):String(saved[which]):'','A number, or for Equals any JSON value'),initial=value.input.value;
    const grid=node('div',undefined,'automation-grid');grid.append(pointer.wrap,labelled('Test',test),value.wrap);box.append(grid);
    return {element:box,read(draft){
      const result={...(saved||{})};for(const [key] of comparisons)delete result[key];
      const path=typed(pointer.input,saved?.pointer)??'';if(path&&!path.startsWith('/'))throw Error('A result pointer starts with /.');
      result.pointer=path;
      if(test.value===which&&value.input.value===initial&&saved)result[which]=saved[which];
      else if(!value.input.value.trim())throw Error('Enter the value the condition tests.');
      else if(test.value==='equals'){try{result.equals=JSON.parse(value.input.value);}catch{throw Error('The value for Equals must be JSON, such as 3, "ok" or true.');}}
      else{const n=Number(value.input.value);if(!Number.isFinite(n))throw Error(`The value for ${test.selectedOptions[0].text} must be a number.`);result[test.value]=n;}
      draft.condition=result;
    }};
  }
  function alerts(draft){
    const saved=isObject(draft.alert)?draft.alert:null,box=part('Incident alerts','Blank fields use the defaults: alert on the first failing run and resolve on the first passing one.');
    const active=numberInput('Failing runs before an alert',saved?.activeAfter,'1'),resolved=numberInput('Passing runs before it resolves',saved?.resolvedAfter,'1'),repeatAfter=numberInput('Repeat an open alert after (seconds)',saved?.repeatSeconds,'Never'),notify=checkbox('Notify when an incident resolves',saved?.notifyResolved);
    const grid=node('div',undefined,'automation-grid');grid.append(active.wrap,resolved.wrap,repeatAfter.wrap);box.append(grid,notify.wrap);
    return {element:box,read(draft){
      const value={...(saved||{})};
      put(value,'activeAfter',whole(active.input,'Failing runs before an alert',{min:0,max:100}));put(value,'resolvedAfter',whole(resolved.input,'Passing runs before it resolves',{min:0,max:100}));
      put(value,'repeatSeconds',whole(repeatAfter.input,'Repeat an open alert after',{min:0,max:31*86400}));put(value,'notifyResolved',notify.input.checked||undefined);
      if(saved||Object.keys(value).length)draft.alert=value;else delete draft.alert;
    }};
  }
  function notices(draft){
    const box=part('Slack notices','Send results to one person, or to a shared channel the Slack policy approves for this notebook and service account.');
    const user=textInput('Slack user ID',draft.notifySlackUser,'U0123ABCD'),saved=draft.notifySlackUser;
    const channel=laterChoice('Shared Slack channel',draft.notifyChannel,slack().then(client=>Object.entries(client.broadcasts||{}).map(([id,b])=>[id,`${id} · ${b.project}${b.audienceApproved?'':' · audience not approved'}`])),{placeholder:'None',missing:value=>`${value} (not a configured shared channel)`});
    const grid=node('div',undefined,'automation-grid');grid.append(user.wrap,channel.wrap);box.append(grid);
    return {element:box,read(draft,strict){
      const person=typed(user.input,saved),shared=channel.read();
      if(strict&&person&&shared)throw Error('Notify one Slack user or one shared channel, not both.');
      put(draft,'notifySlackUser',person);put(draft,'notifyChannel',shared);
    }};
  }
  function usage(draft){
    const saved=isObject(draft.usage)?draft.usage:null,box=part('Recheck after MCP use','Check the recipe again when a query through MCP fails or is slow.');
    const errors=checkbox('After a failed query',saved?.errors),slow=numberInput('After a query slower than (milliseconds)',saved?.slowMs,'Off'),cooldown=numberInput('Wait between rechecks (seconds, 60–86400)',saved?.cooldownSeconds,'3600');
    const grid=node('div',undefined,'automation-grid');grid.append(slow.wrap,cooldown.wrap);box.append(errors.wrap,grid);
    return {element:box,read(draft){
      const slowMs=whole(slow.input,'Slow query threshold',{min:0});
      if(!errors.input.checked&&!slowMs){delete draft.usage;return;}
      const value={...(saved||{})};put(value,'errors',errors.input.checked||undefined);put(value,'slowMs',slowMs||undefined);
      value.cooldownSeconds=whole(cooldown.input,'Wait between rechecks',{min:60,max:86400})??3600;draft.usage=value;
    }};
  }
  function drive(draft){
    const saved=isObject(draft.drive)?draft.drive:{},box=part('Google Drive import','Imports supported documents from these folders and their subfolders, a few files per run.');
    const connection=choiceSelect('Drive connection',ctx.connections.map(info=>[info.id,connectionText(info)]),{placeholder:'Choose a connection'});chooseOption(connection,saved.connection||'',value=>`${value} (not listed)`);
    const destination=textInput('Notebook folder',saved.destination,'imports/drive'),limit=numberInput('Files per run (0–128)',saved.maxFiles,'Default');
    const grid=node('div',undefined,'automation-grid');grid.append(labelled('Drive connection',connection),destination.wrap,limit.wrap);
    const folders=repeat('Folder',saved.folders||[],folder=>{
      const row=node('div',undefined,'automation-row'),id=textInput('Folder ID',folder,'Drive folder ID');row.append(id.wrap);
      return {element:row,name:index=>{id.input.setAttribute('aria-label',`Folder ID ${index+1}`);},read:strict=>{const value=typed(id.input,folder);if(strict&&!value)throw Error('Enter each Drive folder ID, or remove the empty row.');return value??'';}};
    },{min:1});
    box.append(grid,node('p','Root folders','small'),folders.element);
    return {element:box,read(draft,strict){
      if(strict&&!connection.value)throw Error('Choose the Drive connection.');
      const value={...saved,connection:connection.value,folders:folders.read(strict)};
      put(value,'destination',typed(destination.input,saved.destination)??'');put(value,'maxFiles',whole(limit.input,'Files per run',{min:0,max:128}));
      draft.drive=value;
    }};
  }
  function script(draft){
    const box=part('Package');
    if(!isObject(draft.script)){box.append(node('p','Prepare a package installation from Repository packages, then review it here.','small'));return {element:box,read(){}};}
    const saved=draft.script;
    box.append(node('p',`${saved.sourceProject} / ${saved.packagePath} · ${String(saved.revision||'').slice(0,12)} · ${saved.entrypoint} · ${saved.runtime}`,'small'));
    const body=node('div',undefined,'automation-package');body.append(node('p','Loading the package manifest…','small'));box.append(body);
    let read=null;
    // The manifest at the installed revision names the bindings and parameters.
    manage('discover_automation_packages',{project:saved.sourceProject,revision:saved.revision,path:saved.packagePath}).then(entries=>{
      const entry=(entries||[]).find(p=>p.path===saved.packagePath)||entries?.[0];
      if(!entry)throw Error('The package was not found at its installed revision.');
      build(entry.manifest||{});
    }).catch(error=>{body.replaceChildren(node('p',`The package manifest could not be loaded (${error.message}). Its bindings and parameters stay as saved; edit them in the JSON definition.`,'small'));});
    function build(manifest){
      body.replaceChildren();
      const required=Object.keys(manifest.connections||{}),optional=Object.keys(manifest.optionalConnections||{});
      const names=[...new Set([...required,...optional,...Object.keys(saved.connections||{})])],bindings=[];
      if(names.length){
        const set=part('Package connections');
        for(const name of names){const need=!optional.includes(name),select=choiceSelect(name,ctx.connections.map(info=>[info.id,connectionText(info)]),{placeholder:need?'Choose a connection':'Not used'});chooseOption(select,saved.connections?.[name]??'',value=>`${value} (not listed)`);set.append(labelled(need?name:`${name} (optional)`,select));select.setAttribute('aria-label',name);bindings.push({name,need,select});}
        body.append(set);
      }
      const books=[...new Set([...Object.keys(manifest.notebooks||{}),...Object.keys(saved.notebooks||{})])],notebookBindings=[];
      if(books.length){
        const set=part('Package notebooks');
        for(const name of books){const select=choiceSelect(name,notebooks(),{placeholder:'Choose a notebook'});chooseOption(select,saved.notebooks?.[name]??'');const access=manifest.notebooks?.[name];set.append(labelled(access?`${name} (${access})`:name,select));select.setAttribute('aria-label',name);notebookBindings.push({name,select});}
        body.append(set);
      }
      const extras=manifest.optionalCapabilities||[];let capabilities=null;
      if(extras.length){capabilities=checkList('Optional capabilities',extras.map(c=>[c,c]));capabilities.set((saved.capabilities||[]).filter(c=>extras.includes(c)));body.append(capabilities.element);}
      let schema=null;try{schema=manifest.parametersSchema?typeof manifest.parametersSchema==='string'?JSON.parse(manifest.parametersSchema):manifest.parametersSchema:null;}catch{}
      let parameters=null;
      if(schema||isObject(saved.parameters)&&Object.keys(saved.parameters).length){const set=part('Parameters');parameters=argumentFields(schema,saved.parameters||{});set.append(parameters.element);body.append(set);}
      const retention=numberInput('Private journal retention (hours, 1–168)',saved.retentionHours,'Run history policy');body.append(retention.wrap);
      read=(draft,strict)=>{
        // Built on the current draft: the editor's checkboxes set followPrimaryBranch and sandbox.
        const value={...draft.script};
        const linked={};for(const b of bindings){if(strict&&b.need&&!b.select.value)throw Error(`Choose a connection for the package connection ${b.name}.`);if(b.select.value||b.need)linked[b.name]=b.select.value;}
        put(value,'connections',Object.keys(linked).length?linked:undefined);
        const opened={};for(const b of notebookBindings){if(strict&&!b.select.value)throw Error(`Choose a notebook for the package notebook ${b.name}.`);opened[b.name]=b.select.value;}
        put(value,'notebooks',Object.keys(opened).length?opened:undefined);
        if(capabilities){const chosen=capabilities.read(),had=saved.capabilities||[];value.capabilities=[...had.filter(c=>!extras.includes(c)||chosen.includes(c)),...chosen.filter(c=>!had.includes(c))];}
        if(parameters){let given;try{given=parameters.read();}catch(error){throw Error('Package parameters: '+error.message);}put(value,'parameters',Object.keys(given).length?given:undefined);}
        put(value,'retentionHours',whole(retention.input,'Private journal retention',{min:0,max:168}));
        draft.script=value;
      };
    }
    return {element:box,read(draft,strict){read?.(draft,strict);}};
  }
  function trigger(draft){
    const saved=isObject(draft.trigger)?draft.trigger:{},box=part('Source event','Runs after the source changes and then stays quiet, or after the longest wait.');
    const kind=choiceSelect('Event',eventKinds);chooseOption(kind,saved.kind||'repository.updated',value=>value);
    const project=choiceSelect('Notebook to watch',notebooks(),{placeholder:'Choose a notebook'});chooseOption(project,saved.project||'');
    const projectWrap=labelled('Notebook to watch',project);
    const channel=laterChoice('Knowledge channel',saved.channel,slack().then(client=>Object.entries(client.knowledgeChannels||{}).map(([id,notebook])=>[id,`${id} · ${notebook}`])),{placeholder:'Choose a channel'});
    const quiet=numberInput('Quiet period (seconds)',saved.debounceSeconds,'120'),longest=numberInput('Longest wait (seconds, up to 86400)',saved.maxWaitSeconds,'900');
    const grid=node('div',undefined,'automation-grid');grid.append(labelled('Event',kind),projectWrap,channel.wrap,quiet.wrap,longest.wrap);
    box.append(grid,node('p','A watched notebook must be this automation’s notebook or one of its input notebooks. A knowledge channel must be mapped to this automation’s notebook.','small'));
    const show=()=>{projectWrap.hidden=kind.value!=='repository.updated';channel.wrap.hidden=kind.value!=='slack.knowledge';};kind.addEventListener('change',show);show();
    return {element:box,read(draft,strict){
      const value={...saved,kind:kind.value};
      if(kind.value==='repository.updated'){if(strict&&!project.value)throw Error('Choose the notebook to watch.');value.project=project.value;delete value.channel;}
      else{const id=channel.read();if(strict&&!id)throw Error('Choose the knowledge channel.');value.channel=id??'';delete value.project;}
      value.debounceSeconds=whole(quiet.input,'Quiet period',{min:1,max:86400,blank:false});value.maxWaitSeconds=whole(longest.input,'Longest wait',{min:1,max:86400,blank:false});
      if(value.maxWaitSeconds<value.debounceSeconds)throw Error('The longest wait must be at least the quiet period.');
      draft.trigger=value;
    }};
  }

  const api={element,
    // Parts for the draft's kind, and its source event when that trigger is chosen.
    render(draft,{event=false,readOnly=false}={}){
      const kind=draft.kind;parts=[];
      if(kind==='digest')parts.push(calls(draft,{min:1,max:32}));
      if(kind==='monitor')parts.push(calls(draft,{min:1,max:1}),condition(draft));
      if(kind==='checks'||kind==='recipe')parts.push(checks(draft));
      if(kind==='checks')parts.push(repairEvidence(draft));
      if(kind==='recipe')parts.push(usage(draft));
      if(kind==='drive-sync')parts.push(drive(draft));
      if(kind==='script')parts.push(script(draft));
      if(['monitor','native','script'].includes(kind))parts.push(alerts(draft));
      if(['digest','monitor','script'].includes(kind))parts.push(notices(draft));
      if(event)parts.push(trigger(draft));
      element.replaceChildren(...parts.map(p=>p.element));element.hidden=!parts.length;
      api.disable(readOnly);api.sync(draft);
    },
    sync(draft){for(const p of parts)p.sync?.(draft);},
    disable(on){for(const p of parts)p.element.disabled=on;},
    // Writes each part into the draft. Strict reads (save and validate) also
    // name a missing choice; otherwise a part that cannot be read yet is left as
    // it was, so the JSON view follows what is complete.
    read(draft,{strict=false}={}){
      for(const p of parts){
        if(!strict){try{p.read(draft,false);}catch{}continue;}
        p.read(draft,true);
      }
      if(strict&&!draft.disabled){
        const used=[...(draft.calls||[]),...(draft.repair?.evidence||[]),...(draft.checks||[]).flatMap(c=>[c.call,c.reference,...(c.evidence||[])])].filter(Boolean).map(c=>c.connection);
        if(draft.drive?.connection)used.push(draft.drive.connection);
        const missing=[...new Set(used)].filter(id=>id&&!(draft.connections||[]).includes(id));
        if(missing.length)throw Error(`Check ${missing.join(', ')} under Data connections: this automation calls ${missing.length===1?'it':'them'}.`);
      }
    }};
  return api;
}
