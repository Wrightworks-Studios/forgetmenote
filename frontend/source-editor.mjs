import {Annotation,Compartment,EditorSelection,EditorState} from '@codemirror/state';
import {EditorView,drawSelection,keymap,placeholder} from '@codemirror/view';
import {HighlightStyle,syntaxHighlighting} from '@codemirror/language';
import {search,searchKeymap} from '@codemirror/search';
import {tags} from '@lezer/highlight';
import {frontMatter,livePreview,livePreviewMarkdown,markPrivateDraft,privateVideos} from './live-preview.mjs';
import {commandExtensions,createToolbar} from './editor-commands.mjs';
import {annotations} from './editor-annotations.mjs';
import {links} from './editor-links.mjs';
import {focusMode} from './editor-focus.mjs';
import {caretBlink} from './editor-caret.mjs';

// The note editor: one CodeMirror view per note, bound to the note's text
// through its changes, serving every mode. Visual is the live preview; Source
// and Side by side show the plain Markdown. Switching reconfigures a
// compartment and never changes the text. Its own edits leave as text
// changes; changes it did not make (peers, agents, undo, programmatic fixes)
// arrive as text changes and carry this annotation, so they are never
// reported back. Offsets are UTF-16 code units, as the live engine counts
// them: only \n separates lines, so \r\n keeps both units. Nothing here
// changes the text by itself: no history, auto-indent, closing brackets or
// list renumbering.
const outside = Annotation.define();

const markdownColors = HighlightStyle.define([
  {tag: tags.heading, fontWeight: '700'},
  {tag: tags.strong, fontWeight: '700'},
  {tag: tags.emphasis, fontStyle: 'italic'},
  {tag: tags.strikethrough, textDecoration: 'line-through'},
  {tag: [tags.link, tags.url], color: 'var(--accent)'},
  {tag: tags.monospace, color: 'var(--success)'},
  {tag: [tags.processingInstruction, tags.meta, tags.quote, tags.contentSeparator], color: 'var(--muted)'},
]);

// The changes of a CodeMirror change set, each applying to the text the one
// before it left (right to left, so every position is an old position).
export function textChanges(changes) {
  const result = [];
  changes.iterChanges((from, to, _fromB, _toB, inserted) => result.push({from, to, insert: inserted.toString()}));
  return result.reverse();
}
// A transaction spec applying text changes in order.
export function changeSpecs(changes) {
  return changes.map(change => ({changes: {from: change.from, to: change.to, insert: change.insert}, sequential: true}));
}
// The selection after replacing [from, to) with `length` characters, as
// <textarea> setRangeText modes define it.
export function rangeSelection(mode, from, length, previous) {
  if (mode === 'select') return EditorSelection.single(from, from + length);
  if (mode === 'start') return EditorSelection.single(from);
  if (mode === 'end') return EditorSelection.single(from + length);
  return previous;
}

// CodeMirror draws only the lines near the screen, so moves that can leave
// them (up, down, a page, the document's ends, select all) go through the
// editor rather than the browser. Enter inserts a plain line break.
const move = (by, extend = false) => view => {
  const {selection} = view.state;
  const ranges = selection.ranges.map(range => {
    const next = by(view, range);
    return extend ? EditorSelection.range(range.anchor, next.head, next.goalColumn) : next;
  });
  view.dispatch({selection: EditorSelection.create(ranges, selection.mainIndex), scrollIntoView: true, userEvent: 'select'});
  return true;
};
const vertical = (forward, page) => (view, range) =>
  view.moveVertically(range, forward, page ? Math.max(view.defaultLineHeight, Math.min(innerHeight, view.dom.clientHeight) - 2 * view.defaultLineHeight) : undefined);
const start = () => EditorSelection.cursor(0), end = view => EditorSelection.cursor(view.state.doc.length);
// For the same reason the browser's find sees only the drawn lines: Ctrl/Cmd+F
// opens CodeMirror's search, which searches the whole note. Only the find
// bindings are taken; select-next-occurrence and go-to-line are not.
const findKeys = searchKeymap.filter(binding => ['Mod-f', 'F3', 'Mod-g', 'Escape'].includes(binding.key));
const sourceKeys = [
  {key: 'ArrowUp', run: move(vertical(false)), shift: move(vertical(false), true)},
  {key: 'ArrowDown', run: move(vertical(true)), shift: move(vertical(true), true)},
  {key: 'PageUp', run: move(vertical(false, true)), shift: move(vertical(false, true), true)},
  {key: 'PageDown', run: move(vertical(true, true)), shift: move(vertical(true, true), true)},
  {key: 'Mod-Home', mac: 'Mod-ArrowUp', run: move(start), shift: move(start, true)},
  {key: 'Mod-End', mac: 'Mod-ArrowDown', run: move(end), shift: move(end, true)},
  {key: 'Mod-a', run: view => {
    view.dispatch({selection: EditorSelection.single(0, view.state.doc.length), userEvent: 'select'});
    return true;
  }},
  {key: 'Enter', run: view => {
    if (view.state.readOnly) return false;
    view.dispatch(view.state.replaceSelection('\n'), {scrollIntoView: true, userEvent: 'input'});
    return true;
  }},
];

