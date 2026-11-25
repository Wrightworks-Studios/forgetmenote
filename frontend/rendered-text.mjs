import {fromMarkdown} from 'mdast-util-from-markdown';
import {gfmAutolinkLiteral} from 'micromark-extension-gfm-autolink-literal';
import {gfmStrikethrough} from 'micromark-extension-gfm-strikethrough';
import {gfmTable} from 'micromark-extension-gfm-table';
import {gfmTaskListItem} from 'micromark-extension-gfm-task-list-item';
import {gfmAutolinkLiteralFromMarkdown} from 'mdast-util-gfm-autolink-literal';
import {gfmStrikethroughFromMarkdown} from 'mdast-util-gfm-strikethrough';
import {gfmTableFromMarkdown} from 'mdast-util-gfm-table';
import {gfmTaskListItemFromMarkdown} from 'mdast-util-gfm-task-list-item';
import {anchorAt, findAnchor} from '../internal/web/ui/annotations.mjs';

// A note's rendered text, the text comment anchors quote: what the server's
// commentText (internal/workspace/anchors.go) reads from goldmark's HTML. It
// is the visible text of the body after the front matter, with the text of
// every block element (paragraph, heading, list item, table cell, code block,
// quote, line break) set apart by spaces, and every run of white space
// collapsed to one space. Raw HTML, images, link addresses and task boxes
// are not part of it. The parser is micromark with the GFM pieces goldmark's
// GFM has (tables, strikethrough, autolinks, task lists), so no footnotes.
// The corpus in internal/workspace/testdata/comment-text pins the two equal.
//
// Each character of the text keeps the source range it came from (from[i],
// to[i]), so a passage found in the text is a range of the Markdown.

const parse = source => fromMarkdown(source, {
  extensions: [gfmAutolinkLiteral(), gfmStrikethrough(), gfmTable(), gfmTaskListItem()],
  mdastExtensions: [gfmAutolinkLiteralFromMarkdown(), gfmStrikethroughFromMarkdown(), gfmTableFromMarkdown(), gfmTaskListItemFromMarkdown()],
});

