import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createOverlayManager,dialogShell} from './overlay.mjs';

function surface() {
 const handlers=new Map(),node=id=>({id,tagName:'ASIDE',inert:false,hidden:false,contains(other){return other===this},querySelectorAll(){return []}});
 const main=node('main'),first=node('first'),second=node('second'),sidebar=node('sidebar');
 const document={body:{children:[main,first,second,sidebar]},activeElement:main,querySelector(){return null},addEventListener(type,fn){handlers.set(type,fn)},removeEventListener(type){handlers.delete(type)}};
 return {main,first,second,sidebar,document,press(key){const event={key,preventDefault(){this.defaultPrevented=true},stopImmediatePropagation(){}};handlers.get('keydown')?.(event);return event}};
}

test('overlay claims compose without restoring an older inert snapshot',()=>{
 const view=surface(),manager=createOverlayManager(view.document);view.sidebar.inert=true;
 manager.panel(view.first,{active:true,modal:true,covers:[view.main,view.sidebar],close(){}});
 manager.panel(view.second,{active:true,modal:true,covers:[view.main],close(){}});
 manager.panel(view.first,{active:false});assert.equal(view.main.inert,true);assert.equal(view.sidebar.inert,true);
 manager.panel(view.second,{active:true,modal:false,close(){}});assert.equal(view.main.inert,false);
 manager.close();assert.equal(view.sidebar.inert,true);
});

test('Escape closes only the most recently opened panel and respects a nested native dialog',()=>{
 const view=surface(),manager=createOverlayManager(view.document),closed=[];
 for(const node of [view.first,view.second])manager.panel(node,{active:true,modal:false,close(){closed.push(node.id);node.hidden=true;manager.panel(node,{active:false})}});
 view.document.querySelector=()=>({open:true});assert.equal(view.press('Escape').defaultPrevented,undefined);assert.deepEqual(closed,[]);
 view.document.querySelector=()=>null;assert.equal(view.press('Escape').defaultPrevented,true);assert.deepEqual(closed,['second']);
 view.press('Escape');assert.deepEqual(closed,['second','first']);manager.close();
});

test('typing in the page does not search it for an open dialog',()=>{
 const view=surface(),manager=createOverlayManager(view.document);let searches=0;
 manager.panel(view.first,{active:true,modal:false,close(){}});
 view.document.querySelector=()=>{searches++;return null};
 for(const key of ['a','Enter','Backspace',' '])assert.equal(view.press(key).defaultPrevented,undefined);
 assert.equal(searches,0,'a keystroke searched the page, a long note included');
 view.press('Escape');assert.equal(searches,1);manager.close();
});

test('script-built dialogs share one named shell with a title row and close control',()=>{
 const before=globalThis.document;
 const make=tag=>({tag,attributes:{},children:[],listeners:{},setAttribute(name,value){this.attributes[name]=value},append(...nodes){this.children.push(...nodes)},addEventListener(type,fn){this.listeners[type]=fn},close(){this.closed=true;this.listeners.close?.()},remove(){this.removed=true}});
 const body=make('body');
 globalThis.document={body,createElement:make,createElementNS:(ns,tag)=>make(tag)};
 try {
  const dialog=dialogShell('Quick note',{id:'quicknotes-dialog',className:'judgments-dialog quicknotes-dialog'});
  assert.equal(dialog.tag,'dialog');assert.equal(dialog.id,'quicknotes-dialog');assert.equal(dialog.className,'judgments-dialog quicknotes-dialog');
  assert.equal(dialog.attributes['aria-label'],'Quick note');assert.deepEqual(body.children,[dialog]);
  const [heading]=dialog.children,[title,close]=heading.children;
  assert.equal(heading.className,'dialog-title');assert.equal(title.tag,'h2');assert.equal(title.textContent,'Quick note');
  assert.equal(close.attributes['aria-label'],'Close');close.onclick();assert.equal(dialog.closed,true);assert.equal(dialog.removed,undefined);
  const panel=make('aside'),transient=dialogShell('Attach a note version',{className:'chat-dialog',heading:'chat-heading',host:panel,transient:true});
  assert.deepEqual(panel.children,[transient]);assert.equal(transient.children[0].className,'chat-heading');
  transient.close();assert.equal(transient.removed,true);
 }finally{globalThis.document=before;}
});
