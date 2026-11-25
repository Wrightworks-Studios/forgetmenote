import test from 'node:test';
import assert from 'node:assert/strict';
import {Compartment, EditorSelection, EditorState} from '@codemirror/state';
import {ensureSyntaxTree, syntaxTree} from '@codemirror/language';
import {EditorView} from '@codemirror/view';
import {
  blockDecorations, deletionEnd, frontMatter, guardedDeletion, heldVideo, imageURL, inlineDecorations, linkDefinitions, livePreview, livePreviewMarkdown, loadVideo, numericColumns, privateVideos, setFocus, setPrivateDraft, tableModel, taskToggle, toggleTask, toggleTaskAt,
} from '../../../frontend/live-preview.mjs';

const make = (doc, selection, extensions = []) => {
  const state = EditorState.create({
    doc, selection: selection == null ? undefined : typeof selection === 'number' ? EditorSelection.cursor(selection) : selection,
    extensions: [EditorState.lineSeparator.of('\n'), livePreviewMarkdown(), ...extensions],
  });
  // syntaxTree(state) keeps the tree the state was created with (its first
  // 3,000 characters); the decorations take the whole one.
  trees.set(state, ensureSyntaxTree(state, state.doc.length, 5000));
  return state;
};
const trees = new WeakMap();
const tree = state => trees.get(state) || ensureSyntaxTree(state, state.doc.length, 5000);
const inline = (state, ranges, selection) => inlineDecorations(state, ranges, selection, {tree: tree(state)});
const block = (state, ranges, selection) => blockDecorations(state, ranges, selection, {tree: tree(state)});
const whole = state => [{from: 0, to: state.doc.length}];
// The decorations of a set as {from, to, kind}: hidden text, a widget (by
// class), a line class or a mark class.
const list = set => {
  const result = [];
  set.between(0, 1e9, (from, to, value) => {
    if ('isReplace' in value) result.push({from, to, kind: value.widget ? value.widget.constructor.name : 'hidden', widget: value.widget, block: value.block});
    else result.push({from, to, kind: (from === to ? 'line:' : 'mark:') + (value.spec.class || ''), attributes: value.spec.attributes});
  });
  return result;
};
// What a reader sees: the text with hidden ranges removed and replacing
// widgets shown as <WidgetName>.
const shown = (state, selection = null, ranges = whole(state)) => {
  const replaced = list(inline(state, ranges, selection)).concat(list(block(state, ranges, selection)))
    .filter(item => item.kind === 'hidden' || (item.widget && item.to > item.from))
    .sort((a, b) => a.from - b.from);
  let text = '', at = 0;
  for (const item of replaced) {
    if (item.from < at) continue;
    text += state.sliceDoc(at, item.from) + (item.kind === 'hidden' ? '' : '<' + item.kind.replace(/Widget$/, '') + '>');
    at = item.to;
  }
  return text + state.sliceDoc(at);
};
const at = (state, pos) => [EditorSelection.cursor(pos)];
const classes = (state, kind) => list(inline(state, whole(state), null)).filter(item => item.kind.startsWith(kind));

test('emphasis, strong, strikethrough and code markers hide away from the selection', () => {
  const doc = 'Some **bold**, *em*, ~~gone~~ and `code` here.', state = make(doc);
  assert.equal(shown(state), 'Some bold, em, gone and code here.');
  // Unfocused, the selection reveals nothing.
  assert.equal(shown(state, null), shown(make(doc, 8), null));
  // The element the caret is in, or next to, shows its markers.
  assert.equal(shown(state, at(state, 8)), 'Some **bold**, em, gone and code here.');
  assert.equal(shown(state, at(state, 13)), 'Some **bold**, em, gone and code here.');
  assert.equal(shown(state, at(state, 14)), 'Some bold, em, gone and code here.');
  assert.equal(shown(state, at(state, 36)), 'Some bold, em, gone and `code` here.');
  // A selection across several elements shows all of them.
  assert.equal(shown(state, [EditorSelection.range(6, 18)]), 'Some **bold**, *em*, gone and code here.');
  const marks = classes(state, 'mark:').map(item => item.kind);
  for (const name of ['cm-lp-strong', 'cm-lp-em', 'cm-lp-strike', 'cm-lp-inline-code']) assert.ok(marks.includes('mark:' + name), name);
});

test('heading markers hide unless the caret is on the heading line', () => {
  const state = make('# Title\n\nText ## not a heading\n\n## Closed ##\n\nSetext\n======');
  assert.equal(shown(state), 'Title\n\nText ## not a heading\n\nClosed\n\nSetext\n');
  assert.equal(shown(state, at(state, 3)), '# Title\n\nText ## not a heading\n\nClosed\n\nSetext\n');
  assert.equal(shown(state, at(state, 40)), 'Title\n\nText ## not a heading\n\n## Closed ##\n\nSetext\n');
  assert.equal(shown(state, at(state, 46)), 'Title\n\nText ## not a heading\n\nClosed\n\nSetext\n======');
  const lines = classes(state, 'line:').map(item => [state.doc.lineAt(item.from).number, item.kind]);
  assert.deepEqual(lines, [[1, 'line:cm-lp-heading cm-lp-h1'], [5, 'line:cm-lp-heading cm-lp-h2'], [7, 'line:cm-lp-heading cm-lp-h1']]);
});

test('heading lines are headings to assistive technology, at their level', () => {
  const state = make('# One\n\nText\n\n### Three\n\nTwo line\nsetext\n---\n\n> ## Quoted');
  const headings = classes(state, 'line:').filter(item => item.kind.includes('cm-lp-heading'))
    .map(item => [state.doc.lineAt(item.from).text, item.kind, item.attributes]);
  const role = level => ({role: 'heading', 'aria-level': String(level)});
  // A setext heading over two lines is one heading.
  assert.deepEqual(headings, [
    ['# One', 'line:cm-lp-heading cm-lp-h1', role(1)],
    ['### Three', 'line:cm-lp-heading cm-lp-h3', role(3)],
    ['Two line', 'line:cm-lp-heading cm-lp-h2', role(2)],
    ['setext', 'line:cm-lp-heading cm-lp-h2', undefined],
    ['> ## Quoted', 'line:cm-lp-heading cm-lp-h2', role(2)],
  ]);
});