// White space as Go's unicode.IsSpace (strings.Fields) has it.
const space = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/;
const punctuation = /[!-/:-@[-`{-~]/;
const entity = /^&(?:#[xX][\da-fA-F]{1,6}|#\d{1,7}|[a-zA-Z][a-zA-Z\d]{0,31});/;

// The body after the front matter, as internal/workspace frontMatter reads
// it: a first line of exactly "---" (LF or CRLF) and a later line that is
// "---" once its line ends are trimmed, reached within 64 KiB; otherwise the
// whole note.
export function bodyStart(source) {
  if (!source.startsWith('---\n') && !source.startsWith('---\r\n')) return 0;
  let offset = source.indexOf('\n') + 1;
  const encoder = new TextEncoder();
  let bytes = encoder.encode(source.slice(0, offset)).length;
  while (offset < source.length) {
    const end = source.indexOf('\n', offset), next = end < 0 ? source.length : end + 1;
    const line = source.slice(offset, next);
    bytes += encoder.encode(line).length;
    offset = next;
    if (line.replace(/[\r\n]+$/, '') === '---') return offset;
    if (bytes > 65536) break;
  }
  return 0;
}

class Text {
  constructor(size) {
    this.chunks = [];
    this.chunk = '';
    this.length = 0;
    this.from = new Int32Array(size + 1);
    this.to = new Int32Array(size + 1);
    // A space waiting for the next visible character, and where it starts.
    this.space = -1;
    this.last = 0;
    // Source before this offset shows nothing (a task box goldmark reads).
    this.skip = 0;
  }
  grow() {
    for (const name of ['from', 'to']) {
      const bigger = new Int32Array(this[name].length * 2);
      bigger.set(this[name]);
      this[name] = bigger;
    }
  }
  // A block edge or white space at pos: one space before the next character.
  gap(pos) {
    if (this.length && this.space < 0) this.space = pos;
  }
  put(char, from, to) {
    if (from < this.skip) return;
    if (space.test(char)) return this.gap(from);
    if (this.space >= 0) {
      const at = Math.max(this.last, this.space);
      this.space = -1;
      this.push(' ', at, at);
    }
    this.push(char, from, to);
  }
  push(char, from, to) {
    if (this.length + 1 >= this.from.length) this.grow();
    // Offsets never go back, so a passage is always a forward range.
    from = Math.max(from, this.last);
    this.from[this.length] = from;
    this.to[this.length] = Math.max(from, to);
    this.last = from;
    this.chunk += char;
    if (this.chunk.length >= 4096) {
      this.chunks.push(this.chunk);
      this.chunk = '';
    }
    this.length++;
  }
  done() {
    this.chunks.push(this.chunk);
    return {text: this.chunks.join(''), from: this.from.slice(0, this.length), to: this.to.slice(0, this.length)};
  }
}

const decoded = new Map();
function decode(reference) {
  if (!decoded.has(reference)) decoded.set(reference, parse(reference).children[0]?.children?.[0]?.value ?? '');
  return decoded.get(reference);
}

// Puts value (a node's text) to out, each character with the source range it
// came from: value is the source from start with escapes and character
// references decoded and the lines' container prefixes and indentation
// removed, so source characters it lacks are skipped.
function align(out, source, start, end, value) {
  let r = start;
  for (let v = 0; v < value.length;) {
    const char = value[v];
    if (r >= end) {
      out.put(char, end, end);
      v++;
      continue;
    }
    const at = source[r];
    if (at === '\\' && source[r + 1] === char && punctuation.test(char)) {
      out.put(char, r, r + 2);
      r += 2;
      v++;
      continue;
    }
    if (at === '&') {
      const reference = entity.exec(source.slice(r, r + 40))?.[0], text = reference && decode(reference);
      if (text && text !== reference && value.startsWith(text, v)) {
        // One entry per UTF-16 unit: an astral character is two.
        for (let k = 0; k < text.length; k++) out.put(text[k], r, r + reference.length);
        r += reference.length;
        v += text.length;
        continue;
      }
    }
    if (at === char) {
      out.put(char, r, r + 1);
      r++;
      v++;
    } else if (space.test(char)) {
      // A tab's columns or a line end the value spells differently.
      out.put(char, r, space.test(at) ? r + 1 : r);
      if (space.test(at)) r++;
      v++;
    } else r++;
  }
}

const blocks = new Set(['paragraph', 'heading', 'listItem', 'tableCell', 'blockquote']);
const containers = new Set(['root', 'list', 'emphasis', 'strong', 'delete', 'link', 'linkReference']);

// Where goldmark reads differently from micromark: a task box is any
// "[ ]", "[x]" or "[X]" that starts a list item's first paragraph, with or
// without text or a space after it (goldmark's tasklist extension).
const taskBox = /^\[[\t\n\f\r xX]\][\t\n\f\r ]*/;

// The rendered text of a note's Markdown, with each character's source range.
export function renderedText(source) {
  const start = bodyStart(source);
  // goldmark ends lines at \n only and keeps a byte order mark as text,
  // where micromark also ends lines at a lone \r and drops a leading mark:
  // a lone \r is read as the space goldmark makes of it, and the mark after a
  // line break, where micromark keeps it.
  let body = source.slice(start).replace(/\r(?!\n)/g, ' '), offset = start;
  if (body.startsWith('\ufeff')) {
    body = '\n' + body;
    offset--;
  }
  const out = new Text(Math.min(source.length, 1 << 16));
  const visit = node => {
    const from = offset + node.position.start.offset, to = offset + node.position.end.offset;
    switch (node.type) {
      case 'text':
        align(out, source, from, to, node.value);
        return;
      case 'inlineCode': {
        let start = from;
        while (source[start] === '`') start++;
        align(out, source, start, to, node.value);
        return;
      }
      case 'code': {
        // A fenced block's text starts on the line after its fence.
        const fenced = /^[ \t]*(?:`{3}|~{3})/.test(source.slice(from, from + 64));
        const start = fenced ? Math.min(to, source.indexOf('\n', from) + 1 || to) : from;
        out.gap(from);
        align(out, source, start, to, node.value);
        out.gap(to);
        return;
      }
      case 'break':
        out.gap(from);
        return;
      case 'listItem': {
        const first = node.children[0];
        if (node.checked == null && first?.type === 'paragraph') {
          const at = offset + first.position.start.offset, box = taskBox.exec(source.slice(at, at + 64));
          if (box) out.skip = at + box[0].length;
        }
        break;
      }
      case 'table': {
        // Cells past the header row's are dropped, as GFM says.
        const columns = node.children[0]?.children.length ?? 0;
        for (const row of node.children) for (const cell of row.children.slice(0, columns)) visit(cell);
        return;
      }
    }
    if (blocks.has(node.type)) {
      out.gap(from);
      for (const child of node.children) visit(child);
      out.gap(to);
    } else if (containers.has(node.type)) for (const child of node.children) visit(child);
    // html, image, imageReference, definition and thematicBreak show no text.
  };
  visit(parse(body));
  return out.done();
}

// The source ranges of the comments' passages that the text has: a Map from
// comment id to {from, to}.
export function placeAnchors(rendered, comments) {
  const placed = new Map();
  for (const comment of comments) {
    if (comment.resolved || !comment.anchor) continue;
    const found = findAnchor(rendered.text, comment.anchor);
    if (found && found.to > found.from) placed.set(comment.id, {from: rendered.from[found.from], to: rendered.to[found.to - 1]});
  }
  return placed;
}

// The anchor of the source range [from, to): the rendered characters that
// start in it, with their context, or null when it shows no text.
export function anchorOf(rendered, from, to) {
  const first = index => {
    let low = 0, high = rendered.from.length;
    while (low < high) {
      const middle = (low + high) >> 1;
      if (rendered.from[middle] < index) low = middle + 1;
      else high = middle;
    }
    return low;
  };
  const start = first(from), end = first(to);
  if (start >= end) return null;
  const anchor = anchorAt(rendered.text, start, end);
  return anchor.quote ? anchor : null;
}
