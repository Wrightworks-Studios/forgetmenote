import {icon} from './icons.mjs';
import {feedback} from './feedback.mjs';
export const $=id=>document.getElementById(id);
// A field where people write or read note or comment text keeps what they
// type, as the editor does (frontend/source-editor.mjs): the browser neither
// autocorrects, capitalizes nor suggests, and spellcheck only underlines.
// Static fields in the page carry the same attributes.
export function keepTyped(field){
  for(const [name,value] of [['autocorrect','off'],['autocapitalize','off'],['writingsuggestions','false'],['spellcheck','true']])field.setAttribute(name,value);
  return field;
}
// Text-only construction keeps untrusted content out of HTML and event sinks.
export function element(tag, text, attributes = {}) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (typeof attributes === 'string') attributes = {class:attributes};
  for (const [name,value] of Object.entries(attributes || {})) {
    if (name === 'class') node.className = value;
    else node.setAttribute(name,String(value));
  }
  return node;
}
// A notebook ID field follows its name field, as a lowercase slug, until
// someone types an ID of their own. The returned function forgets the last
// suggestion, for a form that was reset.
export function followName(name, id) {
  let suggested='';
  name.addEventListener('input',()=>{
    if(id.value && id.value!==suggested)return;
    suggested=name.value.normalize('NFKD').toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'').slice(0,64);
    id.value=suggested;
  });
  return ()=>{suggested='';};
}
export function button(text, action, attributes = {}) {
  const node=element('button',text,{type:'button',...attributes});
  node.onclick=action;
  return node;
}
const running=new WeakSet();
// Runs one action at a time for `scope` (a form, a button or the panel whose
// actions exclude one another): a call while its scope is busy is ignored, and
// a scope that is not itself a control is marked aria-busy while it runs.
// The `controls` (by default the scope) are disabled while it runs and then
// return to their earlier state. That restore undoes a disabled state the
// action renders for a listed control, so a control whose state follows data
// the action reloads must be rendered again by the caller once busy() resolves.
// When `controls` is a function it is asked again at the end: a control it adds
// was rendered during the run, disabled through isBusy(), and is enabled.
// With `retire` the controls stay disabled once `current()` no longer holds, so
// a view that moved on keeps its retired controls. A failure is reported only
// while `current()` holds: through feedback() when `report` is an element, by
// calling it when it is a function, and otherwise rethrown. Resolves to the
// action's result.
export async function busy(scope, action, {controls=scope, report, current=()=>true, retire=false} = {}) {
  if(running.has(scope))return;
  running.add(scope);
  const list=()=>[typeof controls==='function'?controls():controls].flat().filter(Boolean);
  const saved=new Map(list().map(control=>[control,!!control.disabled]));
  for(const control of saved.keys())control.disabled=true;
  const marked=!saved.has(scope) && typeof scope.setAttribute==='function';
  if(marked)scope.setAttribute('aria-busy','true');
  try {return await action();}
  catch(error) {
    if(!current())return;
    if(typeof report==='function')report(error);
    else if(report)feedback(report,error.message || 'The request failed.');
    else throw error;
  }
  finally {
    running.delete(scope);
    if(marked)scope.removeAttribute('aria-busy');
    if(!retire || current())for(const control of new Set([...saved.keys(),...list()]))control.disabled=saved.get(control) ?? false;
  }
}
export const isBusy=scope=>running.has(scope);
// A button that runs one action at a time. owner() is read at the click: it
// returns nothing to ignore the click, or that click's "still current" check,
// which retires the button and silences its report once the view moves on.
export function actionButton(text, run, {owner=()=>()=>true, report=()=>{}, ...attributes} = {}) {
  const node=button(text,undefined,{class:'quiet',...attributes});
  node.onclick=event=>{
    const current=owner();
    if(current)return busy(node,()=>run(current,event),{report,current,retire:true});
  };
  return node;
}
// A link to one of the app's routes: a plain click runs `action` in the page,
// while a modified click (new tab or window, download) keeps the browser's
// handling of its real URL. With `fragments`, a link to a fragment of this page
// always runs in the page. A link whose view is no longer `active` opens nothing.
const modifiedClick=event=>event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
export function routeLink(anchor, action, {fragments=false, active=()=>true} = {}) {
  anchor.onclick=event=>{
    if(!active()){event.preventDefault();return;}
    if(modifiedClick(event) && !(fragments && anchor.getAttribute('href')?.startsWith('#')))return;
    event.preventDefault();action(event);
  };
  return anchor;
}
export function closeButton(action, label = 'Close') {
  const node=button('',action,{class:'quiet close-control','aria-label':label,title:label});
  node.append(icon('close'));
  return node;
}
// Saves a Blob as a file through a short-lived object URL.
export function downloadBlob(blob, filename) {
  const url=URL.createObjectURL(blob),link=element('a');
  link.href=url;link.download=filename;link.click();
  setTimeout(()=>URL.revokeObjectURL(url),1000);
}
export function field(text, control) {
  const label=element('label',text);
  label.append(control);
  return label;
}

// Rendered Markdown (sanitized notes and answers, or a local private preview)
// is untrusted and shares this document with application elements. Its IDs
// carry this prefix, so a heading cannot replace an application lookup such as
// #notice. Links and routes keep the unprefixed fragment.
const contentPrefix='h-';
export const contentFragment=id=>id.startsWith(contentPrefix)?id.slice(contentPrefix.length):id;
export const contentElement=(root,fragment)=>fragment?root.querySelector('#'+CSS.escape(contentPrefix+fragment)):null;
function fragmentTarget(href) {
  if(/^(https?:|mailto:)/i.test(href))return {external:true};
  if(!href.startsWith('#'))return null;
  try {return {hash:decodeURIComponent(href.slice(1))};} catch {return null;}
}
// The one installer for rendered HTML. `prepare` sees the original links
// (videos, tables, media removal). `resolve` maps each href to a target, or
// null to remove the link; external targets open in a new tab. `follow` routes
// other targets, may set the href, and returns the plain-click action; a
// modifier click keeps the browser's handling of a real URL. Without owners,
// in-document links scroll within the content and other links are removed.
export function installHTML(root,html,{prepare,resolve=fragmentTarget,follow}={}) {
  root.innerHTML=html;
  for(const node of root.querySelectorAll('[id]'))if(!node.id.startsWith(contentPrefix))node.id=contentPrefix+node.id;
  // Only the note's own links are resolved; prepare may add the app's controls.
  const links=[...root.querySelectorAll('a[href]')];
  prepare?.(root);
  for(const anchor of links) {
    if(!root.contains(anchor))continue;
    const target=resolve(anchor.getAttribute('href'));
    if(!target){anchor.removeAttribute('href');continue;}
    if(target.external){if(target.url)anchor.href=target.url;anchor.target='_blank';anchor.rel='noopener noreferrer';continue;}
    const action=follow ? follow(anchor,target) : ()=>contentElement(root,target.hash)?.scrollIntoView({block:'start'});
    if(action)routeLink(anchor,action,{fragments:true});
  }
}
