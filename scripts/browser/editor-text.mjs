// The note editor's Markdown and selection, for specs. Every mode is one
// CodeMirror view (#editor is its content element), so there is no form
// field to read: the editor publishes its text on #editing, and the selection
// is read and set through the DOM selection CodeMirror keeps in sync with its
// own. In Visual the DOM omits hidden markers, so offsets read this way match
// the text only on lines without markup.
import {expect} from '@playwright/test';

export const editorText = page => page.evaluate(() => document.getElementById('editing').markdown);

export async function expectEditorText(page, expected, options) {
  const poll = expect.poll(() => editorText(page), options);
  if (expected instanceof RegExp) await poll.toMatch(expected);
  else await poll.toBe(expected);
}

// Runs in the page: maps the Source DOM (every line of a short note) to text
// offsets, then reads or sets the selection.
function sourceSelectionProbe({select}) {
  const content = document.querySelector('#note-editor .cm-content');
  if (!content) return null;
  const nodes = [];
  let at = 0;
  [...content.querySelectorAll('.cm-line')].forEach((line, index) => {
    if (index) at++;
    const start = at,
      walker = document.createTreeWalker(line, NodeFilter.SHOW_TEXT, {
        acceptNode: node =>
          node.parentElement.closest('[contenteditable=false]') ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT
      });
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      nodes.push({node, start: at});
      at += node.data.length;
    }
    nodes.push({line, start, end: at});
  });
  const selection = getSelection();
  if (select) {
    const point = offset => {
      const text = nodes.find(
        item => item.node && item.start <= offset && offset <= item.start + item.node.data.length
      );
      if (text) return [text.node, offset - text.start];
      const line = nodes.find(item => item.line && item.start <= offset && offset <= item.end);
      return [line.line, 0];
    };
    const [anchor, head] = select.direction === 'backward' ? [select.to, select.from] : [select.from, select.to];
    selection.setBaseAndExtent(...point(anchor), ...point(head));
    return null;
  }
  const offset = (node, at) => {
    const text = nodes.find(item => item.node === node);
    if (text) return text.start + at;
    const line = nodes.find(item => item.line && (item.line === node || item.line.contains(node)));
    return line ? (at ? line.end : line.start) : null;
  };
  const anchor = offset(selection.anchorNode, selection.anchorOffset),
    head = offset(selection.focusNode, selection.focusOffset);
  if (anchor === null || head === null) return null;
  return [
    Math.min(anchor, head),
    Math.max(anchor, head),
    anchor === head ? 'none' : head < anchor ? 'backward' : 'forward'
  ];
}

// [start, end, direction] of the Source selection.
export const sourceSelection = page => page.evaluate(sourceSelectionProbe, {});

// CodeMirror writes its own selection into the DOM 10 ms after it gains
// focus; a selection set before then is lost.
async function focusSource(page) {
  const editor = page.locator('#editor');
  if (await editor.evaluate(element => element !== document.activeElement)) {
    await editor.focus();
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  }
  return editor;
}

// Replaces the whole Source text, as typing over a full selection does.
export async function fillSource(page, text) {
  await (await focusSource(page)).fill(text);
}

export async function selectSource(page, from, to = from, direction = 'forward') {
  await focusSource(page);
  await page.evaluate(sourceSelectionProbe, {select: {from, to, direction}});
  await expect.poll(() => sourceSelection(page)).toEqual([from, to, from === to ? 'none' : direction]);
}

// Visual mode is shown: the live preview, labelled as the note's content.
export async function expectVisual(page) {
  await expect(page.locator('#editing')).toHaveAttribute('data-format', 'visual');
  await expect(page.locator('#format-visual')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  await expect(page.locator('#editor-hint')).toBeHidden();
}

// The editor line showing `text`, in any mode.
export const editorLine = (page, text) => page.locator('#editor .cm-line').filter({hasText: text}).first();

// CodeMirror draws only the lines near the screen: scrolls down until the
// line showing `text` is drawn, then brings it on screen.
export async function revealLine(page, text) {
  const line = editorLine(page, text);
  for (let i = 0; i < 60 && !(await line.count()); i++) {
    const box = await page.locator('#editor').boundingBox();
    await page.mouse.move(box.x + box.width / 2, Math.min(box.y + box.height / 2, 400));
    await page.mouse.wheel(0, 800);
    await page.waitForTimeout(50);
  }
  await line.scrollIntoViewIfNeeded();
  return line;
}

// Puts the caret at the end of the editor line showing `text`, as a reader
// does: a click just after its last character, then End.
export async function caretAtLineEnd(page, text) {
  const line = await revealLine(page, text);
  const point = await line.evaluate(element => {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    let last = null;
    for (let node = walker.nextNode(); node; node = walker.nextNode()) if (node.data.length) last = node;
    const range = document.createRange();
    range.setStart(last, last.data.length - 1);
    range.setEnd(last, last.data.length);
    const rect = range.getBoundingClientRect();
    return {x: rect.right - 1, y: rect.top + rect.height / 2};
  });
  await page.mouse.click(point.x, point.y);
  await page.keyboard.press('End');
}

// Counts the live updates this page sends (POST /api/live).
export function countLiveUpdates(page) {
  const sent = [];
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/api/live')) sent.push(request.url());
  });
  return sent;
}
