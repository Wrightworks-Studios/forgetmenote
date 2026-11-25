import {StateEffect, StateField} from '@codemirror/state';
import {Decoration, EditorView, ViewPlugin, showTooltip} from '@codemirror/view';
import {syntaxTree} from '@codemirror/language';
import {decodeString} from 'micromark-util-decode-string';
import {linkDefinitions} from './live-preview.mjs';

// Links in the note editor, in every mode. Live preview hides a link's
// syntax while the caret is elsewhere and shows its label; this module
// follows links and marks page links to pages that do not exist yet.
//
// links({open, pageTarget}) returns CodeMirror extensions. open({href} |
// {wiki}) follows a link by the reader's rules (the app keeps the draft as
// it does for any note it opens). pageTarget(name) is the path a [[name]]
// link opens, null when there is no such page, or a promise while the
// notebook's pages are not known yet; such links are marked when it settles.

// Decision 3 (provisional): a plain click edits a link (the caret goes in and
// its Markdown shows), Ctrl/Cmd+click follows it, and on a touch screen an
// Open button shows while the caret is in a link.
const follows = event => event.ctrlKey || event.metaKey;
const OPEN_BUTTON_ON_TOUCH = true;

// A link destination as the reader gets it from goldmark's HTML: without
// its <…> brackets (an inline link's; a definition's are gone already),
// with backslash escapes and character references decoded, and
// percent-encoded as goldmark's URLEscape does: the characters encodeURI
// keeps, and a % that starts an escape, stay as they are.
export function destination(text, brackets = true) {
  text = text.trim();
  if (brackets && text.length >= 2 && text.startsWith('<') && text.endsWith('>')) text = text.slice(1, -1);
  return decodeString(text).split(/(%[\da-fA-F]{2})/).map((part, i) => {
    if (i % 2) return part;
    try {
      return encodeURI(part);
    } catch {
      // A lone surrogate, which no saved note holds.
      return part;
    }
  }).join('');
}

// The address of a [text](address), [text][label], [text][] or [text] link
// node: '' for none, undefined when no definition names it.
function address(state, node) {
  const url = node.getChild('URL');
  if (url) return destination(state.sliceDoc(url.from, url.to));
  const label = node.getChild('LinkLabel'), marks = node.getChildren('LinkMark');
  if (!label && marks.length > 2) return '';
  if (marks.length < 2) return undefined;
  const key = label && label.to - label.from > 2 ? state.sliceDoc(label.from + 1, label.to - 1) : state.sliceDoc(marks[0].to, marks[1].from);
  const href = label || marks.length === 2 ? linkDefinitions(state).get(key) : undefined;
  return href == null ? undefined : destination(href, false);
}

// A link written as its address, as goldmark's autolinks read it: www. is
// a web address and an address with @ but no scheme an email address.
const written = url => (/^www\./i.test(url) ? 'http://' + url : !/^[a-z][a-z\d+.-]*:/i.test(url) && url.includes('@') ? 'mailto:' + url : url);

// The link at pos, as {href} or {wiki} with its source range, or null.
export function linkAt(state, pos) {
  const tree = syntaxTree(state);
  for (const side of [1, -1]) {
    for (let node = tree.resolveInner(pos, side); node; node = node.parent) {
      if (node.name === 'WikiLink') {
        const target = node.getChild('WikiLinkTarget'), name = target && state.sliceDoc(target.from, target.to).trim();
        return name ? {wiki: name, from: node.from, to: node.to} : null;
      }
      if (node.name === 'Link') {
        const href = address(state, node);
        if (href) return {href, from: node.from, to: node.to};
      }
      if (node.name === 'Autolink' || (node.name === 'URL' && !['Link', 'Image', 'LinkReference', 'Autolink'].includes(node.parent?.name))) {
        const url = node.name === 'URL' ? node : node.getChild('URL');
        if (url) return {href: written(state.sliceDoc(url.from, url.to)), from: node.from, to: node.to};
      }
    }
  }
  return null;
}

const refresh = StateEffect.define();

// How far a page link reaches (live-preview.mjs reads one within 1,000
// characters of its opening brackets).
const PAGE_LINK_REACH = 1000;

