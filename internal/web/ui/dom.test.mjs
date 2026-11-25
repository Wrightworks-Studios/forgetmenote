import {test} from 'node:test';
import assert from 'node:assert/strict';
import {element,button,field,actionButton,busy,isBusy,routeLink,$} from './dom.mjs';

test('text-only constructors preserve labels attributes and explicit button type',()=>{
 const before=globalThis.document;
 globalThis.document={createElement:tag=>({tag,attributes:{},children:[],setAttribute(name,value){this.attributes[name]=value},append(child){this.children.push(child)}})};
 try {
  const label=element('p','<script>untrusted</script>','small');
  assert.equal(label.textContent,'<script>untrusted</script>');assert.equal(label.className,'small');assert.equal(label.innerHTML,undefined);
  const click=()=>{},control=button('Continue',click,{'aria-label':'Continue reading'});
  assert.equal(control.attributes.type,'button');assert.equal(control.attributes['aria-label'],'Continue reading');assert.equal(control.onclick,click);
  const wrapped=field('Your name',control);assert.equal(wrapped.tag,'label');assert.deepEqual(wrapped.children,[control]);
 }finally{globalThis.document=before;}
});

test('action buttons stay disabled while running and report only for a current owner',async()=>{
 const before=globalThis.document;
 globalThis.document={createElement:tag=>({tag,attributes:{},setAttribute(name,value){this.attributes[name]=value}})};
 try {
  const reported=[];let finish,owner=true,clicks=0;
  const control=actionButton('Save',current=>{clicks++;assert.equal(control.disabled,true);assert.equal(current(),owner);return new Promise((resolve,reject)=>{finish={resolve,reject};});},{owner:()=>()=>owner,report:error=>reported.push(error.message)});
  assert.equal(control.attributes.type,'button');assert.equal(control.className,'quiet');
  let run=control.onclick();assert.equal(control.disabled,true);finish.resolve();await run;
  assert.equal(control.disabled,false);
  run=control.onclick();finish.reject(Error('Denied'));await run;
  assert.deepEqual(reported,['Denied']);assert.equal(control.disabled,false);
  run=control.onclick();owner=false;finish.reject(Error('Stale'));await run;
  assert.deepEqual(reported,['Denied']);assert.equal(control.disabled,true,'a stale view keeps its control retired');
  const ignored=actionButton('Retry',()=>{clicks++;},{owner:()=>null,class:'primary'});
  await ignored.onclick();assert.equal(clicks,3);assert.equal(ignored.className,'primary');assert.notEqual(ignored.disabled,true);
 }finally{globalThis.document=before;}
});

test('the shared $ looks up the current document by id',()=>{
 const before=globalThis.document,node={id:'notice'};
 globalThis.document={getElementById:id=>id==='notice'?node:null};
 try {assert.equal($('notice'),node);assert.equal($('missing'),null);}
 finally{globalThis.document=before;}
});

test('route links run plain clicks in the page and leave modified clicks to the browser',()=>{
 const click=(modifiers={})=>{const event={prevented:false,preventDefault(){this.prevented=true},...modifiers};return event;};
 const opened=[],anchor={getAttribute:()=>'/?project=p&note=A.md'};
 assert.equal(routeLink(anchor,event=>opened.push(event)),anchor);
 const plain=click();anchor.onclick(plain);
 assert.equal(plain.prevented,true);assert.deepEqual(opened,[plain]);
 for(const modifier of ['metaKey','ctrlKey','shiftKey','altKey']){
  const event=click({[modifier]:true});anchor.onclick(event);
  assert.equal(event.prevented,false,modifier+' keeps the browser handling the real URL');
 }
 assert.equal(opened.length,1);
 const fragment={getAttribute:()=>'#section'},scrolled=[];
 routeLink(fragment,()=>scrolled.push('in page'),{fragments:true});
 const modified=click({ctrlKey:true});fragment.onclick(modified);
 assert.equal(modified.prevented,true);assert.deepEqual(scrolled,['in page'],'an in-page fragment always runs in the page');
 let current=false;const stale={getAttribute:()=>'/?note=B.md'};
 routeLink(stale,()=>assert.fail('a retired link ran'),{active:()=>current});
 for(const event of [click(),click({metaKey:true})]){stale.onclick(event);assert.equal(event.prevented,true,'a retired link opens nothing');}
});

test('busy runs one action per scope, disables its controls and reports through feedback',async()=>{
 const scope={},save={disabled:false},field={disabled:true},status={dataset:{},classList:{toggle(){}},hidden:true,setAttribute(name,value){this[name]=value}};
 let finish;const run=busy(scope,()=>new Promise((resolve,reject)=>{finish={resolve,reject};}),{controls:[save,field,null],report:status});
 assert.equal(save.disabled,true);assert.equal(field.disabled,true);assert.equal(isBusy(scope),true);
 assert.equal(await busy(scope,()=>assert.fail('a second action ran')),undefined,'a busy scope ignores another action');
 finish.resolve('done');assert.equal(await run,'done');
 assert.equal(save.disabled,false);assert.equal(field.disabled,true,'each control returns to its earlier state');assert.equal(isBusy(scope),false);
 assert.equal(status.hidden,true,'success reports nothing');
 await busy(scope,async()=>{throw Error('Denied');},{controls:save,report:status});
 assert.equal(status.textContent,'Denied');assert.equal(status.hidden,false);assert.equal(status.role,'alert');assert.equal(save.disabled,false);
 const reported=[];
 await busy(scope,async()=>{throw Error('Conflict');},{report:error=>reported.push(error.message)});
 assert.deepEqual(reported,['Conflict']);
 let owner=true;
 await busy(scope,async()=>{owner=false;throw Error('Stale');},{report:error=>reported.push(error.message),current:()=>owner});
 assert.deepEqual(reported,['Conflict'],'a view that moved on hears nothing');
 await assert.rejects(busy(scope,async()=>{throw Error('Unreported');}),/Unreported/);
 assert.equal(isBusy(scope),false);
 owner=true;const retired={disabled:false};
 await busy(retired,async()=>{owner=false;},{retire:true,current:()=>owner});
 assert.equal(retired.disabled,true,'retire keeps a moved-on view\'s control disabled');
});

test('busy marks a panel scope aria-busy and enables controls rendered during its run',async()=>{
 const attributes={},panel={setAttribute(name,value){attributes[name]=value},removeAttribute(name){delete attributes[name]}},fields=[{disabled:false}];
 let observed;
 await busy(panel,async()=>{
  observed=attributes['aria-busy'];
  fields.push({disabled:isBusy(panel)});
 },{controls:()=>fields});
 assert.equal(observed,'true');assert.equal('aria-busy' in attributes,false,'an idle panel carries no aria-busy');
 assert.deepEqual(fields.map(field=>field.disabled),[false,false],'a control rendered while busy is enabled with the rest');
 const button={disabled:false,setAttribute(){assert.fail('a control scope is not marked busy')}};
 await busy(button,async()=>assert.equal(button.disabled,true));
 assert.equal(button.disabled,false);
});
