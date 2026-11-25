import {EditorSelection, Facet, MapMode, Prec, StateEffect, StateField, findClusterBreak} from '@codemirror/state';
import {Decoration, EditorView, ViewPlugin, WidgetType, keymap} from '@codemirror/view';
import {HighlightStyle, syntaxHighlighting, syntaxTree} from '@codemirror/language';
import {markdown, markdownLanguage} from '@codemirror/lang-markdown';
import {html} from '@codemirror/lang-html';
import {tags} from '@lezer/highlight';
import {SpaceRun, topBlocks} from './markdown-syntax.mjs';

// Live preview: the Markdown text is the document and every edit is an
// ordinary CodeMirror text change. Formatting is drawn over it with
// decorations, and the syntax markers of an element are hidden unless the
// selection touches that element (as in Obsidian's Live Preview), so nothing
// the user did not edit can change. Decorations are built from the
// @codemirror/lang-markdown tree for the drawn part of the document only:
// inline ones by a view plugin for the visible ranges, and the block ones
// (tables, rules, videos, front matter) by a state field for a range around
// the viewport, because only decorations provided directly may replace line
// breaks. The field maps its widgets through each edit and rebuilds only the
// top-level blocks the edit or the selection touched.

// ---------------------------------------------------------------------------
// Parser additions: front matter, [[wiki links]] and footnotes, which GFM in
// lezer does not know (it reads them as a rule and a setext heading, and as
// shortcut links). Pandoc sub/superscript and emoji shortcodes are removed:
// the app's renderer (goldmark, GFM) has neither.

// Front matter, as internal/workspace frontMatter reads it: the note starts
// with a "---" line (LF or CRLF) and a later "---" line closes it, every line
// before the closing one ending within 64 KiB.
const FRONT_MATTER_LIMIT = 65536;
const FRONT_MATTER_READ = FRONT_MATTER_LIMIT + 8192;
function frontMatterEnd(text) {
  const opening = /^---\r?\n/.exec(text);
  if (!opening) return null;
  let at = opening[0].length;
  while (at < text.length) {
    const newline = text.indexOf('\n', at), end = newline < 0 ? text.length : newline;
    if (text.slice(at, end).replace(/\r$/, '') === '---') return {opening: opening[0].replace(/\r?\n$/, '').length, from: at, to: end};
    at = newline < 0 ? text.length : newline + 1;
    if (at > FRONT_MATTER_LIMIT) break;
  }
  return null;
}
const FrontMatter = {
  defineNodes: [
    {name: 'FrontMatter', block: true, style: tags.meta},
    {name: 'FrontMatterMark', style: tags.processingInstruction},
  ],
  parseBlock: [{
    name: 'FrontMatter',
    before: 'HorizontalRule',
    parse(cx, line) {
      if (cx.lineStart !== 0 || !/^---\r?$/.test(line.text)) return false;
      // The block context reads lines one at a time; the closing line is
      // looked for first (@lezer/markdown keeps its input on the context),
      // so an unclosed opening line stays what CommonMark makes of it.
      const input = cx.input, close = input && frontMatterEnd(input.read(0, Math.min(input.length, FRONT_MATTER_READ)));
      if (!close) return false;
      const marks = [cx.elt('FrontMatterMark', 0, close.opening)];
      while (cx.lineStart < close.from && cx.nextLine());
      marks.push(cx.elt('FrontMatterMark', close.from, close.from + 3));
      cx.nextLine();
      cx.addElement(cx.elt('FrontMatter', 0, close.to, marks));
      return true;
    },
  }],
};

// As internal/knowledge/wikilinks.go: [[Target]] or [[Target|label]].
const wikiPattern = /^\[\[([^[\]|\n]+)(?:\|([^[\]\n]+))?\]\]/;
const WikiLink = {
  defineNodes: [
    {name: 'WikiLink', style: tags.link},
    {name: 'WikiLinkMark', style: tags.processingInstruction},
    {name: 'WikiLinkTarget'},
    {name: 'WikiLinkLabel'},
  ],
  parseInline: [{
    name: 'WikiLink',
    before: 'Link',
    parse(cx, next, pos) {
      if (next !== 91 || cx.char(pos + 1) !== 91) return -1;
      const match = wikiPattern.exec(cx.slice(pos, Math.min(cx.end, pos + 1000)));
      if (!match) return -1;
      const end = pos + match[0].length, targetEnd = pos + 2 + match[1].length;
      const children = [cx.elt('WikiLinkMark', pos, pos + 2), cx.elt('WikiLinkTarget', pos + 2, targetEnd)];
      if (match[2] != null) children.push(cx.elt('WikiLinkMark', targetEnd, targetEnd + 1), cx.elt('WikiLinkLabel', targetEnd + 1, end - 2));
      children.push(cx.elt('WikiLinkMark', end - 2, end));
      return cx.addElement(cx.elt('WikiLink', pos, end, children));
    },
  }],
};

const footnoteLabel = /^\[\^[^\]\s]+\]/;
class FootnoteDefinitionParser {
  nextLine() {
    return false;
  }
  finish(cx, leaf) {
    const label = footnoteLabel.exec(leaf.content)[0].length + 1;
    cx.addLeafElement(leaf, cx.elt('FootnoteDefinition', leaf.start, leaf.start + leaf.content.length, [
      cx.elt('FootnoteLabel', leaf.start, leaf.start + label),
      ...cx.parser.parseInline(leaf.content.slice(label), leaf.start + label),
    ]));
    return true;
  }
}
const Footnote = {
  defineNodes: [
    {name: 'FootnoteReference', style: tags.labelName},
    {name: 'FootnoteDefinition', block: true},
    {name: 'FootnoteLabel', style: tags.labelName},
  ],
  parseInline: [{
    name: 'FootnoteReference',
    before: 'Link',
    parse(cx, next, pos) {
      if (next !== 91 || cx.char(pos + 1) !== 94) return -1;
      const match = footnoteLabel.exec(cx.slice(pos, Math.min(cx.end, pos + 200)));
      return match ? cx.addElement(cx.elt('FootnoteReference', pos, pos + match[0].length)) : -1;
    },
  }],
  parseBlock: [{
    name: 'FootnoteDefinition',
    before: 'LinkReference',
    leaf: (_, leaf) => (/^\[\^[^\]\s]+\]:/.test(leaf.content) ? new FootnoteDefinitionParser() : null),
  }],
};

// The Markdown language of every editing mode: GFM, front matter, wiki links
// and footnotes, reading runs of spaces at once (SpaceRun). Source mode uses
// it too, so its colours agree with Visual.
// It brings no keys or edits of its own: lang-markdown's Enter and Backspace
// renumber lists (editor-commands.mjs has the note's own), a URL pasted over
// a selection would become a link, and typing ">" or "/" in HTML would close
// the tag.
const htmlTags = html({matchClosingTags: false, autoCloseTags: false});
export function livePreviewMarkdown(config = {}) {
  return markdown({base: markdownLanguage, extensions: [{remove: ['Superscript', 'Subscript', 'Emoji']}, FrontMatter, WikiLink, Footnote, SpaceRun],
    addKeymap: false, pasteURLAsLink: false, htmlTagLanguage: htmlTags, ...config});
}

// ---------------------------------------------------------------------------
// Front matter, from the text (the same rule as the parser above), so the
// widget does not wait for the tree.
export function frontMatter(doc) {
  if (doc.length < 4) return null;
  const close = frontMatterEnd(doc.sliceString(0, Math.min(doc.length, FRONT_MATTER_READ)));
  return close ? {from: 0, to: close.to} : null;
}

// ---------------------------------------------------------------------------
// Link reference definitions ([label]: address), found line by line and kept
// up to date by rescanning only the lines an edit touched, so [text],
// [text][] and [text][label] resolve against the note's definitions as the
// reader resolves them. The first definition of a label wins (CommonMark).
const definitionLine = /^ {0,3}\[((?:[^\\[\]]|\\.){1,999})\]:[ \t]*(?:<([^<>\n]*)>|(\S+))/;
export const referenceLabel = label => label.replace(/\\([[\]\\])/g, '$1').trim().replace(/\s+/g, ' ').toLowerCase();
function definitionAt(line, pos) {
  if (line.indexOf(']:') < 0) return null;
  const match = definitionLine.exec(line);
  return match && !match[1].startsWith('^') ? {pos, label: referenceLabel(match[1]), href: match[2] ?? match[3]} : null;
}
function scanDefinitions(doc, from, to, out) {
  for (let pos = from; pos <= to && pos <= doc.length;) {
    const line = doc.lineAt(pos), found = definitionAt(line.text, line.from);
    if (found) out.push(found);
    pos = line.to + 1;
  }
  return out;
}
class Definitions {
  constructor(list, map = null) {
    this.list = list;
    this.map = map;
  }
  get(label) {
    if (!this.map) {
      this.map = new Map();
      for (const item of this.list) if (!this.map.has(item.label)) this.map.set(item.label, item.href);
    }
    return this.map.get(referenceLabel(label));
  }
}
const definitions = StateField.define({
  create: state => new Definitions(scanDefinitions(state.doc, 0, state.doc.length, [])),
  update(value, tr) {
    if (!tr.docChanged) return value;
    const touched = [];
    tr.changes.iterChangedRanges((_fromA, _toA, fromB, toB) => touched.push([tr.newDoc.lineAt(fromB).from, tr.newDoc.lineAt(toB).to]));
    const inside = pos => touched.some(([from, to]) => pos >= from && pos <= to);
    const kept = [];
    for (const item of value.list) {
      const pos = tr.changes.mapPos(item.pos, 1, MapMode.TrackDel);
      if (pos != null && !inside(pos) && tr.newDoc.lineAt(pos).from === pos) kept.push({...item, pos});
    }
    for (const [from, to] of touched) scanDefinitions(tr.newDoc, from, to, kept);
    kept.sort((a, b) => a.pos - b.pos);
    const same = kept.length === value.list.length && kept.every((item, i) => item.label === value.list[i].label && item.href === value.list[i].href);
    return new Definitions(kept, same ? value.map : null);
  },
});

