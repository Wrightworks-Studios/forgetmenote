import {test} from 'node:test';
import assert from 'node:assert/strict';
import {EditorState} from '@codemirror/state';
import {BLINK, CaretBlink} from '../../../frontend/editor-caret.mjs';

// An editor view as CaretBlink sees it: the focus and CodeMirror's cursor
// layer, whose every visibility write is recorded.
function editor({focused = true} = {}) {
  const writes = [];
  let visibility = '';
  const layer = {isConnected: true, style: {}};
  Object.defineProperty(layer.style, 'visibility', {
    get: () => visibility,
    set: value => {
      visibility = value;
      writes.push(value);
    },
  });
  const view = {hasFocus: focused, scrollDOM: {querySelector: selector => (selector === ':scope > .cm-cursorLayer' ? layer : null)}};
  return {view, writes, shown: () => visibility !== 'hidden'};
}

const state = EditorState.create({doc: 'Call me Ishmael.'});
const typed = {focusChanged: false, transactions: [state.update({changes: {from: 0, insert: 'x'}, selection: {anchor: 1}, userEvent: 'input.type'})]};
const moved = {focusChanged: false, transactions: [state.update({selection: {anchor: 4}, userEvent: 'select'})]};
const elsewhere = {focusChanged: false, transactions: [state.update({changes: {from: 0, insert: 'A peer wrote this. '}})]};
const focusChanged = {focusChanged: true, transactions: []};

// Fake timers, with performance.now() on the same fake clock, and
// matchMedia answering reduced (prefers-reduced-motion) and counting calls.
function clock(t, {reduced = false} = {}) {
  t.mock.timers.enable({apis: ['setTimeout', 'Date'], now: 0});
  const saved = {performance: globalThis.performance, matchMedia: globalThis.matchMedia};
  const media = {calls: 0, reduced};
  globalThis.performance = {now: () => Date.now()};
  globalThis.matchMedia = query => {
    media.calls++;
    return {matches: query === '(prefers-reduced-motion: reduce)' && media.reduced};
  };
  t.after(() => {
    globalThis.performance = saved.performance;
    if (saved.matchMedia) globalThis.matchMedia = saved.matchMedia;
    else delete globalThis.matchMedia;
  });
  return {media, tick: ms => t.mock.timers.tick(ms)};
}

test('a focused caret holds still for BLINK ms, then is hidden and shown in turn every BLINK ms', t => {
  const {tick} = clock(t), {view, writes, shown} = editor();
  new CaretBlink(view);
  tick(BLINK - 1);
  assert.equal(shown(), true);
  const seen = [];
  for (let i = 0; i < 4; i++) {
    tick(i ? BLINK : 1);
    seen.push(shown());
  }
  assert.deepEqual(seen, [false, true, false, true]);
  assert.equal(writes.length, 4, 'one write per flip');
});

test('a key or a caret move shows a hidden caret at once and holds it still until a pause, writing nothing while it shows', t => {
  const {tick} = clock(t), {view, writes, shown} = editor(), blink = new CaretBlink(view);
  tick(BLINK);
  assert.equal(shown(), false);
  blink.update({...typed, view});
  assert.equal(shown(), true, 'the key shows the caret in its own update');
  assert.equal(writes.length, 2);
  // Three seconds of typing and moving, a key every 100 ms.
  for (let i = 0; i < 30; i++) {
    tick(100);
    blink.update({...(i % 3 ? typed : moved), view});
    assert.equal(shown(), true);
  }
  assert.equal(writes.length, 2, 'typing while the caret shows touches nothing');
  tick(BLINK - 1);
  assert.equal(shown(), true);
  tick(1);
  assert.equal(shown(), false, 'a pause of BLINK ms starts the blink again');
});

test('changes made elsewhere leave the blink alone', t => {
  const {tick} = clock(t), {view, shown} = editor(), blink = new CaretBlink(view);
  tick(BLINK);
  blink.update({...elsewhere, view});
  assert.equal(shown(), false);
  tick(BLINK);
  assert.equal(shown(), true);
  tick(BLINK / 2);
  blink.update({...elsewhere, view});
  tick(BLINK / 2);
  assert.equal(shown(), false, 'the blink keeps its rhythm');
});

test('without the focus the caret shows (CodeMirror then hides it) and nothing runs until the focus returns', t => {
  const {tick, media} = clock(t), {view, writes, shown} = editor({focused: false}), blink = new CaretBlink(view);
  tick(5 * BLINK);
  assert.deepEqual(writes, []);
  view.hasFocus = true;
  blink.update({...focusChanged, view});
  tick(BLINK);
  assert.equal(shown(), false, 'focusing starts the blink');
  view.hasFocus = false;
  blink.update({...focusChanged, view});
  assert.equal(shown(), true);
  const [count, calls] = [writes.length, media.calls];
  tick(10 * BLINK);
  assert.equal(writes.length, count);
  assert.equal(media.calls, calls, 'no timer runs without the focus');
  view.hasFocus = true;
  blink.update({...focusChanged, view});
  tick(BLINK);
  assert.equal(shown(), false);
  blink.destroy();
  assert.equal(shown(), true);
  tick(10 * BLINK);
  assert.equal(shown(), true, 'a destroyed editor blinks no more');
});

test('when the system asks to reduce motion the caret holds still, with no timer between moves', t => {
  const {tick, media} = clock(t, {reduced: true}),
    {view, writes, shown} = editor(),
    blink = new CaretBlink(view);
  tick(10 * BLINK);
  assert.deepEqual(writes, []);
  assert.equal(media.calls, 1, 'one look after the pause, then no timer');
  blink.update({...typed, view});
  tick(10 * BLINK);
  assert.deepEqual(writes, []);
  assert.equal(media.calls, 2);
  // Turned on while the caret blinks, it shows the caret at the next flip
  // and stops.
  media.reduced = false;
  blink.update({...moved, view});
  tick(BLINK);
  assert.equal(shown(), false);
  media.reduced = true;
  tick(BLINK);
  assert.equal(shown(), true);
  const count = writes.length;
  tick(10 * BLINK);
  assert.equal(writes.length, count);
});
