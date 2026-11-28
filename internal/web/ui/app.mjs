import {initSettingsPages,closeSettingsPage} from './settings-pages.mjs';
import {toast,confirmAction,dismissConfirmation} from './feedback.mjs';
import {createApplicationState,requestSequence,moduleOn} from './state.mjs';
import {icon,setLabel} from './icons.mjs';
import {initSearch,searchToURL} from './search.mjs';
import {initQuicknotes} from './quicknotes.mjs';
import {initJudgments} from './judgments.mjs';
import {initRetrievalSettings} from './retrieval-settings.mjs';
import {pendingContainer} from './containers.mjs';
import {paced} from './paced.mjs';
import {navigationURL,routeMethod} from './navigation.mjs';
import {initTrash} from './trash.mjs';
import {initPurge} from './purge.mjs';
import {initChat} from './chat.mjs';
import {initSources} from './sources.mjs';
import {initSidebarMenus} from './sidebar.mjs';
import {initCreation} from './creation.mjs';
import {initFolderBrowser} from './browse.mjs';
import {initKnowledge,citationTarget} from './knowledge.mjs';
import {initSettings} from './settings.mjs';
import {initDraftRecovery} from './draft-recovery.mjs';
import {openDraftStorage} from './draft-storage.mjs';
import {initAdministration} from './administration.mjs';
import {initDocuments} from './documents.mjs';
import {initWriting} from './writing.mjs';
import {initRepositories} from './repositories.mjs';
import {initBackups} from './backups.mjs';
import {initAuthority} from './authority.mjs';
import {initNotebookLinks} from './notebook-links.mjs';
import {initFileActions} from './file-actions.mjs';
import {initProtection} from './protection.mjs';
import {enhanceTables} from './tables.mjs';
import {$,installHTML,contentElement} from './dom.mjs';
import {renderViews} from './view-frames.mjs';
import {request as api,post,notebookPath,assetURL} from './request.mjs';
import {initNoteContext} from './note-context.mjs';
import {initProperties} from './properties.mjs';
import {initConnection} from './connection.mjs';
import {initHistory} from './history.mjs';
import {initLayout} from './layout.mjs';
import {initFocusMode} from './focus-mode.mjs';
import {initMediaInsert} from './media-insert.mjs';
import {renderVideos} from './media.mjs';
import {MarkdownEditor} from './markdown-editor.mjs';
import {initWiki} from './wiki.mjs';
import {noteTitle,resolveLink,restoreDraft} from "./editor.mjs";
const appState=createApplicationState({notebook:null,account:null,note:null,collection:'notes',route:location.href});
const navigationRequests=requestSequence();let settingsPages=null;
for(const node of document.querySelectorAll('[data-icon]'))node.prepend(icon(node.dataset.icon));
$('search-open').querySelector('kbd').textContent=/Mac|iPhone|iPad/.test(navigator.userAgentData?.platform || navigator.platform)?'⌘ K':'Ctrl K';
const sidebarMenus = initSidebarMenus();
let project = null,
  user = null,
  projects = [];
let browsingFolder = null, inboxVisible = false;
let returnLocation = null;
const routeScroll = new Map();
let collection = "notes", routingReady = false, restoringRoute = false, currentRoute = location.href;
// The app state modules read, in one shape: a snapshot per call.
const getContext=()=>({project,user,projects,current,original,collection,folder:browsingFolder,inboxVisible,editing,saving,composing,inputVersion,view:viewID});
function routeURL(params) { return navigationURL({project:project.id,collection,layout:wiki.routeLayout(),...params,...(!params.revision && params.note===current?.path ? {noteId:current?.metadata?.id} : {})}); }
// Every route is published under routeMethod's rule; the departing route keeps
// its scroll position for Back. A route is published before its view replaces
// the departing one, so that position (and the browser's own, saved with the
// history entry) is not one the replacement has already clamped.
function publishRoute(url,{push=false,state=null}={}) {
 if(!restoringRoute)routeScroll.set(currentRoute,window.scrollY);
 const method=routeMethod(location.pathname+location.search+location.hash,url,{push,ready:routingReady,restoring:!!restoringRoute});
 if(method)history[method](state,"",url);
 appState.update({route:location.href});
}
function updateRoute(params, replace = false) {publishRoute(routeURL(params),{push:!replace});}
function renderCollections(counts) {
 for(const name of ['notes','drafts','archives'])$("count-"+name).hidden=project?.kind==="repository" || (counts ? counts[name]===0 : $("count-"+name).textContent==="0");
 $("inbox-open").setAttribute("aria-pressed",String(inboxVisible || !!project?.protectedInbox));
 for(const button of document.querySelectorAll("[data-collection]")){button.setAttribute("aria-pressed",String(!inboxVisible && !project?.protectedInbox && button.dataset.collection===collection));button.disabled=!project;}
 if(counts)for(const name of ["notes","drafts","archives"]){const count=$("count-"+name);count.textContent=String(counts[name]);count.title="Total in this collection, including folders";count.setAttribute("aria-label",counts[name]+" total in collection");}
 $("library-collection").value=collection;
 syncMobileNavigation();
}
let browserDrafts;
const draftScope=(path=current?.path)=>user.id+'/'+project.id+'/'+path;
let current = null,
  original = "",
  editing = false,
  saving = false,
  openID = 0,
  viewID = 0,
  previewID = 0;
