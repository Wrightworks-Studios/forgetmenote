import {openDialog} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {notebookPath} from './request.mjs';
import {$} from './dom.mjs';

function count(n, one, many) {return n + ' ' + (n === 1 ? one : many);}

// What a plan that leaves the notebook unchanged says: a purge keeps commit
// authors and committers, which may be all that holds the text.
export function unchangedText(plan) {
  const authors = (plan.elsewhere || []).some(o => o.kind === 'author');
  return 'Nothing in this notebook’s notes or history holds it' + (authors ? ' but commit authors or committers, which a purge does not erase' : '') + (plan.recordsUnsearched ? '.' : '; only the records below quoted it.');
}

// Where else a plan found what it erases, beside the path it names.
export function occurrenceText(o) {
  switch (o.kind) {
    case 'name': return 'in its file name';
    case 'live': return 'in live edits';
    case 'notebook': return 'another notebook stores the same content';
    case 'author': return 'in the author or committer of ' + count(o.versions, 'commit', 'commits') + ', which a purge does not erase';
    case 'link': return count(o.versions, 'version', 'versions') + ' may link to it by name';
  }
  return count(o.versions, 'version', 'versions');
}

// What a plan or receipt says of the private chat transcripts, Knowledge
// proposals and Jev records outside the notebook that quoted the erased
// content, as lines: their counts, which for someone who is not an Admin
// cover only their own records, and what it says of everyone else's.
export function derivedLines(d = {}, done = false) {
  if (d.withheld) return d.others ? [d.others] : [];
  const records = [d.chats && count(d.chats, 'private chat transcript', 'private chat transcripts'), d.proposals && count(d.proposals, 'Knowledge proposal', 'Knowledge proposals'), d.jevRecords && count(d.jevRecords, 'Jev record', 'Jev records')].filter(Boolean);
  const lines = [];
  if (done) lines.push((d.own ? 'Of your own records, ' : '') + count(d.chats || 0, 'chat transcript', 'chat transcripts') + ', ' + count(d.proposals || 0, 'proposal', 'proposals') + ' and ' + count(d.jevRecords || 0, 'Jev record', 'Jev records') + ' redacted.');
  else if (d.own) lines.push(records.length ? 'Of your own records, ' + records.join(', ') + ' quoted it and will be redacted by the system, without anyone reading them.' : 'None of your own chat transcripts, proposals or Jev records quotes it.');
  else lines.push(records.length ? records.join(', ') + ' that quoted it will be redacted by the system, without anyone reading them.' : 'No chat transcript, proposal or Jev record quotes it.');
  if (d.others) lines.push(d.others);
  return lines;
}

// One stored purge receipt, as the notebook's list of earlier purges shows
// it: what kind of purge, when, who and why, and whether it is unfinished
// or was interrupted (its warnings say what was not done).
export function receiptText(r) {
  const what = r.kind === 'note' ? 'a whole note' : r.kind === 'versions' ? 'chosen versions of a note' : (r.pattern ? 'a pattern' : 'text') + (r.wholeNotebook ? ' everywhere' : ' in chosen notes');
  const status = r.unfinished ? 'Unfinished: ' : r.interrupted ? 'Interrupted: ' : '';
  const when = r.created ? new Date(r.created).toLocaleString() : '';
  return status + 'erased ' + what + (when ? ', ' + when : '') + ', by ' + (r.actorName || r.actor) + ': ' + r.reason;
}

// What a plan says of the notes being edited live that the purge restarts,
// and of how many lose edits not yet saved as a revision, which the restart
// discards.
export function liveText(live = []) {
  const unsaved = live.filter(l => l.pending).length;
  const lost = !unsaved ? '' : live.length === 1 ? ', losing its edits not yet saved' : '; ' + count(unsaved, 'of them loses its', 'of them lose their') + ' edits not yet saved';
  return count(live.length, 'note being edited live restarts', 'notes being edited live restart') + ' from the erased text' + lost + '. Private drafts kept in browsers are not erased: ask whoever edited ' + (live.length === 1 ? 'it' : 'them') + ' to discard them.';
}

