import test from 'node:test';
import assert from 'node:assert/strict';
import {EditorSelection, EditorState} from '@codemirror/state';
import {ensureSyntaxTree} from '@codemirror/language';
import {EditorView, keymap} from '@codemirror/view';
import {deleteMarkupBackward, insertNewlineContinueMarkup} from '@codemirror/lang-markdown';
import {livePreviewMarkdown} from '../../../frontend/live-preview.mjs';
import {
  activeFormats, blockInsertion, canInsertBlock, continueMarkup, deleteMarkup, insertCodeBlock, insertDivider, insertTable,
  setHeading, stringDoc, toggleInline, toggleLink, toggleList, toggleQuote,
} from '../../../frontend/markdown-commands.mjs';
import {applyChanges} from './markdown-editor.mjs';

// A note as the editor holds it: only \n separates lines, so \r stays a
// character, and the whole tree is parsed.
function note(doc, anchor = 0, head = anchor, readOnly = false) {
  const state = EditorState.create({
    doc,
    selection: EditorSelection.single(anchor, head),
    extensions: [EditorState.lineSeparator.of('\n'), livePreviewMarkdown(), EditorState.readOnly.of(readOnly)],
  });
  return {state, tree: ensureSyntaxTree(state, doc.length, 5000)};
}
// Runs a command; returns {text, anchor, head} after it, or null if refused.
function run(command, doc, anchor = 0, head = anchor) {
  const {state, tree} = note(doc, anchor, head);
  const spec = command(state, tree);
  if (!spec) return null;
  const next = state.update({changes: spec.changes, selection: spec.selection}).state;
  return {text: next.doc.toString(), anchor: next.selection.main.anchor, head: next.selection.main.head};
}
// The text with the selection marked: ‸ for a caret, « and » around a range.
function marked(result) {
  if (!result) return null;
  const {text, anchor, head} = result, from = Math.min(anchor, head), to = Math.max(anchor, head);
  return from === to ? text.slice(0, from) + '‸' + text.slice(from) : text.slice(0, from) + '«' + text.slice(from, to) + '»' + text.slice(to);
}
// Parses a marked text into {doc, anchor, head}.
function unmark(source) {
  const caret = source.indexOf('‸');
  if (caret >= 0) return {doc: source.slice(0, caret) + source.slice(caret + 1), anchor: caret, head: caret};
  const from = source.indexOf('«'), to = source.indexOf('»') - 1;
  return {doc: source.slice(0, from) + source.slice(from + 1, to + 1) + source.slice(to + 2), anchor: from, head: to};
}
const apply = (command, source) => {
  const {doc, anchor, head} = unmark(source);
  return marked(run(command, doc, anchor, head));
};

const commands = {
  bold: (s, t) => toggleInline(s, t, 'strong'),
  italic: (s, t) => toggleInline(s, t, 'emphasis'),
  strike: (s, t) => toggleInline(s, t, 'strike'),
  code: (s, t) => toggleInline(s, t, 'code'),
  link: toggleLink,
  ...Object.fromEntries([0, 1, 2, 3, 4, 5, 6].map(level => ['heading' + level, (s, t) => setHeading(s, t, level)])),
  bullet: (s, t) => toggleList(s, t, 'bullet'),
  ordered: (s, t) => toggleList(s, t, 'ordered'),
  task: (s, t) => toggleList(s, t, 'task'),
  quote: toggleQuote,
  codeBlock: insertCodeBlock,
  table: insertTable,
  divider: insertDivider,
  enter: continueMarkup,
  backspace: deleteMarkup,
};

// ---------------------------------------------------------------------------
// The property: over random notes (LF, CRLF, mixed and lone CR line ends)
// and random selections, every command either refuses (changes nothing) or
// changes only the lines the selection touches, keeps the note's line ends,
// and undoes byte for byte; in a read-only note every command refuses.