let live=null,liveStarting=0,inputVersion=0,composing=false,noticeVersion=0,watchSharing=()=>{};
let previewTimer,previewStale=false;
let draftTimer=0,draftKept=true;
// While a live session runs, the draft record is not written whole on the
// typing schedule: the session's recovery record journals the text changes
// made since (draft-storage.mjs folds them back in on recovery), and the
// record is written whole again once the journal grows past this.
let liveJournal=[],journalBytes=0;
const journalLimit=64<<10;
// Adds the editor's changes, each applying to the text the one before it
// left, to the journal. A run of typing, or of deleting what it typed, stays
// one change.
function journal(changes){
  for(const {from,to,insert} of changes){
    const last=liveJournal.at(-1),end=last && last[0]+last[2].length;
    if(last && from===end && to===end)last[2]+=insert;
    else if(last && !insert && to===end && from>=last[0])last[2]=last[2].slice(0,from-last[0]);
    else liveJournal.push([from,to,insert]);
    journalBytes+=insert.length+16;
  }
}
const privateStorageWarning='This browser cannot preserve your private draft. Keep this tab open and copy your text before leaving.';
appState.subscribe((value,previous)=>{if(value.notebook!==previous.notebook || value.account!==previous.account || value.note!==previous.note)dismissConfirmation();project=value.notebook;user=value.account;current=value.note;collection=value.collection;currentRoute=value.route;settingsPages?.check();});
function stopLive(){liveStarting++;live?.stop();live=null;liveJournal=[];journalBytes=0;delete $("document").dataset.live}
function cancelHistorySelection(){versions.cancelSelection()}
// A pending route is not the displayed editor. Only installation retires that
// view; newer writes/drafts supersede pending routes without detaching it.
function beginNavigation(){cancelHistorySelection();draftRecovery.cancelChoice();return openID=navigationRequests.next()}
function installView(){$("wiki-home").hidden=true;writing.reset();documents.hide();noteContext.setPanel(false);leaveAuxiliaryView();fileActions.cancelFilename();cancelHistorySelection();viewID++;previewID++;stopLive()}
// Another view (a folder, Inbox, a repository file) replaces the displayed note.
function clearDisplayedNote(folder=null){
  installView();appState.update({note:null});original="";noteEditor.value="";browsingFolder=folder;
  noteContext.reset();renderMetadata();renderProtection();setMode(false);
}
function beginWrite(){beginNavigation();saving=true}
async function retireLiveDraft(){
  beginNavigation();
  // The draft record takes the text the live record journals before that goes.
  remember();
  const session=live;stopLive();
  try{if(current && project && user)browserDrafts.remove('live',draftScope())}
  catch(error){throw new Error('This browser cannot retain the pause in automatic retries. Keep this tab open and copy your draft before leaving. '+error.message)}
  if(session?.sending)await session.sending.catch(()=>{});
}
function noteView(path=current?.path) {
  const view=viewID,projectId=project?.id;
  return {path,projectId,active:()=>view===viewID && current?.path===path && project?.id===projectId};
}
async function startLive(target,recovered=null) {
 if(target.path?.startsWith("Trash/"))return;
  // A caller owns its captured displayed view, not whichever note is current
  // after its awaits. Stale work must not invalidate a connected session.
  if(!target.active() || !current?.revision || current.privateDraft || target.path==='AGENTS.md' || live)return;
  const id=++liveStarting,{path,projectId}=target;
  const prefix=notebookPath(projectId);
  const scope=user.id+'/'+projectId+'/'+path;
  const startedAt=inputVersion,initialContent=noteEditor.value,initialComposition=composing;
  try {
    const [{LiveSession},snapshot]=await Promise.all([import('./dist/collaboration.js'),api(prefix+'/api/live?path='+encodeURIComponent(path))]);
    if(id!==liveStarting || !target.active())return;
    // Typing the editor has not read yet counts as typing meanwhile.
    noteEditor.takeInput();
    if(startedAt!==inputVersion || initialContent!==noteEditor.value || initialComposition || composing){
      browserDrafts.remove('live',user.id+'/'+projectId+'/'+path);current.privateDraft=true;noteEditor.privateDraftChanged();const retained=remember(true);
      $("draft-conflict").hidden=false;countWords();
      notice(retained?'Your typing was kept as a private draft while live editing connected. Save or reconcile it before joining.':privateStorageWarning);return;
    }
    let saved;try{saved=browserDrafts.liveRecord(user.id+'/'+projectId+'/'+path)}catch{}
    if(recovered && (!saved || saved.generation!==snapshot.liveRevision.split(':')[0]))return;
    if(recovered && (current.privateDraft || saved.content!==initialContent)){
      current.privateDraft=true;noteEditor.privateDraftChanged();const retained=remember(true);
      $("draft-conflict").hidden=false;
      notice(retained?'Your recovered draft includes private changes not represented by its saved live state. Compare and reconcile before joining live editing.':privateStorageWarning);return;
    }
    if(!recovered && dirty())return;
    // Pending or failed navigation does not replace the displayed note. Keep
    // its projection and local input together until installation retires it.
    const ownsView=()=>id===liveStarting && target.active();
    const abort=new AbortController(),active=()=>ownsView() && live===session && !session.closed;
    let connectionNotice=null,metadataRequest=null,shownPending;
    function refreshCommittedMetadata(value) {
      if(current.metadataVersion===value.version || metadataRequest===value.version)return;
      const {revision,version,content}=value;
      metadataRequest=version;
      api(prefix+'/api/note?path='+encodeURIComponent(path)+'&revision='+revision,{signal:abort.signal}).then(doc=>{
        // The note's committed version identifies this metadata, independently
        // of pending typing or an unrelated notebook HEAD advance. Never install
        // text, revisions, live state, or private recovery from this response.
        if(!active() || original!==content || current.version!==version)return;
        if(doc.revision!==revision || doc.version!==version || doc.content!==content)return;
        const membershipChanged=current.metadata?.container!==doc.metadata?.container || current.metadata?.containerError!==doc.metadata?.containerError;
        current.title=doc.title;current.metadata=doc.metadata;current.metadataError=doc.metadataError;current.metadataVersion=version;
        renderMetadata();properties.renderContainerActions();
        if(membershipChanged){search.invalidateCache();search.refresh($("search").value).catch(error=>notice(error.message));}
      }).catch(()=>{}).finally(()=>{if(metadataRequest===version)metadataRequest=null;});
    }
    // `tab` names this session's requests, so the server does not answer its
    // long poll with its own updates. What only says how to answer an update
    // or a checkpoint goes in headers, not the body: a gateway still running
    // the previous release ignores them and answers with the whole note,
    // which the session takes too (internal/web/collaboration.go).
    const tab=crypto.randomUUID();
    // The line says changes are being shared once a second passes with
    // changes unsent and no answer: armed by an answer that leaves some
    // unsent, or by a keystroke while the line says all are synced.
    let sharingState=0;
    const sharing=()=>{if(!saving)state('Sharing changes…')};
    const armSharing=()=>{sharingState||=setTimeout(()=>{sharingState=0;if(live===session && !session.closed && (session.unsent || session.pending))sharing()},1000)};
    watchSharing=()=>{if(live===session && /^(Live changes synced|Saved version)/.test(stateMessage))armSharing()};
    const session=new LiveSession({snapshot,recovered:saved,abort:()=>abort.abort(),
      // The editor's unread input joins the note before any change made
      // elsewhere, which would otherwise reach the editor over it and throw it
      // away (Chrome ends an IME composition before the editor reads its last
      // text, and the session then applies what the composition deferred).
      takeInput:()=>{if(active())noteEditor.takeInput()},
      request:(kind,body)=>{
        if(kind==='read')return api(prefix+'/api/live?path='+encodeURIComponent(path)+(body?'&since='+encodeURIComponent(body)+'&wait=25000&incremental=true&tab='+tab:''),{signal:abort.signal});
        const {since,...operation}=body,headers={'Content-Type':'application/json','X-ForgetMeNote-Live-Tab':tab};
        if(since)headers['X-ForgetMeNote-Live-Since']=since;
        return api(prefix+'/api/live'+(kind==='checkpoint'?'/checkpoint':''),{method:'POST',headers,body:JSON.stringify(operation),signal:abort.signal});
      },
      persist:value=>{
        if(!active())return;
        // Recovery checks the live record against the draft record, which
        // must not be the older: typing the schedule has not written goes first.
        settleDraft();
        try{
          if(!value){browserDrafts.remove('live',scope);return}
          // The live record journals the text changes made after the draft
          // record it names was written whole: write that first when there
          // is none, or when the journal has grown large.
          let draft=browserDrafts.version(scope);
          if(!draft || journalBytes>journalLimit){remember(true,false);draft=browserDrafts.version(scope)}
          if(draft)browserDrafts.write('live',scope,{...value,draft,changes:liveJournal});
          liveStored=!!draft;
        }catch{liveStored=false;notice('This browser cannot retain live edits. Keep the tab open until synced.')}
      },
      // Peers', agents' and undo changes, applied as changes: the editor maps
      // the selection and never replaces the whole document.
      changed:changes=>{
        if(!active())return;
        cancelHistorySelection();
        noteEditor.applyRemote(changes);journal(changes);rememberSoon();countWords(true);
        schedulePreview(150);
      },
      status:(value,unsent,error)=>{
        if(!ownsView() || live!==session)return;
        clearTimeout(sharingState);sharingState=0;
        if(error){
          if(error.privateDraft){
            // The rejected text may equal Git while differing from accepted
            // live text. It is still an explicit, unshared editing intent.
            current.privateDraft=true;noteEditor.privateDraftChanged();
            $("draft-conflict").hidden=false;countWords();
          }
          // A live error may follow newer input or a failed CRDT-state write.
          // Preserve real edits, but do not turn an unchanged note into a draft
          // when a poll fails or navigation aborts its request.
          const hasDraft=dirty() || composing,retained=remember();
          if(retained && error.privateDraft){try{browserDrafts.remove('live',draftScope())}catch{}}
          state(!hasDraft?'Live connection unavailable':retained?'Not synced · draft retained on this device':'Not synced · keep this tab open');
          connectionNotice=notice(!hasDraft || retained?error.message:privateStorageWarning);
          if(error.status===401 || error.status===403){noteEditor.readOnly=true;current.editable=false;setMode(editing)}
          if(live?.closed){live=null;delete $("document").dataset.live}
          return;
        }
        connectionNotice?.();
        connectionNotice=null;
        // The text compared is the whole note: only when the revision, the
        // checkpoint or the committed text it settles would change.
        const settles=value && !unsent && (value.revision!==current.revision || value.version!==current.version || !value.pending && original!==noteEditor.value);
        if(settles && value.content===noteEditor.value) {
          current.metadataVersion ??= current.version;
          current.revision=value.revision;current.version=value.version;
          if(!value.pending){original=value.content;remember();refreshCommittedMetadata(value)}
          countWords();
        }else if(value && value.pending!==shownPending)countWords();
        shownPending=value?.pending;
        // Edits are sent as they are typed, so the line says they are being
        // shared only when that takes a second, rather than flickering (and
        // laying out the header) at every keystroke.
        if(saving)return;
        if(!unsent)state(value?.pending?'Live changes synced · Save to create a version':'Saved version · '+value.revision.slice(0,7));
        else if(!/^(Live changes synced|Saved version)/.test(stateMessage))sharing();
        else armSharing();
      },
    });
    live=session;liveJournal=[];journalBytes=0;liveStored=true;
    $("document").dataset.live='connected';
    noteEditor.sync(live.text);remember();countWords();
    if(noteEditor.value!==initialContent)schedulePreview(150);
    live.receive(snapshot);
  }catch(error){
    if(id!==liveStarting || !target.active())return;
    if(error.privateDraft){
      current.privateDraft=true;noteEditor.privateDraftChanged();$("draft-conflict").hidden=false;countWords();
      const retained=remember(true);if(retained){try{browserDrafts.remove('live',draftScope())}catch{}}
      state(retained?'Not synced · draft retained on this device':'Not synced · keep this tab open');
      notice(retained?error.message:privateStorageWarning);
    }else notice(error.message);
  }
}
const noteEditor = new MarkdownEditor({
  host:$("editing"),root:$("note-editor"),pane:$("rendered-pane"),toolbar:$("formatting-tools"),hint:$("editor-hint"),
  visualButton:$("format-visual"),sourceButton:$("format-source"),splitButton:$("format-split"),
  commentSelection:()=>noteContext.startPassageComment(noteEditor.commentAnchor()),
  openThread:id=>noteContext.openComments(id),
  insertMedia:kind=>mediaInsert.insert(kind),
  openNoteLink:()=>notebookLinks.open(),
  openLink:followEditorLink,
  pageTarget:name=>current ? wiki.pageTarget(name,current.path) : null,
  resolveLink:href=>current && resolveLink(current.path,href),
  liveHistory:{run:redo=>{if(!live)return false;live.undo(redo);return true},enabled:redo=>live?(redo?live.undoManager.canRedo():live.undoManager.canUndo()):null,boundary:()=>live?.undoManager.stopCapturing(),active:()=>!!live},
  imageURL: noteAssetURL,
  privateDraft:videosHeld,
});
const wiki=initWiki({api,post,notice,getProject:appState.notebook,getUser:appState.account,getNote:appState.note,openNote,openFolder,openNotebook:openNotebookReference,refreshProjects:()=>reloadProjects(),
 canCreate:()=>canCreate() && !saving,createPage:name=>creation.openNote(name),previewChanged:()=>noteContext.previewRendered(),
 onLayoutChange:async wikiActive=>{
  if(current){setMode(wikiActive?false:editing);updateRoute({note:current.path},true);wiki.noteOpened();}
  else if(browsingFolder!==null)await openFolder(browsingFolder,false);
 }});
