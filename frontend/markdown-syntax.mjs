import {ensureSyntaxTree, syntaxTree} from '@codemirror/language';

// The editor's Markdown tree, read-only, for every part of the editor that
// needs to know what a node is: live preview, comment anchors and the
// side-by-side pane. Nothing here changes the text.

// What a node is to a reader of the rendered note:
// - 'marker': syntax that frames visible text and is hidden in Visual when
//   the selection is elsewhere (#, *, `, [, ], >, list bullets, fences);
// - 'hidden': source that is never part of the rendered text (link
//   addresses and titles, raw HTML, definitions, front matter, fence info);
// - 'visible': everything else, whose text a reader sees.
const markers = new Set([
  'HeaderMark', 'EmphasisMark', 'CodeMark', 'LinkMark', 'QuoteMark', 'ListMark', 'TaskMarker',
  'StrikethroughMark', 'TableDelimiter', 'WikiLinkMark', 'FrontMatterMark', 'HardBreak',
]);
const hidden = new Set([
  'URL', 'LinkTitle', 'LinkLabel', 'CodeInfo', 'HTMLTag', 'HTMLBlock', 'Comment', 'CommentBlock',
  'ProcessingInstruction', 'ProcessingInstructionBlock', 'LinkReference', 'FrontMatter', 'Image',
]);
export function nodeRole(name) {
  return markers.has(name) ? 'marker' : hidden.has(name) ? 'hidden' : 'visible';
}

// A Lezer Markdown extension every Markdown parser here takes: it reads a
// run of spaces at once. Lezer's HardBreak parser (@lezer/markdown 1.7.2)
// looks for the end of the run at each of its spaces, to see whether a line
// break follows, so a run that does not end its line costs the square of its
// length within one parse step, which no time limit interrupts (1.3 s for
// 40,000 spaces, minutes for 200,000). A run that reaches a line break is
// left to HardBreak, which then reads it once; any other run is plain text,
// skipped whole without a node, so the tree is the one Lezer gives without
// this.
export const SpaceRun = {
  parseInline: [{
    name: 'SpaceRun',
    before: 'HardBreak',
    parse(cx, next, start) {
      if (next !== 32) return -1;
      let end = start + 1;
      while (cx.char(end) === 32) end++;
      return cx.char(end) === 10 ? -1 : end;
    },
  }],
};

// The top-level blocks (children of the Document node) that overlap
// [from, to], as {name, from, to}. Positions between blocks belong to none.
// The live preview asks on every edit and selection change: the cursor
// enters the Document's balanced children at `from` in a few steps, rather
// than walking past every block before it (half of a novel's 2,500).
export function topBlocks(tree, from, to) {
  const result = [];
  const cursor = tree.cursor();
  // The first child ending at `from` or later.
  if (!cursor.childAfter(from - 1)) return result;
  do {
    if (cursor.from > to) break;
    result.push({name: cursor.name, from: cursor.from, to: cursor.to});
  } while (cursor.nextSibling());
  return result;
}

// The top-level block that holds pos, or null between blocks.
export function topBlockAt(tree, pos) {
  for (let node = tree.resolveInner(pos, 1); node; node = node.parent) {
    if (node.parent && !node.parent.parent) return {name: node.name, from: node.from, to: node.to};
    if (!node.parent) break;
  }
  for (let node = tree.resolveInner(pos, -1); node; node = node.parent) {
    if (node.parent && !node.parent.parent) return {name: node.name, from: node.from, to: node.to};
  }
  return null;
}

const idle = typeof requestIdleCallback === 'function'
  ? callback => requestIdleCallback(callback, {timeout: 200})
  : callback => setTimeout(callback, 0);

// The tree parsed up to `upto`, reached in idle slices of at most `slice` ms
// so no slice becomes a long task. CodeMirror itself parses only near the
// viewport; whole-note readers (the rendered pane, comment placement) call
// this off the input task. Resolves null when the view is gone or `signal`
// aborts.
// Every edit gives the editor a new parse that starts again from the note's
// start, reusing what it can: slices between keystrokes would mostly walk
// the same blocks again, seldom finish, and hold up the next key (2 ms a key
// in F1 at 4x). So slices start once the text has been still for `settle`
// ms, and wait again after any change.
export function ensureTreeIdle(view, upto = view.state.doc.length, {slice = 8, settle = 150, signal} = {}) {
  return new Promise(resolve => {
    let doc = view.state.doc;
    const later = () => setTimeout(() => idle(step), settle);
    const step = () => {
      if (signal?.aborted || !view.dom.isConnected) return resolve(null);
      if (view.state.doc !== doc) {
        doc = view.state.doc;
        return later();
      }
      const target = Math.min(upto, view.state.doc.length);
      const tree = ensureSyntaxTree(view.state, target, slice);
      if (tree) return resolve(tree);
      idle(step);
    };
    const current = syntaxTree(view.state);
    if (current.length >= Math.min(upto, view.state.doc.length)) resolve(current);
    else later();
  });
}