function random(seed) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const LINES = [
  '', '', '', 'Plain words in a sentence.', 'Text with *em*, **strong**, ~~gone~~ and `code` inside.',
  'A [link](https://example.com "Title") and [ref][r] and [bare].', '[[Wiki page|label]] and [[Other]]', 'Escapes \\*not\\* &amp; entities',
  '# Heading', '## Two ##', '###### Six', '#nospace', 'Setext title', '===', '---', '***', '- - -',
  '- bullet', '* star', '+ plus', '-', '- ', '1. one', '2) two', '10. ten', '999999999. big', '- [ ] task', '- [x] done', '* [X] star task',
  '  - nested', '    - deeper', '   indented text', '> quote', '>tight', '> > nested quote', '> - quoted item', '> 1. quoted number', '>',
  '```', '```js', '~~~', 'code line', '    indented code', '| a | b |', '| --- | --- |', '| 1 | 2 |',
  '<div>html</div>', '<!-- comment -->', '<br>', '[r]: https://example.com', '[^1]: A footnote.', 'Footnote[^1] here',
  '![image](a.png)', 'tab\there', 'emoji 😀 and é', 'trailing spaces  ', 'lone\rcarriage return', 'ends with backslash\\',
];
function randomNote(next) {
  const style = ['lf', 'crlf', 'mixed', 'cr'][Math.floor(next() * 4)];
  const count = 1 + Math.floor(next() * 12);
  const lines = [];
  if (next() < 0.1) lines.push('---', 'title: Test', '---');
  for (let i = 0; i < count; i++) lines.push(LINES[Math.floor(next() * LINES.length)]);
  let text = '';
  lines.forEach((line, index) => {
    if (index) text += style === 'crlf' || (style === 'mixed' && next() < 0.5) ? '\r\n' : '\n';
    text += style === 'cr' && next() < 0.3 ? line + '\r' : line;
  });
  if (next() < 0.3) text += style === 'crlf' ? '\r\n' : '\n';
  return text;
}
// Positions near line starts and ends are where commands do most.
function randomPosition(next, doc) {
  if (next() < 0.5 || !doc.length) return Math.floor(next() * (doc.length + 1));
  const starts = [0];
  for (let i = 0; i < doc.length; i++) if (doc[i] === '\n') starts.push(i + 1);
  const start = starts[Math.floor(next() * starts.length)];
  return Math.min(doc.length, start + Math.floor(next() * 5));
}
// Inline formatting acts on whole elements, so it may change the rest of the
// paragraph (heading, table cell, task) a selection end is in; line
// commands only the lines the selection touches.
const INLINE_COMMANDS = new Set(['bold', 'italic', 'strike', 'code', 'link']);
const TEXT_BLOCK = /^(Paragraph|ATXHeading\d|SetextHeading\d|TableCell|Task)$/;
function blockAround(tree, pos) {
  for (const side of [1, -1]) {
    for (let node = tree.resolveInner(pos, side); node; node = node.parent) {
      if (TEXT_BLOCK.test(node.name) && node.from <= pos && pos <= node.to) return node;
    }
  }
  return null;
}
function touched(state, tree, from, to, inline) {
  if (inline) {
    from = Math.min(from, blockAround(tree, from)?.from ?? from);
    to = Math.max(to, blockAround(tree, to)?.to ?? to);
  }
  return {from: state.doc.lineAt(from).from, to: state.doc.lineAt(to).to};
}
// The note's blocks and their markup in order: inline formatting over a
// selection keeps every one (a heading stays a heading with its #, a task
// keeps its box, a setext heading its underline).
const SKELETON = new Set([
  'ATXHeading1', 'ATXHeading2', 'ATXHeading3', 'ATXHeading4', 'ATXHeading5', 'ATXHeading6', 'SetextHeading1', 'SetextHeading2',
  'HeaderMark', 'Paragraph', 'BulletList', 'OrderedList', 'ListItem', 'ListMark', 'Task', 'TaskMarker', 'Blockquote', 'QuoteMark',
  'FencedCode', 'CodeBlock', 'HTMLBlock', 'CommentBlock', 'HorizontalRule', 'Table', 'TableHeader', 'TableRow', 'TableCell',
  'FrontMatter', 'LinkReference', 'FootnoteDefinition',
]);
function skeleton(text) {
  const {tree} = note(text);
  const names = [];
  tree.iterate({enter(node) {
    if (SKELETON.has(node.name)) names.push(node.name);
  }});
  return names.join(' ');
}
// Lezer (not goldmark) reads a table delimiter row outside a table, after a
// lazy list line, as the text of a setext heading; formatting that row's
// text changes Lezer's reading, so the skeleton is no oracle there.
function strayDelimiterRow(state, tree) {
  for (let at = 0; at <= state.doc.length; ) {
    const line = state.doc.lineAt(at);
    if (/^\|( *-+ *\|)+[ \t\r]*$/.test(line.text)) {
      let inTable = false;
      for (let node = tree.resolveInner(line.from + 1, 1); node; node = node.parent) if (node.name === 'Table') inTable = true;
      if (!inTable) return true;
    }
    at = line.to + 1;
  }
  return false;
}
// The changes of a CodeMirror change set in order, each applying to the text
// the one before it left, as the editor reports them.
function sequential(changeSet) {
  const result = [];
  changeSet.iterChanges((from, to, _fromB, _toB, inserted) => result.push({from, to, insert: inserted.toString()}));
  return result.reverse();
}

