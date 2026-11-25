import {initTheme} from './theme.mjs';
import {post,responseError} from './request.mjs';
import {downloadBlob} from './dom.mjs';
initTheme();
const status=document.getElementById('connect-status');
const selector=document.getElementById('ai-client');
document.getElementById('revoke-sessions')?.addEventListener('click',async()=>{
  try{
    await post('/account/sessions/revoke',{});
    location.assign('/login');
  }catch(error){status.textContent=error.message;}
});
selector.onchange=()=>{
  for(const section of document.querySelectorAll('[data-client]'))section.hidden=section.dataset.client!==selector.value;
  status.textContent='';
};
for(const button of document.querySelectorAll('[data-copy]'))button.onclick=async()=>{
  const field=document.getElementById(button.dataset.copy);
  try{await navigator.clipboard.writeText(field.value??field.textContent);status.textContent='Copied.';}
  catch{status.textContent='Clipboard unavailable. Select and copy the text above.';field.focus();field.select?.();}
};
for(const link of document.querySelectorAll('[data-download]'))link.onclick=async event=>{
  event.preventDefault();status.textContent='Preparing download…';
  try{
    const response=await fetch(link.href,{headers:{Accept:'application/zip'}});
    if(!response.ok){const problem=await responseError(response);throw Error('Download failed (HTTP '+problem.status+'). '+problem.message.slice(0,300));}
    if(!response.headers.get('Content-Type')?.startsWith('application/zip'))throw Error('Sign in again, then retry the download.');
    downloadBlob(await response.blob(),response.headers.get('Content-Disposition')?.match(/filename="([^"]+)"/)?.[1]||'forgetmenote.zip');
    status.textContent='Downloaded. Follow the installation steps, then verify from your AI client.';
  }catch(error){status.textContent=error.message;}
};
