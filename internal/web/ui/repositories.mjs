import {closeSettingsPage} from './settings-pages.mjs';
import {confirmAction} from './feedback.mjs';
import {$,element as node,busy} from './dom.mjs';
import {poll} from './state.mjs';
import {notebookPath} from './request.mjs';

const size = (n) => new Intl.NumberFormat(void 0, { maximumFractionDigits: 1 }).format((n || 0) / 1048576) + " MiB";
const commit = (value) => value ? value.slice(0, 12) : "Not yet";
function initRepositories({ api, post, getProject, getUser, getProjects = () => [], remember, notice, openDocument }) {
  let connectionProject = new URLSearchParams(location.search).get("repository") === "connect" ? new URLSearchParams(location.search).get("project") : null;
  let state = null, epoch = 0, pollTimer = null, tree = null;
  const operations = new Set();
  const owner = () => getUser()?.admin || getProject()?.role === "owner";
  function clearFiles() {
    tree = null;
    $("repository-files").replaceChildren();
    $("repository-file-filter").value = "";
    $("repository-file-tools").hidden = true;
  }
  function renderStatus(next) {
    if (state?.id !== next.id || !next.connected) clearFiles();
    state = next;
    $("repository-status").textContent = !state.configured ? "GitHub sources are not configured on this server." : state.connected ? state.repository.full_name + " · " + state.repository.default_branch + (state.paused ? " · Paused" : state.pending ? " · Refresh queued" : "") : "No repository connected.";
    $("repository-owner-controls").hidden = !state.configured || !owner();
    $("repository-connect").hidden = !!state.connected;
    $("repository-install").hidden = !!state.connected;
    if (state.installUrl) $("repository-install").href = state.installUrl;
    $("repository-active-controls").hidden = !state.connected;
    $("repository-refresh").disabled = !!state.paused || operations.has($("repository-refresh"));
    $("repository-pause").textContent = state.paused ? "Resume" : "Pause";
    $("repository-detail").replaceChildren();
    if (state.connected) {
      for (const [label, value] of [["Source downloaded", commit(state.downloaded)], ["Last source refresh", state.lastRefresh && !state.lastRefresh.startsWith("0001") ? new Date(state.lastRefresh).toLocaleString() : "Not yet"], ["Source cache", size(state.cacheBytes)], ["Retained evidence", size(state.evidenceBytes)]]) {
        $("repository-detail").append(node("dt", label), node("dd", value));
      }
      if (state.baselineRequired) $("repository-detail").append(node("dt", "Coverage"), node("dd", "The earlier source revision is unavailable; a complete change comparison may require a new baseline."));
    }
    $("repository-error").textContent = [state.error,state.index?.error,state.index?.embeddingError,state.index?.mapError].filter(Boolean).join(" · ");
    $("repository-index-form").hidden=!state.connected||!owner();
    if(state.connected){const index=state.index||{};for(const [label,value] of [["Keyword indexed",commit(index.commit)],["Semantic coverage",`${index.embedded||0} / ${index.chunks||0} chunks`],["Index storage",size(index.bytes)],["Repository map",`${index.mapEntries||0} files · ${index.mapClassified||0} cached AI decisions`]])$("repository-detail").append(node("dt",label),node("dd",value));
      if(!$("repository-index-form").contains(document.activeElement)){const c=state.indexConfig||{};$("repository-index-include").value=(c.include||[]).join("\n");$("repository-index-exclude").value=(c.exclude||[]).join("\n");}
    }
    $("repository-exclusions").querySelector("ul").replaceChildren(...(state.index?.exclusions||[]).map(e=>node("li",e.path+" — "+e.reason)));
    $("repository-browse").hidden = !state.connected || !state.downloaded;
  }
  function schedulePoll(version, selected) {
    pollTimer?.stop();
    pollTimer = poll(async () => {
      try {
        if (!document.querySelector('[data-settings-panel="sources"]').hidden && !operations.size) {
          const next = await api(notebookPath(getProject().id)+"/api/repository");
          if (version !== epoch || getProject()?.id !== selected) return;
          renderStatus(next);
        }
      } catch (error) {
        if (version === epoch) $("repository-error").textContent = error.message;
      }
    }, {every: 2000, active: () => version === epoch && getProject()?.id === selected && $("settings-dialog").open});
  }
  $("settings-dialog").addEventListener("close", () => { pollTimer?.stop(); epoch++; });
  async function open() {
    pollTimer?.stop();
    const version = ++epoch;
    const selected = getProject()?.id;
    if (!selected) return;
    $("repository-status").textContent = "Loading repository source\u2026";
    clearFiles();
    $("repository-select-form").hidden = true;
    try {
      const next = await api(notebookPath(getProject().id)+"/api/repository");
      if (version !== epoch || getProject()?.id !== selected) return;
      renderStatus(next);
      if (connectionProject === selected && owner() && !state.connected) {
        const choices = await api(notebookPath(getProject().id)+"/api/repository/choices");
        if (version !== epoch || getProject()?.id !== selected) return;
        $("repository-choice").replaceChildren();
        for (const choice of choices) {
          const option = node("option", choice.full_name + (choice.private ? " (private)" : ""));
          option.value = JSON.stringify({ repository: choice.id, installation: choice.installation });
          $("repository-choice").append(option);
        }
        $("repository-select-form").hidden = false;
        $("repository-select-submit").disabled = !choices.length;
        if (!choices.length) $("repository-error").textContent = "No repositories are available. Install the GitHub App on a repository you can access, then connect again.";
      }
    } catch (error) {
      if (version === epoch) $("repository-status").textContent = error.message;
    } finally {
      if (version === epoch) schedulePoll(version, selected);
    }
  }
  // Refresh stays disabled while the repository is paused.
  function perform(button, fn) {
    const selected = getProject()?.id;
    operations.add(button);
    return busy(button, fn, {report: $("repository-error"), current: () => selected === getProject()?.id}).finally(() => {
      operations.delete(button);
      button.disabled = button.id === "repository-refresh" && !!state?.paused;
    });
  }
  $("repository-index-form").onsubmit=event=>{event.preventDefault();perform(event.submitter,async()=>{const paths=id=>$(id).value.split("\n").map(p=>p.trim()).filter(Boolean);await post(notebookPath(getProject().id)+"/api/repository/index-config",{generation:state.generation,config:{...state.indexConfig,include:paths("repository-index-include"),exclude:paths("repository-index-exclude")}});await post(notebookPath(getProject().id)+"/api/repository/refresh",{});await open();});};
  $("repository-connect").onclick = (event) => perform(event.currentTarget, async () => {
    remember();
    const result = await post(notebookPath(getProject().id)+"/api/repository/connect", {});
    location.assign(result.url);
  });
  $("repository-select-form").onsubmit = (event) => {
    event.preventDefault();
    perform(event.submitter, async () => {
      await post(notebookPath(getProject().id)+"/api/repository/select", { ...JSON.parse($("repository-choice").value), shareWithNotebook: $("repository-share").checked });
      connectionProject = null;
      const url = new URL(location.href);
      url.searchParams.delete("repository");
      history.replaceState(history.state, "", url);
      await open();
    });
  };
  $("repository-refresh").onclick = (event) => perform(event.currentTarget, async () => {
    const version = epoch, selected = getProject()?.id;
    $("repository-refresh").textContent = "Refreshing…";
    $("repository-error").textContent = "";
    try {
      await post(notebookPath(getProject().id)+"/api/repository/refresh", {});
      if (version !== epoch || selected !== getProject()?.id) return;
      const next = await api(notebookPath(getProject().id)+"/api/repository");
      if (version === epoch && selected === getProject()?.id) renderStatus(next);
    } finally { $("repository-refresh").textContent = "Refresh now"; }
  });
  $("repository-pause").onclick = (event) => perform(event.currentTarget, async () => {
    await post(notebookPath(getProject().id)+"/api/repository/control", { action: state.paused ? "resume" : "pause" });
    await open();
  });
  $("repository-disconnect").onclick = (event) => perform(event.currentTarget, async () => {
    if (!await confirmAction("Disconnect this repository? New source reads and refreshes will stop. Notebook notes and retained evidence will be preserved.")) return;
    await post(notebookPath(getProject().id)+"/api/repository/control", { action: "disconnect" });
    await open();
  });
  $("repository-browse").onclick = (event) => perform(event.currentTarget, async () => {
    const project = getProject()?.id, version = epoch, revision = state.downloaded, source = state.id;
    const entries = await api(notebookPath(getProject().id)+"/api/repository/tree?" + new URLSearchParams({ source, revision }));
    if (project !== getProject()?.id || version !== epoch || source !== state.id) return;
    tree = { entries, source, revision };
    $("repository-file-tools").hidden = false;
    renderFiles();
  });
  function renderFiles() {
    if (!tree) return;
    const { source, revision } = tree;
    const query = $("repository-file-filter").value.trim().toLowerCase();
    const entries = tree.entries.filter(entry => entry.path.toLowerCase().includes(query));
    $("repository-file-count").textContent = entries.length + " of " + tree.entries.length + " files";
    $("repository-tree-revision").textContent = "Browsing " + commit(revision);
    $("repository-files").replaceChildren();
    for (const entry of entries) {
      const row = node("li");
      if (entry.supported) {
        const button = node("button", entry.path);
        button.className = "quiet";
        button.onclick = () => file(source, revision, entry.path).catch((error) => notice(error.message));
        row.append(button);
      } else if(["100644","100755"].includes(entry.mode)){const button=node("button",entry.path+" — read first 200 lines");button.className="quiet";button.onclick=()=>file(source,revision,entry.path,{startLine:1,endLine:200}).catch(error=>notice(error.message));row.append(button);} else row.textContent = entry.path + " — unsupported";
      $("repository-files").append(row);
    }
    if (!entries.length) $("repository-files").append(node("li", "No matching source files."));
  }
  $("repository-file-filter").oninput = renderFiles;
  async function file(source, revision, path, range = {}) {
    closeSettingsPage($("settings-dialog"));
    return openDocument({project:range.project||getProject()?.id,origin:'repository',source,revision,path,...range});
  }
  return { open, file, projectChanged() {
    pollTimer?.stop();
    epoch++;
    state = null;
    clearFiles();
  } };
}
export {
  initRepositories
};
