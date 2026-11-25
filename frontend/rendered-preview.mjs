import {Compartment, StateEffect} from '@codemirror/state';
import {EditorView} from '@codemirror/view';
import {syntaxTree} from '@codemirror/language';
import {GFM, parser as markdownParser} from '@lezer/markdown';
import {fromMarkdown} from 'mdast-util-from-markdown';
import {gfmAutolinkLiteral} from 'micromark-extension-gfm-autolink-literal';
import {gfmStrikethrough} from 'micromark-extension-gfm-strikethrough';
import {gfmTable} from 'micromark-extension-gfm-table';
import {gfmTaskListItem} from 'micromark-extension-gfm-task-list-item';
import {gfmAutolinkLiteralFromMarkdown} from 'mdast-util-gfm-autolink-literal';
import {gfmStrikethroughFromMarkdown} from 'mdast-util-gfm-strikethrough';
import {gfmTableFromMarkdown} from 'mdast-util-gfm-table';
import {gfmTaskListItemFromMarkdown} from 'mdast-util-gfm-task-list-item';
import {normalizeIdentifier} from 'micromark-util-normalize-identifier';
import {normalizeUri} from 'micromark-util-sanitize-uri';
import {enhanceTables} from '../internal/web/ui/tables.mjs';
import {SpaceRun, ensureTreeIdle, topBlocks} from './markdown-syntax.mjs';

// The rendered note in the browser: Side by side's read-only pane, and the
// preview of private drafts (new, unsaved or historical notes), which never
// leave the browser. Read stays rendered on the server (goldmark with GFM,
// then bluemonday); this renderer follows it, and the render corpus
// (internal/webcore/testdata/render-corpus) pins the two together.
//
// Markdown is parsed by micromark (CommonMark with the GFM pieces goldmark
// has: tables, strikethrough, autolink literals and task lists; no footnotes)
// one top-level block at a time, after the note's link definitions the block
// uses, so [text][label] resolves across the note as Read resolves it. The
// result is an allowlisted element tree built from text, never parsed HTML:
// raw HTML is left out as Read leaves it out, links keep only http, https,
// mailto and relative addresses, and view blocks stay code.

// ---------------------------------------------------------------------------
// Rendering Markdown to elements ({tag, attrs, children}) and text (strings).

const parseOptions = {
  extensions: [gfmAutolinkLiteral(), gfmStrikethrough(), gfmTable(), gfmTaskListItem()],
  mdastExtensions: [gfmAutolinkLiteralFromMarkdown(), gfmStrikethroughFromMarkdown(), gfmTableFromMarkdown(), gfmTaskListItemFromMarkdown()],
};

// An address Read keeps: http, https, mailto or relative, percent-encoded as
// goldmark writes it (a % that does not start a hex escape is %25, where
// micromark keeps any two letters or digits after it); null for every other
// scheme (javascript:, data:, …), which the server's sanitizer removes.
export function safeURL(url) {
  const value = normalizeUri(url || '').replace(/%(?![0-9A-Fa-f]{2})/g, '%25');
  if (!value) return null;
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(value);
  return scheme && !/^(?:https?|mailto)$/i.test(scheme[1]) ? null : value;
}