export const FORMATS = ['visual', 'source', 'split'];

// The element that scrolls the editor: its own scroller, or the nearest
// scrolling ancestor (the app lets the page column scroll).
function scroller(view) {
  for (let node = view.scrollDOM; node && node !== document.body; node = node.parentElement) {
    if (node.scrollHeight > node.clientHeight + 1 && /(auto|scroll)/.test(getComputedStyle(node).overflowY)) return node;
  }
  return document.scrollingElement;
}
// Runs `change` (a reconfiguration that changes line heights) and scrolls so
// the caret, or else the text at the top of the screen, stays where it was.
function keepPlace(view, change) {
  const area = scroller(view), top = area === document.scrollingElement ? 0 : Math.max(0, area.getBoundingClientRect().top);
  const {head} = view.state.selection.main, caret = view.coordsAtPos(head);
  const rect = view.contentDOM.getBoundingClientRect();
  const anchor = caret && caret.top >= top && caret.bottom <= innerHeight ? head : view.posAtCoords({x: rect.left + 8, y: Math.max(top, rect.top) + 4}, false);
  const before = view.coordsAtPos(anchor, 1)?.top;
  change();
  if (before == null) return;
  view.requestMeasure({
    read: () => view.coordsAtPos(anchor, 1)?.top,
    write: after => {
      if (after != null && Math.abs(after - before) > 1) area.scrollTop += after - before;
    },
  });
}

// Reads, as this user's edit, text the browser changed that `view` has not
// read yet (see takeInput below). CodeMirror has no public call for it, so
// this uses @codemirror/view 6.43.11 internals: the view's DOMObserver
// (view.observer, its pendingRecords() and forceFlush()) and
// view.updateState. package.json pins that version exactly, and the test
// 'takeInput reads CodeMirror internals that this version still has'
// (source-editor.test.mjs) must pass before any upgrade. Without them typing
// can be lost silently, so the page warns once.
let warnedInput = false;
export function takeViewInput(view) {
  const observer = view.observer;
  if (typeof observer?.pendingRecords !== 'function' || typeof observer.forceFlush !== 'function' || typeof view.updateState !== 'number') {
    if (!warnedInput) console.warn('The note editor cannot read typing the browser has not reported yet (CodeMirror changed): typing may be lost when a change arrives.');
    warnedInput = true;
    return;
  }
  if (view.updateState === 0 && observer.pendingRecords().length) observer.forceFlush();
}

