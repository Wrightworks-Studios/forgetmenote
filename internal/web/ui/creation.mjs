import {openDialog} from './overlay.mjs';
import {feedback} from './feedback.mjs';
import {destinationFolders, fileActionTarget} from './editor.mjs';
import {notebookPath} from './request.mjs';
import {$,busy,isBusy} from './dom.mjs';

export function noteCreationTarget(name, folder) {
  if (folder === null) return {error: 'Choose a destination folder from the list.'};
  const result = fileActionTarget({kind:'note', path:''}, 'duplicate', folder, name);
  if (result.path === 'AGENTS.md') return {error: 'Manage agent instructions in Notebook settings.'};
  return result;
}

// The New note / Add source dialog. A new note's name, folder and lock are
// checked here; `ready()` is the caller's check that the displayed draft is
// retained, `chooseDraft` picks a retained draft for the new path, and
// `install` opens the new note in the caller's editor.
export function initCreation({api, getProject, getFolder, getCollection = () => "notes", canCreate, canEdit, openSource, ready, chooseDraft, install}) {
  const dialog = $('new-dialog');
  const tabs = [$('create-note-tab'), $('create-source-tab')], input = $('new-folder');
  let generation = 0, folders = [], selectedFolder = null, matches = [], active = -1;
  // Each opening of the dialog creates at most one note at a time.
  let sourceReady = false, sourceBusy = false, session = {}, loaded = false, capture = false;
  const noteBusy = () => isBusy(session);
  const label = folder => folder || 'Top level';
  function error(message = '') { feedback($('new-error'), message); }
  const controls = () => [...tabs, $('creation-close'), $('new-submit'), $('new-name'), input, $('new-container'), $('new-folder-toggle')];
  function enabled() {
    const busy = sourceBusy || noteBusy();
    for (const tab of tabs) tab.disabled = busy;
    $('creation-close').disabled = busy;
    $('new-submit').disabled = busy || !loaded;
    $('new-name').disabled = busy;
    input.disabled = busy || !loaded;
    $('new-container').disabled = busy;
    $('new-folder-toggle').disabled = busy || !loaded;
  }
  function closeFolders() {
    $('new-folder-options').hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
  }
  function choose(folder) {
    selectedFolder = folder;
    input.value = label(folder);
    closeFolders();
    input.focus();
    error();
  }
  function renderFolders(query = '', highlight = -1) {
    matches = folders.filter(folder => label(folder).toLocaleLowerCase().includes(query.toLocaleLowerCase()));
    active = highlight;
    const options = matches.map((folder, index) => {
      const option = document.createElement('button');
      option.type = 'button'; option.setAttribute('role', 'option'); option.tabIndex = -1;
      option.id = 'new-folder-option-' + index;
      option.textContent = label(folder);
      option.setAttribute('aria-selected', String(index === active));
      option.onpointerdown = event => event.preventDefault();
      option.onclick = () => choose(folder);
      return option;
    });
    if (!options.length) {
      const empty = document.createElement('p');
      empty.className = 'small'; empty.textContent = 'No matching folders.';
      options.push(empty);
    }
    $('new-folder-options').replaceChildren(...options);
    $('new-folder-options').hidden = false;
    input.setAttribute('aria-expanded', 'true');
    if (active >= 0 && matches[active] !== undefined) {
      input.setAttribute('aria-activedescendant', options[active].id);
      options[active].scrollIntoView({block:'nearest'});
    } else input.removeAttribute('aria-activedescendant');
  }
  input.onfocus = () => { input.select(); renderFolders(); };
  input.oninput = () => { selectedFolder = null; error(); renderFolders(input.value); };
  input.onkeydown = event => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const query = selectedFolder === null ? input.value : '';
      const count = folders.filter(folder => label(folder).toLocaleLowerCase().includes(query.toLocaleLowerCase())).length;
      const next = $('new-folder-options').hidden || active < 0
        ? event.key === 'ArrowDown' ? 0 : count - 1
        : (active + (event.key === 'ArrowDown' ? 1 : -1) + count) % count;
      renderFolders(query, count ? next : -1);
    } else if (event.key === 'Enter' && !$('new-folder-options').hidden) {
      event.preventDefault();
      if (active >= 0 && matches[active] !== undefined) choose(matches[active]);
    } else if (event.key === 'Escape' && !$('new-folder-options').hidden) {
      event.preventDefault(); event.stopPropagation(); closeFolders();
    }
  };
  $('new-folder-toggle').onpointerdown = event => event.preventDefault();
  $('new-folder-toggle').onclick = () => {
    const opening = $('new-folder-options').hidden;
    input.focus();
    if (opening) renderFolders(); else closeFolders();
  };
  $('new-folder-picker').addEventListener('keydown', event => {
    if (event.key === 'Tab') closeFolders();
  });
  // Close this in-flow list after a pointer click, not on blur: collapsing it
  // between pointerdown and click would move the submit button out from under it.
  dialog.addEventListener('click', event => {
    if (!$('new-folder-picker').contains(event.target)) closeFolders();
  });
  async function loadFolders(id, request, folder) {
    try {
      const entries = await api(notebookPath(id) + '/api/notes');
      if (request !== generation || !dialog.open || getProject()?.id !== id) return;
      folders = destinationFolders(entries);
      if (folder && !folders.includes(folder)) folders.push(folder);
      selectedFolder = folder;
      input.value = label(folder);
      loaded = true;
      $('new-folder-status').textContent = '';
    } catch {
      if (request !== generation || !dialog.open || getProject()?.id !== id) return;
      $('new-folder-status').textContent = 'Could not load folders. Close and reopen to retry.';
    }
    enabled();
  }
  function begin() {
    if (dialog.open) return;
    generation++;
    sourceReady = false; sourceBusy = false; session = {}; loaded = false; capture = false;
    folders = []; selectedFolder = null;
    $('new-name').value = ''; input.value = ''; error(); closeFolders();
    $('new-container').value = getCollection() === 'drafts' ? 'draft' : '';
    $('new-folder-status').textContent = 'Loading folders…';
    enabled();
    openDialog(dialog);
    loadFolders(getProject().id, generation, getFolder());
  }
  function showMode(mode) {
    closeFolders();
    $('new-note-panel').hidden = mode !== 'note';
    $('new-source-panel').hidden = mode !== 'source';
    $('creation-tabs').hidden = capture;
    $('creation-title').textContent = capture ? 'Capture a new version' : mode === 'note' ? 'New note' : 'Add source';
    tabs.forEach((tab, index) => {
      const selected = (index === 0) === (mode === 'note');
      tab.setAttribute('aria-selected', String(selected));
      tab.tabIndex = selected ? 0 : -1;
    });
  }
  function select(mode) {
    if (!canCreate() || sourceBusy || noteBusy()) return;
    if (mode === 'source' && !sourceReady) { openSource(); return; }
    showMode(mode);
  }
  tabs.forEach((tab, index) => {
    tab.onclick = () => select(index ? 'source' : 'note');
    tab.onkeydown = event => {
      if (!['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) return;
      event.preventDefault();
      if (sourceBusy || noteBusy()) return;
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? 1 : 1 - index;
      if (tabs[next].hidden) return;
      select(next ? 'source' : 'note'); tabs[next].focus();
    };
  });
  $('creation-close').onclick = () => { if (!sourceBusy && !noteBusy()) dialog.close(); };
  dialog.addEventListener('cancel', event => { if (sourceBusy || noteBusy()) event.preventDefault(); });
  dialog.addEventListener('close', () => { generation++; closeFolders(); });
  $('new-name').oninput = () => error();
  function target() {
    if (!loaded || !canCreate() || sourceBusy || noteBusy()) return null;
    const result = noteCreationTarget($('new-name').value, selectedFolder);
    if (result.error) { error(result.error); return null; }
    const request = generation, id = getProject().id;
    return {path:result.path, container:$('new-container').value, project:id, active:() => dialog.open && generation === request && getProject()?.id === id};
  }
  // A new note needs a free name and an unlocked folder; once a draft is
  // chosen, the caller installs it and the dialog closes.
  $('new-form').onsubmit = event => {
    event.preventDefault();
    const note = target();
    if (!note) return;
    if (!ready()) { error($('notice').textContent); return; }
    const {path} = note;
    error();
    busy(session, async () => {
      const entries = await api(notebookPath(note.project) + "/api/notes");
      if (!note.active()) return;
      if (entries.some((n) => n.path === path)) {
        error("A note with this name already exists in that folder. Choose another name.");
        return;
      }
      const protection = await api(notebookPath(note.project) + "/api/locks?path=" + encodeURIComponent(path));
      if (!note.active() || !canEdit()) return;
      if (protection.rules.length) throw new Error("This path is read-only. Choose an unlocked folder.");
      const candidate = await chooseDraft(path, () => note.active());
      if (candidate === false || !note.active()) return;
      if (!ready()) { error($('notice').textContent); return; }
      install({path, protection, container:note.container, candidate});
    }, {controls:controls(), report:$('new-error'), current:note.active, retire:true});
  };
  return {
    // `name` prefills the dialog, as when following a link to a missing page.
    openNote(name = '') {
      if (!canCreate()) return;
      begin(); $('new-name').value = name; showMode('note'); $('new-name').focus();
    },
    showSource({note} = {}) {
      if (!canCreate() || sourceBusy || noteBusy()) return false;
      begin(); sourceReady = true; capture = !!note; showMode('source');
      return true;
    },
    setSourceBusy(value) { sourceBusy = value; enabled(); },
    projectChanged() { dialog.close(); }
  };
}
