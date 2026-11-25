import {Transaction} from '@codemirror/state';
import {ViewPlugin} from '@codemirror/view';

// The note editor's caret blinks as a desktop caret does. It holds still
// while the person types, clicks or moves it, and once they pause for BLINK
// milliseconds it is hidden and shown in turn, BLINK each, for as long as the
// editor has the focus. Without the focus CodeMirror does not draw it, and
// when the system asks to reduce motion it holds still.
//
// A timer flips the visibility of CodeMirror's cursor layer, and nothing
// else changes: between two flips the page has nothing to draw. CodeMirror's
// own blink is a CSS animation, and while any animation runs (even a steps()
// one that changes twice a cycle) the browser draws a frame at every display
// refresh, so a key's paint waits for the next refresh instead of starting at
// once (6-7 ms more at the median of the F1 keystroke budget). A flip draws
// one frame, so only a key that lands within a frame or two of it waits, as
// with the browser's own blinking caret. A key or a caret move only notes the
// time, and shows the caret if it was hidden. Changes made elsewhere (peers,
// agents) leave the blink alone.
export const BLINK = 530;

const reducedMotion = () => typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;

export class CaretBlink {
  constructor(view) {
    this.view = view;
    this.layer = null;
    this.hidden = false;
    this.timer = null;
    this.moved = performance.now();
    this.tick = this.tick.bind(this);
    if (view.hasFocus) this.wait(BLINK);
  }
  update(update) {
    if (!update.focusChanged && !update.transactions.some(tr => tr.selection || tr.annotation(Transaction.userEvent) !== undefined)) return;
    this.moved = performance.now();
    if (!this.view.hasFocus) this.stop();
    else {
      this.show();
      if (this.timer === null) this.wait(BLINK);
    }
  }
  wait(delay) {
    this.timer = setTimeout(this.tick, delay);
  }
  tick() {
    this.timer = null;
    const left = this.moved + BLINK - performance.now();
    if (left > 0) return this.wait(left);
    // Held still: the next move starts the timer again.
    if (!this.view.hasFocus || reducedMotion()) return this.show();
    this.set(!this.hidden);
    this.wait(BLINK);
  }
  set(hidden) {
    if (!this.layer?.isConnected) this.layer = this.view.scrollDOM.querySelector(':scope > .cm-cursorLayer');
    if (!this.layer) return;
    this.hidden = hidden;
    this.layer.style.visibility = hidden ? 'hidden' : '';
  }
  show() {
    if (this.hidden) this.set(false);
  }
  stop() {
    clearTimeout(this.timer);
    this.timer = null;
    this.show();
  }
  destroy() {
    this.stop();
  }
}

export const caretBlink = ViewPlugin.fromClass(CaretBlink);