function noteAssetURL(href) {
  const link=current && resolveLink(current.path,href);
  return link && !link.external && !link.project ? assetURL(project.id,link.path) : null;
}
function canEdit(p = project) { return p?.kind!=="repository" && ["edit", "owner"].includes(p?.role); }
// Whether the instance runs the sources module, which imports sources.
function sourcesOn() { return moduleOn(user,'sources'); }
function canCreate() { return canEdit() && !project?.protectedInbox && (current !== null || browsingFolder !== null || inboxVisible); }
function updateCreationControls() {
  $("search-open").disabled=!project || (!current && browsingFolder===null && !inboxVisible && $("source-document").hidden && $("source-browser").hidden);
  for (const button of document.querySelectorAll("#new-note, #create-source, #first-note, #first-source, #empty-source")) button.disabled = !canCreate();
  // Set only on a change: a mutation observer runs these controls again.
  for (const id of ["create-source","first-source","create-source-tab"]) if ($(id).hidden === sourcesOn()) $(id).hidden = !sourcesOn();
  $("notebook-outline").hidden=!project || ($("folder-browser").hidden && $("source-browser").hidden);
  $("note-context-toggle").disabled=!current;
  $("note-creation").hidden=!canCreate();
  syncMobileNavigation();
}
new MutationObserver(syncMobileNavigation).observe($("sidebar"),{subtree:true,attributes:true,attributeFilter:["disabled","hidden"]});
function canManage(p = project) { return !!p && (user?.admin || p.role === "owner"); }
// Shows `message` as the page's toast, or clears it when empty. An `action`
// ({label, run}) adds a button that runs it. Returns a function that dismisses
// this toast if it is still shown.
function notice(message, success = false, action) {
  noticeVersion++;
  return toast($('notice'),message,{level:success?'success':'error',action});
}
let stateMessage="",stateShown="";
function state(message) {
  stateMessage=message;
  // Read mode has no Save button, so a hint to save points to Edit instead.
  const full=message.replace(/ · Save to (create a version|share)$/,hint=>editing?hint:" · Edit to save a version");
  // Typing states its message on every keystroke; the line changes rarely.
  if(stateShown===message+"\n"+full)return;
  stateShown=message+"\n"+full;
  const shown=full.replace(/^Saved version · [a-f0-9]{7}$/, "Saved");
  $("save-state").textContent = shown;
  // Phones and focus mode show this short form; the title keeps the whole message.
  $("save-state").dataset.compact=shown.replace(/ · (Save|Edit) to .*$/,"").replace(/ (retained|kept) on this device$/," kept");
  $("save-state").title=full;
  // Routine states only matter while editing; warnings and recovery stay visible.
  $("save-state").dataset.idle=String(/^(Saved|Ready)$/.test($("save-state").textContent));
  // The draft is at risk: not kept on this device, refused, or not shared.
  $("save-state").dataset.attention=String(/keep this tab open|^Not (saved|synced)/.test(message));
}
// Notebook routes name the displayed notebook explicitly; instance routes
// (/api/me, /api/projects, administration) are requested as written.
function currentNotebook() {
  if (!project) throw new Error("Choose a notebook first.");
  return notebookPath(project.id);
}
// Text the server does not hold yet: a new note, a historical version, or a
// draft retained or recovered as private. Read previews it in the browser,
// and Visual loads its outside videos only when asked.
function privateDraft() {
  return !!current && (!current.revision || !!current.privateDraft);
}
// Whether the note's outside video players wait for the user to load them:
// in a private draft, and in a draft recovered as the note opened
// (`recovering`) until live editing takes it. One live editing cannot take
// (it has no live record, or one of an older live document, or the note is
// AGENTS.md, or live editing is unavailable) is not on the server either: it
// stays held until it is saved, reconciled or replaced (see openNote).
// Visual shows a placeholder, and Read's preview the link.
function videosHeld() {
  return privateDraft() || !!current?.recovering;
}
// Pending live text is checked first: comparing texts of the same length
// reads both whole.
function dirty() {
  return current && (current.privateDraft || !!live?.snapshot.pending || noteEditor.value !== original);
}
// Writes the draft record now, superseding a scheduled write and the live
// record's journal; the live record then names the new draft record, unless
// the caller writes it next.
function remember(includeUnchanged = false, rebind = true) {
  clearTimeout(draftTimer);draftTimer=0;
  if (!current) return true;
  try {
    if (includeUnchanged || dirty() || composing)
      browserDrafts.write('draft',draftScope(),{
        content: noteEditor.value,
        revision: current.revision,
        version: current.version,
        private: !!current.privateDraft,
      });
    else
      browserDrafts.remove('draft',draftScope());
    liveJournal=[];journalBytes=0;
    if(rebind && live && !live.closed)live.store();
    return draftKept=true;
  } catch {
    notice(
      "This browser cannot preserve drafts. Keep this tab open until your changes are saved.",
    );
    return draftKept=false;
  }
}
// Typing and remote changes write the draft record on a schedule rather than
// serializing the whole note per keystroke: at most 250 ms after the first
// unwritten change. Writing live state, hiding the page and leaving it write
// what the schedule has not reached yet. A live session that stores its
// record journals the changes instead (journal), so typing in a long note
// never writes it whole.
const typingState=kept=>kept?"Unsaved on this device":"Draft not saved · keep this tab open";
let liveStored=false;
function rememberSoon() {
  if(live && !live.closed && liveStored)return;
  draftTimer||=setTimeout(()=>{
    draftTimer=0;
    const before=draftKept,kept=remember();
    if(kept!==before && stateMessage===typingState(before))state(typingState(kept));
  },250);
}
function settleDraft() {if(draftTimer)remember();}
addEventListener("pagehide",settleDraft);
document.addEventListener("visibilitychange",()=>{if(document.visibilityState==="hidden")settleDraft();});
function setMode(value) {
  editing = value;
  document.body.classList.toggle("editing-note",value);
  $("note-back").hidden=!current;
  $("note-details-toggle").disabled=!current;
  $("editing").hidden = !value;
  $("document").classList.toggle("is-editing", value);
  setLabel($("mode"), value ? "Read" : wiki.active ? "Edit page" : "Edit");
  $("save").disabled = !current || saving || !current.editable || !dirty();
  $("mode").classList.toggle('quiet', value);
  $("save").hidden = !value;
  $("reading-actions").hidden = true;
  noteEditor.show(value);
  noteContext.modeChanged();
  if (value) noteEditor.focus();
  else if (previewStale) preview();
  writing.update();
  if (stateMessage) state(stateMessage);
}
// Where a note's link goes, by the reader's rules: its address and what a
// plain click does (open the note, citation or notebook), or a new tab for
// an attachment.
function noteLinkTarget(link) {
  if(link.project)return {action:()=>openNotebookReference(link).catch(error=>notice(error.message))};
  if(!link.path.endsWith('.md'))return {href:assetURL(project.id,link.path,link.revision||current.revision),newTab:true};
  return {href:routeURL({note:link.path,revision:link.revision,hash:link.hash}),action:()=>{
    if(link.revision)knowledge.openCitation(link.path,link.revision,link.hash).catch(error=>notice(error.message));
    else openNote(link.path,link.hash);
  }};
}
// A link the note editor follows ({href} or a [[page]] link's {wiki} name),
// as the reader follows it; another note opens as openNote keeps the draft.
function followEditorLink({href,wiki:name}) {
  if(!current)return;
  if(name!==undefined)return wiki.openPage(name,current.path);
  const link=citationTarget(current.path,href);
  if(!link)return notice('This link does not lead to a note, file or web page.');
  const target=link.external?{href:link.url,newTab:true}:noteLinkTarget(link);
  if(target.action)target.action();
  else window.open(target.href,'_blank','noopener,noreferrer');
}
// `saved` is the note revision the HTML renders; without it the HTML is a
// draft. A held draft's video links get no player (videosHeld).
function showHTML(html, privatePreview=false, saved=null, held=privatePreview) {
  installHTML($("preview"), html, {
    prepare(root) {
      enhanceTables(root);
      if(!held)renderVideos(root,noteAssetURL);
      renderViews(root,{project:project.id,path:current.path,revision:saved?.revision,privateDraft:privatePreview});
    },
    resolve: href => citationTarget(current.path, href),
    follow(el, link) {
      const target=noteLinkTarget(link);
      if(target.href)el.href=target.href;
      if(target.newTab){el.target='_blank';el.rel='noopener noreferrer';}
      return target.action;
    },
  });
  for (const image of $("preview").querySelectorAll("img[src],img[data-fmn-src]")) {
    const link = citationTarget(current.path, image.getAttribute("data-fmn-src") ?? image.getAttribute("src"));
    image.removeAttribute("data-fmn-src");
    if (!link || link.project || link.external) image.removeAttribute("src");
    else image.src = assetURL(project.id,link.path);
  }
  wiki.linkPages($("preview"),current.path);
  noteContext.previewRendered();
}
async function preview() {
  if (!current) return;
  previewStale = false;
  const id = ++previewID, target = noteView(), content = noteEditor.value;
  try {
    const privatePreview=privateDraft(),held=videosHeld();
    let html;
    if(privatePreview) {
      try {
        // The browser renders the draft in slices, as Read would show it, up to
        // 4 MiB of Markdown and of HTML (F1, a novel, is 1.2 MB). Larger drafts,
        // or drafts it cannot render, stay readable locally as plain text.
        if(content.length > (4<<20) || new TextEncoder().encode(content).length > (4<<20))throw Error('Large private draft');
        const {renderPrivateMarkdown}=await import('./dist/rendered-preview.js');
        if(id!==previewID || !target.active() || content!==noteEditor.value)return;
        // A newer preview (the next typing pause) or leaving the note stops it.
        html=await renderPrivateMarkdown(content,{cancelled:()=>id!==previewID});
        if(html==null)return;
      } catch {
        const text=document.createElement('pre');text.textContent=content;html=text.outerHTML;
      }
    } else html=(await post(currentNotebook()+"/api/preview", { content })).html;
    if (id === previewID && target.active() && content === noteEditor.value) showHTML(html,privatePreview,!privatePreview && content===original?current:null,held);
  } catch (e) {
    if (id === previewID) noteContext.previewFailed();
    if (id === previewID && target.active() && content === noteEditor.value) notice(e.message);
  }
}
// Loading a destination does not freeze the displayed editor. Recheck at the
// installation boundary; an older stored draft is not retention of newer input.
function retainForNavigation() {
  const required = dirty() || composing;
  if (!remember() && required) {
    beginNavigation();
    state("Draft not saved · keep this tab open");
    return false;
  }
  // Visual IME may not have serialized its final characters yet.
  if (composing) {
    beginNavigation();
    notice("Finish composing your text before leaving this note.");
    return false;
  }
  return true;
}
async function openNote(path, anchor = "", chooseCandidate=false, replaceRoute=false) {
 if(path.startsWith("Trash/")){await trash.open(path.slice(6,-3));return;}
  if (saving) return;
  if (
    dirty() &&
    !await confirmAction("Keep your draft on this device and open another note?")
  )
    return;
  if (saving || !retainForNavigation()) return;
  notice("");
  const id = beginNavigation(), signal = navigationRequests.signal(), projectId = project.id, account = user?.id, sidebarRequest = layout.version();
  const active = () => id === openID && project?.id === projectId && user?.id === account;
  state("Loading…");
  try {
    const [doc, protection] = await Promise.all([
      api(currentNotebook()+"/api/note?path=" + encodeURIComponent(path), {signal}),
      api(currentNotebook()+"/api/locks?path=" + encodeURIComponent(path), {signal}),
    ]);
    if (!active()) return;
    const candidate=await draftRecovery.choose(path,active,chooseCandidate);
    if(candidate===false || !active())return;
    let raw=candidate?.raw;
    if (raw) {
      try {
        const legacy = JSON.parse(raw);
        if (!legacy.version && /^[a-f0-9]{40}$/.test(legacy.revision)) {
          const base = await api(currentNotebook()+"/api/note?path=" + encodeURIComponent(path) + "&revision=" + legacy.revision, {signal});
          raw = JSON.stringify({...legacy, version: base.version});
        }
      } catch {}
    }
    if (!active() || saving) return;
    if (saving || !retainForNavigation()) return;
    // Reopening this same note must recover the draft just retained above,
    // not the older record read before its final retention check.
    if(candidate?.owner===browserDrafts.owner && current?.path===path)raw=browserDrafts.read('draft',draftScope(path));
    if(candidate)browserDrafts.adopt(draftScope(path),candidate,raw);
    else if(chooseCandidate)browserDrafts.detach(draftScope(path));
    const draft = restoreDraft(doc, raw);
    if(search.isOpen)history.replaceState({search:search.snapshot(),scroll:window.scrollY},"",location.href);
    if(search.isOpen || browsingFolder!==null || inboxVisible)returnLocation={project:projectId,account,folder:browsingFolder,collection,search:search.isOpen,inbox:inboxVisible,scroll:window.scrollY};
    updateRoute({note:path,noteId:doc.metadata?.id,hash:anchor},replaceRoute);
    installView();
    browsingFolder = null;
    $("folder-browser").hidden = true;
    appState.update({note:{
      ...doc,
      revision: draft.revision,
      version: draft.version,
      privateDraft: !!draft.private,
      // A recovered draft holds its outside videos until live editing takes
      // it; on this note object only, so saving, reconciling or opening
      // another note ends it (videosHeld).
      recovering: !!raw,
      protection,
      editable: path!=="AGENTS.md" && canEdit() && protection.rules.length === 0,
    }});
    const target = noteView(path);
    noteContext.reset();
    renderProtection();
    renderMetadata();
    original = doc.content;
    noteEditor.value = draft.content;
    noteEditor.readOnly = !current.editable;
    $("empty").hidden = true;
    $("document").hidden = false;
    renderNoteHeading();
    updateCreationControls();
    $("mode").disabled = !current.editable;
    $("history-open").disabled = false;
    $("comments-open").disabled = false;
 knowledge.noteChanged();chat.noteChanged();
    setMode(!!raw && current.editable);
    showHTML(doc.html,false,doc);
    wiki.noteOpened();
    noteContext.loadComments().catch(e=>{if(target.active())notice(e.message)});
    if (raw) {
      const opened = current;
      await preview();
      if (!active() || !target.active() || current !== opened) return;
      state(remember(true)?"Unsaved on this device · recovered":"Draft not saved · keep this tab open");
    } else state("Saved version · " + doc.revision.slice(0, 7));
    $("draft-conflict").hidden = !(draft.conflict || current.privateDraft);
    if (draft.conflict)
      notice(
        "Your draft is based on an older revision. Compare and reconcile it before saving.",
      );
    draftRecovery.refreshCandidates();
    if (layout.closeOnPhone(sidebarRequest) && editing) noteEditor.focus();
    if (anchor) contentElement($("preview"), anchor)?.scrollIntoView();
    else window.scrollTo(0,0);
    countWords();
    updateCreationControls();
    const privateBefore=privateDraft();
    await startLive(target,raw);
    // A recovered draft's videos stay held until live editing takes it: its
    // live session joined, and the draft goes to the server, or kept it
    // private, which holds them anyway. Otherwise the draft is on this device
    // only, and they stay held until it is saved, reconciled or replaced,
    // each of which installs a note without `recovering`.
    if(target.active() && current.recovering && (live || privateDraft())){
      current.recovering=false;noteEditor.privateDraftChanged();
      // Read's preview, rendered by the server without players, gains them;
      // a draft kept private is rendered again in the browser.
      if(!privateDraft())schedulePreview(0);
      else if(!privateBefore){for(const player of $("preview").querySelectorAll(".note-video"))player.remove();preview();}
    }
    return active() ? true : undefined;
  } catch (e) {
    if (!active()) return;
    notice(e.message);
    state("Could not load note");
    return false;
  }
}
async function save() {
  if (!current || !current.editable || saving || !dirty() || $("new-dialog").open) return;
  const target = noteView();
  beginWrite();
  setMode(editing);
  noteEditor.readOnly = true;
  state("Committing locally…");
  const submitted = noteEditor.value;
  let committed = false;
  remember();
  try {
    let result;
    if(live){await live.checkpoint();committed=true;result=await api(currentNotebook()+'/api/note?path='+encodeURIComponent(current.path))}
    else result = await post(currentNotebook()+"/api/note", {
      path: current.path,
      content: submitted,
      revision: current.revision,
    });
    committed = true;
    browserDrafts.resolve(draftScope(),noteEditor.value===result.content?null:{content:noteEditor.value,revision:result.revision,version:result.version,private:false});
    // Resolution rotates the owned replay key; retain newer pending state there.
    if(live?.snapshot.pending)live.store();
    appState.update({note:{ ...result, editable: true }});
    noteEditor.privateDraftChanged();
    renderMetadata();
    original = result.content;
    remember();
    draftRecovery.refreshCandidates();
    $("draft-conflict").hidden=true;
    await refreshProtection();
    $("history-open").disabled = false;
    $("comments-open").disabled = false;
 knowledge.noteChanged();chat.noteChanged();
    updateRoute({note:current.path},true);
    renderNoteHeading();
    remember();
    notice("");
    if(target.active()){
      if(result.content===noteEditor.value)showHTML(result.html,false,result);
      else await preview();
    }
    await refreshNotebook();
    if(!live)await startLive(target);
    state(live?.snapshot.pending ? "Live changes synced · Save to create a version" : "Saved version · " + result.revision.slice(0, 7));
  } catch (e) {
    const retained=committed || remember(true);
    notice(retained?e.message:e.message+'\n'+privateStorageWarning);
    state(committed ? "Saved version · view refresh failed" : retained?"Not saved · draft kept on this device":"Not saved · keep this tab open");
    if (!committed) {
     const latest=await api(currentNotebook()+"/api/note?path="+encodeURIComponent(current.path)).catch(()=>null);
     if(latest && latest.version !== current.version) $("draft-conflict").hidden=false;
    }
    await refreshProtection().catch(() => {});
  } finally {
    saving = false;
    noteEditor.readOnly = !current.editable;
    setMode(editing);
  }
}
// The word count, Save and the collection badge. The badge and Save's label
// read the note's front matter, and reading a long note's text copies all of
// it, so after typing or a peer's change they follow once changes pause
// (250 ms, and at least once a second while they do not).
const containerLabels=paced(()=>{
  const targetContainer=pendingContainer(noteEditor.value);
  $("save").textContent=!current?.revision && targetContainer === "draft" ? "Save to Drafts" : "Save";
  properties.renderBadge();
});
function countWords(changing=false) {
  writing.update();
  $("save").disabled = saving || !current?.editable || !dirty();
  if(changing)containerLabels.soon();else containerLabels.now();
}
// While editing, the hidden preview is not rendered (on a novel, a whole-note
// upload and render at every pause): the context panel follows the editor's
// text, and leaving edit mode renders the preview once.
function schedulePreview(delay) {
  clearTimeout(previewTimer);
  if (editing) {previewStale=true;noteContext.textChanged();return;}
  previewTimer = setTimeout(preview, delay);
}
noteEditor.addEventListener("input", event => {
  cancelHistorySelection();
  inputVersion++;
  if(live && !live.closed)journal(event.changes);
  rememberSoon();
  countWords(true);
  // A live note's line follows its session (status), not each keystroke.
  if(!live || live.closed || !draftKept)state(typingState(draftKept));
  else watchSharing();
  live?.local(event.changes);
  schedulePreview(300);
});
$("save").onclick = save;
// The note's text, as opposed to a field of its own inside the editor
// (Source's search panel), whose composition is its own.
const inNote=target=>!!target?.closest?.('#editing') && !target.closest('.cm-panels');
document.addEventListener('compositionstart',event=>{if(inNote(event.target)){cancelHistorySelection();composing=true;inputVersion++;if(live)live.composing=true}},true);
document.addEventListener('compositionend',event=>{
  if(!inNote(event.target))return;
  composing=false;inputVersion++;rememberSoon();
  // The session reads the composition's last text, which the editor may not
  // have read yet, before the remote changes the composition deferred.
  live?.finishComposition();
},true);
// Undo is the note's (the live engine's per-user undo while connected), in
// both formats; the editors keep no history of their own. The browser's undo
// is the page's, so it never runs in the editor: from Source's search field
// it could undo typing in the note behind the editor's back, so there it does
// nothing.
document.addEventListener('keydown',event=>{
  const key=event.key.toLowerCase();
  if(editing && event.target.closest('#editing') && (event.ctrlKey || event.metaKey) && !event.altKey && (key==='z' || key==='y' && !event.shiftKey)){
    event.preventDefault();event.stopImmediatePropagation();
    if(inNote(event.target))noteEditor.undo(key==='y' || event.shiftKey);
  }
},true);
// The Edit menu's and context menu's Undo and Redo: the browser's own undo
// would change the editor's DOM behind its back.
document.addEventListener('beforeinput',event=>{
  if(editing && event.target.closest?.('#editing') && (event.inputType==='historyUndo' || event.inputType==='historyRedo')){
    event.preventDefault();event.stopImmediatePropagation();
    if(inNote(event.target))noteEditor.undo(event.inputType==='historyRedo');
  }
},true);
$("mode").onclick = () => {
  // "Edit page" leaves the wiki for the normal editor.
  if(!editing && wiki.active && current){wiki.leave();setMode(true);updateRoute({note:current.path},true);return;}
  setMode(!editing);
};
$("formatting-expand").onclick=()=>{const open=$("formatting-expand").getAttribute("aria-expanded")!=="true";$("formatting-expand").setAttribute("aria-expanded",String(open));$("formatting-expand").textContent=open?"Fewer tools":"More tools";};
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
    e.preventDefault();
    save();
  }
});
window.addEventListener("beforeunload", (e) => {
  remember();
  if (dirty()) {
    e.preventDefault();
    e.returnValue = "";
  }
});
for (const el of document.querySelectorAll("[data-close]"))
  el.onclick = () => closeSettingsPage($(el.dataset.close));