test('links and wiki links show their text; bare brackets stay as typed', () => {
  const doc = '[text](http://a.example "T"), <http://b.example>, [short], [ref][r], [[Page]], [[Page|label]] and http://c.example\n\n[r]: http://d.example';
  const state = make(doc);
  assert.equal(shown(state), 'text, http://b.example, [short], ref, Page, label and http://c.example\n\n[r]: http://d.example');
  assert.equal(shown(state, at(state, 3)), '[text](http://a.example "T"), http://b.example, [short], ref, Page, label and http://c.example\n\n[r]: http://d.example');
  assert.equal(shown(state, at(state, doc.indexOf('label'))), 'text, http://b.example, [short], ref, Page, [[Page|label]] and http://c.example\n\n[r]: http://d.example');
  // [ref][r] resolves through the note's definition of r.
  const links = classes(state, 'mark:cm-lp-link').map(item => item.attributes['data-lp-href']);
  assert.deepEqual(links, ['http://a.example', 'http://b.example', 'http://d.example', 'http://c.example']);
  const wiki = classes(state, 'mark:cm-lp-wikilink').map(item => [state.sliceDoc(item.from, item.to), item.attributes['data-lp-wiki']]);
  assert.deepEqual(wiki, [['Page', 'Page'], ['label', 'Page']]);
});

test('escapes, entities and backslash line breaks show what they stand for', () => {
  const state = make('\\*not em\\* and \\[x\\] &amp; &#8212;\\\nnext');
  assert.equal(shown(state), '*not em* and [x] <Entity> <Entity>\nnext');
  // Only what the caret touches shows its source; a line break's backslash
  // shows on its line.
  assert.equal(shown(state, at(state, 1)), '\\*not em* and [x] <Entity> <Entity>\\\nnext');
  assert.equal(shown(state, at(state, 23)), '*not em* and [x] &amp; <Entity>\\\nnext');
  const entities = list(inline(state, whole(state), null)).filter(item => item.kind === 'EntityWidget').map(item => item.widget.entity);
  assert.deepEqual(entities, ['&amp;', '&#8212;']);
});

test('bullets become dots, numbers stay, and tasks become checkboxes', () => {
  const doc = '- one\n  - two\n1. three\n- [ ] open\n- [x] done\n\n1. [ ] numbered';
  const state = make(doc);
  // Each prefix (indentation, marker and one space) is one box.
  assert.equal(shown(state), '<Bullet>one\n<Bullet>two\n<Number>three\n<Task>open\n<Task>done\n\n<Task>numbered');
  const numbers = list(inline(state, whole(state), null)).filter(item => item.widget?.text).map(item => item.widget.text);
  assert.deepEqual(numbers, ['1.', '1.']);
  // A bullet or number shows as typed while the caret is on its line; a task
  // box only when the selection touches it.
  assert.equal(shown(state, at(state, 1)), '- one\n<Bullet>two\n<Number>three\n<Task>open\n<Task>done\n\n<Task>numbered');
  assert.equal(shown(state, at(state, 4)), '- one\n<Bullet>two\n<Number>three\n<Task>open\n<Task>done\n\n<Task>numbered');
  assert.equal(shown(state, at(state, doc.indexOf('three'))), '<Bullet>one\n<Bullet>two\n1. three\n<Task>open\n<Task>done\n\n<Task>numbered');
  assert.equal(shown(state, at(state, doc.indexOf('open') + 2)), '<Bullet>one\n<Bullet>two\n<Number>three\n<Task>open\n<Task>done\n\n<Task>numbered');
  assert.equal(shown(state, at(state, doc.indexOf('[ ] open') + 1)), '<Bullet>one\n<Bullet>two\n<Number>three\n- [ ] open\n<Task>done\n\n<Task>numbered');
  const tasks = list(inline(state, whole(state), null)).filter(item => item.kind === 'TaskWidget');
  assert.deepEqual(tasks.map(item => item.widget.checked), [false, true, false]);
  // Each box knows where its state character is.
  for (const task of tasks) assert.match(state.sliceDoc(task.from + task.widget.offset - 1, task.from + task.widget.offset + 2), /^\[[ x]\]$/);
  assert.ok(classes(state, 'mark:cm-lp-task-done').some(item => state.sliceDoc(item.from, item.to) === 'done'));
});

test('a checkbox click changes exactly its one character', () => {
  const doc = 'Intro\n\n- [ ] first\n- [x] second\n- [X] third\n', state = make(doc);
  const tasks = list(inline(state, whole(state), null)).filter(item => item.kind === 'TaskWidget');
  let current = state;
  const dispatched = [];
  const view = {get state() {
    return current;
  }, dispatch(spec) {
    dispatched.push(spec);
    current = current.update(spec).state;
  }};
  for (const task of tasks) {
    const before = current.doc.toString(), pos = task.from + task.widget.offset;
    assert.equal(toggleTaskAt(view, pos), true);
    const after = current.doc.toString();
    assert.equal(after.length, before.length);
    const differing = [...after].map((ch, i) => (ch === before[i] ? -1 : i)).filter(i => i >= 0);
    assert.deepEqual(differing, [pos]);
  }
  assert.equal(current.doc.toString(), 'Intro\n\n- [x] first\n- [ ] second\n- [ ] third\n');
  assert.deepEqual(dispatched.map(spec => spec.changes), [{from: 10, to: 11, insert: 'x'}, {from: 22, to: 23, insert: ' '}, {from: 35, to: 36, insert: ' '}]);
  // Anything but a task's state character is refused.
  assert.equal(taskToggle(current, 9), null);
  assert.equal(taskToggle(current, 0), null);
  // Read-only notes do not change.
  const readOnly = make(doc, null, [EditorState.readOnly.of(true)]);
  assert.equal(toggleTaskAt({state: readOnly, dispatch: () => assert.fail('dispatched')}, 10), false);
});

test('Mod-Enter toggles the task on the caret line', () => {
  let state = make('- [ ] a\n- b\n', 6);
  const view = {get state() {
    return state;
  }, dispatch(spec) {
    state = state.update(spec).state;
  }};
  assert.equal(toggleTask(view), true);
  assert.equal(state.doc.toString(), '- [x] a\n- b\n');
  state = state.update({selection: {anchor: 10}}).state;
  assert.equal(toggleTask(view), false);
  assert.equal(state.doc.toString(), '- [x] a\n- b\n');
});

