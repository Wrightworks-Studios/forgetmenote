import {ChangeSet, EditorSelection, StateEffect, StateField} from '@codemirror/state';
import {Decoration, EditorView, ViewPlugin} from '@codemirror/view';

// Comment highlights and passage anchors in the note editor, in every mode
// (decision 9).
//
// annotations({openThread, analyze}) returns {extension, setComments,
// commentAnchor, focusComment}:
// - extension: the CodeMirror extensions that draw the highlights; a plain
//   click on one calls openThread(id);
// - setComments(view, comments): the note's comments ({id, anchor,
//   resolved}), whose passages are placed then;
// - commentAnchor(view): a promise of the anchor {quote, prefix, suffix} of
//   the selection in the note's rendered text, or of null when it shows no
//   text; null at once for an empty selection;
// - focusComment(view, id): selects the comment's placed passage and reports
//   whether there is one.
//
// Anchors quote the note's rendered text, which only the whole note gives,
// so placing and making them runs in a worker (writing-worker.mjs over
// rendered-text.mjs), never in the input task. The worker places every
// unresolved passage in the text the editor had when it asked; the ranges it
// returns are mapped through the edits made since, and one an edit has
// changed is dropped. Passages are placed again when the comments change and
// PLACE_AFTER_EDIT ms after the last edit, once no composition is open.
// Between placements the highlights move with the text, and an edit inside
// one removes it. analyze(message) replaces the worker (tests).

const PLACE_AFTER_EDIT = 750;

const highlight = id => Decoration.mark({class: 'comment-highlight', attributes: {'data-comment-id': id}, id});

// Placed passages as decorations.
export const setPassages = StateEffect.define();

// Whether a change replaced or inserted text inside [from, to]; an insertion
// at its edges stays outside.
const touched = (changes, from, to) => {
  let hit = false;
  changes.iterChangedRanges((fromA, toA) => {
    if (fromA < to && toA > from) hit = true;
  });
  return hit;
};

export const passages = StateField.define({
  create: () => Decoration.none,
  update(value, tr) {
    if (tr.docChanged && value.size) {
      // An open composition only moves them (plan risk 5).
      if (!tr.isUserEvent('input.type.compose')) {
        const dropped = new Set();
        value.between(0, tr.startState.doc.length, (from, to, decoration) => {
          if (touched(tr.changes, from, to)) dropped.add(decoration);
        });
        if (dropped.size) value = value.update({filter: (_from, _to, decoration) => !dropped.has(decoration)});
      }
      value = value.map(tr.changes);
    }
    for (const effect of tr.effects) if (effect.is(setPassages)) value = effect.value;
    return value;
  },
  provide: field => EditorView.decorations.from(field),
});

// The decorations of ranges ({id, from, to}) placed in a text that the
// changes `since` (a ChangeSet) have edited since.
export function settle(ranges, since) {
  const marks = [];
  for (const {id, from, to} of ranges) {
    if (touched(since, from, to)) continue;
    const start = since.mapPos(from, 1), end = since.mapPos(to, -1);
    if (start < end) marks.push(highlight(id).range(start, end));
  }
  return Decoration.set(marks, true);
}

// The placed passage of comment id, as {from, to}, or null.
export function passage(state, id) {
  let found = null;
  state.field(passages, false)?.between(0, state.doc.length, (from, to, decoration) => {
    if (decoration.spec.id !== id) return;
    found = {from, to};
    return false;
  });
  return found;
}