test('every command changes only the lines its selection touches, keeps line ends and undoes exactly', () => {
  const next = random(Number(process.env.FMN_PROPERTY_SEED || 20261123));
  const counts = Object.fromEntries(Object.keys(commands).map(name => [name, {applied: 0, refused: 0}]));
  for (let round = 0; round < Number(process.env.FMN_PROPERTY_ROUNDS || 1500); round++) {
    const doc = randomNote(next);
    const anchor = randomPosition(next, doc), head = next() < 0.4 ? anchor : randomPosition(next, doc);
    const {state, tree} = note(doc, anchor, head);
    const {state: locked, tree: lockedTree} = note(doc, anchor, head, true);
    const from = Math.min(anchor, head), to = Math.max(anchor, head);
    for (const [name, command] of Object.entries(commands)) {
      const {from: lineFrom, to: lineTo} = touched(state, tree, from, to, INLINE_COMMANDS.has(name));
      const context = `${name} on ${JSON.stringify(doc)} at ${anchor}-${head}`;
      assert.equal(command(locked, lockedTree), null, 'read-only: ' + context);
      const spec = command(state, tree);
      if (!spec) {
        counts[name].refused++;
        continue;
      }
      counts[name].applied++;
      let last = -1;
      for (const change of spec.changes) {
        assert.ok(change.from >= last && change.from <= change.to, 'ordered changes: ' + context);
        assert.ok(change.from >= lineFrom && change.to <= lineTo, 'only touched lines change: ' + context);
        last = change.to;
      }
      const transaction = state.update({changes: spec.changes, selection: spec.selection});
      const after = transaction.state.doc.toString();
      // Only Link inside a link changes the selection alone.
      if (spec.changes.length) assert.notEqual(after, doc, 'a command that applies changes the note: ' + context);
      else assert.equal(name, 'link', context);
      assert.ok(spec.selection.anchor <= after.length && spec.selection.head <= after.length, context);
      // Bytes outside the touched lines are identical.
      const tail = doc.length - lineTo;
      assert.equal(after.slice(0, lineFrom), doc.slice(0, lineFrom), context);
      assert.equal(after.slice(after.length - tail), doc.slice(lineTo), context);
      // Line ends: a note with only CRLF breaks keeps only CRLF breaks, and a
      // note without \r gains none.
      if (!doc.includes('\r')) assert.ok(!after.includes('\r'), 'no CR added: ' + context);
      if (/\n/.test(doc) && !/(^|[^\r])\n/.test(doc)) assert.ok(!/(^|[^\r])\n/.test(after), 'CRLF kept: ' + context);
      // Inline formatting over a selection changes no block or its markup.
      if (INLINE_COMMANDS.has(name) && from < to && !strayDelimiterRow(state, tree)) assert.equal(skeleton(after), skeleton(doc), 'blocks kept: ' + context);
      // The editor's own record of the edit undoes it byte for byte.
      const changes = sequential(transaction.changes);
      const done = applyChanges(doc, changes);
      assert.equal(done.text, after, context);
      assert.equal(applyChanges(done.text, done.inverse).text, doc, 'undo: ' + context);
      assert.equal(transaction.changes.invert(state.doc).apply(transaction.state.doc).toString(), doc, 'invert: ' + context);
    }
  }
  // The random notes reach every command both ways.
  for (const [name, {applied, refused}] of Object.entries(counts)) {
    assert.ok(applied > 20, name + ' applied ' + applied + ' times');
    if (!['table', 'divider'].includes(name)) assert.ok(refused > 5, name + ' refused ' + refused + ' times');
  }
});

// ---------------------------------------------------------------------------
// Behaviour, one command at a time (« » mark a selection, ‸ a caret).

const bold = (s, t) => toggleInline(s, t, 'strong');
const italic = (s, t) => toggleInline(s, t, 'emphasis');
const strike = (s, t) => toggleInline(s, t, 'strike');
const code = (s, t) => toggleInline(s, t, 'code');