test('block quotes hide their markers off the caret line and keep their depth', () => {
  const state = make('> quote\n> > nested\nlazy\n\nafter');
  assert.equal(shown(state), 'quote\nnested\nlazy\n\nafter');
  assert.equal(shown(state, at(state, 10)), 'quote\n> > nested\nlazy\n\nafter');
  const quotes = classes(state, 'line:cm-lp-quote');
  assert.deepEqual(quotes.map(item => item.from), [0, 8, 19]);
  assert.deepEqual(quotes.map(item => item.attributes.style), ['--lp-quote-depth: 1', '--lp-quote-depth: 2', '--lp-quote-depth: 2']);
});

test('fenced and indented code blocks are styled, with fences hidden outside them', () => {
  const doc = 'Before\n\n```js\nlet a = "**x**";\n```\n\n    indented *code*\n\n~~~\nplain\n~~~';
  const state = make(doc);
  assert.equal(shown(state), 'Before\n\n<Language>\nlet a = "**x**";\n\n\n    indented *code*\n\n<Language>\nplain\n');
  const languages = list(inline(state, whole(state), null)).filter(item => item.kind === 'LanguageWidget').map(item => item.widget.language);
  assert.deepEqual(languages, ['js', '']);
  const lines = classes(state, 'line:').map(item => [state.doc.lineAt(item.from).number, item.kind]);
  assert.deepEqual(lines, [
    [3, 'line:cm-lp-codeblock cm-lp-codeblock-first'], [4, 'line:cm-lp-codeblock'], [5, 'line:cm-lp-codeblock cm-lp-codeblock-last'],
    [7, 'line:cm-lp-codeblock cm-lp-codeblock-indented'],
    [9, 'line:cm-lp-codeblock cm-lp-codeblock-first'], [10, 'line:cm-lp-codeblock'], [11, 'line:cm-lp-codeblock cm-lp-codeblock-last'],
  ]);
  // Inside the block, the fences show and nothing inside is hidden.
  assert.equal(shown(state, at(state, doc.indexOf('let'))), 'Before\n\n```js\nlet a = "**x**";\n```\n\n    indented *code*\n\n<Language>\nplain\n');
});

