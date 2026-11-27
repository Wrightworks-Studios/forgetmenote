import {prepareSettingsPage,closeSettingsPage} from './settings-pages.mjs';
import {openDialog} from './overlay.mjs';
import {$,element,busy,isBusy,followName} from './dom.mjs';
import {feedback} from './feedback.mjs';
import {initOrganization} from './organization.mjs';
import {initAISettings} from './ai-settings.mjs';
import {initChatClients} from './chat-clients.mjs';
import {initTaskQueues} from './task-queues.mjs';
import {initRepositoryImport} from './repository-import.mjs';
import {contextMenuPosition} from './editor.mjs';
import {notebookPath} from './request.mjs';
import {moduleOn} from './state.mjs';
const roles = {owner:'Owner', edit:'Editor', comment:'Commenter', read:'Reader'};
const nameOf = (person, names = {}) => person.displayName || names[person.id] || (person.id === '@local-admin' ? 'Recovery administrator' : person.id);
function identity(name, id) {
  const box = element('span', undefined, 'person-identity');
  box.title = id;
  box.append(element('strong', name));
  return box;
}

// Account selection and member search belong to these dialogs, not note drafts.
export function initAdministration({api, post, notice, getUser, getProject, reloadProjects, selectProject, openRetrievalSettings, openRepositorySettings = async()=>{}, openBackupSettings = async()=>{}, openAuthoritySettings = async()=>{}, openWikiSettings = ()=>{}}) {
  let memberProject = null, directory = null, selectedUser = '', searchTimer, searchVersion = 0;
  let notebookVersion = 0, instancePageActive=()=>false;
  let importing = false, connecting = false, repositoryGrant = '';
  const notebookBusy = () => isBusy($('project-form'));
  const organization=initOrganization({post,getProject,getUser,refresh:()=>openInstance(),openNotebook:id=>openWorkspace(id)});
  const ai=initAISettings({post,getUser,getActive:()=>instancePageActive});
  const chatClients=initChatClients({post,getUser,getActive:()=>instancePageActive});
  const chatCategory=document.querySelector('#admin-dialog [data-settings-category=chat-clients]');
  chatCategory.addEventListener('click',()=>chatClients.open());
  const taskQueues=initTaskQueues({post,getUser,getActive:()=>instancePageActive});
  const taskCategory=document.querySelector('#admin-dialog [data-settings-category=tasks]');
  taskCategory.addEventListener('click',()=>taskQueues.open());
  const aiCategory=document.querySelector('#admin-dialog [data-settings-category=ai]');
  const connectionsCategory=document.querySelector('#admin-dialog [data-settings-category=connections]');
  const repositoriesCategory=document.querySelector('#admin-dialog [data-settings-category=repositories]');
  const repositoryImport=initRepositoryImport({post,getUser,getActive:()=>instancePageActive,reloadProjects});
  aiCategory.addEventListener('click',()=>ai.open());
  const manages = project => !!project && (getUser()?.admin || project.role === 'owner');

  async function saveMember(projectID, id, role) {
    await post('/api/members', {project:projectID, user:id, role});
    await reloadProjects();
    if (getProject()?.id !== projectID || !manages(getProject())) {
      closeSettingsPage($('settings-dialog'));
      return;
    }
    await openProject();
    feedback($('member-status'),'Notebook access saved.',{level:'success'});
  }
  async function openProject() {
    const selected = getProject();
    if (!selected) return;
    const page=prepareSettingsPage($('settings-dialog'));
    memberProject = null;
    searchVersion++;
    clearTimeout(searchTimer);
    $('settings-project').textContent = selected.name;
    const sharing=document.querySelector('#settings-dialog [data-settings-category=sharing]');
    sharing.hidden=!manages(selected) || (!!selected.protectedInbox || !!selected.assistantOwner);
    openWikiSettings(selected,manages(selected));
    if(sharing.hidden && sharing.getAttribute('aria-pressed')==='true')document.querySelector('#settings-dialog [data-settings-category=search]').click();
    const sources=document.querySelector('#settings-dialog [data-settings-category=sources]');
    sources.hidden=!moduleOn(getUser(),'repositories');
    if(sources.hidden && sources.getAttribute('aria-pressed')==='true')document.querySelector('#settings-dialog [data-settings-category=search]').click();
    $('project-members').hidden = true;
    $('member-list').replaceChildren();
    organization.clearSharing();
    $('member-results').replaceChildren();
    $('member-search').value = '';
    $('member-status').textContent = '';
    $('member-form').reset();
    $('member-picker').hidePopover();
    $('member-selected').textContent = 'Select a person';
    $('member-submit').disabled = true;
    $('backup-link').href = notebookPath(selected.id) + '/api/backup';
    if (manages(selected)) {
      const data = await api('/api/admin');
      if (getProject()?.id !== selected.id || !page.active()) return;
      const project = data.projects.find(p => p.id === selected.id);
      if (project) {
        memberProject = project.id;
        $('project-members').hidden = false;
        const people = new Map(data.users.map(u => [u.id,u]));
        for (const [id, role] of Object.entries(project.members || {})) {
          const person = people.get(id) || {id, admin:id === '@local-admin'};
          const row = element('div', undefined, 'person-row');
          const label = identity(nameOf(person,data.usernames), id);
          const access=project.effectiveMembers?.[id];
          const effective = person.disabled ? 'Disabled' : person.admin ? 'Admin · all notebooks' : roles[access?.role || role]+(access?.groups?.length?' · group access also applies':'');
          label.append(element('span', effective, 'role-badge'));
          const select = element('select');
          select.setAttribute('aria-label', 'Role for ' + nameOf(person,data.usernames));
          for (const [value,text] of Object.entries({...roles, '':'Remove direct access'})) {
            if(person.kind==='service' && value==='owner')continue;
            const option = element('option',text); option.value=value; select.append(option);
          }
          select.value = role;
          const picker = element('div',undefined,'select-picker'); picker.append(select);
          // Save is available only while the chosen role differs from the saved one.
          const button = element('button','Save','primary'); button.type='button'; button.disabled=true;
          let saved = role;
          select.onchange = () => { button.disabled = select.value===saved; };
          button.onclick = async () => {
            button.disabled = true;
            const value = select.value;
            try { await saveMember(project.id,id,value); saved=value; }
            catch(error) { feedback($('member-status'),error.message); }
            finally { button.disabled=select.value===saved; }
          };
          row.append(label,picker,button);
          $('member-list').append(row);
        }
        if(!selected.protectedInbox)await organization.sharing(project,data,page.active);
      }
    }
    const authority=await openAuthoritySettings();
    if(getProject()?.id!==selected.id||!page.active())return;
    if(authority?.changing){
      document.querySelector('#settings-dialog [data-settings-category=authority]').click();
      page.show();
      return;
    }
    await openRetrievalSettings();
    if (getProject()?.id !== selected.id || !page.active()) return;
    page.show();
    if(!sources.hidden)await openRepositorySettings();
    if(getProject()?.id===selected.id)await openBackupSettings();
  }
  $('settings-open').onclick = () => openProject().catch(error => notice(error.message));
  $('member-search').oninput = () => {
    clearTimeout(searchTimer);
    const version = ++searchVersion, project = memberProject, query = $('member-search').value.trim();
    $('member-user').value=''; $('member-selected').textContent='Select a person'; $('member-submit').disabled=true; $('member-results').replaceChildren();
    if ([...query].length < 2 || !project) { $('member-results').textContent='Type at least 2 characters to find someone.'; return; }
    searchTimer = setTimeout(async () => {
      try {
        const people = await api('/api/people?project='+encodeURIComponent(project)+'&q='+encodeURIComponent(query));
        if (version !== searchVersion || project !== memberProject || getProject()?.id !== project) return;
        for (const person of people) {
          const button=element('button',undefined,'quiet'); button.type='button';
          button.append(identity(person.name,person.id));
          button.onclick=()=>{
            $('member-user').value=person.id; $('member-selected').textContent=person.name;
            $('member-submit').disabled=false; $('member-picker').hidePopover(); $('member-role').focus();
          };
          $('member-results').append(button);
        }
        if (!people.length) $('member-results').textContent='No matching people.';
      } catch(error) { if(version===searchVersion) $('member-results').textContent=error.message; }
      finally { if(version===searchVersion) positionPicker(); }
    },250);
  };
  const picker = $('member-picker');
  function positionPicker() {
    if (!picker.matches(':popover-open')) return;
    const anchor = $('member-choose').getBoundingClientRect();
    const menu = picker.getBoundingClientRect();
    const position = contextMenuPosition({top:anchor.top,bottom:anchor.bottom,right:anchor.left+menu.width},menu,{width:innerWidth,height:innerHeight});
    picker.style.left=position.left+'px';picker.style.top=position.top+'px';
  }
  picker.addEventListener('toggle', () => {
    const open = picker.matches(':popover-open');
    $('member-choose').setAttribute('aria-expanded',String(open));
    if (!open) {searchVersion++;clearTimeout(searchTimer);return;}
    positionPicker();
    $('member-search').focus();$('member-search').oninput();
  });
  picker.addEventListener('keydown',event=>{
    if (!['ArrowDown','ArrowUp','Home','End'].includes(event.key)) return;
    const options=[...$('member-results').querySelectorAll('button')];
    if (!options.length || (document.activeElement===$('member-search') && ['Home','End'].includes(event.key))) return;
    event.preventDefault();
    const i=options.indexOf(document.activeElement);
    const next=event.key==='Home'?0:event.key==='End'?options.length-1:i<0?(event.key==='ArrowUp'?options.length-1:0):(i+(event.key==='ArrowUp'?-1:1)+options.length)%options.length;
    options[next].focus();
  });
  window.addEventListener('resize',positionPicker);
  $('member-form').onsubmit = event => {
    event.preventDefault();
    if (!memberProject || !$('member-user').value) return;
    return busy($('member-form'), () => saveMember(memberProject,$('member-user').value,$('member-role').value), {controls:event.submitter, report:$('member-status')});
  };

  async function openWorkspace(id) {
    closeSettingsPage($('admin-dialog'));
    await selectProject(id);
    await openProject();
  }
  function selectUser(id) {
    selectedUser=id;
    const person=directory?.users.find(u=>u.id===id);
    $('user-details').hidden=!person; $('user-empty').hidden=!!person;
    if (!person) return;
    $('user-heading').textContent=nameOf(person,directory.usernames);
    $('user-identity').textContent=person.id;
    $('user-id').value=id;
    $('user-display-name').value=person.displayName || '';
    $('user-admin').checked=person.admin;
    $('user-disabled').checked=person.disabled;
    $('user-create-workspaces').checked=!!person.canCreateProjects;
    $('local-reset').hidden=!getUser().passwordAvailable || !directory.usernames?.[id];
    $('local-reset-user').value=id;
    organization.person(id).catch(error=>feedback($('admin-status'),error.message));
    $('user-workspaces').replaceChildren();
    for (const project of directory.projects) {
      const role=person.admin ? 'Admin' : roles[project.effectiveMembers?.[id]?.role || project.members?.[id]];
      if (!role) continue;
      const button=element('button',project.name+' · '+(person.disabled ? 'Disabled ('+role+')' : role),'quiet');
      button.type='button'; button.onclick=()=>openWorkspace(project.id).catch(e=>notice(e.message));
      $('user-workspaces').append(button);
    }
    if (!$('user-workspaces').childElementCount) $('user-workspaces').textContent='No notebook access.';
    renderUsers();
  }
  function renderUsers() {
    $('access-list').replaceChildren();
    const query=$('user-search').value.trim().toLowerCase();
    for (const person of directory.users) {
      if(person.kind==='service')continue;
      const name=nameOf(person,directory.usernames);
      if (query && ![name,person.id,directory.usernames?.[person.id] || ''].join(' ').toLowerCase().includes(query)) continue;
      const button=element('button',undefined,'quiet'); button.type='button';
      button.setAttribute('aria-pressed',String(person.id===selectedUser));
      button.append(identity(name,person.id),element('span',person.disabled?'Disabled':person.admin?'Admin':person.canCreateProjects?'Notebook creator':'Member','role-badge'));
      button.onclick=()=>selectUser(person.id); $('access-list').append(button);
    }
    if (!$('access-list').childElementCount) $('access-list').textContent='No matching people.';
  }
  $('user-search').oninput=()=>{if(directory) renderUsers();};
  async function openInstance(preservePerson=()=>false) {
    const page=prepareSettingsPage($('admin-dialog'));instancePageActive=page.active;
    try {
    await reloadProjects();if(!page.active())return;
    if (!getUser().admin) { closeSettingsPage($('admin-dialog')); return; }
    for(const [category,module] of [[connectionsCategory,'data-connections'],[repositoriesCategory,'repositories'],[chatCategory,'messaging'],[taskCategory,'tasks']]){
      category.hidden=!moduleOn(getUser(),module);
      if(category.hidden && category.getAttribute('aria-pressed')==='true')document.querySelector('#admin-dialog [data-settings-category=people]').click();
    }
    const data=await api('/api/admin');if(!page.active())return;directory=data;repositoryImport.open(data.instance);
    await organization.load(directory,page.active);if(!page.active())return;
    if(aiCategory.getAttribute('aria-pressed')==='true')await ai.open();
    if(chatCategory.getAttribute('aria-pressed')==='true')await chatClients.open();
    if(taskCategory.getAttribute('aria-pressed')==='true')await taskQueues.open();
    if(!page.active())return;

    $('managed-workspaces').replaceChildren();
    for(const project of directory.projects) {
      const button=element('button',project.name,'quiet'); button.type='button';
      const count=Object.keys(project.members || {}).length;
      button.append(element('span',`${count} ${count===1?'person':'people'}`,'small'));
      button.onclick=()=>openWorkspace(project.id).catch(e=>notice(e.message));
      $('managed-workspaces').append(button);
    }
    if (!directory.projects.length) $('managed-workspaces').textContent='No owned notebooks yet.';
    if(getUser().admin) {renderUsers();if(!preservePerson())selectUser(selectedUser);}
    page.show();return page.active;
    }catch(error){if(page.active()){closeSettingsPage($('admin-dialog'));notice(error.message);}}
  }
  $('admin-open').onclick=()=>openInstance().catch(error=>notice(error.message));
  $('user-revoke').onclick=async()=>{
    const active=instancePageActive,selected=selectedUser;if(!selected||!active())return;
    try {await post('/account/sessions/revoke',{user:selected});if(active()&&selectedUser===selected)feedback($('admin-status'),'Browser and AI sessions revoked.',{level:'success'});}
    catch(error){if(active()&&selectedUser===selected)feedback($('admin-status'),error.message);}
  };
  const userEdit=id=>({id,displayName:$('user-display-name').value.trim(),admin:$('user-admin').checked,disabled:$('user-disabled').checked,canCreateProjects:$('user-create-workspaces').checked});
  $('user-form').onsubmit=event=>{
    event.preventDefault();let active=instancePageActive;const selected=selectedUser;if(!selected||!active())return;
    $('admin-status').textContent='';
    const saved=text=>feedback($('admin-status'),text,{level:'success'});
    return busy($('user-form'),async()=>{
      const submitted=userEdit(selected);await post('/api/users',submitted);
      if(!active()||selectedUser!==selected)return;
      const changed=()=>selectedUser!==selected||JSON.stringify(userEdit(selected))!==JSON.stringify(submitted);
      const message=()=>changed()?'Account changes saved. Your newer changes are still unsaved.':'Account changes saved.';
      if(changed()){saved(message());return;}
      const next=await openInstance(changed);if(!next)return;active=next;
      if(active()&&selectedUser===selected)saved(message());
    },{controls:event.submitter,report:$('admin-status'),current:()=>active()&&selectedUser===selected});
  };
  function notebookMode(value) {
    if (notebookBusy()) return;
    notebookVersion++;
    connecting = value==='repository'; importing = value===true; repositoryGrant='';
    $('notebook-repository-fields').hidden=!connecting;$('notebook-repository-selection').hidden=true;$('notebook-repository-mode').setAttribute('aria-pressed',String(connecting));
    $('project-id').readOnly=$('project-name').readOnly=false;
    const title = importing ? 'Import notebook' : connecting ? 'Connect GitHub repository' : 'Create notebook';
    $('workspace-create-title').textContent = title;
    $('notebook-submit').textContent = title;
    $('notebook-submit').disabled = false;
    $('notebook-bundle-fields').hidden = !importing;
    $('notebook-bundle').disabled = !importing;
    $('notebook-bundle').required = importing;
    $('notebook-create-mode').setAttribute('aria-pressed', String(!importing&&!connecting));
    $('notebook-import-mode').setAttribute('aria-pressed', String(importing));
    $('workspace-create-status').textContent = '';
  }
  function createWorkspace(importBundle = false) {
    if (!getUser()?.canCreateProjects) return;
    closeSettingsPage($('admin-dialog'));
    $('notebook-repository-mode').hidden=!moduleOn(getUser(),'repositories');
    notebookMode(importBundle);
    openDialog($('workspace-create-dialog'));
    $('project-name').focus();
  }
  async function resumeRepository(id){
    if (!getUser()?.canCreateProjects) return;
    createWorkspace();notebookMode('repository');repositoryGrant=id;
    const version=notebookVersion;
    $('notebook-submit').disabled=true;
    feedback($('workspace-create-status'),'Loading repositories…',{level:'info'});
    try{const result=await api('/api/projects/repository/choices?'+new URLSearchParams({project:id}));
      if(version!==notebookVersion||!$('workspace-create-dialog').open)return;
      $('project-id').value=result.project;$('project-name').value=result.name;$('project-id').readOnly=$('project-name').readOnly=true;
      $('notebook-repository-selection').hidden=false;$('notebook-repository-choice').replaceChildren(...result.choices.map(choice=>new Option(choice.full_name+(choice.private?' (private)':''),JSON.stringify({repository:choice.id,installation:choice.installation}))));
      $('notebook-repository-install').href=result.installUrl;$('notebook-submit').textContent='Create repository notebook';$('notebook-submit').disabled=!result.choices.length;
      feedback($('workspace-create-status'),result.choices.length?'Choose a repository and confirm sharing.':'No repositories are available. Install the App, wait for any organization approval, then restart the connection.',{level:'info'});
    }catch(error){if(version!==notebookVersion||!$('workspace-create-dialog').open)return;repositoryGrant='';$('notebook-submit').disabled=false;feedback($('workspace-create-status'),error.message+' Restart the connection to continue.');}
  }
  $('workspace-create-open').onclick=()=>createWorkspace();
  $('workspace-import-open').onclick=()=>createWorkspace(true);
  $('notebook-create-mode').onclick=()=>notebookMode(false);
  $('notebook-repository-mode').onclick=()=>notebookMode('repository');
  $('notebook-import-mode').onclick=()=>notebookMode(true);
  $('notebook-bundle').onchange=()=>{if(!notebookBusy())$('workspace-create-status').textContent='';};
  $('workspace-create-dialog').addEventListener('cancel', event=>{if(notebookBusy())event.preventDefault();});
  $('workspace-create-dialog').addEventListener('close',()=>{notebookVersion++;});
  const forgetSuggestedID=followName($('project-name'),$('project-id'));
  $('project-form').onsubmit=event=>{
    event.preventDefault();
    if(notebookBusy())return;
    const bundle=$('notebook-bundle').files[0];
    if(importing && (!bundle || bundle.size > 32*1024*1024)) {
      feedback($('workspace-create-status'),'Choose a Git bundle up to 32 MiB.');
      return;
    }
    const id=$('project-id').value.trim();
    const name=$('project-name').value.trim();
    const controls=[...$('project-form').querySelectorAll('input, button, select'),$('notebook-create-mode'),$('notebook-import-mode'),$('notebook-repository-mode'),$('notebook-dialog-close')];
    feedback($('workspace-create-status'),importing?'Uploading and importing your notebook…':'Creating your notebook…',{level:'info'});
    return busy($('project-form'),async()=>{
      if(connecting){
        if(!repositoryGrant){const result=await post('/api/projects/repository/connect',{id,name});location.assign(result.url);return;}
        if(!$('notebook-repository-share').checked)throw Error('Confirm sharing source files with notebook members.');
        await post('/api/projects/repository/finish?'+new URLSearchParams({project:repositoryGrant}),{...JSON.parse($('notebook-repository-choice').value),shareWithNotebook:true});
        const url=new URL(location.href);url.searchParams.delete('repository');url.searchParams.delete('notebook');history.replaceState(history.state,'',url);
      } else if(importing) {
        const form=new FormData();form.append('id',id);form.append('name',name);form.append('bundle',bundle);
        await api('/api/projects/import',{method:'POST',body:form});
      } else await post('/api/projects',{id,name});
      $('project-form').reset();forgetSuggestedID();$('workspace-create-dialog').close();
      try {
        await reloadProjects();
        if(connecting){await selectProject(id);notice('Repository connected. Files and the local index will appear as the first download finishes.',true);}
        else if(importing){await selectProject(id);notice('Notebook imported. Your notes and history are ready.',true);}
        else await openWorkspace(id);
      } catch(error) {notice('Notebook saved, but the view could not refresh. Reload the page. '+error.message);}
    },{controls,report:$('workspace-create-status')});
  };
  $('local-reset-form').onsubmit=event=>{
    event.preventDefault();
    return busy($('local-reset-form'),()=>organization.reset($('local-reset-user').value),{controls:event.submitter,report:$('admin-status')});
  };
  return {createWorkspace,openProject,openInstance,resumeRepository,closed:organization.closed,checkAuthority:organization.checkAuthority};
}