test('Bold, Italic, Strike and Code wrap the selected text and unwrap where the caret is', () => {
  assert.equal(apply(bold, 'a «word» b'), 'a **«word»** b');
  assert.equal(apply(bold, 'a «word »b'), 'a **«word»** b');
  assert.equal(apply(bold, 'a **wo‸rd** b'), 'a wo‸rd b');
  assert.equal(apply(bold, 'a «**word**» b'), 'a «word» b');
  assert.equal(apply(bold, 'a ‸ b'), 'a **‸** b');
  // A caret right next to the same mark: an empty pair there would join it
  // ("**word******"), so nothing changes.
  assert.equal(apply(bold, 'a **word**‸ b'), null);
  assert.equal(apply(bold, 'a ‸**word** b'), null);
  assert.equal(apply(italic, '*x*‸'), null);
  assert.equal(apply(strike, '~~x~~‸'), null);
  assert.equal(apply(code, '`x`‸'), null);
  assert.equal(apply(bold, 'a *x*‸ b'), null);
  assert.equal(apply(strike, 'a **word**‸ b'), 'a **word**~~‸~~ b');
  // Marks after an odd number of backslashes would be escaped.
  assert.equal(apply(bold, 'a\\‸'), null);
  assert.equal(apply(bold, 'a\\\\‸'), 'a\\\\**‸**');
  assert.equal(apply(bold, '«word\\» b'), null);
  assert.equal(apply(bold, 'a \\«word»'), null);
  // Wrapping italic text in bold makes it bold italic.
  assert.equal(apply(bold, '*«word»*'), '***«word»***');
  assert.equal(apply(italic, '«x»'), '*«x»*');
  assert.equal(apply(italic, '*x‸*'), 'x‸');
  assert.equal(apply(strike, '«gone»'), '~~«gone»~~');
  assert.equal(apply(strike, '~~go‸ne~~'), 'go‸ne');
  assert.equal(apply(code, 'run «ls -l» now'), 'run `«ls -l»` now');
  assert.equal(apply(code, 'a «b`c» d'), 'a ``«b`c»`` d');
  assert.equal(apply(code, '«`x»'), '`` «`x» ``');
  assert.equal(apply(code, 'a `l‸s` b'), 'a l‸s b');
});

test('inline formatting wraps each block of a selection separately and never splits syntax', () => {
  assert.equal(apply(bold, '«one\n\ntwo»'), '**«one**\n\n**two»**');
  assert.equal(apply(bold, '- «one\n- two»'), '- **«one**\n- **two»**');
  assert.equal(apply(bold, '> «one\n> two»'), '> **«one\n> two»**');
  // A selection ending inside a link's address takes in the whole link.
  assert.equal(apply(bold, 'see «the [site](https://ex».com) now'), 'see **«the [site](https://ex.com)»** now');
  assert.equal(apply(bold, 'a **«b** c» d'), 'a **«**b** c»** d');
  // Inside code, an address or raw HTML, formatting would be literal text.
  assert.equal(apply(bold, 'a `co‸de` b'), null);
  assert.equal(apply(bold, '```\nco‸de\n```'), null);
  assert.equal(apply(bold, '<div>\nh‸i\n</div>'), null);
  assert.equal(apply(bold, '[x](ht‸tp://e.com)'), null);
  assert.equal(apply(bold, '«   »'), null);
});

// A selection over a whole heading or task line takes in only its text: the
// heading's # and closing sequence, a setext underline and a task box keep
// their meaning.
test('inline formatting over a whole heading or task line wraps its text, not its markup', () => {
  assert.equal(apply(bold, '«# Title»'), '# **«Title»**');
  assert.equal(apply(bold, '«## a ##»'), '## **«a»** ##');
  assert.equal(apply(bold, '«Title\n===»'), '**«Title»**\n===');
  assert.equal(apply(bold, '«Title\n---»'), '**«Title»**\n---');
  assert.equal(apply(bold, '«Title\r\n===»'), '**«Title»**\r\n===');
  assert.equal(apply(bold, '«- [ ] task»'), '- [ ] **«task»**');
  assert.equal(apply(bold, '- «[ ] task»'), '- [ ] **«task»**');
  assert.equal(apply(bold, '«# One\n# Two»'), '# **«One**\n# **Two»**');
  assert.equal(apply(bold, '«# Head\n\n- [ ] task\n»'), '# **«Head**\n\n- [ ] **task»**\n');
  assert.equal(apply(italic, '«## a ##»'), '## *«a»* ##');
  assert.equal(apply(strike, '«- [x] done»'), '- [x] ~~«done»~~');
  assert.equal(apply(code, '«# Title»'), '# `«Title»`');
  assert.equal(apply(code, '«Title\n===»'), '`«Title»`\n===');
  assert.equal(apply(code, '«- [ ] task»'), '- [ ] `«task»`');
  assert.equal(apply(toggleLink, '«# Title»'), '# [Title](‸)');
  assert.equal(apply(toggleLink, '«## a ##»'), '## [a](‸) ##');
  assert.equal(apply(toggleLink, '«Title\n===»'), '[Title](‸)\n===');
  assert.equal(apply(toggleLink, '«- [ ] task»'), '- [ ] [task](‸)');
  // Nothing but markup selected: nothing to wrap.
  assert.equal(apply(bold, '«# »'), null);
  assert.equal(apply(bold, '«- [ ] »'), null);
  // An end in a later line's quote markers stays out of them: a mark after
  // ">" would make that line a list item or leave the mark unclosed.
  assert.equal(apply(italic, '> > «one\n> >» two'), '> > *«one»*\n> > two');
  assert.equal(apply(bold, '> «one\n>» two'), '> **«one»**\n> two');
  assert.equal(apply(bold, '> one\n«> two»'), '> one\n> **«two»**');
  assert.equal(apply(bold, '> > one\n>« > two»'), '> > one\n> > **«two»**');
  assert.equal(apply(bold, '> one\r\n«>» two'), null);
  // A backslash line break stays whole between the marks' lines: a mark at
  // the next line's start before spaces would begin a list item.
  assert.equal(apply(italic, 'e«nds\\\n»    more'), 'e*«nds»*\\\n    more');
  assert.equal(apply(italic, 'e«nds\\\n    mo»re'), 'e*«nds\\\n    mo»*re');
  assert.equal(apply(bold, 'one«\\\ntwo»'), 'one\\\n**«two»**');
  assert.equal(apply(bold, 'one«\\\n»two'), null);
});

