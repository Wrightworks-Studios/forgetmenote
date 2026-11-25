import {post} from './request.mjs';
import {$,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';
const form=$('invitation-form'),status=$('invite-status');
let token=location.hash.slice(1);
try{token ||= sessionStorage.getItem('fmn-invitation')||'';if(token)sessionStorage.setItem('fmn-invitation',token);}catch{}
if(location.hash)history.replaceState(null,'','/invitation');
let needsPassword=false;
async function initialize(){
 if(!token)throw new Error('This invitation link is incomplete.');
 const info=await post('/auth/invitation/inspect',{token});
 $('invite-title').textContent=info.reset?'Reset password':'Join '+document.documentElement.dataset.instanceName;
 const signup=!info.reset&&!info.signedIn&&info.local;needsPassword=info.local&&(info.reset||signup);
 for(const id of ['invite-username','invite-username-label'])if($(id))$(id).hidden=!signup;
 if($('invite-username'))$('invite-username').required=signup;
 for(const id of ['invite-password','invite-password-label','invite-confirm','invite-confirm-label'])if($(id))$(id).hidden=!needsPassword;
 if($('invite-password'))$('invite-password').required=needsPassword;
 if($('invite-confirm'))$('invite-confirm').required=needsPassword;
 form.hidden=info.reset?!info.local:!info.local&&!info.signedIn;
 if(info.reset&&!info.local)status.textContent='Manage your password with your identity provider.';
 $('invite-accept').textContent=info.reset?'Reset password':signup?'Create account':'Accept invitation';
 if(info.reset)$('invite-login')?.remove();
}
form.onsubmit=event=>{
 event.preventDefault();
 return busy(form,async()=>{
  if(needsPassword&&$('invite-password').value!==$('invite-confirm').value)throw new Error('The passwords must match.');
  await post('/auth/invitation/accept',{token,username:$('invite-username')?.hidden?'':$('invite-username')?.value||'',password:needsPassword?$('invite-password').value:''});
  try{sessionStorage.removeItem('fmn-invitation');}catch{}
  form.reset();form.hidden=true;$('invite-login')?.remove();feedback(status,'Done. Sign in to continue.',{level:'success'});$('invite-done').hidden=false;
 },{controls:$('invite-accept'),report:status});
};
initialize().catch(error=>{status.textContent=error.message;form.hidden=true;});
