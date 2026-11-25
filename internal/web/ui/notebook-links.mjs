import {openDialog,dialogShell} from './overlay.mjs';
import {notebookPath} from './request.mjs';
const node = (tag, text) => { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; return n; };

export function initNotebookLinks({api, post, getProject, getNote, editor, notice}) {
  const dialog = dialogShell('Link to a notebook page', {id: 'notebook-link-dialog'});
  const notebook = node('select'), query = node('input'), results = node('div'), status = node('p'), heading = node('select'), insert = node('button', 'Insert link');
  query.type = 'search'; query.placeholder = 'Find a page…'; query.setAttribute('aria-label', 'Find a page');
  notebook.setAttribute('aria-label', 'Notebook'); heading.setAttribute('aria-label', 'Page heading'); status.setAttribute('role', 'status');
  dialog.append(notebook, query, results, status, heading, insert);
  let generation = 0, selection, origin, sourcePath, timer, target;
  const active = () => dialog.open && getProject()?.id === origin && getNote()?.path === sourcePath;
  function clearTarget() { target = null; insert.disabled = true; heading.replaceChildren(new Option('Whole page', '')); heading.disabled = true; }
  async function search() {
    const g = ++generation; clearTarget(); results.replaceChildren(); status.textContent = 'Finding pages…';
    try {
      const entries = await api(notebookPath(origin) + '/api/notebook-links/targets?' + new URLSearchParams({target:notebook.value, q:query.value}));
      if (g !== generation || !active()) return;
      status.textContent = entries.length ? 'Choose a page, then an optional heading.' : 'No matching pages.';
      for (const entry of entries) {
        const button = node('button', entry.title || entry.path); button.className = 'quiet notebook-link-result'; button.title = entry.path;
        button.onclick = async () => {
          const version = ++generation; clearTarget();
          try {
            const resolved = await post(notebookPath(origin) + '/api/notebook-links/resolve', {project:entry.project, path:entry.path, noteId:entry.noteId || ''});
            if (!active() || version !== generation) return;
            target = resolved; heading.replaceChildren(new Option('Whole page', ''), ...(target.headings || []).map(h => new Option(h, h)));
            heading.disabled = false; insert.disabled = false;
            for (const row of results.children) row.setAttribute('aria-pressed', String(row === button));
            status.textContent = target.notebook + ' → ' + (target.title || target.path) + (target.noteId ? '' : ' · Path link; this page has no stable identity yet.');
          } catch (error) { if (active() && version === generation) status.textContent = error.message; }
        };
        results.append(button);
      }
    } catch (error) { if (g === generation && active()) status.textContent = error.message; }
  }
  insert.onclick = async () => {
    if (!active() || !target) return;
    insert.disabled = true;
    try {
      const resolved = await post(notebookPath(origin) + '/api/notebook-links/resolve', {project:target.project, path:target.path, noteId:target.noteId || '', heading:heading.value});
      if (!active()) return;
      const label = (resolved.notebook + ' → ' + (resolved.title || resolved.path)).replace(/[\\\[\]]/g, '\\$&');
      dialog.close(); editor.setRangeText('[' + label + '](' + resolved.url + ')', selection.start, selection.end, 'end'); editor.focus();
    } catch (error) { if (active()) { status.textContent = error.message; insert.disabled = false; } }
  };
  notebook.onchange = search;
  query.oninput = () => { clearTimeout(timer); timer = setTimeout(search, 200); };
  dialog.addEventListener('close', () => { generation++; clearTimeout(timer); });
  return {async open() {
    if (!getNote()?.editable) return;
    origin = getProject().id; sourcePath = getNote().path; selection = {start:editor.selectionStart, end:editor.selectionEnd};
    query.value = ''; clearTarget(); results.replaceChildren(); status.textContent = 'Loading notebooks…'; openDialog(dialog);
    try {
      const projects = await api('/api/projects'); if (!active()) return;
      notebook.replaceChildren(...projects.filter(p => !p.protectedInbox).map(p => new Option(p.name, p.id)));
      notebook.value = origin; await search(); query.focus();
    } catch (error) { notice(error.message); dialog.close(); }
  }};
}