// The note's link definitions, for tests and for other readers of a state.
export const linkDefinitions = state => state.field(definitions, false) ?? new Definitions(scanDefinitions(state.doc, 0, state.doc.length, []));

// ---------------------------------------------------------------------------
// Settings the decorations read: resolveImage(path) gives the URL of a
// notebook image (or null), video(href) a player element for a video link.
const settingsFacet = Facet.define({combine: values => values[0] || {}});

// ---------------------------------------------------------------------------
// Private drafts. While the editor shows text the server does not hold yet (a
// new note, a historical version, a retained or recovered draft), a video
// whose player comes from an outside host shows a placeholder: loading the
// player tells that host the viewer's address and the video. Load video loads
// the player of that one link, in this editor, from then on; another link to
// the same video asks again. The state is not the note's text; it is kept
// through mode changes (privateVideos goes outside the Visual mode). An
// editor without it shows saved notes.

export const setPrivateDraft = StateEffect.define();
// Loads the player of one video link, {at, href}: the link to `href` that
// ends at `at` (its paragraph's end, where the player goes). Exported for tests.
export const loadVideo = StateEffect.define();
const isLoaded = (loaded, {at, href}) => loaded.some(video => video.at === at && video.href === href);
const videoPrivacy = StateField.define({
  create: () => ({draft: false, loaded: [], host: () => null, version: 0}),
  update(value, tr) {
    // A loaded link moves with the text; an edit that deletes its last
    // character (its end) holds it again, so a link that comes to end there,
    // even to the same video, is never loaded without a click. `version`
    // counts the changes of what is held: only those redraw every video.
    if (tr.docChanged && value.loaded.length) {
      const loaded = [];
      for (const video of value.loaded) {
        const at = tr.changes.mapPos(video.at, -1, MapMode.TrackBefore);
        if (at !== null) loaded.push(at === video.at ? video : {...video, at});
      }
      value = {...value, loaded, version: value.version + (loaded.length < value.loaded.length ? 1 : 0)};
    }
    for (const effect of tr.effects) {
      if (effect.is(setPrivateDraft) && effect.value !== value.draft) value = {...value, draft: effect.value, version: value.version + 1};
      else if (effect.is(loadVideo) && !isLoaded(value.loaded, effect.value)) value = {...value, loaded: [...value.loaded, effect.value], version: value.version + 1};
    }
    return value;
  },
});
const heldVersion = state => state.field(videoPrivacy, false)?.version;
// The state of an editor that opens a private draft or not; host(href) names
// the outside host a video link's player loads from, null for one this app
// serves.
export const privateVideos = ({privateDraft = false, host = () => null} = {}) =>
  videoPrivacy.init(() => ({draft: !!privateDraft, loaded: [], host, version: 0}));
// Tells the editor its text became a private draft (true) or was saved. A
// video link drawn with its player then keeps it: its host has been asked
// already, and a placeholder would stop it playing.
export function markPrivateDraft(view, value) {
  const privacy = view.state.field(videoPrivacy, false), effects = [setPrivateDraft.of(!!value)];
  if (value && privacy && !privacy.draft) {
    for (const wrap of view.contentDOM.querySelectorAll('.cm-lp-video')) {
      const href = players.get(wrap);
      if (href && privacy.host(href)) effects.push(loadVideo.of({at: view.posAtDOM(wrap), href}));
    }
  }
  view.dispatch({effects});
}
// The outside host the placeholder of the video link to `href` ending at `at`
// names, or null when its player loads at once: in a saved note, for a video
// this app serves, or once that link is loaded.
export function heldVideo(state, href, at) {
  const privacy = state.field(videoPrivacy, false);
  if (!privacy?.draft || isLoaded(privacy.loaded, {at, href})) return null;
  return privacy.host(href) || null;
}

// ---------------------------------------------------------------------------
// Widgets. None of them holds a position: one that acts on the text asks the
// view where it is now (posAtDOM), so it can never edit stale offsets.

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

// Markdown inline parts (see inlineParts) as DOM.
function renderParts(parent, parts) {
  for (const part of parts) {
    if (typeof part === 'string') parent.append(part);
    else if (part.tag === 'code') parent.append(element('code', 'cm-lp-inline-code', part.text));
    else if (part.tag === 'a') parent.append(element('span', 'cm-lp-link', part.text));
    else if (part.tag === 'wiki') parent.append(element('span', 'cm-lp-wikilink', part.text));
    else renderParts(parent.appendChild(element(part.tag)), part.children);
  }
}

// A list item's prefix (its indentation, marker and the one space after it)
// drawn as a box as wide as the line's hanging indent, so the wrapped lines
// of the item line up under its text.
class PrefixWidget extends WidgetType {
  eq(other) {
    return other.constructor === this.constructor;
  }
  box() {
    const box = element('span', 'cm-lp-list-prefix');
    box.setAttribute('aria-hidden', 'true');
    return box;
  }
  ignoreEvent() {
    return false;
  }
}
class BulletWidget extends PrefixWidget {
  toDOM() {
    const box = this.box();
    box.append(element('span', 'cm-lp-bullet', '•'));
    return box;
  }
}
const bullet = new BulletWidget();

// Ordered list numbers show as typed.
class NumberWidget extends PrefixWidget {
  constructor(text) {
    super();
    this.text = text;
  }
  eq(other) {
    return other instanceof NumberWidget && other.text === this.text;
  }
  toDOM() {
    const box = this.box();
    box.append(element('span', 'cm-lp-list-number', this.text));
    return box;
  }
}

// The task box replaces the item's prefix through "[ ]"; offset is where the
// box's state character (the space or x) is, from the start of what it
// replaces. In a read-only note the box is disabled.
class TaskWidget extends PrefixWidget {
  constructor(checked, offset, readOnly = false, text = '') {
    super();
    this.checked = checked;
    this.offset = offset;
    this.readOnly = readOnly;
    this.text = text;
  }
  eq(other) {
    return other instanceof TaskWidget && other.checked === this.checked && other.offset === this.offset
      && other.readOnly === this.readOnly && other.text === this.text;
  }
  toDOM(view) {
    const wrap = this.box();
    wrap.removeAttribute('aria-hidden');
    if (this.text) wrap.append(element('span', 'cm-lp-list-number', this.text + ' '));
    const box = wrap.appendChild(element('input', 'cm-lp-task'));
    box.type = 'checkbox';
    box.checked = this.checked;
    box.disabled = this.readOnly;
    box.setAttribute('aria-label', this.checked ? 'Done task' : 'Open task');
    // The editor keeps its focus and selection; the click is the only change,
    // so a tap opens no on-screen keyboard. Tab passes the box (Mod-Enter
    // toggles a task from the keyboard). A box that had the focus (Space, or
    // assistive technology activating it) is drawn again by the toggle, so
    // the focus goes back to the text.
    box.tabIndex = -1;
    box.addEventListener('mousedown', event => event.preventDefault());
    box.addEventListener('click', event => {
      event.preventDefault();
      const focused = view.root.activeElement === box;
      toggleTaskAt(view, view.posAtDOM(wrap) + this.offset);
      if (focused) view.focus();
    });
    return wrap;
  }
  ignoreEvent() {
    return true;
  }
}

class RuleWidget extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const rule = element('div', 'cm-lp-hr');
    rule.setAttribute('role', 'separator');
    return rule;
  }
  get estimatedHeight() {
    return 26;
  }
  ignoreEvent() {
    return false;
  }
}
const rule = new RuleWidget();
// A rule after a quote or list marker stays on its line.
class InlineRuleWidget extends WidgetType {
  eq() {
    return true;
  }
  toDOM() {
    const line = element('span', 'cm-lp-hr cm-lp-hr-inline');
    line.setAttribute('role', 'separator');
    return line;
  }
  ignoreEvent() {
    return false;
  }
}
const inlineRule = new InlineRuleWidget();

class LanguageWidget extends WidgetType {
  constructor(language) {
    super();
    this.language = language;
  }
  eq(other) {
    return other.language === this.language;
  }
  toDOM() {
    return element('span', 'cm-lp-code-language', this.language);
  }
  ignoreEvent() {
    return false;
  }
}