test('Link wraps the selection with the caret in the address, and inside a link selects its address', () => {
  assert.equal(apply(toggleLink, 'see «site» now'), 'see [site](‸) now');
  assert.equal(apply(toggleLink, 'see ‸ now'), 'see [‸]() now');
  assert.equal(apply(toggleLink, 'see [si‸te](https://example.com) now'), 'see [site](«https://example.com») now');
  assert.equal(apply(toggleLink, '«one\n\ntwo»'), null);
  assert.equal(apply(toggleLink, '`co‸de`'), null);
  // Link text with an unmatched bracket, or a link in it, would be no link;
  // after "!" it would be an image, after a backslash literal text.
  assert.equal(apply(toggleLink, 'see «a]b» now'), null);
  assert.equal(apply(toggleLink, 'see «a[b» now'), null);
  // "[b]" may be a reference link, and links do not nest.
  assert.equal(apply(toggleLink, 'see «a [b] c» now'), null);
  assert.equal(apply(toggleLink, 'see «a \\] c» now'), 'see [a \\] c](‸) now');
  assert.equal(apply(toggleLink, '«the [site](https://e.com) now»'), null);
  assert.equal(apply(toggleLink, '«see ![i](a.png)»'), null);
  assert.equal(apply(toggleLink, '«see [[Page]]»'), null);
  assert.equal(apply(toggleLink, 'Hey!«word»'), null);
  assert.equal(apply(toggleLink, 'Hey!‸'), null);
  assert.equal(apply(toggleLink, 'a \\«word»'), null);
  assert.equal(apply(toggleLink, '[a]‸'), null);
});

test('Text style sets or removes a heading prefix on each touched line', () => {
  assert.equal(apply((s, t) => setHeading(s, t, 2), 'Ti‸tle'), '## Ti‸tle');
  assert.equal(apply((s, t) => setHeading(s, t, 3), '# Ti‸tle'), '### Ti‸tle');
  assert.equal(apply((s, t) => setHeading(s, t, 0), '## Ti‸tle ##'), 'Ti‸tle');
  assert.equal(apply((s, t) => setHeading(s, t, 1), '«one\ntwo»'), '«# one\n# two»');
  assert.equal(apply((s, t) => setHeading(s, t, 1), '> quo‸te'), '> # quo‸te');
  assert.equal(apply((s, t) => setHeading(s, t, 2), '## Al‸ready'), null);
  assert.equal(apply((s, t) => setHeading(s, t, 0), 'Plain‸ text'), null);
  // Not inside code, tables, front matter or setext headings, whose meaning
  // a prefix would change beyond the line.
  assert.equal(apply((s, t) => setHeading(s, t, 1), '```\nco‸de\n```'), null);
  assert.equal(apply((s, t) => setHeading(s, t, 1), '| a |\n| - |\n| ‸1 |'), null);
  assert.equal(apply((s, t) => setHeading(s, t, 2), 'Ti‸tle\n==='), null);
  assert.equal(apply((s, t) => setHeading(s, t, 2), '---\nti‸tle: x\n---\n'), null);
});

