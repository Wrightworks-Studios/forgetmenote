// The note editor's formatting commands as pure text changes: each takes the
// editor state ({doc, selection, readOnly}, as CodeMirror's) and the Markdown
// syntax tree, and returns {changes, selection} or null. Changes are
// {from, to, insert} in the state's positions, in order; the selection
// ({anchor, head}) is in the changed text's positions. Nothing here touches
// the DOM or dispatches: the editor (editor-commands.mjs) runs a command as one
// transaction, and the app's media insertion uses blockInsertion() over a
// plain string.
//
// A command changes only the lines its selection touches (inline formatting:
// the paragraphs it touches), keeps the note's line ends, and refuses (null)
// in a read-only note or where its markup would mean something else: inside
// code, raw HTML, front matter, a table, a link's address. Enter and
// Backspace change their caret line only and never renumber a list.

// Inline formatting: the marks wrapped around the text, and the node that
// shows the text has them.
const INLINE = {
  strong: {mark: '**', node: 'StrongEmphasis'},
  emphasis: {mark: '*', node: 'Emphasis'},
  strike: {mark: '~~', node: 'Strikethrough'},
  code: {mark: '`', node: 'InlineCode'},
  link: {node: 'Link'},
};
// Blocks whose text inline formatting may wrap.
const TEXT_BLOCKS = new Set([
  'Paragraph', 'ATXHeading1', 'ATXHeading2', 'ATXHeading3', 'ATXHeading4', 'ATXHeading5', 'ATXHeading6',
  'SetextHeading1', 'SetextHeading2', 'TableCell', 'Task',
]);
// Inline syntax whose text is literal: formatting cannot reach into it.
const LITERAL = new Set([
  'InlineCode', 'URL', 'LinkTitle', 'LinkLabel', 'Autolink', 'HTMLTag', 'Comment', 'ProcessingInstruction',
  'Escape', 'Entity', 'Image', 'WikiLink', 'FootnoteReference', 'HardBreak',
]);
// Inline elements that hold formatted text, and their markers.
const CONTAINERS = new Set(['Emphasis', 'StrongEmphasis', 'Strikethrough', 'Link']);
const MARKS = new Set(['EmphasisMark', 'StrikethroughMark', 'LinkMark', 'CodeMark']);
// Blocks a line prefix (heading, list, quote) would change the meaning of
// beyond the line: code, raw HTML, tables, front matter, definitions, rules
// and setext headings, whose underline is another line.
const LITERAL_BLOCKS = new Set([
  'FencedCode', 'CodeBlock', 'HTMLBlock', 'CommentBlock', 'ProcessingInstructionBlock', 'Table', 'FrontMatter',
  'LinkReference', 'FootnoteDefinition', 'HorizontalRule', 'SetextHeading1', 'SetextHeading2',
]);
const HEADINGS = {ATXHeading1: 1, ATXHeading2: 2, ATXHeading3: 3, ATXHeading4: 4, ATXHeading5: 5, ATXHeading6: 6, SetextHeading1: 1, SetextHeading2: 2};

// Insert → Table: three columns, a header and two rows (decision 4).
export const TABLE_TEMPLATE = ['| Column 1 | Column 2 | Column 3 |', '| --- | --- | --- |', '|  |  |  |', '|  |  |  |'];
export const DIVIDER = '---';

// ---------------------------------------------------------------------------
// Lines.

// A line's text without the \r of its \r\n line end; the editor keeps \r as
// a character, so a \r right before \n is the line end and any other \r is
// text.
function lineOf(doc, pos) {
  const line = doc.lineAt(pos);
  const crlf = line.to < doc.length && line.text.endsWith('\r');
  return {from: line.from, to: line.to, end: crlf ? line.to - 1 : line.to, text: crlf ? line.text.slice(0, -1) : line.text};
}
// A caret between a line's \r and \n inserts before the \r.
function beforeLineEnd(doc, pos) {
  const line = lineOf(doc, pos);
  return Math.min(pos, line.end);
}
const previousLine = (doc, line) => (line.from > 0 ? lineOf(doc, line.from - 1) : null);
const nextLine = (doc, line) => (line.to < doc.length ? lineOf(doc, line.to + 1) : null);
const blank = line => /^[ \t]*$/.test(line.text);
// The line break new lines get: the line's own, or for the last line the one
// before it.
function lineBreak(doc, line) {
  if (line.to < doc.length) return line.end < line.to ? '\r\n' : '\n';
  const previous = previousLine(doc, line);
  return previous && previous.end < previous.to ? '\r\n' : '\n';
}
// The lines a selection touches, as {from, to}; a selection ending at a line
// start leaves that line out.
function touchedLines(doc, from, to) {
  const last = to > from && doc.lineAt(to).from === to ? to - 1 : to;
  return {from: doc.lineAt(from).from, to: doc.lineAt(last).to, last};
}
function linesIn(doc, from, to) {
  const lines = [];
  for (let at = from; ; ) {
    const line = lineOf(doc, at);
    lines.push(line);
    if (line.to >= to) return lines;
    at = line.to + 1;
  }
}

