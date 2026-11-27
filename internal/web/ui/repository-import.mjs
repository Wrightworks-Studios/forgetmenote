import {$,busy,followName} from './dom.mjs';
import {feedback} from './feedback.mjs';

// Administration → Repositories imports a repository the instance's Sources
// App is installed on as a read-only notebook, with import_repository, the
// action API and MCP clients use. The server checks that the caller is an
// instance Admin, that sharing was accepted and that the App can read the
// repository now; the category is hidden while the repositories module is off.
export function initRepositoryImport({post,getUser,getActive,reloadProjects}){
 const form=$('repository-import-form'),status=$('repository-import-status');
 const forgetSuggestedID=followName($('repository-import-name'),$('repository-import-id'));
 function open(instance){
  $('repository-import-app').textContent=instance?.repositorySources?'The Sources App is configured. It must be installed on the repository, with access to it.':'No Sources App is configured on this instance, so imports are refused.';
 }
 form.onsubmit=event=>{
  event.preventDefault();
  const active=getActive(),account=getUser()?.id;
  const live=()=>getUser()?.id===account&&active();
  status.textContent='';
  return busy(form,async()=>{
   const name=$('repository-import-name').value.trim();
   const state=await post('/integrations/actions/import_repository',{
    id:$('repository-import-id').value.trim(),name,
    installation:Number($('repository-import-installation').value),repository:Number($('repository-import-repository').value),
    shareWithNotebook:$('repository-import-share').checked,externalProcessing:$('repository-import-external').checked});
   if(!live())return;
   form.reset();forgetSuggestedID();
   feedback(status,`Imported ${state.repository?.full_name||'the repository'} into ${name}. Files appear as the first download finishes.`,{level:'success'});
   await reloadProjects();
  },{controls:event.submitter,report:status,current:live});
 };
 return {open};
}
