import {Prec} from '@codemirror/state';
import {EditorView, keymap} from '@codemirror/view';
import {ensureSyntaxTree, syntaxTree} from '@codemirror/language';
import {
  activeFormats, canInsertBlock, continueMarkup, deleteMarkup, insertCodeBlock, insertDivider, insertTable, setHeading,
  toggleInline, toggleLink, toggleList, toggleQuote,
} from './markdown-commands.mjs';

// The note editor's toolbar, formatting commands and keys, in every mode.
//
// commandExtensions({history, readOnly}) returns the CodeMirror extensions of
// the commands' keys; history is the note's undo ({run(redo), enabled(redo),
// boundary()}) and readOnly() whether the note is read-only.
// createToolbar({host, view, history, insertMedia, commentSelection,
// openNoteLink}) builds the toolbar into host and returns {update(),
// destroy()}; the editor calls update() after each change of its text,
// selection, focus or editability.
//
// Every command is a text change from markdown-commands.mjs, dispatched as
// one transaction between two undo boundaries, so it is one undo step of its
// own; a read-only note refuses it. The toolbar shows what is active where
// the selection is from the syntax tree. Enter and Backspace continue or
// remove list, task and quote markup on the caret line only; elsewhere they
// are the plain keys.

// Provisional keys (the maintainer may change them here): Bold, Italic and Link use
// the keys every editor shares. Strikethrough and code use keys no browser
// binds: Alt+Shift+5 is Google Docs' strikethrough and Ctrl+` Typora's code
// (Ctrl, not Cmd, on a Mac too: Cmd+` switches windows there, and
// Option+Shift+5 types a character, so a Mac uses Ctrl+Shift+5). Rejected:
// Mod+Shift+X (Firefox switches text direction), Mod+Shift+S (Firefox and
// Edge take a screenshot), Mod+E (address bar search), Mod+Shift+C (DevTools).
export const KEYS = {
  bold: {key: 'Mod-b'},
  italic: {key: 'Mod-i'},
  link: {key: 'Mod-k'},
  strike: {key: 'Alt-Shift-5', mac: 'Ctrl-Shift-5'},
  code: {key: 'Ctrl-`'},
};

// A command's userEvent: the editor puts an undo boundary on each side of
// such a transaction (the live preview's task box sends input.toggle).
const COMMAND_EVENT = 'input.format';
const isCommand = update => update.docChanged && update.transactions.some(tr => tr.isUserEvent(COMMAND_EVENT) || tr.isUserEvent('input.toggle'));

// The tree a command reads, parsed through the selection's lines and `ahead`
// characters after them (a setext underline, the end of a list); null if
// that takes longer than `timeout` ms, and the command is then refused.
function treeFor(state, ahead, timeout) {
  const end = state.doc.lineAt(state.selection.main.to).to;
  return ensureSyntaxTree(state, Math.min(state.doc.length, end + ahead), timeout);
}

// Runs a command from markdown-commands.mjs as one transaction; never inside
// an open IME composition, whose text the input method still owns.
function runner(command, userEvent = COMMAND_EVENT, ahead = 2000, timeout = 200) {
  return view => {
    if (view.state.readOnly || view.composing) return false;
    const tree = treeFor(view.state, ahead, timeout);
    const spec = tree && command(view.state, tree);
    if (!spec) return false;
    view.dispatch({changes: spec.changes, selection: spec.selection, scrollIntoView: true, userEvent: spec.changes.length ? userEvent : 'select'});
    return true;
  };
}
const commands = {
  bold: runner((state, tree) => toggleInline(state, tree, 'strong')),
  italic: runner((state, tree) => toggleInline(state, tree, 'emphasis')),
  strike: runner((state, tree) => toggleInline(state, tree, 'strike')),
  code: runner((state, tree) => toggleInline(state, tree, 'code')),
  link: runner(toggleLink),
  heading: level => runner((state, tree) => setHeading(state, tree, level)),
  list: kind => runner((state, tree) => toggleList(state, tree, kind)),
  quote: runner(toggleQuote),
  codeBlock: runner(insertCodeBlock),
  table: runner(insertTable),
  divider: runner(insertDivider),
};
// Enter and Backspace are typing, not commands: their changes join the undo
// step of the typing around them. They read only the caret's line, which is
// on screen and parsed, and never wait for more.
const enter = runner(continueMarkup, 'input', 1, 20);
const backspace = runner(deleteMarkup, 'delete.backward', 1, 20);