// The block markup at a line's start, checked against the tree so only real
// markup counts (a "2." inside a paragraph is text): its quote markers, then
// at most one list marker with an optional task box. Offsets are from the
// line's start. contentFrom is where the line's text starts.
function prefixOf(line, tree) {
  const {text} = line;
  const at = offset => tree.resolveInner(line.from + offset, 1);
  const quotes = [];
  let end = 0;
  for (let match; (match = /^ {0,3}>[ \t]?/.exec(text.slice(end))); ) {
    const mark = end + match[0].indexOf('>');
    if (at(mark).name !== 'QuoteMark') break;
    quotes.push({from: end, mark, to: end + match[0].length});
    end += match[0].length;
  }
  const quoteEnd = end;
  let list = null;
  const match = /^([ \t]*)(?:([-+*])|(\d{1,9})([.)]))([ \t]+|$)/.exec(text.slice(end));
  if (match) {
    const markerFrom = end + match[1].length;
    const node = at(markerFrom);
    if (node.name === 'ListMark' && node.from === line.from + markerFrom) {
      let contentFrom = end + match[0].length, task = null;
      const box = /^\[([ xX])\]([ \t]+|$)/.exec(text.slice(contentFrom));
      if (box && at(contentFrom).name === 'TaskMarker') {
        task = {from: contentFrom, to: contentFrom + 3, space: box[2]};
        contentFrom += box[0].length;
      }
      list = {indentFrom: end, markerFrom, bullet: match[2] || null, number: match[3] ? Number(match[3]) : null,
        delimiter: match[4] || null, space: match[5], task, contentFrom};
      end = contentFrom;
    }
  }
  return {quotes, quoteEnd, list, contentFrom: end};
}
const listKind = prefix => (!prefix.list ? null : prefix.list.task ? 'task' : prefix.list.bullet ? 'bullet' : 'ordered');
// Whether a line's text (after its quote and list markup) is an ATX heading.
const atxHeading = text => /^ {0,3}#{1,6}(?=[ \t]|$)/.test(text);

// Whether the lines [from, to] overlap a node named in `names` (an empty
// line: lies inside one, also as the last line of a block that runs to the
// note's end, such as an unclosed fence); with `partly`, only a node that
// also reaches outside them.
function overlaps(tree, from, to, names, partly = false) {
  let found = false;
  tree.iterate({
    from, to,
    enter(node) {
      if (found) return false;
      const reaches = node.to > from || (from === to && node.to === from && node.from < from);
      if (names.has(node.name) && reaches && node.from < Math.max(to, from + 1) && (!partly || node.from < from || node.to > to)) found = true;
      return !found;
    },
  });
  return found;
}

// ---------------------------------------------------------------------------
// Results.

// The position `pos` maps to after `changes` (in order, not overlapping);
// assoc says which side of an insertion at pos it stays on.
function mapPos(changes, pos, assoc = -1) {
  let delta = 0;
  for (const {from, to, insert} of changes) {
    if (from > pos || (from === pos && (to > from || assoc < 0))) break;
    if (to <= pos) {
      delta += insert.length - (to - from);
      continue;
    }
    return from + delta + (assoc < 0 ? 0 : insert.length);
  }
  return pos + delta;
}
const sorted = changes => changes.filter(change => change.to > change.from || change.insert).sort((a, b) => a.from - b.from || a.to - b.to);
// A result whose selection is the old one mapped; the start and end of a range
// keep to the sides given.
function mapped(state, changes, {start = -1, end = 1, caret = 1} = {}) {
  changes = sorted(changes);
  if (!changes.length) return null;
  const {anchor, head} = state.selection.main;
  if (anchor === head) {
    const pos = mapPos(changes, head, typeof caret === 'function' ? caret(head) : caret);
    return {changes, selection: {anchor: pos, head: pos}};
  }
  const backward = head < anchor, from = Math.min(anchor, head), to = Math.max(anchor, head);
  const a = mapPos(changes, from, typeof start === 'function' ? start(from) : start), b = mapPos(changes, to, typeof end === 'function' ? end(to) : end);
  return {changes, selection: backward ? {anchor: b, head: a} : {anchor: a, head: b}};
}
const usable = state => !state.readOnly;