// Options: root (the host element), text, changed(changes) for this editor's
// own edits, readOnly, format ('visual', 'source' or 'split'), imageURL(path)
// for notebook images, video(href) for a video player, privateDraft and
// videoHost(href) for the videos Visual holds in a private draft (see
// privateVideos and markPrivateDraft; setPrivateDraft changes it),
// message(text) for a refused paste or drop, and the seams' callbacks:
// history (the note's undo), toolbar (its host), insertMedia,
// commentSelection, openNoteLink, openThread, openLink and pageTarget.
export function createSource({root, text, changed, readOnly = false, format = 'source', imageURL, video, privateDraft = false, videoHost, message = () => {},
  history, toolbar, insertMedia, commentSelection, openNoteLink, openThread, openLink, pageTarget}) {
  const editable = new Compartment(), mode = new Compartment();
  const editing = value => [EditorState.readOnly.of(value), EditorView.editable.of(!value)];
  // No mode lets the browser change what is typed: every mode keeps
  // CodeMirror's autocorrect=off, autocapitalize=off and
  // writingsuggestions=false, which also turns the double-space period of
  // phone and Mac keyboards back into the spaces typed. Spellcheck (below)
  // only underlines; a word changes only when the person picks a suggestion.
  const modes = {
    visual: [livePreview({resolveImage: imageURL, video}), EditorView.contentAttributes.of({'aria-label': 'Note content'})],
    source: [syntaxHighlighting(markdownColors), EditorView.contentAttributes.of({'aria-label': 'Markdown content'})],
  };
  const modeOf = value => (value === 'visual' ? modes.visual : modes.source);
  const notes = annotations({openThread});
  let bar = null, current = FORMATS.includes(format) ? format : 'source';
  // Writing starts after the front matter, which stays a Properties summary
  // until the caret moves into it.
  const doc = EditorState.create({doc: text, extensions: EditorState.lineSeparator.of('\n')}).doc, front = frontMatter(doc);
  const view = new EditorView({
    parent: root,
    state: EditorState.create({
      doc,
      selection: EditorSelection.cursor(front ? Math.min(doc.length, front.to + 1) : 0),
      extensions: [
        EditorState.lineSeparator.of('\n'),
        EditorView.cspNonce.of(document.querySelector('meta[name="source-style-nonce"]')?.content || ''),
        EditorView.lineWrapping,
        // The editor draws the caret and the selection, in the app's colours
        // (the theme below). The browser's own caret took CodeMirror's
        // caret-color, black in an editor not declared dark, and so could not
        // be seen in the dark theme, in Chromium or Safari. CodeMirror's own
        // blink is off: it is a CSS animation that keeps the page drawing
        // frames, and a key's paint then waited for the next one (6-7 ms more
        // at the median in the F1 keystroke budget). caretBlink blinks the
        // caret on a timer instead (editor-caret.mjs).
        drawSelection({cursorBlinkRate: 0}),
        caretBlink,
        EditorView.contentAttributes.of({id: 'editor', spellcheck: 'true'}),
        placeholder('Start writing…'),
        editable.of(editing(readOnly)),
        // The language is part of the editor from the start, so Visual's
        // first paint is already formatted.
        livePreviewMarkdown(),
        mode.of(modeOf(current)),
        // Whether the text is a private draft outlives mode changes.
        privateVideos({privateDraft, host: videoHost}),
        keymap.of([...sourceKeys, ...findKeys]),
        commandExtensions({history, readOnly: () => view.state.readOnly}),
        notes.extension,
        links({open: openLink, pageTarget}),
        focusMode,
        // A match is centred, clear of the sticky toolbar above the editor.
        search({scrollToMatch: range => EditorView.scrollIntoView(range, {y: 'center'})}),
        // Files go through Insert, and dragging never moves text. Pastes
        // insert plain text only (CodeMirror reads text/plain): a clipboard
        // holding text pastes its text even when it also holds an image, as
        // spreadsheet and Office copies do (and a file copied in a Mac's
        // Finder pastes its name, as in any text field). Only files without
        // text are refused.
        EditorView.domEventHandlers({
          paste(event) {
            if (!event.clipboardData?.files.length || event.clipboardData.getData('text/plain')) return false;
            event.preventDefault();
            message('Use Insert → Image or file to add files to this project.');
            return true;
          },
          drop(event) {
            event.preventDefault();
            message('Use Insert → Image or file to add files, or cut and paste to move text.');
            return true;
          },
        }),
        // The note records an edit before the toolbar reads its undo state.
        EditorView.updateListener.of(update => {
          if (update.docChanged && !update.transactions.some(tr => tr.annotation(outside))) changed(textChanges(update.changes));
          if (bar && (update.docChanged || update.selectionSet || update.focusChanged || update.startState.readOnly !== update.state.readOnly)) bar.update();
        }),
        EditorView.theme({
          '&': {backgroundColor: 'transparent', color: 'inherit'},
          '&.cm-focused': {outline: 'none'},
          '.cm-scroller': {fontFamily: 'inherit', lineHeight: 'inherit'},
          '.cm-placeholder': {color: 'var(--muted)'},
          '.cm-cursor': {borderLeft: '2px solid var(--text)', marginLeft: '-1px'},
          // The drawn selection lies behind the text, where the backgrounds
          // of code, front matter and inline code would hide it, so the
          // browser also paints its own over the text. Both use the app's
          // selection colour, with or without the focus. WebKit darkens an
          // opaque selection colour in a dark colour scheme, and paints an
          // all but opaque one as given.
          '.cm-selectionBackground, &.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground': {background: 'var(--selection)'},
          '&.cm-editor .cm-line ::selection, &.cm-editor .cm-line::selection': {backgroundColor: 'color-mix(in srgb, var(--selection) 99%, transparent) !important'},
          // The search panel looks like the app's own controls, in every theme.
          '.cm-panels': {backgroundColor: 'var(--surface)', color: 'var(--text)', fontFamily: 'var(--ui-font)', fontSize: 'var(--text-small)', lineHeight: '1.4'},
          '.cm-panels-bottom': {borderTop: '1px solid var(--border)'},
          '.cm-panels-top': {borderBottom: '1px solid var(--border)'},
          '.cm-panel.cm-search': {padding: 'var(--space-2) var(--space-8) var(--space-2) var(--space-3)'},
          '.cm-panel.cm-search label': {display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', fontSize: 'inherit'},
          '.cm-panel.cm-search input[type=checkbox]': {width: 'auto', minHeight: 'auto', verticalAlign: 'middle'},
          '.cm-textfield': {width: '16em', maxWidth: '100%', fontSize: 'inherit', color: 'var(--text)', backgroundColor: 'var(--surface)', border: '1px solid var(--border)', borderRadius: 'var(--radius-control)'},
          '.cm-button': {fontSize: 'inherit', color: 'var(--text)', backgroundColor: 'var(--surface)', backgroundImage: 'none', border: '1px solid var(--border)', borderRadius: 'var(--radius-control)', padding: '0 var(--space-3)', minHeight: '32px'},
          '.cm-button:active': {backgroundImage: 'none'},
          '.cm-panel.cm-search [name=close]': {top: 'var(--space-2)', right: 'var(--space-3)', fontSize: 'var(--text-control)', color: 'var(--muted)', minHeight: 'auto'},
          '.cm-searchMatch': {backgroundColor: 'var(--selected)', borderRadius: '2px'},
          '.cm-searchMatch-selected': {backgroundColor: 'var(--selected)', outline: '2px solid var(--accent)'},
        }),
      ],
    }),
  });
  if (toolbar) bar = createToolbar({host: toolbar, view, history, insertMedia, commentSelection, openNoteLink});
  return {
    view,
    get format() {
      return current;
    },
    // Visual, Source or Side by side: the text, the selection and the place
    // on screen stay.
    setFormat(next) {
      if (!FORMATS.includes(next) || next === current) return;
      const changesMode = (next === 'visual') !== (current === 'visual');
      current = next;
      if (changesMode) keepPlace(view, () => view.dispatch({effects: mode.reconfigure(modeOf(next))}));
    },
    // Changes made elsewhere; the selection maps through them.
    apply(changes) {
      if (changes.length) view.dispatch(view.state.update(...changeSpecs(changes), {annotations: outside.of(true)}));
    },
    // Reads, as this user's edit, text the browser changed that the view has
    // not read yet. The view reads the DOM's changes when its observer
    // reports them, at the end of the browser's task; a listener running
    // before that (Chrome ends an IME composition before the observer reports
    // its last text) finds them unread, and any transaction dispatched then
    // throws them away (EditorView.update clears the observer's pending
    // records; focusing restarts the observer, which drops them). So the note
    // editor calls this before anything reaches the view, and the live
    // session before it applies a change made elsewhere. CodeMirror has no
    // public call for it (takeViewInput). Nothing is read inside an update,
    // which cannot dispatch, nor when nothing is pending (a flush then
    // measures the view).
    takeInput() {
      takeViewInput(view);
    },
    readonly(value) {
      view.dispatch({effects: editable.reconfigure(editing(value))});
    },
    setPrivateDraft(value) {
      markPrivateDraft(view, value);
    },
    focus() {
      view.focus();
    },
    selection() {
      const {from, to, head, anchor} = view.state.selection.main;
      return {start: from, end: to, direction: head < anchor ? 'backward' : 'forward'};
    },
    select(from, to) {
      view.dispatch({selection: EditorSelection.single(from, to), scrollIntoView: true});
    },
    // An undo's place: the caret goes after its change, as CodeMirror's own
    // history puts it, and the change scrolls into view.
    reveal(at) {
      const offset = Math.min(at, view.state.doc.length);
      view.dispatch({selection: EditorSelection.cursor(offset), scrollIntoView: true});
    },
    // A replacement the user asked for (media insert, a link): an edit of
    // this editor like typing.
    replace(insert, from, to, mode) {
      const selection = rangeSelection(mode, from, insert.length, undefined);
      view.dispatch({changes: {from, to, insert}, ...(selection ? {selection} : {}), scrollIntoView: true, userEvent: 'input'});
    },
    // Comment highlights and anchors (editor-annotations.mjs).
    setComments(comments) {
      notes.setComments(view, comments);
    },
    commentAnchor() {
      return notes.commentAnchor(view);
    },
    focusComment(id, anchor) {
      return notes.focusComment(view, id, anchor);
    },
    // The toolbar's state after a change the view did not see (live undo).
    refresh() {
      bar?.update();
    },
    destroy() {
      bar?.destroy();
      view.destroy();
    },
  };
}