test('tables are a widget unless the selection is in them', () => {
  const doc = 'Text\n\n| Left | Centre | Right |\n|:-----|:------:|------:|\n| *a* | `b\\|c` | [l](http://x.example) |\n|  | only |\n| 1 | 2 | 3 | 4 |\n\nAfter';
  const state = make(doc), start = doc.indexOf('| Left'), end = doc.indexOf('\n\nAfter');
  const blocks = list(block(state, whole(state), null));
  assert.equal(blocks.length, 1);
  assert.deepEqual([blocks[0].from, blocks[0].to, blocks[0].kind, blocks[0].block], [start, end, 'TableWidget', true]);
  const model = blocks[0].widget.model;
  assert.deepEqual(model.align, ['left', 'center', 'right']);
  assert.deepEqual(model.header.map(cell => cell.parts), [['Left'], ['Centre'], ['Right']]);
  assert.deepEqual(model.rows.map(row => row.map(cell => cell.parts)), [
    [[{tag: 'em', children: ['a']}], [{tag: 'code', text: 'b|c'}], [{tag: 'a', text: 'l'}]],
    [[], ['only'], []],
    [['1'], ['2'], ['3']],
  ]);
  // A cell's offset is where its text starts, so a click puts the caret there.
  for (const cell of [...model.header, model.rows[0][2], model.rows[1][1]]) {
    const text = state.sliceDoc(start + cell.offset, start + cell.offset + 4);
    assert.match(text, /^(Left|Cent|Righ|\[l\]\(|only)/);
  }
  assert.equal(shown(state), 'Text\n\n<Table>\n\nAfter');
  // The caret anywhere on its lines, or a selection into it, shows the source.
  for (const pos of [start, start + 30, end]) assert.equal(list(block(state, whole(state), at(state, pos))).length, 0, 'caret at ' + pos);
  assert.equal(list(block(state, whole(state), [EditorSelection.range(0, start + 1)])).length, 0);
  assert.equal(list(block(state, whole(state), at(state, end + 3))).length, 1);
  // The model is the tree's: the same through tableModel directly.
  let table;
  syntaxTree(state).iterate({enter: node => {
    if (node.name === 'Table') table = node.node;
  }});
  assert.deepEqual(tableModel(state.doc, table), model);
});

test('rules and images render; unsafe image sources do not load', () => {
  const svg = 'data:image/svg+xml;base64,' + Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"/>').toString('base64');
  const doc = `Para\n\n---\n\n![dot](${svg}) and ![bad](javascript:alert(1)) and ![local](media/a.png)`;
  const state = make(doc);
  assert.equal(shown(state), 'Para\n\n<Rule>\n\n<Image> and <Image> and <Image>');
  // Only notebook images load, through the app's resolver; web and data:
  // images show their alt text instead.
  const images = list(inline(state, whole(state), null)).filter(item => item.kind === 'ImageWidget').map(item => item.widget);
  assert.deepEqual(images.map(image => [image.url, image.alt]), [[null, 'dot'], [null, 'bad'], [null, 'local']]);
  const resolved = list(inlineDecorations(state, whole(state), null, {tree: tree(state), resolveImage: path => '/files/' + path}))
    .filter(item => item.kind === 'ImageWidget').map(item => item.widget.url);
  assert.deepEqual(resolved, [null, null, '/files/media/a.png']);
  assert.equal(imageURL('media/a.png', path => '/files/' + path), '/files/media/a.png');
  assert.equal(imageURL('<media/a b.png>', path => '/files/' + path), '/files/media/a b.png');
  for (const source of ['https://example.com/a.png', '//example.com/a.png', svg, 'vbscript:x']) assert.equal(imageURL(source, path => '/files/' + path), null, source);
  // At the caret the source shows, with the image after it.
  const near = list(inline(state, whole(state), at(state, doc.indexOf('![dot]') + 2)));
  assert.ok(near.some(item => item.kind === 'ImageWidget' && item.from === item.to && item.from === doc.indexOf(') and ![bad]') + 1));
  assert.equal(shown(state, at(state, 6)), 'Para\n\n---\n\n<Image> and <Image> and <Image>');
});

test('front matter is collapsed, never read as a heading, and shows when edited', () => {
  const doc = '---\ntitle: A note\ntags: [a, b]\n---\n\n# Heading\n\nText';
  const state = make(doc), end = doc.indexOf('\n\n# Heading');
  assert.deepEqual(frontMatter(state.doc), {from: 0, to: end});
  const blocks = list(block(state, whole(state), null));
  assert.deepEqual(blocks.map(item => [item.from, item.to, item.kind, item.block]), [[0, end, 'FrontMatterWidget', true]]);
  assert.deepEqual(blocks[0].widget.fields, [['title', 'A note'], ['tags', '[a, b]']]);
  // No heading or rule decorations inside it.
  assert.deepEqual(classes(state, 'line:').map(item => state.doc.lineAt(item.from).number), [6]);
  // With the caret inside, its source shows, styled.
  assert.equal(list(block(state, whole(state), at(state, 8))).length, 0);
  const styled = list(inline(state, whole(state), at(state, 8))).filter(item => item.kind === 'line:cm-lp-frontmatter');
  assert.equal(styled.length, 4);
  // As the server reads it: a closing line is required, CRLF is allowed.
  assert.equal(frontMatter(make('---\ntitle: open\n\nText').doc), null);
  assert.deepEqual(frontMatter(make('---\r\na: 1\r\n---\r\nText').doc), {from: 0, to: 15});
  assert.equal(frontMatter(make('Text\n---\na: 1\n---').doc), null);
});

// Short random strings from an alphabet, by xorshift32.
function randomStrings(alphabet, count, longest, seed = 0x9e3779b9) {
  const next = () => {
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return (seed >>> 0) / 4294967296;
  };
  return Array.from({length: count}, () => Array.from({length: Math.floor(next() * (longest + 1))}, () => alphabet[Math.floor(next() * alphabet.length)]).join(''));
}
const timed = run => {
  const started = performance.now(), result = run();
  return {result, ms: performance.now() - started};
};

// The Properties summary once read each line with a regular expression whose
// cost grew with the square of a run of spaces (0.9 s for 60,000, at every
// focus change); its code now walks the line once and must give what that
// gave.
test('front matter fields are what the regular expression gave, in linear time', () => {
  const regexFields = lines => lines.flatMap(line => {
    const match = /^([^\s:#][^:]*):\s*(.*?)\s*$/.exec(line.replace(/\r$/, ''));
    return match ? [[match[1], match[2].replace(/^(["'])(.*)\1$/, '$2')]] : [];
  });
  const alphabet = [':', ':', ' ', ' ', '\t', '#', '"', "'", 'a', 'b', '\r', '\v', '\f', '\u00a0', '\u2028', '\u2029', '\ufeff'];
  const lines = randomStrings(alphabet, 40000, 12);
  for (let i = 0; i < lines.length; i += 2000) {
    const batch = lines.slice(i, i + 2000), state = make('---\n' + batch.join('\n') + '\n---\n\nText');
    const [summary] = list(block(state, whole(state), null));
    assert.equal(summary.kind, 'FrontMatterWidget');
    assert.deepEqual(summary.widget.fields, regexFields(batch));
  }
  const state = make('---\na: x' + ' '.repeat(60000) + 'y\n---\n\nText');
  const built = timed(() => list(block(state, whole(state), at(state, state.doc.length))));
  assert.deepEqual(built.result[0].widget.fields, [['a', 'x' + ' '.repeat(60000) + 'y']]);
  assert.ok(built.ms < 50, `the front matter widget took ${built.ms.toFixed(1)} ms`);
});

// Lezer's HardBreak parser cost the square of a run of spaces that does not
// end its line, in one parse step no time limit interrupts: such a note froze
// the tab in every editing mode (minutes for 200,000 spaces).
test('a long run of spaces in a line parses and draws at once', () => {
  const run = ' '.repeat(200000);
  const notes = [['A line with' + run + 'padding.', 'Paragraph'], ['# Title' + run + 'x', 'ATXHeading1'], ['- a' + run + 'x', 'BulletList'], ['> *em*' + run + '**strong**', 'Blockquote']];
  for (const [doc, name] of notes) {
    const built = timed(() => {
      const state = make(doc, 0, [livePreview()]);
      return {state, inline: list(inline(state, whole(state), null)), block: list(block(state, whole(state), null))};
    });
    assert.ok(built.ms < 500, `${name}: ${built.ms.toFixed(1)} ms`);
    const {state} = built.result;
    assert.equal(tree(state).length, doc.length);
    assert.equal(tree(state).topNode.firstChild.name, name);
  }
  // What follows the run is still read: its emphasis is drawn.
  const quoted = make(notes[3][0]);
  assert.equal(shown(quoted), 'em' + run + 'strong');
});

test('HTML blocks and footnotes are styled source', () => {
  const doc = '<div class="x">\n**not hidden**\n</div>\n\nText[^1] and <kbd>Ctrl</kbd>.\n\n[^1]: The *note*.';
  const state = make(doc);
  assert.equal(shown(state), '<div class="x">\n**not hidden**\n</div>\n\nText[^1] and <kbd>Ctrl</kbd>.\n\n[^1]: The note.');
  const lines = classes(state, 'line:').map(item => [state.doc.lineAt(item.from).number, item.kind]);
  assert.deepEqual(lines, [[1, 'line:cm-lp-html'], [2, 'line:cm-lp-html'], [3, 'line:cm-lp-html'], [7, 'line:cm-lp-footnote']]);
  const refs = classes(state, 'mark:cm-lp-footnote-ref').map(item => state.sliceDoc(item.from, item.to));
  assert.deepEqual(refs, ['[^1]', '[^1]:']);
  assert.equal(classes(state, 'mark:cm-lp-html-inline').length, 2);
});

test('live preview switches on and off in a compartment without touching the text', () => {
  const mode = new Compartment(), doc = '# Title\n\n| a |\n|---|\n| 1 |\n';
  let state = make(doc, null, [mode.of([])]);
  for (const on of [true, false, true, true]) {
    // A reconfiguration together with an edit and a selection.
    state = state.update({effects: mode.reconfigure(on ? livePreview() : []), changes: {from: 0, insert: 'x'}, selection: {anchor: 1}}).state;
  }
  assert.equal(state.doc.toString(), 'xxxx' + doc);
});

test('only the given range is decorated', () => {
  const doc = Array.from({length: 400}, (_, i) => `Line ${i} with **bold** and [a link](http://x.example/${i}).`).join('\n\n');
  const state = make(doc), from = state.doc.line(401).from, to = state.doc.line(421).to;
  const items = list(inline(state, [{from, to}], null));
  assert.ok(items.length > 50);
  for (const item of items) assert.ok(item.from >= from && item.to <= to, `${item.from}-${item.to} outside ${from}-${to}`);
});

test('hidden text never spans a line break and the extension never edits', () => {
  let seed = 7;
  const random = n => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    return seed % n;
  };
  const pieces = ['# ', '**', '*', '~~', '`', '[', ']', '(', ')', '[[', ']]', '|', '- ', '- [ ] ', '> ', '\n', '\n\n', '```', '---', '![', 'http://x.y', 'word ', '\\', '<b>', '[^1]', ': '];
  for (let round = 0; round < 300; round++) {
    const doc = Array.from({length: 5 + random(40)}, () => pieces[random(pieces.length)]).join('');
    let state = make(doc, null, [livePreview()]);
    for (let step = 0; step < 5; step++) {
      const from = random(state.doc.length + 1), to = Math.min(state.doc.length, from + random(4));
      const insert = pieces[random(pieces.length)], expected = state.sliceDoc(0, from) + insert + state.sliceDoc(to);
      const anchor = random(expected.length + 1);
      state = state.update({changes: {from, to, insert}, selection: {anchor}}).state;
      assert.equal(state.doc.toString(), expected);
      ensureSyntaxTree(state, state.doc.length, 5000);
      const selection = [state.selection.main];
      for (const item of list(inline(state, whole(state), selection))) {
        if (item.kind === 'hidden' || (item.widget && item.to > item.from)) {
          assert.equal(state.doc.lineAt(item.from).number, state.doc.lineAt(item.to).number, JSON.stringify({doc: state.doc.toString(), item: [item.from, item.to]}));
        }
      }
      for (const item of list(block(state, whole(state), selection))) {
        if (item.from === item.to) continue;
        assert.equal(state.doc.lineAt(item.from).from, item.from);
        assert.equal(state.doc.lineAt(item.to).to, item.to);
      }
    }
  }
});

test('reference links resolve through the note’s definitions, which follow its edits', () => {
  const doc = 'See [Guide], [the guide][], [text][GUIDE] and [missing][nope].\n\n[guide]: http://g.example "Title"\n[other]: <http://o.example/a b>';
  let state = make(doc);
  assert.equal(shown(state), 'See Guide, [the guide][], text and [missing][nope].\n\n[guide]: http://g.example "Title"\n[other]: <http://o.example/a b>');
  const hrefs = () => classes(state, 'mark:cm-lp-link').map(item => [state.sliceDoc(item.from, item.to), item.attributes['data-lp-href']]);
  assert.deepEqual(hrefs(), [['Guide', 'http://g.example'], ['text', 'http://g.example']]);
  // [the guide][] looks up "the guide", which is not defined until it is.
  const extension = [livePreview()];
  state = make(doc, null, extension);
  const definitions = () => [...linkDefinitions(state).list.map(item => [item.label, item.href])];
  assert.deepEqual(definitions(), [['guide', 'http://g.example'], ['other', 'http://o.example/a b']]);
  state = state.update({changes: {from: state.doc.length, insert: '\n[The  Guide]: http://t.example\n[nope]: http://n.example'}}).state;
  assert.deepEqual(definitions(), [['guide', 'http://g.example'], ['other', 'http://o.example/a b'], ['the guide', 'http://t.example'], ['nope', 'http://n.example']]);
  assert.equal(linkDefinitions(state).get('THE guide'), 'http://t.example');
  // Editing a definition's line rescans it; the first definition wins.
  const at = state.doc.toString().indexOf('http://g.example');
  state = state.update({changes: {from: at, to: at + 16, insert: 'http://changed.example'}}).state;
  assert.equal(linkDefinitions(state).get('guide'), 'http://changed.example');
  state = state.update({changes: {from: 0, insert: '[guide]: http://first.example\n\n'}}).state;
  assert.equal(linkDefinitions(state).get('guide'), 'http://first.example');
  // Joining a definition's line to the one before ends it.
  const other = state.doc.toString().indexOf('\n[other]');
  state = state.update({changes: {from: other, to: other + 1, insert: ' '}}).state;
  assert.equal(linkDefinitions(state).get('other'), undefined);
  assert.deepEqual(definitions(), linkDefinitions(make(state.doc.toString())).list.map(item => [item.label, item.href]));
});

test('list items hang: wrapped lines start under the item’s text at every depth', () => {
  const doc = '- one\n  - nested\n    1. deep\n> - quoted\n\n10. ten\n- [ ] task';
  const state = make(doc);
  const hangs = classes(state, 'line:cm-lp-list').map(item => [state.doc.lineAt(item.from).number, item.attributes.style]);
  assert.deepEqual(hangs, [
    [1, '--lp-hang: calc(0 * var(--lp-list-step) + 1.4em)'],
    [2, '--lp-hang: calc(1 * var(--lp-list-step) + 1.4em)'],
    [3, '--lp-hang: calc(2 * var(--lp-list-step) + 2ch + 0.5em)'],
    [4, '--lp-hang: calc(0 * var(--lp-list-step) + 1.4em)'],
    [6, '--lp-hang: calc(0 * var(--lp-list-step) + 3ch + 0.5em)'],
    [7, '--lp-hang: calc(0 * var(--lp-list-step) + 1.6em)'],
  ]);
  // In a quote, the prefix box starts after the quote marker and its space.
  const prefixes = list(inline(state, whole(state), null)).filter(item => item.widget && item.to > item.from).map(item => state.sliceDoc(item.from, item.to));
  assert.deepEqual(prefixes, ['- ', '  - ', '    1. ', '- ', '10. ', '- [ ] ']);
  assert.equal(shown(state), '<Bullet>one\n<Bullet>nested\n<Number>deep\n<Bullet>quoted\n\n<Number>ten\n<Task>task');
});

test('front matter is one node in every mode, so Source does not read a rule and a heading', () => {
  const doc = '---\ntitle: A\n---\n\nText\n\n---\n\nSetext\n---', state = make(doc);
  const names = [];
  tree(state).iterate({enter: node => {
    if (node.name !== 'Document') names.push(node.name);
    return node.name === 'Document';
  }});
  assert.deepEqual(names, ['FrontMatter', 'Paragraph', 'HorizontalRule', 'SetextHeading2']);
  // Unclosed, or not at the start, it is what CommonMark makes of it.
  for (const text of ['---\ntitle: A\n\nText', 'Text\n---\na: 1\n---', ' ---\na: 1\n---']) {
    const top = [];
    tree(make(text)).iterate({enter: node => {
      if (node.name !== 'Document') top.push(node.name);
      return node.name === 'Document';
    }});
    assert.ok(!top.includes('FrontMatter'), text);
  }
  // CRLF front matter, as the server reads it.
  const crlf = make('---\r\na: 1\r\n---\r\nText');
  assert.equal(tree(crlf).topNode.firstChild.name, 'FrontMatter');
  assert.deepEqual(frontMatter(crlf.doc), {from: 0, to: 15});
});

test('a lone rule is a block widget and a video link gets a player after its paragraph', () => {
  const doc = 'Text\n\n---\n\n- ***\n\n[Video](https://youtu.be/x "video")\n\n[Not](https://youtu.be/x "video") alone';
  const plain = make(doc), withVideo = make(doc, null, [livePreview({video: () => null})]);
  const blocks = state => list(block(state, whole(state), null)).map(item => [state.sliceDoc(item.from, item.to), item.kind, item.block, item.widget?.href]);
  assert.deepEqual(blocks(plain), [['---', 'RuleWidget', true, undefined]]);
  const end = doc.indexOf('\n\n[Not]');
  assert.deepEqual(blocks(withVideo), [['---', 'RuleWidget', true, undefined], ['', 'VideoWidget', true, 'https://youtu.be/x']]);
  assert.equal(list(block(withVideo, whole(withVideo), null))[1].from, end);
  // The rule in a list item stays inline; the caret on a rule's line shows it.
  assert.equal(shown(plain), 'Text\n\n<Rule>\n\n<Bullet><InlineRule>\n\n[Video](https://youtu.be/x "video")\n\n[Not](https://youtu.be/x "video") alone'
    .replace('[Video](https://youtu.be/x "video")', 'Video').replace('[Not](https://youtu.be/x "video")', 'Not'));
  assert.equal(list(block(plain, whole(plain), at(plain, 7))).length, 0);
});

test('a private draft holds each outside video link until the user loads that link; a saved note or a notebook video plays at once', () => {
  const youtube = 'https://youtu.be/M7lc1UVf-VE', vimeo = 'https://vimeo.com/123456789', clip = '/assets/clip.mp4';
  const hosts = {[youtube]: 'www.youtube-nocookie.com', [vimeo]: 'player.vimeo.com'}, host = href => hosts[href] ?? null;
  const first = `[Video](${youtube} "video")`, again = `[Again](${youtube} "video")`, clipLink = `[Clip](${clip} "video")`;
  const doc = `${first}\n\n${clipLink}\n\nText\n\n${again}\n\n[Vimeo](${vimeo} "video")`;
  // Where a link ends, which is where its player goes.
  const end = (link, text = doc) => text.indexOf(link) + link.length;
  const open = privateDraft => make(doc, null, [livePreview({video: () => null}), privateVideos({privateDraft, host})]);
  const widgets = state => list(block(state, whole(state), null)).map(item => [item.widget.href, item.widget.host]);
  // What the editor shows: the field's own decorations, kept up to date.
  const field = state => state.facet(EditorView.decorations).find(value => typeof value !== 'function');
  const drawn = state => list(field(state)).map(item => [item.widget.href, item.widget.host]);
  const saved = open(false), draft = open(true);
  assert.deepEqual(widgets(saved), [[youtube, null], [clip, null], [youtube, null], [vimeo, null]]);
  assert.equal(heldVideo(saved, youtube, end(first)), null);
  const held = [[youtube, 'www.youtube-nocookie.com'], [clip, null], [youtube, 'www.youtube-nocookie.com'], [vimeo, 'player.vimeo.com']];
  assert.deepEqual(widgets(draft), held);
  assert.deepEqual(drawn(draft), held);
  assert.equal(heldVideo(draft, youtube, end(first)), 'www.youtube-nocookie.com');
  assert.equal(heldVideo(draft, clip, end(clipLink)), null);
  // Loading a video link plays it, and only it, from then on in this editor:
  // another link to the same video stays held.
  const loaded = draft.update({effects: loadVideo.of({at: end(first), href: youtube})}).state;
  assert.equal(heldVideo(loaded, youtube, end(first)), null);
  assert.equal(heldVideo(loaded, youtube, end(again)), 'www.youtube-nocookie.com');
  const loadedFirst = [[youtube, null], [clip, null], [youtube, 'www.youtube-nocookie.com'], [vimeo, 'player.vimeo.com']];
  assert.deepEqual(drawn(loaded), loadedFirst);
  assert.deepEqual(drawn(loaded.update({changes: {from: doc.length, insert: '\n\nTyped.'}}).state), loadedFirst);
  // The loaded link moves with edits before it, and an edit of its label
  // keeps it loaded.
  const moved = loaded.update({changes: {from: 0, insert: '# Title\n\n'}}).state;
  assert.deepEqual(drawn(moved), loadedFirst);
  assert.equal(heldVideo(moved, youtube, end(first, moved.doc.toString())), null);
  assert.deepEqual(drawn(loaded.update({changes: {from: 1, insert: 'My '}}).state), loadedFirst);
  // Another address there is another video; deleting the link's end and
  // typing it again makes a link the user has not loaded.
  const other = 'https://youtu.be/aaaaaaaaaaa', at = doc.indexOf(youtube);
  hosts[other] = 'www.youtube-nocookie.com';
  assert.deepEqual(drawn(loaded.update({changes: {from: at, to: at + youtube.length, insert: other}}).state)[0], [other, 'www.youtube-nocookie.com']);
  const broken = loaded.update({changes: {from: end(first) - 1, to: end(first)}}).state;
  assert.deepEqual(drawn(broken.update({changes: {from: end(first) - 1, insert: ')'}}).state), held);
  // Deleting the text between two links to the same video joins the later
  // link's end to the earlier one: neither is loaded unless it was.
  const second = draft.update({effects: loadVideo.of({at: end(again), href: youtube})}).state;
  assert.deepEqual(drawn(second), [[youtube, 'www.youtube-nocookie.com'], [clip, null], [youtube, null], [vimeo, 'player.vimeo.com']]);
  assert.deepEqual(drawn(second.update({changes: {from: end(first), to: end(again)}}).state), [[youtube, 'www.youtube-nocookie.com'], [vimeo, 'player.vimeo.com']]);
  // Holding or loading changes no text.
  assert.equal(loaded.doc.toString(), doc);
  // A saved draft plays every video; a later private draft holds again only
  // those the user did not load.
  assert.deepEqual(drawn(draft.update({effects: setPrivateDraft.of(false)}).state), widgets(saved));
  assert.deepEqual(drawn(draft.update({effects: setPrivateDraft.of(false)}).state.update({effects: setPrivateDraft.of(true)}).state), held);
  assert.deepEqual(drawn(loaded.update({effects: setPrivateDraft.of(false)}).state.update({effects: setPrivateDraft.of(true)}).state), loadedFirst);
  // An editor without the private-draft state shows saved notes only.
  assert.deepEqual(widgets(make(doc, null, [livePreview({video: () => null})])), widgets(saved));
});

test('table columns of numbers align right, as in the reader', () => {
  const state = make('| Name | Count | Mixed |\n|---|---|---|\n| a | 1,200 | 3 |\n| b | -4.5% | x |\n| c |  | 7 |');
  let table;
  tree(state).iterate({enter: node => {
    if (node.name === 'Table') table = node.node;
  }});
  assert.deepEqual(numericColumns(tableModel(state.doc, table)), [false, true, false]);
});

test('the block field rebuilds only what an edit or the selection touched, and matches a full build', () => {
  let seed = 3;
  const random = n => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    return seed % n;
  };
  const pieces = ['| a | b |\n|---|---|\n| 1 | 2 |\n', '---\n', 'text\n', '\n', '> ', '- ', '[V](https://youtu.be/x "video")\n', '```\n', '| 3 | 4 |\n', 'word ', '|', '\n\n'];
  const describe = set => {
    const out = [];
    set.between(0, 1e9, (from, to, value) => {
      out.push([from, to, value.widget?.constructor.name, value.widget?.source ?? value.widget?.href ?? ''].join(':'));
    });
    return out.sort();
  };
  for (let round = 0; round < 150; round++) {
    const front = random(3) === 0 ? '---\na: 1\n---\n' : '';
    const doc = front + Array.from({length: 4 + random(16)}, () => pieces[random(pieces.length)]).join('');
    let state = make(doc, null, [livePreview({video: () => null})]);
    state = state.update({effects: setFocus.of(true)}).state;
    for (let step = 0; step < 12; step++) {
      const spec = {};
      if (random(3)) {
        const from = random(state.doc.length + 1), to = Math.min(state.doc.length, from + random(6));
        spec.changes = {from, to, insert: pieces[random(pieces.length)]};
      }
      const length = state.doc.length + (spec.changes ? spec.changes.insert.length - (spec.changes.to - spec.changes.from) : 0);
      if (random(2)) spec.selection = {anchor: random(length + 1), head: random(length + 1)};
      state = state.update(spec).state;
      ensureSyntaxTree(state, state.doc.length, 5000);
      const field = state.facet(EditorView.decorations).find(value => typeof value !== 'function');
      const full = blockDecorations(state, whole(state), state.selection.ranges, {tree: syntaxTree(state)});
      assert.deepEqual(describe(field), describe(full), JSON.stringify({doc: state.doc.toString(), selection: state.selection.main}));
    }
  }
});

test('the syntax helpers name node roles, find top-level blocks and parse a long note in idle slices', async () => {
  const {ensureTreeIdle, nodeRole, topBlockAt, topBlocks} = await import('../../../frontend/markdown-syntax.mjs');
  assert.deepEqual(['HeaderMark', 'LinkMark', 'TaskMarker', 'URL', 'HTMLBlock', 'FrontMatter', 'Paragraph', 'Emphasis'].map(nodeRole),
    ['marker', 'marker', 'marker', 'hidden', 'hidden', 'hidden', 'visible', 'visible']);
  const doc = '---\na: 1\n---\n# Title\n\n> quote\n> more\n\n| a |\n|---|\n| 1 |\n', state = make(doc);
  assert.deepEqual(topBlocks(tree(state), 0, doc.length).map(block => block.name), ['FrontMatter', 'ATXHeading1', 'Blockquote', 'Table']);
  assert.deepEqual(topBlockAt(tree(state), doc.indexOf('more')), {name: 'Blockquote', from: doc.indexOf('> quote'), to: doc.indexOf('\n\n| a')});
  assert.equal(topBlockAt(tree(state), doc.indexOf('\n\n> quote') + 1), null);
  // A note far longer than CodeMirror parses up front is parsed whole, a
  // slice at a time, without the caller waiting on one long task.
  const long = Array.from({length: 4000}, (_, i) => `Paragraph ${i} with *emphasis* and a [link](http://x.example/${i}).`).join('\n\n');
  const longState = EditorState.create({doc: long, extensions: [EditorState.lineSeparator.of('\n'), livePreviewMarkdown()]});
  assert.ok(syntaxTree(longState).length < long.length);
  const full = await ensureTreeIdle({state: longState, dom: {isConnected: true}}, long.length, {slice: 8});
  assert.equal(full.length, long.length);
  // A view that is gone gets nothing.
  const fresh = EditorState.create({doc: long, extensions: [EditorState.lineSeparator.of('\n'), livePreviewMarkdown()]});
  assert.equal(await ensureTreeIdle({state: fresh, dom: {isConnected: false}}, long.length), null);
});

// What a deleting key does in Visual with the caret at pos and the editor
// focused: the text after it and the caret, or null when the browser deletes
// as usual. by is 'char', 'group' (a word) or 'line'.
const press = (doc, pos, forward, by = 'char', extensions = []) => {
  const state = make(doc, pos, extensions), selection = at(state, pos);
  const spec = guardedDeletion(state, [block(state, whole(state), selection), inline(state, whole(state), selection)], forward, by);
  if (!spec) return null;
  const next = state.update(spec).state;
  return {text: next.doc.toString(), caret: next.selection.main.head};
};
// doc without the one character at `at`.
const without = (doc, at) => doc.slice(0, at) + doc.slice(at + 1);

test('Backspace and Delete next to a table, rule or front matter delete only the line break', () => {
  // A table: Backspace on the line after it (none blank, one blank between)
  // and Delete on the line before it (none blank, one blank between).
  const table = '| a | b |\n|---|---|\n| 1 | 2 |';
  for (const [doc, caret, forward] of [
    ['# T\n\n' + table + '\n# After\n', '# After', false],
    ['# T\n\n' + table + '\n\nAfter table.\n', '\nAfter table.', false],
    ['Before.\n' + table + '\n', '\n|', true],
    ['Before.\n\n' + table + '\n', '\n|', true],
  ]) {
    const pos = doc.indexOf(caret), line = forward ? pos : pos - 1;
    for (const by of ['char', 'group', 'line']) {
      assert.deepEqual(press(doc, pos, forward, by), {text: without(doc, line), caret: line}, JSON.stringify({doc, by}));
    }
  }
  // A lone rule, the same four ways. Joining a line to a rule is an edit of
  // the text the user sees as the caret then shows the rule's source.
  for (const [doc, caret, forward] of [
    ['Above.\n\n---\nBelow rule.\n', 'Below rule.', false],
    ['Above.\n\n---\n\nBelow.\n', '\nBelow.', false],
    ['# Title\n---\nBelow.\n', '\n---', true],
    ['Above.\n\n---\n\nBelow.\n', '\n---', true],
  ]) {
    const pos = doc.indexOf(caret), line = forward ? pos : pos - 1;
    for (const by of ['char', 'group', 'line']) {
      assert.deepEqual(press(doc, pos, forward, by), {text: without(doc, line), caret: line}, JSON.stringify({doc, by}));
    }
  }
  // Front matter: Backspace on the first line after it moves the caret into
  // it and changes nothing; on an empty line it deletes the line break, and
  // the front matter stays.
  const front = '---\ntitle: Keep me\nstatus: draft\n---';
  for (const by of ['char', 'group', 'line']) {
    assert.deepEqual(press(front + '\nFirst line.\n', front.length + 1, false, by), {text: front + '\nFirst line.\n', caret: front.length});
    const blank = front + '\n\nFirst line.\n', after = press(blank, front.length + 1, false, by);
    assert.deepEqual(after, {text: front + '\nFirst line.\n', caret: front.length});
    assert.deepEqual(frontMatter(make(after.text).doc), {from: 0, to: front.length});
  }
  // A CRLF note's empty line is a lone CR: Backspace there moves into the
  // front matter too, as deleting the line break would end it.
  const crlf = '---\r\ntitle: x\r\n---\r\n\r\nText\r\n', crlfFront = frontMatter(make(crlf).doc);
  assert.deepEqual(press(crlf, crlfFront.to + 1, false), {text: crlf, caret: crlfFront.to});
  // A video player after its paragraph: the line break around it goes alone.
  const video = '[Video](https://youtu.be/x "video")\nNext\n\n[Video](https://youtu.be/x "video")\n\nLast\n', player = [livePreview({video: () => null})];
  const lineEnd = video.indexOf('\n\nLast');
  assert.deepEqual(press(video, lineEnd + 1, false, 'char', player), {text: without(video, lineEnd), caret: lineEnd});
  assert.deepEqual(press(video, lineEnd, true, 'char', player), {text: without(video, lineEnd), caret: lineEnd});
});

test('a word deletion stops at hidden markers, which then show', () => {
  const doc = 'Some **bold** text\n';
  // Backward from before "text": only the space goes, not the hidden "**".
  assert.deepEqual(press(doc, doc.indexOf('text'), false, 'group'), {text: 'Some **bold**text\n', caret: doc.indexOf(' text')});
  // Forward from after "Some": only the space.
  assert.deepEqual(press(doc, 4, true, 'group'), {text: 'Some**bold** text\n', caret: 4});
  // The caret then touches the element, so its markers show.
  const after = make('Some **bold**text\n', 13);
  assert.equal(shown(after, at(after, 13)), 'Some **bold**text\n');
  // A hidden heading mark after a line break, and a link's hidden address
  // before the caret, are not deleted either.
  assert.deepEqual(press('Para.\n## Heading\n', 5, true), {text: 'Para.## Heading\n', caret: 5});
  const link = 'A [link](http://x.example) here\n';
  assert.deepEqual(press(link, link.indexOf('here'), false, 'group'), {text: 'A [link](http://x.example)here\n', caret: link.indexOf(' here')});
});

test('deleting keys away from hidden text, with a selection, or read-only are left to the browser', () => {
  assert.equal(press('Plain text here\n', 5, false), null);
  assert.equal(press('Plain text here\n', 5, true, 'group'), null);
  assert.equal(press('Some **bold** text and more\n', 22, false, 'group'), null);
  // Inside an element its markers show, so nothing is hidden.
  assert.equal(press('Some **bold** text\n', 9, false), null);
  const table = '| a | b |\n|---|---|\n| 1 | 2 |\n\nAfter.\n';
  const selected = make(table, EditorSelection.single(table.length - 3, table.length - 1));
  assert.equal(guardedDeletion(selected, [block(selected, whole(selected), selected.selection.ranges)], false), null);
  const readOnly = make(table, table.indexOf('\nAfter'), [EditorState.readOnly.of(true)]);
  assert.equal(guardedDeletion(readOnly, [block(readOnly, whole(readOnly), at(readOnly, table.indexOf('\nAfter')))], false), null);
  // Where deletions end, as Source's keys go.
  const state = make('ab cd**ef**\n\nx');
  assert.deepEqual([deletionEnd(state, 5, false, 'group'), deletionEnd(state, 3, true, 'group'), deletionEnd(state, 2, true, 'group')], [3, 5, 5]);
  assert.deepEqual([deletionEnd(state, 12, false), deletionEnd(state, 11, true), deletionEnd(state, 4, false, 'line'), deletionEnd(state, 0, false)], [11, 12, 0, 0]);
});

test('a code block’s fences show while the caret is at the start of the line after it or the end of the line before', () => {
  const doc = 'Before.\n```js\ncode\n```\nAfter fence.\n', state = make(doc);
  const after = doc.indexOf('After'), before = doc.indexOf('\n```js');
  assert.equal(shown(state, at(state, after)), doc);
  assert.equal(shown(state, at(state, before)), doc);
  assert.equal(shown(state, at(state, after + 1)), 'Before.\n<Language>\ncode\n\nAfter fence.\n');
  assert.equal(shown(state, at(state, before - 1)), 'Before.\n<Language>\ncode\n\nAfter fence.\n');
  // So Backspace there joins a line the user sees, and Source does the same.
  assert.equal(press(doc, after, false), null);
});