function newNote() {
  creation.openNote();
}
$("new-note").onclick = newNote;
$("create-source").onclick = () => sourceBrowser.open();
$("first-note").onclick = newNote;
// Opens a new, unsaved note at `path` in the editor, recovering the retained
// draft chosen for it.
function installNewNote({path,protection,container,candidate}) {
  const raw=candidate?.raw;if(candidate)browserDrafts.adopt(draftScope(path),candidate);
  noteContext.reset();
  beginNavigation();
  installView();
  browsingFolder = null;
  $("folder-browser").hidden = true;
  appState.update({note:{ path, revision: "", editable: true, protection }});
  chat.noteChanged();
  renderProtection();
  original = "";
  noteEditor.value = restoreDraft(
    { content: "---\nfmn-id: "+crypto.randomUUID().replaceAll('-','')+"\n"+(container === "draft" ? "fmn-container: draft\n" : "")+"---\n", revision: "" },
    raw,
  ).content;
  noteEditor.readOnly = false;
  renderNoteHeading();
  $("empty").hidden = true;
  $("document").hidden = false;
  $("mode").disabled = false;
  renderMetadata();
  $("history-open").disabled = true;
  $("comments-open").disabled = true;
  $("preview").replaceChildren();
  $("new-dialog").close();
  layout.closeOnPhone();
  setMode(true);
  state("Unsaved on this device · Save to share");
  notice("");
  countWords();
  draftRecovery.refreshCandidates();
}
$("purge-open").onclick = () => { closeSettingsPage($("settings-dialog")); purge.open(); };
$("library-collection").onchange=()=>openFolder("",true,$("library-collection").value);
$("note-back").onclick=async()=>{
 const target=returnLocation;
 if(target?.project===project?.id && target?.account===user?.id){
  if(target.search){await search.open(target.scroll);return;}
  const restored=target.inbox ? (await openInbox(),inboxVisible) : await openFolder(target.folder || "",true,target.collection);
  if(restored)window.scrollTo(0,target.scroll);
 }else if(project?.protectedInbox)await openInbox();else await openFolder(current?.path.split("/").slice(0,-1).join("/") || "");
};
function syncMobileNavigation(){
 for(const button of document.querySelectorAll("[data-destination]")){
  const id=button.dataset.destination,source=$(id);
  if(id==="browse-root")button.textContent=project?.kind==="repository"?"Files":"Notes";
  const disabled=source.disabled || source.hidden;
  if(button.disabled!==disabled)button.disabled=disabled;
  if(id==="create-source"&&button.hidden===sourcesOn())button.hidden=!sourcesOn();
  if(["browse-root","search-open","inbox-open","review-open"].includes(id))button.setAttribute("aria-current",(id==="search-open"?search.isOpen:id==="inbox-open"?inboxVisible || project?.protectedInbox:id==="browse-root"?!search.isOpen && !inboxVisible && !project?.protectedInbox:false)?"page":"false");
 }
}
for(const button of document.querySelectorAll("[data-destination]"))button.onclick=()=>$(button.dataset.destination).click();
// Reads the notebook's listing and connection again after a change.
async function refreshNotebook({invalidate=false}={}) {
  if(invalidate)search.invalidateCache();
  await search.refresh($("search").value);await connection.check();
}
function renderProtection() {
  renderNoteHeading();
  $("note-trash").hidden = !current?.revision || !canManage() || current.path === "AGENTS.md" || current.path.startsWith("Trash/");
  $("note-trash").disabled = !current?.editable;
  $("note-trash").title = current?.editable ? "Move to Trash" : "Unlock this note before moving it to Trash";
  $("protection-open").hidden = !current;
  if (!current) return;
  const rule = current.protection?.rules[0];
  // Owners see the action the dialog offers; others, who cannot change locks, see the state.
  $("protection-label").textContent = current.path==="AGENTS.md" ? "Read-only" : canManage() ? (rule ? "Unlock…" : "Lock…") : rule ? "Locked" : current.editable ? "Editable" : "Read-only";
  $("protection-open").title = current.path==="AGENTS.md" ? "Edit in Notebook settings → Agent instructions" : rule
    ? "Read-only · " + (rule.path === current.path ? "locked note" : "inherited from " + rule.path)
    : current.editable ? "Manage note and folder locks" : "Your notebook role does not allow editing";
  $("lock-shackle").setAttribute("d", current.editable ? "M8 10V7a4 4 0 017.5-2" : "M8 10V7a4 4 0 018 0v3");
}
async function refreshProtection() {
  if (!current) return;
  const note = current;
  const protection = await api(currentNotebook()+"/api/locks?path=" + encodeURIComponent(note.path));
  if (current !== note) return;
  current.protection = protection;
  current.editable = current.path!=="AGENTS.md" && canEdit() && protection.rules.length === 0;
  noteEditor.readOnly = !current.editable;
  $("mode").disabled = !current.editable;
  renderProtection();
}
// Runs a write that commits the note at `path`, pausing that note's live session
// while it runs; `install` shows the saved note when it is still displayed.
async function writeNote(path,write) {
 const target=noteView(),same=current?.path===path,session=same?live:null;
 beginWrite();if(same)noteEditor.readOnly=true;
 try {
  if(session){await session.flush();stopLive()}
  await write(saved=>{
   if(!same || !target.active())return;
   appState.update({note:{...saved,editable:true,protection:current.protection}});original=saved.content;noteEditor.value=saved.content;remember();renderMetadata();showHTML(saved.html,false,saved);countWords();state('Saved version · '+saved.revision.slice(0,7));
  });
 } finally {
  saving=false;
  if(same && target.active()){noteEditor.readOnly=!current.editable;countWords();if(session)await startLive(target)}
 }
}
const layout=initLayout({sidebarMenus,isEditing:()=>editing,isSearchOpen:()=>search.isOpen});
const {smallScreen}=layout;
const connection=initConnection({api,getContext,notice,onStatus:v=>{wiki.revisionChanged(v.revision);knowledge.refreshReview();}});
const versions=initHistory({api,notice,state,getContext,noteView,noteEditor,dirty,canManage,canEdit,beginNavigation,setMode,remember,preview,countWords,
 retireView:()=>{++previewID;stopLive();},
 showRevision:old=>{showHTML(old.html,false,old);properties.showCover(old.metadata,old.revision);sourceBrowser.render({...old,editable:false});},
 openPurge:options=>purge.open(options)});
