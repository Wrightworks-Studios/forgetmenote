// The note editor's caret can be seen wherever it is: in Chromium and WebKit,
// in Visual, Source and Side by side, in the light and dark themes and focus
// mode, on a desktop and a phone, at the start of a note, mid-text, at line
// ends, next to hidden markers and next to widgets (front matter, tables,
// rules, images), in code and on empty lines. Focus is not trusted: each case
// compares a screenshot of the caret's place with one where every caret (the
// browser's and the editor's own) is hidden, and the caret must differ from
// the background there by at least WCAG's 3:1 non-text contrast. A caret
// blinks, so the place is sampled for more than one blink before a case
// fails. The selection is painted in the app's selection colour, over plain
// text and over a code block's background. The caret blinks as a desktop
// caret does: steady while the person types or moves it, hidden and shown in
// turn once they pause, steady again at the next key, without the focus not
// drawn at all, and steady when the system asks to reduce motion. It blinks
// on a timer, never with an animation (frontend/editor-caret.mjs).
import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {editorText, expectVisual} from './editor-text.mjs';

const note = [
  '---',
  'title: Caret check',
  '---',
  '',
  '# A heading',
  '',
  'Plain words, then **strong** text and a [link](https://example.com/) end',
  '',
  '| Left | Right |',
  '|------|-------|',
  '| one  | two   |',
  '',
  '---',
  '',
  '![Picture](assets/caret-picture.png)',
  '',
  '```',
  'A code block line',
  '```',
  '',
  'Last line of the note.',
  ''
].join('\n');
const after = (needle, shift = 0) => note.indexOf(needle) + needle.length + shift;
const before = (needle, shift = 0) => note.indexOf(needle) + shift;
// Where the caret goes, as text offsets.
const places = {
  'the start of the note': 0,
  'the empty line after the front matter': after('\n---\n'),
  'the end of a heading': after('# A heading'),
  'an empty line': after('# A heading\n'),
  'one character before hidden bold markers': after('then'),
  'the middle of bold text': before('strong', 3),
  'one character before a hidden link': after('and a'),
  'the end of a line': after(' end'),
  'the empty line before a table': before('| Left', -1),
  'the empty line after a table': after('two   |\n'),
  'the empty line after a rule': after('|\n\n---\n'),
  'the end of an image, next to its picture': after('.png)'),
  'the empty line after an image': after('.png)\n'),
  'the middle of a code block line': before('block line'),
  'the middle of a line': before('of the note'),
  'the end of the note': note.length
};

let app;

