import {closeSettingsPage} from './settings-pages.mjs';
import {openDialog} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {originalURL} from './sources.mjs';
import { resolveLink } from "./editor.mjs";
import {$,installHTML,contentElement,element as el,actionButton,keepTyped} from './dom.mjs';
import {notebookPath,assetURL} from './request.mjs';
import {moduleOn} from './state.mjs';
function citationTarget(source, href) {
  const link = resolveLink(source, href);
  if (!link) return null;
  const revision = new URLSearchParams(href.split("#")[0].split("?")[1] || "").get("fmn-rev");
  return revision && /^[a-f0-9]{40}$/.test(revision) ? { ...link, revision } : link;
}
function lineDiff(before, after) {
  const a = before.split("\n"), b = after.split("\n");
  if (a.length * b.length > 25e4) return [...a.map((text) => ({ kind: "removed", text })), ...b.map((text) => ({ kind: "added", text }))];
  const rows = Array.from({ length: a.length + 1 }, () => new Uint16Array(b.length + 1));
  for (let i2 = a.length - 1; i2 >= 0; i2--) for (let j2 = b.length - 1; j2 >= 0; j2--) rows[i2][j2] = a[i2] === b[j2] ? 1 + rows[i2 + 1][j2 + 1] : Math.max(rows[i2 + 1][j2], rows[i2][j2 + 1]);
  const result = [];
  let i = 0, j = 0;
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) {
      result.push({ kind: "context", text: a[i++] });
      j++;
    } else if (i < a.length && (j === b.length || rows[i + 1][j] >= rows[i][j + 1])) result.push({ kind: "removed", text: a[i++] });
    else result.push({ kind: "added", text: b[j++] });
  }
  return result;
}
function indexMessage(status) {
  if (status.state === "disabled") return status.config?.model
    ? "Semantic search is paused."
    : "Embeddings are not enabled.";
  const progress = `${status.indexed} of ${status.total} passages indexed.`;
  if (status.message) return progress + " " + status.message;
  if (!status.browserSearch) return progress + " Browser search needs a query-embedding endpoint.";
  return progress + (status.state === "ready" ? " Ready for semantic search." : " Indexing…");
}
function historyResultDetail(hit) {
  const count=hit.occurrenceCount || hit.occurrences?.length || 1;
  return `${hit.path} · ${hit.revision.slice(0,7)} · ${new Date(hit.committedAt).toLocaleDateString()} · ${hit.current ? "Current version" : "Historical version"}${count>1 ? ` · ${count} recorded versions` : ""}`;
}
function initKnowledge({ api, post, getProject, getUser, getNote, canEdit, canManage, openNote, openNotebook, openCode, archiveSource, refresh, onSearchStatus }) {
  const dialog = $("knowledge-dialog"), body = $("knowledge-body");
  let generation = 0, scope = "", account = "", indexStatus = null;
  let reviewRequest = 0;
  function reviewAttention(data) {
    const count = (data.findings || []).filter(f => !f.comment.resolved).length
      + (data.proposals || []).filter(p => p.status === "proposed").length;
    $("review-count").hidden = !count;
    $("review-count").textContent = count ? String(count) : "";
    $("review-count").title = count + " open findings and proposed changes";
  }
  async function refreshReview() {
    const id = getProject()?.id, request = ++reviewRequest;
    if (!id) { reviewAttention({}); return; }
    try {
      const data = await api(notebookPath(id) + "/api/knowledge/review");
      if (request === reviewRequest && getProject()?.id === id) reviewAttention(data);
    } catch {
      if (request === reviewRequest && getProject()?.id === id) {
        $("review-count").hidden = false;
        $("review-count").textContent = "unavailable";
        $("review-count").title = "Could not check for open findings or proposed changes. Open Review to retry.";
      }
    }
  }
  function button(text, action) {
    return actionButton(text, () => action(), {
      owner: () => {const started = generation, project = scope;return () => active(started) && scope === project;},
      report: e => error(e.message),
    });
  }
  function error(text) { feedback($("knowledge-error"), text); }
  function begin(title) {
    generation++;
    scope = getProject()?.id;account = getUser()?.id;
    body.replaceChildren();
    error("");
    $("knowledge-title").textContent = title;
    $("review-sections").hidden = title !== "Review";
    $("review-checks").hidden = true;body.hidden=false;
    if (!dialog.open) openDialog(dialog);
    return generation;
  }
  function active(g) {
    return g === generation && getProject()?.id === scope && getUser()?.id === account && dialog.open && !body.hidden;
  }
  function openView(action, ...args) {
    const pending = action(...args), g = generation, project = scope, user = account;
    return Promise.resolve(pending).catch(problem => {
      if (active(g) && scope === project && account === user) error(problem.message);
    });
  }
  function read(path) {
    return api(notebookPath(scope) + "/api/knowledge/" + path);
  }
  function write(path, value) {
    return post(notebookPath(scope) + "/api/knowledge/" + path, value);
  }
  function section(title) {
    const s = el("section", void 0, "knowledge-section");
    s.append(el("h3", title));
    body.append(s);
    return s;
  }
  function targetButton(path, revision, label, hash = "") {
    return button(label || path, async () => {
      if (revision) await openView(historical, path, revision, hash);
      else {
        dialog.close();
        await openNote(path, hash);
      }
    });
  }
  async function historical(path, revision, hash = "", hit = null) {
    const g = begin("Historical version"), projectId = scope;
    body.append(el("p", path + " \xB7 " + revision.slice(0, 7) + (hash ? " \xB7 " + hash : ""), "small"));
    const doc = await api(notebookPath(scope) + "/api/note?path=" + encodeURIComponent(path) + "&revision=" + revision);
    if (!active(g)) return;
    if(hit?.text){const passage=el("pre",hit.text);passage.id="historical-passage";body.append(passage);}
    const actions=el("div",undefined,"knowledge-actions"), copied=el("p","","small");copied.setAttribute("role","status");
    async function copy(text){await navigator.clipboard.writeText(text);if(active(g))copied.textContent="Copied.";}
    const copyPassage=button(hit ? "Copy passage" : "Copy Markdown",()=>copy(hit?.text || doc.content));copyPassage.id="historical-copy";actions.append(copyPassage);
    const citation=hit?.citation || `/${path.split('/').map(encodeURIComponent).join('/')}?fmn-rev=${revision}`;
    const citationLabel=(hit?.heading || path).replace(/[\\\[\]]/g,'\\$&');
    const copyCitation=button("Copy citation",()=>copy(`[${citationLabel}](${citation})`));copyCitation.id="historical-copy-citation";actions.append(copyCitation);
    const comparison=el("section");comparison.id="historical-comparison";
    async function compareWith(target){
      const latest=await api(notebookPath(projectId)+`/api/note?path=${encodeURIComponent(target)}`);
      if(!active(g))return;
      comparison.replaceChildren(el("h3",`Historical version → current committed note · ${target}`));
      if(doc.content.length+latest.content.length>131072 || doc.content.split('\n').length+latest.content.split('\n').length>2000){
        for(const [label,text] of [["Historical Markdown",doc.content],["Current committed Markdown",latest.content]]){const field=keepTyped(el('textarea'));field.readOnly=true;field.rows=12;field.value=text;field.setAttribute('aria-label',label);comparison.append(el('p',label),field);}
      }else{const diff=el('pre',undefined,'knowledge-diff');for(const row of lineDiff(doc.content,latest.content))diff.append(el('span',(row.kind==='added'?'+ ':row.kind==='removed'?'- ':'  ')+row.text+'\n',row.kind));comparison.append(diff);}
    }
    const compare=button("Compare with current note",()=>compareWith(path));compare.id="historical-compare";compare.disabled=true;actions.append(compare);
    const open=getNote();if(open?.path && open.path!==path)actions.append(button("Compare with open note",()=>compareWith(open.path)));
    body.append(actions,copied,comparison);
    if(hit?.occurrences?.length>1){
      const versions=el('details');versions.append(el('summary',`Recorded in ${hit.occurrenceCount || hit.occurrences.length} committed versions`));
      if(hit.occurrencesTruncated)versions.append(el('p',`Showing ${hit.occurrences.length} recorded versions. Narrow the path, tag or status filters to inspect others.`,'small'));
      for(const occurrence of hit.occurrences)versions.append(targetButton(occurrence.path,occurrence.revision,`${occurrence.path} · ${occurrence.revision.slice(0,7)} · ${new Date(occurrence.committedAt).toLocaleString()}`));
      body.append(versions);
    }
    if(doc.metadata?.source) {
      const url=originalURL(scope,doc.metadata.source,revision);
      if(url){const original=el('a','Download original at this revision');original.href=url;body.append(original);}
      const view=el('div',undefined,'prose citation-source');
      installHTML(view,doc.html,{
        prepare:root=>{for(const image of root.querySelectorAll('img'))image.remove();},
        resolve:href=>citationTarget(path,href),
        follow(a,link){
          if(link.project)return ()=>{dialog.close();openNotebook(link);};
          if(!link.path.endsWith('.md')){a.href=assetURL(scope,link.path,link.revision||revision);return;}
          a.href='#';return ()=>openView(historical,link.path,link.revision||revision,link.hash);
        },
      });
      body.append(view);
      contentElement(view,hash)?.scrollIntoView({block:'start'});
    } else body.append(el("pre", doc.content, "citation-content"));
    try {
      await api(notebookPath(projectId)+`/api/note?path=${encodeURIComponent(path)}`);
      if(active(g)){body.append(targetButton(path,"","Open current note"));compare.disabled=false;}
    } catch {
      if(active(g)){compare.disabled=true;body.append(el('p','No current note is available at this historical path. It may have been moved or removed.','small'));}
    }
  }
  async function references(host, owns, close, onCount=()=>{}) {
    const note=getNote(), project=getProject()?.id;
    if(!note)return;
    host.replaceChildren(el('p','Loading references…','small'));
    const action=(label,run)=>actionButton(label,()=>run(),{owner:()=>owns()&&owns,report:e=>host.append(el('p',e.message,'small'))});
    const target=(path,revision,label,hash='')=>action(label || path,async()=>{close();if(revision)await openView(historical,path,revision,hash);else await openNote(path,hash);});
    try {
      const data=await api(notebookPath(project)+'/api/knowledge/evidence?path='+encodeURIComponent(note.path));
      if(!owns())return;
      onCount(data.sources.length+data.backlinks.length);
      host.replaceChildren(el('p',note.path,'small'));
      for(const [title,refs] of [['Sources',data.sources],['Referenced by',data.backlinks]]){
        const section=el('section',undefined,'knowledge-section');section.append(el('h3',title));host.append(section);
        if(!refs.length)section.append(el('p','No resolved links yet.','small'));
        for(const ref of refs){
          const item=el('article',undefined,'knowledge-card');
          if((ref.project||ref.sourceProject)&&!ref.state.includes('unavailable')){
            const backlink=title==='Referenced by';item.append(action((ref.notebook?ref.notebook+' → ':'')+(ref.title||ref.label||ref.path),()=>{close();return openNotebook({project:backlink?ref.sourceProject:ref.project,path:backlink?ref.note:ref.path,noteId:backlink?'':ref.noteId,revision:backlink?'':ref.revision,heading:backlink?'':ref.heading});}));
          }else if(ref.external){
            const a=el('a',ref.label || ref.url);a.href=ref.url;a.target='_blank';a.rel='noopener noreferrer';item.append(a);
            if(ref.code)item.append(action('Inspect code',()=>openCode(ref.code)));
            else if(ref.path)item.append(target(ref.path,ref.revision,'Open archived copy'));
            else if(canEdit() && archiveSource && moduleOn(getUser(),'sources'))item.append(action('Archive source',()=>{close();archiveSource(ref.url);}));
          }else if(ref.state.includes('unavailable'))item.append(el('span',ref.label || ref.path));
          else item.append(target(title==='Referenced by'?ref.note:ref.path,title==='Sources'?ref.revision:'',ref.label || ref.note || ref.path,ref.heading));
          item.append(el('p',ref.state+(ref.scope?' · '+ref.scope:''),'small'));section.append(item);
        }
      }
    }catch(e){if(owns())host.replaceChildren(el('p',e.message,'small'));}
  }
  async function review(findingID, selected = "findings") {
    const g = begin("Review"), projectID = scope;
    async function decide(path, value, refreshWorkspace = false) {
      if (!active(g)) return;
      await post(notebookPath(projectID) + "/api/knowledge/" + path, value);
      if (getProject()?.id !== projectID) return;
      // A completed decision updates counts without reclaiming a newer view.
      if (active(g)) await openView(review, undefined, selected);
      else await refreshReview();
      if (refreshWorkspace && getProject()?.id === projectID) await refresh();
    }
    for(const button of $("review-sections").querySelectorAll("button"))button.setAttribute("aria-pressed",String(button.dataset.reviewSection===selected));
    body.append(el("p", getProject().name + " · Notebook review", "settings-scope"));
    const data = await read("review");
    if (!active(g)) return;
    ++reviewRequest;
    reviewAttention(data);
    const findings = section("Findings");findings.hidden=selected!=="findings";
    if (!data.findings.length) findings.append(el("p", "No findings. Review checks saved notes for unsupported claims and contradictory sources. An administrator can enable AI reviews in Administration.", "small"));
    for (const f of data.findings) {
      const c = f.comment, detail = c.finding, item = el("article", void 0, "knowledge-card");
      if(c.id===findingID){item.tabIndex=-1;queueMicrotask(()=>{if(active(g))item.focus();});}
      item.append(el("h4", detail.title), el("p", (c.resolved ? detail.dismissed ? "Dismissed" : "Resolved" : "Open") + " \xB7 Agent judgment" + (detail.stale ? " \xB7 Source changed" : ""), "knowledge-status"), targetButton(f.path, "", f.path), el("p", c.text));
      for (const ref of detail.evidence || []) {
        if (ref.path) item.append(targetButton(ref.path, ref.revision, ref.path, ref.heading));
        else if (ref.url) {
          const a = el("a", ref.url);
          a.href = ref.url;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          item.append(a);
        }
        if (ref.explanation) item.append(el("p", ref.explanation, "small"));
      }
      item.append(targetButton(detail.sourcePath || f.path, detail.sourceRevision, "Read reviewed version"));
      if (!c.resolved && (canEdit() || c.author === getUser()?.id)) {
        const actions = el("div", void 0, "knowledge-actions");
        for (const decision of ["resolve", "dismiss"]) actions.append(button(decision === "resolve" ? "Resolve" : "Dismiss", async () => {
          await decide("findings/decide", { path: f.path, id: c.id, revision: f.revision, decision }, true);
        }));
        item.append(actions);
      }
      findings.append(item);
    }
    const proposals = section("Proposed changes");proposals.hidden=selected!=="proposals";
    if (!data.proposals.length) proposals.append(el("p", "No proposed groups. Notebook review can suggest related notes to organize together; you approve changes before they are applied.", "small"));
    for (const p of data.proposals) {
      const item = el("article", void 0, "knowledge-card");
      item.append(el("h4", p.group.summary), el("p", p.status + (p.stale ? " \xB7 Base content changed" : "") + " \xB7 Proposed by " + p.provenance.name + (p.acceptedBy ? " \xB7 Decision by " + (p.acceptedIdentity.name || p.acceptedBy) : ""), "knowledge-status"));
      for (const edit of p.group.edits) {
        const details = el("details"), summary = el("summary", edit.path + (edit.delete ? " \xB7 Delete" : ""));
        details.append(summary);
        let rendered = false;
        details.ontoggle = () => {
          if (!details.open || rendered) return;
          rendered = true;
          const before=p.before[edit.path]||"",after=edit.delete?"":edit.content;
          if(before.length+after.length>131072 || before.split("\n").length+after.split("\n").length>2000){
            details.append(el("p","Showing full versions for this large note.","small"));
            for(const [label,content] of [["Before",before],["After",after]]){const field=keepTyped(el("textarea"));field.value=content;field.readOnly=true;field.rows=12;field.setAttribute("aria-label",label+" · "+edit.path);details.append(el("p",label),field)}
            return;
          }
          const pre = el("pre", void 0, "knowledge-diff");
          for (const row of lineDiff(p.before[edit.path] || "", edit.delete ? "" : edit.content)) {
            pre.append(el("span", (row.kind === "added" ? "+ " : row.kind === "removed" ? "- " : "  ") + row.text + "\n", row.kind));
          }
          details.append(pre);
        };
        item.append(details);
      }
      if (p.status === "proposed") {
        const actions = el("div", void 0, "knowledge-actions");
        const deletes = p.group.edits.some(edit => edit.delete);
        if (deletes && !canManage()) actions.append(el("p", "Only an Owner or Admin can accept a proposal that moves notes to Trash.", "small"));
        if (deletes ? canManage() : canEdit()) actions.append(button("Accept group", async () => {
          await decide("proposals/decide", { id: p.id, decision: "accept" }, true);
        }));
        if (canEdit() || p.author === getUser()?.id) actions.append(button("Dismiss", async () => {
          await decide("proposals/decide", { id: p.id, decision: "dismiss" });
        }));
        item.append(actions);
      }
      if (["applied","dismissed"].includes(p.status) && canEdit()) item.append(button("Archive",async()=>{await decide("proposals/decide",{id:p.id,decision:"archive"});}));
      proposals.append(item);
    }
    const checkpoints = section("Last reported processing");checkpoints.hidden=selected!=="activity";
    if (!data.checkpoints.length) checkpoints.append(el("p", "No processing checkpoints yet.", "small"));
    for (const cp of data.checkpoints) {
      const item = el("article", void 0, "knowledge-card");
      item.append(el("h4", cp.consumer), el("p", cp.summary), el("p", cp.user + " \xB7 " + new Date(cp.completedAt).toLocaleString() + " \xB7 " + cp.revision.slice(0, 7), "small"));
      checkpoints.append(item);
    }
  }
  async function instructions() {
    closeSettingsPage($("settings-dialog"));
    const g = begin("Notebook agent instructions");
    const data = await read("instructions");
    if (!active(g)) return;
    const label = el("label", "Versioned as AGENTS.md");
    label.htmlFor = "instructions-text";
    const input = keepTyped(el("textarea"));
    input.id = "instructions-text";
    input.rows = 16;
    input.value = data.content;
    input.readOnly = !canManage();
    body.append(label, input);
    if (canManage()) body.append(button("Save instructions", async () => {
      const saved = await write("instructions", { content: input.value, revision: data.revision });
      data.revision = saved.revision;
      error("");
      await refresh();
      body.append(el("p", "Instructions saved.", "small"));
    }));
  }
  async function semantic() {
    closeSettingsPage($("settings-dialog"));
    const g = begin("Semantic search");
    const status = await read("status");
    if (!active(g)) return;
    indexStatus = status;
    body.append(el("p", indexMessage(status), "small"));
    body.append(button("Refresh status", () => openView(semantic)));
    body.append(el("p", "AI settings are managed in Administration → AI features.", "small"));
    if (status.config.model) body.append(el("p", `Model: ${status.config.model}`, "small"));
  }

  async function update() {
    const project = getProject();
    $("review-open").disabled = !project;
    $("sources-open").disabled = !getNote();
    if (!project) { onSearchStatus(null); return; }
    const id = project.id;
    try {
      indexStatus = await api(notebookPath(id) + "/api/knowledge/status");
      if (getProject()?.id !== id) return;
      onSearchStatus(indexStatus);
    } catch {
    }
  }
  for (const [id, fn] of [["review-open", review], ["instructions-open", instructions], ["semantic-settings-open", semantic]]) $(id).onclick = () => openView(fn);

  dialog.addEventListener("close", () => {
    generation++;
  });
  return { references, suspendReview(){generation++;}, update, refreshReview, openReview: (...args) => openView(review, ...args), openCitation: (...args) => openView(historical, ...args), projectChanged() {
    ++reviewRequest;
    reviewAttention({});
    dialog.close();
    update();
  }, noteChanged() {
    $("sources-open").disabled = !getNote();
  } };
}
export {
  citationTarget,
  indexMessage,
  historyResultDetail,
  initKnowledge,
  lineDiff
};