// ---------------------------------------------------------------------------
// Inline formatting.

// The innermost node named `name` around the selection, or null: a caret must
// be strictly inside it (a caret just after "**bold**" is not in it).
function enclosing(tree, name, from, to) {
  for (let node = tree.resolveInner(from, 1); node; node = node.parent) {
    if (node.name === name && (from === to ? node.from < from && from < node.to : node.from <= from && to <= node.to)) return node;
  }
  for (let node = tree.resolveInner(to, -1); node; node = node.parent) {
    if (node.name === name && (from === to ? node.from < from && from < node.to : node.from <= from && to <= node.to)) return node;
  }
  return null;
}
// The text blocks around [from, to] (a caret's block), as {from, to} of their text.
function textBlocks(state, tree, from, to) {
  const blocks = [];
  tree.iterate({
    from, to,
    enter(node) {
      if (!TEXT_BLOCKS.has(node.name)) return true;
      // The cursor's node, whose children textOf reads.
      if (from === to ? node.from <= from && from <= node.to : node.from < to && node.to > from) blocks.push(textOf(state.doc, node.node));
      return false;
    },
  });
  return blocks;
}
// A text block's text: after a heading's or task's marker, before a closing
// heading sequence or setext underline. Each child access makes a new node
// object, so markers compare by position.
function textOf(doc, node) {
  let from = node.from, to = node.to;
  const first = node.firstChild, last = node.lastChild;
  if (node.name.startsWith('ATXHeading') && first?.name === 'HeaderMark') {
    from = first.to;
    if (last && last.from !== first.from && last.name === 'HeaderMark') to = last.from;
  } else if (node.name.startsWith('SetextHeading') && last?.name === 'HeaderMark') {
    to = doc.lineAt(last.from).from - 1;
  } else if (node.name === 'Task' && first?.name === 'TaskMarker') {
    from = first.to;
  }
  return {from: Math.min(from, to), to};
}
// The inline nodes strictly around pos, outermost first.
function around(tree, pos) {
  const nodes = [];
  tree.iterate({
    from: pos, to: pos,
    enter(node) {
      if (node.from < pos && pos < node.to) nodes.push(node.node);
    },
  });
  return nodes;
}
// The syntax a position inside `node` belongs to: a marker or a link's
// address belongs to its element.
const LINK_PARTS = new Set(['URL', 'LinkTitle', 'LinkLabel', 'LinkMark']);
function owner(node) {
  if (MARKS.has(node.name) || (LINK_PARTS.has(node.name) && node.parent?.name === 'Link')) return node.parent ?? node;
  return node;
}
// [from, to] widened so it splits no inline syntax: an end inside an element
// that does not hold the other end, or inside an element's markers or
// address, moves out to that element's edge. Null when both ends are inside
// literal text (code, an address, raw HTML, an image).
function widen(tree, from, to) {
  for (let round = 0; round < 64; round++) {
    let moved = false;
    for (const start of [true, false]) {
      const pos = start ? from : to, other = start ? to : from;
      for (const node of around(tree, pos)) {
        const unit = owner(node);
        if (!LITERAL.has(unit.name) && !CONTAINERS.has(unit.name)) continue;
        const holdsOther = unit.from < other && other < unit.to;
        if (holdsOther && LITERAL.has(unit.name)) return null;
        if (holdsOther && unit === node) continue;
        const edge = start ? unit.from : unit.to;
        if (start ? edge < from : edge > to) {
          if (start) from = edge;
          else to = edge;
          moved = true;
          break;
        }
      }
      if (moved) break;
    }
    if (!moved) return {from, to};
  }
  return null;
}

// The marks of a node: its first and last marker children (two different
// ones: child nodes compare by position).
function marksOf(node) {
  const first = node.firstChild, last = node.lastChild;
  if (!first || !last || first.from === last.from) return null;
  if (!MARKS.has(first.name) || !MARKS.has(last.name)) return null;
  return [{from: first.from, to: first.to}, {from: last.from, to: last.to}];
}

