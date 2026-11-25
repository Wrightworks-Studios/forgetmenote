import {test} from 'node:test';
import assert from 'node:assert/strict';
import {derivedLines, occurrenceText, receiptText, unchangedText} from './purge.mjs';

test('a purge plan says that commit authors keep the text it erases', () => {
  const authors = {kind: 'author', path: '', versions: 2};
  assert.equal(occurrenceText(authors), 'in the author or committer of 2 commits, which a purge does not erase');
  assert.equal(occurrenceText({kind: 'file', path: 'b.md', versions: 1}), '1 version');
  assert.equal(occurrenceText({kind: 'link', path: 'index.md', versions: 2}), '2 versions may link to it by name');
  assert.equal(unchangedText({elsewhere: [authors], recordsUnsearched: true}), 'Nothing in this notebook’s notes or history holds it but commit authors or committers, which a purge does not erase.');
  assert.equal(unchangedText({elsewhere: []}), 'Nothing in this notebook’s notes or history holds it; only the records below quoted it.');
});

test('an Owner who is not an Admin is told only of their own records, and that other people’s are redacted too', () => {
  const others = 'Other people’s records that quote it are redacted too; only an instance Admin is told how many.';
  assert.deepEqual(derivedLines({chats: 2, jevRecords: 1}), ['2 private chat transcripts, 1 Jev record that quoted it will be redacted by the system, without anyone reading them.']);
  assert.deepEqual(derivedLines({}), ['No chat transcript, proposal or Jev record quotes it.']);
  assert.deepEqual(derivedLines({chats: 1, own: true, others}), ['Of your own records, 1 private chat transcript quoted it and will be redacted by the system, without anyone reading them.', others]);
  assert.deepEqual(derivedLines({own: true, others}), ['None of your own chat transcripts, proposals or Jev records quotes it.', others]);
  assert.deepEqual(derivedLines({chats: 1, own: true, others}, true), ['Of your own records, 1 chat transcript, 0 proposals and 0 Jev records redacted.', others]);
  assert.deepEqual(derivedLines({chats: 3, proposals: 1}, true), ['3 chat transcripts, 1 proposal and 0 Jev records redacted.']);
  assert.deepEqual(derivedLines({withheld: true, others: 'Only the person who ran it is told how many.'}, true), ['Only the person who ran it is told how many.']);
  assert.deepEqual(derivedLines({withheld: true}, true), []);
});

test('the list of earlier purges says which are unfinished or were interrupted', () => {
  const base = {purge: 'purge-1', actor: 'keeper', reason: 'Leaked key', created: ''};
  assert.equal(receiptText({...base, kind: 'text', wholeNotebook: true}), 'erased text everywhere, by keeper: Leaked key');
  assert.equal(receiptText({...base, kind: 'text', pattern: true, actorName: 'Kim'}), 'erased a pattern in chosen notes, by Kim: Leaked key');
  assert.equal(receiptText({...base, kind: 'note', unfinished: true}), 'Unfinished: erased a whole note, by keeper: Leaked key');
  assert.equal(receiptText({...base, kind: 'versions', interrupted: true}), 'Interrupted: erased chosen versions of a note, by keeper: Leaked key');
});