// The output's size, counted as escaped HTML, so a few definitions used many
// times cannot expand into an unbounded page.
export class Budget {
  constructor(limit) {
    this.left = limit;
  }
  charge(size) {
    this.left -= size;
    if (this.left < 0) throw new RangeError('The note is too large to render here.');
  }
}
const specials = /[&<>"]/g;
const escapedSize = value => value.length + 5 * (value.match(specials)?.length || 0);

// Trims here walk the string once from each end: a regular expression for
// trailing spaces tries every start in a run of them, which costs the square
// of its length (seconds for a heading with tens of thousands of spaces).
const blank = char => char === ' ' || char === '\t';
const asciiSpace = char => blank(char) || char === '\n' || char === '\v' || char === '\f' || char === '\r';

// Heading ids as goldmark makes them (parser.WithAutoHeadingID, prefixed
// with webcore.HeadingIDPrefix): from the heading's last source line, ASCII
// letters and digits lowercased, spaces, '-' and '_' as '-', everything else
// dropped, then made unique in the note with -1, -2, ….
export class HeadingIDs {
  constructor() {
    this.seen = new Set();
  }
  id(line) {
    let slug = '', from = 0, to = line.length;
    while (from < to && asciiSpace(line[from])) from++;
    while (to > from && asciiSpace(line[to - 1])) to--;
    for (const char of line.slice(from, to)) {
      if (/^[A-Za-z0-9]$/.test(char)) slug += char.toLowerCase();
      else if (/^[ \t\n\v\f\r_-]$/.test(char)) slug += '-';
    }
    slug ||= 'heading';
    let id = slug;
    for (let n = 1; this.seen.has(id); n++) id = slug + '-' + n;
    this.seen.add(id);
    return 'h-' + id;
  }
}
// The source goldmark makes a heading's id from: an ATX heading's text
// without its # marks (a closing run of # counts when a space or tab, or
// nothing, comes before it), or a setext heading's last line.
export function headingLine(raw) {
  if (!raw.includes('\n')) {
    const text = raw.replace(/^ {0,3}#{1,6}/, '');
    let end = text.length;
    while (end > 0 && blank(text[end - 1])) end--;
    let marks = end;
    while (marks > 0 && text[marks - 1] === '#') marks--;
    let start = marks;
    while (start > 0 && blank(text[start - 1])) start--;
    return marks < end && (start < marks || marks === 0) ? text.slice(0, start) : text;
  }
  const lines = raw.split('\n');
  lines.pop();
  return lines.length > 1 ? lines.at(-1).replace(/^[ \t>]*/, '') : lines[0];
}

function element(cx, tag, attrs = null, children = []) {
  if (cx.budget) {
    let size = 2 * tag.length + 5;
    for (const key in attrs) size += key.length + 4 + 6 * attrs[key].length;
    cx.budget.charge(size);
  }
  return {tag, attrs, children};
}
// Line ends in text are \n, as the browser reads them from HTML, so a
// CRLF note's paragraphs and code look as Read shows them.
function text(cx, value) {
  if (value.includes('\r')) value = value.replace(/\r\n?/g, '\n');
  cx.budget?.charge(escapedSize(value));
  return value;
}
function blocks(nodes, cx, tight) {
  const out = [];
  for (const node of nodes) {
    if (tight && node.type === 'paragraph') out.push(...inline(node.children, cx));
    else out.push(...block(node, cx));
  }
  return out;
}
function block(node, cx) {
  switch (node.type) {
    case 'paragraph':
      return [element(cx, 'p', null, inline(node.children, cx))];
    case 'heading': {
      const {start, end} = node.position;
      const attrs = cx.ids ? {id: cx.ids.id(headingLine(cx.source.slice(start.offset, end.offset)))} : null;
      return [element(cx, 'h' + node.depth, attrs, inline(node.children, cx))];
    }
    case 'thematicBreak':
      return [element(cx, 'hr')];
    case 'blockquote':
      return [element(cx, 'blockquote', null, blocks(node.children, cx, false))];
    case 'list': {
      // Tight lists show their paragraphs without <p>, as CommonMark says.
      // The server's sanitizer drops <ol start> and task checkboxes, so Read
      // shows neither.
      const loose = node.spread || node.children.some(item => item.spread);
      return [element(cx, node.ordered ? 'ol' : 'ul', null, node.children.map(item => element(cx, 'li', null, blocks(item.children, cx, !loose))))];
    }
    case 'code':
      // Only a view block keeps its language: the sanitizer allows no other.
      return [element(cx, 'pre', null, [element(cx, 'code', node.lang === 'view' ? {class: 'language-view'} : null, node.value ? [text(cx, node.value + '\n')] : [])])];
    case 'table': {
      const [head, ...rows] = node.children, columns = head.children.length;
      const row = (source, cell) => element(cx, 'tr', null, Array.from({length: columns}, (_, i) => element(cx, cell, null, source.children[i] ? inline(source.children[i].children, cx) : [])));
      const parts = [element(cx, 'thead', null, [row(head, 'th')])];
      if (rows.length) parts.push(element(cx, 'tbody', null, rows.map(source => row(source, 'td'))));
      return [element(cx, 'table', null, parts)];
    }
    case 'html':
    case 'definition':
      return [];
    default:
      return node.children ? blocks(node.children, cx, false) : [];
  }
}
function inline(nodes, cx) {
  const out = [];
  for (const node of nodes) {
    switch (node.type) {
      case 'text':
        out.push(text(cx, node.value));
        break;
      case 'emphasis':
        out.push(element(cx, 'em', null, inline(node.children, cx)));
        break;
      case 'strong':
        out.push(element(cx, 'strong', null, inline(node.children, cx)));
        break;
      case 'delete':
        out.push(element(cx, 'del', null, inline(node.children, cx)));
        break;
      case 'inlineCode':
        out.push(element(cx, 'code', null, [text(cx, node.value)]));
        break;
      case 'break':
        out.push(element(cx, 'br'), text(cx, '\n'));
        break;
      case 'link':
        out.push(...link(node.url, node.title, node.children, cx));
        break;
      case 'linkReference': {
        const definition = cx.definitions.get(node.identifier);
        out.push(...(definition ? link(definition.url, definition.title, node.children, cx) : inline(node.children, cx)));
        break;
      }
      case 'image':
        out.push(image(node.url, node.alt, node.title, cx));
        break;
      case 'imageReference': {
        const definition = cx.definitions.get(node.identifier);
        out.push(definition ? image(definition.url, node.alt, definition.title, cx) : text(cx, node.alt || ''));
        break;
      }
      case 'html':
        break;
      default:
        if (node.children) out.push(...inline(node.children, cx));
        else if (typeof node.value === 'string') out.push(text(cx, node.value));
    }
  }
  return out;
}
// A link to an address Read drops shows as its text, as the sanitizer
// leaves it.
function link(url, title, children, cx) {
  const href = safeURL(url), content = inline(children, cx);
  if (href == null) return content;
  return [element(cx, 'a', title ? {href, title} : {href}, content)];
}
function image(url, alt, title, cx) {
  const src = safeURL(url), attrs = src == null ? {alt: alt || ''} : {src, alt: alt || ''};
  if (title) attrs.title = title;
  return element(cx, 'img', attrs);
}

// Renders Markdown to elements and text. `ids` numbers headings across calls
// (a HeadingIDs, or null for no ids); `budget` bounds the output.
export function renderMarkdown(source, {ids = null, budget = null} = {}) {
  const tree = fromMarkdown(source, parseOptions), definitions = new Map();
  (function collect(nodes) {
    for (const node of nodes) {
      if (node.type === 'definition') {
        if (!definitions.has(node.identifier)) definitions.set(node.identifier, node);
      } else if (node.type === 'blockquote' || node.type === 'list' || node.type === 'listItem') collect(node.children);
    }
  })(tree.children);
  return blocks(tree.children, {source, ids, budget, definitions}, false);
}

// HTML of rendered elements. Images name their address in `imageAttribute`:
// data-fmn-src keeps a page that is not shown yet from fetching anything
// before the reader resolves it.
const escapeText = value => value.replace(/[&<>]/g, char => (char === '&' ? '&amp;' : char === '<' ? '&lt;' : '&gt;'));
const escapeAttribute = value => value.replace(specials, char => (char === '"' ? '&quot;' : char === '&' ? '&amp;' : char === '<' ? '&lt;' : '&gt;'));
const voids = new Set(['br', 'hr', 'img']);
export function toHTML(nodes, {imageAttribute = 'src'} = {}) {
  const out = [];
  const walk = list => {
    for (const node of list) {
      if (typeof node === 'string') {
        out.push(escapeText(node));
        continue;
      }
      out.push('<', node.tag);
      for (const key in node.attrs) out.push(' ', node.tag === 'img' && key === 'src' ? imageAttribute : key, '="', escapeAttribute(node.attrs[key]), '"');
      out.push('>');
      if (voids.has(node.tag)) continue;
      walk(node.children);
      out.push('</', node.tag, '>');
    }
  };
  walk(nodes);
  return out.join('');
}

// ---------------------------------------------------------------------------
// Blocks and the note's link definitions.

// Where the note's body starts after front matter, as the server's
// workspace.MarkdownBody reads it: a "---" line, then a later line that is
// only "---", every line before it ending within 64 KiB. Front matter that
// does not close is body.
export function bodyStart(text) {
  const open = text.startsWith('---\r\n') ? 5 : text.startsWith('---\n') ? 4 : 0;
  let bytes = open;
  for (let at = open; open && at < text.length;) {
    const newline = text.indexOf('\n', at), end = newline < 0 ? text.length : newline + 1, line = text.slice(at, end);
    if (line.replace(/[\r\n]+$/, '') === '---') return end;
    bytes += utf8Length(line);
    if (bytes > 65536) break;
    at = end;
  }
  return 0;
}
// UTF-8 bytes of a string, as the server receives it: 4 for a character
// outside the BMP (a surrogate pair), 3 for a lone surrogate (sent as U+FFFD).
function utf8Length(value) {
  let bytes = value.length;
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff && (value.charCodeAt(i + 1) & 0xfc00) === 0xdc00) {
      bytes += 2;
      i++;
    } else if (code >= 0x800) bytes += 2;
    else if (code >= 0x80) bytes += 1;
  }
  return bytes;
}