test('list buttons set, switch and remove list markers without renumbering other lines', () => {
  const bullet = (s, t) => toggleList(s, t, 'bullet'), ordered = (s, t) => toggleList(s, t, 'ordered'), task = (s, t) => toggleList(s, t, 'task');
  assert.equal(apply(bullet, 'it‸em'), '- it‸em');
  assert.equal(apply(bullet, '- it‸em'), 'it‸em');
  assert.equal(apply(bullet, '«a\n\nb»'), '«- a\n\n- b»');
  assert.equal(apply(bullet, '1. it‸em'), '- it‸em');
  assert.equal(apply(task, '* it‸em'), '* [ ] it‸em');
  assert.equal(apply(task, '- [x] it‸em'), 'it‸em');
  assert.equal(apply(ordered, '«a\nb»'), '«1. a\n2. b»');
  assert.equal(apply(ordered, '3. one\n«two\nthree»\n9. four'), '3. one\n«4. two\n5. three»\n9. four');
  assert.equal(apply(ordered, '«1. a\nb»'), '«1. a\n2. b»');
  assert.equal(apply(bullet, '> quo‸te'), '> - quo‸te');
  assert.equal(apply(bullet, '‸'), '- ‸');
  assert.equal(apply(bullet, '‸---'), null);
  // Decision (provisional): a bullet on the blank line right under a
  // paragraph inserts what typing it would, so the paragraph reads as a
  // setext heading ("- " underlines it) until the item has text.
  assert.equal(apply(bullet, 'Para\n‸'), 'Para\n- ‸');
  assert.equal(apply(bullet, '```\nco‸de\n```'), null);
});

// Removing a heading, list or quote prefix never makes the next line, which
// the command does not touch, read as that line's setext underline.
test('removing a prefix above a line of = or - is refused, as that line would underline it', () => {
  const paragraph = (s, t) => setHeading(s, t, 0), bullet = (s, t) => toggleList(s, t, 'bullet'), task = (s, t) => toggleList(s, t, 'task');
  assert.equal(apply(paragraph, '# He‸ad\n---'), null);
  assert.equal(apply(paragraph, '# He‸ad\n==='), null);
  assert.equal(apply(paragraph, '# He‸ad\r\n---\r\n'), null);
  assert.equal(apply(paragraph, '# He‸ad\n- '), null);
  assert.equal(apply(paragraph, '> # He‸ad\n> ---'), null);
  assert.equal(apply(paragraph, '- # He‸ad\n  ---'), null);
  assert.equal(apply(paragraph, '«# a\n===»'), null);
  assert.equal(apply(bullet, '- it‸em\n---'), null);
  assert.equal(apply(bullet, '- it‸em\n==='), null);
  assert.equal(apply(task, '- [ ] it‸em\n---'), null);
  assert.equal(apply(toggleQuote, '> quo‸te\n---'), null);
  assert.equal(apply(toggleQuote, '> > quo‸te\n> ---'), null);
  // A blank line between, another depth or other text: no underline.
  assert.equal(apply(paragraph, '# He‸ad\n\n---'), 'He‸ad\n\n---');
  assert.equal(apply(paragraph, '# He‸ad\n- item'), 'He‸ad\n- item');
  assert.equal(apply(paragraph, '> # He‸ad\n---'), '> He‸ad\n---');
  assert.equal(apply(toggleQuote, '> > quo‸te\n---'), '> quo‸te\n---');
  assert.equal(apply(bullet, '«- a\n- »'), '«a\n»');
  assert.equal(apply(bullet, '- it‸em\n- ---'), 'it‸em\n- ---');
  // Switching to another heading level or list kind keeps the prefix.
  assert.equal(apply((s, t) => setHeading(s, t, 2), '# He‸ad\n---'), '## He‸ad\n---');
  assert.equal(apply((s, t) => toggleList(s, t, 'ordered'), '- it‸em\n---'), '1. it‸em\n---');
});

test('Quote adds or removes one quote level on the touched lines', () => {
  assert.equal(apply(toggleQuote, 'wo‸rds'), '> wo‸rds');
  assert.equal(apply(toggleQuote, '> wo‸rds'), 'wo‸rds');
  assert.equal(apply(toggleQuote, '«a\n\nb»'), '«> a\n>\n> b»');
  assert.equal(apply(toggleQuote, '«> a\n>\n> b»'), '«a\n\nb»');
  assert.equal(apply(toggleQuote, '‸'), '> ‸');
  assert.equal(apply(toggleQuote, '```\nco‸de\n```'), null);
});

