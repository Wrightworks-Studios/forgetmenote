import {icon} from './icons.mjs';
import {openDialog} from './overlay.mjs';
import {fileActionTarget,destinationFolders,copyName,blockingFileDrafts,contextMenuPosition,movedPath} from './editor.mjs';
import {notebookPath} from './request.mjs';
import {confirmAction} from './feedback.mjs';
import {$,busy} from './dom.mjs';
// Menu items carry a leading icon; only their text and icon change with state.
function setItem(button,text,name) {
  const label=[...button.childNodes].find(node=>node.nodeType===Node.TEXT_NODE);
  if(label)label.textContent=text;else button.append(text);
  if(name)button.querySelector('.ui-icon')?.replaceWith(icon(name));
}
// A folder moves to Trash as one transaction of its notes (the Trash API takes up to 50).
const folderTrashLimit=50;
function folderTrashProblem(notes) {
  if(!notes.length)return 'This folder has no notes to move.';
  if(notes.length>folderTrashLimit)return `Folders with more than ${folderTrashLimit} notes can’t move to Trash at once. Move some notes out first.`;
  if(notes.some(entry=>!entry.editable))return 'Unlock the locked notes in this folder first.';
  return '';
}

// The note/folder action menu, its rename/move/duplicate dialog and the open
// note's inline filename form. Moves and copies hold the caller's save state
// (`beginWrite`/`endWrite`) and editor, then reopen what was displayed.
export function initFileActions({api,post,notice,getContext,noteView,noteEditor,canManage,canEdit,dirty,draftPaths,classifyNote,classifyFolder,moveToTrash,openProtection,openPurge,openNote,openFolder,beginWrite,endWrite,refresh}) {
  let fileAction = null;
  let fileActionRequest = 0;
  let fileMenuTrigger;
  let fileMenuScroll;
  const fileMenu = $("file-menu");
  function closeFileMenu(restoreFocus = false) {
    fileMenu.hidePopover();
    fileMenuTrigger?.setAttribute("aria-expanded", "false");
    if (restoreFocus && fileMenuTrigger?.isConnected) fileMenuTrigger.focus();
  }
  fileMenu.addEventListener("toggle", () => {
    fileMenuTrigger?.setAttribute("aria-expanded", String(fileMenu.matches(":popover-open")));
  });
  fileMenu.addEventListener("keydown", event => {
    if (event.key === "Escape" || event.key === "Tab") {
      closeFileMenu(true);
      if (event.key === "Escape") {event.preventDefault(); event.stopPropagation();}
      return;
    }
    const items = [...fileMenu.querySelectorAll("[role=menuitem]:not(:disabled):not([hidden])")];
    if (!items.length || !["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const index = items.indexOf(document.activeElement);
    const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1
      : index < 0 ? (event.key === "ArrowDown" ? 0 : items.length - 1)
      : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
    items[next].focus();
  });
  window.addEventListener("resize", () => closeFileMenu());
  window.addEventListener("scroll", () => {
    if(fileMenu.matches(":popover-open") && (scrollY!==fileMenuScroll.top || scrollX!==fileMenuScroll.left))closeFileMenu();
  }, {passive:true});
  $("file-action-dialog").addEventListener("close", () => {
    if (fileMenuTrigger?.isConnected) fileMenuTrigger.focus();
  });
  function checkFileActionReady(action, from, to) {
    const {project,user,current,saving}=getContext();
    if (action ? !canManage() : !canEdit()) throw new Error("Your notebook role does not allow this action.");
    if (saving) throw new Error("Wait for the current save to finish.");
    if (!action) return;
    const drafts = draftPaths(user.id + "/" + project.id + "/");
    const blocked = new Set(blockingFileDrafts(action, from, to, drafts));
    // The editor is reloaded after organizing, so keep its unsaved draft in place.
    if (dirty()) blocked.add(current.path);
    if (blocked.size) {
      const reason = action === "duplicate"
        ? "Save or reconcile the affected drafts before duplicating."
        : "Save or reconcile this notebook's drafts before moving or renaming. Links in other notes may change.";
      const error = new Error(reason);
      error.drafts = [...blocked];
      throw error;
    }
  }
  function setupFileActions(row, node, ownsResults = () => true) {
    const projectID=getContext().project?.id,accountID=getContext().user?.id;
    const current=()=>getContext().project?.id===projectID && getContext().user?.id===accountID && row.isConnected && ownsResults();
    if ((!canManage() && !(canEdit() && node.kind === "note")) || node.path === "AGENTS.md" || node.path === "Trash" || node.path.startsWith("Trash/")) return;
    const button = document.createElement("button");
    button.className = "tree-menu quiet";
    button.append(icon("more"));
    button.title = "Actions for " + node.path;
    button.setAttribute("aria-label", button.title);
    button.setAttribute("aria-haspopup", "menu");
    button.setAttribute("aria-controls", "file-menu");
    button.setAttribute("aria-expanded", "false");
    button.onclick = async event => {
      event.preventDefault(); event.stopPropagation();
      if(!current())return;
      try {checkFileActionReady();} catch (error) {notice(error.message); return;}
      if (fileMenu.matches(":popover-open") && fileMenuTrigger === button) {closeFileMenu(true); return;}
      closeFileMenu();
      fileMenuTrigger = button;
      fileAction = null;
      const request = ++fileActionRequest;
      fileMenu.setAttribute("aria-label", "Actions for " + node.path);
      $("file-menu-error").hidden = true;
      document.querySelectorAll("[data-file-action]").forEach(button => button.disabled = true);
      $("file-menu-trash-hint").hidden = true;
      $("file-menu-collection-hint").hidden = true;
      setItem(fileMenu.querySelector('[data-file-action=trash]'), node.kind === "note" ? "Move to Trash" : "Move to Trash…");
      setItem(fileMenu.querySelector('[data-file-action=protection]'), "Lock…", "lock");
      fileMenuScroll={top:scrollY,left:scrollX};
      fileMenu.showPopover();
      const position = contextMenuPosition(button.getBoundingClientRect(), fileMenu.getBoundingClientRect(), {width:window.innerWidth, height:window.innerHeight});
      fileMenu.style.left = position.left + "px"; fileMenu.style.top = position.top + "px";
      button.setAttribute("aria-expanded", "true");
      fileMenu.focus();
      try {
        const [entries, snapshot, locks] = await Promise.all([api(notebookPath(projectID)+"/api/notes"), api(notebookPath(projectID)+"/api/status"), api(notebookPath(projectID)+"/api/locks?path="+encodeURIComponent(node.path)).catch(() => null)]);
        if (!current() || request !== fileActionRequest || !fileMenu.matches(":popover-open")) return;
        fileAction = {node, entries, folders:destinationFolders(entries), revision:snapshot.revision, project:projectID, current};
        // A folder offers each collection that would move at least one of its notes.
        const folderNotes=node.kind==="note" ? [] : entries.filter(entry=>entry.path.startsWith(node.path+"/") && entry.path.endsWith(".md"));
        const containerOf=metadata=>metadata?.container || "notes";
        // A lock blocks moving a folder's notes between collections; say so up front.
        const collectionProblem=node.kind==="note" || !folderNotes.some(entry=>!entry.editable) ? "" : locks?.rules?.length ? "Unlock this folder to move its notes." : "Unlock the locked notes in this folder first.";
        document.querySelectorAll("[data-file-action]").forEach(button => {
          const action=button.dataset.fileAction,containerAction=action.startsWith("container-"),metadata=entries.find(entry=>entry.path===node.path)?.metadata;
          if(containerAction && node.kind!=="note"){
            button.hidden=!canEdit() || !folderNotes.some(entry=>"container-"+containerOf(entry.metadata)!==action);
            button.disabled=!!collectionProblem || folderNotes.some(entry=>entry.metadata?.containerError);
            return;
          }
          button.hidden=containerAction ? node.kind!=="note" || action==="container-"+containerOf(metadata) : !canManage() || action==="purge" && (node.kind!=="note" || !canEdit());
          button.disabled=containerAction ? !!metadata?.containerError : false;
        });
        if (locks?.rules?.length) setItem(fileMenu.querySelector('[data-file-action=protection]'), "Unlock…", "unlock");
        $("file-menu-collection-hint").textContent = collectionProblem;
        $("file-menu-collection-hint").hidden = !collectionProblem || !canEdit();
        // Say why a folder cannot move to Trash instead of only greying it out.
        const trashProblem = node.kind === "note" ? "" : folderTrashProblem(folderNotes);
        fileMenu.querySelector('[data-file-action=trash]').disabled = !!trashProblem;
        $("file-menu-trash-hint").textContent = trashProblem;
        $("file-menu-trash-hint").hidden = !trashProblem || !canManage();
        if (document.activeElement === fileMenu) fileMenu.querySelector("[role=menuitem]:not([hidden]):not(:disabled)")?.focus();
      } catch (error) {
        if (!current() || request !== fileActionRequest || !fileMenu.matches(":popover-open")) return;
        $("file-menu-error").textContent = error.message; $("file-menu-error").hidden = false;
      }
    };
    row.append(button);
  }
  function startFileAction(action) {
    if (!fileAction || fileAction.project!==getContext().project?.id || fileAction.current && !fileAction.current()) {closeFileMenu();return;}
    if(action.startsWith("container-")){
      const selected=fileAction,container=action.slice(10)==="notes"?"":action.slice(10);closeFileMenu();
      if(selected.node.kind==="note"){classifyNote(selected.node.path,container,selected.revision).catch(error=>notice(error.message));return;}
      const moving=selected.entries.filter(entry=>entry.path.startsWith(selected.node.path+"/") && entry.path.endsWith(".md") && (entry.metadata?.container || "")!==container).length;
      classifyFolder(selected.node.path,container,selected.revision,moving).catch(error=>notice(error.message));
      return;
    }
    if(action === "trash"){
      const {node,entries}=fileAction;closeFileMenu();
      if(node.kind === "note"){moveToTrash(node.path);return;}
      trashFolder(node,entries).catch(error=>notice(error.message));
      return;
    }
    if (action === "protection") {
      const node = fileAction.node;
      closeFileMenu();
      openProtection(node);
      return;
    }
    if (action === "purge") {
      const node = fileAction.node;
      closeFileMenu();
      openPurge(node);
      return;
    }
    const node = fileAction.node, parts = node.path.split("/"), filename = parts.pop();
    fileAction.action = action;
    fileAction.folder = parts.join("/");
    const verb = {rename:"Rename",move:"Move",duplicate:"Duplicate"}[action];
    $("file-action-title").textContent = verb + " " + node.kind;
    $("file-action-source").textContent = node.path;
    $("file-name-field").hidden = action === "move";
    $("folder-picker").hidden = action === "rename";
    $("file-extension").hidden = node.kind !== "note";
    $("file-name").value = action === "duplicate" ? copyName(node,fileAction.entries) : node.kind === "note" ? filename.slice(0,-3) : filename;
    $("file-action-submit").textContent = action === "move" ? "Move here" : action === "duplicate" ? "Create copy" : "Rename";
    closeFileMenu();
    renderFolderPicker();
    validateFileAction();
    openDialog($("file-action-dialog"));
    if (action !== "move") {$("file-name").focus(); $("file-name").select();}
  }
  async function trashFolder(node,entries) {
    const inside=entries.filter(entry=>entry.path.startsWith(node.path+"/"));
    const notes=inside.filter(entry=>entry.path.endsWith(".md"));
    const problem=folderTrashProblem(notes);
    if(problem)throw new Error(problem);
    const count=notes.length+" "+(notes.length===1?"note":"notes");
    const others=inside.length>notes.length ? " Other files in the folder stay where they are." : "";
    if(!await confirmAction(`Move the ${count} in “${node.path}” to Trash? You can restore them from Trash.${others}`))return;
    await moveToTrash(notes.map(entry=>entry.path));
  }
  function renderFolderPicker() {
    const {folder,node,folders} = fileAction;
    $("folder-breadcrumbs").replaceChildren();
    const crumbs = [{path:"",name:"Project"}];
    const parts = folder ? folder.split("/") : [];
    parts.forEach((name,index) => crumbs.push({name,path:parts.slice(0,index+1).join("/")}));
    for (const crumb of crumbs) {
      const button = document.createElement("button");
      button.type = "button"; button.className = "quiet"; button.textContent = crumb.name;
      button.disabled = crumb.path === folder;
      button.onclick = () => chooseDestination(crumb.path);
      $("folder-breadcrumbs").append(button);
    }
    $("folder-choices").replaceChildren();
    const children = folders.filter(path => path && path.split("/").slice(0,-1).join("/") === folder && !(node.kind === "folder" && (path === node.path || path.startsWith(node.path+"/"))));
    for (const path of children) {
      const button = document.createElement("button"), arrow = document.createElement("span");
      button.type = "button"; button.className = "quiet"; button.textContent = path.split("/").pop();
      button.setAttribute("aria-label","Open folder " + path);
      arrow.textContent = "›"; arrow.setAttribute("aria-hidden","true"); button.append(arrow);
      button.onclick = () => chooseDestination(path);
      $("folder-choices").append(button);
    }
    if (!children.length) {
      const empty = document.createElement("p");empty.className = "small";empty.textContent = "No subfolders here.";$("folder-choices").append(empty);
    }
  }
  function chooseDestination(folder) {
    fileAction.folder = folder;
    renderFolderPicker(); validateFileAction();
  }
  function validateFileAction() {
    if (!fileAction) return null;
    $("file-action-drafts").replaceChildren();
    const {node,action,folder,entries,folders} = fileAction;
    const result = fileActionTarget(node,action,folder,$("file-name").value);
    if (result.path && [...entries.map(entry=>entry.path),...folders].some(path=>path.toLowerCase()===result.path.toLowerCase())) result.error = "That name is already used in this folder.";
    $("file-action-preview").textContent = result.path || "—";
    $("file-action-error").textContent = result.error || "";
    $("file-action-submit").disabled = getContext().saving || !!result.error;
    return result.error ? null : result.path;
  }
  for (const button of document.querySelectorAll("[data-file-action]")) button.onclick = () => startFileAction(button.dataset.fileAction);
  $("file-name").oninput = validateFileAction;
  $("file-action-form").onsubmit = event => {
    event.preventDefault();
    const to = validateFileAction(); if (!to) return;
    const source = fileAction;
    busy($("file-action-form"), () => perform(source, to, () => $("file-action-dialog").close()), {
      controls:[$("file-action-fields"), $("file-action-submit")],
      report(error) {
        $("file-action-error").textContent = error.message;
        for (const draftPath of error.drafts || []) {
          const button = document.createElement("button");
          button.type = "button"; button.className = "quiet";
          button.textContent = "Open draft: " + draftPath;
          button.onclick = () => {$("file-action-dialog").close(); openNote(draftPath);};
          $("file-action-drafts").append(button);
        }
      },
    });
  };
  async function perform(source, to, onCommitted) {
    const path = getContext().current?.path, copying = source.action === "duplicate";
    checkFileActionReady(source.action, source.node.path, to);
    if (source.project !== getContext().project.id) throw new Error("Choose the original notebook first.");
    try {
      beginWrite();
      $("project-select").disabled = true;
      noteEditor.readOnly = true;
      await post(notebookPath(getContext().project.id) + (copying ? "/api/duplicate" : "/api/move"),{from:source.node.path,to,revision:source.revision});
      endWrite();
      onCommitted();
      const {folder} = getContext();
      if (folder !== null) await openFolder(copying ? folder : movedPath(folder,source.node.path,to));
      else if (copying && source.node.kind === "note") await openNote(to);
      else if (path) await openNote(copying ? path : movedPath(path,source.node.path,to));
      await refresh();
      notice(({rename:"Renamed",move:"Moved",duplicate:"Created copy at"})[source.action] + " " + to + ".",true);
    } finally {
      endWrite();
      $("project-select").disabled = false;
      noteEditor.readOnly = !getContext().current?.editable;
    }
  }

  // The open note's inline filename form renames it in place.
  let filenameEdit = null;
  function cancelFilename(restoreFocus = false) {
    filenameEdit = null;
    $("filename-form").hidden = true;
    if (restoreFocus) $("note-details-toggle").focus();
  }
  function canRenameFilename() {
    const {current} = getContext();
    return !!current?.revision && current.editable && canManage() && current.path !== "AGENTS.md" && !current.path.startsWith("Trash/");
  }
  async function openFilename() {
    if (getContext().saving || !canRenameFilename()) return;
    const target = noteView(), state = {target};
    filenameEdit = state;
    $("filename-input").value = target.path.split("/").pop().replace(/\.md$/i, "");
    $("filename-error").textContent = "";
    $("filename-save").disabled = true;
    $("filename-input").disabled = false;
    $("filename-cancel").disabled = false;
    $("filename-form").hidden = false;
    $("filename-input").focus(); $("filename-input").select();
    try {
      const notebook = notebookPath(target.projectId);
      const [entries, snapshot] = await Promise.all([api(notebook+"/api/notes"), api(notebook+"/api/status")]);
      if (filenameEdit !== state || !target.active()) return;
      state.source = {node:{kind:"note",path:target.path}, action:"rename", entries, folders:destinationFolders(entries), revision:snapshot.revision, project:target.projectId};
      $("filename-save").disabled = false;
    } catch (error) {
      if (filenameEdit === state && target.active()) $("filename-error").textContent = error.message;
    }
  }
  $("filename-open").onclick = openFilename;
  $("filename-cancel").onclick = () => cancelFilename(true);
  $("filename-form").onkeydown = event => {
    if (event.key === "Escape" && !getContext().saving) {event.preventDefault(); event.stopPropagation(); cancelFilename(true);}
  };
  document.addEventListener("pointerdown", event => {
    if (filenameEdit && !getContext().saving && !event.target.closest("#filename-form, #filename-open")) cancelFilename();
  });
  $("filename-form").onsubmit = event => {
    event.preventDefault();
    const state = filenameEdit;
    if (!state?.source || !state.target.active() || getContext().saving) return;
    const result = fileActionTarget(state.source.node, "rename", "", $("filename-input").value);
    if (result.path && [...state.source.entries.map(entry=>entry.path), ...state.source.folders].some(path=>path.toLowerCase()===result.path.toLowerCase())) result.error = "That name is already used in this folder.";
    $("filename-error").textContent = result.error || "";
    if (result.error) return;
    busy($("filename-form"), async () => {
      if (!canRenameFilename()) throw new Error("Unlock this note before renaming it.");
      await perform(state.source, result.path, () => cancelFilename());
    }, {
      controls:["filename-input", "filename-save", "filename-cancel"].map($),
      report(error) {
        if (filenameEdit === state && state.target.active()) $("filename-error").textContent = error.message;
        else notice(error.message);
      },
    });
  };
  return {
    setup:setupFileActions,checkReady:checkFileActionReady,close:closeFileMenu,cancelFilename,canRename:canRenameFilename,
    reset(){closeFileMenu();fileAction=null;++fileActionRequest;},
  };
}
