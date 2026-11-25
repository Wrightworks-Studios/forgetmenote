import {openDialog} from './overlay.mjs';
import {requestSequence} from './state.mjs';
import {notebookPath} from './request.mjs';
import {$} from './dom.mjs';

// Note and folder locks. The caller owns save state: `beginWrite`/`endWrite`
// bracket the lock change, and `refresh` reloads search and notebook status.
export function initProtection({api,post,getContext,canManage,dirty,beginWrite,endWrite,restoreMode,openNote,refresh}) {
  let selectedLock = null, lockRequests=requestSequence();
  async function loadLockScope() {
    const selected=getContext().project,account=getContext().user?.id,owns=lockRequests.begin(()=>getContext().project===selected&&getContext().user?.id===account&&$('protection-dialog').open);
    selectedLock = null;
    $("lock-change").disabled = true;
    $("lock-error").hidden = true;
    $("lock-rules").replaceChildren();
    $("lock-description").textContent = "Loading protection…";
    try {
      const info = await api(notebookPath(getContext().project.id)+"/api/locks?path=" + encodeURIComponent($("lock-scope").value));
      if (!owns()) return;
      selectedLock = info;
      const exact = info.rules.some(rule => rule.path === info.path);
      const inherited = info.rules.some(rule => rule.path !== info.path);
      $("lock-description").textContent = info.rules.length
        ? "Read-only" + (inherited ? " · inherited protection" : "")
        : "No lock on this path.";
      for (const rule of info.rules) {
        const item = document.createElement("li");
        item.textContent = rule.path + " — select this path above to remove its lock.";
        $("lock-rules").append(item);
      }
      $("lock-change").hidden = !canManage();
      $("lock-change").textContent = exact ? "Unlock this path" : "Lock this path";
      $("lock-change").disabled = getContext().saving || dirty() || (!exact && inherited);
      if (!canManage()) $("lock-description").textContent += " Only an Owner or Admin can change locks.";
      if (dirty()) $("lock-description").textContent += " Save or reconcile your draft before changing locks.";
    } catch (error) {
      if (!owns()) return;
      $("lock-description").textContent = "Protection could not be loaded.";
      $("lock-error").textContent = error.message;
      $("lock-error").hidden = false;
    }
  }
  async function openProtection(target = getContext().current ? {path:getContext().current.path,kind:"note"} : null) {
    if (!getContext().project || getContext().saving) return;
    openDialog($("protection-dialog"));
    $("lock-scope").replaceChildren();
    $("lock-change").disabled = true;
    $("lock-description").textContent = "Loading protection…";
    $("lock-rules").replaceChildren();
    $("lock-error").hidden = true;
    try {
      const all = await api(notebookPath(getContext().project.id)+"/api/locks");
      const paths = new Map();
      if (target) {
        paths.set(target.path, (target.kind === "folder" ? "Folder · " : "Note · ") + target.path);
        const parts = target.path.split("/");
        parts.pop();
        while (parts.length) {
          const path = parts.join("/");
          paths.set(path, "Folder · " + path);
          parts.pop();
        }
      }
      for (const rule of all.rules)
        if (!paths.has(rule.path)) paths.set(rule.path, "Locked · " + rule.path);
      $("lock-scope").replaceChildren(...[...paths].map(([path,label])=>new Option(label,path)));
      if (paths.size) await loadLockScope();
      else {
        selectedLock = null;
        $("lock-description").textContent = "No notebook locks. Open a note to lock it or a parent folder.";
        $("lock-change").hidden = true;
      }
    } catch (error) {
      $("lock-description").textContent = error.message;
    }
  }
  $("protection-open").onclick = () => openProtection();
  $("lock-scope").onchange = loadLockScope;
  $("lock-change").onclick = async () => {
    if (!selectedLock || !canManage() || getContext().saving || dirty() || $("lock-change").disabled) return;
    const notePath = getContext().current?.path, selection = selectedLock;
    beginWrite();
    $("lock-change").disabled = true;
    $("lock-scope").disabled = true;
    restoreMode();
    try {
      const exact = selection.rules.some(rule => rule.path === selection.path);
      await post(notebookPath(getContext().project.id)+"/api/locks", {path: selection.path, locked: !exact, revision: selection.revision});
      $("protection-dialog").close();
      endWrite();
      if (notePath) await openNote(notePath);
      await refresh();
    } catch (error) {
      $("lock-error").textContent = error.message + " Close and reopen protection to load current settings.";
      $("lock-error").hidden = false;
      selectedLock = null;
    } finally {
      endWrite();
      $("lock-scope").disabled = false;
      restoreMode();
    }
  };
  return {open:openProtection};
}