// Page links to missing pages, in the drawn part of the note. A key press
// only maps them, unless page link brackets are near the change.
const missingPages = pageTarget => ViewPlugin.fromClass(class {
  constructor(view) {
    this.decorations = this.build(view);
  }
  update(update) {
    const {state} = update;
    let rebuild = update.viewportChanged || update.transactions.some(tr => tr.effects.some(effect => effect.is(refresh)));
    if (!rebuild && update.docChanged) {
      update.changes.iterChangedRanges((_fromA, _toA, fromB, toB) => {
        const near = state.sliceDoc(Math.max(0, fromB - PAGE_LINK_REACH), Math.min(state.doc.length, toB + PAGE_LINK_REACH));
        if (near.includes('[[') || near.includes(']]')) rebuild = true;
      });
      if (!rebuild) this.decorations = this.decorations.map(update.changes);
    }
    // A tree that grows over the drawn text may hold page links.
    if (!rebuild && !update.docChanged && syntaxTree(update.startState) !== syntaxTree(state)) rebuild = true;
    if (rebuild) this.decorations = this.build(update.view);
  }
  build(view) {
    const marks = [], tree = syntaxTree(view.state);
    let waiting = null;
    for (const {from, to} of view.visibleRanges) {
      tree.iterate({from, to, enter: node => {
        if (node.name !== 'WikiLink') return;
        const target = node.node.getChild('WikiLinkTarget'), shown = node.node.getChild('WikiLinkLabel') || target;
        const name = target && view.state.sliceDoc(target.from, target.to).trim();
        if (!name) return false;
        const page = pageTarget(name);
        if (typeof page?.then === 'function') waiting = page;
        else if (page === null) marks.push(Decoration.mark({class: 'cm-lp-wikilink-missing', attributes: {title: 'No page named “' + name + '” yet'}}).range(shown.from, shown.to));
        return false;
      }});
    }
    // Marked again once the pages are known.
    if (waiting && !this.waiting) {
      this.waiting = waiting;
      waiting.then(() => {
        this.waiting = null;
        if (!this.destroyed) view.dispatch({effects: refresh.of(null)});
      }, () => {
        this.waiting = null;
      });
    }
    return Decoration.set(marks, true);
  }
  destroy() {
    this.destroyed = true;
  }
}, {decorations: plugin => plugin.decorations});

// Whether the last pointer in the editor was a finger (or a pen).
const setTouch = StateEffect.define();
const touch = StateField.define({
  create: () => false,
  update(value, tr) {
    for (const effect of tr.effects) if (effect.is(setTouch)) value = effect.value;
    return value;
  },
});

// The Open button above the link that holds the caret, on a touch screen.
const openButton = open => StateField.define({
  create: () => null,
  update(value, tr) {
    if (!tr.docChanged && !tr.selection && !tr.effects.some(effect => effect.is(setTouch))) return value;
    const {state} = tr, range = state.selection.main;
    const link = state.field(touch) && range.empty ? linkAt(state, range.head) : null;
    if (!link) return null;
    if (value && value.pos === link.from && value.link.href === link.href && value.link.wiki === link.wiki) return value;
    return {
      pos: link.from,
      above: true,
      link,
      create: () => {
        const dom = document.createElement('div'), button = document.createElement('button');
        dom.className = 'cm-lp-open-link';
        button.type = 'button';
        button.textContent = 'Open';
        button.title = link.href ?? link.wiki;
        button.setAttribute('aria-label', 'Open link ' + (link.href ?? link.wiki));
        // The caret and the on-screen keyboard stay.
        button.addEventListener('pointerdown', event => event.preventDefault());
        button.addEventListener('mousedown', event => event.preventDefault());
        button.addEventListener('click', () => open(link.href != null ? {href: link.href} : {wiki: link.wiki}));
        dom.append(button);
        return {dom};
      },
    };
  },
  provide: field => showTooltip.from(field),
});

const theme = EditorView.baseTheme({
  // A missing page reads as the reader shows it (styles/wiki.css), not as
  // live preview's page link around or inside it.
  '.cm-lp-wikilink-missing': {color: 'var(--muted, #5a655f)', textDecoration: 'underline dashed', textUnderlineOffset: '3px'},
  '.cm-lp-wikilink-missing .cm-lp-wikilink': {color: 'inherit', textDecoration: 'none'},
  '.cm-lp-wikilink:has(> .cm-lp-wikilink-missing)': {textDecoration: 'none'},
  '.cm-tooltip.cm-lp-open-link': {border: '0', background: 'transparent'},
  '.cm-lp-open-link button': {
    minHeight: '32px', padding: '2px 12px', border: '1px solid var(--border, #d9ded5)', borderRadius: '999px',
    background: 'var(--surface, #fff)', color: 'var(--accent, #246aa2)', font: '600 13px/1.4 var(--ui-font, sans-serif)', boxShadow: 'var(--shadow-popover, none)',
  },
});

export function links({open, pageTarget} = {}) {
  const extensions = [theme];
  if (pageTarget) extensions.push(missingPages(pageTarget));
  if (!open) return extensions;
  extensions.push(
    EditorView.domEventHandlers({
      mousedown(event, view) {
        if (event.button !== 0 || !follows(event)) return false;
        const pos = view.posAtCoords({x: event.clientX, y: event.clientY});
        const link = pos == null ? null : linkAt(view.state, pos);
        if (!link) return false;
        event.preventDefault();
        open(link.href != null ? {href: link.href} : {wiki: link.wiki});
        return true;
      },
      pointerdown(event, view) {
        const finger = OPEN_BUTTON_ON_TOUCH && (event.pointerType === 'touch' || event.pointerType === 'pen');
        if (finger !== view.state.field(touch)) view.dispatch({effects: setTouch.of(finger)});
        return false;
      },
    }),
    touch,
    openButton(open),
  );
  return extensions;
}
