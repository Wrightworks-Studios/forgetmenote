import {openDialog} from './overlay.mjs';
import {element,button} from './dom.mjs';

const timers=new WeakMap();
export function feedback(target,text,{level='error',duration=0}={}) {
  clearTimeout(timers.get(target));
  target.textContent=text || '';target.hidden=!text;
  target.dataset.level=level;
  target.classList.toggle('success',level==='success');
  target.setAttribute('role',level==='error'?'alert':'status');
  if(text && duration)timers.set(target,setTimeout(()=>{target.hidden=true;target.textContent='';timers.delete(target)},duration));
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