// The part of [from, to] in a text block, without the spaces (and \r) at its
// ends, which would keep marks from taking effect, and out of the markup
// between its lines: a start in a later line's quote markers moves to the
// line's text and an end in them back to the line before; a backslash line
// break stays outside both ends. Null when nothing is left.
function trimmed(doc, tree, block, from, to) {
  let start = Math.max(from, block.from), end = Math.min(to, block.to);
  const quoteEnd = pos => {
    const line = lineOf(doc, pos);
    return line.from + prefixOf(line, tree).quoteEnd;
  };
  const hardBreak = (pos, side) => {
    const node = tree.resolveInner(pos, side);
    return node.name === 'HardBreak' && (side > 0 ? node.from <= pos && pos < node.to : node.from < pos && pos <= node.to) ? node : null;
  };
  // Each round only moves start forward or end back.
  for (let last = null; end > start && last !== start + ':' + end; ) {
    last = start + ':' + end;
    const text = doc.sliceString(start, end);
    start += text.length - text.trimStart().length;
    end -= text.length - text.trimEnd().length;
    if (end <= start) break;
    const before = hardBreak(start, 1), after = hardBreak(end, -1);
    if (before) start = before.to;
    else if (start < quoteEnd(start)) start = quoteEnd(start);
    if (after) end = after.from;
    else if (doc.lineAt(end).from > block.from && end <= quoteEnd(end)) end = doc.lineAt(end).from - 1;
  }
  return end > start ? {from: start, to: end, block} : null;
}
// Whether marks can go at a caret: not inside literal text or syntax.
function atCaret(tree, pos) {
  const range = widen(tree, pos, pos);
  return !!range && range.from === pos && range.to === pos;
}
// Whether a mark inserted at pos would be escaped: an odd number of
// backslashes right before it.
function escapedAt(doc, pos) {
  let slashes = 0;
  while (pos - slashes > 0 && doc.sliceString(pos - slashes - 1, pos - slashes) === '\\') slashes++;
  return slashes % 2 === 1;
}
// Whether an empty pair of marks at a caret would join the same mark
// character next to it ("**word**" and "****" make "**word******").
const joins = (doc, pos, char) => doc.sliceString(Math.max(0, pos - 1), pos) === char || doc.sliceString(pos, pos + 1) === char;
// Whether [from, to] can be a link's text: no link, image or wiki link in it
// (links do not nest), and its brackets, but for escaped ones, match.
const LINKS = new Set(['Link', 'Image', 'WikiLink', 'Autolink']);
function linkText(doc, tree, from, to) {
  let nested = false;
  tree.iterate({from, to, enter(node) {
    if (LINKS.has(node.name) && node.from < to && node.to > from) nested = true;
    return !nested;
  }});
  if (nested) return false;
  let depth = 0;
  for (const char of doc.sliceString(from, to).replace(/\\[\s\S]/g, '')) {
    if (char === '[') depth++;
    else if (char === ']' && --depth < 0) return false;
  }
  return depth === 0;
}
// Whether "[" at pos would start something other than a link: an image
// after "!", a reference after "]", or escaped text.
const linkStartRefused = (doc, pos) => /[!\]]/.test(doc.sliceString(Math.max(0, pos - 1), pos)) || escapedAt(doc, pos);

