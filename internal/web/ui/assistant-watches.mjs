import {element as node,button} from './dom.mjs';
import {post} from './request.mjs';
import {argumentFields} from './operation-arguments.mjs';

// The person's data watches: each runs one read-only operation on a data
// connection whenever their assistant checks for new items. Connections and
// operations come from the discovery every caller uses, so the choices are the
// ones the person may use; save_assistant_settings checks each watch again.
// A personal run processes what it reads with AI, so only connections that allow
// AI processing can be chosen. A saved watch the person can no longer use stays
// as it is, marked, until they remove it.
export function initWatches(root){
  const call=(name,input={})=>post('/integrations/actions/'+name,input);
  const list=node('div',undefined,'assistant-watch-list'),message=node('p','','small'),rows=[];
  const add=button('Add data watch',()=>row(),{class:'quiet'});
  root.replaceChildren(message,list,add);
  let connections=[],operations=new Map(),version=0;
  function operationsOf(id){
    if(!operations.has(id))operations.set(id,call('list_data_operations',{connection:id}).then(found=>found.filter(op=>op.readOnly)));
    return operations.get(id);
  }
  function row(watch=null){
    const box=node('fieldset',undefined,'assistant-watch'),current=version;box.append(node('legend','Data watch'));
    const connection=node('select'),operation=node('select'),about=node('small'),args=node('div'),status=node('p','','small');
    const connectionLabel=node('label','Connection'),operationLabel=node('label','Operation');connection.setAttribute('aria-label','Connection');operation.setAttribute('aria-label','Operation');connectionLabel.append(connection);operationLabel.append(operation);
    connection.append(new Option('Choose a connection',''));
    for(const info of connections){const option=new Option(info.externalProcessing?info.label:info.label+' (no AI processing)',info.id);option.disabled=!info.externalProcessing&&info.id!==watch?.connection;connection.append(option);}
    if(watch&&!connections.some(info=>info.id===watch.connection))connection.append(new Option(watch.connection+' (not available to you)',watch.connection));
    connection.value=watch?.connection||'';
    const grid=node('div',undefined,'assistant-grid');grid.append(connectionLabel,operationLabel);
    const state={box,fields:null,pending:null};
    box.append(grid,about,args,status,button('Remove data watch',()=>{rows.splice(rows.indexOf(state),1);box.remove();},{class:'quiet'}));
    // Fields for the chosen operation; the saved arguments only for the saved one.
    function argumentsFor(op){
      const saved=watch&&connection.value===watch.connection&&operation.value===watch.operation;
      about.textContent=op?.description||'';
      state.fields=argumentFields(op?op.inputSchema:null,saved?watch.arguments:{});
      args.replaceChildren(state.fields.element);
    }
    async function loadOperations(){
      const id=connection.value;state.fields=null;args.replaceChildren();about.textContent='';status.textContent='';
      operation.replaceChildren(new Option(id?'Loading operations…':'Choose a connection first',''));operation.disabled=true;
      if(!id)return;
      let found,failure;
      try{found=await operationsOf(id);}catch(error){failure=error;operations.delete(id);}
      if(current!==version||connection.value!==id)return;
      operation.disabled=false;operation.replaceChildren(new Option('Choose an operation',''));
      for(const op of found||[])operation.append(new Option(op.name,op.name));
      const savedHere=watch&&watch.connection===id;
      if(savedHere&&!(found||[]).some(op=>op.name===watch.operation))operation.append(new Option(watch.operation+(failure?'':' (not available to you)'),watch.operation));
      operation.value=savedHere?watch.operation:'';
      if(failure)status.textContent='Operations could not be loaded: '+failure.message;
      const op=(found||[]).find(op=>op.name===operation.value);
      // Without discovery the saved arguments stay editable as JSON.
      if(op||failure&&savedHere)argumentsFor(op);
    }
    connection.onchange=()=>{state.pending=loadOperations();};
    operation.onchange=()=>{operationsOf(connection.value).then(found=>{if(current===version)argumentsFor(found.find(op=>op.name===operation.value));},()=>argumentsFor(null));};
    state.read=()=>{
      if(!connection.value)throw Error('Choose a connection for each data watch.');
      if(!operation.value)throw Error('Choose an operation for each data watch.');
      if(!state.fields){
        if(watch&&connection.value===watch.connection&&operation.value===watch.operation)return watch;
        throw Error('Wait for the data watch operations to load.');
      }
      try{return {connection:connection.value,operation:operation.value,arguments:state.fields.read()};}
      catch(error){throw Error(`Data watch ${operation.value}: ${error.message}`);}
    };
    rows.push(state);list.append(box);
    state.pending=loadOperations();
    return state;
  }
  // Renders the saved watches; resolves once each one shows its operation.
  async function render(watches,available=true){
    const current=++version;operations=new Map();rows.length=0;list.replaceChildren();message.textContent='';
    connections=[];
    if(available){
      try{connections=await call('list_data_connections');}
      catch(error){if(current===version)message.textContent='Data connections could not be loaded: '+error.message;}
    }
    if(current!==version)return;
    if(available&&!message.textContent&&!connections.some(info=>info.externalProcessing))message.textContent='No data connection you can use allows AI processing, so there is nothing to watch yet.';
    add.hidden=!connections.some(info=>info.externalProcessing);
    await Promise.all((watches||[]).map(watch=>row(watch).pending));
  }
  return {render,read:()=>rows.map(state=>state.read())};
}