// What a plan says of the GitHub copy. The Git export catches up in the
// background, so a plan can only say it may be force-pushed; the receipt
// says whether it is (doneLines).
export function exportText(plan) {
  return plan.exportForcePush ? 'The GitHub copy may be force-pushed once to replace its history; the receipt says whether it is.' : 'No GitHub copy needs to be force-pushed.';
}

// What a purge's receipt says it did beyond the notebook's history: the notes
// being edited live it restarted, which may include notes opened after the
// plan was made, and whether the next backup force-pushes the GitHub copy,
// which the plan could only say it may.
export function doneLines(r) {
  const lines = [];
  if (r.liveReset) lines.push(count(r.liveReset, 'note being edited live was', 'notes being edited live were') + ' restarted from the erased text. Private drafts kept in browsers are not erased: ask whoever edited ' + (r.liveReset === 1 ? 'it' : 'them') + ' to discard them.');
  if (r.exportForcePush) lines.push('The next backup force-pushes the GitHub copy once.');
  else if (r.export) lines.push('The Git export held none of the rewritten revisions, so no GitHub copy needs to be force-pushed.');
  return lines;
}

// Permanent erasure (purge): an Owner or Admin chooses what to erase, reviews
// the plan the server makes, and confirms exactly that plan with a reason.
// Nothing changes until the confirmation; the plan's confirmation binds it.
// An agent's prepared plan arrives as a token in the page URL.
export function initPurge({api, post, getProject, canManage, notice, refresh}) {
  const dialog = $('purge-dialog');
  let generation = 0, busy = false;
  // The open request: its notebook, the note it started from, the scope
  // being planned, and the plan to confirm.
  let open = null;
  const active = c => c && c === open && c.generation === generation && c.project === getProject()?.id;
  const prefix = c => notebookPath(c.project) + '/api/';
  const error = message => feedback($('purge-error'), message);
  const kind = () => dialog.querySelector('input[name=purge-kind]:checked')?.value || '';

  function step(name) {
    $('purge-scope').hidden = name !== 'scope';
    $('purge-plan').hidden = name !== 'plan';
    $('purge-done').hidden = name !== 'done';
  }
  function controls() {
    for (const element of dialog.querySelectorAll('button, input, textarea')) {
      if (element.id !== 'purge-close') element.disabled = busy || element.name === 'purge-kind' && !open?.path && element.value !== 'notebook';
    }
    const plan = open?.plan;
    $('purge-confirm').disabled = busy || !plan || !!plan.blocked || !$('purge-understood').checked || !$('purge-reason').value.trim();
  }
  function showFields() {
    const k = kind(), note = open?.path;
    $('purge-versions-field').hidden = k !== 'versions';
    $('purge-text-field').hidden = k !== 'text' && k !== 'notebook';
    $('purge-paths-field').hidden = k !== 'text';
    $('purge-target').textContent = k === 'notebook' ? 'Every note of this notebook and every commit message.'
      : note ? 'Note: ' + note : 'Choose a note first, or erase text everywhere in the notebook.';
    if (k === 'versions' && open && !open.versionsLoaded) loadVersions(open).catch(e => active(open) && error(e.message));
  }
  async function loadVersions(c) {
    c.versionsLoaded = true;
    $('purge-versions').textContent = 'Loading…';
    const rows = await api(prefix(c) + 'history?path=' + encodeURIComponent(c.path));
    if (!active(c)) return;
    $('purge-versions').replaceChildren();
    for (const row of rows) {
      const item = document.createElement('li'), label = document.createElement('label'), box = document.createElement('input');
      box.type = 'checkbox'; box.value = row.revision; box.checked = c.versions.includes(row.revision);
      label.append(box, ' ' + row.revision.slice(0, 7) + ' · ' + row.message + (row.authoredAt ? ' · ' + new Date(row.authoredAt).toLocaleString() : ''));
      item.append(label); $('purge-versions').append(item);
    }
    if (!rows.length) $('purge-versions').textContent = 'This note has no saved versions.';
  }
  function scope(c) {
    const k = kind();
    if (k === 'note') return {kind: 'note', path: c.path};
    if (k === 'versions') return {kind: 'versions', path: c.path, versions: [...$('purge-versions').querySelectorAll('input:checked')].map(box => box.value)};
    const value = $('purge-text').value, paths = k === 'notebook' ? [] : $('purge-paths').value.split('\n').map(p => p.trim()).filter(Boolean);
    return $('purge-pattern').checked ? {kind: 'text', pattern: value, paths} : {kind: 'text', text: value, paths};
  }
  function checkScope(s) {
    if ((s.kind === 'note' || s.kind === 'versions') && !s.path) return 'Choose a note to erase.';
    if (s.kind === 'versions' && !s.versions.length) return 'Choose at least one version.';
    if (s.kind === 'text' && !(s.text || s.pattern)) return 'Enter the text to erase.';
    if (s.kind === 'text' && kind() === 'text' && !s.paths.length) return 'List at least one note, or erase the text everywhere in the notebook.';
    return '';
  }
  function item(list, text, strong) {
    const li = document.createElement('li');
    if (strong) {const b = document.createElement('strong'); b.textContent = strong; li.append(b, ' ');}
    li.append(text); list.append(li); return li;
  }
  function element(tag, text) {const e = document.createElement(tag); e.textContent = text; return e;}
  // What the plan erases, from its scope, so whoever confirms it sees the
  // kind and the text, whoever prepared it.
  function describe(s) {
    if (s.kind === 'note') return ['Erase the whole note ', element('strong', s.path), ', with its discussion, from every version.'];
    const versions = s.versions || [];
    if (s.kind === 'versions') return ['Erase ' + count(versions.length, 'chosen version', 'chosen versions') + ' of ', element('strong', s.path), ' (' + versions.map(v => v.slice(0, 7)).join(', ') + ') from its history.'];
    const paths = s.paths || [], where = [];
    paths.forEach((path, i) => where.push(i ? ', ' : 'in ', element('strong', path)));
    return [s.pattern ? 'Replace every match of the pattern ' : 'Replace the exact text ', element('code', s.pattern || s.text || ''), ' with [purged] ',
      ...(paths.length ? where : ['everywhere in this notebook']), ', in every version and every commit message.'];
  }
  function render(c, plan) {
    c.plan = plan;
    $('purge-prepared').hidden = !c.token;
    $('purge-prepared').textContent = c.token ? 'An agent prepared this plan. Check that it erases what you intend before you confirm it.' : '';
    $('purge-what').replaceChildren(...describe(plan.scope || {}));
    const summary = $('purge-summary');
    summary.replaceChildren();
    if (plan.notebookUnchanged) item(summary, unchangedText(plan));
    for (const note of plan.notes || []) {
      item(summary, count(note.versions.length, 'version', 'versions') + (note.current ? '' : ' (no longer in the notebook)'), note.path);
    }
    if (plan.versions) item(summary, count(plan.versions, 'version changes', 'versions change') + '; ' + count(plan.revisionsRewritten, 'revision gets a new ID', 'revisions get new IDs') + '.');
    if (plan.messages) item(summary, count(plan.messages, 'commit message is', 'commit messages are') + ' redacted.');
    if (plan.live?.length) item(summary, liveText(plan.live));
    item(summary, plan.referencesToRemap ? count(plan.referencesToRemap, 'reference to an old revision', 'references to old revisions') + ' in your notes will resolve to the new revisions.' : 'No reference in your notes names a rewritten revision.');
    item(summary, exportText(plan));
    const d = plan.derived || {};
    if (plan.recordsUnsearched) item(summary, 'Chat transcripts, proposals and Jev records are searched only for content this notebook holds; only an instance Admin can search them for this.');
    else for (const line of derivedLines(d)) item(summary, line);
    if (d.partial) item(summary, 'Some of the erased content was too large to look for in full elsewhere; quotes of it may remain.');
    if (!plan.notebookUnchanged) item(summary, 'Search and semantic indexes are rebuilt from the erased notebook; where embeddings are configured, every note is embedded again, which a paid embedding service charges for.');
    for (const o of plan.unredactable || []) item(summary, count(o.versions, 'version is', 'versions are') + ' not text and can only be erased whole.', o.path);
    for (const o of plan.shared || []) item(summary, o.kind === 'notebook' ? 'keeps the same content; it is not erased there.' : 'also holds content being erased and keeps it.', o.path);
    // What the person should know before erasing that the purge cannot
    // change, such as copies it does not reach.
    $('purge-warnings').replaceChildren();
    for (const warning of plan.warnings || []) item($('purge-warnings'), warning, 'Warning');
    $('purge-warnings').hidden = !(plan.warnings || []).length;
    const elsewhere = (plan.elsewhere || []);
    $('purge-elsewhere-title').textContent = plan.scope?.kind === 'note' ? 'Other notes may still name it in links, which keep its name; a text purge of the name removes it:' : 'The text also appears outside what you chose:';
    $('purge-elsewhere').replaceChildren();
    for (const o of elsewhere) {
      item($('purge-elsewhere'), occurrenceText(o), o.path);
    }
    c.include = elsewhere.filter(o => o.kind === 'file' || o.kind === 'live').map(o => o.path);
    $('purge-elsewhere-field').hidden = !elsewhere.length;
    $('purge-include').hidden = !c.include.length || plan.scope?.kind !== 'text' || !(plan.scope.paths || []).length;
    $('purge-blocked').hidden = !plan.blocked;
    $('purge-blocked').textContent = plan.blocked ? 'This purge cannot run as planned: ' + plan.blocked : '';
    $('purge-understood').checked = false;
    step('plan');
    controls();
    $('purge-reason').focus();
  }
  async function prepare(c, body) {
    busy = true; controls(); error('');
    try {
      const plan = await post(prefix(c) + 'purge/prepare', body);
      if (!active(c)) return;
      c.scope = plan.scope;
      render(c, plan);
    } catch (e) {
      if (active(c)) error(e.message);
    } finally {
      busy = false; controls();
    }
  }
  // The notebook's earlier purges: their receipts, and an entry for each
  // purge that has kept none yet (unfinished) or that a starting gateway
  // finished as far as it could (interrupted).
  async function loadReceipts(c) {
    const list = $('purge-receipts');
    list.replaceChildren();
    $('purge-history').hidden = true;
    let receipts;
    try {
      receipts = (await api(prefix(c) + 'purge')).receipts || [];
    } catch (e) {
      if (!active(c)) return;
      item(list, 'Earlier purges could not be listed: ' + e.message);
      $('purge-history').hidden = false;
      return;
    }
    if (!active(c) || !receipts.length) return;
    for (const r of receipts) {
      const li = item(list, receiptText(r));
      if (r.unfinished || r.interrupted) li.classList.add('purge-warning');
      if (r.warnings?.length) {
        const notes = document.createElement('ul');
        for (const warning of r.warnings) notes.append(element('li', warning));
        li.append(notes);
      }
    }
    $('purge-history').open = receipts.some(r => r.unfinished || r.interrupted);
    $('purge-history').hidden = false;
  }
  function start(c, initial) {
    open = c;
    loadReceipts(c);
    $('purge-scope').reset();
    $('purge-reason').value = '';
    $('purge-understood').checked = false;
    $('purge-versions').replaceChildren();
    $('purge-paths').value = c.path || '';
    const choice = dialog.querySelector(`input[name=purge-kind][value=${initial}]`);
    if (choice) choice.checked = true;
    error(''); step('scope'); showFields(); controls();
    if (!dialog.open) openDialog(dialog);
  }
  // From a note's menu or its history (kind 'versions'), or the notebook's
  // settings (kind 'notebook').
  function openFor({path = '', kind: initial = path ? 'note' : 'notebook', versions = []} = {}) {
    const project = getProject();
    if (!project || !canManage()) {notice('Only a notebook Owner or an Admin can permanently erase content.'); return;}
    start({project: project.id, generation, path, versions}, initial);
    if (initial === 'notebook' || initial === 'text') $('purge-text').focus();
  }
  // A plan an agent prepared: the server keeps its scope under token.
  async function openPrepared(token) {
    const project = getProject();
    if (!project) return;
    const c = {project: project.id, generation, path: '', versions: [], token};
    start(c, 'notebook');
    await prepare(c, {token});
  }
  $('purge-scope').onsubmit = event => {
    event.preventDefault();
    if (busy || !open) return;
    const s = scope(open), problem = checkScope(s);
    if (problem) {error(problem); return;}
    open.token = '';
    prepare(open, {scope: s});
  };
  for (const radio of dialog.querySelectorAll('input[name=purge-kind]')) radio.onchange = showFields;
  $('purge-include').onclick = () => {
    const c = open;
    if (busy || !c?.scope) return;
    const paths = [...new Set([...(c.scope.paths || []), ...c.include])];
    $('purge-paths').value = paths.join('\n');
    c.token = '';
    prepare(c, {scope: {...c.scope, paths}});
  };
  $('purge-back').onclick = () => {
    if (busy || !open) return;
    if (open.scope) {
      const s = open.scope, text = s.kind === 'text';
      const radio = dialog.querySelector(`input[name=purge-kind][value=${text ? ((s.paths || []).length ? 'text' : 'notebook') : s.kind}]`);
      if (radio) radio.checked = true;
      if (text) {$('purge-text').value = s.text || s.pattern || ''; $('purge-pattern').checked = !!s.pattern; $('purge-paths').value = (s.paths || []).join('\n');}
      if (s.path) open.path = s.path;
    }
    open.plan = null; error(''); step('scope'); showFields(); controls();
  };
  $('purge-reason').oninput = controls;
  $('purge-understood').onchange = controls;
  $('purge-confirm').onclick = async () => {
    const c = open, plan = c?.plan;
    if (busy || !plan || plan.blocked || !$('purge-understood').checked) return;
    const reason = $('purge-reason').value.trim();
    if (!reason) {error('Give a reason; the purge receipt keeps it.'); return;}
    busy = true; controls(); error('');
    try {
      const body = {revision: plan.revision, confirmation: plan.confirmation, reason};
      if (c.token) body.token = c.token; else body.scope = c.scope;
      const receipt = await post(prefix(c) + 'purge/execute', body);
      if (!active(c)) return;
      c.plan = null; c.scope = null;
      $('purge-text').value = '';
      const list = $('purge-receipt');
      list.replaceChildren();
      item(list, receipt.purge, 'Receipt');
      item(list, receipt.notebookUnchanged ? 'The notebook’s history was not changed.' : count(receipt.rewritten, 'revision was', 'revisions were') + ' rewritten; ' + count(receipt.removedVersions, 'stored version was', 'stored versions were') + ' deleted.');
      for (const line of derivedLines(receipt.derived, true)) item(list, line);
      for (const line of doneLines(receipt)) item(list, line);
      for (const warning of receipt.warnings || []) item(list, warning, 'Warning');
      item(list, 'Rotate any leaked secret now: erasing does not undo exposure.');
      step('done');
      await refresh();
    } catch (e) {
      if (active(c)) error(e.message + (e.status === 409 ? ' Review the plan again.' : ''));
    } finally {
      busy = false; controls();
    }
  };
  $('purge-finish').onclick = () => dialog.close();
  $('purge-close').onclick = () => {if (!busy) dialog.close();};
  dialog.addEventListener('cancel', event => {if (busy) event.preventDefault();});
  dialog.addEventListener('close', () => {
    open = null;
    $('purge-text').value = '';
    $('purge-reason').value = '';
  });
  function reset() {generation++; open = null; if (dialog.open) dialog.close();}
  return {open: openFor, openPrepared, reset};
}
