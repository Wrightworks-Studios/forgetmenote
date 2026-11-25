import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';
import {EditorState} from '@codemirror/state';
import {ensureSyntaxTree, syntaxTree} from '@codemirror/language';
import {GFM, parser} from '@lezer/markdown';
import {livePreviewMarkdown} from '../../../frontend/live-preview.mjs';
import {SpaceRun, ensureTreeIdle, topBlocks} from '../../../frontend/markdown-syntax.mjs';

// A long note of varied top-level blocks, and its whole tree.
const blocks = ['# Chapter', 'A paragraph with *emphasis*.', '> A quote\n> over two lines', '- a\n- list', '| a | b |\n|---|---|\n| 1 | 2 |', '```js\ncode\n```', '---'];
const note = count => Array.from({length: count}, (_, i) => blocks[i % blocks.length].replace('Chapter', 'Chapter ' + i)).join('\n\n') + '\n';
const treeOf = doc => {
  const state = EditorState.create({doc, extensions: [livePreviewMarkdown()]});
  return ensureSyntaxTree(state, state.doc.length, 60000);
};
// Every top-level block overlapping [from, to], read from the first block on.
function reference(tree, from, to) {
  const result = [], cursor = tree.cursor();
  if (!cursor.firstChild()) return result;
  do {
    if (cursor.to < from) continue;
    if (cursor.from > to) break;
    result.push({name: cursor.name, from: cursor.from, to: cursor.to});
  } while (cursor.nextSibling());
  return result;
}

test('the top-level blocks of a range are the ones overlapping it, edges and gaps included', () => {
  const doc = note(700), tree = treeOf(doc);
  assert.equal(tree.length, doc.length);
  let seed = 7;
  const random = limit => (seed = (seed * 1103515245 + 12345) % 2147483648) % limit;
  const points = [0, 1, doc.length - 1, doc.length];
  // Block edges and the blank lines between blocks.
  for (const {index} of doc.matchAll(/\n\n/g)) points.push(index - 1, index, index + 1, index + 2);
  for (let i = 0; i < 400; i++) points.push(random(doc.length + 1));
  for (let i = 0; i < 2000; i++) {
    const a = points[random(points.length)], b = points[random(points.length)];
    const [from, to] = random(4) ? [Math.min(a, b), Math.max(a, b)] : [a, a];
    assert.deepEqual(topBlocks(tree, from, to), reference(tree, from, to), `[${from}, ${to}]`);
  }
  assert.deepEqual(topBlocks(treeOf(''), 0, 0), []);
});

// The live preview reads the blocks around every edit and selection: in the
// middle of a long note that must not walk every block before them.
test('finding the blocks in the middle of a long note costs about what it costs at its start', () => {
  const doc = note(8000), tree = treeOf(doc), middle = doc.indexOf('Chapter 4004');
  const time = at => {
    const started = performance.now();
    for (let i = 0; i < 300; i++) topBlocks(tree, at, at + 10);
    return performance.now() - started;
  };
  time(0);
  time(middle);
  const start = Math.min(time(0), time(0), time(0)),
    inMiddle = Math.min(time(middle), time(middle), time(middle));
  // Walking 4,000 blocks first takes hundreds of times as long.
  assert.ok(inMiddle < start * 20 + 5, `${inMiddle.toFixed(2)} ms in the middle, ${start.toFixed(2)} ms at the start`);
});

// Every edit gives CodeMirror a new parse that starts again from the note's
// start, so parsing the rest of a long note in slices between keystrokes
// only walks the same blocks again and delays the next key: the whole-note
// tree waits until the text has been still for a moment.
test('the whole-note tree is parsed once the text is still, not between keystrokes', async () => {
  let state = EditorState.create({doc: note(3000), extensions: [livePreviewMarkdown()]});
  const view = {get state() {return state}, dom: {isConnected: true}};
  assert.ok(syntaxTree(state).length < state.doc.length);
  const upto = state.doc.length;
  let resolved = null;
  const parsed = ensureTreeIdle(view, upto, {slice: 50, settle: 100}).then(tree => (resolved = tree));
  // A key every 10 ms for 400 ms, near the end of the note.
  for (let key = 0; key < 40; key++) {
    await new Promise(resolve => setTimeout(resolve, 10));
    state = state.update({changes: {from: state.doc.length - 10, insert: 'x'}}).state;
  }
  assert.ok(resolved === null, 'the note was parsed while it was being typed in');
  const tree = await parsed;
  assert.ok(tree.length >= upto, `parsed to ${tree.length} of ${upto}`);
});

// Lezer's HardBreak parser looks for the end of a run of spaces at each of
// its spaces, so a run that does not end its line cost the square of its
// length in one parse step (1.3 s for 40,000 spaces, minutes for 200,000).
// SpaceRun reads such a run at once and must leave every tree as it was.
test('a run of spaces parses at once, to the tree Lezer gives without SpaceRun', () => {
  const plain = parser.configure(GFM), quick = parser.configure([GFM, SpaceRun]);
  const dump = (markdown, text) => {
    const nodes = [];
    markdown.parse(text).iterate({enter: node => void nodes.push(`${node.name} ${node.from}-${node.to}`)});
    return nodes.join('\n');
  };
  const samples = [
    'two  \nbreak', 'three   \nbreak', 'one \nno break', 'tab\t\nno break', ' \t  \nmixed', 'back\\\nslash', 'last line  ',
    'a *em*  b **strong**   c', '* not em *', 'a  *  b  *  c', '_a_   _b_', 'x**   y**', '~~del~~   ~~x~~', 'a ~~ b ~~',
    'see https://example.com   and www.example.com  x', 'mail me@example.com   now', '`code   span`  x', '``  a  ``', '<b  class="x">  html  </b>',
    '# heading   with   spaces   ##   ', '#    ', '- [ ] task   with   spaces', '1.    spaced', '| a   | b |\n|---|---|\n| 1   |   2 |',
    '&amp;   &#35;   x', '   leading', '[link   text]( url  "t" )   x', '![img](a.png)   x', '[a]   [b]\n\n[b]: /x',
    '> quote   with  \n> break', '    indented   code', 'line one  \nline two   \nline three',
  ];
  const corpus = new URL('../../webcore/testdata/render-corpus/', import.meta.url);
  for (const name of readdirSync(corpus).filter(file => file.endsWith('.md'))) samples.push(readFileSync(new URL(name, corpus), 'utf8'));
  let seed = 2463534242;
  const random = limit => {
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return (seed >>> 0) % limit;
  };
  const alphabet = [' ', ' ', ' ', ' ', '\n', '\t', '*', '_', '~', '\\', '`', '#', '-', '>', '[', ']', '(', ')', '<', '&', ';', '|', ':', '/', '.', '@', 'a', 'w'];
  for (let i = 0; i < 20000; i++) samples.push(Array.from({length: random(40)}, () => alphabet[random(alphabet.length)]).join(''));
  for (const text of samples) assert.equal(dump(quick, text), dump(plain, text), JSON.stringify(text));
  const run = ' '.repeat(200000);
  for (const text of ['a' + run + 'x', '# ' + run + 'x', '- a' + run + 'x', '*a*' + run + '*b*' + run + '\nnext']) {
    const started = performance.now(), tree = quick.parse(text), ms = performance.now() - started;
    assert.equal(tree.length, text.length);
    assert.ok(ms < 500, `${JSON.stringify(text.slice(0, 4))}: ${ms.toFixed(1)} ms`);
  }
});
