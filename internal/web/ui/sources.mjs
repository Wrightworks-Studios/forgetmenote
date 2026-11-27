import {openDialog} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {$,element as node,installHTML} from './dom.mjs';
import {poll} from './state.mjs';
import {destinationFolders} from './editor.mjs';
import {notebookPath,assetURL} from './request.mjs';

export function sourceCitation(note, anchor = '') {
  if (!note?.path?.endsWith('.md') || !/^[a-f0-9]{40}$/.test(note.revision)) throw new Error('A saved source revision is required.');
  const label = (note.metadata?.title || note.path).replace(/[\\\[\]]/g, '\\$&');
  const path = note.path.split('/').map(encodeURIComponent).join('/');
  return `[${label}](/${path}?fmn-rev=${note.revision}${anchor ? '#' + encodeURIComponent(anchor) : ''})`;
}
export function originalURL(project, record, revision) {
  const path = record?.original;
  if (!path?.startsWith('assets/') || path.split('/').some(part => !part || part.startsWith('.')) || /[\\\x00-\x1f?#%]/.test(path) || !/\.(pdf|html|htm|txt|docx|epub|xlsx|csv|tsv|png|jpg|jpeg|tiff|tif|webp|bmp|pptx|eml|msg|odt|ods|odp|pages|key|wav|mp3|m4a|aac|ogg|flac|mp4|mov|avi)$/.test(path) || !/^[a-f0-9]{40}$/.test(revision)) return null;
  return assetURL(project, path, revision);
}

function publicLink(url, label) {
  try { const u = new URL(url); if (!['https:', 'http:'].includes(u.protocol) || u.username || u.password) return null; const a = node('a', label); a.href = u.href; a.target = '_blank'; a.rel = 'noopener noreferrer'; return a; } catch { return null; }
}
export function initSources({api, post, getProject, getNote, getFolder = () => '', present, onBusy = () => {}, canEdit, dirty, refresh, openNote, insertCitation,notice}) {
  const dialog = $('new-dialog');
  let formats = [], sourceBytes = 0;
  let prepared = null, scope = '', target = null, imported = null, busy = false, loading = false, generation = 0, jobID = '';
  const rememberedKey = () => `fmn.source-job.${scope}`;
  function remember() { try { localStorage.setItem(rememberedKey(), JSON.stringify({id: jobID, path: target?.path || ''})); } catch {} }
  function forget() { try { localStorage.removeItem(rememberedKey()); } catch {} }
  function error(message) { feedback($('archive-error'), message); }
  function invalidate() { prepared = null; $('archive-preview').hidden = true; $('archive-save').hidden = true; }
  function fromURL() { return $('archive-fetch').checked; }
  function enabled() {
    onBusy(busy);
    const remote = fromURL();
    $('archive-preview-button').disabled = busy || loading; $('archive-save').disabled = busy || loading;
    $('archive-folder').disabled = busy || loading;
    $('archive-upload').disabled = busy; $('archive-fetch').disabled = busy;
    $('archive-file').disabled = busy || remote; $('archive-origin-url').disabled = busy || remote;
    $('archive-url').disabled = busy || !remote;
  }
  async function open({url = '', note = null} = {}) {
    if (!canEdit() || busy) return;
    if(note && dirty()) { notice('Save or reconcile this source draft before capturing a new version.'); return; }
    if (present && !present({note})) return;
    generation++; scope = getProject().id; target = note; imported = null; jobID = ''; invalidate(); error('');
    $('archive-job-status').textContent = ''; $('archive-cancel').hidden = true; $('archive-resume').hidden = true;
    $('archive-copy').textContent = 'Copy citation';
    $('creation-title').textContent = note ? 'Capture a new version' : 'Add source';
    $('archive-fetch').checked = !!url; $('archive-upload').checked = !url;
    $('archive-file').value = ''; $('archive-url').value = url;
    $('archive-origin-url').value = note?.metadata?.source?.method === 'uploaded' ? note.metadata.source.url || '' : '';
    $('archive-origin-details').open = !!$('archive-origin-url').value;
    $('archive-folder-field').hidden = !!note;
    const selectedFolder = getFolder();
    loading = true;
    $('archive-title').value = note?.metadata?.title || ''; $('archive-saved').hidden = true; $('archive-inputs').hidden = false;
    mode(); if (!dialog.open) openDialog(dialog);
    const g = generation;
    try {
      const [available, entries] = await Promise.all([api(notebookPath(scope)+`/api/sources/formats`), note ? [] : api(notebookPath(scope)+`/api/notes`)]);
      if (g !== generation) return;
      if (!note) {
        const folders = destinationFolders(entries);
        if (!folders.includes(selectedFolder)) folders.push(selectedFolder);
        $('archive-folder').replaceChildren(...folders.map(folder => new Option(folder || 'Top level', folder)));
        $('archive-folder').value = selectedFolder;
      }
      formats = available; sourceBytes = formats[0]?.maxBytes || 0;
      if (!Number.isSafeInteger(sourceBytes) || sourceBytes <= 0) throw new Error('Source upload budget is unavailable.');
      loading = false; enabled();
      $('archive-file').accept = formats.flatMap(format => format.extensions).join(',');
      $('archive-formats').textContent = `Documents, images, email, audio, and video · Up to ${sourceBytes / (1024 * 1024)} MiB`;
      let remembered; try { remembered = JSON.parse(localStorage.getItem(rememberedKey())); } catch {}
      if (remembered?.id && remembered.path === (target?.path || '')) {
        jobID = remembered.id;
        try { const result = await api(notebookPath(scope)+`/api/sources/jobs/${jobID}`); if (g === generation) showJob(result); }
        catch { if (g === generation) { jobID = ''; forget(); } }
      }
    } catch { if (g === generation) error('Could not load source options. Reopen Add source to try again.'); }

  }
  function mode() {
    const remote = fromURL();
    $('archive-file-field').hidden = remote; $('archive-url-field').hidden = !remote;
    $('archive-file').required = !remote; $('archive-url').required = remote;
    invalidate(); error(''); enabled();
  }
  $('archive-upload').onchange = mode; $('archive-fetch').onchange = mode;
  $('archive-file').onchange = invalidate; $('archive-url').oninput = invalidate; $('archive-origin-url').oninput = invalidate;
  $('first-source').onclick = () => open();
  dialog.addEventListener('close', () => { generation++; prepared = null; });
  $('archive-form').onsubmit = async event => {
    event.preventDefault(); if (busy || loading) return; const g = generation;
    busy = true; enabled(); invalidate(); error('');
    $('archive-preview-button').textContent = 'Extracting…';
    try {
      const input = {url: $(fromURL() ? 'archive-url' : 'archive-origin-url').value.trim(), ...(target ? {path: target.path} : {folder: $('archive-folder').value})};
      if (!fromURL()) {
        const file = $('archive-file').files[0]; if (!file) throw new Error('Choose a source file.');
        if (file.size > sourceBytes) throw new Error(`Source files must be ${sourceBytes / (1024 * 1024)} MiB or smaller.`);
        input.filename = file.name;
        input.data = await new Promise((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(reader.result.split(',')[1]); reader.onerror = () => reject(new Error('Could not read this file.')); reader.readAsDataURL(file); });
      }
      const result = await post(notebookPath(scope)+`/api/sources/jobs`, input);
      if (g !== generation || scope !== getProject()?.id || !dialog.open) return;
      jobID = result.job.id; remember();
      await followJob(g);
    } catch (e) { if (g === generation) error(e.message); }
    finally { busy = false; $('archive-preview-button').textContent = 'Preview source'; enabled(); }
  };
  function showJob(result) {
    const state = result.job.state;
    $('archive-job-status').textContent = `${state[0].toUpperCase() + state.slice(1)} · ${result.job.pages} physical pages completed`;
    $('archive-cancel').hidden = !['queued', 'running'].includes(state);
    $('archive-resume').hidden = result.job.resumeBlocked || !['interrupted', 'canceled', 'failed', 'queued', 'running'].includes(state);
    $('archive-resume').textContent = ['queued', 'running'].includes(state) ? 'Show progress' : 'Resume processing';
    if (!result.prepared) return;
    const preview = result.prepared;
    prepared = {job: jobID};
    if (!$('archive-title').value) $('archive-title').value = preview.capture.title;
    $('archive-warning').textContent = (target ? 'This replaces the source note with a new capture; previous versions remain in History. ' : '') + preview.capture.record.warning;
    installHTML($('archive-preview-text'), preview.html || '<p>No extracted text. You can still archive the original.</p>');
    $('archive-preview').hidden = false; $('archive-save').hidden = false;
  }
  function followJob(g) {
    return poll(async () => {
      const result = await api(notebookPath(scope)+`/api/sources/jobs/${jobID}`);
      if (g !== generation) return false;
      showJob(result);
      if (['ready', 'partial'].includes(result.job.state)) return false;
      if (!['queued', 'running'].includes(result.job.state)) throw new Error(result.job.warning || `Source processing ${result.job.state}.`);
    }, {first: 0, every: 1000, active: () => g === generation && scope === getProject()?.id && dialog.open}).done;
  }
  $('archive-cancel').onclick = async () => {
    const g = generation;
    try { const result = await post(notebookPath(scope)+`/api/sources/jobs/${jobID}/cancel`, {}); if (g === generation) showJob(result); }
    catch (e) { if (g === generation) error(e.message); }
  };
  $('archive-resume').onclick = async () => {
    if (busy || !jobID) return;
    const g = generation; busy = true; enabled(); error('');
    try {
      const result = await api(notebookPath(scope)+`/api/sources/jobs/${jobID}`);
      if (!['queued', 'running', 'ready', 'partial'].includes(result.job.state)) await post(notebookPath(scope)+`/api/sources/jobs/${jobID}/resume`, {});
      await followJob(g);
    } catch (e) { if (g === generation) error(e.message); }
    finally { busy = false; enabled(); }
  };
  $('archive-save').onclick = async () => {
    if (!prepared || busy || loading) return; const g = generation;
    busy = true; enabled(); error('');
    try {
      const doc = await post(notebookPath(scope)+`/api/sources/import`, {...prepared, title: $('archive-title').value, path: target?.path || '', revision: target?.revision || '', ...(!target ? {folder: $('archive-folder').value} : {})});
      if (g !== generation || scope !== getProject()?.id || !dialog.open) return;
      imported = doc; prepared = null; forget(); jobID = ''; $('archive-job-status').textContent = ''; $('archive-cancel').hidden = true; $('archive-resume').hidden = true; $('archive-inputs').hidden = true; $('archive-preview').hidden = true; $('archive-save').hidden = true;
      $('archive-saved').hidden = false; $('archive-insert').disabled = !getNote()?.editable || !!target;
      $('archive-result').textContent = sourceCitation(doc); await refresh();
    } catch (e) { if (g === generation) error(e.message); }
    finally { busy = false; enabled(); }
  };
  $('archive-view').onclick = async () => { if (imported) { const doc = imported; dialog.close(); await openNote(doc.path); } };
  $('archive-copy').onclick = async () => { try { await navigator.clipboard.writeText(sourceCitation(imported)); $('archive-copy').textContent = 'Copied'; } catch { error('Copy the citation shown below.'); } };
  $('archive-insert').onclick = () => { if(imported && scope === getProject()?.id && getNote()?.editable) { const text = sourceCitation(imported); dialog.close(); insertCitation(text); } };
  async function render(note) {
    const panel = $('archive-details'); panel.replaceChildren(); const record = note?.metadata?.source;
    panel.hidden = !record; if (!record) return;
    const projectID = getProject().id;
    const date=new Date(record.capturedAt);
    const summary = node('summary', `Archived source · ${Number.isNaN(date.valueOf()) ? 'Capture date unknown' : date.toLocaleString()}`); panel.append(summary);
    const body = node('div', undefined, 'archive-provenance'); panel.append(body);
    const actions = node('div', undefined, 'archive-links'); body.append(actions);
    const original = originalURL(projectID, record, note.revision);
    if (original) { const a = node('a', 'Download original'); a.href = original; actions.append(a); }
    const website = publicLink(record.url, 'Original website'); if (website) actions.append(website);
    const anchors = node('select'); anchors.setAttribute('aria-label', 'Citation section'); anchors.add(new Option('Whole source', ''));
    const parsed = new DOMParser().parseFromString(note.html || '', 'text/html');
    for (const heading of parsed.querySelectorAll('h2[id],h3[id],h4[id],h5[id],h6[id]')) if(heading.id !== 'extracted-content') anchors.add(new Option(heading.textContent, heading.id));
    actions.append(anchors);
    const copy = node('button','Copy citation','quiet'); actions.append(copy);
    copy.onclick = async () => { try { await navigator.clipboard.writeText(sourceCitation(note,anchors.value)); copy.textContent='Copied'; } catch { const text=node('textarea',sourceCitation(note,anchors.value));body.append(text);text.select(); } };
    if(note.editable){const refreshButton=node('button','Update source','quiet'); actions.append(refreshButton);refreshButton.onclick=()=>open({url:record.method==='server-http'?record.url:'',note});}
    if (record.warning) body.append(node('p',record.warning,'small'));
    const problem=node('p',undefined,'archive-integrity-warning');problem.hidden=true;problem.setAttribute('role','status');body.append(problem);
    const technical=node('details',undefined,'archive-technical');technical.append(node('summary','Technical details'));body.append(technical);
    const details=node('dl');technical.append(details);
    for(const [label,value] of [['Imported by',record.importedBy],['Capture method',record.method],['Extraction',record.extractor],['Extraction status',record.extraction],['Original SHA-256',record.sha256],['Extracted text SHA-256',record.textSHA256]]){details.append(node('dt',label),node('dd',value));}
    const integrity=node('p','Checking stored bytes…','small');technical.append(integrity);
    const summaryText=summary.textContent;
    function showProblem(label,message) {
      summary.textContent=summaryText+' · '+label;
      problem.textContent=message;problem.hidden=false;
    }
    try {
      const result=await api(notebookPath(projectID)+`/api/sources/verify?path=${encodeURIComponent(note.path)}&revision=${note.revision}`);
      integrity.textContent=result.originalState+' · '+result.textState;
      const changed=[];
      if(result.originalStatus === 'changed') changed.push('The original file differs from its recorded capture.');
      if(result.textStatus === 'changed') changed.push('The extracted text has changed since capture.');
      if(changed.length) showProblem('Content changed',changed.join(' '));
      if(!['match','changed'].includes(result.originalStatus) || !['match','changed'].includes(result.textStatus)) {
        showProblem('Verification incomplete',[...changed,'Could not verify all stored source content.'].join(' '));
      }
    } catch {
      integrity.textContent='Could not verify this capture.';
      showProblem('Verification unavailable',integrity.textContent);
    }
  }
  return {open,render};
}
