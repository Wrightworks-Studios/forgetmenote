import {$} from './dom.mjs';
import {notebookPath} from './request.mjs';

// The notebook connection indicator. It shows only problems, checks the open
// notebook's status on request, and checks again when the browser comes back
// online or the tab becomes visible. `onStatus` hears each current answer.
export function initConnection({api,getContext,notice,onStatus}) {
  function show(message, healthy) {
    const indicator=$("connection");
    indicator.classList.toggle("problem", healthy === false);
    indicator.hidden = !!healthy;
    indicator.textContent = healthy ? "" : message;
    indicator.setAttribute("aria-label", message);
    indicator.title = message;
  }
  // `owns` is the caller's request check: a superseded request, or one for a
  // notebook no longer open, changes nothing here and returns the answer.
  async function check(owns = () => true) {
    const projectID = getContext().project?.id;
    let v;
    try {
      if (!projectID) throw new Error("Choose a notebook first.");
      v = await api(notebookPath(projectID)+"/api/status");
    } catch (error) {
      if (owns() && getContext().project?.id === projectID) show("Could not check notebook connection", false);
      throw error;
    }
    if (!owns() || getContext().project?.id !== projectID) return v;
    onStatus(v);
    show(v.dirty ? "External changes need attention" : "Local repository · " + v.revision.slice(0, 7), !v.dirty);
    if (v.dirty)
      notice(
        "The notebook contains external changes. Resolve them on the server before saving.",
      );
    return v;
  }
  function refresh() {
    if (getContext().project) check().catch(() => {});
  }
  window.addEventListener("offline", () => show("Offline · drafts remain on this device", false));
  window.addEventListener("online", refresh);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });
  return {check,show};
}
