import test from 'node:test';
import assert from 'node:assert/strict';
import {noteCreationTarget} from './creation.mjs';

test('note creation joins a selected folder and plain name with one Markdown extension', () => {
  assert.deepEqual(noteCreationTarget('  My idea  ', 'research/ideas'), {path:'research/ideas/My idea.md'});
  assert.deepEqual(noteCreationTarget('My idea.MD', ''), {path:'My idea.md'});
  assert.deepEqual(noteCreationTarget('Æsir & memories', 'Ideas'), {path:'Ideas/Æsir & memories.md'});
});
test('note creation refuses path input, missing folder selection and reserved instructions', () => {
  for (const name of ['', ' ', '.hidden', '../idea', 'folder/idea.md', 'folder\\idea', 'bad\nname', 'a'.repeat(513)]) {
    assert.ok(noteCreationTarget(name, '').error, JSON.stringify(name));
  }
  assert.ok(noteCreationTarget('Idea', null).error);
  assert.ok(noteCreationTarget('Idea', '../private').error);
  assert.ok(noteCreationTarget('AGENTS', '').error);
  assert.deepEqual(noteCreationTarget('AGENTS', 'research'), {path:'research/AGENTS.md'});
});