const codeFence = text => {
  let longest = 0;
  for (const run of text.match(/`+/g) || []) longest = Math.max(longest, run.length);
  const fence = '`'.repeat(longest + 1), pad = /^`|`$/.test(text) ? ' ' : '';
  return [fence + pad, pad + fence];
};

// Bold, Italic, Strike or Code ('strong', 'emphasis', 'strike', 'code'):
// unwraps the element the selection is in, or wraps the selected text of
// each block it touches (without surrounding spaces), or at a caret inserts
// an empty pair to type into.
export function toggleInline(state, tree, kind) {
  if (!usable(state)) return null;
  const {mark, node: name} = INLINE[kind];
  const {from, to} = state.selection.main;
  const node = enclosing(tree, name, from, to);
  if (node) {
    const marks = marksOf(node.node ?? node);
    if (!marks) return null;
    return mapped(state, marks.map(range => ({...range, insert: ''})));
  }
  const doc = state.doc;
  if (from === to) {
    const at = beforeLineEnd(doc, from);
    const blocks = textBlocks(state, tree, at, at);
    const line = lineOf(doc, from);
    if (!blocks.length && !(blank(line) && !overlaps(tree, line.from, line.to, LITERAL_BLOCKS))) return null;
    if (!atCaret(tree, at)) return null;
    const [open, close] = kind === 'code' ? ['`', '`'] : [mark, mark];
    if (joins(doc, at, open[0]) || escapedAt(doc, at)) return null;
    return {changes: [{from: at, to: at, insert: open + close}], selection: {anchor: at + open.length, head: at + open.length}};
  }
  const changes = [];
  let first = null, last = null;
  for (const block of textBlocks(state, tree, from, to)) {
    const part = trimmed(doc, tree, block, from, to);
    if (!part) continue;
    const range = widen(tree, part.from, part.to);
    if (!range || range.from < block.from || range.to > block.to) return null;
    // A backslash would escape the opening mark, or (but in code, where a
    // backslash is literal) the closing one.
    if (escapedAt(doc, range.from) || (kind !== 'code' && escapedAt(doc, range.to))) return null;
    const [open, close] = kind === 'code' ? codeFence(doc.sliceString(range.from, range.to)) : [mark, mark];
    changes.push({from: range.from, to: range.from, insert: open}, {from: range.to, to: range.to, insert: close});
    first ??= {at: range.from, open};
    last = {at: range.to, close};
  }
  if (!changes.length) return null;
  const result = sorted(changes);
  const a = mapPos(result, first.at, 1), b = mapPos(result, last.at, -1);
  const backward = state.selection.main.head < state.selection.main.anchor;
  return {changes: result, selection: backward ? {anchor: b, head: a} : {anchor: a, head: b}};
}

// Link: wraps the selected text as [text]() with the caret in the address,
// or at a caret inserts []() with the caret in the brackets. Inside a link,
// selects its address (or label) to edit; that changes no text.
export function toggleLink(state, tree) {
  if (!usable(state)) return null;
  const {from, to} = state.selection.main;
  const link = enclosing(tree, 'Link', from, to);
  if (link) {
    const target = (link.node ?? link).getChild('URL') ?? (link.node ?? link).getChild('LinkLabel');
    if (!target) return null;
    return {changes: [], selection: {anchor: target.from, head: target.to}};
  }
  const line = lineOf(state.doc, from);
  if (from === to) {
    const at = beforeLineEnd(state.doc, from);
    if (!textBlocks(state, tree, at, at).length && !(blank(line) && !overlaps(tree, line.from, line.to, LITERAL_BLOCKS))) return null;
    if (!atCaret(tree, at) || linkStartRefused(state.doc, at)) return null;
    return {changes: [{from: at, to: at, insert: '[]()'}], selection: {anchor: at + 1, head: at + 1}};
  }
  const blocks = textBlocks(state, tree, from, to).map(block => trimmed(state.doc, tree, block, from, to)).filter(Boolean);
  if (blocks.length !== 1) return null;
  const range = widen(tree, blocks[0].from, blocks[0].to);
  if (!range || range.from < blocks[0].block.from || range.to > blocks[0].block.to) return null;
  if (linkStartRefused(state.doc, range.from) || escapedAt(state.doc, range.to) || !linkText(state.doc, tree, range.from, range.to)) return null;
  const changes = [{from: range.from, to: range.from, insert: '['}, {from: range.to, to: range.to, insert: ']()'}];
  const caret = range.to + 3;
  return {changes, selection: {anchor: caret, head: caret}};
}

// ---------------------------------------------------------------------------
// Line prefixes: headings, lists and quotes.

// The lines a line command acts on: the touched lines that are not blank, or
// the caret's line when all are. Null when one is in a block a prefix would
// change beyond its line.
function prefixLines(state, tree) {
  const {from, to} = state.selection.main;
  const range = touchedLines(state.doc, from, to);
  if (overlaps(tree, range.from, range.to, LITERAL_BLOCKS)) return null;
  const lines = linesIn(state.doc, range.from, range.last);
  const filled = lines.filter(line => !blank(line));
  return filled.length ? filled : [lines[0]];
}

// Whether the line after `line` would read as its setext underline, making
// it a heading, once `line` holds paragraph text at quote depth `depth`, in a
// list item whose text starts `indent` characters after the quote markers
// (0 outside a list): "===" or "---" (a lone "- " too) right under it, at
// that depth and indent. A line the command changes as well (in `changed`)
// is not read.
function underlined(doc, tree, line, depth, indent, changed) {
  const next = nextLine(doc, line);
  if (!next || changed.has(next.from)) return false;
  const prefix = prefixOf(next, tree);
  if (prefix.quotes.length !== depth) return false;
  const rest = next.text.slice(prefix.quoteEnd);
  return /^ */.exec(rest)[0].length >= indent && /^ {0,3}(?:=+|-+)[ \t]*$/.test(rest.slice(indent));
}
// Where a list item's text starts after the quote markers: the column the
// lines under it are indented to.
const itemIndent = prefix => (prefix.list ? (prefix.list.task ? prefix.list.task.from : prefix.list.contentFrom) - prefix.quoteEnd : 0);
// The lines that `changes` (each within one line) touch, by their start.
const changedLines = (doc, changes) => new Set(changes.map(change => doc.lineAt(change.from).from));

// Text style: level 1–6 makes each touched line that heading, 0 a paragraph.
export function setHeading(state, tree, level) {
  if (!usable(state)) return null;
  const lines = prefixLines(state, tree);
  if (!lines) return null;
  const changes = [];
  for (const line of lines) {
    const prefix = prefixOf(line, tree), content = line.text.slice(prefix.contentFrom);
    const heading = /^( {0,3})(#{1,6})(?=[ \t]|$)[ \t]*/.exec(content);
    const at = line.from + prefix.contentFrom;
    if (heading) {
      if (heading[2].length === level) continue;
      const start = at + heading[1].length;
      changes.push({from: start, to: at + heading[0].length, insert: level ? '#'.repeat(level) + ' ' : ''});
      // A paragraph keeps no closing sequence.
      const closing = level ? null : /[ \t]+#+[ \t]*$/.exec(content);
      if (closing && closing.index >= heading[0].length) changes.push({from: at + closing.index, to: line.end, insert: ''});
    } else if (level) {
      changes.push({from: at, to: at, insert: '#'.repeat(level) + ' '});
    }
  }
  // A paragraph right above "---" or "===" would be a heading after all.
  if (!level) {
    const changed = changedLines(state.doc, changes);
    for (const line of lines) {
      const prefix = prefixOf(line, tree);
      if (changed.has(line.from) && underlined(state.doc, tree, line, prefix.quotes.length, itemIndent(prefix), changed)) return null;
    }
  }
  return mapped(state, changes);
}

// Bulleted ('bullet'), numbered ('ordered') or task ('task') list: when every
// touched line is already that kind, removes their markers; otherwise makes
// every touched line an item of that kind. New numbers follow the item just
// above; other lines keep theirs.
export function toggleList(state, tree, kind) {
  if (!usable(state)) return null;
  const lines = prefixLines(state, tree);
  if (!lines) return null;
  const prefixes = lines.map(line => prefixOf(line, tree));
  const changes = [];
  if (prefixes.every(prefix => listKind(prefix) === kind)) {
    lines.forEach((line, index) => {
      const {list} = prefixes[index];
      changes.push({from: line.from + list.markerFrom, to: line.from + list.contentFrom, insert: ''});
    });
    // Text right above "---" or "===" would be a heading after all.
    const changed = changedLines(state.doc, changes);
    if (lines.some((line, index) => !atxHeading(line.text.slice(prefixes[index].contentFrom)) && underlined(state.doc, tree, line, prefixes[index].quotes.length, 0, changed))) return null;
    return mapped(state, changes);
  }
  let number = 1, delimiter = '.';
  const above = previousLine(state.doc, lines[0]);
  if (above && !blank(above)) {
    const previous = prefixOf(above, tree).list;
    if (previous?.number != null) {
      number = previous.number + 1;
      delimiter = previous.delimiter;
    }
  }
  lines.forEach((line, index) => {
    const prefix = prefixes[index], {list} = prefix;
    if (listKind(prefix) === kind) {
      if (list.number != null) number = list.number + 1;
      return;
    }
    let marker;
    if (kind === 'ordered') marker = (number <= 999999999 ? number++ : 1) + delimiter + ' ';
    else marker = (list?.bullet || '-') + ' ' + (kind === 'task' ? '[ ] ' : '');
    if (list) changes.push({from: line.from + list.markerFrom, to: line.from + list.contentFrom, insert: marker});
    else {
      const indent = /^[ \t]*/.exec(line.text.slice(prefix.quoteEnd))[0].length;
      const at = line.from + prefix.quoteEnd + indent;
      changes.push({from: at, to: at, insert: marker});
    }
  });
  return mapped(state, changes);
}

// Quote: removes one quote level from the touched lines when every non-blank
// one is quoted, otherwise quotes them all (blank lines between them too).
export function toggleQuote(state, tree) {
  if (!usable(state)) return null;
  const {from, to} = state.selection.main;
  const range = touchedLines(state.doc, from, to);
  // Quoting a whole code block keeps it one; quoting part of one would not.
  if (overlaps(tree, range.from, range.to, LITERAL_BLOCKS, true)) return null;
  const lines = linesIn(state.doc, range.from, range.last);
  const prefixes = lines.map(line => prefixOf(line, tree));
  const filled = lines.filter(line => !blank(line));
  const quoted = filled.length && lines.every((line, index) => blank(line) || prefixes[index].quotes.length);
  const changes = [];
  if (quoted) {
    lines.forEach((line, index) => {
      const quote = prefixes[index].quotes[0];
      if (quote) changes.push({from: line.from + quote.mark, to: line.from + quote.to, insert: ''});
    });
    // Quoted text right above "---" or "===" at the depth it moves to would
    // be a heading after all.
    const changed = changedLines(state.doc, changes);
    if (lines.some((line, index) => {
      const prefix = prefixes[index];
      return prefix.quotes.length && !atxHeading(line.text.slice(prefix.contentFrom)) && underlined(state.doc, tree, line, prefix.quotes.length - 1, itemIndent(prefix), changed);
    })) return null;
  } else {
    for (const line of lines) changes.push({from: line.from, to: line.from, insert: blank(line) && lines.length > 1 ? '>' : '> '});
  }
  return mapped(state, changes);
}

// ---------------------------------------------------------------------------
// Insert: code blocks, tables and dividers.

// Code block: fences the touched lines (in their quote), or at a caret on a
// blank line opens an empty block to type into.
export function insertCodeBlock(state, tree) {
  if (!usable(state)) return null;
  const doc = state.doc, {from, to} = state.selection.main;
  const range = touchedLines(doc, from, to);
  if (overlaps(tree, range.from, range.to, LITERAL_BLOCKS)) return null;
  const lines = linesIn(doc, range.from, range.last);
  const first = lines[0], last = lines[lines.length - 1], eol = lineBreak(doc, last);
  const quote = first.text.slice(0, prefixOf(first, tree).quoteEnd);
  // The fences go in the first line's quote: a line at another depth (a lazy
  // or blank line) would end that quote and leave a fence open.
  const depth = prefixOf(first, tree).quotes.length;
  if (lines.some(line => prefixOf(line, tree).quotes.length !== depth)) return null;
  if (lines.length === 1 && blank(first)) {
    const open = quote + '```' + eol;
    return {changes: [{from: first.from, to: first.from, insert: open + eol + quote + '```'}], selection: {anchor: first.from + open.length, head: first.from + open.length}};
  }
  let longest = 2;
  for (const line of lines) for (const run of line.text.match(/^[ \t>]*(`{3,})/)?.slice(1) || []) longest = Math.max(longest, run.length);
  const fence = quote + '`'.repeat(longest + 1);
  const changes = [{from: first.from, to: first.from, insert: fence + eol}, {from: last.end, to: last.end, insert: eol + fence}];
  return mapped(state, changes, {start: 1, end: -1, caret: pos => (pos === first.from ? 1 : -1)});
}

// Where a block (lines of text) goes for a caret at pos: on a blank line, in
// its place; otherwise after the caret's line. Blank lines keep it apart from
// the text around it. Returns {from, insert, caret}: the insertion and the
// caret after it, at the start of the line after the block.
export function blockInsertion(doc, pos, block) {
  const line = lineOf(doc, pos), eol = lineBreak(doc, line), body = block.join(eol);
  if (blank(line)) {
    const previous = previousLine(doc, line);
    const insert = (previous && !blank(previous) ? eol : '') + body + eol;
    return {from: line.from, insert, caret: line.from + insert.length};
  }
  const next = nextLine(doc, line);
  const insert = eol + eol + body + (!next || !blank(next) ? eol : '');
  const caret = line.end + insert.length + (next && blank(next) ? line.to + 1 - line.end : 0);
  return {from: line.end, insert, caret};
}
// A block after a rule is a block of its own; anywhere else LITERAL_BLOCKS
// would take it in or be split by it.
const INSERT_REFUSED = new Set([...LITERAL_BLOCKS].filter(name => name !== 'HorizontalRule'));
// Whether a block (a table, a divider, an image or a video) can go at the
// caret: not where its line is in code, raw HTML, a table, front matter, a
// definition or a setext heading.
export function canInsertBlock(state, tree) {
  const line = lineOf(state.doc, state.selection.main.head);
  return !overlaps(tree, line.from, line.to, INSERT_REFUSED);
}
function insertBlock(state, tree, block, select = null) {
  if (!usable(state) || !canInsertBlock(state, tree)) return null;
  const {head} = state.selection.main;
  const {from, insert, caret} = blockInsertion(state.doc, head, block);
  if (select) {
    const start = from + insert.indexOf(select);
    return {changes: [{from, to: from, insert}], selection: {anchor: start, head: start + select.length}};
  }
  return {changes: [{from, to: from, insert}], selection: {anchor: caret, head: caret}};
}
// A 3×3 table with its first header selected to type over.
export const insertTable = (state, tree) => insertBlock(state, tree, TABLE_TEMPLATE, 'Column 1');
export const insertDivider = (state, tree) => insertBlock(state, tree, [DIVIDER]);

// The document interface the commands read (CodeMirror's Text), over a
// plain string, for callers without an editor state: lines are separated by
// \n only, as in the editor.
export function stringDoc(text) {
  return {
    length: text.length,
    sliceString: (from, to = text.length) => text.slice(from, to),
    lineAt(pos) {
      const from = pos > 0 ? text.lastIndexOf('\n', pos - 1) + 1 : 0, next = text.indexOf('\n', pos);
      const to = next < 0 ? text.length : next;
      return {from, to, text: text.slice(from, to)};
    },
  };
}

// ---------------------------------------------------------------------------
// Enter and Backspace.

// Enter on a list item, task or quote line: a new line with the same markup
// (the next number, an open task box), or on an empty item or quote line the
// markup removed instead. Null elsewhere: Enter is then a plain line break.
export function continueMarkup(state, tree) {
  if (!usable(state)) return null;
  const doc = state.doc, {from, to} = state.selection.main;
  const line = lineOf(doc, from);
  if (lineOf(doc, to).from !== line.from) return null;
  const prefix = prefixOf(line, tree);
  if (!prefix.quotes.length && !prefix.list) return null;
  if (overlaps(tree, line.from, line.to, LITERAL_BLOCKS)) return null;
  const contentFrom = line.from + prefix.contentFrom;
  if (from < contentFrom) return null;
  if (blank({text: line.text.slice(prefix.contentFrom)})) {
    // An empty item ends its list; an empty quote line ends its quote.
    const start = line.from + (prefix.list ? prefix.list.indentFrom : prefix.quotes[prefix.quotes.length - 1].mark);
    return {changes: [{from: start, to: line.end, insert: ''}], selection: {anchor: start, head: start}};
  }
  const {list} = prefix;
  let markup = line.text.slice(0, prefix.quoteEnd);
  if (list) {
    markup += line.text.slice(list.indentFrom, list.markerFrom);
    markup += list.bullet || (list.number < 999999999 ? list.number + 1 : list.number) + list.delimiter;
    markup += list.space || ' ';
    if (list.task) markup += '[ ]' + (list.task.space || ' ');
  }
  const start = Math.min(from, line.end), end = Math.min(Math.max(to, start), line.end);
  const insert = lineBreak(doc, line) + markup;
  return {changes: [{from: start, to: end, insert}], selection: {anchor: start + insert.length, head: start + insert.length}};
}

// Backspace right after a line's markup removes its innermost part: a task
// box, a list marker (its indent stays) or one quote marker.
export function deleteMarkup(state, tree) {
  if (!usable(state)) return null;
  const {from, to} = state.selection.main;
  if (from !== to) return null;
  const line = lineOf(state.doc, from), prefix = prefixOf(line, tree);
  if (from !== line.from + prefix.contentFrom || from === line.from) return null;
  if (overlaps(tree, line.from, line.to, LITERAL_BLOCKS)) return null;
  const {list, quotes} = prefix;
  let start;
  if (list?.task) start = list.task.from;
  else if (list) start = list.markerFrom;
  else if (quotes.length) start = quotes[quotes.length - 1].mark;
  else return null;
  start += line.from;
  return {changes: [{from: start, to: from, insert: ''}], selection: {anchor: start, head: start}};
}

// ---------------------------------------------------------------------------
// What the toolbar shows as active where the selection is: the inline
// elements around it, the heading level of the caret's line (0 for a
// paragraph, null where a heading cannot be set), whether Text style can
// change it (not a setext heading, whose underline is another line), its
// list kind and whether it is quoted.
export function activeFormats(state, tree) {
  const {from, to, head} = state.selection.main;
  const result = {};
  for (const [kind, {node}] of Object.entries(INLINE)) result[kind] = !!enclosing(tree, node, from, to);
  const line = lineOf(state.doc, head);
  let heading = 0, styled = true, list = null, quote = false;
  for (let node = tree.resolveInner(line.from + prefixOf(line, tree).contentFrom, 1); node; node = node.parent) {
    if (node.name in HEADINGS) heading = HEADINGS[node.name];
    if (LITERAL_BLOCKS.has(node.name)) styled = false;
    if (LITERAL_BLOCKS.has(node.name) && !(node.name in HEADINGS)) heading = null;
    if (node.name === 'ListItem' && !list) list = node.getChild('Task') ? 'task' : node.parent?.name === 'OrderedList' ? 'ordered' : 'bullet';
    if (node.name === 'Blockquote') quote = true;
  }
  return {...result, heading, styled, list, quote};
}