// An HTML entity as its character. Only the entity syntax lezer accepts
// reaches innerHTML.
class EntityWidget extends WidgetType {
  constructor(entity) {
    super();
    this.entity = entity;
  }
  eq(other) {
    return other.entity === this.entity;
  }
  toDOM() {
    const span = element('span', 'cm-lp-entity');
    if (/^&(#\d{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});$/.test(this.entity)) span.innerHTML = this.entity;
    else span.textContent = this.entity;
    return span;
  }
  ignoreEvent() {
    return false;
  }
}

// A notebook image through the app's asset URL. Anything else (web images
// included) is never requested: its alt text shows instead.
class ImageWidget extends WidgetType {
  constructor(url, alt, source) {
    super();
    this.url = url;
    this.alt = alt;
    this.source = source;
  }
  eq(other) {
    return other.url === this.url && other.alt === this.alt && other.source === this.source;
  }
  toDOM(view) {
    if (!this.url) {
      const missing = element('span', 'cm-lp-image-missing', this.alt || this.source || 'Image');
      missing.title = 'Image not shown: ' + this.source;
      return missing;
    }
    const image = element('img', 'cm-lp-image');
    image.alt = this.alt;
    image.src = this.url;
    image.draggable = false;
    // Its height is known only once it has loaded.
    image.addEventListener('load', () => view.requestMeasure(), {once: true});
    return image;
  }
  ignoreEvent() {
    return false;
  }
}

// The element of each video widget drawn with its player, to its link's address.
const players = new WeakMap();

// A video link's player, after its paragraph; while the video is held (host,
// see heldVideo) a placeholder instead. Clicks on its controls are its own:
// the editor ignores them.
class VideoWidget extends WidgetType {
  constructor(href, host) {
    super();
    this.href = href;
    this.host = host;
  }
  eq(other) {
    return other.href === this.href && other.host === this.host;
  }
  toDOM(view) {
    const wrap = element('div', 'cm-lp-video');
    wrap.contentEditable = 'false';
    this.fill(wrap, view);
    return wrap;
  }
  // The same link loaded (or held again) changes in place, and its player
  // takes the focus of the Load video button.
  updateDOM(dom, view, from) {
    if (from?.href !== this.href) return false;
    const focused = dom.contains(document.activeElement);
    this.fill(dom, view);
    if (focused) dom.querySelector('iframe, video')?.focus();
    return true;
  }
  fill(wrap, view) {
    const content = this.host ? heldVideoBox(view, this.href, this.host) : view.state.facet(settingsFacet).video?.(this.href);
    wrap.replaceChildren(...(content ? [content] : []));
    if (content && !this.host) players.set(wrap, this.href);
    else players.delete(wrap);
  }
  get estimatedHeight() {
    return this.host ? 110 : 320;
  }
  ignoreEvent() {
    return true;
  }
}

// A held video: the host its player would contact, the link, and Load video,
// which loads it in this editor.
function heldVideoBox(view, href, host) {
  const box = element('div', 'cm-lp-video-held');
  box.setAttribute('role', 'group');
  box.setAttribute('aria-label', 'Video not loaded');
  const note = element('p', null, 'Video not loaded in this private draft. Loading it connects to ');
  note.append(element('strong', null, host), '.');
  const load = element('button', 'cm-lp-video-load', 'Load video');
  load.type = 'button';
  // The link is the one whose paragraph ends where the widget is now.
  load.addEventListener('click', () => view.dispatch({effects: loadVideo.of({at: view.posAtDOM(box), href})}));
  box.append(note, element('p', 'cm-lp-video-link', href), load);
  return box;
}

// A click in a block widget puts the caret at the source of what was clicked,
// which shows that source.
function caretFromWidget(view, dom, event) {
  const target = event.target.closest?.('[data-lp-offset]');
  const offset = Number(target?.dataset.lpOffset ?? dom.dataset.lpOffset ?? 0);
  event.preventDefault();
  view.dispatch({selection: {anchor: Math.min(view.posAtDOM(dom) + offset, view.state.doc.length)}, userEvent: 'select.pointer'});
  view.focus();
}

// Text-only cells of a column that are all numbers align right, as the
// reader's tables do (tables.mjs numericColumns).
const numeric = /^[-+]?[$€£¥]?\s*\d[\d,.\s]*%?$/;
const cellText = cell => cell.parts.map(part => (typeof part === 'string' ? part : part.text ?? '')).join('').trim();
export function numericColumns(model) {
  return model.header.map((_, column) => model.rows.some(row => cellText(row[column]))
    && model.rows.every(row => !cellText(row[column]) || numeric.test(cellText(row[column]))));
}
class TableWidget extends WidgetType {
  constructor(model, source) {
    super();
    this.model = model;
    this.source = source;
  }
  eq(other) {
    return other.source === this.source;
  }
  toDOM(view) {
    const wrap = element('div', 'cm-lp-table-wrap'), table = wrap.appendChild(element('table', 'cm-lp-table'));
    const numbers = numericColumns(this.model);
    const row = (parent, cells, tag) => {
      const tr = parent.appendChild(element('tr'));
      cells.forEach((cell, column) => {
        const td = tr.appendChild(element(tag));
        td.dataset.lpOffset = cell.offset;
        if (this.model.align[column]) td.style.textAlign = this.model.align[column];
        else if (numbers[column]) td.classList.add('numeric');
        renderParts(td, cell.parts);
      });
    };
    row(table.appendChild(element('thead')), this.model.header, 'th');
    const body = table.appendChild(element('tbody'));
    for (const cells of this.model.rows) row(body, cells, 'td');
    wrap.addEventListener('mousedown', event => caretFromWidget(view, wrap, event));
    return wrap;
  }
  get estimatedHeight() {
    return 34 * (this.model.rows.length + 1) + 16;
  }
  ignoreEvent() {
    return true;
  }
}

class FrontMatterWidget extends WidgetType {
  constructor(fields, source) {
    super();
    this.fields = fields;
    this.source = source;
  }
  eq(other) {
    return other.source === this.source;
  }
  toDOM(view) {
    const box = element('div', 'cm-lp-frontmatter-summary');
    box.dataset.lpOffset = 4;
    box.title = 'Properties: click to edit';
    box.append(element('span', 'cm-lp-frontmatter-label', 'Properties'));
    for (const [key, value] of this.fields) {
      const field = box.appendChild(element('span', 'cm-lp-frontmatter-field'));
      field.append(element('span', 'cm-lp-frontmatter-key', key), value ? ' ' + value : '');
    }
    box.addEventListener('mousedown', event => caretFromWidget(view, box, event));
    return box;
  }
  get estimatedHeight() {
    return 40;
  }
  ignoreEvent() {
    return true;
  }
}

// ---------------------------------------------------------------------------
// Tasks: a click on the box, or Mod-Enter on its line, changes exactly the
// character between its brackets, after checking the bytes it changes.

// The change that toggles the task whose state character is at pos, or null
// when there is none.
export function taskToggle(state, pos) {
  const around = state.sliceDoc(pos - 1, pos + 2);
  if (!/^\[[ xX]\]$/.test(around)) return null;
  return {from: pos, to: pos + 1, insert: around[1] === ' ' ? 'x' : ' '};
}

// Toggles that task; never in a read-only note or inside an open IME
// composition, whose text the input method still owns.
export function toggleTaskAt(view, pos) {
  if (view.state.readOnly || view.composing) return false;
  const change = taskToggle(view.state, pos);
  if (!change) return false;
  view.dispatch({changes: change, userEvent: 'input.toggle'});
  return true;
}

export function toggleTask(view) {
  const line = view.state.doc.lineAt(view.state.selection.main.head);
  let marker = null;
  syntaxTree(view.state).iterate({
    from: line.from,
    to: line.to,
    enter: node => {
      if (!marker && node.name === 'TaskMarker' && node.from >= line.from) marker = node.from;
    },
  });
  return marker != null && toggleTaskAt(view, marker + 1);
}

// ---------------------------------------------------------------------------
// Decorations.

const touches = (selection, from, to) => !!selection && selection.some(range => range.from <= to && range.to >= from);
const marks = {};
const mark = className => (marks[className] ||= Decoration.mark({class: className}));
const lines = {};
const lineClass = className => (lines[className] ||= Decoration.line({class: className}));
const hidden = Decoration.replace({});
// A heading's line tells assistive technology that it is one, and its level,
// as Read's <h1>-<h6> do. A setext heading's text lines after its first take
// the class only, so it stays one heading, named by its first line.
const headingLines = [];
const headingLine = level => (headingLines[level] ||= Decoration.line({
  class: 'cm-lp-heading cm-lp-h' + level, attributes: {role: 'heading', 'aria-level': String(level)},
}));
const quoteLines = [];
const quoteLine = depth => (quoteLines[depth] ||= Decoration.line({class: 'cm-lp-quote', attributes: {style: '--lp-quote-depth: ' + depth}}));
const hangLines = {};
// A list item's first line hangs: wrapped lines start under its text. Each
// nesting level adds one step; a number's box is as wide as the number.
const hangLine = (depth, width) => {
  const key = depth + ':' + width;
  return (hangLines[key] ||= Decoration.line({class: 'cm-lp-list', attributes: {style: '--lp-hang: calc(' + depth + ' * var(--lp-list-step) + ' + width + ')'}}));
};

// Decision 5 (provisional): a bullet or number shows as typed while the caret
// is on its line, as headings and quotes do; a task's box stays a box there,
// and shows its brackets only when the selection touches them.
const LIST_MARKS_ON_CARET_LINE = true;

// Image sources live preview shows: notebook attachments, through the app's
// resolver. Web, data: and every other scheme are never loaded.
export function imageURL(source, resolve) {
  const value = source.trim().replace(/^<(.*)>$/, '$1');
  if (!value || /^[a-z][a-z0-9+.-]*:/i.test(value) || value.startsWith('//')) return null;
  return resolve ? resolve(value) || null : null;
}

// The inline content of node, from..to, as text and {tag, children} parts.
function inlineParts(doc, node, from = node.from, to = node.to) {
  const parts = [];
  let at = from;
  for (let child = node.firstChild; child; child = child.nextSibling) {
    if (child.to <= from || child.from >= to) continue;
    if (child.from > at) parts.push(doc.sliceString(at, child.from));
    parts.push(...inlinePart(doc, child));
    at = child.to;
  }
  if (at < to) parts.push(doc.sliceString(at, to));
  return parts;
}
function inlinePart(doc, node) {
  const inner = () => inlineParts(doc, node, node.firstChild.to, node.lastChild.from);
  switch (node.name) {
    case 'Emphasis':
      return [{tag: 'em', children: inner()}];
    case 'StrongEmphasis':
      return [{tag: 'strong', children: inner()}];
    case 'Strikethrough':
      return [{tag: 'del', children: inner()}];
    case 'InlineCode':
      return [{tag: 'code', text: doc.sliceString(node.firstChild.to, node.lastChild.from)}];
    case 'Escape':
      return [doc.sliceString(node.from + 1, node.to)];
    case 'WikiLink': {
      const label = node.getChild('WikiLinkLabel') || node.getChild('WikiLinkTarget');
      return [{tag: 'wiki', text: doc.sliceString(label.from, label.to)}];
    }
    case 'Link':
    case 'Image': {
      const [open, close] = node.getChildren('LinkMark');
      return close ? [{tag: 'a', text: doc.sliceString(open.to, close.from)}] : [doc.sliceString(node.from, node.to)];
    }
    default:
      return [doc.sliceString(node.from, node.to)];
  }
}

// The cells of a table row: the text between its pipes, without the empty
// outside of a leading or trailing pipe. Lezer makes cell nodes only for
// cells with content.
function rowCells(doc, row) {
  const pipes = [], cells = [], segments = [];
  for (let child = row.firstChild; child; child = child.nextSibling) (child.name === 'TableDelimiter' ? pipes : cells).push(child);
  let start = row.from;
  for (const pipe of pipes) {
    segments.push([start, pipe.from]);
    start = pipe.to;
  }
  segments.push([start, row.to]);
  const blank = ([from, to]) => !doc.sliceString(from, to).trim();
  if (pipes.length && blank(segments[0])) segments.shift();
  if (pipes.length && segments.length && blank(segments[segments.length - 1])) segments.pop();
  return segments.map(([from, to]) => {
    const cell = cells.find(node => node.from >= from && node.to <= to);
    return {cell, from, to};
  });
}

// In a table, \| is a pipe even inside code (GFM).
const unescapePipes = parts => parts.map(part => (typeof part === 'string' ? part.replace(/\\\|/g, '|')
  : part.text != null ? {...part, text: part.text.replace(/\\\|/g, '|')} : {...part, children: unescapePipes(part.children)}));

// What a table widget draws: header and body cells (inline parts, and the
// offset of their text from the table's first line), and the alignments.
export function tableModel(doc, table) {
  const start = doc.lineAt(table.from).from;
  const cells = row => rowCells(doc, row).map(({cell, from, to}) => ({
    parts: cell ? unescapePipes(inlineParts(doc, cell)) : [],
    offset: (cell ? cell.from : Math.min(from + 1, to)) - start,
  }));
  const header = table.getChild('TableHeader');
  const delimiter = table.getChild('TableDelimiter');
  const columns = header ? cells(header) : [];
  const align = delimiter
    ? doc.sliceString(delimiter.from, delimiter.to).replace(/^\s*\|/, '').replace(/\|\s*$/, '').split('|').map(spec => {
      spec = spec.trim();
      return spec.startsWith(':') && spec.endsWith(':') ? 'center' : spec.endsWith(':') ? 'right' : spec.startsWith(':') ? 'left' : '';
    })
    : [];
  const fit = list => {
    const result = list.slice(0, columns.length);
    while (result.length < columns.length) result.push({parts: [], offset: result.length ? result[result.length - 1].offset : 0});
    return result;
  };
  return {header: columns, align, rows: table.getChildren('TableRow').map(row => fit(cells(row)))};
}

// The Properties summary's fields: each "key: value" line, its key what comes
// before the first colon (not starting with a space, ':' or '#'), its value
// trimmed and unquoted. A value with a line separator inside (a lone \r,
// U+2028 or U+2029) is no field. The line is read with indexOf and trim: a
// regular expression trimming the value costs the square of a run of spaces.
function frontMatterFields(doc, range) {
  const fields = [];
  for (let line of doc.sliceString(range.from, range.to).split('\n').slice(1, -1)) {
    if (line.endsWith('\r')) line = line.slice(0, -1);
    const colon = line.indexOf(':');
    if (colon < 1 || /[\s#]/.test(line[0])) continue;
    const value = line.slice(colon + 1).trim();
    if (!/[\r\u2028\u2029]/.test(value)) fields.push([line.slice(0, colon), value.replace(/^(["'])(.*)\1$/, '$2')]);
  }
  return fields;
}

// A paragraph that is only a link titled "video" gets a player after it, as
// the reader's renderVideos gives one.
function videoLink(doc, paragraph) {
  const link = paragraph.firstChild;
  if (!link || link.name !== 'Link' || link.nextSibling || link.from !== paragraph.from || link.to !== paragraph.to) return null;
  const title = link.getChild('LinkTitle'), url = link.getChild('URL');
  if (!title || !url || doc.sliceString(title.from + 1, title.to - 1) !== 'video') return null;
  return doc.sliceString(url.from, url.to);
}

// A rule alone on its line is a block widget; one after a quote or list
// marker stays inline.
const ruleLine = (doc, node) => {
  const line = doc.lineAt(node.from);
  return !doc.sliceString(line.from, node.from).trim() && !doc.sliceString(node.to, line.to).trim() ? line : null;
};

// The block decorations of the cover ranges, as ranges: front matter (when
// withFront), tables and lone rules become widgets unless the selection
// touches them, and video links get a player. selection is null when the
// editor does not have focus.
function blockRanges(state, cover, selection, {tree = syntaxTree(state), front = frontMatter(state.doc), withFront = true} = {}) {
  const doc = state.doc, result = [], seen = new Set(), video = state.facet(settingsFacet).video;
  if (front && withFront && !touches(selection, front.from, front.to)) {
    const widget = new FrontMatterWidget(frontMatterFields(doc, front), doc.sliceString(front.from, front.to));
    result.push(Decoration.replace({widget, block: true}).range(front.from, front.to));
  }
  for (const {from, to} of cover) {
    tree.iterate({
      from,
      to,
      enter: node => {
        const name = node.name;
        if (name === 'Document' || name === 'Blockquote' || name === 'BulletList' || name === 'OrderedList' || name === 'ListItem') return true;
        if ((front && node.from <= front.to) || seen.has(node.from)) return false;
        seen.add(node.from);
        if (name === 'Table') {
          const start = doc.lineAt(node.from).from, end = doc.lineAt(node.to).to;
          if (!touches(selection, start, end)) {
            const widget = new TableWidget(tableModel(doc, node.node), doc.sliceString(start, end));
            result.push(Decoration.replace({widget, block: true}).range(start, end));
          }
        } else if (name === 'HorizontalRule') {
          const line = ruleLine(doc, node);
          if (line && !touches(selection, line.from, line.to)) result.push(Decoration.replace({widget: rule, block: true}).range(line.from, line.to));
        } else if (name === 'Paragraph' && video) {
          const href = videoLink(doc, node.node);
          if (href) result.push(Decoration.widget({widget: new VideoWidget(href, heldVideo(state, href, node.to)), block: true, side: 1}).range(node.to));
        }
        return false;
      },
    });
  }
  return result;
}
export function blockDecorations(state, cover, selection, settings = {}) {
  return Decoration.set(blockRanges(state, cover, selection, settings), true);
}

// The inline decorations of ranges (which must not overlap). selection is
// null when the editor does not have focus: then no markers show.
export function inlineDecorations(state, ranges, selection, {tree = syntaxTree(state), front = frontMatter(state.doc), resolveImage, definitions: defined = linkDefinitions(state)} = {}) {
  const doc = state.doc, out = [];
  const active = (from, to) => touches(selection, from, to);
  const lineActive = pos => {
    const line = doc.lineAt(pos);
    return active(line.from, line.to);
  };
  const add = (from, to, decoration) => {
    if (from < to) out.push(decoration.range(from, to));
  };
  // Hidden text never includes a line break: only directly provided
  // decorations may replace those.
  const hide = (from, to) => {
    while (from < to) {
      const end = Math.min(to, doc.lineAt(from).to);
      add(from, end, hidden);
      from = end + 1;
    }
  };
  const replace = (from, to, widget) => {
    if (doc.lineAt(from).to >= to) out.push(Decoration.replace({widget}).range(from, to));
  };
  const eachLine = (from, to, range, decoration) => {
    for (let pos = Math.max(from, range.from); pos <= Math.min(to, range.to);) {
      const line = doc.lineAt(pos);
      out.push((typeof decoration === 'function' ? decoration(line) : decoration).range(line.from));
      pos = line.to + 1;
    }
  };
  const spaceAfter = pos => {
    while (pos < doc.length && /[ \t]/.test(doc.sliceString(pos, pos + 1))) pos++;
    return pos;
  };
  // Emphasis-like elements: the content is styled, the delimiters hidden.
  const delimited = (node, className) => {
    add(node.from, node.to, mark(className));
    if (active(node.from, node.to)) return;
    const open = node.firstChild, close = node.lastChild;
    if (open && close && open !== close) {
      hide(open.from, open.to);
      hide(close.from, close.to);
    }
  };
  // The address of a link or image without one in parentheses: '' for
  // [text](), the note's definition for [text], [text][] and [text][label],
  // and undefined (not a link) when there is no such definition.
  const reference = (node, open, close) => {
    const label = node.getChild('LinkLabel'), linkMarks = node.getChildren('LinkMark');
    if (!label && linkMarks.length > 2) return '';
    const key = label && label.to - label.from > 2 ? doc.sliceString(label.from + 1, label.to - 1) : doc.sliceString(open.to, close.from);
    return label || linkMarks.length === 2 ? defined.get(key) : undefined;
  };
  // Where a list item's prefix starts: at its indentation, but after the one
  // space a hidden quote marker takes with it.
  const prefixStart = listMark => {
    const line = doc.lineAt(listMark.from);
    let start = listMark.from;
    while (start > line.from && /[ \t]/.test(doc.sliceString(start - 1, start))) start--;
    if (start > line.from && start < listMark.from && doc.sliceString(start - 1, start) === '>') start++;
    return start;
  };
  const afterSpace = pos => (/[ \t]/.test(doc.sliceString(pos, pos + 1)) ? pos + 1 : pos);
  let quoteDepth = 0, listDepth = 0, frontStyled = false;
  const quoted = new Map();

  for (const range of ranges) {
    tree.iterate({
      from: range.from,
      to: range.to,
      enter: ref => {
        const name = ref.name;
        if (name === 'Document') return true;
        // Front matter is a widget unless the selection touches it.
        if (front && ref.from <= front.to) {
          if (!frontStyled && active(front.from, front.to)) eachLine(front.from, front.to, range, lineClass('cm-lp-frontmatter'));
          frontStyled = true;
          return false;
        }
        const node = ref.node;
        switch (name) {
          case 'ATXHeading1': case 'ATXHeading2': case 'ATXHeading3':
          case 'ATXHeading4': case 'ATXHeading5': case 'ATXHeading6': {
            out.push(headingLine(Number(name.slice(-1))).range(doc.lineAt(node.from).from));
            if (lineActive(node.from)) return true;
            for (const headerMark of node.getChildren('HeaderMark')) {
              if (headerMark.from === node.from) hide(headerMark.from, spaceAfter(headerMark.to));
              else {
                let start = headerMark.from;
                while (start > node.from && /[ \t]/.test(doc.sliceString(start - 1, start))) start--;
                hide(start, headerMark.to);
              }
            }
            return true;
          }
          case 'SetextHeading1': case 'SetextHeading2': {
            const underline = node.getChild('HeaderMark'), level = Number(name.slice(-1)), first = doc.lineAt(node.from).from;
            eachLine(node.from, underline ? underline.from - 1 : node.to, range, line => (line.from === first ? headingLine(level) : lineClass('cm-lp-heading cm-lp-h' + level)));
            if (underline && !active(node.from, node.to)) hide(underline.from, underline.to);
            return true;
          }
          case 'Emphasis':
            delimited(node, 'cm-lp-em');
            return true;
          case 'StrongEmphasis':
            delimited(node, 'cm-lp-strong');
            return true;
          case 'Strikethrough':
            delimited(node, 'cm-lp-strike');
            return true;
          case 'InlineCode':
            delimited(node, 'cm-lp-inline-code');
            return false;
          case 'Escape':
            if (!active(node.from, node.to)) hide(node.from, node.from + 1);
            return false;
          case 'Entity':
            if (!active(node.from, node.to)) replace(node.from, node.to, new EntityWidget(doc.sliceString(node.from, node.to)));
            return false;
          case 'HardBreak':
            if (doc.sliceString(node.from, node.from + 1) === '\\' && !lineActive(node.from)) hide(node.from, node.from + 1);
            return false;
          case 'Link': {
            const [open, close] = node.getChildren('LinkMark'), url = node.getChild('URL');
            if (!close) return true;
            // Brackets with no address and no definition stay as typed.
            const href = url ? doc.sliceString(url.from, url.to) : reference(node, open, close);
            if (href === undefined) return true;
            add(open.to, close.from, Decoration.mark({class: 'cm-lp-link', attributes: href ? {'data-lp-href': href, title: href} : {}}));
            if (!active(node.from, node.to)) {
              hide(open.from, open.to);
              hide(close.from, node.to);
            }
            return true;
          }
          case 'Autolink': {
            const url = node.getChild('URL');
            if (url) add(url.from, url.to, Decoration.mark({class: 'cm-lp-link', attributes: {'data-lp-href': doc.sliceString(url.from, url.to)}}));
            if (!active(node.from, node.to)) for (const linkMark of node.getChildren('LinkMark')) hide(linkMark.from, linkMark.to);
            return false;
          }
          case 'URL': {
            const parent = node.parent?.name;
            if (parent !== 'Link' && parent !== 'Image' && parent !== 'LinkReference') {
              add(node.from, node.to, Decoration.mark({class: 'cm-lp-link', attributes: {'data-lp-href': doc.sliceString(node.from, node.to)}}));
            }
            return false;
          }
          case 'Image': {
            const [open, close] = node.getChildren('LinkMark'), url = node.getChild('URL');
            const source = url ? doc.sliceString(url.from, url.to) : close ? reference(node, open, close) ?? '' : '';
            const widget = new ImageWidget(imageURL(source, resolveImage), close ? doc.sliceString(open.to, close.from) : '', source);
            if (!active(node.from, node.to) && doc.lineAt(node.from).to >= node.to) replace(node.from, node.to, widget);
            else {
              add(node.from, node.to, mark('cm-lp-image-source'));
              out.push(Decoration.widget({widget, side: 1}).range(node.to));
            }
            return false;
          }
          case 'WikiLink': {
            const target = node.getChild('WikiLinkTarget'), label = node.getChild('WikiLinkLabel'), shown = label || target;
            add(shown.from, shown.to, Decoration.mark({class: 'cm-lp-wikilink', attributes: {'data-lp-wiki': doc.sliceString(target.from, target.to)}}));
            if (!active(node.from, node.to)) {
              hide(node.from, shown.from);
              hide(shown.to, node.to);
            }
            return false;
          }
          case 'FootnoteReference':
            add(node.from, node.to, mark('cm-lp-footnote-ref'));
            return false;
          case 'FootnoteDefinition': {
            eachLine(node.from, node.to, range, lineClass('cm-lp-footnote'));
            const footnote = node.getChild('FootnoteLabel');
            if (footnote) add(footnote.from, footnote.to, mark('cm-lp-footnote-ref'));
            return true;
          }
          case 'ListItem':
            listDepth++;
            return true;
          case 'ListMark': {
            const ordered = node.parent?.parent?.name === 'OrderedList';
            const task = node.nextSibling?.name === 'Task' ? node.nextSibling.getChild('TaskMarker') : null;
            const start = prefixStart(node), end = afterSpace(task ? task.to : node.to), text = doc.sliceString(node.from, node.to);
            const line = doc.lineAt(node.from);
            const width = task ? (ordered ? `${text.length + 1}ch + 1.6em` : '1.6em') : ordered ? `${text.length}ch + 0.5em` : '1.4em';
            if (line.from === doc.lineAt(start).from) out.push(hangLine(Math.max(0, listDepth - 1), width).range(line.from));
            const revealed = task || !LIST_MARKS_ON_CARET_LINE ? active(start, end) : lineActive(node.from);
            if (end > line.to || revealed) {
              add(node.from, node.to, mark(ordered ? 'cm-lp-list-number' : 'cm-lp-list-mark'));
              if (task) add(task.from, task.to, mark('cm-lp-task-marker'));
            } else if (task) {
              const checked = /x/i.test(doc.sliceString(task.from, task.to));
              replace(start, end, new TaskWidget(checked, task.from + 1 - start, state.readOnly, ordered ? text : ''));
            } else replace(start, end, ordered ? new NumberWidget(text) : bullet);
            return false;
          }
          case 'Task': {
            const marker = node.getChild('TaskMarker');
            if (marker && /x/i.test(doc.sliceString(marker.from, marker.to))) add(spaceAfter(marker.to), node.to, mark('cm-lp-task-done'));
            return true;
          }
          case 'TaskMarker':
            return false;
          case 'Blockquote': {
            quoteDepth++;
            for (let pos = Math.max(node.from, range.from); pos <= Math.min(node.to, range.to);) {
              const line = doc.lineAt(pos);
              quoted.set(line.from, Math.max(quoted.get(line.from) || 0, quoteDepth));
              pos = line.to + 1;
            }
            return true;
          }
          case 'QuoteMark': {
            if (!lineActive(node.from)) hide(node.from, Math.min(node.to + (doc.sliceString(node.to, node.to + 1) === ' ' ? 1 : 0), doc.lineAt(node.from).to));
            return false;
          }
          case 'FencedCode': {
            const fences = node.getChildren('CodeMark'), info = node.getChild('CodeInfo');
            const first = doc.lineAt(node.from), last = doc.lineAt(node.to);
            const closed = fences.length > 1 && doc.lineAt(fences[fences.length - 1].from).number > first.number;
            eachLine(node.from, node.to, range, line => lineClass(
              'cm-lp-codeblock' + (line.from === first.from ? ' cm-lp-codeblock-first' : '') + (closed && line.from === last.from ? ' cm-lp-codeblock-last' : ''),
            ));
            // The fences also show while the caret is just outside them (at
            // the end of the line before, or the start of the line after),
            // where Delete or Backspace would join that line to a fence.
            if (!active(Math.max(0, node.from - 1), Math.min(doc.length, node.to + 1))) {
              if (first.from >= range.from) replace(fences[0].from, first.to, new LanguageWidget(info ? doc.sliceString(info.from, info.to) : ''));
              if (closed) hide(fences[fences.length - 1].from, fences[fences.length - 1].to);
            } else for (const fence of fences) add(fence.from, fence.from === fences[0].from ? first.to : fence.to, mark('cm-lp-code-fence'));
            return false;
          }
          case 'CodeBlock':
            eachLine(node.from, node.to, range, lineClass('cm-lp-codeblock cm-lp-codeblock-indented'));
            return false;
          case 'HorizontalRule':
            // A rule alone on its line is the block field's widget.
            if (lineActive(node.from)) add(node.from, node.to, mark('cm-lp-hr-source'));
            else if (!ruleLine(doc, node)) replace(node.from, node.to, inlineRule);
            return false;
          case 'HTMLBlock': case 'CommentBlock': case 'ProcessingInstructionBlock':
            eachLine(node.from, node.to, range, lineClass('cm-lp-html'));
            return false;
          case 'HTMLTag': case 'Comment': case 'ProcessingInstruction':
            add(node.from, node.to, mark('cm-lp-html-inline'));
            return false;
          case 'LinkReference':
            eachLine(node.from, node.to, range, lineClass('cm-lp-definition'));
            return false;
          case 'Table':
            // A widget unless the selection touches it (blockDecorations);
            // its source lines are styled for when they show.
            eachLine(node.from, node.to, range, lineClass('cm-lp-table-source'));
            return false;
          default:
            return true;
        }
      },
      leave: ref => {
        if (ref.name === 'Blockquote') quoteDepth--;
        else if (ref.name === 'ListItem') listDepth--;
      },
    });
  }
  for (const [from, depth] of quoted) out.push(quoteLine(Math.min(depth, 6)).range(from));
  return Decoration.set(out, true);
}

// ---------------------------------------------------------------------------
// State: whether the editor (or its search panel) has focus, and the block
// decorations of a range around the viewport.

// Whether the editor has focus (markers show only then); exported for tests.
export const setFocus = StateEffect.define();
const setCover = StateEffect.define();
const refresh = StateEffect.define();
const COVER_MARGIN = 20000;

const focus = StateField.define({
  create: () => false,
  update(value, tr) {
    for (const effect of tr.effects) if (effect.is(setFocus)) value = effect.value;
    return value;
  },
});

// Sorted, merged ranges.
function merge(ranges) {
  const sorted = ranges.filter(range => range.to >= range.from).sort((a, b) => a.from - b.from), out = [];
  for (const range of sorted) {
    const last = out[out.length - 1];
    if (last && range.from <= last.to + 1) last.to = Math.max(last.to, range.to);
    else out.push({from: range.from, to: range.to});
  }
  return out;
}
// The top-level blocks around [from, to] in tree, as whole lines.
function blockSpan(doc, tree, from, to) {
  let start = doc.lineAt(from).from, end = doc.lineAt(to).to;
  for (const block of topBlocks(tree, start, end)) {
    start = Math.min(start, block.from);
    end = Math.max(end, block.to);
  }
  return {from: doc.lineAt(start).from, to: doc.lineAt(Math.min(end, doc.length)).to};
}
const sameRanges = (a, b) => a === b || (!!a && !!b && a.length === b.length && a.every((range, i) => range.from === b[i].from && range.to === b[i].to));

const blocks = StateField.define({
  create(state) {
    const front = frontMatter(state.doc), cover = [{from: 0, to: Math.min(state.doc.length, COVER_MARGIN)}];
    return {cover, front, tree: syntaxTree(state), selection: null, stale: false, decorations: blockDecorations(state, cover, null, {front})};
  },
  update(value, tr) {
    let {cover, front} = value, covered = false, refreshed = false;
    for (const effect of tr.effects) {
      if (effect.is(setCover)) {
        cover = effect.value;
        covered = true;
      }
      if (effect.is(refresh)) refreshed = true;
    }
    const startFront = front;
    if (tr.docChanged) {
      cover = cover.map(({from, to}) => ({from: tr.changes.mapPos(from, -1), to: tr.changes.mapPos(to, 1)}));
      // Front matter is read again only after an edit near it.
      const old = tr.startState.doc, candidate = /^---\r?$/.test(old.line(1).text);
      const reach = front ? front.to + 1 : candidate ? FRONT_MATTER_READ : old.line(1).to + 1;
      if (tr.changes.touchesRange(0, Math.min(reach, old.length))) front = frontMatter(tr.newDoc);
    }
    // An IME composition is never redrawn under the user: decorations only
    // move with it, and are built again when it ends.
    if (tr.isUserEvent('input.type.compose') && !covered) {
      return {...value, cover, front, stale: true, decorations: value.decorations.map(tr.changes)};
    }
    // A reconfiguration that adds live preview updates the new field with a
    // start state that has none of it.
    const state = tr.state, focused = state.field(focus), selection = focused ? state.selection.ranges : null;
    const tree = syntaxTree(state);
    // A draft that becomes private or saved, or a video loaded, redraws its videos.
    const full = value.stale || covered || refreshed || startFront?.to !== front?.to || !!value.selection !== !!selection
      || (!tr.docChanged && tree !== value.tree) || heldVersion(tr.startState) !== heldVersion(state);
    if (full) return {cover, front, tree, selection, stale: false, decorations: blockDecorations(state, cover, selection, {tree, front})};
    const selectionMoved = !!selection && !sameRanges(value.selection, selection);
    if (!tr.docChanged && !selectionMoved) return value;
    // Only the top-level blocks an edit or the selection touched (before or
    // after) are built again; every other widget moves with the text.
    const doc = state.doc, dirty = [];
    if (tr.docChanged) {
      tr.changes.iterChangedRanges((fromA, toA, fromB, toB) => {
        dirty.push(blockSpan(doc, tree, fromB, toB));
        for (const old of topBlocks(value.tree, fromA, toA)) dirty.push({from: tr.changes.mapPos(old.from, -1), to: tr.changes.mapPos(old.to, 1)});
      });
    }
    for (const range of value.selection || []) dirty.push(blockSpan(doc, tree, tr.changes.mapPos(range.from, -1), tr.changes.mapPos(range.to, 1)));
    for (const range of selection || []) dirty.push(blockSpan(doc, tree, range.from, range.to));
    const regions = merge(dirty.map(range => ({from: Math.max(0, range.from), to: Math.min(doc.length, range.to)})));
    const overlaps = (from, to) => regions.some(region => from <= region.to && to >= region.from);
    const inCover = merge(regions.flatMap(region => cover
      .map(range => ({from: Math.max(region.from, range.from), to: Math.min(region.to, range.to)}))
      .filter(range => range.from <= range.to)));
    const withFront = !!front && overlaps(front.from, front.to);
    let decorations = tr.docChanged ? value.decorations.map(tr.changes) : value.decorations;
    decorations = decorations.update({
      filter: (from, to) => !overlaps(from, to),
      filterFrom: regions.length ? regions[0].from : 0,
      filterTo: regions.length ? regions[regions.length - 1].to : 0,
      add: blockRanges(state, inCover, selection, {tree, front, withFront}),
      sort: true,
    });
    return {cover, front, tree, selection, stale: false, decorations};
  },
  provide: field => EditorView.decorations.from(field, value => value.decorations),
});

function schedule(view, plugin, effects) {
  queueMicrotask(() => {
    if (plugin.destroyed) return;
    const list = effects().filter(Boolean);
    if (list.length) view.dispatch({effects: list});
  });
}

const inline = settings => ViewPlugin.fromClass(class {
  constructor(view) {
    this.decorations = this.build(view);
    this.stale = false;
    this.covering = false;
    this.cover(view);
  }
  build(view) {
    const selection = view.state.field(focus) ? view.state.selection.ranges : null;
    return inlineDecorations(view.state, view.visibleRanges, selection, {
      front: view.state.field(blocks).front, resolveImage: settings.resolveImage, definitions: view.state.field(definitions),
    });
  }
  update(update) {
    const {view} = update;
    if (view.composing || view.compositionStarted) {
      if (update.docChanged) this.decorations = this.decorations.map(update.changes);
      this.stale = true;
      return;
    }
    if (this.stale || update.docChanged || update.viewportChanged || update.selectionSet
      || update.startState.field(focus, false) !== update.state.field(focus)
      || update.startState.readOnly !== update.state.readOnly
      || syntaxTree(update.startState) !== syntaxTree(update.state)
      || update.transactions.some(tr => tr.effects.some(effect => effect.is(refresh)))) {
      this.decorations = this.build(view);
      this.stale = false;
    }
    if (update.viewportChanged || update.docChanged) this.cover(view);
  }
  // The block field follows the viewport with a margin, so a table or front
  // matter scrolled near is already a widget when it shows.
  cover(view) {
    const {from, to} = view.viewport;
    if (this.covering || view.state.field(blocks).cover.some(range => range.from <= from && range.to >= to)) return;
    this.covering = true;
    schedule(view, this, () => {
      this.covering = false;
      const {from, to} = view.viewport, length = view.state.doc.length;
      if (view.state.field(blocks).cover.some(range => range.from <= from && range.to >= to)) return [];
      return [setCover.of([{from: Math.max(0, from - COVER_MARGIN), to: Math.min(length, to + COVER_MARGIN)}])];
    });
  }
  destroy() {
    this.destroyed = true;
  }
}, {
  decorations: plugin => plugin.decorations,
  eventHandlers: {
    focusin(_, view) {
      schedule(view, this, () => [view.state.field(focus) ? null : setFocus.of(true)]);
    },
    focusout(_, view) {
      schedule(view, this, () => [view.state.field(focus) && !view.dom.contains(document.activeElement) ? setFocus.of(false) : null]);
    },
    compositionend(_, view) {
      const settle = (tries = 0) => setTimeout(() => {
        if (this.destroyed) return;
        if (view.composing && tries < 20) return settle(tries + 1);
        view.dispatch({effects: refresh.of(null)});
      }, 0);
      settle();
    },
    // Ctrl/Cmd-click follows a link when the host gives openLink; a plain
    // click edits it. (The app's link handling is editor-links.mjs.)
    mousedown(event, view) {
      if (event.button !== 0 || !(event.metaKey || event.ctrlKey) || !settings.openLink) return false;
      const target = event.target.closest?.('[data-lp-href], [data-lp-wiki]');
      if (!target || !view.contentDOM.contains(target)) return false;
      event.preventDefault();
      settings.openLink(target.dataset.lpHref != null ? {href: target.dataset.lpHref} : {wiki: target.dataset.lpWiki});
      return true;
    },
  },
});

// Reading font sizes follow the app's rendered notes (styles/reader.css);
// the app's styles/live-preview.css sets the font and the measure.
const theme = EditorView.baseTheme({
  '&': {'--lp-list-step': '1.5em'},
  '.cm-line.cm-lp-heading': {fontWeight: '600', letterSpacing: '-0.02em'},
  '.cm-line.cm-lp-h1': {fontSize: '1.79em', lineHeight: '1.25', paddingTop: '0.3em'},
  '.cm-line.cm-lp-h2': {fontSize: '1.32em', lineHeight: '1.35', paddingTop: '0.5em'},
  '.cm-line.cm-lp-h3': {fontSize: '1.15em', lineHeight: '1.4', paddingTop: '0.4em'},
  '.cm-lp-em': {fontStyle: 'italic'},
  '.cm-lp-strong': {fontWeight: '700'},
  '.cm-lp-strike': {textDecoration: 'line-through'},
  '.cm-lp-inline-code': {fontFamily: 'ui-monospace, monospace', fontSize: '0.85em', backgroundColor: 'var(--sidebar, #f0efe9)', borderRadius: '4px', padding: '0.1em 0.25em'},
  '.cm-lp-link': {color: 'var(--accent, #246aa2)', textDecoration: 'underline', textUnderlineOffset: '0.15em'},
  '.cm-lp-wikilink': {color: 'var(--accent, #246aa2)', textDecoration: 'underline dotted', textUnderlineOffset: '0.15em'},
  '.cm-lp-footnote-ref': {color: 'var(--accent, #246aa2)', fontSize: '0.8em'},
  '.cm-line.cm-lp-footnote': {fontSize: '0.9em', color: 'var(--muted, #5a655f)'},
  // Hanging indent: the first line starts at the prefix box, wrapped lines
  // under the item's text.
  '.cm-line.cm-lp-list': {paddingLeft: 'calc(6px + var(--lp-hang))', textIndent: 'calc(-1 * var(--lp-hang))'},
  '.cm-line.cm-lp-quote.cm-lp-list': {paddingLeft: 'calc(var(--lp-quote-depth) * 16px + 6px + var(--lp-hang))'},
  '.cm-lp-list-prefix': {display: 'inline-block', width: 'var(--lp-hang)', textIndent: '0', textAlign: 'right', paddingRight: '0.45em', boxSizing: 'border-box', whiteSpace: 'nowrap'},
  '.cm-lp-bullet': {color: 'var(--muted, #5a655f)', fontWeight: '700'},
  '.cm-lp-list-number, .cm-lp-list-mark, .cm-lp-task-marker': {color: 'var(--muted, #5a655f)'},
  '.cm-lp-task': {width: '1em', height: '1em', margin: '0', verticalAlign: '-0.1em', cursor: 'pointer', accentColor: 'var(--accent, #246aa2)'},
  '.cm-lp-task:disabled': {cursor: 'default'},
  '.cm-lp-task-done': {color: 'var(--muted, #5a655f)', textDecoration: 'line-through'},
  '.cm-line.cm-lp-quote': {
    color: 'var(--muted, #5a655f)',
    paddingLeft: 'calc(var(--lp-quote-depth) * 16px + 6px)',
    backgroundImage: 'repeating-linear-gradient(90deg, var(--border, #d9ded5) 0 3px, transparent 3px 16px)',
    backgroundSize: 'calc(var(--lp-quote-depth) * 16px) 100%',
    backgroundRepeat: 'no-repeat',
  },
  '.cm-line.cm-lp-codeblock': {fontFamily: 'ui-monospace, monospace', fontSize: '0.85em', lineHeight: '1.6', backgroundColor: 'var(--sidebar, #f0efe9)', paddingInline: '14px'},
  '.cm-line.cm-lp-codeblock-first': {borderRadius: '6px 6px 0 0', paddingTop: '4px'},
  '.cm-line.cm-lp-codeblock-last': {borderRadius: '0 0 6px 6px', paddingBottom: '4px'},
  '.cm-lp-code-fence': {color: 'var(--muted, #5a655f)'},
  '.cm-lp-code-language': {float: 'right', fontFamily: 'var(--ui-font, sans-serif)', fontSize: '0.8em', color: 'var(--muted, #5a655f)'},
  '.cm-lp-hr': {height: '0', margin: '12px 6px', borderTop: '2px solid var(--border, #d9ded5)'},
  '.cm-lp-hr-inline': {display: 'inline-block', width: '100%', margin: '0', verticalAlign: 'middle'},
  '.cm-lp-hr-source': {color: 'var(--muted, #5a655f)'},
  '.cm-lp-image': {display: 'inline-block', maxWidth: '100%', maxHeight: '24em', verticalAlign: 'bottom', borderRadius: '4px'},
  '.cm-lp-image-missing': {color: 'var(--muted, #5a655f)', fontStyle: 'italic'},
  '.cm-lp-image-source': {color: 'var(--muted, #5a655f)'},
  '.cm-lp-video': {margin: '6px 6px 12px'},
  '.cm-lp-video-held': {
    padding: '10px 14px', border: '1px dashed var(--control-border, #858f86)', borderRadius: '6px',
    fontFamily: 'var(--ui-font, sans-serif)', fontSize: '0.8em', lineHeight: '1.5', color: 'var(--muted, #5a655f)',
  },
  '.cm-lp-video-held p': {margin: '0 0 8px'},
  '.cm-lp-video-held strong': {color: 'var(--text, inherit)', fontWeight: '600'},
  '.cm-lp-video-link': {fontFamily: 'ui-monospace, monospace', color: 'var(--text, inherit)', overflowWrap: 'anywhere'},
  '.cm-line.cm-lp-html, .cm-line.cm-lp-definition, .cm-line.cm-lp-frontmatter': {fontFamily: 'ui-monospace, monospace', fontSize: '0.85em', color: 'var(--muted, #5a655f)'},
  '.cm-line.cm-lp-frontmatter': {backgroundColor: 'var(--sidebar, #f0efe9)'},
  '.cm-lp-html-inline': {fontFamily: 'ui-monospace, monospace', fontSize: '0.85em', color: 'var(--muted, #5a655f)'},
  '.cm-line.cm-lp-table-source': {fontFamily: 'ui-monospace, monospace', fontSize: '0.85em'},
  '.cm-lp-table-wrap': {overflowX: 'auto', margin: '6px 0', padding: '0 6px', cursor: 'text'},
  '.cm-lp-table': {borderCollapse: 'collapse', fontSize: '0.9em', lineHeight: '1.55'},
  '.cm-lp-table th, .cm-lp-table td': {padding: '6px 12px', borderBottom: '1px solid var(--border, #d9ded5)', textAlign: 'left', verticalAlign: 'top', minWidth: '4em'},
  '.cm-lp-table th': {fontWeight: '600', borderBottomColor: 'var(--control-border, #858f86)'},
  '.cm-lp-table .numeric': {textAlign: 'right', fontVariantNumeric: 'tabular-nums'},
  '.cm-lp-frontmatter-summary': {
    display: 'flex', flexWrap: 'wrap', gap: '4px 14px', alignItems: 'baseline', margin: '4px 6px 10px', padding: '6px 10px',
    border: '1px solid var(--border, #d9ded5)', borderRadius: '6px', fontFamily: 'var(--ui-font, sans-serif)', fontSize: '0.75em',
    color: 'var(--muted, #5a655f)', cursor: 'text', overflow: 'hidden',
  },
  '.cm-lp-frontmatter-label': {fontWeight: '600', color: 'var(--text, inherit)'},
  '.cm-lp-frontmatter-field': {minWidth: '0', overflowWrap: 'anywhere'},
  '.cm-lp-frontmatter-key': {fontWeight: '600'},
});

// Up and down stop at a table, rule or front matter widget instead of
// passing over it: the caret lands on its first line going down and its last
// going up, which shows its source.
const enterBlock = forward => view => {
  const range = view.state.selection.main;
  if (!range.empty || !view.state.field(blocks, false)) return false;
  const line = view.state.doc.lineAt(range.head), next = view.moveVertically(range, forward).head;
  let target = null;
  view.state.field(blocks).decorations.between(forward ? line.to : next, forward ? next : line.from, (from, to) => {
    if (to === from) return;
    if (forward && from > line.to && from <= next && (target == null || from < target)) target = from;
    if (!forward && to < line.from && to >= next && (target == null || to > target)) target = to;
  });
  if (target == null) return false;
  view.dispatch({selection: {anchor: target}, scrollIntoView: true, userEvent: 'select'});
  return true;
};

// ---------------------------------------------------------------------------
// Deleting keys (plan risks 4 and 7). The browser deletes a table, rule or
// front matter widget next to the caret as if it were one character, with
// all of its hidden source, and a word deletion can end inside hidden
// markers. So with an empty selection, a Backspace or Delete (or a word or
// line variant) whose text touches hidden text is handled here: it deletes
// what Source would, but never into hidden text, where it stops; the caret
// then touches that text, which shows. Elsewhere the browser deletes as usual.

// Decision (provisional): Backspace at the start of the first line after
// front matter moves the caret into the front matter, which then shows its
// YAML, instead of joining the line onto the closing "---" (which would end
// the front matter). On an empty line it deletes the line break as usual.
const BACKSPACE_ENTERS_FRONT_MATTER = true;

// Where a deletion from pos ends, as Source's keys go: by a character (a
// grapheme), a group (a word, as CodeMirror's word deletion) or to the line's
// boundary (view gives the wrapped line's); at a line's edge, its line break.
export function deletionEnd(state, pos, forward, by = 'char', view = null) {
  const doc = state.doc, line = doc.lineAt(pos);
  if (pos === (forward ? line.to : line.from)) return forward ? Math.min(doc.length, pos + 1) : Math.max(0, pos - 1);
  if (by === 'char') return findClusterBreak(line.text, pos - line.from, forward) + line.from;
  if (by === 'line') {
    const end = view ? view.moveToLineBoundary(EditorSelection.cursor(pos), forward).head : forward ? line.to : line.from;
    return end === pos ? pos + (forward ? 1 : -1) : end;
  }
  const categorize = state.charCategorizer(pos);
  let at = pos;
  for (let category = null; at !== (forward ? line.to : line.from);) {
    const next = findClusterBreak(line.text, at - line.from, forward) + line.from;
    const char = line.text.slice(Math.min(at, next) - line.from, Math.max(at, next) - line.from), nextCategory = categorize(char);
    if (category != null && nextCategory !== category) break;
    if (char !== ' ' || at !== pos) category = nextCategory;
    at = next;
  }
  return at;
}

// The text the decoration sets hide that touches [from, to]: hidden markers,
// replacing widgets and block widgets, as {from, to, front}.
function hiddenAround(sets, from, to) {
  const out = [];
  for (const set of sets) {
    set.between(from, to, (start, end, value) => {
      if (start < end ? value === hidden || !!value.spec.widget : !!value.spec.block) {
        out.push({from: start, to: end, front: value.spec.widget instanceof FrontMatterWidget});
      }
    });
  }
  return out;
}

// The transaction a deleting key makes in Visual, given the decoration sets
// on screen (the block field's and the inline plugin's), or null when the
// browser may delete as usual: the selection is not one caret, or the text
// the key deletes touches nothing hidden.
export function guardedDeletion(state, sets, forward, by = 'char', view = null) {
  const {selection} = state, range = selection.main;
  if (state.readOnly || selection.ranges.length > 1 || !range.empty) return null;
  const pos = range.head, end = deletionEnd(state, pos, forward, by, view);
  if (end === pos) return null;
  let from = Math.min(pos, end), to = Math.max(pos, end);
  const near = hiddenAround(sets, from, to);
  if (!near.length) return null;
  const front = near.find(item => item.front && item.to === from);
  if (!forward && front && BACKSPACE_ENTERS_FRONT_MATTER && state.doc.lineAt(pos).length) {
    return {selection: EditorSelection.cursor(front.to), userEvent: 'select'};
  }
  for (const item of near) {
    if (item.from >= to || item.to <= from) continue;
    if (forward) to = Math.max(from, Math.min(to, item.from));
    else from = Math.min(to, Math.max(from, item.to));
  }
  if (from === to) return {selection: EditorSelection.cursor(from), userEvent: 'select'};
  return {changes: {from, to}, selection: EditorSelection.cursor(from), userEvent: forward ? 'delete.forward' : 'delete.backward'};
}

// The deleting keys, as CodeMirror's standard keymap has them: word deletion
// is Alt on a Mac, where Cmd deletes to the line's boundary and Ctrl-H,
// Ctrl-D and Ctrl-K delete too.
const deletionKeys = guard => [
  {key: 'Backspace', run: guard(false, 'char'), shift: guard(false, 'char')},
  {key: 'Delete', run: guard(true, 'char'), shift: guard(true, 'char')},
  {key: 'Mod-Backspace', mac: 'Alt-Backspace', run: guard(false, 'group')},
  {key: 'Mod-Delete', mac: 'Alt-Delete', run: guard(true, 'group')},
  {mac: 'Mod-Backspace', run: guard(false, 'line')},
  {mac: 'Mod-Delete', run: guard(true, 'line')},
  {mac: 'Ctrl-h', run: guard(false, 'char')},
  {mac: 'Ctrl-d', run: guard(true, 'char')},
  {mac: 'Ctrl-Alt-h', run: guard(false, 'group')},
  {mac: 'Ctrl-k', run: guard(true, 'line')},
];

// Markers and URLs that show are quiet; formatting comes from the
// decorations, so a [bracket] that is no link is not coloured as one.
const highlight = HighlightStyle.define([
  {tag: [tags.processingInstruction, tags.meta, tags.contentSeparator, tags.labelName, tags.url, tags.atom], color: 'var(--muted, #5a655f)'},
]);

// Live preview over a Markdown editor that uses livePreviewMarkdown().
// Settings: resolveImage(path) gives the URL of a notebook image (or null),
// video(href) a player element for a video link (held in a private draft,
// see privateVideos), openLink({href} | {wiki}) follows a Ctrl/Cmd-clicked
// link.
export function livePreview(settings = {}) {
  const preview = inline(settings);
  const guard = (forward, by) => view => {
    if (view.composing || !view.state.field(blocks, false)) return false;
    const sets = [view.state.field(blocks).decorations, view.plugin(preview)?.decorations ?? Decoration.none];
    const spec = guardedDeletion(view.state, sets, forward, by, view);
    if (!spec) return false;
    view.dispatch({...spec, scrollIntoView: true});
    return true;
  };
  return [
    settingsFacet.of(settings),
    focus,
    definitions,
    blocks,
    preview,
    Prec.high(keymap.of([
      {key: 'Mod-Enter', run: toggleTask},
      {key: 'ArrowDown', run: enterBlock(true)},
      {key: 'ArrowUp', run: enterBlock(false)},
      ...deletionKeys(guard),
    ])),
    syntaxHighlighting(highlight),
    theme,
  ];
}