export function commandExtensions({history} = {}) {
  const boundary = update => {
    if (isCommand(update)) history?.boundary?.();
  };
  // The note records an edit in the editor's own update listener; these run
  // just before and just after it.
  return [
    Prec.highest(EditorView.updateListener.of(boundary)),
    Prec.lowest(EditorView.updateListener.of(boundary)),
    Prec.high(keymap.of([
      // The keys stay the editor's even where a command does not apply: Mod-K
      // never opens the app's search from inside the note, and the browser's
      // own bold or bookmarks never run.
      {...KEYS.bold, run: commands.bold, preventDefault: true},
      {...KEYS.italic, run: commands.italic, preventDefault: true},
      {...KEYS.link, run: commands.link, preventDefault: true, stopPropagation: true},
      {...KEYS.strike, run: commands.strike, preventDefault: true},
      {...KEYS.code, run: commands.code, preventDefault: true},
      {key: 'Enter', run: enter},
      {key: 'Backspace', run: backspace},
    ])),
  ];
}

// ---------------------------------------------------------------------------
// Toolbar.

const icons = {
  Undo: 'M9 5 4 10l5 5 M4 10h10a6 6 0 0 1 0 12',
  Redo: 'm15 5 5 5-5 5 M20 10H10a6 6 0 0 0 0 12',
  Bold: 'M6 4h6a4 4 0 0 1 0 8H6z M6 12h7a4 4 0 0 1 0 8H6z',
  Italic: 'M10 4h10 M4 20h10 M15 4 9 20',
  Strikethrough: 'M18 6c-1-3-11-4-12 1-.5 2 2 3 4 4 M14 14c7 3 2 9-5 6-1-.4-2-1-3-2 M3 12h18',
  'Inline code': 'm8 6-6 6 6 6 m8-12 6 6-6 6 m-3-15-2 18',
  Link: 'M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-2 2 M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l2-2',
  'Bulleted list': 'M9 6h11 M9 12h11 M9 18h11 M4.5 6h.01 M4.5 12h.01 M4.5 18h.01',
  'Numbered list': 'M10 6h10 M10 12h10 M10 18h10 M4 4.5 5.5 3.5V9 M3.5 14.5a1.5 1.5 0 0 1 3 0c0 1.2-3 2.4-3 4h3',
  'Task list': 'm3 6 1.5 1.5L7.5 4.5 M3.5 15h4v4h-4z M11 6h9 M11 17h9',
  'Comment on selection': 'M21 11a8 8 0 0 1-8 8H8l-5 3V11a9 9 0 0 1 18 0Z M7 10h10 M7 14h6',
};

const mac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || '');
// A key as the toolbar's tooltip shows it and aria-keyshortcuts names it.
function shortcut(binding) {
  const name = (mac && binding.mac) || binding.key;
  const parts = name.split('-').map(part => (part === 'Mod' ? (mac ? 'Meta' : 'Control') : part === 'Ctrl' ? 'Control' : part.length === 1 ? part.toUpperCase() : part));
  const shown = parts.map(part => (mac ? ({Meta: '⌘', Control: '⌃', Alt: '⌥', Shift: '⇧'}[part] ?? part) : ({Control: 'Ctrl'}[part] ?? part)));
  return {aria: parts.join('+'), title: mac ? shown.join('') : shown.join('+')};
}

