import {test} from 'node:test';
import assert from 'node:assert/strict';
import {EditorSelection, EditorState} from '@codemirror/state';
import {ensureSyntaxTree} from '@codemirror/language';
import {livePreviewMarkdown} from '../../../frontend/live-preview.mjs';
import {currentBlock} from '../../../frontend/editor-focus.mjs';

const note = '# Chapter\n\nFirst paragraph\nwith two lines.\n\n- a list\n- of items\n\n\nLast.\n';
const lines = (caret, ranges = [{from: 0, to: note.length}]) => {
  const state = EditorState.create({doc: note, selection: EditorSelection.cursor(caret), extensions: [livePreviewMarkdown()]});
  const marked = [];
  currentBlock(state, ranges, ensureSyntaxTree(state, note.length, 5000)).between(0, note.length, from => {
    marked.push(state.doc.lineAt(from).text);
  });
  return marked;
};

test('Dim others keeps the whole block that holds the caret bright', () => {
  assert.deepEqual(lines(note.indexOf('two lines')), ['First paragraph', 'with two lines.']);
  assert.deepEqual(lines(note.indexOf('of items')), ['- a list', '- of items']);
  assert.deepEqual(lines(2), ['# Chapter']);
});

test('between blocks only the caret line is bright, and only drawn lines are marked', () => {
  assert.deepEqual(lines(note.indexOf('\n\n\nLast') + 2), ['']);
  const second = note.indexOf('with');
  assert.deepEqual(lines(note.indexOf('First'), [{from: second, to: note.length}]), ['with two lines.']);
});