// The writing worker, started on the first request.
function workerAnalyzer() {
  let worker = null, next = 0;
  const waiting = new Map();
  const fail = message => {
    for (const job of waiting.values()) job.reject(new Error(message));
    waiting.clear();
  };
  return {
    run(message) {
      if (typeof FMN_WRITING_WORKER_URL !== 'string') return Promise.reject(new Error('Comment passages need the app.'));
      if (!worker) {
        worker = new Worker(FMN_WRITING_WORKER_URL, {type: 'module'});
        worker.onmessage = ({data}) => {
          const job = waiting.get(data.id);
          waiting.delete(data.id);
          if (data.error) job?.reject(new Error(data.error));
          else job?.resolve(data);
        };
        worker.onerror = () => {
          worker?.terminate();
          worker = null;
          fail('Comment passages are unavailable. Reload to retry.');
        };
      }
      const id = ++next;
      return new Promise((resolve, reject) => {
        waiting.set(id, {resolve, reject});
        worker.postMessage({...message, id});
      });
    },
    stop() {
      worker?.terminate();
      worker = null;
      fail('The editor closed.');
    },
  };
}

export function annotations({openThread, analyze} = {}) {
  const analyzer = analyze ? {run: analyze, stop() {}} : workerAnalyzer();
  const placer = ViewPlugin.fromClass(class {
    constructor(view) {
      this.view = view;
      this.comments = [];
      this.request = 0;
      // The edits since the text the pending request read.
      this.since = null;
      this.timer = 0;
    }
    update(update) {
      if (!update.docChanged) return;
      if (this.since) this.since = this.since.compose(update.changes);
      if (this.comments.length) this.schedule(PLACE_AFTER_EDIT);
    }
    setComments(comments) {
      this.comments = comments.filter(comment => !comment.resolved && comment.anchor?.quote).map(({id, anchor}) => ({id, anchor}));
      this.schedule(0);
    }
    schedule(delay) {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.place(), delay);
    }
    place() {
      const {view} = this;
      if (this.destroyed) return;
      if (view.composing || view.compositionStarted) return this.schedule(PLACE_AFTER_EDIT);
      const request = ++this.request;
      if (!this.comments.length) {
        this.since = null;
        if (view.state.field(passages).size) view.dispatch({effects: setPassages.of(Decoration.none)});
        return;
      }
      const source = view.state.doc.toString();
      this.since = ChangeSet.empty(source.length);
      analyzer.run({kind: 'comments', source, comments: this.comments}).then(({ranges}) => {
        if (this.destroyed || request !== this.request) return;
        if (view.composing || view.compositionStarted) return this.schedule(PLACE_AFTER_EDIT);
        const placed = settle(ranges, this.since);
        this.since = null;
        view.dispatch({effects: setPassages.of(placed)});
      }, () => {
        if (request === this.request) this.since = null;
      });
    }
    destroy() {
      this.destroyed = true;
      clearTimeout(this.timer);
      analyzer.stop();
    }
  });
  // A plain click on a highlight places the caret as usual and opens the
  // comment's thread. The highlight is the one pressed, and the thread opens
  // on the button's release: placing the caret can redraw the line (live
  // preview shows its markers), and then the browser sends no click.
  let pressed = null;
  const extension = [
    passages,
    placer,
    EditorView.domEventHandlers({
      mousedown(event, view) {
        const mark = event.target.closest?.('.comment-highlight[data-comment-id]');
        pressed = event.button === 0 && !(event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) && mark && view.contentDOM.contains(mark) ? mark.dataset.commentId : null;
        return false;
      },
      mouseup(_event, view) {
        const id = pressed;
        pressed = null;
        if (id && view.state.selection.main.empty) openThread?.(id);
        return false;
      },
    }),
  ];
  return {
    extension,
    setComments(view, comments) {
      view.plugin(placer)?.setComments(comments || []);
    },
    commentAnchor(view) {
      const {from, to, empty} = view.state.selection.main;
      if (empty) return null;
      return analyzer.run({kind: 'anchor', source: view.state.doc.toString(), from, to}).then(result => result.anchor);
    },
    focusComment(view, id) {
      const found = passage(view.state, id);
      if (!found) return false;
      view.dispatch({selection: EditorSelection.single(found.from, found.to), effects: EditorView.scrollIntoView(found.from, {y: 'center'})});
      view.focus();
      return true;
    },
  };
}