export function createToolbar({host, view, history, insertMedia = () => {}, commentSelection = () => {}}) {
  const bar = document.createElement('div');
  bar.className = 'formatting-toolbar';
  bar.setAttribute('role', 'group');
  bar.setAttribute('aria-label', 'Formatting toolbar');
  const controls = [];
  // As every command, never inside an open IME composition (runner()).
  const run = action => {
    if (view.state.readOnly || view.composing) return;
    action(view);
    view.focus();
  };
  const group = label => {
    const element = document.createElement('div');
    element.className = 'formatting-group';
    element.setAttribute('role', 'group');
    element.setAttribute('aria-label', label);
    bar.append(element);
    return element;
  };
  const button = (parent, label, action, {caption = '', enabled = () => true, active = null, key = null} = {}) => {
    const element = document.createElement('button');
    element.type = 'button';
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    for (const [name, value] of Object.entries({viewBox: '0 0 24 24', width: '18', height: '18', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.8', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', focusable: 'false'})) svg.setAttribute(name, value);
    const path = document.createElementNS(svg.namespaceURI, 'path');
    path.setAttribute('d', icons[label]);
    svg.append(path);
    element.append(svg);
    if (caption) element.append(Object.assign(document.createElement('span'), {textContent: caption}));
    element.setAttribute('aria-label', label);
    element.title = label;
    if (key) {
      const {aria, title} = shortcut(key);
      element.setAttribute('aria-keyshortcuts', aria);
      element.title = label + ' (' + title + ')';
    }
    // The editor keeps its focus and selection.
    element.onmousedown = event => event.preventDefault();
    element.onclick = () => run(action);
    parent.append(element);
    controls.push({element, enabled, active});
    return element;
  };
  const select = (parent, label, options, action, enabled = () => true) => {
    const element = document.createElement('select');
    element.setAttribute('aria-label', label);
    element.title = label;
    for (const [value, text] of options) element.add(new Option(text, value));
    element.onchange = () => {
      const value = element.value;
      run(() => action(value));
      update();
    };
    parent.append(element);
    controls.push({element, enabled});
    return element;
  };

  // What is active where the selection is.
  let formats = {};
  const edits = group('Edit history');
  button(edits, 'Undo', () => history?.run(false), {enabled: () => !!history?.enabled(false)});
  button(edits, 'Redo', () => history?.run(true), {enabled: () => !!history?.enabled(true)});
  const blocks = group('Text style');
  const style = select(blocks, 'Text style', [['0', 'Paragraph'], ...[1, 2, 3, 4, 5, 6].map(level => [String(level), 'Heading ' + level])],
    value => commands.heading(Number(value))(view), () => formats.styled);
  const marks = group('Text formatting');
  button(marks, 'Bold', commands.bold, {active: () => formats.strong, key: KEYS.bold});
  button(marks, 'Italic', commands.italic, {active: () => formats.emphasis, key: KEYS.italic});
  button(marks, 'Strikethrough', commands.strike, {active: () => formats.strike, key: KEYS.strike});
  button(marks, 'Inline code', commands.code, {active: () => formats.code, key: KEYS.code});
  button(marks, 'Link', commands.link, {active: () => formats.link, key: KEYS.link});
  const lists = group('Lists');
  button(lists, 'Bulleted list', commands.list('bullet'), {active: () => formats.list === 'bullet'});
  button(lists, 'Numbered list', commands.list('ordered'), {active: () => formats.list === 'ordered'});
  button(lists, 'Task list', commands.list('task'), {active: () => formats.list === 'task'});
  const insertions = {table: commands.table, quote: commands.quote, code: commands.codeBlock, divider: commands.divider};
  // An image, a video, a table and a divider are blocks of their own: they
  // are offered only where a block can go (not in code, a table or front
  // matter), as the commands check again when run.
  const BLOCKS = new Set(['attachment', 'video', 'table', 'divider']);
  const inserts = select(group('Insert content'), 'Insert block', [
    ['', 'Insert'], ['attachment', 'Image or file…'], ['video', 'Video embed…'], ['table', 'Table'], ['quote', 'Quote'], ['code', 'Code block'], ['divider', 'Divider'],
  ], value => {
    inserts.value = '';
    if (value === 'attachment' || value === 'video') {
      const tree = treeFor(view.state, 2000, 200);
      if (tree && canInsertBlock(view.state, tree)) insertMedia(value);
    } else insertions[value]?.(view);
  });
  button(group('Discussion'), 'Comment on selection', commentSelection, {caption: 'Comment', enabled: () => !view.state.selection.main.empty});

  // On a phone the formatting buttons come first.
  const compact = typeof matchMedia === 'function' ? matchMedia('(max-width: 720px)') : null;
  const arrange = () => {
    if (compact?.matches) bar.prepend(marks);
    else blocks.after(marks);
  };
  compact?.addEventListener?.('change', arrange);
  arrange();
  host.append(bar);

  function update() {
    formats = activeFormats(view.state, syntaxTree(view.state));
    const readOnly = view.state.readOnly;
    for (const {element, enabled, active} of controls) {
      element.disabled = readOnly || !enabled();
      if (active) element.setAttribute('aria-pressed', String(!!active()));
    }
    style.value = String(formats.heading ?? 0);
    const blockable = canInsertBlock(view.state, syntaxTree(view.state));
    for (const option of inserts.options) if (BLOCKS.has(option.value)) option.disabled = !blockable;
  }
  update();
  return {
    update,
    destroy() {
      compact?.removeEventListener?.('change', arrange);
      bar.remove();
    },
  };
}
