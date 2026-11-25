import {test} from 'node:test';
import assert from 'node:assert/strict';
import {wikiTree, wikiFolder, wikiPager, wikiLayout, wikiLinkTarget} from './wiki.mjs';

const note = (path, metadata = {}, title = '') => ({path, title, metadata: {tags: [], ...metadata}});
const names = folder => folder.children.map(child => (child.kind === 'folder' ? child.path + '/' : child.path));

test('pages and folders follow their order value, then their title', () => {
  const tree = wikiTree([
    note('Zebra.md'),
    note('apple.md', {title: 'Apple'}),
    note('Setup.md', {order: 2}),
    note('Welcome.md', {order: -1}),
    note('guide/index.md', {title: 'The guide', order: 1}),
    note('guide/Chapter 10.md'),
    note('guide/Chapter 9.md'),
    note('zoo/animals.md'),
    note('index.md', {title: 'Home'}),
    note('AGENTS.md'),
    note('Trash/old.md')
  ]);
  assert.deepEqual(names(tree), ['Welcome.md', 'guide/', 'Setup.md', 'apple.md', 'Zebra.md', 'zoo/']);
  assert.equal(tree.index.path, 'index.md');
  const guide = wikiFolder(tree, 'guide');
  assert.equal(guide.title, 'The guide');
  assert.equal(guide.index.path, 'guide/index.md');
  assert.deepEqual(names(guide), ['guide/Chapter 9.md', 'guide/Chapter 10.md']);
  assert.equal(wikiFolder(tree, 'zoo').title, 'zoo');
  assert.equal(wikiFolder(tree, 'missing'), null);
});

test('a folder overview without its own title keeps the folder name', () => {
  const tree = wikiTree([note('recipes/index.md'), note('recipes/Soup.md')]);
  assert.equal(wikiFolder(tree, 'recipes').title, 'recipes');
  assert.equal(wikiFolder(tree, 'recipes').index.title, 'Overview');
});

test('previous and next stay within a folder, starting from its overview', () => {
  const tree = wikiTree([
    note('guide/index.md', {title: 'Guide'}),
    note('guide/b.md', {order: 2}, 'Second'),
    note('guide/a.md', {order: 1}, 'First'),
    note('guide/deeper/c.md'),
    note('other.md')
  ]);
  assert.deepEqual(wikiPager(tree, 'guide/index.md'), {previous: null, next: wikiFolder(tree, 'guide').children[0]});
  const middle = wikiPager(tree, 'guide/a.md');
  assert.equal(middle.previous.path, 'guide/index.md');
  assert.equal(middle.next.path, 'guide/b.md');
  assert.equal(middle.next.title, 'Second');
  const last = wikiPager(tree, 'guide/b.md');
  assert.equal(last.previous.path, 'guide/a.md');
  assert.equal(last.next, null);
  assert.deepEqual(wikiPager(tree, 'other.md'), {previous: null, next: null});
  assert.deepEqual(wikiPager(tree, 'drafts/elsewhere.md'), {previous: null, next: null});
});

test('the layout follows the link, then this browser’s choice, then the notebook default', () => {
  const wiki = {id: 'w', openAsWiki: true},
    plain = {id: 'p'};
  assert.equal(wikiLayout(wiki, '', ''), 'wiki');
  assert.equal(wikiLayout(plain, '', ''), 'editor');
  assert.equal(wikiLayout(wiki, '', 'editor'), 'editor');
  assert.equal(wikiLayout(plain, '', 'wiki'), 'wiki');
  assert.equal(wikiLayout(wiki, 'editor', 'wiki'), 'editor');
  assert.equal(wikiLayout(plain, 'wiki', ''), 'wiki');
  assert.equal(wikiLayout(plain, 'unknown', 'other'), 'editor');
  for (const notebook of [{id: 'r', kind: 'repository', openAsWiki: true}, {id: 'i', protectedInbox: 'u', openAsWiki: true}, null])
    assert.equal(wikiLayout(notebook, 'wiki', 'wiki'), 'editor');
});

test('[[links]] find a page by title, then by path', () => {
  const tree = wikiTree([
    note('index.md', {title: 'Home'}),
    note('Setup.md', {title: 'Getting started'}),
    note('guide/index.md', {title: 'The guide'}),
    note('guide/start.md', {title: 'Getting started'}),
    note('guide/Glossary.md'),
    note('guide/deep/start.md', {title: 'Getting started'}),
    note('notes/Glossary.md', {title: 'Terms'}),
    note('a/twin.md', {title: 'Twin'}),
    note('B/twin.md', {title: 'Twin'}),
    note('AGENTS.md')
  ]);
  const target = (source, text) => wikiLinkTarget(tree, source, text);
  // A title is matched without regard to case; the nearest page with it wins.
  assert.equal(target('index.md', 'getting STARTED'), 'Setup.md');
  assert.equal(target('guide/other.md', 'Getting started'), 'guide/start.md');
  assert.equal(target('guide/deep/x.md', ' Getting started '), 'guide/deep/start.md');
  assert.equal(target('notes/a.md', 'The guide'), 'guide/index.md');
  assert.equal(target('index.md', 'Home'), 'index.md');
  // Equally near pages fall back to path order, as the server orders them.
  assert.equal(target('index.md', 'Twin'), 'B/twin.md');
  // Titles come before paths: notes/Glossary.md is titled Terms.
  assert.equal(target('notes/a.md', 'Glossary'), 'guide/Glossary.md');
  assert.equal(target('index.md', 'Terms'), 'notes/Glossary.md');
  // Paths are from the notebook root, then from the linking page's folder.
  assert.equal(target('index.md', 'notes/Glossary'), 'notes/Glossary.md');
  assert.equal(target('index.md', '/notes/Glossary.md'), 'notes/Glossary.md');
  assert.equal(target('guide/x.md', 'deep/start'), 'guide/deep/start.md');
  assert.equal(target('index.md', 'guide'), 'guide/index.md');
  for (const missing of ['Missing page', 'AGENTS', '', '../index', 'notes']) assert.equal(target('guide/x.md', missing), null, missing);
});
