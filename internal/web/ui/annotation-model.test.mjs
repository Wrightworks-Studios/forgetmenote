import {test} from 'node:test';
import assert from 'node:assert/strict';
import {ChangeSet, EditorState} from '@codemirror/state';
import {passage, passages, setPassages, settle} from '../../../frontend/editor-annotations.mjs';
import {anchorOf, placeAnchors, renderedText} from '../../../frontend/rendered-text.mjs';

const text = '# Title\n\nA **bright** star shines over the harbour.\n';
const star = {from: text.indexOf('bright'), to: text.indexOf('star') + 4};
const placed = (state = EditorState.create({doc: text, extensions: [passages]}), ranges = [{id: 'c', ...star}]) =>
  state.update({effects: setPassages.of(settle(ranges, ChangeSet.empty(state.doc.length)))}).state;
const shown = state => {
  const found = passage(state, 'c');
  return found && state.sliceDoc(found.from, found.to);
};

test('a passage placed by its anchor in any mode is the Markdown of its rendered text', () => {
  const rendered = renderedText(text), anchor = anchorOf(rendered, star.from, star.to);
  assert.deepEqual(anchor, {quote: 'bright star', prefix: 'Title A ', suffix: ' shines over the harbour.'});
  const state = placed(undefined, [...placeAnchors(rendered, [{id: 'c', anchor}])].map(([id, range]) => ({id, ...range})));
  assert.equal(shown(state), 'bright** star');
});

test('highlights move with edits around them and an edit inside removes them', () => {
  const state = placed();
  const before = state.update({changes: {from: 0, insert: 'Intro.\n\n'}}).state;
  assert.equal(shown(before), 'bright** star');
  // Typing at either edge stays outside the passage.
  const edges = state.update({changes: [{from: star.from, insert: 'very '}, {from: star.to, insert: 's'}]}).state;
  assert.equal(shown(edges), 'bright** star');
  for (const change of [{from: star.from + 3, insert: 'x'}, {from: star.from, to: star.from + 1}, {from: star.from - 3, to: star.to + 2, insert: 'gone'}])
    assert.equal(passage(state.update({changes: change}).state, 'c'), null, JSON.stringify(change));
});

test('an open composition only moves highlights, so the IME text is left alone', () => {
  const state = placed(), composed = state.update({changes: {from: star.from + 3, insert: 'あ'}, userEvent: 'input.type.compose'}).state;
  assert.deepEqual(passage(composed, 'c'), {from: star.from, to: star.to + 1});
});

test('ranges placed in an older text follow the edits made since and drop the ones they changed', () => {
  const start = EditorState.create({doc: text, extensions: [passages]});
  const shine = {from: text.indexOf('shines'), to: text.indexOf('shines') + 6};
  let since = ChangeSet.empty(text.length), state = start;
  for (const change of [{from: 0, insert: 'New first line.\n'}, {from: 16 + shine.from + 2, insert: 'i'}]) {
    const tr = state.update({changes: change});
    since = since.compose(tr.changes);
    state = tr.state;
  }
  state = state.update({effects: setPassages.of(settle([{id: 'c', ...star}, {id: 'gone', ...shine}], since))}).state;
  assert.equal(shown(state), 'bright** star');
  assert.equal(passage(state, 'gone'), null);
});
