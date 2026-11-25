import {element,closeButton} from './dom.mjs';
// One document owner combines current overlay claims. A closing/resizing panel
// must not restore an old inert snapshot over another visible panel.
export function createOverlayManager(document) {
  const panels=new Map(),baseline=new Map();
  let order=0;
  const visible=()=>[...panels.entries()].filter(([node,value])=>value.active && !node.hidden);
  function synchronize() {
    const covered=new Set();
    for(const [panel,value] of visible())if(value.modal) {
      for(const node of value.covers || [...document.body.children]) {
        if(node===panel || node.contains(panel) || ['SCRIPT','DIALOG','STYLE'].includes(node.tagName) || node.id?.endsWith('-backdrop'))continue;
        covered.add(node);
      }
    }
    for(const node of covered) {
      if(!baseline.has(node))baseline.set(node,node.inert);
      node.inert=true;
    }
    for(const [node,inert] of baseline)if(!covered.has(node)) {node.inert=inert;baseline.delete(node);}
  }
  function panel(node,value) {
    const previous=panels.get(node);
    if(!value.active)panels.delete(node);
    else {
      if(value.group)for(const [other,entry] of [...panels])if(other!==node && entry.group===value.group)entry.close();
      panels.set(node,{...value,order:previous?.order || ++order});
    }
    synchronize();
  }
  function keydown(event) {
    // Only Escape and Tab concern an overlay; looking for an open dialog
    // searches the whole page, a long note's included, on every keystroke.
    if(event.defaultPrevented || event.key!=='Escape' && event.key!=='Tab' || document.querySelector('dialog[open]'))return;
    const top=visible().sort((a,b)=>b[1].order-a[1].order)[0];
    if(!top)return;
    const [node,value]=top;
    if(event.key==='Escape') {event.preventDefault();event.stopImmediatePropagation();value.close();}
    if(event.key==='Tab' && value.modal) {
      const controls=[...node.querySelectorAll('button,select,input,textarea,summary,a[href],[tabindex]')].filter(item=>!item.disabled && item.tabIndex!==-1 && item.getClientRects().length);
      const first=controls[0],last=controls.at(-1);if(!first)return;
      if(event.shiftKey && (document.activeElement===first || !node.contains(document.activeElement))) {event.preventDefault();last.focus();}
      else if(!event.shiftKey && (document.activeElement===last || !node.contains(document.activeElement))) {event.preventDefault();first.focus();}
    }
  }
  document.addEventListener('keydown',keydown);
  return {panel,synchronize,close(){document.removeEventListener('keydown',keydown);panels.clear();synchronize();}};
}
let owner;
export function overlays() {return owner ||= createOverlayManager(document);}

// Native dialogs keep browser top-layer ordering and cancellable Escape events;
// feature-specific busy guards remain on their own cancel handlers.
export function openDialog(dialog,{opener=document.activeElement,fallback}={}) {
  if(dialog.open)return;
  dialog.addEventListener('close',()=>{
    const focused=document.activeElement;
    if(focused!==document.body && focused!==opener && !dialog.contains(focused))return;
    const target=opener?.isConnected && !opener.closest('[inert]') && opener.getClientRects().length ? opener : fallback;
    if(target?.isConnected && !target.closest('[inert]') && target.getClientRects().length)target.focus({preventScroll:true});
  },{once:true});
  dialog.showModal();
}

// Script-built dialogs share one shell: a native dialog named by its title,
// with a title row and close control, appended to its host. openDialog owns
// focus return; a transient dialog removes itself once closed.
export function dialogShell(title,{id,className,host=document.body,heading='dialog-title',transient=false}={}) {
  const dialog=element('dialog',undefined,{'aria-label':title});
  if(id)dialog.id=id;
  if(className)dialog.className=className;
  const row=element('div',undefined,{class:heading});
  row.append(element('h2',title),closeButton(()=>dialog.close()));
  dialog.append(row);host.append(dialog);
  if(transient)dialog.addEventListener('close',()=>dialog.remove());
  return dialog;
}