test('Insert adds a code block, a 3×3 table or a divider as blocks of their own', () => {
  assert.equal(apply(insertCodeBlock, 'some «code»'), '```\nsome «code»\n```');
  assert.equal(apply(insertCodeBlock, '‸'), '```\n‸\n```');
  assert.equal(apply(insertCodeBlock, 'has ‸```'), '```\nhas ‸```\n```');
  assert.equal(apply(insertCodeBlock, '‸```a`b'), '````\n‸```a`b\n````');
  assert.equal(apply(insertCodeBlock, '> quo‸ted'), '> ```\n> quo‸ted\n> ```');
  assert.equal(apply(insertCodeBlock, '«> a\n> b»'), '> ```\n«> a\n> b»\n> ```');
  // Lines at another quote depth (a lazy line, a blank line) would leave the
  // fence open in a quote of its own.
  assert.equal(apply(insertCodeBlock, '«> a\nb»'), null);
  assert.equal(apply(insertCodeBlock, '«> a\n\n> b»'), null);
  assert.equal(apply(insertCodeBlock, '«a\n> b»'), null);
  assert.equal(apply(insertCodeBlock, '```\nco‸de\n```'), null);
  assert.equal(apply(insertTable, 'Text‸\nmore'),
    'Text\n\n| «Column 1» | Column 2 | Column 3 |\n| --- | --- | --- |\n|  |  |  |\n|  |  |  |\n\nmore');
  assert.equal(apply(insertTable, '‸'), '| «Column 1» | Column 2 | Column 3 |\n| --- | --- | --- |\n|  |  |  |\n|  |  |  |\n');
  assert.equal(apply(insertDivider, 'Text‸'), 'Text\n\n---\n‸');
  assert.equal(apply(insertDivider, 'Text‸\n\nmore'), 'Text\n\n---\n‸\nmore');
  assert.equal(apply(insertDivider, 'a\n‸\nb'), 'a\n\n---\n‸\nb');
  assert.equal(apply(insertDivider, '```\nco‸de\n```'), null);
  // A CRLF note gets CRLF breaks.
  assert.equal(apply(insertDivider, 'Text‸\r\nmore'), 'Text\r\n\r\n---\r\n‸\r\nmore');
});

test('Enter continues list, task and quote markup on the caret line only', () => {
  assert.equal(apply(continueMarkup, '- one‸'), '- one\n- ‸');
  assert.equal(apply(continueMarkup, '* one‸'), '* one\n* ‸');
  assert.equal(apply(continueMarkup, '- [x] done‸'), '- [x] done\n- [ ] ‸');
  assert.equal(apply(continueMarkup, '> said‸'), '> said\n> ‸');
  assert.equal(apply(continueMarkup, '> - quoted‸'), '> - quoted\n> - ‸');
  assert.equal(apply(continueMarkup, '  - nested‸'), '  - nested\n  - ‸');
  assert.equal(apply(continueMarkup, '- sp‸lit'), '- sp\n- ‸lit');
  assert.equal(apply(continueMarkup, '- sp«li»t'), '- sp\n- ‸t');
  // The new item takes the next number; later items keep theirs.
  assert.equal(apply(continueMarkup, '1. one‸\n2. two\n3. three'), '1. one\n2. ‸\n2. two\n3. three');
  assert.equal(apply(continueMarkup, '7) seven‸'), '7) seven\n8) ‸');
  // CRLF notes keep CRLF, also with the caret after the line's \r.
  assert.equal(apply(continueMarkup, '- one‸\r\n- two'), '- one\r\n- ‸\r\n- two');
  assert.equal(apply(continueMarkup, '- one\r‸\n- two'), '- one\r\n- ‸\r\n- two');
  // An empty item or quote line ends the list or quote instead.
  assert.equal(apply(continueMarkup, '- one\n- ‸'), '- one\n‸');
  assert.equal(apply(continueMarkup, '- one\n- [ ] ‸'), '- one\n‸');
  assert.equal(apply(continueMarkup, '> - ‸'), '> ‸');
  assert.equal(apply(continueMarkup, '> ‸'), '‸');
  // Elsewhere Enter is a plain line break.
  assert.equal(apply(continueMarkup, 'plain‸'), null);
  assert.equal(apply(continueMarkup, 'text\n2. no‸t a list'), null);
  assert.equal(apply(continueMarkup, '```\n- co‸de\n```'), null);
  assert.equal(apply(continueMarkup, '-‸ one'), null);
  assert.equal(apply(continueMarkup, '- o«ne\n- tw»o'), null);
});

