import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';
import {anchorOf, bodyStart, placeAnchors, renderedText} from '../../../frontend/rendered-text.mjs';

const corpus = new URL('../../workspace/testdata/comment-text/', import.meta.url);
const notes = readdirSync(corpus).filter(name => name.endsWith('.md')).sort();
const read = name => readFileSync(new URL(name, corpus), 'utf8');

// The server's commentText wrote each golden (go test ./internal/workspace
// -run TestCommentTextCorpus -update-comment-text).
for (const name of notes)
  test('the rendered text of ' + name + ' is the server’s', () => {
    assert.equal(renderedText(read(name)).text, read(name.replace(/\.md$/, '.txt')));
  });

// Every character keeps the source it shows: itself, an escape or a
// character reference; a space keeps a place between its neighbours.
for (const name of notes)
  test('every rendered character of ' + name + ' maps back to its source', () => {
    const source = read(name), {text, from, to} = renderedText(source);
    assert.equal(from.length, text.length);
    for (let i = 0; i < text.length; i++) {
      assert.ok(from[i] <= to[i] && to[i] <= source.length, 'range of ' + i);
      if (i) assert.ok(from[i] >= from[i - 1], 'order at ' + i);
      if (text[i] === ' ') continue;
      const shown = source.slice(from[i], to[i]);
      assert.ok(shown === text[i] || shown === '\\' + text[i] || shown.startsWith('&') || /[\ud800-\udfff]/.test(text[i]),
        name + ': ' + JSON.stringify(text[i]) + ' at ' + i + ' comes from ' + JSON.stringify(shown));
    }
  });

test('front matter is read as the server reads it', () => {
  for (const note of ['---\ntitle: x\n---\nBody', '---\r\ntitle: x\r\n---\r\nBody', '---\ntitle: x\n---\r\r\nBody'])
    assert.equal(bodyStart(note), note.indexOf('Body'));
  assert.equal(bodyStart('---\ntitle: x\n...\nBody'), 0);
  assert.equal(bodyStart('--- \ntitle: x\n---\n'), 0);
  assert.equal(bodyStart('\ufeff---\ntitle: x\n---\n'), 0);
  assert.equal(bodyStart('---\nunclosed\n'), 0);
  const long = '---\n' + 'k: v\n'.repeat(13200) + '---\nBody';
  assert.equal(bodyStart(long), 0);
  assert.equal(bodyStart('---\n' + 'k: v\n'.repeat(13000) + '---\nBody'), 4 + 65000 + 4);
});

test('a passage across emphasis, escapes and entities is found and selected in the Markdown', () => {
  const source = '# Title\n\nSome *very* bright\nstar &amp; \\*moon\\*.\n\nOther very bright star.\n';
  const rendered = renderedText(source);
  assert.equal(rendered.text, 'Title Some very bright star & *moon*. Other very bright star.');
  const placed = placeAnchors(rendered, [
    {id: 'a', anchor: {quote: 'very bright star & *moon*', prefix: 'Some ', suffix: '.'}},
    {id: 'b', anchor: {quote: 'very bright star', prefix: 'Other ', suffix: '.'}},
    {id: 'gone', anchor: {quote: 'dim', prefix: '', suffix: ''}},
    {id: 'resolved', resolved: true, anchor: {quote: 'Title', prefix: '', suffix: ''}},
  ]);
  assert.deepEqual([...placed.keys()], ['a', 'b']);
  const a = placed.get('a'), b = placed.get('b');
  assert.equal(source.slice(a.from, a.to), 'very* bright\nstar &amp; \\*moon\\*');
  assert.equal(source.slice(b.from, b.to), 'very bright star');
});

test('the anchor of a source selection quotes its rendered text with context', () => {
  const source = 'Intro.\n\n> A *quoted*\n> line with [a link](http://x.example).\n';
  const rendered = renderedText(source);
  const from = source.indexOf('quoted'), to = source.indexOf('link]') + 4;
  assert.deepEqual(anchorOf(rendered, from, to), {quote: 'quoted line with a link', prefix: 'Intro. A ', suffix: '.'});
  // Markup alone shows no text.
  assert.equal(anchorOf(rendered, source.indexOf('(http'), source.indexOf(')') + 1), null);
  assert.equal(anchorOf(rendered, 3, 3), null);
});

test('a quote repeated in the note is placed by its context', () => {
  const source = '# A\n\nThe same words.\n\n# B\n\nThe same words.\n';
  const rendered = renderedText(source), second = source.lastIndexOf('same words');
  const anchor = anchorOf(rendered, second, second + 'same words'.length);
  assert.deepEqual(anchor, {quote: 'same words', prefix: 'A The same words. B The ', suffix: '.'});
  assert.deepEqual(placeAnchors(rendered, [{id: 'x', anchor}]).get('x'), {from: second, to: second + 10});
});

test('a character reference to an emoji or other astral character keeps later text in place', () => {
  for (const reference of ['&#x1F600;', '&#128512;', '&Afr;']) {
    const source = 'Smile ' + reference + ' then the quiet harbour lights.\n';
    const rendered = renderedText(source);
    assert.equal(rendered.from.length, rendered.text.length, reference);
    const from = source.indexOf('harbour'), to = from + 'harbour'.length;
    assert.deepEqual(anchorOf(rendered, from, to), {quote: 'harbour', prefix: rendered.text.slice(0, rendered.text.indexOf('harbour')), suffix: ' lights.'}, reference);
    const placed = placeAnchors(rendered, [{id: 'h', anchor: {quote: 'harbour', prefix: 'quiet ', suffix: ' lights'}}]).get('h');
    assert.equal(source.slice(placed.from, placed.to), 'harbour', reference);
  }
});