const noteContext=initNoteContext({api,post,notice,getContext,noteEditor,setMode,dirty,canManage,canEdit,beginWrite,
 endWrite:()=>{saving=false;},refreshStatus:()=>connection.check(),references:(...args)=>knowledge.references(...args),onRoute:()=>appState.update({route:location.href}),
 previewStale:()=>previewStale});
const mediaInsert=initMediaInsert({api,notice,state,getContext,noteEditor,remember,countWords,preview,setMode,beginWrite,endWrite:()=>{saving=false;},
 openNotebookLink:()=>notebookLinks.open()});
const properties=initProperties({api,post,notice,getContext,getContent:()=>noteEditor.value,currentNotebook,dirty,canEdit,
 draftPaths:prefix=>browserDrafts.paths(prefix),beginWrite,endWrite:()=>{saving=false;},writeNote,renderProtection,openNote,openFolder,
 openSearch:()=>search.open(),invalidateSearch:()=>search.invalidateCache(),refresh:options=>refreshNotebook(options),moveToTrash:(path,error)=>trash.move(path,error)});
const protection=initProtection({api,post,getContext,canManage,dirty,beginWrite,endWrite:()=>{saving=false;},
 restoreMode:()=>setMode(editing),openNote,refresh:()=>refreshNotebook()});
const fileActions=initFileActions({api,post,notice,getContext,noteView,noteEditor,canManage,canEdit,dirty,beginWrite,endWrite:()=>{saving=false;},refresh:()=>refreshNotebook(),
 draftPaths:prefix=>browserDrafts.paths(prefix),classifyNote:properties.classifyNote,classifyFolder:properties.classifyFolder,moveToTrash:path=>trash.move(path),openProtection:node=>protection.open(node),openPurge:node=>purge.open({path:node.path}),openNote,openFolder});