test('Backspace after list, task or quote markup removes that markup only', () => {
  assert.equal(apply(deleteMarkup, '- ‸one'), '‸one');
  assert.equal(apply(deleteMarkup, '  - ‸one'), '  ‸one');
  assert.equal(apply(deleteMarkup, '- [ ] ‸one'), '- ‸one');
  assert.equal(apply(deleteMarkup, '12. ‸one'), '‸one');
  assert.equal(apply(deleteMarkup, '> ‸one'), '‸one');
  assert.equal(apply(deleteMarkup, '> > ‸one'), '> ‸one');
  assert.equal(apply(deleteMarkup, '> - ‸one'), '> ‸one');
  assert.equal(apply(deleteMarkup, '- o‸ne'), null);
  assert.equal(apply(deleteMarkup, '‸one'), null);
  assert.equal(apply(deleteMarkup, '- «one»'), null);
  assert.equal(apply(deleteMarkup, '```\n- ‸x\n```'), null);
});

test('active formats come from the syntax tree', () => {
  const active = source => {
    const {doc, anchor, head} = unmark(source);
    const {state, tree} = note(doc, anchor, head);
    return activeFormats(state, tree);
  };
  assert.deepEqual(active('a **b‸** c'), {strong: true, emphasis: false, strike: false, code: false, link: false, heading: 0, styled: true, list: null, quote: false});
  assert.equal(active('a **b**‸ c').strong, false);
  assert.equal(active('***b‸***').emphasis, true);
  assert.equal(active('~~b‸~~').strike, true);
  assert.equal(active('`b‸`').code, true);
  assert.equal(active('[b‸](u)').link, true);
  assert.equal(active('## Ti‸tle').heading, 2);
  assert.equal(active('Ti‸tle\n---').heading, 2);
  assert.equal(active('Ti‸tle\n---').styled, false);
  assert.equal(active('## Ti‸tle').styled, true);
  assert.equal(active('| a |\n| - |\n| ‸1 |').styled, false);
  assert.equal(active('- [ ] ta‸sk').list, 'task');
  assert.equal(active('1. o‸ne').list, 'ordered');
  assert.equal(active('- o‸ne').list, 'bullet');
  assert.equal(active('> - o‸ne').quote, true);
  assert.equal(active('```\nco‸de\n```').heading, null);
});

// Image, video, table and divider share one rule for where a block can go.
test('a block can go at a caret outside code, raw HTML, tables and front matter', () => {
  const can = source => {
    const {doc, anchor, head} = unmark(source);
    const {state, tree} = note(doc, anchor, head);
    return canInsertBlock(state, tree);
  };
  for (const source of ['Text‸', '‸', '> quo‸te', '- it‸em', 'a\n\n---‸\n', '# He‸ad']) assert.equal(can(source), true, source);
  for (const source of ['```\nco‸de\n```', '```\n‸', '| a |\n| - |\n| ‸1 |', '---\nti‸tle: x\n---\n', '<div>\nh‸i\n</div>', 'Ti‸tle\n===', '[r]: https://e.‸com'])
    assert.equal(can(source), false, source);
  // Where it cannot, the toolbar's Table and Divider change nothing either.
  assert.equal(apply(insertTable, '```\nco‸de\n```'), null);
  assert.equal(apply(insertDivider, '| a |\n| - |\n| ‸1 |'), null);
});

test('block insertion over a plain string matches the editor', () => {
  for (const source of ['Text‸\nmore', 'a\n‸\nb', '‸', 'Text‸\r\nmore', 'end‸', '\n‸x']) {
    const {doc, anchor} = unmark(source);
    const fromString = blockInsertion(stringDoc(doc), anchor, ['![a](b.png)']);
    const fromEditor = blockInsertion(EditorState.create({doc, extensions: EditorState.lineSeparator.of('\n')}).doc, anchor, ['![a](b.png)']);
    assert.deepEqual(fromString, fromEditor, source);
  }
  assert.deepEqual(blockInsertion(stringDoc('Text\nmore'), 2, ['X']), {from: 4, insert: '\n\nX\n', caret: 8});
});

// The Markdown language brings no keys or edits of its own: Enter and
// Backspace are the commands above (lang-markdown's renumber lists), and
// typing ">" or "/" in HTML never closes a tag.
test('the editor language adds no editing keys and no automatic edits', () => {
  const state = EditorState.create({extensions: [livePreviewMarkdown()]});
  const bindings = state.facet(keymap).flat();
  assert.ok(!bindings.some(binding => binding.run === insertNewlineContinueMarkup || binding.run === deleteMarkupBackward));
  assert.equal(state.facet(EditorView.inputHandler).length, 0);
});
