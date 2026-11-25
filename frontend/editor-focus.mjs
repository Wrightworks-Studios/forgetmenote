import {StateEffect} from '@codemirror/state';
import {Decoration, EditorView, ViewPlugin} from '@codemirror/view';
import {syntaxTree} from '@codemirror/language';
import {topBlockAt} from './markdown-syntax.mjs';

// Focus mode in the note editor, in Visual and Source alike. The page owns
// its settings (internal/web/ui/focus-mode.mjs) and shows them as attributes
// on an ancestor of the editor: the focus-mode class, then data-typewriter
// and data-dim. It sends an fmn-focus-settings event to the editor's content
// whenever they change. Outside focus mode nothing here runs on a key press.
// - Dim others: the lines of the top-level block holding the caret (or the
//   caret's line between blocks) carry fmn-current-block, so the page's CSS
//   can dim every other line (styles/focus.css). Only drawn lines are marked.
// - Typewriter: a caret the keyboard moves (typing, arrows, deleting) is
//   scrolled to the middle of the screen; a click leaves the page in place.
//   Turning it on centres the caret at once.

const current = Decoration.line({class: 'fmn-current-block'});
const typewriter = view => !!view.dom.closest('.focus-mode [data-typewriter]');
const dimming = view => !!view.dom.closest('.focus-mode [data-dim]');
const settingsChanged = StateEffect.define();

// The current block's lines, within the drawn ranges.
export function currentBlock(state, ranges, tree = syntaxTree(state)) {
  const {head} = state.selection.main, doc = state.doc;
  const block = topBlockAt(tree, head) ?? doc.lineAt(head);
  const marks = [];
  for (const {from, to} of ranges) {
    for (let pos = Math.max(from, block.from); pos <= Math.min(to, block.to);) {
      const line = doc.lineAt(pos);
      marks.push(current.range(line.from));
      pos = line.to + 1;
    }
  }
  return Decoration.set(marks, true);
}

export const focusMode = ViewPlugin.fromClass(class {
  constructor(view) {
    this.decorations = this.marks(view);
    this.keyboard = false;
  }
  marks(view) {
    return dimming(view) ? currentBlock(view.state, view.visibleRanges) : Decoration.none;
  }
  update(update) {
    const {view} = update;
    if (update.transactions.some(tr => tr.effects.some(effect => effect.is(settingsChanged)))) this.decorations = this.marks(view);
    else if (this.decorations.size && (update.docChanged || update.selectionSet || update.viewportChanged || syntaxTree(update.startState) !== syntaxTree(update.state))) {
      this.decorations = this.marks(view);
    }
    if ((update.docChanged || update.selectionSet) && this.keyboard && view.hasFocus && !view.composing) this.centre(view);
  }
  centre(view) {
    if (!typewriter(view)) return;
    queueMicrotask(() => {
      if (!this.destroyed) view.dispatch({effects: EditorView.scrollIntoView(view.state.selection.main.head, {y: 'center'})});
    });
  }
  destroy() {
    this.destroyed = true;
  }
}, {
  decorations: plugin => plugin.decorations,
  eventHandlers: {
    keydown() {
      this.keyboard = true;
      return false;
    },
    pointerdown() {
      this.keyboard = false;
      return false;
    },
    'fmn-focus-settings'(_event, view) {
      view.dispatch({effects: settingsChanged.of(null)});
      this.centre(view);
      return true;
    },
  },
});