// Container blocks whose children can hold link definitions and headings.
const containers = new Set(['Document', 'Blockquote', 'BulletList', 'OrderedList', 'ListItem']);
// The note's link definitions in order, each {label, text}, from a Lezer
// Markdown tree whose text `read(from, to)` gives. Only a label's first
// definition counts. A definition's text leaves out the quote marks of the
// blockquotes it sits in, so it reads the same at the top of a block's
// Markdown (a list item's indent does not change it).
export function treeDefinitions(tree, read) {
  const list = [], seen = new Set();
  const unquoted = (from, to) => {
    let text = '', at = from;
    tree.iterate({
      from,
      to,
      enter(node) {
        if (node.name !== 'QuoteMark' || node.from < from || node.to > to) return;
        text += read(at, node.from);
        at = node.to;
      },
    });
    return text + read(at, to);
  };
  tree.iterate({
    enter(node) {
      if (node.name === 'LinkReference') {
        const label = node.node.getChild('LinkLabel');
        if (label) {
          const key = normalizeIdentifier(unquoted(label.from + 1, label.to - 1)).toLowerCase();
          if (!seen.has(key)) {
            seen.add(key);
            list.push({label: key, text: unquoted(node.from, node.to)});
          }
        }
        return false;
      }
      return containers.has(node.name);
    },
  });
  return list;
}
const bracketed = /\[((?:[^\\[\]]|\\[^])+)\]/g;
// The Markdown a block renders from: the definitions its brackets may name,
// then its own text, so a reference resolves to the note's first definition
// wherever that is.
export function blockSource(text, definitions) {
  if (!definitions.length || !text.includes('[')) return text;
  const labels = new Set();
  for (const match of text.matchAll(bracketed)) labels.add(normalizeIdentifier(match[1]).toLowerCase());
  const used = definitions.filter(definition => labels.has(definition.label));
  return used.length ? used.map(definition => definition.text).join('\n') + '\n\n' + text : text;
}

