import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';
import {ChangeSet, EditorState} from '@codemirror/state';
import {ensureSyntaxTree} from '@codemirror/language';
import {livePreviewMarkdown} from '../../../frontend/live-preview.mjs';
import {
  Budget, HeadingIDs, blockSource, bodyStart, fraction, headingLine, itemAt, keepUnparsed, paneBlocks, reconcile, renderMarkdown, renderPrivateMarkdown, safeURL, toHTML, treeDefinitions,
} from '../../../frontend/rendered-preview.mjs';

// The render corpus: Markdown and the HTML Read shows for it
// (internal/webcore TestRenderCorpusGolden writes the goldens).
const corpus = new URL('../../webcore/testdata/render-corpus/', import.meta.url);
const cases = readdirSync(corpus).filter(name => name.endsWith('.md')).sort();

// HTML as a reader sees it, so two renderers can be compared: entities
// decoded, attributes sorted, whitespace between and at the edges of blocks
// dropped (and collapsed outside <pre>). Two differences are the server's
// sanitizer's and not the reader's concern: it adds rel="nofollow" to links,
// and a private preview names image sources data-fmn-src until the reader
// resolves them.
const blockTags = new Set(['p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'blockquote', 'ul', 'ol', 'li', 'pre', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'br']);
const decode = value => value.replace(/&(#x[0-9a-f]+|#[0-9]+|amp|lt|gt|quot|apos);/gi, (_, name) => {
  if (name[0] === '#') return String.fromCodePoint(name[1].toLowerCase() === 'x' ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10));
  return {amp: '&', lt: '<', gt: '>', quot: '"', apos: "'"}[name.toLowerCase()];
});
function normalize(html) {
  const out = [];
  let pre = 0;
  for (const [, close, tag, attributes, text] of html.matchAll(/<(\/?)([a-zA-Z0-9]+)((?:\s+[^\s=>/]+(?:="[^"]*")?)*)\s*\/?>|([^<]+)/g)) {
    if (text !== undefined) {
      const value = decode(text);
      out.push({text: pre ? value : value.replace(/\s+/g, ' ')});
      continue;
    }
    const name = tag.toLowerCase();
    if (name === 'pre') pre += close ? -1 : 1;
    const attrs = [...attributes.matchAll(/([^\s=]+)(?:="([^"]*)")?/g)]
      .map(([, key, value = '']) => [key === 'data-fmn-src' ? 'src' : key, decode(value)])
      .filter(([key]) => key !== 'rel')
      .sort(([a], [b]) => a.localeCompare(b));
    out.push({tag: (close ? '/' : '') + name + attrs.map(([key, value]) => ` ${key}=${JSON.stringify(value)}`).join(''), block: blockTags.has(name)});
  }
  // Space next to a block boundary is not shown.
  for (let i = 0; i < out.length; i++) {
    if (out[i].text === undefined) continue;
    if (i === 0 || out[i - 1].block) out[i].text = out[i].text.replace(/^ /, '');
    if (i === out.length - 1 || out[i + 1].block) out[i].text = out[i].text.replace(/ $/, '');
  }
  return out.map(part => (part.text !== undefined ? part.text : `<${part.tag}>`)).join('');
}

test('private previews render the corpus as Read renders it', async () => {
  assert.ok(cases.length >= 8);
  for (const name of cases) {
    const markdown = readFileSync(new URL(name, corpus), 'utf8');
    const golden = readFileSync(new URL(name.replace(/\.md$/, '.html'), corpus), 'utf8');
    assert.equal(normalize(await renderPrivateMarkdown(markdown)), normalize(golden), name);
  }
});

test('the corpus normalizer still tells different renderings apart', () => {
  assert.notEqual(normalize('<p>a <em>b</em></p>'), normalize('<p>a b</p>'));
  assert.notEqual(normalize('<pre><code>a  b</code></pre>'), normalize('<pre><code>a b</code></pre>'));
  assert.equal(normalize('<p> a\n b </p>\n<a href="x" rel="nofollow">y</a>'), normalize('<p>a b</p><a href="x">y</a>'));
});

test('private previews stay inert: no scripts, handlers, script URLs or loaded images', async () => {
  const html = await renderPrivateMarkdown([
    '<script>window.bad = 1</script>',
    '<img src=x onerror="bad()"> <b onclick="bad()">x</b>',
    '[a](javascript:alert(1)) [b](JaVaScRiPt:alert(1)) [c](data:text/html,x) [d](vbscript:x) <javascript:alert(1)>',
    '![remote](https://example.invalid/remote.png) ![data](data:image/png;base64,AAAA) ![local](images/a.png)',
    '```view\n<script>alert(1)</script>\n```',
    '[ref]: javascript:alert(1)\n\n[ref] and ![pic][ref]',
    '[e](&#106;avascript:alert(1)) [f](  javascript:alert(1)) [g](JAVA&#x53;CRIPT:x) [h](<javascript:alert(1)>)',
  ].join('\n\n'));
  assert.doesNotMatch(html, /<script|<b\b|\son[a-z]+=|="(?:javascript|vbscript|data):/i);
  // An address Read refuses stays visible as text, never as a link.
  assert.match(html, /<p>a b c d javascript:alert\(1\)<\/p>/);
  assert.match(html, /<p>e f g h<\/p>/);
  assert.doesNotMatch(html, /\ssrc=/);
  assert.match(html, /<img data-fmn-src="https:\/\/example.invalid\/remote.png" alt="remote">/);
  assert.match(html, /<img data-fmn-src="images\/a.png" alt="local">/);
  assert.match(html, /<code class="language-view">&lt;script&gt;alert\(1\)&lt;\/script&gt;\n<\/code>/);
});

test('only http, https, mailto and relative addresses are kept, encoded as goldmark writes them', () => {
  assert.equal(safeURL('https://example.com/?a=1&b=2'), 'https://example.com/?a=1&b=2');
  assert.equal(safeURL('a b.md'), 'a%20b.md');
  assert.equal(safeURL('naïve.md'), 'na%C3%AFve.md');
  assert.equal(safeURL('mailto:x@example.com'), 'mailto:x@example.com');
  assert.equal(safeURL('#h-x'), '#h-x');
  assert.equal(safeURL('notes/a:b.md'), 'notes/a:b.md');
  // A % that does not start a hex escape is encoded, whatever follows it.
  assert.equal(safeURL('a%zz.md'), 'a%25zz.md');
  assert.equal(safeURL('b%41%2f.md'), 'b%41%2f.md');
  assert.equal(safeURL('100%'), '100%25');
  for (const unsafe of ['javascript:x', 'JAVASCRIPT:x', 'data:text/html,x', 'vbscript:x', 'file:///etc/passwd', 'ftp://x', '']) assert.equal(safeURL(unsafe), null, unsafe);
});

test('references resolve across blocks to the note first definition', async () => {
  const note = '[early] and [Later][] and [text][quoted]\n\n> [quoted]: https://example.com/q\n\n[later]: https://example.com/later "T"\n[early]: https://example.com/early\n[early]: https://example.com/ignored\n\n# [early]\n';
  const html = await renderPrivateMarkdown(note);
  assert.match(html, /<a href="https:\/\/example.com\/early">early<\/a>/);
  assert.match(html, /<a href="https:\/\/example.com\/later" title="T">Later<\/a>/);
  assert.match(html, /<a href="https:\/\/example.com\/q">text<\/a>/);
  assert.match(html, /<h1 id="h-early"><a href="https:\/\/example.com\/early">early<\/a><\/h1>/);
  assert.doesNotMatch(html, /ignored/);
  // A block renders with only the definitions its brackets name.
  const state = EditorState.create({doc: note, extensions: livePreviewMarkdown()});
  const definitions = treeDefinitions(ensureSyntaxTree(state, state.doc.length, 5000), (from, to) => state.sliceDoc(from, to));
  assert.deepEqual(definitions.map(item => item.label), ['quoted', 'later', 'early']);
  assert.equal(blockSource('[early] only', definitions), '[early]: https://example.com/early\n\n[early] only');
  assert.equal(blockSource('no brackets', definitions), 'no brackets');
  assert.equal(blockSource('[unknown]', definitions), '[unknown]');
});

test('a definition inside a blockquote resolves without its quote marks', async () => {
  const note = 'Use [one] and [Two][] and ![img][three].\n\n> [one]: https://example.com/one\n>  "Title on next line"\n\n- [Two]: https://example.com/two\n\n> > [three]: images/a.png\n> > \'Three\'\n';
  const state = EditorState.create({doc: note, extensions: livePreviewMarkdown()});
  const definitions = treeDefinitions(ensureSyntaxTree(state, state.doc.length, 5000), (from, to) => state.sliceDoc(from, to));
  assert.deepEqual(definitions.map(item => item.text), ['[one]: https://example.com/one\n  "Title on next line"', '[Two]: https://example.com/two', "[three]: images/a.png\n  'Three'"]);
  const html = await renderPrivateMarkdown(note);
  assert.match(html, /^<p>Use <a href="https:\/\/example.com\/one" title="Title on next line">one<\/a> and <a href="https:\/\/example.com\/two">Two<\/a> and <img data-fmn-src="images\/a.png" alt="img" title="Three">\.<\/p>/);
});

test('heading ids follow goldmark: source text, ASCII only, unique across the note', async () => {
  const ids = new HeadingIDs();
  assert.deepEqual(['Notice', 'Notice', 'notice-1', '☃', ' *A* `b` ', 'snake_case-x'].map(line => ids.id(line)), ['h-notice', 'h-notice-1', 'h-notice-1-1', 'h-heading', 'h-a-b', 'h-snake-case-x']);
  assert.equal(headingLine('## Closed ##'), ' Closed');
  assert.equal(headingLine('## Not # closed'), ' Not # closed');
  assert.equal(headingLine('Two\nlines\n---'), 'lines');
  assert.equal(headingLine('One\n==='), 'One');
  const html = await renderPrivateMarkdown('# Same\n\npara\n\n## Same\n\n> ### Same\n');
  assert.deepEqual([...html.matchAll(/id="([^"]+)"/g)].map(match => match[1]), ['h-same', 'h-same-1', 'h-same-2']);
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

// headingLine and HeadingIDs once trimmed with regular expressions whose cost
// grew with the square of a run of spaces (2 s for a heading with 80,000);
// their code now walks the string once and must give what those gave.
test('heading lines and ids are what the regular expressions gave, in linear time', () => {
  const regexLine = raw => (raw.includes('\n') ? headingLine(raw) : raw.replace(/^ {0,3}#{1,6}/, '').replace(/(?:^|[ \t]+)#+[ \t]*$/, ''));
  const regexID = line => {
    let slug = '';
    for (const char of line.replace(/^[ \t\n\v\f\r]+|[ \t\n\v\f\r]+$/g, '')) {
      if (/^[A-Za-z0-9]$/.test(char)) slug += char.toLowerCase();
      else if (/^[ \t\n\v\f\r_-]$/.test(char)) slug += '-';
    }
    return 'h-' + (slug || 'heading');
  };
  const alphabet = ['#', '#', ' ', ' ', '\t', 'a', 'B', '_', '-', '\n', '\r', '\v', '\f', '\u00a0', '\u2028', '\ufeff', '☃'];
  for (const raw of randomStrings(alphabet, 100000, 12)) {
    assert.equal(headingLine(raw), regexLine(raw), JSON.stringify(raw));
    assert.equal(new HeadingIDs().id(raw), regexID(raw), JSON.stringify(raw));
  }
  // Closing sequences, more than six opening marks included.
  const marks = ['#######', '########  \t', ' ##########', '#######a', '#######\t#', '#  #', '# a#', '# a\t# #'];
  for (const raw of [...marks, ...randomStrings(['#', '#', '#', ' ', '\t', 'a'], 100000, 14, 12345)]) {
    assert.equal(headingLine(raw), regexLine(raw), JSON.stringify(raw));
  }
  const heading = '# a' + ' '.repeat(100000) + 'b';
  const line = timed(() => headingLine(heading));
  assert.equal(line.result, ' a' + ' '.repeat(100000) + 'b');
  assert.ok(line.ms < 50, `headingLine took ${line.ms.toFixed(1)} ms`);
  const id = timed(() => new HeadingIDs().id(line.result));
  assert.equal(id.result, 'h-a' + '-'.repeat(100000) + 'b');
  assert.ok(id.ms < 50, `HeadingIDs.id took ${id.ms.toFixed(1)} ms`);
  const closed = timed(() => headingLine('## a' + ' \t'.repeat(50000) + '##  '));
  assert.equal(closed.result, ' a');
  assert.ok(closed.ms < 50, `headingLine of a closed heading took ${closed.ms.toFixed(1)} ms`);
  const rendered = timed(() => renderMarkdown(heading, {ids: new HeadingIDs()}));
  assert.equal(rendered.result[0].attrs.id, id.result);
  assert.ok(rendered.ms < 200, `renderMarkdown with ids took ${rendered.ms.toFixed(1)} ms`);
});

// Lezer's HardBreak parser cost the square of a run of spaces that does not
// end its line, and a private preview parses in steps it cannot interrupt:
// a draft with such a line hung the page (minutes for 200,000 spaces).
test('a private preview of a long run of spaces in a line renders at once', async () => {
  const run = ' '.repeat(200000);
  const notes = [
    ['A line with' + run + 'padding.', '<p>A line with' + run + 'padding.</p>'],
    ['# Title' + run + 'x', '<h1 id="h-title' + '-'.repeat(200000) + 'x">Title' + run + 'x</h1>'],
    ['- a' + run + 'x\n- *b*', '<ul>\n<li>a' + run + 'x</li>\n<li><em>b</em></li>\n</ul>'],
  ];
  for (const [markdown, html] of notes) {
    const started = performance.now(), rendered = await renderPrivateMarkdown(markdown), ms = performance.now() - started;
    assert.ok(ms < 500, `${markdown.slice(0, 4)}: ${ms.toFixed(1)} ms`);
    assert.equal(normalize(rendered), normalize(html));
  }
});

test('front matter ends where the server says the body starts', async () => {
  assert.equal(bodyStart('---\ntitle: x\n---\nbody'), 17);
  assert.equal(bodyStart('---\r\ntitle: x\r\n---\r\nbody'), 20);
  assert.equal(bodyStart('---\ntitle: x\n'), 0);
  assert.equal(bodyStart('--- \ntitle: x\n---\n'), 0);
  assert.equal(bodyStart('---\n' + 'x'.repeat(70000) + '\n---\nbody'), 0);
  assert.equal(bodyStart('---\n' + 'é'.repeat(33000) + '\n---\nbody'), 0);
  assert.equal(bodyStart('---\n' + 'é'.repeat(32000) + '\n---\nbody'), 32009);
  // A character outside the BMP is four bytes, a lone surrogate three (the
  // server receives it as U+FFFD).
  assert.equal(bodyStart('---\n' + '😀'.repeat(20000) + '\n---\nbody'), 0);
  assert.equal(bodyStart('---\n' + '😀'.repeat(16000) + '\n---\nbody'), 32009);
  assert.equal(bodyStart('---\n' + '\ud800'.repeat(22000) + '\n---\nbody'), 0);
  assert.equal(bodyStart('---\n' + '\ud800'.repeat(21000) + '\n---\nbody'), 21009);
  assert.equal(await renderPrivateMarkdown('---\ntitle: Hidden\n---\n# Shown\n'), '<h1 id="h-shown">Shown</h1>');
  assert.match(await renderPrivateMarkdown('---\nunclosed: yes\n'), /<hr>/);
});

test('a private preview larger than its budget is refused before it is built', async () => {
  const note = '# Many references\n\n' + '[example][r] '.repeat(64) + '\n\n[r]: https://example.invalid/' + 'a'.repeat(16384);
  await assert.rejects(renderPrivateMarkdown(note), RangeError);
  assert.match(await renderPrivateMarkdown(note, {limit: 64 << 20}), /example.invalid/);
  assert.throws(() => renderMarkdown('x'.repeat(100), {budget: new Budget(50)}), RangeError);
});

test('private previews of long notes render in slices', async () => {
  const note = Array.from({length: 4000}, (_, i) => `## Part ${i}\n\nText of part ${i} with *emphasis*.\n`).join('\n');
  let ticks = 0;
  const timer = setInterval(() => ticks++, 0);
  const html = await renderPrivateMarkdown(note, {slice: 2});
  clearInterval(timer);
  assert.equal(html.match(/<h2 /g).length, 4000);
  assert.match(html, /id="h-part-3999"/);
  assert.ok(ticks > 2, 'other tasks ran while it rendered: ' + ticks);
});

test('a cancelled private preview stops at its next pause', async () => {
  const note = Array.from({length: 4000}, (_, i) => `## Part ${i}\n\nText of part ${i} with *emphasis*.\n`).join('\n');
  // Asked at every pause, it says so the second time: no slice runs after.
  let asked = 0;
  assert.equal(await renderPrivateMarkdown(note, {slice: 1, cancelled: () => ++asked >= 2}), null);
  assert.equal(asked, 2);
  // Not cancelled, the same note renders whole.
  assert.match(await renderPrivateMarkdown(note, {slice: 1, cancelled: () => false}), /id="h-part-3999"/);
});

test('elements become HTML with text and attributes escaped', () => {
  assert.equal(toHTML(renderMarkdown('a < b & "c" [x](<y"z>) ![q"](i.png "t&")')), '<p>a &lt; b &amp; "c" <a href="y%22z">x</a> <img src="i.png" alt="q&quot;" title="t&amp;"></p>');
  assert.equal(toHTML(renderMarkdown('![a](b.png)'), {imageAttribute: 'data-fmn-src'}), '<p><img data-fmn-src="b.png" alt="a"></p>');
});

// The pane's blocks over an editor state's whole tree.
const paneState = doc => {
  const state = EditorState.create({doc, extensions: [EditorState.lineSeparator.of('\n'), livePreviewMarkdown()]});
  return {state, tree: ensureSyntaxTree(state, state.doc.length, 5000)};
};

// The pane's path through the corpus: the editor's tree, its blocks and the
// note's definitions, each block rendered on its own. The pane gives headings
// no ids (a link to one finds it by the id Read gives it), so ids are left
// out of the goldens here.
function paneHTML(markdown) {
  const {state, tree} = paneState(markdown);
  const definitions = treeDefinitions(tree, (from, to) => state.sliceDoc(from, to));
  return paneBlocks(tree, state.doc)
    .map(block => toHTML(renderMarkdown(blockSource(state.sliceDoc(block.from, block.to), definitions))))
    .join('\n');
}

test('the pane renders the corpus as Read renders it', () => {
  for (const name of cases) {
    const markdown = readFileSync(new URL(name, corpus), 'utf8');
    const golden = readFileSync(new URL(name.replace(/\.md$/, '.html'), corpus), 'utf8');
    assert.equal(normalize(paneHTML(markdown)), normalize(golden).replace(/ id="[^"]*"/g, ''), name);
  }
});

test('the pane takes the body blocks of the tree, with the unparsed rest as one block', () => {
  const {state, tree} = paneState('---\ntitle: x\n---\n# Head\n\npara\n\n- a\n- b\n');
  assert.deepEqual(paneBlocks(tree, state.doc).map(({name, from, to}) => [name, state.sliceDoc(from, to)]), [
    ['ATXHeading1', '# Head'],
    ['Paragraph', 'para'],
    ['BulletList', '- a\n- b'],
  ]);
  const partial = paneBlocks({length: 25, cursor: () => tree.cursor()}, state.doc);
  assert.deepEqual(partial.map(block => block.name), ['ATXHeading1', 'Rest']);
  assert.equal(partial.at(-1).to, state.doc.length);
});

test('an edit inside one block keeps every other pane item and its element', () => {
  const {state, tree} = paneState('# One\n\nTwo words\n\nThree\n\nFour\n');
  // The pane's items, mapped through an edit as the pane maps them.
  const edit = change => {
    const items = reconcile([], paneBlocks(tree, state.doc)).map((item, i) => ({...item, element: 'element-' + i, dirty: false}));
    const changes = ChangeSet.of(change, state.doc.length);
    for (const item of items) {
      if (changes.touchesRange(item.from, item.to)) item.dirty = true;
      item.from = changes.mapPos(item.from, -1);
      item.to = changes.mapPos(item.to, 1);
    }
    const next = paneState(changes.apply(state.doc).toString());
    return {next, items: reconcile(items, paneBlocks(next.tree, next.state.doc))};
  };
  const typed = edit({from: 10, insert: ' more'});
  assert.deepEqual(typed.items.map(item => item.element), ['element-0', 'element-1', 'element-2', 'element-3']);
  assert.deepEqual(typed.items.map(item => item.dirty), [false, true, false, false]);
  assert.equal(typed.next.state.sliceDoc(typed.items[1].from, typed.items[1].to), 'Two more words');
  // Joining two paragraphs replaces both with one new item.
  const joined = edit({from: 16, to: 17});
  assert.equal(joined.next.state.doc.toString(), '# One\n\nTwo words\nThree\n\nFour\n');
  assert.deepEqual(joined.items.map(item => item.element), ['element-0', null, 'element-3']);
});

test('while the tree is partial, the items past it stay as they were', () => {
  const previous = [
    {name: 'Paragraph', from: 0, to: 9},
    {name: 'Paragraph', from: 11, to: 20},
    {name: 'ATXHeading1', from: 22, to: 30},
    {name: 'Paragraph', from: 32, to: 40},
  ];
  const parsed = [{name: 'Paragraph', from: 0, to: 9}, {name: 'Rest', from: 11, to: 40}];
  assert.deepEqual(keepUnparsed(parsed, previous), [parsed[0], ...previous.slice(1)]);
  // A block cut by the parse's end leaves a placeholder up to the next item.
  const cut = [{name: 'Paragraph', from: 0, to: 9}, {name: 'Rest', from: 15, to: 40}];
  assert.deepEqual(keepUnparsed(cut, previous), [cut[0], {name: 'Rest', from: 15, to: 21}, ...previous.slice(2)]);
  assert.deepEqual(keepUnparsed(parsed, []), parsed);
  const whole = [{name: 'Paragraph', from: 0, to: 9}];
  assert.equal(keepUnparsed(whole, previous), whole);
  // Reconciling keeps those items, with their elements.
  const items = previous.map((item, i) => ({...item, element: 'element-' + i, dirty: false}));
  assert.deepEqual(reconcile(items, keepUnparsed(parsed, items)).map(item => item.element), ['element-0', 'element-1', 'element-2', 'element-3']);
});

test('scroll positions map through sorted items and fractions', () => {
  const items = [{from: 0}, {from: 10}, {from: 25}];
  assert.deepEqual([0, 9, 10, 24, 25, 99].map(value => itemAt(items, value, item => item.from)), [0, 0, 1, 1, 2, 2]);
  assert.equal(itemAt([], 5, item => item.from), 0);
  assert.equal(fraction(15, 10, 20), 0.5);
  assert.equal(fraction(5, 10, 20), 0);
  assert.equal(fraction(25, 10, 20), 1);
  assert.equal(fraction(10, 10, 10), 0);
});

test('pane blocks are whole lines, so indented code and CRLF notes render as Read shows them', async () => {
  const {state, tree} = paneState('# A\r\n\r\npara\r\nmore\r\n\r\n    indented code\r\n');
  assert.deepEqual(paneBlocks(tree, state.doc).map(({from, to}) => state.sliceDoc(from, to)), ['# A\r', 'para\r\nmore\r', '    indented code\r']);
  assert.equal(await renderPrivateMarkdown('# A\r\n\r\npara\r\nmore\r\n\r\n    code\r\n'), await renderPrivateMarkdown('# A\n\npara\nmore\n\n    code\n'));
});

test('an open <textarea> runs on past blank lines, as Read reads it', async () => {
  const note = 'Before.\n\n<textarea>\n\n*x*\n\n</textarea>\n\nAfter.\n\nPara\n<textarea> open\n\n*y*\n';
  assert.equal(await renderPrivateMarkdown(note), '<p>Before.</p>\n\n<p>After.</p>\n<p>Para</p>');
  const {state, tree} = paneState(note);
  assert.deepEqual(paneBlocks(tree, state.doc).map(({name, from, to}) => [name, state.sliceDoc(from, to)]), [
    ['Paragraph', 'Before.'],
    ['HTMLBlock', '<textarea>\n\n*x*\n\n</textarea>'],
    ['Paragraph', 'After.'],
    ['Paragraph', 'Para\n<textarea> open\n\n*y*'],
  ]);
  // A CRLF note's too.
  const crlf = paneState('Para\r\n<textarea\r\n\r\n*z*\r\n');
  assert.equal(paneBlocks(crlf.tree, crlf.state.doc).length, 1);
  assert.equal(await renderPrivateMarkdown('Para\r\n<textarea\r\n\r\n*z*\r\n'), '<p>Para</p>');
  // A closed one, or one inside code, leaves its blocks as they are.
  const closed = paneState('<textarea>x</textarea>\n\n*a*\n\n```\n<textarea>\n```\n\n*b*\n');
  assert.equal(paneBlocks(closed.tree, closed.state.doc).length, 4);
});
