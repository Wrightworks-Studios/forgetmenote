import {test} from 'node:test';
import assert from 'node:assert/strict';
import {EditorState} from '@codemirror/state';
import {ensureSyntaxTree} from '@codemirror/language';
import {livePreviewMarkdown} from '../../../frontend/live-preview.mjs';
import {linkAt} from '../../../frontend/editor-links.mjs';
import {resolveLink} from './editor.mjs';

const note = [
  'An [inline link](notes/a.md#part "Title") and [a reference][Ref] and [shortcut] and [nowhere][none].',
  'Autolinks <https://example.com/x> and <someone@example.com>, bare www.example.org and https://example.net/y.',
  'Pages [[Garden plan]] and [[folder/Other|its label]], an image ![alt](pic.png) and [![badge](b.png)](https://example.com/b).',
  'Empty [nothing]() and plain words.',
  '',
  '[ref]: https://example.com/ref',
  '[shortcut]: ../up.md',
].join('\n') + '\n';
const state = EditorState.create({doc: note, extensions: [livePreviewMarkdown()]});
ensureSyntaxTree(state, state.doc.length, 5000);
const at = (text, offset = 1) => {
  const link = linkAt(state, note.indexOf(text) + offset);
  return link && (link.href != null ? {href: link.href} : {wiki: link.wiki});
};

test('a Ctrl/Cmd-click follows the link under the pointer, as the reader would', () => {
  assert.deepEqual(at('inline link'), {href: 'notes/a.md#part'});
  assert.deepEqual(at('Title'), {href: 'notes/a.md#part'});
  assert.deepEqual(at('a reference'), {href: 'https://example.com/ref'});
  assert.deepEqual(at('shortcut]'), {href: '../up.md'});
  assert.deepEqual(at('https://example.com/x'), {href: 'https://example.com/x'});
  assert.deepEqual(at('someone@'), {href: 'mailto:someone@example.com'});
  assert.deepEqual(at('www.example.org'), {href: 'http://www.example.org'});
  assert.deepEqual(at('https://example.net/y'), {href: 'https://example.net/y'});
  assert.deepEqual(at('Garden plan'), {wiki: 'Garden plan'});
  assert.deepEqual(at('its label'), {wiki: 'folder/Other'});
  assert.deepEqual(at('badge'), {href: 'https://example.com/b'});
});

test('text that leads nowhere is edited, not followed', () => {
  for (const text of ['nowhere', 'alt](pic', 'nothing', 'plain words', 'An '])
    assert.equal(linkAt(state, note.indexOf(text) + 1), null, text);
});

// The reader follows the href goldmark writes: the address without its <…>
// brackets, with escapes and character references decoded, percent-encoded.
test('an address in <…>, with escapes or with character references leads where the reader goes', () => {
  const doc = [
    '[file](</attachments/My file.pdf>) [app file](</attachments/My%20file.pdf> "video") [note](<other note.md>)',
    '[esc](foo\\_bar.md) [ent](a&amp;b.md) [pct](100%.md) [hex](caf&#xE9;.md) [ref][r] [spaced]( <x y.md> )',
    '[web](<https://example.com/a b?q=1&amp;r=2#top>)',
    '',
    '[r]: <ref\\_x y.md>',
  ].join('\n') + '\n';
  const links = EditorState.create({doc, extensions: [livePreviewMarkdown()]});
  ensureSyntaxTree(links, links.doc.length, 5000);
  const follow = label => {
    const link = linkAt(links, doc.indexOf('[' + label + ']') + 1);
    return link && {href: link.href, target: resolveLink('dir/n.md', link.href)};
  };
  assert.deepEqual(follow('file'), {href: '/attachments/My%20file.pdf', target: {path: 'attachments/My file.pdf', hash: ''}});
  assert.deepEqual(follow('app file'), {href: '/attachments/My%20file.pdf', target: {path: 'attachments/My file.pdf', hash: ''}});
  assert.deepEqual(follow('note'), {href: 'other%20note.md', target: {path: 'dir/other note.md', hash: ''}});
  assert.deepEqual(follow('esc'), {href: 'foo_bar.md', target: {path: 'dir/foo_bar.md', hash: ''}});
  assert.deepEqual(follow('ent'), {href: 'a&b.md', target: {path: 'dir/a&b.md', hash: ''}});
  assert.deepEqual(follow('pct'), {href: '100%25.md', target: {path: 'dir/100%.md', hash: ''}});
  assert.deepEqual(follow('hex'), {href: 'caf%C3%A9.md', target: {path: 'dir/café.md', hash: ''}});
  assert.deepEqual(follow('ref'), {href: 'ref_x%20y.md', target: {path: 'dir/ref_x y.md', hash: ''}});
  assert.deepEqual(follow('spaced'), {href: 'x%20y.md', target: {path: 'dir/x y.md', hash: ''}});
  assert.deepEqual(follow('web'), {href: 'https://example.com/a%20b?q=1&r=2#top', target: {external: true, url: 'https://example.com/a%20b?q=1&r=2#top'}});
});