// Opens the note (with its picture) in format, in theme, focused by a click
// or a tap on its last line.
async function open(page, testInfo, {theme, format, focus = false, touch = false}) {
  const path = 'caret-' + testInfo.testId + '.md';
  await page.addInitScript(theme => localStorage.setItem('gitnotes:theme', theme), theme);
  await page.goto(app.origin + '/login');
  // A small two-colour picture, so the image widget has a size of its own.
  const uploaded = await page.evaluate(async origin => {
    const canvas = new OffscreenCanvas(120, 48),
      context = canvas.getContext('2d');
    context.fillStyle = '#3a7d44';
    context.fillRect(0, 0, 120, 48);
    context.fillStyle = '#e8bc2d';
    context.fillRect(30, 12, 60, 24);
    const form = new FormData();
    form.append('file', await canvas.convertToBlob({type: 'image/png'}), 'caret-picture.png');
    return (await fetch(origin + '/p/default/api/upload', {method: 'POST', body: form})).status;
  }, app.origin);
  expect([200, 409]).toContain(uploaded);
  await app.api('/p/default/api/note', {path, content: note, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-' + format).click();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
  if (format === 'visual') await expectVisual(page);
  if (focus) {
    await page.locator('#focus-toggle').click();
    await expect(page.locator('body')).toHaveClass(/\bfocus-mode\b/);
  }
  await expect.poll(() => editorText(page)).toBe(note);
  // A person clicks (or taps) into the text: the editor then has the focus
  // and is editable, with the browser's selection in it.
  const last = page.locator('#editor .cm-line', {hasText: 'Last line of the note.'});
  await last.scrollIntoViewIfNeeded();
  if (touch) await last.tap();
  else await last.click();
  expect(await editorState(page)).toEqual({focused: true, editable: 'true', readOnly: false, selectionInside: true});
  expect(await animations(page)).toEqual([]);
}

// The animations running on the caret. It blinks on a timer, never with an
// animation: while an animation runs the page draws a frame at every display
// refresh, and each key's paint waits for the next one (editor-caret.mjs).
// An editor that draws no caret has no cursor layer; the cases then report
// what they see of the browser's own caret.
const animations = page =>
  page.evaluate(() =>
    (document.querySelector('#note-editor .cm-cursorLayer')?.getAnimations({subtree: true}) ?? []).map(
      animation => animation.animationName ?? animation.constructor.name
    )
  );

// Whether the editor has the focus, is editable, and holds the browser's
// selection.
const editorState = page =>
  page.evaluate(() => {
    const content = document.querySelector('#editor'),
      view = content.cmTile.root.view,
      selection = getSelection();
    return {
      focused: document.activeElement === content && view.hasFocus,
      editable: content.getAttribute('contenteditable'),
      readOnly: view.state.readOnly,
      selectionInside: !!selection.focusNode && content.contains(selection.focusNode)
    };
  });

// Puts the caret at offset, as the keyboard or a click would (offset null
// leaves it where it is), and returns the box of the text beside it (CSS
// pixels): next to a picture, the text's height, not the picture's.
const caretAt = (page, at) =>
  page.evaluate(async at => {
    const view = document.querySelector('#editor').cmTile.root.view;
    if (at === null) at = view.state.selection.main.head;
    else
      view.dispatch({
        selection: {anchor: at},
        effects: view.constructor.scrollIntoView(at, {y: 'center'}),
        userEvent: 'select'
      });
    for (let i = 0; i < 3; i++) await new Promise(requestAnimationFrame);
    const [box] = [view.coordsAtPos(at, -1), view.coordsAtPos(at, 1)]
      .filter(box => box && box.bottom > box.top)
      .sort((a, b) => a.bottom - a.top - (b.bottom - b.top));
    return box && {x: box.left, top: box.top, bottom: box.bottom};
  }, at);
// The screenshot clip around a caret's box.
const caretClip = box => ({
  x: Math.max(0, Math.round(box.x) - 8),
  y: Math.round(box.top),
  width: 16,
  height: Math.max(4, Math.round(box.bottom - box.top))
});

// Adds a style sheet to the page under name, or removes it (css null). The
// app's policy refuses injected <style> elements; a constructed sheet is
// CSSOM, which it allows.
const style = (page, name, css) =>
  page.evaluate(
    ([name, css]) => {
      const sheets = (window.caretSpecSheets ||= {});
      document.adoptedStyleSheets = document.adoptedStyleSheets.filter(sheet => sheet !== sheets[name]);
      if (css === null) return;
      sheets[name] = new CSSStyleSheet();
      sheets[name].replaceSync(css);
      document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheets[name]];
    },
    [name, css]
  );
// Every caret hidden: the browser's and CodeMirror's drawn one.
const noCarets = '#editor { caret-color: transparent !important } .cm-cursorLayer { visibility: hidden !important }';
// The selection unpainted, whoever paints it (an id outranks the editor's
// theme).
const noSelection =
  '#editor ::selection, #editor::selection { background-color: transparent !important }' +
  ' .cm-selectionLayer { visibility: hidden !important }';

// Compares, in the page, two screenshots of one place. For a caret: the
// column that changed most, the share of its rows that changed, and the
// median contrast ratio of its changed pixels against the background. For a
// selection: the share of pixels it changed, the colour it painted most, the
// app's selection colour and how many levels the two differ by in the
// channel where they differ most.
const compare = (page, kind, shown, hidden) =>
  page.evaluate(
    async ([kind, shown, hidden]) => {
      const pixels = async base64 => {
        const bytes = Uint8Array.from(atob(base64), c => c.charCodeAt(0)),
          bitmap = await createImageBitmap(new Blob([bytes], {type: 'image/png'})),
          canvas = new OffscreenCanvas(bitmap.width, bitmap.height),
          context = canvas.getContext('2d');
        context.drawImage(bitmap, 0, 0);
        return context.getImageData(0, 0, bitmap.width, bitmap.height);
      };
      const [a, b] = [await pixels(shown), await pixels(hidden)];
      // The light theme's selection colour is 16 levels from a code block's
      // background, so a selection counts any change.
      const least = kind === 'selection' ? 4 : 16,
        changed = i => Math.max(...[0, 1, 2].map(k => Math.abs(a.data[i + k] - b.data[i + k]))) > least;
      if (kind === 'selection') {
        const colours = {};
        let count = 0;
        for (let i = 0; i < a.data.length; i += 4) {
          if (!changed(i)) continue;
          count++;
          const colour = `rgb(${a.data[i]}, ${a.data[i + 1]}, ${a.data[i + 2]})`;
          colours[colour] = (colours[colour] || 0) + 1;
        }
        const probe = document.body.appendChild(document.createElement('div'));
        probe.style.backgroundColor = 'var(--selection)';
        const token = getComputedStyle(probe).backgroundColor;
        probe.remove();
        const colour = Object.entries(colours).sort((p, q) => q[1] - p[1])[0]?.[0] ?? null,
          levels = value => value.match(/\d+/g).map(Number);
        return {
          painted: Math.round((count / (a.width * a.height)) * 100) / 100,
          colour,
          token,
          off: colour ? Math.max(...levels(colour).map((level, k) => Math.abs(level - levels(token)[k]))) : 255
        };
      }
      const channel = value => {
        const c = value / 255;
        return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
      };
      const luminance = (data, i) =>
        0.2126 * channel(data[i]) + 0.7152 * channel(data[i + 1]) + 0.0722 * channel(data[i + 2]);
      let best = {coverage: 0, contrast: 1, column: -1};
      for (let x = 0; x < a.width; x++) {
        const ratios = [];
        for (let y = 0; y < a.height; y++) {
          const i = (y * a.width + x) * 4;
          if (!changed(i)) continue;
          const [l1, l2] = [luminance(a.data, i), luminance(b.data, i)];
          ratios.push((Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05));
        }
        ratios.sort((p, q) => p - q);
        const column = {
          coverage: ratios.length / a.height,
          contrast: ratios.length ? ratios[Math.floor(ratios.length / 2)] : 1,
          column: x
        };
        if (column.coverage * column.contrast > best.coverage * best.contrast) best = column;
      }
      return {
        coverage: Math.round(best.coverage * 100) / 100,
        contrast: Math.round(best.contrast * 10) / 10,
        offset: best.column - a.width / 2
      };
    },
    [kind, shown.toString('base64'), hidden.toString('base64')]
  );

// How visible the caret at offset is: the best of several samples over more
// than one blink, as {coverage, contrast}, with what the page says about it.
async function caretVisibility(page, at) {
  const box = await caretAt(page, at);
  if (!box) return {error: 'no coordinates'};
  const clip = caretClip(box);
  await style(page, 'carets', noCarets);
  const without = await page.screenshot({clip, caret: 'hide'});
  await style(page, 'carets', null);
  let best = {coverage: 0, contrast: 1};
  for (let started = Date.now(); Date.now() - started < 1600; ) {
    const sample = await compare(page, 'caret', await page.screenshot({clip, caret: 'initial'}), without);
    if (sample.coverage * sample.contrast > best.coverage * best.contrast) best = sample;
    if (best.coverage >= 0.6 && best.contrast >= 3) break;
    await page.waitForTimeout(120);
  }
  const why = await page.evaluate(() => {
    const content = document.querySelector('#editor'),
      cursor = document.querySelector('#note-editor .cm-cursor');
    return {
      caretColor: getComputedStyle(content).caretColor,
      drawn: cursor
        ? {width: cursor.getBoundingClientRect().width, color: getComputedStyle(cursor).borderLeftColor}
        : null
    };
  });
  return {...best, ...why, focus: await editorState(page)};
}

// Every place in the note where the caret cannot be seen, with the evidence.
async function invisibleCarets(page) {
  const failures = {};
  for (const [name, at] of Object.entries(places)) {
    const seen = await caretVisibility(page, at);
    if (!(seen.coverage >= 0.6 && seen.contrast >= 3)) failures[name] = seen;
  }
  return failures;
}

// Selects text (the first occurrence of needle) and returns how the
// selection is painted over it, with the carets hidden.
async function selectionPaint(page, needle) {
  const from = note.indexOf(needle),
    to = from + needle.length;
  const box = await page.evaluate(
    async ([from, to]) => {
      const view = document.querySelector('#editor').cmTile.root.view;
      view.dispatch({
        selection: {anchor: from, head: to},
        effects: view.constructor.scrollIntoView(from, {y: 'center'}),
        userEvent: 'select'
      });
      for (let i = 0; i < 3; i++) await new Promise(requestAnimationFrame);
      const start = view.coordsAtPos(from, 1),
        end = view.coordsAtPos(to, -1);
      return {x: start.left, y: start.top, width: end.right - start.left, height: start.bottom - start.top};
    },
    [from, to]
  );
  const clip = {
    x: Math.round(box.x) + 1,
    y: Math.round(box.y) + 1,
    width: Math.round(box.width) - 2,
    height: Math.round(box.height) - 2
  };
  await style(page, 'carets', noCarets);
  await style(page, 'selection', noSelection);
  const hidden = await page.screenshot({clip, caret: 'hide'});
  await style(page, 'selection', null);
  const shown = await page.screenshot({clip, caret: 'hide'});
  await style(page, 'carets', null);
  return compare(page, 'selection', shown, hidden);
}

// Every selection that is not painted in the app's selection colour: over
// plain text and over a code block's background. The browser's own
// selection, painted over the text, is all but opaque, so it can be a level
// off; WebKit's darkened one is more than 30 levels off in the dark theme,
// and CodeMirror's own greys more than 20.
async function unpaintedSelections(page) {
  const failures = {};
  for (const needle of ['Plain words', 'code block']) {
    const paint = await selectionPaint(page, needle);
    if (!(paint.painted >= 0.5 && paint.off <= 4)) failures[needle] = paint;
  }
  return failures;
}

// How long the caret holds still after a key, and then each half of a blink
// (BLINK in frontend/editor-caret.mjs).
const BLINK = 530;

// Starts a record of when the caret is shown and hidden (the computed
// visibility of its cursor layer, read at each change of the layer's
// attributes) and of the time of the last key, on the page's clock.
const watchCaret = page =>
  page.evaluate(() => {
    const layer = document.querySelector('#note-editor .cm-cursorLayer'),
      shown = () => getComputedStyle(layer).visibility !== 'hidden',
      watch = (window.caretWatch ||= {});
    watch.observer?.disconnect();
    if (!watch.keys) {
      watch.keys = () => (watch.key = performance.now());
      addEventListener('keydown', watch.keys, true);
    }
    watch.key = null;
    watch.flips = [{at: performance.now(), shown: shown()}];
    watch.observer = new MutationObserver(() => {
      if (shown() !== watch.flips.at(-1).shown) watch.flips.push({at: performance.now(), shown: shown()});
    });
    watch.observer.observe(layer, {attributes: true});
  });
// Waits until ms have passed since the last key.
const sinceKey = (page, ms) => page.waitForFunction(ms => performance.now() - caretWatch.key >= ms, ms, {polling: 50});
// The record since the last key: whether the caret showed at the key (by
// 50 ms after it), when it was first hidden after it, the halves of the blink
// from then on (ms), and the whole record for a failure's message.
const caretRecord = page =>
  page.evaluate(() => {
    const {key, flips} = caretWatch,
      after = flips.filter(flip => flip.at > key).map(flip => ({after: Math.round(flip.at - key), shown: flip.shown})),
      atKey = [{after: 0, shown: flips.filter(flip => flip.at <= key).at(-1).shown}, ...after]
        .filter(flip => flip.after <= 50)
        .at(-1).shown,
      hidden = after.findIndex(flip => !flip.shown);
    return {
      shownAtKey: atKey,
      firstHidden: hidden < 0 ? null : after[hidden].after,
      halves: hidden < 0 ? [] : after.slice(hidden + 1).map((flip, i) => flip.after - after[hidden + i].after),
      flips: after
    };
  });
// Whether a record shows a caret that is shown at the key, holds still for
// BLINK, and then is hidden and shown in turn, with at least `halves` whole
// halves of a blink. Timers can run late on a loaded machine, never early.
const blinksAfterKey = (record, halves) => ({
  shownAtKey: record.shownAtKey,
  steady: record.firstHidden === null || record.firstHidden >= BLINK - 30,
  blinks:
    record.firstHidden !== null &&
    record.firstHidden <= BLINK + 470 &&
    record.halves.length >= halves &&
    record.halves.every(half => half >= BLINK - 130 && half <= BLINK + 470)
});

// Waits (at most 5 s) for the caret, blinking since the last key, to be in
// the phase where it is shown (true) or hidden (false), and returns how many
// times it has flipped.
async function blinkTo(page, shown) {
  const flips = await page
    .waitForFunction(
      shown =>
        caretWatch.flips.some(flip => flip.at > caretWatch.key && !flip.shown) &&
        caretWatch.flips.at(-1).shown === shown &&
        caretWatch.flips.length,
      shown,
      {polling: 20, timeout: 5000}
    )
    .then(
      handle => handle.jsonValue(),
      () => null
    );
  if (flips === null)
    throw Error(
      `the caret was not ${shown ? 'shown again' : 'hidden'} within 5 s of a key: ${JSON.stringify(await caretRecord(page))}`
    );
  return flips;
}
// Screenshots the caret's place while it blinks, in its next phase where it
// is shown (true) or hidden (false); a shot that a flip interrupted is taken
// again.
async function blinkPhase(page, clip, shown) {
  for (let tries = 0; tries < 4; tries++) {
    const flips = await blinkTo(page, shown);
    const shot = await page.screenshot({clip, caret: 'initial'});
    if ((await page.evaluate(() => caretWatch.flips.length)) === flips) return shot;
  }
  throw Error('a flip of the caret interrupted every screenshot of its ' + (shown ? 'shown' : 'hidden') + ' phase');
}

// The caret's blink at a key in the middle of a line. After typing it holds
// still, then blinks: hidden, nothing of it shows; shown, it shows at 3:1 or
// more. A key while it is hidden shows it at once, and it holds still again.
// Without the focus it is not drawn, and nothing blinks. No animation runs.
async function expectBlink(page) {
  await caretAt(page, places['the middle of a line']);
  await watchCaret(page);
  await page.keyboard.press('x');
  const clip = caretClip(await caretAt(page, null));
  const hidden = await blinkPhase(page, clip, false),
    shown = await blinkPhase(page, clip, true);
  await sinceKey(page, 2400);
  const typed = await caretRecord(page);
  expect.soft(blinksAfterKey(typed, 2), 'after typing ' + JSON.stringify(typed)).toEqual({
    shownAtKey: true,
    steady: true,
    blinks: true
  });
  expect.soft(await animations(page)).toEqual([]);
  // A key while the caret is hidden.
  await blinkTo(page, false);
  await watchCaret(page);
  await page.keyboard.press('ArrowLeft');
  await sinceKey(page, BLINK + 700);
  const moved = await caretRecord(page);
  expect.soft(blinksAfterKey(moved, 0), 'after a key while hidden ' + JSON.stringify(moved)).toEqual({
    shownAtKey: true,
    steady: true,
    blinks: true
  });
  await style(page, 'carets', noCarets);
  const without = await page.screenshot({clip, caret: 'hide'});
  await style(page, 'carets', null);
  const off = await compare(page, 'caret', hidden, without),
    on = await compare(page, 'caret', shown, without);
  expect
    .soft({hidden: off.coverage <= 0.1, shown: on.coverage >= 0.6 && on.contrast >= 3}, JSON.stringify({off, on}))
    .toEqual({
      hidden: true,
      shown: true
    });
  // Without the focus.
  await page.evaluate(() => document.activeElement.blur());
  await watchCaret(page);
  await page.waitForTimeout(3 * BLINK);
  const unfocused = await page.evaluate(() => ({
    focused: document.querySelector('#editor').cmTile.root.view.hasFocus,
    flips: caretWatch.flips.length - 1,
    drawn: [...document.querySelectorAll('#note-editor .cm-cursor')].filter(
      cursor => getComputedStyle(cursor).display !== 'none'
    ).length
  }));
  expect(unfocused).toEqual({focused: false, flips: 0, drawn: 0});
}

// The caret under reduced motion: after a key and a pause as long as two
// blinks it has never been hidden, and it shows at 3:1 or more.
async function expectSteady(page) {
  await caretAt(page, places['the middle of a line']);
  await watchCaret(page);
  await page.keyboard.press('x');
  await sinceKey(page, 2400);
  const record = await caretRecord(page);
  expect.soft(record).toEqual({shownAtKey: true, firstHidden: null, halves: [], flips: []});
  expect.soft(await animations(page)).toEqual([]);
  const seen = await caretVisibility(page, null);
  expect(seen.coverage >= 0.6 && seen.contrast >= 3, JSON.stringify(seen)).toBe(true);
}

// Defines every case in the file's browser engine, named in each title
// (editor-caret.spec.mjs runs them in Chromium, editor-caret-webkit.spec.mjs
// in WebKit, Safari's engine). A title names the engine, theme and device:
// reports and the release gate tell cases apart by file and title alone, and
// both files' cases are defined in this one.
export function caretCases(engine) {
  // Each case samples sixteen places, more slowly when a caret is missing,
  // and watches the caret blink for a few seconds.
  test.describe.configure({timeout: 150000});
  test.beforeAll(async () => {
    app = await startApp({
      name: 'forgetmenote-caret-',
      account: {username: 'caret-admin', password: 'DisposableCaret123!'}
    });
  });
  test.afterAll(async () => {
    await app?.close();
  });
  test.beforeEach(async ({page}) => {
    page.on('pageerror', error => {
      throw error;
    });
    await app.login(page, 'recovery');
  });
  for (const theme of ['light', 'dark']) {
    test.describe(() => {
      test.use({viewport: {width: 1280, height: 800}});
      const where = `(${engine}, ${theme} theme, desktop)`;
      for (const format of ['visual', 'source', 'split']) {
        test(`the caret is visible everywhere and blinks in ${format} ${where}`, async ({page}, testInfo) => {
          await open(page, testInfo, {theme, format});
          expect.soft(await invisibleCarets(page)).toEqual({});
          expect.soft(await unpaintedSelections(page)).toEqual({});
          await expectBlink(page);
        });
      }
      test(`the caret is visible everywhere and blinks in focus mode ${where}`, async ({page}, testInfo) => {
        await open(page, testInfo, {theme, format: 'visual', focus: true});
        expect.soft(await invisibleCarets(page)).toEqual({});
        expect.soft(await unpaintedSelections(page)).toEqual({});
        await expectBlink(page);
      });
    });
    test.describe(() => {
      test.use({viewport: {width: 390, height: 844}, hasTouch: true, isMobile: true, deviceScaleFactor: 3});
      const where = `(${engine}, ${theme} theme, phone)`;
      for (const format of ['visual', 'source']) {
        test(`the caret is visible everywhere and blinks in ${format} ${where}`, async ({page}, testInfo) => {
          await open(page, testInfo, {theme, format, touch: true});
          expect.soft(await invisibleCarets(page)).toEqual({});
          expect.soft(await unpaintedSelections(page)).toEqual({});
          await expectBlink(page);
        });
      }
    });
  }
  for (const [device, options] of [
    ['desktop', {viewport: {width: 1280, height: 800}}],
    ['phone', {viewport: {width: 390, height: 844}, hasTouch: true, isMobile: true, deviceScaleFactor: 3}]
  ])
    test.describe(() => {
      test.use({...options, reducedMotion: 'reduce'});
      test(`the caret holds still when the system reduces motion (${engine}, ${device})`, async ({page}, testInfo) => {
        await open(page, testInfo, {theme: 'dark', format: 'visual', touch: device === 'phone'});
        await expectSteady(page);
      });
    });
}