const notebookLinks=initNotebookLinks({api,post,getProject:appState.notebook,getNote:appState.note,editor:noteEditor,notice});
const knowledge=initKnowledge({api,post,openNotebook:openNotebookReference,openCode:ref=>repositories.file(ref.source,ref.revision,ref.path),getProject:appState.notebook,getUser:appState.account,getNote:appState.note,canEdit,canManage,openNote,archiveSource:url=>{sourceBrowser.open({url});},onSearchStatus:status=>search.setMeaningAvailable(!!status?.browserSearch),refresh:()=>refreshNotebook()});
const creation = initCreation({api,getProject:appState.notebook,getCollection:()=>collection,getFolder:()=>browsingFolder ?? current?.path.split("/").slice(0,-1).join("/") ?? "",canCreate:()=>canCreate() && !saving,canEdit,openSource:()=>sourceBrowser.open(),
 ready:()=>!saving && retainForNavigation(),chooseDraft:(path,active)=>draftRecovery.choose(path,active),install:installNewNote});
// After a purge, what this page shows of the notebook is read again, so no
// erased text stays on screen; an open note reloads from the server.
const purge=initPurge({api,post,getProject:appState.notebook,canManage,notice,
 refresh:async()=>{await refreshNotebook({invalidate:true});
  const path=current?.path;if(path && !dirty())await openNote(path).catch(()=>openFolder(""));}});
const trash=initTrash({api,post,getProject:appState.notebook,canManage,notice,
 beforeOpen:()=>openFolder("",false,"notes"),
 beforeMove:async paths=>{
  fileActions.checkReady();
  const open=paths.includes(current?.path),retained=browserDrafts.paths(user.id+'/'+project.id+'/');
  if(open && (dirty() || current.privateDraft))throw Error('Save or reconcile this draft before moving it to Trash.');
  if(paths.some(path=>retained.includes(path)))throw Error(paths.length===1?'Save or reconcile retained drafts for this note before moving it to Trash.':'Save or reconcile the retained drafts in this folder before moving it to Trash.');
  saving=true;const target=noteView(),session=open?live:null;
  if(session){noteEditor.readOnly=true;try{await session.flush();stopLive()}catch(e){saving=false;noteEditor.readOnly=!current?.editable;throw e}}
  return ()=>{saving=false;if(current && target.active()){noteEditor.readOnly=!current.editable;if(session)startLive(target).catch(e=>notice(e.message))}};
 },
 afterMove:async paths=>{if(paths.includes(current?.path)){$('properties-dialog').close();clearDisplayedNote();saving=false;await openFolder('')}else if(paths.length>1 && browsingFolder!==null){saving=false;await openFolder(browsingFolder,false)}},
 refresh:()=>refreshNotebook({invalidate:true})
});
const sourceBrowser = initSources({api,post,notice,available:sourcesOn,getProject:appState.notebook,getNote:appState.note,getFolder:()=>browsingFolder ?? current?.path.split("/").slice(0,-1).join("/") ?? "",present:creation.showSource,onBusy:creation.setSourceBusy,canEdit,dirty,openNote,refresh:()=>refreshNotebook(),insertCitation:text=>{
 if(!current?.editable || saving)return;
 noteEditor.setRangeText('\n\n'+text+'\n\n',noteEditor.selectionStart,noteEditor.selectionEnd,'end');remember();setMode(true);countWords();preview();noteEditor.focus();
}});
let sidebarBeforeChat = null;
const chat = initChat({api,post,getProject:appState.notebook,getUser:appState.account,
 getNote:()=>current && ({path:current.path,revision:current.revision,liveRevision:live?.snapshot?.liveRevision}),
 // Only the explicit private attachment button calls this snapshot callback.
 getDraftAttachment:()=>current && ({path:current.path,revision:current.revision || '',content:noteEditor.value}),
 canEdit,canManage,openCitation:(path,revision,heading)=>knowledge.openCitation(path,revision,heading),
 openDocument:ref=>ref.origin==='repository'?documents.open(ref):openNotebookReference(ref),
 openReview:()=>$("review-open").click(),openSource:url=>sourceBrowser.open({url}),
 openJudgmentSettings:()=>judgments.openSettings(),
 mountCitationChecks:(host,scope)=>judgments.mountCitationChecks(host,scope),

 onOpen:()=>{if(sidebarBeforeChat===null)sidebarBeforeChat=layout.snapshot();if(smallScreen.matches)layout.closeMobile();layout.update();},
 onClose:()=>{if(sidebarBeforeChat){layout.restore(sidebarBeforeChat);sidebarBeforeChat=null;layout.update();}}
});
const judgments = initJudgments({api,post,getUser:appState.account,getProject:appState.notebook,getProjects:()=>projects,
 openReview:async(id,finding,section="findings")=>{judgments.leaveChecks();if(project?.id!==id)await chooseProject(projects.find(p=>p.id===id));if(project?.id===id)await knowledge.openReview(finding,section);},
 prepareChat:async(id,text)=>{if(project?.id!==id)await chooseProject(projects.find(p=>p.id===id));if(project?.id===id)await chat.prepareMessage(text);}
});
for(const button of document.querySelectorAll('[data-review-section]'))button.onclick=()=>{
  if(button.dataset.reviewSection==='checks'){knowledge.suspendReview();judgments.openChecks();}
  else {judgments.leaveChecks();knowledge.openReview(undefined,button.dataset.reviewSection);}
};
async function openCaptureNote(capture) {
  if(saving || !user)return;
  const account=user.id;
  let request=beginNavigation();
  const active=()=>request===openID && user?.id===account;
  try {
    const catalog=await api("/api/projects");
    if(!active())return;
    const destination=catalog.find(p=>p.id===capture.project);
    if(!destination)throw new Error("This capture's notebook is no longer available.");
    projects=catalog;renderProjects();
    const selection=chooseProject(destination,capture.path,null,"notes","","","push");
    // chooseProject synchronously takes ownership of the next navigation stage.
    request=openID;
    await selection;
  } catch(error) {if(active())throw error;}
}
const quicknotes=initQuicknotes({api,post,getUser:appState.account,getDraftStorage:()=>browserDrafts,onCapture:()=>{judgments.refreshNotifications();},openInbox,openCaptureNote});
const settings = initSettings({api, post, notice, remember, getUser:appState.account, getProjects:()=>projects});
const retrievalSettings=initRetrievalSettings({api,post,getProject:appState.notebook,getUser:appState.account,onChange:()=>{search.cancel();if(project)search.refresh($("search").value).catch(error=>notice(error.message))}});
const documents=initDocuments({api,post,getContext,notice,openNote,currentNotebook,beginNavigation,retainForNavigation,clearDisplayedNote,updateRoute,state,updateCreationControls,layout,
 chooseNotebook:id=>chooseProject(projects.find(p=>p.id===id)),ownsNavigation:request=>request===openID,navigationSignal:()=>navigationRequests.signal(),
 setCollection:value=>{appState.update({collection:value});renderCollections();},repositoriesOn:()=>moduleOn(user,'repositories')});
const writing=initWriting({editor:noteEditor,api,post,notice,openDocument:documents.open,getProject:appState.notebook,
 getDocument:()=>current?{project:project.id,path:current.path,revision:current.revision,origin:'notes',local:true,content:noteEditor.value,editable:current.editable,editing}:documents.current?{project:project.id,path:documents.current.path,revision:documents.current.revision,sourceID:documents.current.source,origin:'repository',local:false,content:documents.current.content,partial:documents.current.more||(documents.current.startLine||1)>1}:null,
 canFix:()=>!!current?.editable&&editing&&!saving&&!composing,
 onOpen:()=>{layout.closeOnPhone();},
});
const repositories=initRepositories({api,post,notice,remember,openDocument:documents.open,getUser:appState.account,getProject:appState.notebook,getProjects:()=>projects});
const search=initSearch({api,post,retrievalSettings,notice,renderCollections,openNote,setupFileActions:fileActions.setup,canEdit,canCreate,
  getContext,
  onOpen:()=>{beginNavigation();settingsPages.leave({navigate:false,resume:true});publishSearchRoute(search.snapshot(),true);noteContext.setPanel(false);if(!$("chat-panel").hidden)chat.close();layout.closeOnPhone();},
  onClose:()=>{publishSearchRoute(null,true);(current?$("mode"):$("search-open")).focus({preventScroll:true});},
  onStateChange:snapshot=>publishSearchRoute(snapshot),
  onVisibilityChange:syncMobileNavigation,
  openCitation:(...args)=>knowledge.openCitation(...args),openTrash:id=>trash.open(id),
  openSource:(...args)=>repositories.file(...args),addSource:()=>sourceBrowser.open(),sourcesOn,
  repositoriesOn:()=>moduleOn(user,'repositories'),judgmentsOn:()=>moduleOn(user,'judgments'),
});
function publishSearchRoute(snapshot,push=false){
 const query=searchToURL(new URLSearchParams(location.search),snapshot);
 publishRoute(location.pathname+'?'+query+location.hash,{push,state:snapshot?{search:snapshot,scroll:window.scrollY}:null});
}
const backups=initBackups({api,post,getProject:appState.notebook,getUser:appState.account,remember});
const authority=initAuthority({api,post,getProject:appState.notebook,getUser:appState.account,remember});
const administration = initAdministration({openWikiSettings:wiki.openSettings,openAuthoritySettings:authority.open,openBackupSettings:backups.open,api, post, notice, openRepositorySettings:repositories.open, openRetrievalSettings:retrievalSettings.openSettings, getUser: () => user, getProject: () => project, reloadProjects, selectProject: async id => chooseProject(projects.find(p => p.id === id))});
appState.subscribe((next,previous)=>{if(next.account!==previous.account){administration.checkAuthority();settings.checkAuthority();}});
settingsPages=initSettingsPages({
 onLeave:name=>{if(name==='administration')administration.closed();},
 getContext,
 onEnter(){remember();search.cancel();search.hide();if(!$("chat-panel").hidden)chat.close();noteContext.setPanel(false);writing.close(false);layout.closeOnPhone();if($('knowledge-dialog').open)$('knowledge-dialog').close();},
 onReturn(base){const url=new URL(base,location.origin),query=url.searchParams;if(query.get("search")==="1"){restoreSearchRoute(query).catch(error=>notice(error.message));return;}if(query.get('note')&&query.get('revision')&&query.get('origin')!=='repository')knowledge.openCitation(query.get('note'),query.get('revision'),url.hash.slice(1)).catch(error=>notice(error.message));},
 onRoute:(url,replace)=>publishRoute(url,{push:!replace,state:history.state}),
 load:name=>name==='account'?settings.openAccount():name==='notebook'?administration.openProject():administration.openInstance(),
});
initFocusMode({layout,closePanels:()=>{if(!$("chat-panel").hidden)chat.close();noteContext.setPanel(false);},
  isEditing:()=>editing,focusEditor:()=>noteEditor.focus(),openSearch:()=>search.open().catch(error=>notice(error.message)),toggleContext:noteContext.toggle});
