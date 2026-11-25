# D2 editor bake-off: results

> **Superseded (the maintainer, 2026-11-23).** Visual mode is no longer Milkdown: it is a CodeMirror 6 inline live preview over the Markdown text, beside Source and Side by side ([storage-v2.md §10](storage-v2.md#10-editor-a-live-preview-over-markdown-text)). Milkdown, the block bridge (`frontend/markdown-bridge.mjs` and its tests) and section mode were removed from the product. This document is kept as the history of the D2 decision; the bridge's later rounds are on `wip/storage-v2-editor`. Current editor measurements are in [storage-v2-results-store.md](storage-v2-results-store.md).

> Copy of `proto/storage-v2/editor/RESULTS.md` (the prototypes, not merged); relative paths refer to that directory.

- **Date:** 2026-11-22
- **Plan:** `storage-v2-bakeoff.md` §3.2, §4 (editor rows) and §5 (D2 rules)
- **Production code:** `frontend/markdown-bridge.mjs`, tested by `internal/web/ui/markdown-bridge.test.mjs` (`npm test`)
- **Prototypes (not merged):** `proto/storage-v2/editor/`. Raw results are in `results/raw/`, the property-test reports in `results/property-*.json`, every fidelity byte change in `results/fidelity/`, and bundle sizes in `results/editor-sizes.json`.

## Decision D2

1. **WYSIWYG library: keep Milkdown (E-MD), with Crepe's virtual caret turned off.** This is subject to the maintainer's hands-on check (below).
   - Tiptap does not pass more must rows. Its section mode measures 17.3 ms (median of 4 runs, unthrottled) against the 16 ms budget. Milkdown measures 15.3 ms once Crepe's `cursor` feature drops its virtual caret (`addFeature(cursor, {virtual: false})` keeps the drop and gap cursors). With the virtual caret, as production configures it today, Milkdown measures 23.2 ms: the caret costs about 7 ms of rendering per keystroke.
   - Tiptap's Markdown fidelity is clearly worse, not better.
     - On F1, 228 of 2,566 blocks would be rewritten on their first edit, against 55 with Milkdown: it turns every `_emphasis_` into `*emphasis*` and escapes `&`.
     - It drops raw HTML tags and HTML comments (content loss), footnotes, empty headings and empty list items.
     - It always escapes `[[wiki links]]`.
     - It emits list items its own schema rejects, which the adapter has to repair.
   - The features left out are cheaper in Milkdown: Crepe already ships a toolbar, a slash menu and math, and about 420 lines of today's editor integration are Milkdown code.
   - Tiptap's bundle is smaller (167 KiB against 216 KiB gzipped), but bundle size is a *should* row and no D2 rule uses it.
2. **Section mode: needed.**
   - Milkdown misses the keystroke budget on the full F1 even with its mitigations. With content-visibility and no virtual caret it measures 21.2 ms unthrottled (budget 16 ms; it passes the 4× budget at 23.2 ms). Spellcheck makes no difference.
   - **Threshold: 16 KiB of Markdown.** Notes over 16 KiB open in section mode.
     - 16 KiB is the largest size measured that meets the budget: 15.8 ms unthrottled and 15.5 ms at 4×, which is the frame floor.
     - The next size measured, 32 KiB, is already over: 17.6 ms, or 16.1 ms with content-visibility.
     - Below 16 KiB nothing is measurably faster than a bare contenteditable, so the headroom is the gap up to the first size that fails.
   - F1 has 15 chapters over 16 KiB; the largest is 45 KiB. Wave 2 should split a section that is itself over the threshold at its next heading level (open issue 3).
   - **Within noise.** The frame floor is 15.9 ms (1×) and 16.3 ms (4×), so two deciding comparisons sit within run-to-run noise: E-MD section with the plain caret (15.3 ms, 2 runs) against E-TT section (17.3 ms), and 16 KiB (15.8 ms) against 32 KiB with content-visibility (16.1 ms). The library choice does not rest on them, since a tie keeps Milkdown under D2. The threshold does: wave 2 should revisit it with the work p95 metric or with the 5 runs the plan asks for.
   - A section resolves references and footnotes against definitions anywhere in the note: `SectionBridge` scans the text around the section for definitions (never parsing it) and parses the section after them.
3. **Source mode: adopt CodeMirror 6 (E-CM6).**
   - It meets its budget on the full F1: 15.9 ms unthrottled and 16.3 ms at 4× (medians of 4 runs), with no long tasks under remote updates and a remote update applied in 1.4 ms at p95.
   - Today's `<textarea>` measures 19.3 ms and 53 ms, and reassigning it whole on each remote update produced 162 long tasks of up to 97 ms.

### How each rule was applied

| D2 rule | Evidence | Outcome |
| --- | --- | --- |
| E-TT passes more **must** rows than E-MD? | Keystroke row: E-MD (no virtual caret) passes in section mode; E-TT section misses unthrottled by 1.3 ms (median). Remote-update row: both pass in section mode. Bridge property row: identical production test; with the real editors both pass 10,000/10,000 on three seeds | No: E-TT passes fewer or equal |
| E-TT clearly better Markdown fidelity? | 228 vs 55 F1 blocks rewritten when edited; E-TT drops raw HTML, comments and footnotes, and escapes wiki links | No: clearly worse |
| E-TT makes the missing features clearly cheaper? | Toolbar, slash menu and math ship with Crepe; switching ports ~420 lines | No |
| Winner passes the hands-on check | Not run yet: needs the maintainer on a laptop (`demo.sh`) | **Pending** |
| Section mode not needed if the full F1 meets the budget with mitigations | E-MD full F1, content-visibility, no virtual caret: 21.2 ms (1×) | Needed; threshold 16 KiB |
| Adopt E-CM6 if it meets its budget on F1 | 15.9 ms (1×) / 16.3 ms (4×) | Adopt |

### What wave 2 has to carry over

- **The bridge.** Wire `MarkdownBridge`/`SectionBridge`, `ProseMirrorBinding` and `milkdownAdapter` from `frontend/markdown-bridge.mjs` into `frontend/milkdown.mjs` and `ui/markdown-editor.mjs`, in place of whole-document replace-and-diff.
- **Milkdown configuration.**
  - Drop Milkdown's `history` plugin: the engine's `Y.UndoManager` owns undo.
  - Drop `trailing`: it adds an empty paragraph with no Markdown.
  - Drop `listener`: it serializes the whole document on every change.
  - Configure `cursor` with `{virtual: false}`.
- **Section mode** above 16 KiB, with an outline (the prototype's `prose-variant.mjs` shows the wiring).

## How to try it (hands-on check)

This one command builds the prototypes in the dev image and serves them on `http://127.0.0.1:8765/`. From another machine, forward the port first with `ssh -L 8765:127.0.0.1:8765 HOST`.

```sh
~/src/fmn-wt/editor/proto/storage-v2/editor/demo.sh
```

- **What the script does.** It copies F1 from `~/forgetmenote-design/fixtures/` (the build checks its SHA-256), runs `npm ci` if needed and `node build.mjs` through `fmn-node`, then serves `dist/` from the same image with the port published on localhost only.
- **What the index page links.** E-MD and E-TT (full document and section mode), E-CM6, E-TA, today's Visual mode, the fidelity page and the property-test page.
- **Options to add to an E-MD link.**
  - `&cursor=plain`: Milkdown without the virtual caret, the recommended configuration.
  - `&cv=1`: content-visibility.
  - `&spell=0`: spellcheck off.
  - `&doc=stress.md`: the stress file.
- **What to check.** Twenty minutes of writing in E-MD (`?section=1&cursor=plain`), in E-TT, and in full-document mode. Note comfort, how switching sections feels, and any blocking gap. Undo is engine undo (Ctrl+Z); the remote peer is idle in the demo.

## Budgets (§4, editor rows)

The values are the median of each configuration's runs, measured in the middle of F1.

| Row | Level | E-MD | E-TT | E-CM6 | Today (E-TA / Visual) |
| --- | --- | --- | --- | --- | --- |
| Keystroke p95 < 16 ms (1×), full or section | must | **Pass** in section mode without the virtual caret: 15.3 ms. Fail with it: 23.2. Full F1, best: 21.2 | **Fail**: section 17.3 (15.1–17.9); full 21.1 | **Pass** 15.9 | Fail: 19.3 / 162 |
| Keystroke p95 < 50 ms (4×) | must | Pass: section 15.9; full + content-visibility 23.2; plain full 64.2 fails | Pass: section 18.1; full 37.9 | Pass 16.3 | Fail: 53 / 674 |
| Remote and agent updates: no long task over 50 ms, cost follows the change | must | **Pass** in section mode: 0 long tasks; apply p95 0.7 ms (1×) / 2.2 ms (4×); apply cost the same on a tenth of F1 (3.3 vs 3.0 ms, full document). Full document at 4×: 72 long tasks (keystroke rendering) | **Pass**: 0 long tasks in both modes; apply p95 0.7 / 2.7 ms (section) | **Pass**: 0 long tasks; apply p95 1.4 ms; size-independent | Fail: 162 long tasks (max 97 ms), cost grows with size / 98 long tasks, 856 ms per update |
| Bridge property test 10,000/10,000, untouched blocks byte-identical | must | Production test (`npm test`, library-agnostic): **10,000/10,000**, plus soaks clean (Bridge correctness). Through Milkdown's ProseMirror: **10,000/10,000** on three seeds | Same production test. Through Tiptap's ProseMirror: **10,000/10,000** on three seeds | n/a (binds the engine directly) | n/a |
| Source mode (E-CM6) on the full F1, same budget | must | | | **Pass**: 15.9 (1×) / 16.3 (4×) | E-TA fails |
| Bundle (engine + editor JS, gzipped) ≤ today's Milkdown + Yjs chunks (208.7 + 28.5 = 237.2 KiB) | should | 215.9 + 28.2 = 244.1 KiB: **fail**, 7 KiB over | 166.9 + 28.2 = 195.1 KiB: **pass** | 161.2 KiB, mostly the CodeMirror production already ships for the source viewer | |

**The frame floor.** A bare contenteditable with no handlers measures keystroke-to-paint p95 at 15.2–16.3 ms (median 15.9) here, because a keystroke often waits for the next 60 Hz frame. So "p95 < 16 ms" means "no slower than an empty editable". The **work** column is keydown until the editor and the bridge finished handling the input. It shows what each editor itself costs, and it fits the budget almost everywhere; rendering a large DOM is what separates full-document mode from section mode.

**The Event Timing API.** It reports only events of 16 ms or more, in 8 ms steps; even the bare contenteditable shows 93 of 500 keydowns there. It is recorded in the appendix but cannot resolve a 16 ms budget.

## Method

- **Machine.** The development server, 12th Gen Intel Core i7-12700K (20 threads), Linux 6.1.0-56; Playwright 1.63.0 Chromium in `fmn-dev-browser:local`, limited to 4 CPUs (`fmn-node` default). 4× throttle uses CDP `Emulation.setCPUThrottlingRate`. Viewport 1280×900.
- **Libraries (pinned in `proto/storage-v2/editor/package.json`).** Milkdown/Crepe 7.22.1 and CodeMirror (`@codemirror/view` 6.43.11, `lang-markdown` 6.5.2) at the versions production already pins; Tiptap 3.32.3 (`@tiptap/core`, `pm`, `starter-kit`, `markdown`, `extension-table`, `extension-list`, `extension-image`), released 2026-11-05, with `marked` 17.0.6 under `@tiptap/markdown`; Yjs 13.6.32 as the engine (the current engine; D1 runs in parallel).
- **Engine binding.** Every prototype edits a Yjs `Y.Text`. Local editor changes become Y operations with origin `local`; a second Y.Doc plays the remote peer; per-user undo is a `Y.UndoManager` tracking only `local` (the editors' own history plugins are removed: Milkdown `history`, Tiptap `undoRedo`). Remote updates wait while an IME composition is open, as `LiveSession` does today.
- **Variants.**
  - E-MD and E-TT: `frontend/markdown-bridge.mjs` (`MarkdownBridge` or `SectionBridge`, `ProseMirrorBinding`, `milkdownAdapter` or `tiptapAdapter`). Milkdown is configured as production configures it (Crepe builder with link tooltip, table, list item and cursor features, upload and empty-line plugins removed) minus `history`, `listener` (whole-document serialization) and `trailing` (adds an unmapped empty paragraph). Tiptap uses StarterKit (without `undoRedo` and `trailingNode`), Markdown, TableKit, TaskList/TaskItem and inline Image.
  - Section mode shows one section at a time (sections split at the shallowest heading level used more than once: F1's `##` chapters, with the `#` title as its own section) with an outline sidebar. The section's references and footnotes resolve against definitions anywhere in the note.
  - E-CM6: CodeMirror 6 with `lang-markdown` and line wrapping, bound to the engine directly (CodeMirror changes ↔ Y.Text changes).
  - E-TA: today's Source mode behaviour: a `<textarea>` diffed against its previous value on every input and reassigned whole on every remote change (`ui/markdown-editor.mjs` `applyRemote`).
  - Visual today: `frontend/milkdown.mjs` `createVisual` unchanged, wired like `ui/markdown-editor.mjs`: every change serializes the whole document (`getMarkdown`), every remote change replaces it (`replace`).
  - Control: a bare contenteditable paragraph with no handlers, to show what waiting for the next frame costs by itself.
- **Measurements.**
  - Time to interactive: `performance.now()` (from navigation) at the first frame after the editor holds F1 and is editable; median of five loads.
  - Keystrokes: 500 per position (start: chapter 1; middle: 50% of the text; end: 97%), 5% Backspace, one at a time with at least 30 ms between them and each waiting for its frame (a slow editor is measured without a growing backlog). **Paint** is keydown `timeStamp` to the first task after the next `requestAnimationFrame`; **work** is keydown to the end of the input's handling (a microtask queued by the last, bubbling `input` listener, after the editor's DOM observer and the bridge ran). The Event Timing API (`PerformanceEventTiming`, `durationThreshold` 16, the minimum) is recorded alongside: it reports nothing under 16 ms and rounds to 8 ms, so it can only count keystrokes at or over the budget.
  - Remote updates: one remote update every 100 ms (every fifth an agent-style word replacement) while typing 200 keys in the middle; `apply` is the synchronous cost from the engine update to the editor showing it, `paint` to the next frame; long tasks via `PerformanceObserver('longtask')`. The same on a tenth of F1 shows whether cost follows document size.
  - Scrolling: the whole scroller from top to bottom, 1,200 px per frame (400 px for a section), frame times and long tasks.
  - Memory: CDP `Performance.getMetrics` `JSHeapUsedSize` after a forced GC, after opening and after all typing (about 1,700 keystrokes and 60–160 remote updates, roughly two minutes, not ten).
  - Fidelity: each library parses and serializes F1 and `stress.md` whole (what today's Visual mode does on the first keystroke) and block by block (what the bridge rewrites when one block is edited); every changed byte is listed in `results/fidelity/`.
- **Repetitions.** Time to interactive is the median of five loads. Keystroke and remote-update figures are percentiles over 500 (200) samples per run. Every variant ran once per throttle setting; the configurations that decide D2 (section modes, CodeMirror, full-document Tiptap, the control) ran 4 times and the size sweep twice (Appendix B), and their medians decide the budget rows. The raw JSON keeps every run.

## Must-haves, and the effort for what was left out

| Must-have | E-MD (Milkdown) | E-TT (Tiptap 3) |
| --- | --- | --- |
| Headings, emphasis, links, images, task lists, block quotes, code blocks, tables | Rendered from `stress.md` (census below); standalone images need Crepe's image-block feature, which production does not enable either | Rendered from `stress.md`; Image must be configured inline, or Markdown paragraphs holding an image are invalid |
| Comment highlights | `commentPlugin` in `src/harness.mjs`: decorations mapped through every transaction (shown in the demo on "Call me Ishmael", "the whale", "Queequeg") | The same plugin |
| IME composition | Measured: a remote update arriving during composition waits until it ends, and the composed text commits once | Measured, the same |
| Per-user undo through the engine | Measured: typing then engine undo restores the exact bytes, including a `*` list the serializer rewrites | Measured, same |
| Read-only mode | `crepe.setReadonly` | `editor.setEditable(false)` |
| Binding to the engine through the bridge | `ProseMirrorBinding` + `milkdownAdapter` | `ProseMirrorBinding` + `tiptapAdapter` |

Effort estimates (not measured; read from the libraries and today's code):

| Feature | Milkdown/Crepe | Tiptap 3 |
| --- | --- | --- |
| Toolbar polish | Keep `frontend/toolbar.mjs` (104 lines, Milkdown commands); Crepe also ships a selection toolbar feature: hours | Rewrite `toolbar.mjs` against Tiptap commands and the `BubbleMenu` extension: about a day |
| Slash menu | Crepe's `block-edit` feature (slash menu and block handle): enable and style, hours | No official extension; build on `@tiptap/suggestion` as Tiptap's example does: one to two days |
| Math | Crepe's `latex` feature (KaTeX, remark-math; the scanner already keeps `$$` blocks together): hours, plus KaTeX in the bundle | Official `@tiptap/extension-mathematics` (KaTeX); Markdown parse and render rules for `$…$`/`$$…$$` in `@tiptap/markdown` need writing: about a day |
| Carrying today's editor over | Already Milkdown: `milkdown.mjs` (255 lines), `toolbar.mjs`, `annotations.mjs` (60) change only where the bridge replaces whole-document replace | The same ~420 lines move from Milkdown APIs (`$prose`, `$view`, ctx slices, commands) to Tiptap extensions; private-draft preview needs Tiptap's parser; wiki links need an extension (Tiptap escapes `[[…]]` on edit): about a week |

## Bridge correctness

### Production tests (`internal/web/ui/markdown-bridge.test.mjs`, run by `npm test`)

All 23 tests pass in about 14 s.

- **10,000 interleaved rounds.** Local, remote and agent edits run against an adapter backed by remark's CommonMark+GFM parser (`mdast-util-from-markdown` with `micromark-extension-gfm`, the parser stack Milkdown uses). After every round, the test checks:
  - the changes sent reproduce the bridge's text;
  - when the direct text edit means what the editor shows, the local edit gives exactly the direct text edit;
  - the editor holds the bridge's nodes, and the chunks equal a fresh scan;
  - nodes of blocks the change did not reach keep their identity, and only changed blocks are parsed, by characters;
  - once the editor takes a normalization the bridge offered, reading its document back sends nothing.

  Every tenth round it also checks that the block-by-block nodes equal a fresh bridge and the whole-note parse. The run covers 10,000 rounds over 40 documents.
- **The same property with a normalizing serializer** (remark's `mdast-util-to-markdown`): an edit rewrites only the blocks it changed, and taking a normalization back sends nothing. It runs 203 documents, including the three seeds on which taking a normalization back once rewrote untouched blocks.
- **Section bridges** under random edits and section switches. Half the documents keep their definitions in the last section; each section's nodes must equal a fresh bridge on the same range, which reads the whole note's definitions.
- **Unit tests** for:
  - chunking;
  - minimal local changes;
  - re-parsing only touched blocks;
  - keeping a re-serialized block apart from a neighbour it would merge with;
  - deferred normalization while remote changes keep indexing the editor's nodes;
  - taking back a normalization the editor applied without rewriting the bytes it kept;
  - reference definitions and footnotes resolved across blocks, including a label over two lines and a definition deep in a list item;
  - a section resolving a footnote and a reference link defined in another section, as the whole-note parse does;
  - trailing blank lines found in linear time;
  - an edit in a note whose front matter never closes re-scanning only around it;
  - an invalid change refusing its whole batch;
  - sections;
  - deltas and offset mapping (an embed in a delta throws).
- **`ProseMirrorBinding` against a real ProseMirror state:**
  - the document stays consistent after load and remote block changes;
  - minimal typing changes, with the caret mapped through a remote edit in its paragraph;
  - a normalization waits while the caret is in its block, and applying it later sends nothing;
  - plugin fix-ups are read back after a whole remote batch;
  - a remote batch that empties the note part way shows only what it leaves;
  - when the engine refuses a change, the binding reloads the engine's text.
- **Soaks.**
  - Before the review fixes, the same property ran with 15 more seeds × 40,000 rounds (600,000 rounds), all clean.
  - After them, five more seed bases each ran 40,000 rounds of the main property, 300 section documents × 60 steps and 400 normalizing documents × 50 steps, all clean.
  - Earlier soaks of about 40 seeds found the scanner edge cases fixed during development: CRLF and lone-CR line ends, HTML blocks, fences nested in list items, empty list items, micromark's list-after-indented-code quirk, definitions inside containers, and re-scanning after a definitions change.

### With the real editors (`property.html`, ProseMirror transactions through `ProseMirrorBinding`)

Each run is 10,000 rounds per library with seed 0x5eed (the table, `results/property-*.json`), then seeds 1234 and 777777 (`results/property-extra-seeds.log`).

- **Local edits** are typing, deleting, splitting, joining, deleting a block, inserting a block and changing block type.
- **Remote and agent edits** insert random Markdown fragments anywhere and delete ranges across blocks.

After each round the test checks that the engine text equals the bridge's text, that the editor holds the bridge's nodes, that blocks the edit did not touch keep their bytes, that the document is valid, and that fragment sizes are consistent. Every 25 rounds it compares the bridge's nodes with the library parsing the whole note.

| Library | Rounds | Local / remote | Normalized | Failures | Whole-note parse agrees |
| --- | --- | --- | --- | --- | --- |
| Milkdown 7.22.1 | 10,000 | 7,348 / 2,652 | 25 | **0** (0 on seeds 1234 and 777777) | 378 / 400 |
| Tiptap 3.32.3 | 10,000 | 7,289 / 2,711 | 14 | **0** (0 on seeds 1234 and 777777) | 359 / 400 |

The first runs failed 4 (Milkdown) and 5 (Tiptap) of 10,000 rounds. The two bugs behind them are fixed and covered by Node tests (see the list below). A run ends a document at its first failure, so the counts of local and remote rounds and of whole-note checks differ from the first runs.

The real-editor runs and the review found and fixed eight binding, bridge and adapter bugs before these numbers:

- taking back a normalization the editor had applied re-serialized it, so a normalizing serializer rewrote bytes nobody touched (Tiptap's two "bytes outside the edited block changed" failures);
- a remote batch passing through an empty note left ProseMirror's placeholder paragraph in the document (every "editor shows other nodes" failure, all in documents of a few characters);
- the editor's document shared the bridge's node array, so later splices corrupted it;
- a plugin fix-up after the first replacement of a remote batch was read back before the rest were applied;
- deferred normalizations left the bridge indexing nodes the editor did not show;
- Tiptap's Markdown parser emits invalid list items, duplicate marks and a bare top-level image;
- Tiptap turns blank-line runs into empty paragraphs;
- Tiptap's `trailingNode` adds a paragraph with no Markdown.

The whole-note disagreements come from adversarial text where each library's own transforms depend on context. Examples: Milkdown's HTML handling inside list items, and Tiptap's blank-line paragraphs and `marked` quirks. They are listed with their text in the JSON reports.

## Markdown fidelity

The whole-document round trip is what today's Visual mode writes on the first keystroke. "Block by block" is what the bridge rewrites when one block is edited; untouched blocks always keep their bytes. Every change is listed in `results/fidelity/<library>-<fixture>-{whole,blocks}.diff`.

| | Milkdown F1 | Tiptap F1 | Milkdown stress | Tiptap stress |
| --- | --- | --- | --- | --- |
| Whole document: changed places | 54 (+45 bytes) | 192 (+55 bytes) | 17 (−133 bytes) | 14 (−121 bytes) |
| Blocks rewritten when edited | 55 of 2,566 | 228 of 2,566 | 16 of 34 | 24 of 36 |
| Parse and serialize the whole of F1 | 418 ms | 3,958 ms | | |

- **On F1:**
  - Both libraries drop the blank line after the front matter in a whole-document round trip (today's Visual mode does too: it rewrote those bytes on the first keystroke). The bridge never touches them.
  - Milkdown keeps `_emphasis_` and only escapes stray `*`/`_` (`1_st_` → `1_st\_`) and rewrites `* * * * * *` as `***`.
  - Tiptap rewrites every `_emphasis_` as `*emphasis*`, turns `&` into `&amp;`, and writes `* * * * * *` as `---`.
- **On the stress file, Milkdown:**
  - changes `-` and `+` bullets to `*`, and `~~~` fences to backticks;
  - turns setext headings into ATX headings;
  - adds blank lines in loose lists;
  - writes hard breaks as `\`;
  - normalizes the table delimiter row;
  - converts reference links into inline links (production refuses reference links in Visual mode today);
  - links bare `www.` autolinks;
  - escapes `[[` when the note has reference definitions (without definitions it keeps `[[wiki]]` links);
  - decodes entities.

  It keeps raw HTML blocks, HTML comments and footnotes.
- **On the stress file, Tiptap:**
  - changes `*` and `+` bullets to `-`, and renumbers `3)` lists from `4.`;
  - turns indented code into fenced code;
  - drops the tags of raw HTML blocks (keeping the text, escaped) and drops HTML comments entirely;
  - writes footnotes as escaped literal text;
  - escapes `[[wiki links]]` always.

  In the adapter probe it also wrote an empty heading (`###`) and an empty ordered item (`1.`) as nothing.

Constructs each library renders from `stress.md` (node census):

| | Milkdown | Tiptap |
| --- | --- | --- |
| Headings, paragraphs, block quotes, tables, horizontal rules | 4, 51, 2, 1, 4 | 4, 51, 2, 1, 4 |
| Lists (bullet / ordered / task items) | 8 / 3 / 3 | 7 + 2 task lists / 2 / 3 |
| Code blocks, images, hard breaks | 3, 1, 5 | 4, 2, 4 |
| Raw HTML nodes, footnote references / definitions | 9, 2 / 2 | 0, 0 / 0 |
| Emphasis, strong, strike, inline code, links | 3, 5, 1, 3, 7 | 3, 5, 1, 3, 7 |

## Bundle sizes (gzipped, `results/editor-sizes.json`)

| Chunk | JS | CSS |
| --- | --- | --- |
| Milkdown editor as configured, with the bridge | 215.9 KiB | 3.1 KiB |
| Tiptap 3 editor as configured, with the bridge | 166.9 KiB | – |
| CodeMirror 6 with `lang-markdown` (which pulls the HTML, CSS and JavaScript parsers) | 161.2 KiB | – |
| The bridge alone | 8.4 KiB | – |
| Yjs | 28.2 KiB | – |
| Today's production Milkdown chunk and the chunks it imports | 208.7 KiB | – |

Per-package minified bytes for each prototype page are in `results/bundle-sizes.json`. Milkdown's chunk includes Vue (Crepe's components) and DOMPurify.

## Key measurements

### Time to interactive, opening F1 (median of 5 loads)

| | E-MD full | E-MD section | E-TT full | E-TT section | E-CM6 | E-TA | Visual today |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Unthrottled | 612 ms | 117 ms | 237 ms | 105 ms | 77 ms | 116 ms | 1,160 ms |
| 4× throttle | 2,393 ms | 381 ms | 878 ms | 337 ms | 242 ms | 444 ms | 4,420 ms |

### Keystroke p95 in the middle of F1, every run (paint, ms)

Unthrottled runs vary by up to about 5 ms because other work streams share this machine; 4× runs are steadier. The medians decide the rows above.

| Configuration | Unthrottled runs | Median | 4× runs | Median | Work p95 (1× / 4×) |
| --- | --- | --- | --- | --- | --- |
| Control: bare contenteditable | 15.2, 15.9, 15.9, 16.3 | 15.9 | 16.7, 16.3, 16.1, 17.4 | 16.3 | 1.4 / 2.0 |
| E-MD section, Crepe virtual caret (production today) | 17.8, 23.2, 23.4, 23.3 | 23.2 | 22.7, 22.7, 23.1, 22.9 | 22.7 | 6.4 / 6.9 |
| E-MD section, no Crepe cursor feature | 15.8, 16.2, 15.6, 15.2 | 15.6 | 15.7, 16.1, 15.7, 15.8 | 15.7 | 5.7 / 6.2 |
| E-MD section, cursor without virtual caret | 15.6, 15.3 | 15.3 | 15.9, 16.1 | 15.9 | 5.5 / 6.5 |
| E-MD full + content-visibility, no virtual caret | 21.2, 21.6 | 21.2 | 23.2, 23.9 | 23.2 | 11.1 / 11.5 |
| E-TT section | 15.1, 17.9, 17.3, 17.5 | 17.3 | 18.2, 18.1, 18.1, 17.4 | 18.1 | 4.4 / 4.9 |
| E-TT full | 16.5, 22.1, 21.9, 21.1 | 21.1 | 38.2, 37.8, 37.9, 38.6 | 37.9 | 18.8 / 30.2 |
| E-TT full + content-visibility | 18.4, 21.8, 22.6, 22.4 | 21.8 | 19.8, 19.7, 20.4, 20.2 | 19.8 | 8.6 / 8.9 |
| E-CM6 | 15.8, 16.0, 15.9, 16.1 | 15.9 | 16.3, 16.1, 16.9, 16.4 | 16.3 | 5.5 / 5.8 |

### Full-document keystroke p95 against size (F1 prefixes cut at chapters; 2 runs each, median; ms)

| Size | E-MD (no virtual caret) 1× / 4× | E-MD + content-visibility 1× / 4× | E-TT 1× / 4× |
| --- | --- | --- | --- |
| 16 KiB | 15.8 / 15.5 | 15.7 / 15.3 | 17.4 / 17.8 |
| 32 KiB | 17.6 / 17.2 | 16.1 / 15.5 | 15.4 / 15.7 |
| 64 KiB | 18.7 / 18.1 | 17.0 / 16.4 | 16.5 / 16.1 |
| 128 KiB | 18.4 / 19.3 | 18.0 / 17.1 | 18.2 / 18.2 |
| 256 KiB | 17.8 / 19.3 | 19.6 / 19.7 | 18.5 / 18.5 |
| 512 KiB | 20.5 / 25.3 | 19.3 / 21.7 | – |
| F1 (1.2 MB) | – | 21.2 / 23.2 | 21.1 / 37.9 (no c-v) |

### Remote updates while typing (one every 100 ms, every fifth agent-style)

| | E-MD full | E-MD section | E-TT full | E-TT section | E-CM6 | E-TA | Visual today |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Apply p95, 1× | 4.4 ms | 0.7 ms | 1.4 ms | 0.7 ms | 1.4 ms | 11.0 ms | 879 ms |
| Long tasks, 1× | 0 | 0 | 0 | 0 | 0 | 162 (max 97 ms) | 98 (max 887 ms) |
| Apply p95, 4× | 12.6 ms | 2.2 ms | 4.9 ms | 2.7 ms | 5.0 ms | 40.3 ms | 3,443 ms |
| Long tasks, 4× | 72 (max 74 ms) | 0 | 0 | 0 | 0 | 305 (max 373 ms) | 30 (max 3,445 ms) |
| Apply p50, a tenth of F1 vs F1 (1×) | 3.3 vs 3.0 ms | | 1.4 vs 0.8 ms | | 1.7 vs 0.9 ms | 4.6 vs 9.8 ms | 87 vs 856 ms |

**Re-check after the review fixes.** `SectionBridge` now scans the text around the section for definitions, so section mode was measured again on the fixed bridge (`results/raw/recheck-2026-11-22T22-59-40-288Z.json`). This run is kept out of the medians and Appendix B.

- **Remote updates.** Apply p95 is 0.9 ms (1×) and 1.6 ms (4×) for E-MD section, and 0.7 ms and 1.5 ms for E-TT section, with 0 long tasks.
- **Keystroke p95 in the middle (1× / 4×).** E-MD section with the virtual caret: 22.6 / 22.4 ms. Without it: 15.3 / 17.3 ms. E-TT section: 17.5 / 18.0 ms.
- **Outcome.** The 1× figures are within the earlier runs' spread. Every row's outcome is unchanged.

### Scrolling, memory and section switching

- **Scrolling.** Scrolling the whole F1 (300,000–480,000 px at 1,200 px per frame) produced no frame over 50 ms and no long task in any full-document variant, unthrottled or at 4×. The one exception is content-visibility at 4×: 0–2 frames over 50 ms, and frame p95 rises from about 17 ms to 34–43 ms.
- **JS heap after opening F1.** E-MD 6.1 MB, E-TT 7.0 MB, E-CM6 3.4 MB, E-TA 1.4 MB, Visual today 10.4 MB. After about 1,700 keystrokes and 60–160 remote updates: 9.0, 9.5, 7.0, 4.6 and 24.2 MB.
- **Switching sections.** p50 / p95 is 7.3 / 20 ms for Milkdown and 3.0 / 3.7 ms for Tiptap unthrottled; at 4× it is 25 / 47 ms and 12 / 18 ms.

### Engine undo, IME and read-only

| | E-MD full and section | E-TT full and section | E-CM6 | E-TA |
| --- | --- | --- | --- | --- |
| Type 16 characters, engine-undo: exact bytes (F1, and a `*` list in the stress file that the serializer rewrites) | yes | yes | yes | yes |
| IME (CDP `imeSetComposition`): a remote update during composition waits, the composed text commits once | yes | yes | yes | yes |
| Read-only refuses typing | yes | yes | yes | yes |

## Open issues

1. **Hands-on check.** Twenty minutes per editor on a laptop, with `demo.sh`, is still to do. D2's library choice is conditional on it.
2. **Real-editor property runs: resolved.** The first runs missed 10,000/10,000 by 4 (Milkdown) and 5 (Tiptap) rounds. Both causes are fixed, with Node regression tests: taking back an applied normalization, and a remote batch passing through an empty note. Both libraries now pass 10,000/10,000 on three seeds (`results/property-*.json`; reproduce with `property.html?lib=milkdown&rounds=N&seed=S`). Wave 2 should run the same page against the real `milkdown.mjs` wiring.
3. **Sections over the threshold** (15 of F1's 137; the largest is 45 KiB) still type at about 16–18 ms. Split such a section at its next heading level, or bind the bridge to a block-aligned window: `SectionBridge` takes any chunk-aligned `range`.
4. **Reference links and footnotes.**
   - The bridge resolves definitions across blocks, and section mode resolves them across the whole note. Milkdown still rewrites reference links as inline links when a block is edited.
   - Production refuses both in Visual mode today; keep that refusal or accept the normalization.
   - If wave 2 keeps the refusal (`checkTree` in `milkdown.mjs`), it must check the nodes the bridge parsed, which carry the note's definitions. Checking a section's text parsed alone would miss a footnote defined in another section.
5. **Measurement limits.**
   - Distributions come from one run of 500 keystrokes per position and configuration. The deciding rows were repeated 2–4 times; the plan asks for 5. At 1× several deciding comparisons are within noise of the 15.9 ms frame floor (Decision D2, point 2).
   - The "10 minutes of typing" heap figure is about 2 minutes.
   - Headless Chromium has no spellcheck dictionary, so spellcheck on and off could not differ.
   - Unthrottled runs share the machine with other streams.
   - Today's `textChanges()` refuses notes over 1 MiB, so E-TA and Visual today use the same prefix/suffix interval it starts from.
6. **CodeMirror's `lang-markdown`** bundles the HTML, CSS and JavaScript parsers (161 KiB). Production's source viewer already ships CodeMirror, so wave 2 should share that chunk and load code-block languages lazily.
7. **The milestone record.** Recording D2 in design §16/§17 and in project memory is left to the coordinator: those files are outside this stream's scope.

## Appendix A: every measured configuration (first run of each)

Chromium 153.0.8010.12, 12th Gen Intel(R) Core(TM) i7-12700K, container limited to 4 CPUs. Raw: results/raw/bench-2026-11-22T20-30-51-983Z.json, results/raw/bench-2026-11-22T21-07-14-251Z.json.

### Time to interactive (median of 5 loads, ms)

| Variant | Unthrottled | 4× throttle | Runs (1×) | Runs (4×) |
| --- | --- | --- | --- | --- |
| E-MD full | 612 | 2393 | 603, 612, 609, 612, 613 | 2399, 2393, 2387, 2362, 2394 |
| E-MD section | 117 | 381 | 121, 118, 116, 117, 115 | 392, 381, 381, 386, 378 |
| E-TT full | 237 | 878 | 243, 236, 238, 237, 236 | 880, 887, 876, 875, 878 |
| E-TT section | 105 | 337 | 104, 109, 105, 109, 101 | 342, 357, 335, 334, 337 |
| E-CM6 source | 77 | 242 | 75, 77, 77, 75, 77 | 241, 241, 243, 242, 249 |
| E-TA source (today) | 116 | 444 | 116, 115, 114, 117, 118 | 446, 444, 444, 443, 447 |
| Visual today (whole doc) | 1160 | 4420 | 1131, 1154, 1160, 1173, 1166 | 4414, 4478, 4424, 4420, 4398 |

### Keystroke latency, unthrottled (ms; 500 keystrokes per position)

Paint: keydown to the next frame (rAF, then a task). Work: keydown until the editor and the bridge finished handling the input. Event Timing: keydown entries of 16 ms or more (the API reports nothing shorter, in 8 ms steps).

| Variant | Where | Keys | Paint p50 | Paint p95 | Work p50 | Work p95 | Paint max | ET ≥16 ms | ET max | Long tasks |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Control (bare contenteditable) | middle | 500 | 1.1 | 15.2 ✓ | 0.4 | 0.6 ✓ | 17.3 | 93/500 | 16 | 0 |
| E-MD full | start | 500 | 20.9 | 26.5 | 9.4 | 10.7 | 31.5 | 457/500 | 24 | 0 |
| E-MD full | middle | 500 | 20.9 | 27.2 ✗ | 9.8 | 11.1 ✓ | 30.2 | 492/500 | 24 | 0 |
| E-MD full | end | 500 | 21 | 27.3 | 10.1 | 11.6 | 30.8 | 500/500 | 24 | 0 |
| E-MD section | start | 500 | 11.5 | 18.4 | 1.7 | 2.6 | 20 | 180/500 | 16 | 0 |
| E-MD section | middle | 500 | 10.8 | 17.8 ✗ | 1.6 | 2.3 ✓ | 20 | 157/500 | 16 | 0 |
| E-MD section | end | 500 | 12.6 | 18.9 | 1.9 | 2.8 | 20 | 196/500 | 16 | 0 |
| E-TT full | start | 500 | 8.8 | 16.8 | 6.7 | 8.5 | 18.6 | 108/500 | 24 | 0 |
| E-TT full | middle | 500 | 9 | 16.5 ✗ | 7 | 9.3 ✓ | 18.8 | 117/500 | 16 | 0 |
| E-TT full | end | 500 | 9.5 | 17.1 | 7.3 | 9.4 | 18.5 | 114/500 | 16 | 0 |
| E-TT section | start | 500 | 2.3 | 15.2 | 1.2 | 1.7 | 17.6 | 94/500 | 16 | 0 |
| E-TT section | middle | 500 | 1.7 | 15.1 ✓ | 1 | 1.4 ✓ | 17.2 | 96/500 | 16 | 0 |
| E-TT section | end | 500 | 2.3 | 15.1 | 1.1 | 1.6 | 17.5 | 96/500 | 16 | 0 |
| E-CM6 source | start | 500 | 2.6 | 15.6 | 1.2 | 1.7 | 16.8 | 97/500 | 16 | 0 |
| E-CM6 source | middle | 500 | 2.8 | 15.8 ✓ | 1.3 | 2 ✓ | 17.8 | 96/500 | 16 | 0 |
| E-CM6 source | end | 500 | 3.7 | 16.3 | 2.1 | 3 | 18.1 | 95/500 | 16 | 0 |
| E-TA source (today) | start | 500 | 11 | 19.3 | 7.7 | 9.7 | 23.8 | 159/500 | 24 | 0 |
| E-TA source (today) | middle | 500 | 11.6 | 19.3 ✗ | 8.1 | 10.3 ✓ | 22.2 | 292/500 | 24 | 0 |
| E-TA source (today) | end | 500 | 11.8 | 19.5 | 8.3 | 10.1 | 24.3 | 417/500 | 24 | 0 |
| Visual today (whole doc) | start | 500 | 154.4 | 163.3 | 147.9 | 156.5 | 173.7 | 500/500 | 176 | 500 (max 167) |
| Visual today (whole doc) | middle | 500 | 153.3 | 162 ✗ | 146.9 | 155.7 ✗ | 178.4 | 499/500 | 176 | 500 (max 171) |
| Visual today (whole doc) | end | 500 | 153.7 | 162.9 | 147.4 | 156.5 | 187.4 | 500/500 | 184 | 500 (max 180) |
| E-MD full + content-visibility | middle | 500 | 16.1 | 21.4 ✗ | 3.8 | 5 ✓ | 26.7 | 237/500 | 32 | 0 |
| E-MD full, spellcheck off | middle | 500 | 21.4 | 26.8 ✗ | 9.6 | 11.6 ✓ | 31.1 | 481/500 | 24 | 0 |
| E-MD full + c-v, spellcheck off | middle | 500 | 15.8 | 20.7 ✗ | 3.8 | 5.1 ✓ | 31.5 | 245/500 | 24 | 0 |
| E-TT full + content-visibility | middle | 500 | 4.9 | 18.4 ✗ | 2.1 | 3 ✓ | 20.2 | 133/500 | 24 | 0 |
| E-TT full, spellcheck off | middle | 500 | 9.1 | 16.5 ✗ | 7.1 | 9.9 ✓ | 19.3 | 120/500 | 24 | 0 |
| E-TT full + c-v, spellcheck off | middle | 500 | 4.9 | 18.4 ✗ | 2.1 | 3 ✓ | 20.7 | 132/500 | 24 | 0 |
| E-MD section, no comment highlights | middle | 500 | 16.5 | 22.9 ✗ | 5.4 | 6.6 ✓ | 28.3 | 257/500 | 32 | 0 |
| E-MD section, no Crepe cursor feature | middle | 500 | 6.7 | 15.8 ✓ | 4.5 | 5.8 ✓ | 23.5 | 121/500 | 24 | 0 |
| E-MD section, no Crepe features or highlights | middle | 500 | 6.5 | 16.8 ✗ | 4.4 | 5.4 ✓ | 27 | 121/500 | 32 | 0 |
| E-MD full + c-v, no Crepe cursor feature | middle | 500 | 13.6 | 21.6 ✗ | 8.8 | 11.3 ✓ | 40 | 423/500 | 40 | 0 |

### Keystroke latency, 4× CPU throttle (ms; 500 keystrokes per position)

Paint: keydown to the next frame (rAF, then a task). Work: keydown until the editor and the bridge finished handling the input. Event Timing: keydown entries of 16 ms or more (the API reports nothing shorter, in 8 ms steps).

| Variant | Where | Keys | Paint p50 | Paint p95 | Work p50 | Work p95 | Paint max | ET ≥16 ms | ET max | Long tasks |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Control (bare contenteditable) | middle | 500 | 3.2 | 16.7 ✓ | 1.2 | 2.1 ✓ | 20.2 | 98/500 | 24 | 0 |
| E-MD full | start | 500 | 59.3 | 68.5 | 35.3 | 41 | 100 | 499/500 | 80 | 246 (max 77) |
| E-MD full | middle | 500 | 58.8 | 64.2 ✗ | 35 | 37.4 ✓ | 87.3 | 499/500 | 72 | 240 (max 73) |
| E-MD full | end | 500 | 61.4 | 64.9 | 37.5 | 39.7 | 87.3 | 499/500 | 72 | 219 (max 64) |
| E-MD section | start | 500 | 15.7 | 23 | 5.7 | 7.2 | 32.4 | 208/500 | 32 | 0 |
| E-MD section | middle | 500 | 16.3 | 22.7 ✓ | 5.3 | 6.9 ✓ | 28.2 | 206/500 | 16 | 0 |
| E-MD section | end | 500 | 16.7 | 23.4 | 6.2 | 8 | 27.3 | 236/500 | 24 | 0 |
| E-TT full | start | 500 | 31.9 | 36.8 | 24.7 | 28.8 | 56.4 | 499/500 | 56 | 0 |
| E-TT full | middle | 500 | 33 | 38.2 ✓ | 25.6 | 30.2 ✓ | 47 | 499/500 | 48 | 0 |
| E-TT full | end | 500 | 34.9 | 39.9 | 27.4 | 31.9 | 45.3 | 499/500 | 40 | 0 |
| E-TT section | start | 500 | 6 | 18 | 3.7 | 5 | 24.2 | 115/500 | 24 | 0 |
| E-TT section | middle | 500 | 5.9 | 18.2 ✓ | 3.6 | 4.8 ✓ | 19.7 | 110/500 | 24 | 0 |
| E-TT section | end | 500 | 6.5 | 18.1 | 3.9 | 5 | 20 | 114/500 | 24 | 0 |
| E-CM6 source | start | 500 | 6.8 | 17.3 | 4 | 5.5 | 21.3 | 117/500 | 24 | 0 |
| E-CM6 source | middle | 500 | 7.3 | 16.3 ✓ | 4.4 | 5.8 ✓ | 20 | 115/500 | 24 | 0 |
| E-CM6 source | end | 500 | 10.3 | 18.5 | 7.3 | 8.9 | 20.8 | 119/500 | 24 | 0 |
| E-TA source (today) | start | 500 | 41.6 | 51.9 | 28.8 | 36.2 | 126.7 | 499/500 | 104 | 3 (max 93) |
| E-TA source (today) | middle | 500 | 43.5 | 53 ✗ | 30.2 | 37.6 ✓ | 72.2 | 499/500 | 64 | 0 |
| E-TA source (today) | end | 500 | 44.7 | 55.2 | 31.5 | 38.8 | 80.3 | 499/500 | 72 | 0 |
| Visual today (whole doc) | start | 253 (time cap) | 581.6 | 680.9 | 556.7 | 654.6 | 728.3 | 252/253 | 712 | 253 (max 699) |
| Visual today (whole doc) | middle | 254 (time cap) | 582.1 | 673.8 ✗ | 557.9 | 650.1 ✗ | 712.5 | 253/254 | 696 | 254 (max 699) |
| Visual today (whole doc) | end | 254 (time cap) | 584.6 | 658.2 | 560.2 | 632 | 728.2 | 253/254 | 712 | 254 (max 725) |
| E-MD full + content-visibility | middle | 500 | 26.1 | 32.1 ✓ | 12.7 | 15.3 ✓ | 73 | 499/500 | 64 | 1 (max 57) |
| E-MD full, spellcheck off | middle | 500 | 59.9 | 63.2 ✗ | 35.9 | 37.8 ✓ | 99.5 | 499/500 | 88 | 285 (max 83) |
| E-MD full + c-v, spellcheck off | middle | 500 | 26.4 | 32.8 ✓ | 13 | 15.5 ✓ | 93.6 | 499/500 | 64 | 0 |
| E-TT full + content-visibility | middle | 500 | 16.8 | 19.8 ✓ | 7.5 | 8.9 ✓ | 51.2 | 500/500 | 48 | 0 |
| E-TT full, spellcheck off | middle | 500 | 34.1 | 38.5 ✓ | 26.8 | 30.5 ✓ | 68.3 | 499/500 | 72 | 1 (max 52) |
| E-TT full + c-v, spellcheck off | middle | 500 | 16.8 | 19.5 ✓ | 7.5 | 8.7 ✓ | 54.3 | 500/500 | 48 | 0 |
| E-MD section, no comment highlights | middle | 500 | 15.4 | 22.4 ✓ | 5.5 | 7.2 ✓ | 32 | 202/500 | 32 | 0 |
| E-MD section, no Crepe cursor feature | middle | 500 | 7 | 15.7 ✓ | 4.7 | 6.2 ✓ | 29.1 | 114/500 | 32 | 0 |
| E-MD section, no Crepe features or highlights | middle | 500 | 6.9 | 15.8 ✓ | 4.6 | 5.9 ✓ | 28.5 | 112/500 | 32 | 0 |
| E-MD full + c-v, no Crepe cursor feature | middle | 500 | 20.3 | 23.3 ✓ | 10 | 11.5 ✓ | 64.3 | 500/500 | 56 | 0 |

### Remote updates while typing, unthrottled

One remote update every 100 ms (every fifth an agent-style word replacement) while typing 200 keys in the middle. Apply: engine update to the editor showing it, synchronous. Paint: to the next frame.

| Variant | Updates | Apply p50 | Apply p95 | Apply max | Paint p95 | Long tasks (>50 ms) | Keystroke paint p95 | Keystroke work p95 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| E-MD full | 60 | 3 | 4.4 | 5 | 44 | 0 ✓ | 28.2 | 12.8 |
| E-MD section | 60 | 0.4 | 0.7 | 1.2 | 31.6 | 0 ✓ | 18 | 2.6 |
| E-TT full | 60 | 0.8 | 1.4 | 2.6 | 32.7 | 0 ✓ | 16.5 | 10 |
| E-TT section | 60 | 0.4 | 0.7 | 1 | 10.5 | 0 ✓ | 14.6 | 1.6 |
| E-CM6 source | 60 | 0.9 | 1.4 | 1.5 | 13.8 | 0 ✓ | 16.2 | 2.2 |
| E-TA source (today) | 164 | 9.8 | 11 | 11.8 | 99.3 | 162 (max 97) ✗ | 94 | 70.8 |
| Visual today (whole doc) | 67 | 856.3 | 878.8 | 886.5 | 1907 | 98 (max 887) ✗ | 1908.7 | 1021.6 |

### Remote updates while typing, 4× CPU throttle

One remote update every 100 ms (every fifth an agent-style word replacement) while typing 200 keys in the middle. Apply: engine update to the editor showing it, synchronous. Paint: to the next frame.

| Variant | Updates | Apply p50 | Apply p95 | Apply max | Paint p95 | Long tasks (>50 ms) | Keystroke paint p95 | Keystroke work p95 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| E-MD full | 157 | 10.9 | 12.6 | 17.1 | 111.2 | 72 (max 74) ✗ | 104 | 66.6 |
| E-MD section | 60 | 1.3 | 2.2 | 2.9 | 30.2 | 0 ✓ | 23.2 | 7.9 |
| E-TT full | 84 | 2.7 | 4.9 | 6.6 | 47.4 | 0 ✓ | 42.8 | 34.5 |
| E-TT section | 60 | 1.7 | 2.7 | 3.4 | 16.1 | 0 ✓ | 17 | 5.6 |
| E-CM6 source | 60 | 3 | 5 | 6.2 | 26.8 | 0 ✓ | 18.5 | 7.7 |
| E-TA source (today) | 234 | 37.4 | 40.3 | 58.4 | 741.8 | 305 (max 373) ✗ | 663.4 | 315.3 |
| Visual today (whole doc) | 18 | 3369.5 | 3443.2 | 3443.2 | 7472.1 | 30 (max 3445) ✗ | 7423 | 3984.1 |

### Remote update cost against document size (unthrottled)

| Variant | Tenth of F1: chars | Apply p50 | Apply p95 | Full F1: apply p50 | Apply p95 |
| --- | --- | --- | --- | --- | --- |
| E-MD full | 128239 | 3.3 | 4.8 | 3 | 4.4 |
| E-TT full | 128239 | 1.4 | 2.6 | 0.8 | 1.4 |
| E-CM6 source | 128239 | 1.7 | 2.6 | 0.9 | 1.4 |
| E-TA source (today) | 128239 | 4.6 | 5.1 | 9.8 | 11 |
| Visual today (whole doc) | 128239 | 87 | 93.2 | 856.3 | 878.8 |

### Scrolling the whole document, JS heap, section switching

| Variant | CPU | Height px | Frames | Frames >50 ms | Frame p95 | Frame max | Long tasks | Heap open MB | Heap after typing MB | Section switch p50 / p95 ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| E-MD full | 1× | 477929 | 398 | 0 | 17.2 | 18.4 | 0 | 6.1 | 9 | – |
| E-MD section | 1× | 3580 | 7 | 0 | 16.8 | 16.8 | 0 | 4.1 | 11.5 | 7.3 / 20 |
| E-TT full | 1× | 348018 | 290 | 0 | 17 | 17.9 | 0 | 7 | 9.5 | – |
| E-TT section | 1× | 2703 | 5 | 0 | 16.7 | 16.7 | 0 | 4.3 | 9.2 | 3 / 3.7 |
| E-CM6 source | 1× | 304671 | 253 | 0 | 18.5 | 19.9 | 0 | 3.4 | 7 | – |
| E-TA source (today) | 1× | 324916 | 271 | 0 | 17.8 | 20.2 | 0 | 1.4 | 4.6 | – |
| Visual today (whole doc) | 1× | 477954 | 398 | 0 | 17.2 | 23.9 | 0 | 10.4 | 24.2 | – |
| E-MD full + content-visibility | 1× | 477669 | 398 | 0 | 20.3 | 24.2 | 0 | 6.1 | 7.9 | – |
| E-MD full, spellcheck off | 1× | 477669 | 398 | 0 | 17.2 | 18.7 | 0 | 6.1 | 8 | – |
| E-MD full + c-v, spellcheck off | 1× | 477669 | 398 | 0 | 20.5 | 23.2 | 0 | 6.1 | 8 | – |
| E-TT full + content-visibility | 1× | 347836 | 290 | 0 | 19.2 | 21.7 | 0 | 7 | 8.7 | – |
| E-TT full, spellcheck off | 1× | 347676 | 289 | 0 | 17.2 | 18.1 | 0 | 7 | 8.7 | – |
| E-TT full + c-v, spellcheck off | 1× | 347836 | 290 | 0 | 18.8 | 25.8 | 0 | 7 | 8.7 | – |
| E-MD section, no comment highlights | 1× | 3580 | 7 | 0 | 17 | 17 | 0 | 4.1 | 10.8 | 7.2 / 13.6 |
| E-MD section, no Crepe cursor feature | 1× | 3580 | 7 | 0 | 16.9 | 16.9 | 0 | 4.1 | 10.7 | 6.9 / 12.5 |
| E-MD section, no Crepe features or highlights | 1× | 3580 | 7 | 0 | 17 | 17 | 0 | 3.7 | 10.2 | 8.1 / 12.5 |
| E-MD full + c-v, no Crepe cursor feature | 1× | 477669 | 398 | 0 | 20.9 | 29.2 | 0 | 6.1 | 7.7 | – |
| E-MD full | 4× | 477689 | 398 | 0 | 17.6 | 27.7 | 0 | 6.1 | 9.5 | – |
| E-MD section | 4× | 3580 | 7 | 0 | 17.3 | 17.3 | 0 | 4.1 | 11.5 | 25.3 / 47.4 |
| E-TT full | 4× | 348066 | 290 | 0 | 17.5 | 22.5 | 0 | 7 | 9.8 | – |
| E-TT section | 4× | 2703 | 5 | 0 | 17 | 17 | 0 | 4.3 | 9.2 | 12.1 / 18 |
| E-CM6 source | 4× | 304691 | 253 | 0 | 22.9 | 27.7 | 0 | 3.2 | 6.9 | – |
| E-TA source (today) | 4× | 325084 | 271 | 0 | 17.9 | 23.1 | 0 | 1.4 | 4.9 | – |
| Visual today (whole doc) | 4× | 477790 | 398 | 0 | 17.7 | 27.6 | 0 | 10.4 | 20.4 | – |
| E-MD full + content-visibility | 4× | 477669 | 398 | 1 | 40.5 | 50.4 | 0 | 6.1 | 8 | – |
| E-MD full, spellcheck off | 4× | 477669 | 398 | 0 | 17.5 | 25.1 | 0 | 6.1 | 8 | – |
| E-MD full + c-v, spellcheck off | 4× | 477669 | 398 | 0 | 41.4 | 50 | 0 | 6.1 | 8 | – |
| E-TT full + content-visibility | 4× | 347836 | 290 | 0 | 34.6 | 43.8 | 0 | 7 | 8.7 | – |
| E-TT full, spellcheck off | 4× | 347676 | 289 | 0 | 17.4 | 20.6 | 0 | 7 | 8.8 | – |
| E-TT full + c-v, spellcheck off | 4× | 347836 | 290 | 0 | 34.3 | 40 | 0 | 7 | 8.7 | – |
| E-MD section, no comment highlights | 4× | 3580 | 7 | 0 | 17.4 | 17.4 | 0 | 4.1 | 10.8 | 25.6 / 54.4 |
| E-MD section, no Crepe cursor feature | 4× | 3580 | 7 | 0 | 17 | 17 | 0 | 4.1 | 10.7 | 25.3 / 55.8 |
| E-MD section, no Crepe features or highlights | 4× | 3580 | 7 | 0 | 17.4 | 17.4 | 0 | 3.7 | 10.2 | 22.9 / 56.1 |
| E-MD full + c-v, no Crepe cursor feature | 4× | 477669 | 398 | 2 | 43 | 54 | 0 | 6.1 | 7.8 | – |

### Engine undo, IME and read-only (unthrottled)

| Variant | Type then engine-undo, F1: exact bytes | Same on a "*" list in the stress file | IME: remote update waits, commits once | Read-only refuses typing |
| --- | --- | --- | --- | --- |
| E-MD full | yes | yes | yes | yes |
| E-MD section | yes | yes | yes | yes |
| E-TT full | yes | yes | yes | yes |
| E-TT section | yes | yes | yes | yes |
| E-CM6 source | yes | yes | yes | yes |
| E-TA source (today) | yes | yes | yes | yes |

## Appendix B: keystroke p95 in the middle of F1 across every run

| Configuration | Runs | Paint p95 per run | Median paint p95 | Median work p95 | Median paint p50 |
| --- | --- | --- | --- | --- | --- |
| cm6@1x | 4 | 15.8, 16, 15.9, 16.1 | 15.9 | 5.5 | 7.1 |
| cm6@4x | 4 | 16.3, 16.1, 16.9, 16.4 | 16.3 | 5.8 | 7.2 |
| floor@1x | 4 | 15.2, 15.9, 15.9, 16.3 | 15.9 | 1.4 | 2.5 |
| floor@4x | 4 | 16.7, 16.3, 16.1, 17.4 | 16.3 | 2 | 3 |
| md-128k-cv@1x | 2 | 18.2, 18 | 18 | 7.7 | 9.1 |
| md-128k-cv@4x | 2 | 17.1, 17.7 | 17.1 | 8.3 | 9.1 |
| md-128k@1x | 2 | 19.2, 18.4 | 18.4 | 10.2 | 11.6 |
| md-128k@4x | 2 | 19.3, 19.5 | 19.3 | 10.6 | 12.1 |
| md-16k-cv@1x | 2 | 15.7, 15.9 | 15.7 | 6 | 7.2 |
| md-16k-cv@4x | 2 | 15.3, 16.5 | 15.3 | 6.6 | 7.3 |
| md-16k@1x | 2 | 16, 15.8 | 15.8 | 6.4 | 7.5 |
| md-16k@4x | 2 | 15.5, 15.8 | 15.5 | 6.8 | 7.6 |
| md-256k-cv@1x | 2 | 19.6, 19.9 | 19.6 | 9.1 | 11.3 |
| md-256k-cv@4x | 2 | 19.9, 19.7 | 19.7 | 9.4 | 11.5 |
| md-256k@1x | 2 | 17.8, 18.5 | 17.8 | 13.8 | 13.1 |
| md-256k@4x | 2 | 19.3, 20.6 | 19.3 | 14 | 16.5 |
| md-32k-cv@1x | 2 | 16.1, 16.2 | 16.1 | 5.8 | 7.1 |
| md-32k-cv@4x | 2 | 15.5, 15.9 | 15.5 | 6.4 | 7.4 |
| md-32k@1x | 2 | 17.6, 17.6 | 17.6 | 7.5 | 8.9 |
| md-32k@4x | 2 | 17.2, 17.3 | 17.2 | 7.3 | 9.1 |
| md-512k-cv@1x | 2 | 20.3, 19.3 | 19.3 | 8.9 | 12.3 |
| md-512k-cv@4x | 2 | 21.7, 21.7 | 21.7 | 8.9 | 13.2 |
| md-512k@1x | 2 | 20.7, 20.5 | 20.5 | 17.2 | 13.5 |
| md-512k@4x | 2 | 25.3, 25.6 | 25.3 | 18.3 | 23.3 |
| md-64k-cv@1x | 2 | 17, 17.3 | 17 | 6.9 | 8.2 |
| md-64k-cv@4x | 2 | 16.6, 16.4 | 16.4 | 7.6 | 8.3 |
| md-64k@1x | 2 | 18.7, 19 | 18.7 | 8.7 | 10.3 |
| md-64k@4x | 2 | 18.1, 18.6 | 18.1 | 9.1 | 10.4 |
| md-full-cv-nocursor@1x | 1 | 21.6 | 21.6 | 11.3 | 13.6 |
| md-full-cv-nocursor@4x | 1 | 23.3 | 23.3 | 11.5 | 20.3 |
| md-full-cv-nospell@1x | 1 | 20.7 | 20.7 | 5.1 | 15.8 |
| md-full-cv-nospell@4x | 1 | 32.8 | 32.8 | 15.5 | 26.4 |
| md-full-cv-plaincursor@1x | 2 | 21.2, 21.6 | 21.2 | 11.1 | 13.4 |
| md-full-cv-plaincursor@4x | 2 | 23.2, 23.9 | 23.2 | 11.5 | 20.1 |
| md-full-cv@1x | 1 | 21.4 | 21.4 | 5 | 16.1 |
| md-full-cv@4x | 1 | 32.1 | 32.1 | 15.3 | 26.1 |
| md-full-nospell@1x | 1 | 26.8 | 26.8 | 11.6 | 21.4 |
| md-full-nospell@4x | 1 | 63.2 | 63.2 | 37.8 | 59.9 |
| md-full@1x | 1 | 27.2 | 27.2 | 11.1 | 20.9 |
| md-full@4x | 1 | 64.2 | 64.2 | 37.4 | 58.8 |
| md-section-bare@1x | 1 | 16.8 | 16.8 | 5.4 | 6.5 |
| md-section-bare@4x | 1 | 15.8 | 15.8 | 5.9 | 6.9 |
| md-section-nocomments@1x | 1 | 22.9 | 22.9 | 6.6 | 16.5 |
| md-section-nocomments@4x | 1 | 22.4 | 22.4 | 7.2 | 15.4 |
| md-section-nocursor@1x | 4 | 15.8, 16.2, 15.6, 15.2 | 15.6 | 5.7 | 6.7 |
| md-section-nocursor@4x | 4 | 15.7, 16.1, 15.7, 15.8 | 15.7 | 6.2 | 7 |
| md-section-plaincursor@1x | 2 | 15.6, 15.3 | 15.3 | 5.5 | 6.7 |
| md-section-plaincursor@4x | 2 | 15.9, 16.1 | 15.9 | 6.5 | 7 |
| md-section@1x | 4 | 17.8, 23.2, 23.4, 23.3 | 23.2 | 6.4 | 16.6 |
| md-section@4x | 4 | 22.7, 22.7, 23.1, 22.9 | 22.7 | 6.9 | 16 |
| ta@1x | 1 | 19.3 | 19.3 | 10.3 | 11.6 |
| ta@4x | 1 | 53 | 53 | 37.6 | 43.5 |
| tt-128k@1x | 2 | 18.3, 18.2 | 18.2 | 8.4 | 10 |
| tt-128k@4x | 2 | 18.2, 18.3 | 18.2 | 8.8 | 10.1 |
| tt-16k@1x | 2 | 17.5, 17.4 | 17.4 | 4.9 | 5.8 |
| tt-16k@4x | 2 | 17.8, 18 | 17.8 | 5.3 | 6.2 |
| tt-256k@1x | 2 | 18.9, 18.5 | 18.5 | 11.2 | 12.1 |
| tt-256k@4x | 2 | 19.4, 18.5 | 18.5 | 11.4 | 13.2 |
| tt-32k@1x | 2 | 15.4, 16.2 | 15.4 | 6.1 | 7.2 |
| tt-32k@4x | 2 | 15.9, 15.7 | 15.7 | 6.6 | 7.7 |
| tt-64k@1x | 2 | 16.6, 16.5 | 16.5 | 6.8 | 7.9 |
| tt-64k@4x | 2 | 16.1, 16.3 | 16.1 | 7.3 | 8.2 |
| tt-full-cv-nospell@1x | 1 | 18.4 | 18.4 | 3 | 4.9 |
| tt-full-cv-nospell@4x | 1 | 19.5 | 19.5 | 8.7 | 16.8 |
| tt-full-cv@1x | 4 | 18.4, 21.8, 22.6, 22.4 | 21.8 | 8.6 | 13.1 |
| tt-full-cv@4x | 4 | 19.8, 19.7, 20.4, 20.2 | 19.8 | 8.9 | 16.9 |
| tt-full-nospell@1x | 1 | 16.5 | 16.5 | 9.9 | 9.1 |
| tt-full-nospell@4x | 1 | 38.5 | 38.5 | 30.5 | 34.1 |
| tt-full@1x | 4 | 16.5, 22.1, 21.9, 21.1 | 21.1 | 18.8 | 13.5 |
| tt-full@4x | 4 | 38.2, 37.8, 37.9, 38.6 | 37.9 | 30.2 | 33.3 |
| tt-section@1x | 4 | 15.1, 17.9, 17.3, 17.5 | 17.3 | 4.4 | 5.4 |
| tt-section@4x | 4 | 18.2, 18.1, 18.1, 17.4 | 18.1 | 4.9 | 5.9 |
| visual-now@1x | 1 | 162 | 162 | 155.7 | 153.3 |
| visual-now@4x | 1 | 673.8 | 673.8 | 650.1 | 582.1 |
