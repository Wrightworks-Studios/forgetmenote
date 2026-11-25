import {navigationURL} from './navigation.mjs';
import {notebookPath} from './request.mjs';
import {$,element as el,routeLink} from './dom.mjs';
import {noteTitle} from './editor.mjs';

// Wiki mode reads a notes notebook as a site: its index.md as the home page, a
// page tree, previous/next links within a folder and a backlinks footer. Pages
// stay ordinary notes; this module only arranges and links them.

const collator = new Intl.Collator(undefined, {numeric: true, sensitivity: 'base'});
const parentOf = path => (path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '');
const finite = value => (typeof value === 'number' && Number.isFinite(value) ? value : null);
// An explicit `order` comes first, ascending; then titles in natural order.
function compare(a, b) {
  if (a.order !== b.order) {
    if (a.order === null) return 1;
    if (b.order === null) return -1;
    return a.order - b.order;
  }
  return collator.compare(a.title, b.title) || (a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
}

// Builds folders of pages from the notes collection listing. A folder's
// index.md is its overview: it lends the folder its title and order and is not
// listed again among the folder's pages. Agent instructions are not a page.
export function wikiTree(entries) {
  const folders = new Map();
  const folder = path => {
    let node = folders.get(path);
    if (!node) {
      node = {kind: 'folder', path, name: path.split('/').pop(), title: '', order: null, index: null, children: []};
      folders.set(path, node);
      if (path) folder(parentOf(path)).children.push(node);
    }
    return node;
  };
  const root = folder(''),
    named = new Map();
  for (const entry of entries) {
    const path = entry.path || '';
    if (!path.endsWith('.md') || path === 'AGENTS.md' || path.startsWith('Trash/')) continue;
    const page = {kind: 'page', path, title: noteTitle(entry), order: finite(entry.metadata?.order)};
    const parent = folder(parentOf(path));
    if (path.split('/').pop() === 'index.md') {
      parent.index = page;
      named.set(parent, entry.metadata?.title || entry.title || '');
    } else parent.children.push(page);
  }
  for (const node of folders.values()) {
    node.title = named.get(node) || node.name;
    node.order = node.index?.order ?? null;
  }
  for (const node of folders.values()) node.children.sort(compare);
  return root;
}
export function wikiFolder(tree, path) {
  let node = tree;
  for (const part of path ? path.split('/') : []) {
    node = node.children.find(child => child.kind === 'folder' && child.name === part);
    if (!node) return null;
  }
  return node;
}
function wikiPage(tree, path) {
  const folder = wikiFolder(tree, parentOf(path));
  return folder?.index?.path === path ? folder.index : folder?.children.find(child => child.path === path) || null;
}
// Previous and next run through one folder: its overview, then its pages.
export function wikiPager(tree, path) {
  const folder = wikiFolder(tree, parentOf(path));
  const pages = folder ? [folder.index, ...folder.children.filter(child => child.kind === 'page')].filter(Boolean) : [];
  const at = pages.findIndex(page => page.path === path);
  return at < 0 ? {previous: null, next: null} : {previous: pages[at - 1] || null, next: pages[at + 1] || null};
}
// [[Target]] names a page by its title, in any case, and the page nearest the
// link wins; otherwise by its path from the notebook root, then from the
// linking page's folder. A folder's path names its overview page. The server
// resolves these links the same way for backlinks and maps
// (internal/knowledge/wikilinks.go); change both together.
export function wikiLinkTarget(tree, source, text) {
  const wanted = (text || '').trim();
  if (!tree || !wanted) return null;
  const key = wanted.toLocaleLowerCase(),
    pages = new Set(),
    titled = [];
  const named = (...titles) => titles.some(title => title && title.toLocaleLowerCase() === key);
  const visit = folder => {
    if (folder.index) {
      pages.add(folder.index.path);
      if (named(folder.title, folder.index.title)) titled.push(folder.index.path);
    }
    for (const child of folder.children) {
      if (child.kind === 'folder') visit(child);
      else {
        pages.add(child.path);
        if (named(child.title)) titled.push(child.path);
      }
    }
  };
  visit(tree);
  if (titled.length) {
    const from = parentOf(source || '').split('/');
    const shared = path => {
      const parts = parentOf(path).split('/');
      let n = 0;
      while (n < parts.length && parts[n] && parts[n] === from[n]) n++;
      return n;
    };
    const depth = path => path.split('/').length;
    return titled.sort((a, b) => shared(b) - shared(a) || depth(a) - depth(b) || (a < b ? -1 : a > b ? 1 : 0))[0];
  }
  const relative = wanted.replace(/^\/+/, '').replace(/\.md$/i, '');
  if (!relative || relative.split('/').some(part => !part || part === '.' || part === '..')) return null;
  for (const base of wanted.startsWith('/') ? [''] : ['', parentOf(source || '')]) {
    const path = [base, relative].filter(Boolean).join('/');
    if (pages.has(path + '.md')) return path + '.md';
    const index = wikiFolder(tree, path)?.index;
    if (index) return index.path;
  }
  return null;
}
// [[Target]] or [[Target|label]] in prose; never inside code.
const pageLinkPattern = /\[\[([^[\]|\n]+)(?:\|([^[\]\n]+))?\]\]/g;
function pageLinkTexts(root) {
  const texts = [],
    walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  while (walker.nextNode()) {
    const text = walker.currentNode;
    if (text.nodeValue.includes('[[') && !text.parentElement?.closest('pre, code, kbd, samp, a, button, script, style, textarea')) texts.push(text);
  }
  return texts;
}
// A link's explicit layout wins, then this browser's last choice for the
// notebook, then the notebook's "Open as wiki" default. Repository notebooks and
// the private Inbox have no Markdown pages to arrange.
export function wikiLayout(notebook, requested, remembered) {
  if (!notebook || notebook.kind === 'repository' || notebook.protectedInbox) return 'editor';
  for (const choice of [requested, remembered]) if (choice === 'wiki' || choice === 'editor') return choice;
  return notebook.openAsWiki ? 'wiki' : 'editor';
}

export function initWiki({api, post, notice, getProject, getUser, getNote, openNote, openFolder, openNotebook, refreshProjects, onLayoutChange, canCreate, createPage, previewChanged}) {
  let active = false,
    tree = null,
    revision = '',
    loading = null,
    generation = 0,
    searchRequest = 0,
    footerRequest = 0,
    linkRequest = 0,
    // The generation whose page listing failed, and the pages found for the
    // editor's page links.
    unlisted = -1,
    targets = {tree: null, paths: new Map()},
    // The folder whose generated contents page is showing, while no note is open.
    contentsOf = null;
  const opened = new Set();
  const preferenceKey = () => 'fmn:wiki-layout:' + getUser()?.id + ':' + getProject()?.id;
  function remembered() {
    try {
      return localStorage.getItem(preferenceKey()) || '';
    } catch {
      return '';
    }
  }
  function apply(layout) {
    active = layout === 'wiki';
    document.body.classList.toggle('wiki-layout', active);
    $('wiki-toggle').hidden = wikiLayout(getProject(), 'wiki', '') !== 'wiki';
    $('wiki-toggle').querySelector('span').textContent = active ? 'Editor layout' : 'Read as wiki';
    $('wiki-navigation').hidden = !active;
    if (!active) clearSearch();
  }
  // The URL names the layout whenever it is not what an unremembered visit
  // would show, so reloads, history and shared links keep it.
  const routeLayout = () => (active ? 'wiki' : getProject()?.openAsWiki ? 'editor' : undefined);
  // Ordinary links keep their address, so a new tab opens the same layout.
  function link(label, target, open) {
    const project = getProject(),
      a = el('a', label);
    a.href = navigationURL({project: project.id, collection: 'notes', layout: routeLayout(), ...target});
    return routeLink(a, () => Promise.resolve(open()).catch(error => notice(error.message)));
  }
  const pageLink = (page, label = page.title) => link(label, {note: page.path}, () => openNote(page.path));
  const folderLink = folder => link(folder.title, {folder: folder.path}, () => openFolder(folder.path));
  function load() {
    const project = getProject(),
      request = generation;
    if (!project) return Promise.resolve(null);
    if (tree) return Promise.resolve(tree);
    loading ??= api(notebookPath(project.id) + '/api/notes?collection=notes')
      .then(entries => {
        if (request !== generation) return null;
        tree = wikiTree(entries);
        renderTree();
        return tree;
      })
      .finally(() => {
        if (request === generation) loading = null;
      });
    return loading;
  }
  function renderTree() {
    if (!tree) return;
    const current = getNote()?.path;
    const here = folder => (current ? folder.index?.path === current : contentsOf === folder.path);
    for (let path = current ? parentOf(current) : contentsOf || ''; path; path = parentOf(path)) opened.add(path);
    const item = node => {
      const row = el('li', undefined, 'wiki-' + node.kind);
      if (node.kind === 'page') {
        const a = pageLink(node);
        if (node.path === current) a.setAttribute('aria-current', 'page');
        row.append(a);
        return row;
      }
      const details = el('details'),
        summary = el('summary');
      details.open = opened.has(node.path);
      details.ontoggle = () => (details.open ? opened.add(node.path) : opened.delete(node.path));
      const a = folderLink(node);
      if (here(node)) a.setAttribute('aria-current', 'page');
      summary.append(a);
      const children = el('ul');
      children.append(...node.children.map(item));
      details.append(summary, children);
      row.append(details);
      return row;
    };
    const home = el('li', undefined, 'wiki-home-link'),
      homeLink = link('Home', tree.index ? {note: tree.index.path} : {folder: ''}, () => openFolder(''));
    if (here(tree)) homeLink.setAttribute('aria-current', 'page');
    home.append(homeLink);
    $('wiki-tree').replaceChildren(home, ...tree.children.map(item));
  }
  function clearSearch() {
    searchRequest++;
    $('wiki-results').replaceChildren();
    $('wiki-results').hidden = true;
    $('wiki-search-status').textContent = '';
    $('wiki-tree').hidden = false;
  }
  $('wiki-search').onsubmit = async event => {
    event.preventDefault();
    const text = $('wiki-query').value.trim(),
      project = getProject(),
      request = ++searchRequest;
    if (!text || !project) return clearSearch();
    $('wiki-search-status').textContent = 'Searching…';
    try {
      const result = await post(notebookPath(project.id) + '/api/knowledge/hybrid-search', {text, mode: 'all', scope: 'current', collection: 'notes', limit: 20});
      if (request !== searchRequest || getProject() !== project) return;
      const matches = result.matches || [];
      $('wiki-results').replaceChildren(
        ...matches.map(hit => {
          const row = el('li');
          row.append(link(hit.title || noteTitle({path: hit.path}), {note: hit.path}, () => openNote(hit.path)));
          if (hit.excerpt || hit.text) row.append(el('p', hit.excerpt || hit.text, 'small'));
          return row;
        })
      );
      $('wiki-results').hidden = false;
      $('wiki-tree').hidden = true;
      $('wiki-search-status').textContent = matches.length ? matches.length + (matches.length === 1 ? ' page' : ' pages') : 'No pages match.';
    } catch (error) {
      if (request === searchRequest) $('wiki-search-status').textContent = error.message;
    }
  };
  $('wiki-query').addEventListener('input', () => {
    if (!$('wiki-query').value.trim()) clearSearch();
  });
  function renderPager(path) {
    const {previous, next} = tree ? wikiPager(tree, path) : {};
    const step = (page, label, name) => {
      const a = pageLink(page, '');
      a.className = name;
      a.append(el('small', label), el('span', page.title));
      return a;
    };
    $('wiki-pager').replaceChildren(...[previous && step(previous, 'Previous', 'wiki-previous'), next && step(next, 'Next', 'wiki-next')].filter(Boolean));
  }
  // The "Referenced by" list is the note context panel's evidence: links from
  // other pages, including permitted pages in other notebooks.
  function renderBacklinks(evidence, project) {
    const list = $('wiki-backlinks-list'),
      seen = new Set();
    list.replaceChildren();
    $('wiki-backlinks').hidden = false;
    if (evidence.error) {
      list.append(el('li', 'References are unavailable: ' + evidence.error.message, 'small'));
      return;
    }
    for (const ref of evidence.backlinks || []) {
      const elsewhere = !!ref.sourceProject && ref.sourceProject !== project.id;
      const key = (elsewhere ? ref.sourceProject : '') + '\n' + ref.note;
      if (ref.state?.includes('unavailable') || seen.has(key)) continue;
      seen.add(key);
      const row = el('li');
      if (elsewhere) {
        const a = el('a', (ref.notebook ? ref.notebook + ' → ' : '') + (ref.title || noteTitle({path: ref.note})));
        a.href = navigationURL({project: ref.sourceProject, collection: 'notes', note: ref.note});
        row.append(routeLink(a, () => openNotebook({project: ref.sourceProject, path: ref.note}).catch(error => notice(error.message))));
      } else row.append(pageLink({path: ref.note, title: (tree && wikiPage(tree, ref.note)?.title) || noteTitle({path: ref.note})}));
      list.append(row);
    }
    if (!list.children.length) list.append(el('li', 'No other pages link here yet.', 'small'));
  }
  async function noteOpened() {
    const note = getNote(),
      project = getProject(),
      request = ++footerRequest;
    contentsOf = null;
    $('wiki-pager').replaceChildren();
    $('wiki-backlinks-list').replaceChildren();
    $('wiki-backlinks').hidden = true;
    if (!active || !note?.revision || !project) return;
    renderTree();
    const [, evidence] = await Promise.all([
      load().catch(() => null),
      api(notebookPath(project.id) + '/api/knowledge/evidence?path=' + encodeURIComponent(note.path)).catch(error => ({error}))
    ]);
    if (request !== footerRequest || !active || getProject() !== project || getNote()?.path !== note.path) return;
    renderPager(note.path);
    renderBacklinks(evidence, project);
  }
  function pageReference(source, target, label) {
    const path = wikiLinkTarget(tree, source, target);
    if (path) {
      const a = link(label, {note: path}, () => openNote(path));
      a.className = 'wiki-link';
      return a;
    }
    if (!canCreate()) return el('span', label, {class: 'wiki-link-missing', title: 'No page named “' + target + '” yet'});
    const create = el('button', label, {type: 'button', class: 'wiki-link-missing', title: 'Create page “' + target + '”'});
    create.onclick = () => createPage(target.split('/').pop().replace(/\.md$/i, ''));
    return create;
  }
  function linkPageTexts(root, source) {
    for (const text of pageLinkTexts(root)) {
      const value = text.nodeValue,
        parts = [];
      let last = 0;
      for (const match of value.matchAll(pageLinkPattern)) {
        const target = match[1].trim(),
          label = (match[2] ?? match[1]).trim();
        if (!target || !label) continue;
        parts.push(value.slice(last, match.index), pageReference(source, target, label));
        last = match.index + match[0].length;
      }
      if (!parts.length) continue;
      parts.push(value.slice(last));
      text.replaceWith(...parts.filter(part => part !== ''));
    }
  }
  $('wiki-toggle').onclick = () => {
    const layout = active ? 'editor' : 'wiki';
    try {
      localStorage.setItem(preferenceKey(), layout);
    } catch {}
    apply(layout);
    if (active) load().catch(error => notice(error.message));
    Promise.resolve(onLayoutChange(active)).catch(error => notice(error.message));
  };
  $('wiki-default').onchange = async () => {
    const project = getProject(),
      value = $('wiki-default').checked;
    $('wiki-default').disabled = true;
    $('wiki-setting-status').textContent = '';
    try {
      await post(notebookPath(project.id) + '/api/wiki', {openAsWiki: value});
      await refreshProjects();
      if (getProject()?.id === project.id)
        $('wiki-setting-status').textContent = value ? 'Members now open this notebook as a wiki.' : 'Members now open this notebook in the editor layout.';
    } catch (error) {
      $('wiki-default').checked = !value;
      $('wiki-setting-status').textContent = error.message;
    } finally {
      $('wiki-default').disabled = false;
    }
  };
  return {
    get active() {
      return active;
    },
    routeLayout,
    projectChanged(project) {
      generation++;
      tree = null;
      loading = null;
      revision = '';
      opened.clear();
      $('wiki-tree').replaceChildren();
      $('wiki-query').value = '';
      const query = new URLSearchParams(location.search);
      apply(wikiLayout(project, query.get('project') === project?.id ? query.get('layout') : '', remembered()));
      if (active) load().catch(error => notice(error.message));
    },
    // "Edit page" opens the editor layout for this visit only.
    leave() {
      apply('editor');
    },
    // A folder's overview page, known synchronously once the tree is loaded
    // (null before), so a navigation that holds its token never waits here.
    indexOf(path) {
      return tree ? wikiFolder(tree, path)?.index?.path || '' : null;
    },
    async folderIndex(path) {
      return wikiFolder((await load()) || {children: []}, path)?.index?.path || '';
    },
    showContents(path) {
      const project = getProject(),
        folder = tree && wikiFolder(tree, path);
      const contents = node =>
        node.children.map(child => {
          const row = el('li', undefined, 'wiki-' + child.kind);
          if (child.kind === 'page') row.append(pageLink(child));
          else {
            const nested = el('ul');
            nested.append(...contents(child));
            row.append(folderLink(child), nested);
          }
          return row;
        });
      $('wiki-home-title').textContent = path ? folder?.title || path.split('/').pop() : project.name;
      $('wiki-contents').replaceChildren(...(folder ? contents(folder) : []));
      $('wiki-home-empty').hidden = !!folder?.children.length;
      $('wiki-home').hidden = false;
      contentsOf = path;
      renderTree();
    },
    noteOpened,
    // Turns [[Page title]] text in a rendered note into page links in either
    // layout, leaving code as written. The first use waits for the listing.
    linkPages(root, source) {
      const request = ++linkRequest,
        project = getProject();
      if (!project || project.kind === 'repository' || !pageLinkTexts(root).length) return;
      if (tree) return linkPageTexts(root, source);
      load()
        .then(loaded => {
          if (!loaded || request !== linkRequest || getProject() !== project || getNote()?.path !== source) return;
          linkPageTexts(root, source);
          previewChanged();
        })
        .catch(() => {});
    },
    // The page a [[name]] link in note `source` opens, for the editor: its
    // path, null when there is none (or the notebook has no pages), or a
    // promise while the pages load.
    pageTarget(name, source) {
      const project = getProject();
      if (!project || project.kind === 'repository') return null;
      if (tree) {
        // The editor asks again as the text changes; answers hold per listing.
        if (targets.tree !== tree) targets = {tree, paths: new Map()};
        const key = source + '\n' + name;
        if (!targets.paths.has(key)) targets.paths.set(key, wikiLinkTarget(tree, source, name));
        return targets.paths.get(key);
      }
      // A listing that failed is not asked for again on every keystroke.
      if (unlisted === generation) return null;
      const request = generation;
      return load().then(
        () => undefined,
        error => {
          unlisted = request;
          throw error;
        }
      );
    },
    // Follows a [[name]] link from the editor as the reader's page links do:
    // opens the page, or offers to create a missing one.
    async openPage(name, source) {
      try {
        const loaded = await load(),
          target = name.trim(),
          path = loaded && wikiLinkTarget(loaded, source, target);
        if (path) return await openNote(path);
        if (canCreate()) return createPage(target.split('/').pop().replace(/\.md$/i, ''));
        notice('No page named “' + target + '” yet.');
      } catch (error) {
        notice(error.message);
      }
    },
    // The notebook moved on: rebuild the tree from its new listing.
    revisionChanged(value) {
      if (!value || value === revision) return;
      const first = !revision;
      revision = value;
      if (first || !tree) return;
      generation++;
      tree = null;
      loading = null;
      if (active)
        load()
          .then(() => {
            if (getNote()) noteOpened();
          })
          .catch(() => {});
    },
    openSettings(project, manages) {
      $('wiki-setting').hidden = !manages || !!project.assistantOwner || wikiLayout(project, 'wiki', '') !== 'wiki';
      $('wiki-default').checked = !!project.openAsWiki;
      $('wiki-setting-status').textContent = '';
    }
  };
}