const draftRecovery=initDraftRecovery({api,getNote:appState.note,getProject:appState.notebook,getContent:()=>noteEditor.value,getDrafts:()=>browserDrafts,scope:draftScope,openNote,notice,beforeReview:retireLiveDraft,
 applyDraft:async(latest,content,discard)=>{
  const target=noteView(),session=live;
  // Save's keyboard shortcut can reconnect while Compare remains open.
  beginNavigation();stopLive();
  if(session?.sending)await session.sending.catch(()=>{});
  if(!target.active())return false;
  await refreshProtection();if(!target.active())return false;
  browserDrafts.resolve(draftScope(),content===latest.content?null:{content,revision:latest.revision,version:latest.version,private:false});
  appState.update({note:{...current,...latest,privateDraft:false,recovering:false}});original=latest.content;noteEditor.value=content;
  draftRecovery.refreshCandidates();
  $("draft-conflict").hidden=true;notice("");
  setMode(!discard);await preview();if(!target.active())return false;countWords();
  state(discard?"Saved version · "+latest.revision.slice(0,7):"Reconciled draft · save when ready");
  if(discard)await startLive(target);
  return target.active() && (discard || noteEditor.value===content);
 }
});
updateCreationControls();
layout.update();
$('account-actions-toggle').disabled=true;
initialize().catch((e) => notice(e.message)).finally(()=>{$('account-actions-toggle').disabled=false;});

function leaveAuxiliaryView() {
  inboxVisible=false;$("inbox-view").hidden=true;renderCollections();
  search.hide();
  if(typeof quicknotes!=="undefined")quicknotes.hide();
}
async function openInbox() {
  if(saving || !user || user.id==="@local-admin")return;
  if(dirty() && !await confirmAction("Keep your draft on this device and open Inbox?"))return;
  if(saving || !retainForNavigation())return;
  const request=beginNavigation();search.cancel();
  const params=new URLSearchParams({area:"inbox"});if(project && !project.protectedInbox)params.set("project",project.id);
  publishRoute("/?"+params,{push:true});
  clearDisplayedNote();
  inboxVisible=true;$("inbox-view").hidden=false;$("empty").hidden=true;$("folder-browser").hidden=true;$("document").hidden=true;
  $("path").textContent="Inbox";state("Private to your account");$("mode").disabled=true;$("comments-open").disabled=true;$("history-open").disabled=true;
  $("inbox-open").setAttribute("aria-pressed","true");renderCollections();updateCreationControls();
  document.title="Inbox · "+document.documentElement.dataset.instanceName;layout.closeOnPhone();
  await quicknotes.show();
  return request===openID ? request : undefined;
}
$("inbox-open").onclick=()=>openInbox().catch(error=>notice(error.message));

async function restoreSearchRoute(query,scroll){
 let requested;
 try{requested=search.restoreURL(query);}
 catch(error){
  search.cancel();search.hide();publishSearchRoute(null);notice(error.message);
  return;
 }
 if(!requested)return;
 if(query.get("page")){
  if(!settingsPages.active())publishSearchRoute(search.snapshot());
 }else await search.open(scroll);
}

async function restoreSettingsRoute(query){
 await restoreSearchRoute(query);
 await settingsPages.restore(query);
}

