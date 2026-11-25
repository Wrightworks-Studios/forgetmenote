import {$} from './dom.mjs';
import {openDialog} from './overlay.mjs';
import {confirmAction} from './feedback.mjs';
import {requestSequence} from './state.mjs';
import {timeElement} from './time.mjs';
import {notebookPath} from './request.mjs';
import {closeSettingsPage} from './settings-pages.mjs';

// The Version history dialog for the open note, its folders or the notebook.
// Choosing one of the note's versions shows it read-only (`showRevision`) or
// loads it into the editor as a private draft that saving restores. The caller
// owns the editor, the displayed view (`retireView`) and drafts (`remember`);
// any newer edit or navigation cancels a pending choice (`cancelSelection`).
export function initHistory({api,notice,state,getContext,noteView,noteEditor,dirty,canManage,canEdit,beginNavigation,retireView,showRevision,setMode,remember,preview,countWords,openPurge}) {
  const dialog = $("history-dialog"), requests = requestSequence();
  let selection = 0;
  function cancelSelection() {selection++;}
  function attribution(row) {
    const label=document.createElement('span');label.className='history-attribution';
    const source=row.source==='mcp'?'MCP'+(row.client?' · Client '+row.client:''):row.source==='browser'?'Browser':row.source==='system'?'System':'Legacy / external';
    label.textContent=(row.authorDisplayName || 'Contributor')+' · '+source;
    if(row.authoredAt)label.append(document.createTextNode(' · '),timeElement(row.authoredAt));
    label.title='Saved version '+row.revision;
    return label;
  }
  async function choose(row, target) {
    if (getContext().saving || !target.active() || !dialog.open) return;
    const draftVersion=getContext().inputVersion;
    cancelSelection();
    if (
      dirty() &&
      !await confirmAction("Replace this draft with the selected historical version?")
    )
      return;
    if(getContext().saving || draftVersion!==getContext().inputVersion || !target.active() || !dialog.open)return;
    beginNavigation();
    const chosen=selection, note=getContext().current, edited=getContext().inputVersion, content=noteEditor.value;
    let installed=false;
    const ownsSelection=()=>{
      const {saving,current,inputVersion}=getContext();
      return chosen===selection && target.active() && dialog.open && !saving && current===note && edited===inputVersion && (installed || content===noteEditor.value);
    };
    try {
      const old = await api(
        notebookPath(getContext().project.id) + "/api/note?path=" +
          encodeURIComponent(row.path || target.path) +
          "&revision=" +
          row.revision,
      );
      if (!ownsSelection()) return;
      retireView();
      if (!note.editable) {
        showRevision(old);
        setMode(false);
        notice(
          "Viewing an older revision. Open the note again to return to the current version.",
        );
        dialog.close();
        return;
      }
      // Choosing history is private intent even when it equals Git but
      // differs from newer accepted live text.
      note.privateDraft=true;
      noteEditor.value = old.content;
      installed=true;
      setMode(true);
      remember();
      await preview();
      if (!ownsSelection()) return;
      countWords();
      state("Historical version loaded · save to restore");
      dialog.close();
    } catch (e) {
      if (ownsSelection()) notice(e.message);
    }
  }
  async function load() {
    cancelSelection();
    const request = requests.next(), scope = $("history-scope").value, {project,current} = getContext();
    const notePath = current?.path, noteScope = scope === notePath && !!current?.revision;
    const target = noteView(notePath);
    $("history-list").replaceChildren();
    $("history-purge").hidden = !noteScope || !canManage() || !canEdit();
    $("history-help").textContent = noteScope
      ? "Restoring creates a new version."
      : "Choose a note to view or restore its versions.";
    try {
      if (!project) throw new Error("Choose a notebook first.");
      const rows = await api(notebookPath(project.id)+"/api/history?path=" + encodeURIComponent(scope));
      if (!requests.owns(request) || !target.active() || !dialog.open) return;
      $("history-list").replaceChildren();
      for (const row of rows) {
        const b = document.createElement(noteScope ? "button" : "p");
        b.className = noteScope ? "quiet" : "history-entry";
        b.textContent = row.revision.slice(0, 7) + " · " + row.message;
        b.append(attribution(row));
        if (noteScope) b.onclick = () => choose(row, target);
        $("history-list").append(b);
      }
      if (!rows.length) $("history-help").textContent = "No history for this scope yet.";
    } catch (e) {
      if (requests.owns(request) && target.active() && dialog.open) $("history-help").textContent = e.message;
    }
  }
  function open(projectOnly = false) {
    const {project,saving,current,folder} = getContext();
    if (!project || saving) return;
    const options = [];
    if (current && !projectOnly) {
      if (current.revision) options.push(new Option("Note · " + current.path, current.path));
      const parts = current.path.split("/"); parts.pop();
      while (parts.length) {
        const path = parts.join("/"); options.push(new Option("Folder · " + path,path)); parts.pop();
      }
    }
    if(folder !== null && !projectOnly) {
      const parts=folder.split('/').filter(Boolean);
      while(parts.length){const path=parts.join('/');options.push(new Option("Folder · "+path,path));parts.pop();}
    }
    options.push(new Option("Notebook · " + project.name, ""));
    $("history-scope").replaceChildren(...options);
    openDialog(dialog);
    load();
  }
  $("history-open").onclick = () => open();
  $("project-history-open").onclick = () => { closeSettingsPage($("settings-dialog")); open(true); };
  $("history-purge").onclick = () => {
    const {current,saving} = getContext(), path = current?.path;
    if (!path || saving) return;
    dialog.close();
    openPurge({path, kind: "versions"});
  };
  $("history-scope").onchange = load;
  for(const event of ['close','cancel'])dialog.addEventListener(event,()=>{cancelSelection();requests.next()});
  return {cancelSelection};
}