// Lezer ends a <textarea> HTML block at a blank line (its raw tags are only
// script, pre and style), where CommonMark and goldmark go on to the line
// with </textarea>, or to the end of the note. A paragraph or HTML block with
// a <textarea> line left open takes the blocks after it, up to the one that
// closes it, so micromark reads them together as Read does. Callers first
// check that the note has a <textarea> line at all, which is cheap.
const textareaOpen = /(?:^|\n) {0,3}<textarea(?:[ \t\r>]|\n|$)/gi, textareaClose = /<\/textarea>/i;
const textareaAnywhere = new RegExp(textareaOpen.source, 'i'), textareaLine = /^ {0,3}<textarea(?:[ \t\r>]|$)/i, textareaChecked = new WeakMap();
// Whether a CodeMirror Text has a line that opens a <textarea>, remembered
// for each version of the note.
function hasTextarea(doc) {
  let found = textareaChecked.get(doc);
  if (found === undefined) {
    found = false;
    for (const iter = doc.iterLines(); !iter.next().done;) {
      if (iter.value.includes('<') && textareaLine.test(iter.value)) {
        found = true;
        break;
      }
    }
    textareaChecked.set(doc, found);
  }
  return found;
}
const leavesOpen = text => {
  let last = -1;
  for (const match of text.matchAll(textareaOpen)) last = match.index;
  return last >= 0 && !textareaClose.test(text.slice(last));
};
export function joinRaw(blocks, read) {
  const out = [];
  for (let i = 0; i < blocks.length; i++) {
    const block = blocks[i];
    if ((block.name === 'HTMLBlock' || block.name === 'Paragraph') && leavesOpen(read(block.from, block.to))) {
      let last = i;
      while (last + 1 < blocks.length && blocks[last + 1].name !== 'Rest') {
        last++;
        if (textareaClose.test(read(blocks[last].from, blocks[last].to))) break;
      }
      out.push({...block, to: blocks[last].to});
      i = last;
    } else out.push(block);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Private drafts: the whole note as HTML for the reader's installer, built in
// slices so a long draft does not block the page while it renders.

const blockParser = markdownParser.configure([GFM, SpaceRun]);
const now = () => performance.now();
const pause = () => new Promise(resolve => setTimeout(resolve, 0));

// The HTML Read would show for a private draft, at most `limit` characters
// of it, working in slices of about `slice` ms. Throws a RangeError for a
// draft that would render larger; the caller shows it as plain text. A
// render `cancelled()` calls superseded stops at its next pause and resolves
// null.
export async function renderPrivateMarkdown(markdown, {limit = 4 << 20, slice = 8, cancelled = () => false} = {}) {
  const body = markdown.slice(bodyStart(markdown));
  const parse = blockParser.startParse(body);
  let tree, started = now();
  // Resolves true when the render is cancelled.
  const yieldNow = async () => {
    if (now() - started < slice) return false;
    await pause();
    started = now();
    return cancelled();
  };
  while (!(tree = parse.advance())) if (await yieldNow()) return null;
  const read = (from, to) => body.slice(from, to), definitions = treeDefinitions(tree, read);
  const budget = new Budget(limit), ids = new HeadingIDs(), html = [];
  let lines = topBlocks(tree, 0, body.length).map(({name, from, to}) => {
    const end = body.indexOf('\n', to);
    return {name, from: body.lastIndexOf('\n', from - 1) + 1, to: end < 0 ? body.length : end};
  });
  if (textareaAnywhere.test(body)) lines = joinRaw(lines, read);
  for (const {from, to} of lines) {
    html.push(toHTML(renderMarkdown(blockSource(read(from, to), definitions), {ids, budget}), {imageAttribute: 'data-fmn-src'}));
    if (await yieldNow()) return null;
  }
  return html.join('\n');
}

// ---------------------------------------------------------------------------
// Side by side's pane.
//
// The pane shows the editor's note rendered, one element per top-level block
// of the editor's Lezer tree. Blocks within two screens of the pane's
// viewport are rendered; the others are placeholders as tall as the editor
// shows their source (or as they were when last rendered), so both sides are
// about the same height. (Rendered blocks have no content-visibility: their
// margins meet as Read's do, and on F1 at 4x CPU it made scrolling slower.)
// A keystroke only notes the change: blocks render again about 150 ms after
// typing stops, in idle slices of at most 8 ms, and only those whose Markdown
// changed. The two sides scroll together, driven by whichever side the
// reader last scrolled, typed or clicked in.

const SETTLE = 150, FLING = 120, SLICE = 8, CACHE = 400, BLOCK_LIMIT = 4 << 20;
const idle = typeof requestIdleCallback === 'function' ? callback => requestIdleCallback(callback, {timeout: 500}) : callback => setTimeout(callback, 0);
const allowed = new Set(['p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'blockquote', 'ul', 'ol', 'li', 'pre', 'code', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'em', 'strong', 'del', 'a', 'img', 'br']);
const pageLinks = /\[\[([^[\]|\n]+)(?:\|([^[\]\n]+))?\]\]/g;

// Rendered elements into `parent` with createElement and text only. Links
// and images go through the reader's rules (hooks.link, hooks.image); [[page]]
// links in text become page links, as wiki.linkPages makes them in Read.
export function toDOM(nodes, parent, hooks, plain = false) {
  for (const node of nodes) {
    if (typeof node === 'string') {
      if (plain || !node.includes('[[')) parent.append(node);
      else appendPageLinks(parent, node, hooks);
      continue;
    }
    if (!allowed.has(node.tag)) continue;
    const element = document.createElement(node.tag), attrs = node.attrs || {};
    if (node.tag === 'a') hooks.link(element, attrs.href);
    else if (node.tag === 'img') {
      element.alt = attrs.alt || '';
      element.loading = 'lazy';
      element.decoding = 'async';
      const src = attrs.src && hooks.image(attrs.src);
      if (src) element.src = src;
    } else if (node.tag === 'code' && attrs.class === 'language-view') element.className = 'language-view';
    if (attrs.title && (node.tag === 'a' || node.tag === 'img')) element.title = attrs.title;
    toDOM(node.children, element, hooks, plain || node.tag === 'a' || node.tag === 'code' || node.tag === 'pre');
    parent.append(element);
  }
  return parent;
}
function appendPageLinks(parent, value, hooks) {
  let last = 0;
  for (const match of value.matchAll(pageLinks)) {
    const target = match[1].trim(), label = (match[2] ?? match[1]).trim();
    if (!target || !label) continue;
    parent.append(value.slice(last, match.index));
    const link = document.createElement('a');
    link.className = 'page-link';
    link.dataset.wiki = target;
    link.textContent = label;
    link.tabIndex = 0;
    link.setAttribute('role', 'link');
    if (hooks.missingPage?.(target)) link.classList.add('wiki-link-missing');
    parent.append(link);
    last = match.index + match[0].length;
  }
  parent.append(value.slice(last));
}

// The index of the last item of a sorted list whose key(item) is at most
// `value` (0 when there is none).
export function itemAt(items, value, key) {
  let low = 0, high = items.length - 1;
  while (low < high) {
    const middle = (low + high + 1) >> 1;
    if (key(items[middle]) <= value) low = middle;
    else high = middle - 1;
  }
  return low;
}
// Where `value` falls between top and bottom, from 0 to 1.
export const fraction = (value, top, bottom) => (bottom > top ? Math.min(1, Math.max(0, (value - top) / (bottom - top))) : 0);

// The blocks the pane shows for a tree of `doc` (a CodeMirror Text): its
// top-level blocks after any front matter, on whole lines (an indented code
// block's node starts after its indent) and joined where an open <textarea>
// runs on (joinRaw), and while the tree is partial, one 'Rest' block for the
// text not parsed yet.
export function paneBlocks(tree, doc) {
  const parsed = tree.length >= doc.length;
  let list = [];
  for (const block of topBlocks(tree, 0, doc.length)) {
    if (block.name === 'FrontMatter' || (!parsed && block.to >= tree.length)) continue;
    list.push({name: block.name, from: doc.lineAt(block.from).from, to: doc.lineAt(block.to).to});
  }
  if (hasTextarea(doc)) list = joinRaw(list, (from, to) => doc.sliceString(from, to));
  if (!parsed) list.push({name: 'Rest', from: list.length ? Math.min(doc.length, list.at(-1).to + 1) : 0, to: doc.length});
  return list;
}
// While the tree is partial (after an edit CodeMirror parses only up to a
// little past its viewport), the previous items past the parsed text stand
// for their blocks until the parse reaches them, so an edit does not rebuild
// the rest of a long note.
export function keepUnparsed(blocks, previous) {
  const rest = blocks.at(-1);
  if (rest?.name !== 'Rest') return blocks;
  const tail = previous.filter(item => item.name !== 'Rest' && item.from >= rest.from);
  if (!tail.length) return blocks;
  const gap = tail[0].from > rest.from ? [{name: 'Rest', from: rest.from, to: tail[0].from - 1}] : [];
  return [...blocks.slice(0, -1), ...gap, ...tail.map(({name, from, to}) => ({name, from, to}))];
}
// The pane's items for new blocks: a previous item (already mapped through
// the edit) that matches a block exactly is kept with its element, and the
// other blocks get new items.
export function reconcile(previous, blocks) {
  const next = [];
  let i = 0;
  for (const block of blocks) {
    while (i < previous.length && previous[i].from < block.from) i++;
    const item = previous[i];
    if (item && item.from === block.from && item.to === block.to && item.name === block.name) {
      next.push(item);
      i++;
    } else next.push({name: block.name, from: block.from, to: block.to, element: null, key: null, dirty: true});
  }
  return next;
}

// One editor shows at most one pane at a time.
const paneListener = new Compartment();

class Pane {
  constructor({host, view, imageURL, resolve, open, pageTarget}) {
    Object.assign(this, {host, view, imageURL, resolve, open, pageTarget});
    this.root = document.createElement('div');
    this.root.className = 'rendered-preview prose';
    this.root.setAttribute('role', 'region');
    this.root.setAttribute('aria-label', 'Rendered note');
    this.root.tabIndex = 0;
    host.replaceChildren(this.root);
    this.items = [];
    this.byElement = new WeakMap();
    this.near = new Set();
    this.definitions = [];
    this.definitionsKey = '';
    this.cache = new Map();
    this.changes = null;
    this.driver = 'editor';
    this.hooks = {
      link: (element, href) => this.linkTo(element, href),
      image: src => this.imageURL?.(src) || null,
      missingPage: target => (this.pageTarget ? !this.pageTarget(target) : false),
    };
    this.observer = new IntersectionObserver(entries => this.intersect(entries), {root: this.root, rootMargin: '200% 0px'});
    this.listeners = [];
    const on = (target, type, listener, options) => {
      target.addEventListener(type, listener, options);
      this.listeners.push(() => target.removeEventListener(type, listener, options));
    };
    // Input on either side also ends the pane's hold on its end (see
    // editorFollows).
    for (const type of ['wheel', 'pointerdown', 'touchstart', 'keydown']) {
      on(view.scrollDOM, type, () => this.drive('editor'), {capture: true, passive: true});
      on(this.root, type, () => this.drive('pane'), {capture: true, passive: true});
    }
    on(view.scrollDOM, 'scroll', () => this.driver !== 'pane' && this.follow('pane'), {passive: true});
    on(this.root, 'scroll', () => this.scrolled(), {passive: true});
    on(this.root, 'click', event => this.click(event));
    on(this.root, 'keydown', event => this.key(event));
    // The pane follows the editor's text through its updates; the listener
    // only notes what changed. It is removed with the pane, and the next pane
    // of the same editor reuses its compartment.
    const listener = (this.listener = EditorView.updateListener.of(update => this.update(update)));
    view.dispatch({effects: paneListener.get(view.state) === undefined ? StateEffect.appendConfig.of(paneListener.of(listener)) : paneListener.reconfigure(listener)});
    this.refresh();
  }
  drive(side) {
    this.driver = side;
    this.atEnd = false;
  }
  // A scroll of more than a screen at once (a fast fling, a scrollbar drag,
  // the editor's caret jumping) holds rendering back until the pane slows
  // down: the blocks it flies past are never drawn, and frames stay short.
  scrolled() {
    const top = this.root.scrollTop;
    if (Math.abs(top - (this.lastTop ?? top)) > (this.screen || innerHeight)) this.flying = now();
    this.lastTop = top;
    if (this.driver === 'pane') this.follow('editor');
  }
  update(update) {
    if (this.destroyed) return;
    if (update.docChanged) {
      this.changes = this.changes ? this.changes.compose(update.changes) : update.changes;
      clearTimeout(this.timer);
      this.timer = setTimeout(() => idle(() => this.refresh()), SETTLE);
    }
    // A place the editor scrolls to (the caret, a search match, an undo)
    // takes the pane along.
    if (update.transactions.some(tr => tr.scrollIntoView)) this.drive('editor');
  }
  // Brings the items up to date with the editor's tree, then renders the
  // blocks near the pane's viewport.
  refresh() {
    if (this.destroyed) return;
    const {state} = this.view, tree = syntaxTree(state), length = state.doc.length;
    if (this.changes) {
      const changes = this.changes;
      this.changes = null;
      for (const item of this.items) {
        if (changes.touchesRange(item.from, item.to)) item.dirty = true;
        item.from = changes.mapPos(item.from, -1);
        item.to = changes.mapPos(item.to, 1);
      }
    }
    this.place(this.items, reconcile(this.items, keepUnparsed(paneBlocks(tree, state.doc), this.items)));
    // Definitions come from the whole note only: those at its end would be
    // missing from a partial tree.
    if (tree.length >= length) {
      const definitions = treeDefinitions(tree, (from, to) => state.sliceDoc(from, to)), key = definitions.map(item => item.text).join('\n');
      if (key !== this.definitionsKey) {
        this.definitions = definitions;
        this.definitionsKey = key;
        for (const item of this.items) item.dirty = true;
      }
    }
    this.renderNear();
    // CodeMirror parses only near its viewport; the rest of the note is
    // parsed in idle slices, then the pane takes its blocks.
    if (tree.length < length && !this.parsing) {
      const parsing = (this.parsing = new AbortController());
      ensureTreeIdle(this.view, length, {slice: SLICE, signal: parsing.signal}).then(done => {
        if (this.parsing === parsing) this.parsing = null;
        if (done && !this.destroyed) this.parsed(tree, done);
      });
    }
  }
  // The whole note is parsed. The parse went into the editor's parse
  // context, and its state shows the tree it was made with until a
  // transaction lets it take the new one, so an empty one does, as
  // CodeMirror's forceParsing does: it changes no text, selection or scroll.
  // The pane refreshes only for a tree that grew, never in a loop.
  parsed(before, done) {
    if (syntaxTree(this.view.state) !== done) this.view.dispatch({});
    if (syntaxTree(this.view.state) !== before) this.refresh();
  }
  // Puts the items' elements in the pane, touching only the run that changed.
  place(previous, items) {
    let start = 0, end = 0;
    while (start < previous.length && start < items.length && previous[start] === items[start]) start++;
    while (end < previous.length - start && end < items.length - start && previous[previous.length - 1 - end] === items[items.length - 1 - end]) end++;
    const kept = new Set(items.slice(start, items.length - end));
    for (const item of previous.slice(start, previous.length - end)) {
      if (kept.has(item)) continue;
      this.observer.unobserve(item.element);
      this.near.delete(item);
      item.element.remove();
    }
    const fragment = document.createDocumentFragment();
    for (let i = start; i < items.length - end; i++) {
      const item = items[i];
      if (!item.element) {
        item.element = document.createElement('div');
        item.element.className = 'rendered-block';
        item.element.style.height = this.sourceHeight(item, items[i + 1]) + 'px';
        this.byElement.set(item.element, item);
        this.observer.observe(item.element);
      }
      fragment.append(item.element);
    }
    this.root.insertBefore(fragment, end ? items[items.length - end].element : null);
    this.items = items;
    for (const item of items) {
      if (item.element.dataset.from !== String(item.from)) item.element.dataset.from = item.from;
      if (item.element.dataset.to !== String(item.to)) item.element.dataset.to = item.to;
    }
  }
  // How tall the editor shows an item's source, up to the next item.
  sourceHeight(item, next) {
    const {view} = this, top = view.lineBlockAt(item.from).top;
    const bottom = next ? view.lineBlockAt(next.from).top : view.lineBlockAt(view.state.doc.length).bottom;
    return Math.max(0, Math.round(bottom - top));
  }
  intersect(entries) {
    for (const entry of entries) {
      const item = this.byElement.get(entry.target);
      if (!item) continue;
      // How far the block is from the viewport, as the observer measured
      // it, orders rendering without a layout of its own.
      item.distance = Math.abs(entry.boundingClientRect.top - (entry.rootBounds?.top ?? 0));
      // The observed area is the pane's viewport and two screens either side.
      if (entry.rootBounds) this.screen = entry.rootBounds.height / 5;
      if (entry.isIntersecting) this.near.add(item);
      else {
        this.near.delete(item);
        // A block far from the viewport goes back to a placeholder of its
        // rendered height, so the pane holds only the blocks around it.
        if (item.key != null) this.unrender(item, entry.boundingClientRect.height);
      }
    }
    this.renderNear();
  }
  unrender(item, height) {
    item.element.replaceChildren();
    item.element.classList.remove('rendered');
    item.element.style.height = Math.round(height) + 'px';
    item.key = null;
  }
  renderNear() {
    if (this.rendering || this.destroyed) return;
    this.rendering = true;
    idle(() => {
      this.rendering = false;
      if (this.destroyed) return;
      if (now() - this.flying < FLING) {
        this.rendering = true;
        setTimeout(() => {
          this.rendering = false;
          this.renderNear();
        }, FLING);
        return;
      }
      // Nearest blocks first, and nothing here reads layout: a layout forced
      // in this task would lay out the editor's freshly drawn lines too, and
      // on F1 at 4x CPU that made most frames of a fast scroll long tasks.
      // The text not parsed yet stays a placeholder until it is.
      const started = now(), wanted = [...this.near].filter(item => item.name !== 'Rest' && (item.key == null || item.dirty));
      wanted.sort((a, b) => (a.distance ?? 0) - (b.distance ?? 0));
      for (const item of wanted) {
        this.renderItem(item);
        if (now() - started > SLICE) return this.renderNear();
      }
      if (this.driver !== 'pane') this.follow('pane');
      else if (this.atEnd) this.follow('editor');
    });
  }
  renderItem(item) {
    item.dirty = false;
    const source = item.name === 'Rest' ? '' : blockSource(this.view.state.sliceDoc(item.from, item.to), this.definitions);
    if (source === item.key) return;
    let nodes = this.cache.get(source);
    if (nodes) this.cache.delete(source);
    else {
      try {
        nodes = renderMarkdown(source, {budget: new Budget(BLOCK_LIMIT)});
      } catch {
        // A block too large to render shows as its Markdown.
        nodes = [{tag: 'pre', attrs: null, children: [source]}];
      }
    }
    this.cache.set(source, nodes);
    if (this.cache.size > CACHE) this.cache.delete(this.cache.keys().next().value);
    item.element.replaceChildren(toDOM(nodes, document.createDocumentFragment(), this.hooks));
    enhanceTables(item.element);
    item.element.classList.add('rendered');
    item.element.style.height = '';
    item.key = source;
  }
  // Links follow the reader's rules: an external link opens in a new tab, a
  // link within the note scrolls to its heading, a link to another note opens
  // it in the app, and an address the reader refuses is plain text.
  linkTo(element, href) {
    const target = href ? this.resolve?.(href) : null;
    if (!target) return;
    if (target.external) {
      element.href = target.url || href;
      element.target = '_blank';
      element.rel = 'noopener noreferrer';
      return;
    }
    element.dataset.href = href;
    element.tabIndex = 0;
    element.setAttribute('role', 'link');
  }
  // Enter follows a focused link. End (Ctrl+End, Cmd+Down) goes to the end
  // at once, not in a smooth scroll the blocks rendering on the way would
  // cut short, and holds it there (see editorFollows).
  key(event) {
    if (event.key === 'Enter') return this.click(event);
    if (event.key === 'End' || (event.metaKey && event.key === 'ArrowDown')) {
      event.preventDefault();
      this.atEnd = true;
      this.root.scrollTop = this.root.scrollHeight;
      this.follow('editor');
    }
  }
  click(event) {
    const link = event.target.closest?.('a[data-href], a[data-wiki]');
    if (!link || !this.root.contains(link)) return;
    event.preventDefault();
    if (link.dataset.wiki) return this.open?.({wiki: link.dataset.wiki});
    const href = link.dataset.href;
    if (href.startsWith('#')) {
      let fragment = href.slice(1);
      try {
        fragment = decodeURIComponent(fragment);
      } catch {
        return;
      }
      return this.reveal(fragment);
    }
    this.open?.({href});
  }
  // Scrolls both sides to the heading a #fragment names, by the id Read
  // gives it.
  reveal(fragment) {
    const wanted = fragment.startsWith('h-') ? fragment : 'h-' + fragment, ids = new HeadingIDs(), {state} = this.view;
    let found = null;
    syntaxTree(state).iterate({
      enter: node => {
        if (found != null) return false;
        if (/^(ATX|Setext)Heading/.test(node.name)) {
          if (ids.id(headingLine(state.sliceDoc(node.from, node.to))) === wanted) found = node.from;
          return false;
        }
        return containers.has(node.name);
      },
    });
    if (found == null) return;
    this.drive('editor');
    this.view.dispatch({effects: EditorView.scrollIntoView(found, {y: 'start'})});
  }
  // Scrolls `side` to match the other one, at most once a frame.
  follow(side) {
    if (this.frame) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = 0;
      if (this.destroyed || !this.items.length) return;
      if (side === 'pane') this.paneFollows();
      else this.editorFollows();
    });
  }
  // The heights of an item's source in the editor's document.
  editorSpan(index) {
    const {view} = this, item = this.items[index], next = this.items[index + 1];
    const top = view.lineBlockAt(item.from).top;
    return {top, bottom: next ? view.lineBlockAt(next.from).top : view.lineBlockAt(view.state.doc.length).bottom};
  }
  // The editor's top line gives a block and how far into it the reader is;
  // the pane shows the same block the same fraction in.
  paneFollows() {
    const {view, root} = this, scroller = view.scrollDOM, end = scroller.scrollHeight - scroller.clientHeight;
    if (end > 2 && scroller.scrollTop >= end - 2) {
      root.scrollTop = root.scrollHeight;
      return;
    }
    const height = scroller.getBoundingClientRect().top - view.documentTop;
    if (height <= 0) {
      root.scrollTop = 0;
      return;
    }
    const index = itemAt(this.items, view.lineBlockAtHeight(height).from, item => item.from);
    const {top, bottom} = this.editorSpan(index), element = this.items[index].element;
    root.scrollTop = element.offsetTop + fraction(height, top, bottom) * element.offsetHeight;
  }
  // A pane the reader sent to its end with End stays at its end, with the
  // editor at its own, while the blocks there render taller than their
  // placeholders, until the reader's next input on either side.
  editorFollows() {
    const {view, root} = this, scroller = view.scrollDOM, end = root.scrollHeight - root.clientHeight;
    if (this.atEnd) root.scrollTop = root.scrollHeight;
    if (this.atEnd || (end > 2 && root.scrollTop >= end - 2)) {
      scroller.scrollTop = scroller.scrollHeight;
      return;
    }
    const top = root.scrollTop;
    if (top <= 0) {
      scroller.scrollTop = 0;
      return;
    }
    const index = itemAt(this.items, top, item => item.element.offsetTop), element = this.items[index].element;
    const span = this.editorSpan(index), height = span.top + fraction(top, element.offsetTop, element.offsetTop + element.offsetHeight) * (span.bottom - span.top);
    scroller.scrollTop = view.documentTop - scroller.getBoundingClientRect().top + scroller.scrollTop + height;
  }
  destroy() {
    this.destroyed = true;
    clearTimeout(this.timer);
    cancelAnimationFrame(this.frame);
    this.parsing?.abort();
    this.observer.disconnect();
    for (const remove of this.listeners) remove();
    try {
      if (paneListener.get(this.view.state) === this.listener) this.view.dispatch({effects: paneListener.reconfigure([])});
    } catch {
      // The editor is gone already.
    }
    this.root.remove();
  }
}

// mountPreview({host, view, imageURL, resolve, open, pageTarget}) shows the
// note of the editor view rendered, read-only, in host, following the
// editor's text and scroll. imageURL(path) gives a notebook image's URL (an
// image it gives none for is not loaded), resolve(href) a link's target, as
// the reader resolves them; open({href} | {wiki}) follows a link to another
// note or page, and pageTarget(name) says whether a [[page]] exists. Returns
// {destroy()}.
export function mountPreview(options) {
  const pane = new Pane(options);
  return {destroy: () => pane.destroy()};
}
