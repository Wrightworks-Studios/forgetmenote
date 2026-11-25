import {element} from './dom.mjs';
import {openDialog} from './overlay.mjs';
import {requestSequence} from './state.mjs';

const pages={account:'account-dialog',notebook:'settings-dialog',administration:'admin-dialog'};
function withoutPage(value) {
 const url=new URL(value,location.origin);url.searchParams.delete('page');url.searchParams.delete('category');url.searchParams.delete('account');
 return url.pathname+url.search+url.hash;
}
function pageURL(base,page,category) {
 const url=new URL(base,location.origin);url.searchParams.set('page',page);url.searchParams.set('category',category);
 return url.pathname+url.search+url.hash;
}

// Long settings tasks have a URL and one page lifetime. The existing dialog
// elements use show(), not the modal top layer, so short child tasks retain
// native dialog behavior and existing feature state remains with its owner.
let owner;
export function initSettingsPages({getContext,onRoute,onEnter,onReturn=()=>{},onLeave=()=>{},load}) {
 const requests=requestSequence(),host=element('main',undefined,{id:'settings-page',hidden:''});
 document.body.append(host);let current=null,retained=null,restoring=null;
 const entries=new Map(Object.entries(pages).map(([name,id])=>[document.getElementById(id),name]));
 const controls=dialog=>[...dialog.querySelectorAll('[data-settings-category]')];
 const selected=dialog=>controls(dialog).find(button=>button.getAttribute('aria-pressed')==='true'&&!button.hidden)||controls(dialog).find(button=>!button.hidden);
 // A route is published before the page or its view replaces the other, so
 // the departing route keeps its scroll position for Back.
 function remember(replace=false) {
  if(!current)return;
  const category=selected(current.dialog)?.dataset.settingsCategory;
  if(category)onRoute(pageURL(current.base,current.name,category),replace||restoring);
 }
 function dismiss({navigate=true,focus=true,resume=navigate}={}) {
  if(!current)return;
  const entry=current;current=null;onLeave(entry.name);
  if(navigate)onRoute(entry.base,false);
  if(entry.dialog.open)entry.dialog.close();
  host.hidden=true;document.body.classList.remove('settings-page-open');
  if(resume)onReturn(entry.base);
  if(focus&&!document.querySelector('dialog:modal')) {
   const target=entry.opener?.getClientRects().length?entry.opener:document.getElementById('account-actions-toggle')?.getClientRects().length?document.getElementById('account-actions-toggle'):document.getElementById('menu');
   target?.focus({preventScroll:true});
  }
 }
 function leave(options) {requests.next();dismiss(options);}
 function prepare(dialog) {
  const name=entries.get(dialog),context=getContext();
  const fallback=document.getElementById({account:'account-open',notebook:'settings-open',administration:'admin-open'}[name]);
  const opener=document.activeElement===document.body||host.contains(document.activeElement)?fallback:document.activeElement;
  const allowed=()=>getContext().user?.id===context.user?.id&&getContext().view===context.view&&
   (name!=='notebook'||!!context.project&&getContext().project?.id===context.project.id)&&
   (name!=='administration'||!!getContext().user?.admin);
  const pending=requests.begin(allowed);let installed;
  // Loading a replacement retires pending reads, not the usable installed
  // page. Its controls retire only when a replacement is shown or it closes.
  const active=()=>allowed()&&(installed?current===installed:pending());
  if(restoring?.name===name&&!restoring.attempt)restoring.attempt=active;
  else if(restoring)restoring=null;
  return {active,show(){
   if(!active())return;
   if(current?.dialog===dialog){current={...current,allowed};installed=current;retained=current;remember();return;}
   dismiss({navigate:false,focus:false});onEnter();
   const base=withoutPage(location.href);current={dialog,name,base,opener,allowed,notebook:context.project?.id,account:context.user?.id,view:context.view};installed=current;retained=current;
   // The header's close control is the page's one way back; its tooltip names the destination.
   const close=dialog.querySelector('.dialog-title .close-control');if(close)close.title=context.project?'Back to '+context.project.name:'Back to notes';
   remember();host.hidden=false;document.body.classList.add('settings-page-open');dialog.show();
   dialog.querySelector('h2')?.focus({preventScroll:true});window.scrollTo(0,0);
  }};
 }
 for(const [dialog] of entries) {
  dialog.dataset.settingsPage='';dialog.classList.add('settings-page-panel');
  dialog.setAttribute('role','region');dialog.setAttribute('aria-label',dialog.querySelector('h2').textContent);
  dialog.querySelector('h2').tabIndex=-1;host.append(dialog);
  dialog.addEventListener('close',()=>{if(!dialog.open&&current?.dialog===dialog)leave();});
  dialog.querySelector('[data-settings-categories]').addEventListener('click',event=>{
   const choice=event.target.closest('[data-settings-category]');
   // On phones the categories are one scrolling strip; keep the chosen one in view.
   choice?.scrollIntoView({block:'nearest',inline:'nearest'});
   if(current?.dialog===dialog&&choice)remember();
  });
 }
 document.addEventListener('keydown',event=>{
  if(event.key==='Escape'&&!event.defaultPrevented&&current&&!document.querySelector('dialog:modal')){
   event.preventDefault();leave();
  }
 });
 function retainsView() {const context=getContext();return !!retained&&retained.allowed()&&context.project?.id===retained.notebook&&context.user?.id===retained.account&&context.view===retained.view;}
 owner={prepare,leave,retainsContext(url){return retainsView()&&withoutPage(url)===retained.base;},close(dialog){if(current?.dialog===dialog)leave();else if(dialog.open)dialog.close();},active:()=>!!current,check(){if(retained&&!retainsView()){retained=null;leave({navigate:false});}},async restore(query){
  const name=query.get('page'),dialog=document.getElementById(pages[name]);
  if(!dialog)throw Error('Settings page unavailable.');
  const context=getContext();
  if(name==='administration'&&!context.user?.admin)throw Error('Administration requires an administrator.');
  if(name==='notebook'&&!context.project)throw Error('Choose a notebook to open its settings.');
  const restoration={name};restoring=restoration;
  try {
   await load(name);
   if(restoring!==restoration||!restoration.attempt?.()||current?.dialog!==dialog)return;
   const choice=controls(dialog).find(button=>button.dataset.settingsCategory===query.get('category')&&!button.hidden);
   if(choice)choice.click();remember(true);
  } finally {if(restoring===restoration)restoring=null;}
 }};
 return owner;
}

export function prepareSettingsPage(dialog) {
 return owner?owner.prepare(dialog):{active:()=>true,show:()=>openDialog(dialog)};
}
export function closeSettingsPage(dialog) {
 if(owner&&dialog.dataset.settingsPage!==undefined)owner.close(dialog);else dialog.close();
}
