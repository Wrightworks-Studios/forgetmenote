import {openDialog,overlays} from './overlay.mjs';
import {element,button,field,closeButton,installHTML} from './dom.mjs';
import {icon} from './icons.mjs';
import {reconcileNodes} from './keyed-list.mjs';
import {createTranscriptView} from './chat-transcript.mjs';
import {createRunView} from './chat-runs.mjs';
import {requestSequence,poll} from './state.mjs';
// Chat owns its private transcript and composer; app.mjs owns all editor drafts.
import {activeStates,validCorpus,applyChatEvent,chatCitation} from './chat-model.mjs';
import {createComposerDrafts} from './chat-composer.mjs';
import {openAttachmentPicker,openTranscriptSave} from './chat-dialogs.mjs';
import {notebookPath} from './request.mjs';
export {applyChatEvent,createTurnSubmission,attachmentSnapshot,chatCitation} from './chat-model.mjs';
function iconButton(label,name,action,attrs = {}) {
  const control = button('',action,{class:'quiet chat-icon','aria-label':label,title:label,...attrs});control.append(icon(name));return control;
}

export function initChat({api,post,getProject,getUser,getNote,getDraftAttachment,canEdit,canManage,openCitation,openDocument,openReview,openSource,openJudgmentSettings,mountCitationChecks,onOpen = () => {},onClose = () => {}}) {
  const panel = document.getElementById('chat-panel'), opener = document.getElementById('chat-open');
  const openerHome=opener.parentElement;
  // Keep the labeled control reachable inside the active contextual surface.
  opener.replaceChildren(icon('chat'),document.createTextNode('Ask notebook'));opener.setAttribute('aria-label','Ask notebook');opener.title='Ask notebook';
  let projectID = '', userID = '', initialized = false, controller = new AbortController(), stream = null, timer = null;
  let corpusDefaults = null, corpusDefaultsError = '', corpusDefaultsLoading = false;
  let config = null, conversations = [], chat = null, busy = false, denied = false, draftKey = '', newIntent = false;
  const lifetime=requestSequence(),conversationRequests=requestSequence(),defaultRequests=requestSequence(),listRequests=requestSequence(),configurationRequests=requestSequence();
  const failedDecisions = new Map(), retryableDecisions = new Set(), mobile = matchMedia('(max-width: 1279px)');
  let composerRevision = 0;
  const drafts=createComposerDrafts();let composer=drafts.select(''),pendingDeletion=null;
  const header = element('div',undefined,{class:'chat-heading'});
  const title = element('h2','Ask notebook',{id:'chat-title'});
  const settingsButton = iconButton('Chat settings','settings',openSettings,{id:'chat-settings-open'});
  header.append(title,iconButton('New chat','new',() => {newIntent = true;selectChat(null);},{id:'chat-new'}),settingsButton);
  const scope = element('p','',{id:'chat-scope',class:'small'});
  const select = element('select',undefined,{id:'chat-conversations','aria-label':'Your conversations'});
  const reloadButton = iconButton('Refresh','refresh',() => refresh({reviewDecisions:true}).catch(showError),{id:'chat-refresh'});
  const picker = element('div',undefined,{class:'select-picker'});picker.append(select);
  const deleteButton = iconButton('Delete conversation','delete',deleteDialog,{id:'chat-delete'});
  const listRow = element('div',undefined,{class:'chat-row'});listRow.append(picker,deleteButton,reloadButton);
  const message = element('p','',{id:'chat-message',role:'status'});
  const setup = element('a','Set up chat',{id:'chat-setup',class:'button primary'});setup.hidden = true;
  const settings = element('dialog',undefined,{id:'chat-settings',class:'chat-dialog','aria-label':'Chat settings'});
  const settingsHeading = element('div',undefined,{class:'chat-heading'});
  settingsHeading.append(element('h2','Chat settings'),closeButton(() => settings.close()));settings.append(settingsHeading);
  settings.addEventListener('close',() => {if (active()) settingsButton.focus();});
  const toolbar = element('div',undefined,{class:'chat-toolbar'});toolbar.append(listRow);
  const error = element('p','',{id:'chat-error',role:'alert'});error.hidden = true;
  const connection = element('select',undefined,{id:'chat-connection'}), model = element('select',undefined,{id:'chat-model'});
  const mode = element('select',undefined,{id:'chat-mode'});
  for (const [value,label] of [['ask','Ask'],['suggest','Suggest'],['edit','Edit']]) mode.append(element('option',label,{value}));
  const choices = element('div',undefined,{class:'chat-choices'});
  const connectionLabel = field('Connection',connection), modelLabel = field('Model',model);
  choices.append(connectionLabel,modelLabel);
  const corpusFields = element('fieldset');corpusFields.append(element('legend','Notebook context'));
  const archives = element('select',undefined,{id:'chat-archives'}), sharedDrafts = element('select',undefined,{id:'chat-drafts'});
  for (const control of [archives,sharedDrafts]) {
    control.append(element('option','Choose…',{value:'',disabled:''}));
    for (const [value,label] of [['include','Include'],['lower','Lower priority'],['exclude','Exclude']]) control.append(element('option',label,{value}));
  }
  const history = element('input',undefined,{type:'checkbox',id:'chat-history'});
  const noteProjects=element('select',undefined,{multiple:true,size:3,'aria-label':'Additional notebooks for chat'});
  const sourceProjects=element('select',undefined,{multiple:true,size:3,'aria-label':'Repository notebooks for chat'});
  const corpusStatus = element('p','',{class:'small',role:'status'});
  corpusFields.append(field('Additional notes notebooks',noteProjects),field('Repository sources (optional)',sourceProjects),element('p','Selected excerpts go to your chat provider.',{class:'small'}),field('Archives',archives),field('Drafts',sharedDrafts),field('Include previous versions',history),element('p','Applies to new messages; Continue keeps its original settings.',{class:'small'}),corpusStatus);
  const controls = element('details',undefined,{id:'chat-options'});
  controls.append(element('summary','Advanced'));
  const scopes = element('fieldset');scopes.append(element('legend','Allow edits to'));
  const scopeInputs = new Map();
  for (const [value,label] of [['notes','Committed notes'],['live','Shared live notes'],['comments','Discussions'],['review','Review findings and proposals']]) {
    const input = element('input',undefined,{type:'checkbox',value});
    scopeInputs.set(value,input);scopes.append(field(label,input));
  }
  const research = element('input',undefined,{type:'checkbox',id:'chat-research'}), researchLabel = field('External research',research);
  const review = element('input',undefined,{type:'checkbox',id:'chat-review'});
  const purpose = element('textarea',undefined,{id:'chat-review-purpose',rows:2,placeholder:'What should this notebook review examine?'});
  const purposeLabel = field('Review purpose',purpose);purposeLabel.hidden = true;
  controls.append(researchLabel,field('Review the notebook',review),purposeLabel);
  const transcript = element('div',undefined,{id:'chat-transcript',tabindex:0,'aria-label':'Conversation transcript'});
  const older = button('Load earlier messages',() => loadEarlier().catch(showError),{id:'chat-older',class:'quiet'});older.hidden = true;
  const turns = element('div',undefined,{id:'chat-turns'});transcript.append(older,turns);
  const runPanel = element('div',undefined,{id:'chat-runs'});
  const form = element('form',undefined,{id:'chat-form'});
  const attachments = element('div',undefined,{id:'chat-attachments'});
  const input = element('textarea',undefined,{id:'chat-input',rows:3,placeholder:'Ask about this notebook…','aria-label':'Message'});
  const send = element('button','Send',{type:'submit',id:'chat-send',class:'primary'});
  const attach = iconButton('Attach note','attach',attachmentDialog,{id:'chat-attach'});
  const stop = button('Stop',() => {const run = activeRun();if (run) controlRun(run,'cancel');},{id:'chat-stop',class:'quiet'});stop.hidden = true;
  const composerActions = element('div',undefined,{class:'chat-row chat-composer-actions'});composerActions.append(attach,send,stop);
  const context=element('div',undefined,{class:'chat-composer-context'});context.append(scope,field('Mode',mode));form.append(context,attachments,input,composerActions);
  const body = element('div',undefined,{id:'chat-body'});
  const settingsBody = element('div',undefined,{class:'chat-settings-body'});
  settingsBody.append(choices,corpusFields,scopes,controls);
  if(openJudgmentSettings)settingsBody.append(button('Evidence checks',()=>{settings.close();openJudgmentSettings();},{class:'quiet'}));
  settings.append(settingsBody);
  body.append(toolbar,message,setup,error,transcript,runPanel);
  panel.append(header,body,form,settings);

  function renderContext() {const epoch=lifetime.current(),id=chat?.id;return {project:projectID,signal:controller.signal,active:()=>valid(epoch)&&chat?.id===id};}
  const transcriptView=createTranscriptView({viewport:transcript,context:renderContext,post,messageActions,sourceDetails,installAnswer,showError});
  const runView=createRunView({runPanel,context:renderContext,operationCard,sourceDetails,controlRun,mountCitationChecks});

  function openSettings() {if (!settings.open) openDialog(settings);}
  function closeDialogs({preserveDeletion = false} = {}) {
    if (settings.open) settings.close();
    for (const dialog of panel.querySelectorAll('dialog:not(#chat-settings)')) {
      if (dialog === pendingDeletion?.dialog) {if (!preserveDeletion) pendingDeletion.dismiss();}
      else {dialog.close();dialog.remove();}
    }
  }

  function url(suffix = '') { return notebookPath(projectID) + '/api/chat' + (chat ? '/' + encodeURIComponent(chat.id) : '') + suffix; }
  function active() { return !panel.hidden; }
  // Only presentation identifiers survive reload; transcript and composer text
  // remain server-side and in memory respectively, scoped to the current account.
  const viewKey = 'gitnotes:chat-view';
  function rememberView() {
    try {
      if (projectID && userID && !denied) sessionStorage.setItem(viewKey,JSON.stringify({user:userID,project:projectID,open:active(),conversation:chat?.id || '',newChat:newIntent}));
      else sessionStorage.removeItem(viewKey);
    } catch {}
  }
  function restoredView() {
    try {
      const value = JSON.parse(sessionStorage.getItem(viewKey));
      sessionStorage.removeItem(viewKey);
      if (value?.user === userID && value.project === projectID) return value;
    } catch {}
    return null;
  }
  function valid(epoch) { return lifetime.owns(epoch) && projectID === getProject()?.id && userID === getUser()?.id && !denied; }
  function stopStream() { stream?.close();stream = null;timer?.stop();timer = null; }
  function resetRequests() { lifetime.next();conversationRequests.next();defaultRequests.next();listRequests.next();configurationRequests.next();corpusDefaultsLoading = false;runView.reset();transcriptView.reset();failedDecisions.clear();retryableDecisions.clear();controller.abort();controller = new AbortController();stopStream(); }
  function rememberComposer() { composer.text = input.value;drafts.remember(chat?.id || draftKey,composer); }
  function clearSensitive() {
    resetRequests();denied = true;chat = null;draftKey = '';conversations = [];drafts.clear();composer=drafts.select('');
    rememberView();input.value = '';purpose.value = '';turns.replaceChildren();runPanel.replaceChildren();attachments.replaceChildren();select.replaceChildren();
    closeDialogs();updateControls();
  }
  function showError(problem) {
    if (problem?.name === 'AbortError') return;
    if (problem?.status === 401 || problem?.status === 403) clearSensitive();
    error.textContent = problem?.message || 'Chat request failed. Try again.';error.hidden = false;
  }
  function clearError() { error.textContent = '';error.hidden = true; }
  function selectedConnection() { return config?.connections?.find(item => item.id === connection.value); }
  function chooseModels(wanted) {
    const item = selectedConnection();model.replaceChildren();
    for (const value of item?.models || []) model.append(element('option',value,{value}));
    if (wanted && !(item?.models || []).includes(wanted)) {model.prepend(element('option','Choose a model (previous unavailable)',{value:''}));model.value = '';}
    else if (wanted) model.value = wanted;
    else if (item?.defaultModel) model.value = item.defaultModel;
    updateControls();
  }
  function selectedCorpus() { return {version:1,archives:archives.value,drafts:sharedDrafts.value,history:history.checked,...(noteProjects.selectedOptions.length?{noteProjects:[...noteProjects.selectedOptions].map(o=>o.value)}:{}),...(sourceProjects.selectedOptions.length?{sourceProjects:[...sourceProjects.selectedOptions].map(o=>o.value)}:{})}; }
  function restoreCorpus() {
    const saved = chat?.options ? (chat.options.corpus ?? (chat.version === 2 ? null : corpusDefaults)) : (chat ? null : corpusDefaults);
    const selection = composer.submission.current()?.options.corpus ?? composer.corpus ?? saved;
    archives.value = validCorpus(selection) ? selection.archives : '';sharedDrafts.value = validCorpus(selection) ? selection.drafts : '';history.checked = validCorpus(selection) && selection.history;
    for(const option of sourceProjects.options)option.selected=(selection?.sourceProjects||[]).includes(option.value);
    for(const option of noteProjects.options)option.selected=(selection?.noteProjects||[]).includes(option.value);
    updateCorpusStatus();
  }
  function updateCorpusStatus() {
    corpusStatus.textContent = validCorpus(selectedCorpus()) ? '' : chat?.version === 2 || chat?.options?.corpus ? 'Choose Archives and Drafts to use this conversation again.' : corpusDefaultsError || 'Loading notebook defaults…';
    corpusStatus.hidden = !corpusStatus.textContent;
  }
  function options() {
    const corpus = selectedCorpus();if (!validCorpus(corpus)) throw Error('Choose Archives and Drafts in Chat settings.');
    const result = {connection:connection.value,model:model.value,mode:mode.value,corpus,actions:[...scopeInputs].filter(([,control]) => !control.disabled && control.checked).map(([name]) => name)};
    if (research.checked && !research.disabled) result.research = true;
    if (review.checked) result.review = true;
    return result;
  }
  function activeRun() { return chat?.runs?.find(run => activeStates.has(run.state)); }
  function updateControls() {
    const configured = !!config?.enabled && !!selectedConnection()?.models?.includes(model.value);
    // Without a chat connection only past conversations remain to read: the composer stays in
    // place but visibly off, and no new chat can start.
    const unavailable = !!config && !(config.enabled && config.connections?.length);
    document.getElementById('chat-new').hidden = unavailable;
    picker.hidden = deleteButton.hidden = unavailable && !conversations.length;
    input.placeholder = unavailable ? 'Chat is not set up yet' : 'Ask about this notebook…';
    setup.hidden = !unavailable || !getUser()?.admin;
    if (!setup.hidden) {const target = new URL(location.href);target.searchParams.set('page','administration');target.searchParams.set('category','ai');setup.href = target.pathname + target.search;}
    const running = !!activeRun(), pending = !!composer.submission.current();
    const loadingDefaults = !chat && corpusDefaultsLoading && !pending;
    const blocked = !projectID || denied || busy || pending;
    for (const control of [connection,model,research,review,purpose]) control.disabled = blocked;
    mode.disabled = blocked || unavailable;
    for (const control of [archives,sharedDrafts,history,sourceProjects,noteProjects]) control.disabled = blocked || loadingDefaults;
    mode.querySelector('[value="edit"]').disabled = !canEdit();
    mode.querySelector('[value="suggest"]').disabled = !['comment','edit','owner'].includes(getProject()?.role) && !getUser()?.admin;
    if (mode.selectedOptions[0]?.disabled) mode.value = 'ask';
    scopes.hidden = mode.value !== 'edit';
    for (const [name,control] of scopeInputs) control.disabled = blocked || mode.value !== 'edit' || (name === 'comments' ? !['comment','edit','owner'].includes(getProject()?.role) && !getUser()?.admin : !canEdit());
    researchLabel.hidden = !config?.research || !!config?.localOnly;
    research.disabled = blocked || researchLabel.hidden;
    if (researchLabel.hidden) research.checked = false;
    purposeLabel.hidden = !review.checked;
    connectionLabel.hidden = config?.connections?.length === 1 && !!selectedConnection();
    modelLabel.hidden = selectedConnection()?.models?.length === 1 && configured;
    settingsButton.disabled = !projectID || denied;
    input.disabled = !configured || !projectID || denied || busy || pending;
    const note = getNote();
    attach.disabled = blocked || running || !configured || !note?.path;
    attach.title = note?.path ? 'Choose a version of ' + note.path : 'Open a note to attach a version';
    send.disabled = loadingDefaults || !validCorpus(selectedCorpus()) || !configured || !projectID || denied || busy || (running && !pending);
    send.textContent = busy ? 'Sending…' : pending ? 'Retry send' : 'Send';
    send.hidden = running && !pending;stop.hidden = !running;stop.disabled = busy || denied;
    stop.textContent = activeRun()?.cancelRequested ? 'Stopping…' : 'Stop';
    for (const action of turns.querySelectorAll('[data-message-save]')) action.disabled = !canEdit() || busy || denied;
    document.getElementById('chat-new').disabled = busy || denied || !projectID;
    select.disabled = busy || denied;
    deleteButton.disabled = busy || denied || !chat;
    for (const control of panel.querySelectorAll('#chat-runs button,.chat-run button')) control.disabled = control.dataset.unprovenScope === 'true' || busy || denied || running && control.dataset.runControl === 'resume' || !!control.dataset.confirmation && failedDecisions.get(control.closest('[data-action]').dataset.action) === control.dataset.confirmation;
    older.hidden = !chat?.nextBefore;
  }
  function renderAttachments() {
    attachments.replaceChildren();
    for (const [index,item] of composer.attachments.entries()) {
      const row = element('div',undefined,{class:'chat-attachment'});
      row.append(element('span',`${item.kind === 'private' ? 'Private draft' : item.kind === 'live' ? 'Shared live' : 'Committed'} · ${item.path}`));
      const remove = button('Remove',() => {composer.attachments.splice(index,1);renderAttachments();},{'aria-label':'Remove attachment ' + item.path,class:'quiet'});
      remove.disabled = busy || !!composer.submission.current();row.append(remove);attachments.append(row);
    }
  }
  function citationButton(citation) {
    const target = chatCitation(citation,projectID), label = citation.title || citation.path || citation.url || citation.id || 'Citation';
    if (!target) return element('span',label + ' (citation unavailable)',{class:'small'});
    if(target.kind==='data'){const details=element('details',undefined,{id:target.url.slice(1)});details.append(element('summary',label+' · '+citation.capturedAt),element('pre',target.text));return details;}
    if (target.kind === 'note') return button(label,() => Promise.resolve(openCitation(target.path,target.revision,target.heading)).catch(showError),{class:'quiet chat-citation',title:target.revision});
    if (target.kind === 'document' && openDocument) return button(label,() => Promise.resolve(openDocument(target.ref)).catch(showError),{class:'quiet chat-citation',title:target.ref.project+' · '+target.ref.revision});
    const row = element('span',undefined,{class:'chat-source-citation'});
    row.append(element('a',label,{href:target.url,target:'_blank',rel:'noopener noreferrer'}));
    if (target.kind==='source' && canEdit() && openSource) row.append(button('Archive source',() => openSource(target.url),{class:'quiet'}));
    return row;
  }
  function installAnswer(node, html, turn) {
    // Server-rendered Markdown is sanitized. All note navigation still uses a
    // pinned preview callback, so a response cannot replace the editor draft.
    installHTML(node,html,{
      prepare:root => root.querySelectorAll('img,video,audio,iframe').forEach(media => media.remove()),
      resolve(href) {
        let citation = (turn.citations || []).find(item => item.url === href);
        if (!citation && !/^[a-z]+:/i.test(href)) {
          try {
            const parsed = new URL(href,'https://notebook.invalid/');
            const revision = parsed.searchParams.get('fmn-rev') || parsed.searchParams.get('revision');
            const path = parsed.searchParams.get('note') || decodeURIComponent(parsed.pathname.replace(/^\//,''));
            citation = {path,revision,project:parsed.searchParams.get('project') || projectID,heading:decodeURIComponent(parsed.hash.slice(1))};
          } catch {}
        }
        const target = chatCitation(citation || {url:href},projectID);
        return target && (target.kind === 'data' || target.kind === 'note' || target.kind === 'document' && openDocument ? target : {external:true,url:target.url});
      },
      follow(anchor, target) {
        if(target.kind==='data')return ()=>{const details=document.getElementById(target.url.slice(1));if(details){details.open=true;details.scrollIntoView({block:'nearest'});}};
        if (target.kind === 'note') {
          anchor.href = '/?project=' + encodeURIComponent(projectID) + '&note=' + encodeURIComponent(target.path) + '&revision=' + target.revision;
          return () => Promise.resolve(openCitation(target.path,target.revision,target.heading)).catch(showError);
        }
        anchor.href=target.url;
        return () => Promise.resolve(openDocument(target.ref)).catch(showError);
      },
    });
  }
  function messageActions(turn) {
    const group = element('div',undefined,{class:'chat-message-actions'});
    const menu = element('div',undefined,{class:'chat-message-menu',role:'group','aria-label':'Message actions'});menu.hidden = true;
    const trigger = iconButton('Message actions','more',() => {
      const open = menu.hidden;
      closeMessageMenus();menu.hidden = !open;trigger.setAttribute('aria-expanded',String(open));
      if (open) menu.querySelector('button:not(:disabled)')?.focus();
    },{'aria-expanded':'false'});
    menu.append(button('Save to note',() => {menu.hidden = true;trigger.setAttribute('aria-expanded','false');saveDialog(turn.id,trigger);},{'data-message-save':turn.id}));
    group.addEventListener('keydown',event => {
      if (event.key === 'Escape' && !menu.hidden) {event.preventDefault();event.stopPropagation();menu.hidden = true;trigger.setAttribute('aria-expanded','false');trigger.focus();}
    });
    group.append(trigger,menu);return group;
  }
  function closeMessageMenus() {
    for (const group of turns.querySelectorAll('.chat-message-actions')) {group.querySelector('.chat-message-menu').hidden = true;group.querySelector('button').setAttribute('aria-expanded','false');}
  }
  function sourceDetails(citations, open = false) {
    const unique = new Map();
    for (const citation of citations) {
      const key = JSON.stringify(chatCitation(citation,projectID) || citation);
      if (!unique.has(key)) unique.set(key,citation);
    }
    if (!unique.size) return null;
    const details = element('details',undefined,{class:'chat-sources'});details.open = open;
    details.append(element('summary',`Sources · ${unique.size}`));
    const list = element('div',undefined,{class:'chat-citations'});
    for (const citation of unique.values()) list.append(citationButton(citation));
    details.append(list);return details;
  }
  function operationCard(operation, run) {
    const card = element('section',undefined,{class:'chat-operation','data-action':operation.id});
    card.append(element('strong',operation.name),element('span',' · ' + (operation.state || 'prepared')));
    const details = element('details');details.append(element('summary','Arguments and result'));
    details.append(element('pre',JSON.stringify({arguments:operation.arguments,result:operation.result},null,2)));card.append(details);
    if (operation.error) card.append(element('p',operation.error,{role:'alert'}));
    if (/propos|finding/.test(operation.name) && openReview) card.append(button('Open Review',() => openReview(),{class:'quiet'}));
    if (operation.confirmation && operation.state === 'awaiting_confirmation') {
      details.open = true;
      card.append(element('p','Approval applies to this action only.'));
      if (operation.name === 'empty_trash') card.append(element('p',`Empty Trash affects ${operation.result?.count ?? operation.result?.entries?.length ?? 'the listed'} items. Old Git history and backups remain. A changed Trash manifest requires a new explicit review.`));
      const decisions = element('div',undefined,{class:'chat-approval-actions'});
      for (const decision of ['approve','decline']) {
        const action = button(decision === 'approve' ? 'Approve action' : 'Decline',() => decide(operation,decision,card),{class:decision === 'approve' ? '' : 'quiet'});
        action.dataset.confirmation = operation.confirmation;action.dataset.unprovenScope = String(!validCorpus(run.options?.corpus));action.disabled = !validCorpus(run.options?.corpus) || busy || failedDecisions.get(operation.id) === operation.confirmation;decisions.append(action);
      }
      card.append(decisions);
    }
    if (operation.state === 'uncertain') card.append(element('p','Inspect the result in the notebook before continuing. This action cannot safely be replayed automatically.'));
    return card;
  }
  function render() {
    const nearBottom = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 90;
    const messages=transcriptView.render(chat),runs=runView.render(chat),last=new Map(),ordered=[];
    for(const row of messages)if(row.dataset.runId)last.set(row.dataset.runId,row);
    const pending=new Map(runs.map(row=>[row.dataset.run,row]));
    for(const row of messages) {
      ordered.push(row);
      if(last.get(row.dataset.runId)===row&&pending.has(row.dataset.runId)) {ordered.push(pending.get(row.dataset.runId));pending.delete(row.dataset.runId);}
    }
    reconcileNodes(turns,[...ordered,...pending.values()]);renderAttachments();updateControls();
    if (nearBottom) transcript.scrollTop = transcript.scrollHeight;

  }
  function renderList() {
    select.replaceChildren(element('option','New conversation',{value:''}));
    for (const item of conversations) select.append(element('option',item.title || 'Untitled chat',{value:item.id}));
    for (const [key,draft] of drafts.entries()) if (key.startsWith('draft:') && (draft.text || draft.attachments.length)) select.append(element('option','Unsent draft (deleted conversation)',{value:key}));
    select.value = chat?.id || draftKey;
  }
  async function refreshList() {
    const epoch=lifetime.current(),owns=listRequests.begin(()=>valid(epoch));
    let result;
    try {result=await api(notebookPath(projectID)+'/api/chat',{signal:controller.signal});}
    catch(problem){if(owns())throw problem;return;}
    if (!owns()) return;
    rememberComposer();
    conversations = result.conversations || [];
    const savedIDs = new Set(conversations.map(item => item.id));
    for (const [id,draft] of [...drafts.entries()]) {
      if (!/^[a-f0-9]{32}$/.test(id) || id === chat?.id || savedIDs.has(id)) continue;
      drafts.retire(id);
    }
    renderList();
  }
  async function loadChat({reconnect = true,reviewDecisions = false} = {}) {
    if (!chat) return;
    const epoch=lifetime.current(),owns=conversationRequests.begin(()=>valid(epoch));
    let result;
    try {result = await api(url(),{signal:controller.signal});}
    catch (problem) {
      if(!owns())return;
      if (owns() && problem.status === 404) {retireConversation(chat.id);message.textContent = 'This conversation was deleted or is unavailable. Any unsent message is kept as a draft.';return;}
      throw problem;
    }
    if (!owns()) return;
    const latestIDs = new Set((result.turns || []).map(turn => turn.id));
    const overlaps = (chat.turns || []).some(turn => latestIDs.has(turn.id));
    const contiguous = Number.isInteger(chat.totalTurns) && chat.totalTurns === result.nextBefore;
    const prior = overlaps || contiguous ? (chat.turns || []).filter(turn => !latestIDs.has(turn.id)) : [];
    chat = {...result,turns:[...prior,...result.turns || []],nextBefore:prior.length ? Math.min(chat.nextBefore ?? result.nextBefore,result.nextBefore) : result.nextBefore};
    if (reviewDecisions) {
      for (const run of result.runs || []) for (const operation of run.operations || []) {
        if (operation.state === 'awaiting_confirmation' && operation.confirmation && failedDecisions.get(operation.id) === operation.confirmation && retryableDecisions.has(operation.id)) {
          failedDecisions.delete(operation.id);retryableDecisions.delete(operation.id);runView.invalidate();
        }
      }
    }
    restoreCorpus();render();
    if (reconnect && active()) connect();
  }
  function connect() {
    stopStream();
    if (!chat || !active() || denied) return;
    const epoch = lifetime.current(), id = chat.id;
    stream = new EventSource(url('/events?after=' + (chat.eventCursor || 0)));
    stream.addEventListener('chat',event => {
      if (!valid(epoch) || chat?.id !== id) return;
      try {
        const envelope = JSON.parse(event.data), next = applyChatEvent(chat,envelope);
        if (next.gap) {loadChat().catch(showError);return;}
        chat = next;render();
        if (envelope.type === 'review') loadChat({reconnect:false}).catch(showError);
        if (envelope.type === 'state' && !activeStates.has(envelope.data?.state)) loadChat().then(()=>{if(valid(epoch))return refreshList();}).catch(problem=>{if(valid(epoch))showError(problem);});
      } catch {message.textContent = 'Refreshing an interrupted stream…';loadChat().catch(showError);}
    });
    stream.onopen = () => {if (valid(epoch)) message.textContent = '';};
    stream.onerror = () => {
      if (!valid(epoch)) return;
      message.textContent = 'Connection interrupted. Reconnecting to saved progress…';
      loadChat({reconnect:false}).catch(showError);
    };
    // Idle streams must also detect revoked notebook access, without submitting
    // another message or granting the run a new budget.
    timer = poll(() => busy ? undefined : loadChat({reconnect:false}).catch(showError),{every:15000,active:()=>valid(epoch)});
  }
  async function loadCorpusDefaults() {
    const epoch=lifetime.current(),owns=defaultRequests.begin(()=>valid(epoch));
    corpusDefaults = null;corpusDefaultsError = '';corpusDefaultsLoading = true;restoreCorpus();updateControls();
    try {
      const result = await api(notebookPath(projectID) + '/api/knowledge/retrieval-settings',{signal:AbortSignal.any([controller.signal,AbortSignal.timeout(10000)])});
      if (!owns()) return;
      const defaults = result?.settings;
      const candidate = defaults?.version === 1 && defaults.ai?.version === 1 ? {...defaults.ai,history:false} : null;
      if (!validCorpus(candidate)) throw Error('Notebook AI defaults are unavailable.');
      corpusDefaults = candidate;
    } catch (problem) {
      if (!owns()) return;
      if ([401,403].includes(problem.status)) throw problem;
      corpusDefaultsError = (problem.name === 'TimeoutError' ? 'Loading notebook defaults timed out.' : problem.message || 'Notebook AI defaults are unavailable.') + ' Choose Archives and Drafts explicitly, or refresh to retry.';
    } finally {
      if (owns()) {corpusDefaultsLoading = false;restoreCorpus();updateControls();}
    }
  }
  async function refresh({reviewDecisions = false} = {}) {
    if (!projectID || !active()) return;
    denied = false;clearError();const epoch=lifetime.current(),owns=configurationRequests.begin(()=>valid(epoch));
    try {
      const [result,notebooks] = await Promise.all([api('/api/chat/connections',{signal:controller.signal}),api('/api/projects',{signal:controller.signal}),loadCorpusDefaults()]);
      if (!owns()) return;
      const selection=selectedCorpus();noteProjects.replaceChildren(...notebooks.filter(p=>p.kind!=='repository'&&p.id!==projectID).map(p=>element('option',p.name,{value:p.id})));sourceProjects.replaceChildren(...notebooks.map(p=>element('option',p.name,{value:p.id})));for(const option of sourceProjects.options)option.selected=(selection.sourceProjects||[]).includes(option.value);restoreCorpus();
      config = result;const previous = connection.value || chat?.options?.connection,previousModel = model.value || chat?.options?.model;connection.replaceChildren();
      for (const item of config.connections || []) connection.append(element('option',item.label,{value:item.id}));
      if (previous && !config.connections?.some(item => item.id === previous)) {connection.prepend(element('option','Choose a connection (previous unavailable)',{value:''}));connection.value = '';}
      else connection.value = previous || config.defaultConnection || config.connections?.[0]?.id || '';
      chooseModels(previousModel);
      message.textContent = config.enabled && config.connections?.length ? '' : 'Chat is not configured. Chat answers questions using notebook content and can propose edits for your approval. An administrator can enable Chat and configure a model connection.';
      await refreshList();
      if (!owns()) return;
      if (chat) {
        if (chat.options) await loadChat({reviewDecisions});
        else await selectChat(conversations.some(item => item.id === chat.id) ? chat.id : conversations[0]?.id || null);
      }
      else if (!newIntent && !draftKey && !composer.text && !composer.attachments.length && !composer.submission.current() && conversations.length) await selectChat(conversations[0].id);
      updateControls();
    } catch (problem) {if(owns())showError(problem);}
  }
  async function selectChat(id) {
    rememberComposer();resetRequests();clearError();busy = false;
    const saved = id && !id.startsWith('draft:');
    draftKey = !saved && id || '';
    newIntent = !saved;
    chat = saved ? {id,turns:[],runs:[]} : null;
    composer=drafts.select(id || '');input.value = composer.text;
    const defaults = saved ? null : loadCorpusDefaults();
    restoreCorpus();rememberView();renderList();render();
    const epoch = lifetime.current();
    if (saved) {
      try {await loadChat();if (!valid(epoch) || !chat) return;connection.value = chat.options.connection;chooseModels(chat.options.model);mode.value = chat.options.mode;updateControls();}
      catch (problem) {if (valid(epoch)) showError(problem);}
    } else {
      try {await defaults;} catch (problem) {if (valid(epoch)) showError(problem);}
    }
  }
  async function loadEarlier() {
    if (!chat?.nextBefore) return;
    const epoch = lifetime.current(),before = chat.nextBefore;older.disabled = true;
    try {
      const result = await api(url('?before=' + before),{signal:controller.signal});
      if (!valid(epoch)) return;
      const ids = new Set(chat.turns.map(turn => turn.id));
      chat.turns = [...(result.turns || []).filter(turn => !ids.has(turn.id)),...chat.turns];chat.nextBefore = result.nextBefore;
      const height = transcript.scrollHeight;render();transcript.scrollTop += transcript.scrollHeight - height;
    } catch(problem){if(valid(epoch))throw problem;}
    finally {if(valid(epoch))older.disabled=false;}
  }
  async function submit(event) {
    event.preventDefault();if (busy || send.disabled) return;
    const epoch = lifetime.current();let body;
    try {
      const pending = composer.submission.current();
      let text = input.value;
      if (!pending && review.checked && purpose.value.trim()) text += '\n\nNotebook review purpose: ' + purpose.value.trim();
      if (!pending && !text.trim()) throw Error('Enter a message or review purpose.');
      body = pending || composer.submission.prepare(text,options(),composer.attachments);
      busy = true;clearError();renderAttachments();updateControls();
      if (!chat) {
        const result = await post(notebookPath(projectID) + '/api/chat',{connection:body.options.connection,model:body.options.model,mode:body.options.mode,corpus:body.options.corpus},{signal:controller.signal});
        if (!valid(epoch)) return;
        chat = result;newIntent = false;rememberView();drafts.discard(draftKey);draftKey = '';drafts.remember(chat.id,composer);conversations.unshift(result);renderList();
      }
      await post(url('/turns'),body,{signal:controller.signal});
      if (!valid(epoch)) return;
      composer.submission.confirmed();composer.text = '';composer.attachments = [];delete composer.corpus;input.value = '';
      await loadChat();await refreshList();
    } catch (problem) {
      if (!valid(epoch)) return;
      // A definite rejection permits correction; an uncertain acknowledgement
      // keeps the exact ID and body for an explicit retry.
      if (problem.status && problem.status < 500) composer.submission.confirmed();
      showError(problem);
    } finally {if (valid(epoch)) {busy = false;renderAttachments();updateControls();if (!input.disabled) input.focus();}}
  }
  async function controlRun(run, action) {
    const epoch = lifetime.current();if (busy) return;
    busy = true;clearError();updateControls();
    try {
      const body = {};
      await post(url('/runs/' + encodeURIComponent(run.id) + '/' + action),body,{signal:controller.signal});
      if (valid(epoch)) await loadChat();
    } catch (problem) {if (valid(epoch)) showError(problem);}
    finally {if (valid(epoch)) {busy = false;render();}}
  }
  async function decide(operation, decision, card) {
    if (busy) return;const epoch = lifetime.current();busy = true;clearError();updateControls();
    card.querySelectorAll('button').forEach(control => control.disabled = true);
    try {
      await post(url('/actions/' + encodeURIComponent(operation.id)),{decision,confirmation:operation.confirmation},{signal:controller.signal});
      if (valid(epoch)) await loadChat();
    } catch (problem) {
      // Do not silently obtain a replacement for a stale destructive token.
      if (valid(epoch)) {
        failedDecisions.set(operation.id,operation.confirmation);
        const transient = !problem.status || problem.status >= 500 || problem.status === 429 || problem.status === 409 && /still pausing|busy|retry shortly/i.test(problem.message);
        if (transient) retryableDecisions.add(operation.id);else retryableDecisions.delete(operation.id);
        showError(problem);card.append(element('p',transient ? 'This decision was not confirmed. Refresh to inspect current state, then choose a decision explicitly.' : 'This confirmation is no longer usable. Inspect current notebook state and prepare a new action explicitly.'));
      }
    } finally {if (valid(epoch)) {busy = false;updateControls();}}
  }
  function retireConversation(id) {
    conversations = conversations.filter(item => item.id !== id);
    if (chat?.id !== id) {
      drafts.retire(id);
      renderList();return;
    }
    rememberComposer();resetRequests();closeDialogs({preserveDeletion:true});busy = !!pendingDeletion?.inFlight;
    draftKey=drafts.retire(id);chat=null;newIntent=true;
    if(!draftKey)composer=drafts.select('');
    input.value = composer.text;restoreCorpus();rememberView();renderList();render();
  }
  function deleteDialog() {
    if (!chat || busy || pendingDeletion) return;
    const dialog = element('dialog',undefined,{class:'chat-dialog','aria-label':'Delete conversation?'});
    // Transcript retirement is not a cleanup acknowledgement. Keep this endpoint
    // and request alive across transcript refreshes until success or dismissal.
    const deletion = {id:chat.id,endpoint:url(),project:projectID,user:userID,controller:new AbortController(),dialog,inFlight:false,dismiss};
    pendingDeletion = deletion;
    const current = () => pendingDeletion === deletion && !denied && deletion.project === projectID && deletion.user === userID && projectID === getProject()?.id && userID === getUser()?.id;
    function dismiss() {
      if (pendingDeletion !== deletion) return;
      pendingDeletion = null;deletion.controller.abort();
      if (deletion.inFlight) busy = false;
      dialog.close();dialog.remove();updateControls();
      if (active()) (deleteButton.disabled ? select : deleteButton).focus();
    }
    dialog.append(element('h2','Delete conversation?'),element('p',chat.title || conversations.find(item => item.id === deletion.id)?.title || 'This conversation'),element('p','Messages and attachments will be deleted. Notebook changes and your unsent draft stay.'));
    const failure = element('p','',{role:'alert'});
    const cancel = button('Cancel',dismiss,{class:'quiet',autofocus:''});
    const confirm = button('Delete conversation',async () => {
      if (!current()) {dismiss();return;}
      if (deletion.inFlight) return;
      deletion.inFlight = true;busy = true;updateControls();confirm.disabled = true;cancel.disabled = true;failure.textContent = '';
      try {
        await api(deletion.endpoint,{method:'DELETE',signal:deletion.controller.signal});
        if (!current()) return;
        retireConversation(deletion.id);dismiss();message.textContent = 'Conversation deleted.';select.focus();
      } catch (problem) {
        if (!current()) return;
        if ([401,403].includes(problem.status)) {showError(problem);return;}
        failure.textContent = (problem.message || 'Chat request failed.') + ' Deletion was not confirmed. Retry explicitly or cancel to dismiss.';
        confirm.textContent = 'Retry deletion';
      } finally {
        if (current()) {deletion.inFlight = false;busy = false;updateControls();confirm.disabled = false;cancel.disabled = false;}
      }
    },{class:'danger'});
    dialog.append(failure,cancel,confirm);dialog.addEventListener('cancel',event => {if (deletion.inFlight) event.preventDefault();});
    dialog.addEventListener('close',dismiss);panel.append(dialog);openDialog(dialog);cancel.focus();
  }
  function attachmentDialog() {
    openAttachmentPicker({panel,note:getNote(),getNote,getDraftAttachment,scope:renderContext(),onAttach(snapshot){composer.attachments.push(snapshot);renderAttachments();},showError});
  }
  function saveDialog(turnID,returnFocus) {
    if(!chat || !canEdit())return;
    openTranscriptSave({panel,chat:structuredClone(chat),endpoint:url('/save-note'),scope:renderContext(),post,onSaved(result){message.textContent='Saved chat to '+result.path;},showError,turnID,returnFocus});
  }
  function updateOverlay() {
    const modal = active() && mobile.matches;
    if(active()){if(opener.parentElement!==header)header.append(opener);}else if(opener.parentElement!==openerHome)openerHome.insertBefore(opener,openerHome.querySelector('.note-actions'));
    if (modal) {
      panel.setAttribute('role','dialog');panel.setAttribute('aria-modal','true');

    } else {
      panel.removeAttribute('role');panel.removeAttribute('aria-modal');
    }
    overlays().panel(panel,{active:active(),modal,group:'note-panels',close});
  }
  function close() {composerRevision++;rememberComposer();closeDialogs();closeMessageMenus();panel.hidden = true;rememberView();stopStream();updateOverlay();document.body.classList.remove('chat-open');opener.setAttribute('aria-expanded','false');opener.replaceChildren(icon('chat'),document.createTextNode('Ask notebook'));opener.setAttribute('aria-label','Ask notebook');opener.title='Ask notebook';onClose();opener.focus({preventScroll:true});}
  function open() {
    if(!active())onOpen();panel.hidden = false;rememberView();document.body.classList.add('chat-open');opener.setAttribute('aria-expanded','true');opener.replaceChildren(icon('close'));opener.setAttribute('aria-label','Close notebook chat');opener.title='Close notebook chat';updateOverlay();opener.focus({preventScroll:true});
    if (!projectID) message.textContent = 'Choose a notebook to open your private chats.';
    refresh().catch(showError);
  }
  function noteChanged() {
    panel.querySelector('dialog[data-attachment-picker]')?.close();
    updateControls();
  }
  function setProject() {
    const nextProject = getProject()?.id || '', nextUser = getUser()?.id || '';
    if (initialized && projectID === nextProject && userID === nextUser) {
      scope.textContent = getProject() ? 'Notebook: ' + getProject().name + ' · Private chat' : 'Choose a notebook';
      noteChanged();if (active()) refresh().catch(showError);return;
    }
    const first = !initialized;initialized = true;
    resetRequests();newIntent = false;projectID = nextProject;userID = nextUser;chat = null;draftKey = '';config = null;corpusDefaults = null;corpusDefaultsError = '';conversations = [];drafts.clear();denied = false;busy = false;
    const restored = first ? restoredView() : null;
    if (restored && /^[a-f0-9]{32}$/.test(restored.conversation)) chat = {id:restored.conversation,turns:[],runs:[]};
    newIntent = !!restored?.newChat;
    composer=drafts.select('');input.value = '';purpose.value = '';review.checked = false;research.checked = false;
    connection.replaceChildren();model.replaceChildren();mode.value = 'ask';controls.open = false;
    scopeInputs.forEach(control => control.checked = false);restoreCorpus();closeDialogs();
    scope.textContent = getProject() ? 'Notebook: ' + getProject().name + ' · Private chat' : 'Choose a notebook';clearError();renderList();render();
    if (restored?.open && !active()) open();
    else {rememberView();if (active()) refresh().catch(showError);}
  }
  mobile.addEventListener('change',updateOverlay);
  document.addEventListener('pointerdown',event => {if (!event.target.closest('.chat-message-actions')) closeMessageMenus();});
  opener.onclick = () => active() ? close() : open();select.onchange = () => selectChat(select.value || null);
  connection.onchange = () => chooseModels();model.onchange = updateControls;mode.onchange = updateControls;review.onchange = updateControls;
  for (const control of [archives,sharedDrafts,history,sourceProjects,noteProjects]) control.onchange = () => {composer.corpus = selectedCorpus();updateCorpusStatus();updateControls();};
  form.onsubmit = submit;input.oninput = () => {composerRevision++;composer.text = input.value;};
  input.addEventListener('keydown',event => {if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') {event.preventDefault();form.requestSubmit();}});

  async function prepareMessage(text) {
    const key='draft:judgments-'+crypto.randomUUID();
    const selection=selectChat(key);
    // Selection installs its composer synchronously, before loading defaults.
    // A later selection, edit, or close supersedes this correction handoff.
    const epoch=lifetime.current(),target=composer,revision=composerRevision,original=target.text;
    await selection;
    if(!valid(epoch) || chat || draftKey!==key || composer!==target || composerRevision!==revision || target.text!==original || input.value!==original)return;
    target.text=text;input.value=text;rememberComposer();open();input.focus();
  }
  updateControls();return {setProject,noteChanged,refresh,open,close,prepareMessage};
}
