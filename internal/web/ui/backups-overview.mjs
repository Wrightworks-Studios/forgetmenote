import {element as node,button} from './dom.mjs';

// Administration → Backups: every notebook's managed backup destinations and
// when each last verified a snapshot, notebooks without one first. It reads
// managed_backup_overview, the tool MCP and API clients use; the server
// orders the rows and leaves out credentials and personal notebooks' accounts.
const when=time=>time?new Date(time).toLocaleString():'';
function describe(d){
 const verified=d.lastSuccess?`verified ${when(d.lastSuccess)}${d.lastRevision?' at '+d.lastRevision.slice(0,12):''}`:'no snapshot verified yet';
 const condition=!d.enabled?'paused':d.blocked?'stopped':'on';
 return `${d.name}: ${d.target}${d.account?' ('+d.account+')':''} · ${condition} · ${verified}${d.encrypted?' · encrypted':''}`;
}
export function initBackupsOverview({post,getUser,getActive}){
 const root=document.getElementById('backups-overview-panel');let version=0;
 async function open(){
  const active=getActive(),current=++version;root.replaceChildren(node('p','Loading backups…'));
  const live=()=>current===version&&!!getUser()?.admin&&active();
  try{
   const overview=await post('/api/organization/action/managed_backup_overview',{});if(!live())return;
   const heading=node('div','',{class:'actions settings-heading'});heading.append(node('h3','Backups'),button('Refresh',open,{class:'quiet'}));
   const note=node('p','This lists the backups notebooks’ Owners set up here: GitHub repositories, Google Drive and Dropbox folders. Backups an operator runs on the server’s host are not visible to the app and are not listed.','small');
   if(!overview.configured){root.replaceChildren(heading,note,node('p','Managed backups are not configured on this server.'));return;}
   const unprotected=overview.notebooks.filter(row=>!row.destinations.length).length;
   const summary=node('p',unprotected?`${unprotected} of ${overview.notebooks.length} ${overview.notebooks.length===1?'notebook has':'notebooks have'} no managed backup.`:'Every notebook has a managed backup.',unprotected?'settings-warning':'small');
   const list=node('ul','',{class:'backups-overview'});
   for(const row of overview.notebooks){
    const item=node('li','',{'data-backup-notebook':row.project});
    const title=node('div','',{class:'backups-overview-notebook'});
    title.append(node('strong',row.name||row.project),node('span',' '+row.project+(row.owners.length?' · '+row.owners.join(', '):'')+(row.personal?' · personal':''),'small'));
    item.append(title);
    if(!row.destinations.length)item.append(node('p','No managed backup','small backups-overview-none'));
    for(const d of row.destinations){
     item.append(node('p',describe(d),'small'));
     if(d.error)item.append(node('p',d.error,'small settings-warning'));
    }
    list.append(item);
   }
   root.replaceChildren(heading,note,summary,list);
  }catch(error){if(live())root.replaceChildren(node('p',error.message));}
 }
 return {open};
}
