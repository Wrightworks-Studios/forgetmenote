import {openDialog} from './overlay.mjs';
import {element,button,closeButton} from './dom.mjs';

// A status line in a form or panel: it stays until replaced or cleared.
export function feedback(target,text,{level='error'}={}) {
  target.textContent=text || '';target.hidden=!text;
  target.dataset.level=level;
  target.classList.toggle('success',level==='success');
  target.setAttribute('role',level==='error'?'alert':'status');
}

// A toast is the passing notice in the feedback tray at the bottom of the page:
// `message`, the live region inside its toast, an optional action and a
// Dismiss button. Every toast dismisses itself. One that reports an error or
// offers an action stays long enough to read and act on, a longer message
// stays longer, and the timer pauses while the pointer rests on the toast or
// focus is inside it. Showing a toast replaces the one shown; the returned
// function dismisses this toast only while it is still the one shown.
const toasts=new WeakMap();
export function toast(message,text,{level='error',action}={}) {
  let shown=toasts.get(message);
  if(!shown)toasts.set(message,shown=toastSlot(message));
  return shown(text || '',level,action);
}
function toastSlot(message) {
  const box=message.parentElement,close=closeButton(()=>hide(),'Dismiss');
  let generation=0,timer=0,left=0,due=0,pointer=false,focus=false;
  close.classList.add('toast-dismiss');box.append(close);
  function hide() {
    clearTimeout(timer);timer=0;generation++;
    box.hidden=true;message.textContent='';
    box.querySelector(':scope > .toast-action')?.remove();
  }
  function run() {
    if(box.hidden || timer || pointer || focus)return;
    due=Date.now()+left;timer=setTimeout(hide,left);
  }
  function pause() {
    if(!timer)return;
    clearTimeout(timer);timer=0;left=Math.max(0,due-Date.now());
  }
  box.addEventListener('pointerenter',()=>{pointer=true;pause()});
  box.addEventListener('pointerleave',()=>{pointer=false;run()});
  box.addEventListener('focusin',()=>{focus=true;pause()});
  box.addEventListener('focusout',event=>{if(!box.contains(event.relatedTarget)){focus=false;run()}});
  return (text,level,action)=>{
    hide();
    if(!text)return ()=>{};
    message.setAttribute('role',level==='error'?'alert':'status');
    message.textContent=text;
    box.dataset.level=level;box.classList.toggle('success',level==='success');
    if(action){
      const control=button(action.label,async()=>{control.disabled=true;try{await action.run()}finally{control.disabled=false}},{class:'toast-action'});
      close.before(control);
    }
    box.hidden=false;
    // A removed or hidden control may leave without a focusout event.
    pointer=box.matches(':hover');focus=box.contains(document.activeElement);
    left=Math.max(level==='error' || action?15000:5000,text.length*60);run();
    const mine=generation;
    return ()=>{if(generation===mine)hide()};
  };
}

let pending;
export function dismissConfirmation() {pending?.finish(false);}
export function confirmAction(text,{valid=()=>true}={}) {
  dismissConfirmation();
  const dialog=element('dialog',undefined,{class:'confirmation-dialog','aria-label':'Confirm action','data-confirm-action':''});
  const message=element('p',text);
  return new Promise(resolve=>{
    let finished=false;
    const finish=value=>{
      if(finished)return;finished=true;
      if(pending?.dialog===dialog)pending=null;
      dialog.close();dialog.remove();
      resolve(value && valid());
    };
    const cancel=button('Cancel',()=>finish(false),{class:'quiet',autofocus:''});
    dialog.append(element('h2','Confirm action'),message,cancel,button('Continue',()=>finish(true)));
    dialog.addEventListener('cancel',event=>{event.preventDefault();finish(false)});
    dialog.addEventListener('close',()=>finish(false));
    pending={dialog,finish};document.body.append(dialog);openDialog(dialog);cancel.focus();
  });
}