async function initialize() {
  const initialQuery=new URLSearchParams(location.search);
  browserDrafts=await openDraftStorage();
  appState.update({account:await api("/api/me")});
  try { if(!sessionStorage.getItem('fmn:assistant-onboarding:'+user.id)) {
   sessionStorage.setItem('fmn:assistant-onboarding:'+user.id,'1');
   post('/api/assistant/action/assistant_status',{}).then(status=>{
    if(status?.available&&!status.settings.revision){const banner=document.getElementById('assistant-welcome');if(banner)banner.hidden=false;}
   }).catch(()=>{});
  }} catch {}
  projects = await api("/api/projects");
  updateIdentity();
  renderProjects();
  $("admin-open").hidden = !user.admin;
  settings.initialize(user);
  await restoreRoute(initialQuery);
  await restoreSearchRoute(initialQuery);
  routingReady=true;$("settings-open").disabled=!project;appState.update({route:location.href});
  if(initialQuery.get('repository')==='create')await administration.resumeRepository(initialQuery.get('notebook'));
  if(['connect','gdrive','dropbox'].includes(initialQuery.get('backup'))){
    await administration.openProject();
    document.querySelector('#settings-dialog [data-settings-category=history]').click();
  }
  if(initialQuery.get('repository')==='connect'){
    await administration.openProject();
    document.querySelector('#settings-dialog [data-settings-category=sources]').click();
  }
}
// Initial links and browser history restore the same pinned version and collection.
async function restoreRoute(query, hash = location.hash.slice(1)) {
 settingsPages.leave({navigate:false,focus:false});
 const revision=query.get("revision"),account=user?.id,request=beginNavigation();let note=query.get("note");
 const sourceRoute=query.get("origin")==="repository";
 const requested=query.get('project');
 if(requested&&!projects.some(p=>p.id===requested))throw Error('Notebook unavailable or access denied.');
 if(requested&&query.get('noteId')){
  const target=await post(notebookPath(requested)+'/api/notebook-links/resolve',{project:requested,path:note||'',noteId:query.get('noteId'),revision:revision||'',heading:hash});
  if(request!==openID||user?.id!==account)return;
  note=target.path;
 }
 if(revision && note && !/^[a-f0-9]{40}$/.test(revision))throw new Error("Invalid historical revision.");
 const selected=await chooseProject(projects.find(p=>p.id===query.get("project")) || projects.find(p=>!p.protectedInbox) || projects[0],sourceRoute?null:note,sourceRoute?null:query.get("folder"),query.get("collection"),revision,hash);
 if(selected!==openID || user?.id!==account)return;
 if(query.get("area")==="inbox"){const installed=await openInbox();if(query.get("page"))await restoreSettingsRoute(query);return installed;}
 if(!project){if(query.get("page"))await restoreSettingsRoute(query);return selected;}
 if(sourceRoute){
  const installed=query.get("path")?await documents.open({project:project.id,origin:"repository",source:query.get("source"),revision:query.get("revision"),path:query.get("path"),heading:hash,startLine:Number(query.get("startLine"))||1,endLine:Number(query.get("endLine"))||200,cursor:query.get('cursor')||''}):await documents.openFolder(query.get("folder")||"");
  if(installed===openID&&user?.id===account&&query.get('page'))await restoreSettingsRoute(query);
  return installed;
 }
 if(query.get("trash"))await trash.open(query.get("trash"));
 else if(query.get("purge"))await purge.openPrepared(query.get("purge"));
 else if(revision && note && !query.get('page'))await knowledge.openCitation(note,revision,hash);
 if(selected===openID&&user?.id===account){
  if(query.has("account")){const legacy=new URLSearchParams(query);legacy.set("page","account");legacy.set("category","account");await restoreSettingsRoute(legacy);}
  else if(query.get("page"))await restoreSettingsRoute(query);
 }
 return selected===openID && user?.id===account ? selected : undefined;
}
async function openNotebookReference(ref){
 const request=beginNavigation(),account=user?.id;
 const active=()=>request===openID&&account===user?.id;
 let target,available;
 try {
  target=await post(notebookPath(ref.project)+'/api/notebook-links/resolve',{project:ref.project,path:ref.path||'',noteId:ref.noteId||'',revision:ref.revision||'',heading:ref.hash||ref.heading||''});
  if(!active())return;
  available=await api('/api/projects');
 }catch(error){if(active())throw error;return;}
 if(!active())return;
 const selected=available.find(p=>p.id===target.project);if(!selected)throw Error('Notebook unavailable or access denied.');
 projects=available;renderProjects();
 const installed=await chooseProject(selected,target.path,undefined,undefined,target.revision,target.heading,"push");
 if(installed===openID&&target.revision)await knowledge.openCitation(target.path,target.revision,target.heading);
}
async function reloadProjects() {
  appState.update({account:await api("/api/me")});
  settings.update(user);
  projects = await api("/api/projects");
  $("admin-open").hidden = !user.admin;
  renderProjects();
  const next = projects.find(p => p.id === project?.id);
  if (project && next) {
    fileActions.close();
    appState.update({notebook:next});
    chat.setProject();judgments.projectChanged();quicknotes.userChanged();
    $("project-select").value = project.id;
    updateIdentity();
    updateCreationControls();
    await refreshProtection();
    setMode(editing);
  } else await chooseProject(next || projects.find(p=>!p.protectedInbox) || projects[0]);
}
function updateIdentity() {
  const role = user.admin ? 'Admin' : ({owner:'Owner', edit:'Editor', comment:'Commenter', read:'Reader'}[project?.role] || 'No notebook access');
  const name = user.displayName || user.username || (user.id === "@local-admin" ? "Recovery administrator" : "Account");
  $("identity").textContent = name + ' · ' + role;
  $("account-name").textContent = name;
  // The assistant page holds the personal assistant and agent memory.
  document.querySelector('#account-actions a[href="/account/assistant"]').hidden = !moduleOn(user,'personal') && !moduleOn(user,'memory');
  $("account-initial").textContent = [...name.trim()][0]?.toLocaleUpperCase() || "A";
}
function renderProjects() {
  $("project-select").replaceChildren(
    ...projects.filter(p=>!p.protectedInbox).map((p) => new Option(p.name, p.id)),
    new Option("Choose notebook", ""),

  );
  $("notebook-creation").hidden=!user.canCreateProjects;
}
$("notebook-create").onclick=()=>administration.createWorkspace(false);
$("notebook-import").onclick=()=>administration.createWorkspace(true);
// Completion returns the owned navigation token, including a valid empty notebook catalog.
async function chooseProject(next, note, folder = null, nextCollection = "notes", revision = "", hash = "", routeHistory = "replace") {
  if (saving) return;
  if (
    dirty() &&
    !await confirmAction("Keep this draft on this device and switch notebooks?")
  ) {
    $("project-select").value = project.id;
    return;
  }
  if (saving || !retainForNavigation()) {
    $("project-select").value = project?.id || "";
    return;
  }
  if(project?.id!==next?.id){returnLocation=null;routeScroll.clear();}
  const account=user?.id,keepInbox=routeHistory==="push" && inboxVisible;
  const request = beginNavigation();
  const active=()=>request===openID && user?.id===account && project===next;
  // Capture setup does not replace its Inbox; only a successful note installation does.
  if(!keepInbox)installView();
  search.cancel();trash.reset();purge.reset();
  sidebarMenus.closeAll();
  appState.update({note:null});
  browsingFolder = null;
  $("folder-browser").hidden = true;
  original = "";
  fileActions.reset();
  creation.projectChanged();
  appState.update({notebook:next});
  wiki.projectChanged(next);
  appState.update({collection:["notes","drafts","archives"].includes(nextCollection)?nextCollection:"notes"});search.reset(collection);renderCollections();
  chat.setProject();judgments.projectChanged();quicknotes.userChanged();
  knowledge.projectChanged();
  repositories.projectChanged();
  backups.projectChanged();
  documents.projectChanged();
  document.body.classList.toggle("repository-notebook",next?.kind==="repository");
  $("browse-root").querySelector("span").textContent=next?.kind==="repository"?"Files":"Notes";
  $("collection-drafts").hidden=$("collection-archives").hidden=$("trash-open").hidden=next?.kind==="repository";
  updateIdentity();
  connection.show(next ? "Checking notebook…" : "Ask an Owner or Admin for notebook access", null);
  renderMetadata();
  renderProtection();
  $("document").hidden = true;
  $("empty").hidden = keepInbox;
  noteEditor.value = "";
  $("history-open").disabled = !next;
  noteContext.reset();
  $("comments-open").disabled = true;
  $("mode").disabled = true;
  $("save").disabled = true;
  updateCreationControls();
  $("notebook-link-insert").disabled = !canEdit(next);
  $("settings-open").disabled = !next || !routingReady;
  if (!next) {
    state("No notebook access yet");
    return request;
  }
  $("project-select").value = next.protectedInbox ? "" : next.id;
  $("create-note-destination").textContent = next.protectedInbox ? "Choose a notebook" : next.name;
  if(!keepInbox)$("path").textContent = next.name + " · " + next.role;
  $("backup-link").href = notebookPath(next.id) + "/api/backup";
  $("purge-open").hidden = $("purge-settings-help").hidden = !canManage(next) || next.kind === "repository";
  // A wiki opens a folder on its overview page; resolve it before routing.
  if(wiki.active && !note && !revision){const index=await wiki.folderIndex(folder || "").catch(()=>"");if(!active())return;note=index || note;}
  const route=note ? {note,revision,hash} : {folder:folder || ""};
  // User-selected captures push only after the guarded note installation succeeds.
  if(routeHistory==="replace")updateRoute(route,true);
  const warnings=[];
  try{await retrievalSettings.projectChanged();}
  catch(error){if(!active())return;warnings.push(error.message);}
  if(!active())return;
  const listError=next.kind==="repository" || note && !revision ? await search.refresh() : null;
  if(!active())return;
  if(listError)warnings.push(listError.message);
  try{await connection.check(active);}
  catch(error){
    if(!active())return;
    if(!keepInbox)throw error;
    notice(error.message);state("Could not open note");return false;
  }
  if(!active())return;
  const selection=note && !revision ? openNote(note,hash,false,routeHistory==="replace") : openFolder(folder || "",false,collection,route);
  const selectedRequest=openID,warningVersion=noticeVersion;
  const installed=await selection;
  if(selectedRequest!==openID || user?.id!==account || project!==next || !installed)return;
  if(warnings.length && noticeVersion===warningVersion)notice(warnings.join("\n"));
  return selectedRequest;
}
$("project-select").onchange = () => {
  const selected = $("project-select").value;
  chooseProject(projects.find(p => p.id === selected)).catch(e => notice(e.message));
};
function renderNoteHeading() {
  if (!current) return;
  $("path").textContent = noteTitle(current);
  $("path").title = noteTitle(current);
  document.title=$("path").textContent+" · "+document.documentElement.dataset.instanceName;
  $("note-path").textContent = current.path;
  $("note-path").hidden = false;
  $("note-folder").textContent = current.path.includes("/") ? current.path.slice(0, current.path.lastIndexOf("/")) : "";
  $("filename-open").hidden = !fileActions.canRename();
}
// Dialogs launched from the collapsed More menu return to its visible toggle.
for(const [action,dialog] of [["properties-open","properties-dialog"],["history-open","history-dialog"],["protection-open","protection-dialog"]]){
  let fromDetails=false;
  $(action).addEventListener("click",()=>{fromDetails=true;});
  $(dialog).addEventListener("close",()=>{
    const focused=document.activeElement;
    // Native close events are queued; do not steal focus from continued editing.
    if(fromDetails && (focused===document.body || focused===$(action) || $(dialog).contains(focused)) && $("note-details-toggle").getClientRects().length)$("note-details-toggle").focus();
    fromDetails=false;
  });
}
function renderMetadata() {
 $("note-details-toggle").disabled=!current;
 if(current)renderNoteHeading();else {$("note-path").hidden=true;$("note-folder").textContent="";$("filename-open").hidden=true;}
 sourceBrowser.render(current);
 properties.render();
}

const folderBrowser = initFolderBrowser({root:$("main"),openFolder,openNote,setupActions:fileActions.setup});
async function openFolder(path,closeSidebar=true,nextCollection=collection,route=null) {
  if(project?.kind==="repository")return documents.openFolder(path,closeSidebar,nextCollection);
  if(path === "Trash"){await trash.open();return;}
  if (!project || saving) return;
  if(wiki.active){
    // A wiki folder opens on its overview page; without one it lists its pages.
    nextCollection="notes";
    let index=wiki.indexOf(path);
    if(index===null){const pending=openID;index=await wiki.folderIndex(path).catch(()=>"");if(pending!==openID || !wiki.active)return;}
    if(index)return openNote(index);
  }
  if (dirty() && !await confirmAction("Keep your draft on this device and browse this folder?")) return;
  if (saving || !retainForNavigation()) return;
  const request=beginNavigation(), signal=navigationRequests.signal(), selectedProject=project, sidebarRequest=layout.version();
  fileActions.close();
  try {
    const data=await api(currentNotebook()+"/api/browse?path="+encodeURIComponent(path)+"&collection="+nextCollection,{signal});
    if(request!==openID || project!==selectedProject || saving)return;
    if (saving || !retainForNavigation()) return;
    const collectionChanged=collection!==nextCollection;appState.update({collection:nextCollection});
    clearDisplayedNote(data.path);
    $("document").hidden=true;$("empty").hidden=true;$("folder-browser").hidden=wiki.active;updateCreationControls();
    $("comments-open").disabled=true;$("mode").disabled=true;$("history-open").disabled=false;
    knowledge.noteChanged();chat.noteChanged();
    if(wiki.active)wiki.showContents(data.path);else folderBrowser.render(data,project,user.id);
    $("path").textContent=data.path || project.name;state("");
    document.title=(data.path.split('/').pop() || project.name)+" · "+document.documentElement.dataset.instanceName;
    updateRoute(route || {folder:data.path});renderCollections();
    if(collectionChanged)search.setCollection(collection);
    const listError=await search.refresh($("search").value);
    if(request!==openID || project!==selectedProject)return;
    notice(listError?.message || "");
    updateCreationControls();
    if(closeSidebar)layout.closeOnPhone(sidebarRequest);
    return true;
  } catch(error){if(request===openID)notice(error.message);}
}
for(const button of document.querySelectorAll("[data-collection]"))button.onclick=()=>{
 const selected=button.dataset.collection;
 return openFolder("",true,selected!=="notes" && selected===collection?"notes":selected);
};
window.addEventListener("popstate",async event=>{
 const previous=currentRoute,query=new URLSearchParams(location.search),restoration={};
 // History installs several intermediate views; retain only the departing view,
 // before those installs collapse its layout, and never a pending restoration.
 if(!restoringRoute)routeScroll.set(previous,window.scrollY);
 restoringRoute=restoration;
 const scroll=event.state?.scroll ?? routeScroll.get(location.href) ?? 0;
 try{
  let installed;
  if(settingsPages.retainsContext(location.href)){
   const request=beginNavigation(),account=user?.id;
   if(query.get('page'))await restoreSettingsRoute(query);
   else settingsPages.leave({navigate:false,resume:true});
   if(request===openID&&user?.id===account){appState.update({route:location.href});installed=request;}
  }else installed=await restoreRoute(query);
  if(installed===openID && restoringRoute===restoration){
   await restoreSearchRoute(query,scroll);
   if(installed===openID && restoringRoute===restoration)window.scrollTo(0,scroll);
  }
 }
 catch(error){if(restoringRoute===restoration)notice(error.message)}
 finally{
  // A retired history request must not finish or repair a newer restoration.
  if(restoringRoute===restoration){
   restoringRoute=false;
   if(currentRoute===previous && location.href!==previous)history.pushState(null,"",previous);
  }
 }
});



