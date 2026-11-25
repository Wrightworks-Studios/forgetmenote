# Storage v2 bake-off plan

- **Status:** plan
- **Date:** 2026-11-22
- **Design:** [storage-v2-design.md](storage-v2.md)

The bake-off answers three questions with measurements before any production code changes:

| Decision | Question | Candidates |
| --- | --- | --- |
| **D1 engine** | Which CRDT engine runs live editing and fine-grained history? | **Y**: Yjs in the browser, `reearth/ygo` on the server (current) · **L-cgo**: `loro-crdt` in the browser, community `loro-go` over loro-ffi on the server · **L-wasm**: `loro-crdt` in the browser, Loro compiled to `wasm32-wasip1` with a thin C-ABI shim, run with `wazero` on the server (no cgo) |
| **D2 editor** | Which WYSIWYG editor handles novel-size Markdown alongside live collaboration and agent edits, and is section mode needed? | **E-MD**: Milkdown/Crepe (current) with the block-by-block bridge · **E-TT**: Tiptap 3 with the official `@tiptap/markdown`, same bridge · each measured on the full document and in section mode · Source mode: **E-CM6** CodeMirror 6, with the current `<textarea>` (**E-TA**) as baseline |
| **D3 store** | Does the Postgres revision store with in-process Git hashing meet the budgets, export reproducibly, and purge completely? | **P-v2**: prototype store · **G-now**: today's Git workspace, measured as the baseline |

`Deln0r/loro-go` (pure Go) is not a candidate: it only handles Loro's wire format and cannot apply edits or produce text.

## 1. Ground rules

- **Isolation.** Prototype code lives on a throwaway branch `proto/storage-v2`, in a separate Go module under `proto/`, so cgo and WebAssembly dependencies never touch the main module. It is never merged. The results report and decisions are the deliverable.
- **Dependencies.** Pin every new dependency and say why, as the engineering rules require: `loro-go`, `wazero`, `loro-crdt`, `@tiptap/*` (including `@tiptap/markdown`), plus any CodeMirror extensions beyond those already in `package.json`.
- **Data.** Fixtures are synthetic or public domain. Real notebooks are used only for the D3 round trip, and only with explicit authorization from each instance's operator. They are processed locally and never committed or logged. ("Personal-content migration requires separate authorization.")
- **Machines.**
  - Server benchmarks run on the development server (20 cores, 46 GB) with `GOMAXPROCS=4` to model a small host. Note the CPU model and kernel.
  - Browser benchmarks use Playwright Chromium in the existing `fmn-dev-browser:local` image, unthrottled and with a 4× CPU throttle, plus one manual check on a real laptop.
- **Repetition.** Report the median of 5 runs, and p50/p95 for latency distributions. Keep the raw results in `proto/storage-v2/results/`.

## 2. Fixtures

| ID | Content | Use |
| --- | --- | --- |
| **F1 novel** | *Moby-Dick* (Project Gutenberg, public domain; header and footer removed; about 1.2 MB, about 210k words), converted to one Markdown note with a front matter block and 135 `##` chapters. It is fetched by a script with a pinned SHA-256, not committed. | Engine, editor and store |
| **F1h history** | A seeded simulator replays 5,000 revisions onto F1, using 3 human and 2 agent actors with trailers. The mix: 70% word or sentence edits, 15% paragraph inserts or deletes, 5% chapter moves, 5% front matter changes, 5% renames. | Engine history, store history and export |
| **F2 notebook** | 2,000 generated notes (2–20 KB each, with `fmn-id`), 200 generated images (1–5 MB), comments, locks, a trash index and a `Trash/` folder; 20,000 revisions over 18 months of simulated time | Store, read model, export and import |
| **F3 real** | Bundles of the single-host instance's notebooks, and the hosted instance's only if its operator authorizes them | D3 round trip only |
| **T1 traces** | Recorded edit traces: 10 minutes of typing at the start, middle and end of F1; a 50 KB paste; deleting a chapter; 3 concurrent clients plus an agent patch every 5 s; IME composition sequences | Engine and editor |

## 3. Harnesses and measurements

### 3.1 D1: engine harness (Go and browser)

Implement the `liveEngine` interface (design §9) for Y, L-cgo and L-wasm, plus a matching JavaScript adapter.

| Metric | Method |
| --- | --- |
| Load F1 with full F1h history: time and RSS | Separate process per run; `/proc/self/status` VmHWM |
| Snapshot size with full history, and after compaction or shallow snapshot | Bytes |
| Apply: 10k single-character updates; 1 remote batch of 1k operations | `testing.B`, ns/op and allocations |
| `EncodeSince`, `Text()` and `Version()` on F1 | `testing.B` |
| Agent `Patch` against a base 1 minute, 1 hour and 1 day (500 revisions) old | Success rate and latency. Yjs needs a retained base snapshot; note how much it costs to keep it. |
| History: materialize revision N; diff between N and M; blame for a 10 KB range | Latency and correctness against F1h ground truth. Y may report "not supported" or use revision diffs. |
| **Interop fuzz:** 3 peers (2 JS, 1 Go) with random operations including concurrent edits in the same paragraph, 10,000 rounds | All peers converge to byte-identical text, and Go and JS agree on every round |
| **Builds:** linux/amd64 and arm64 static images through the existing `Dockerfile`; a darwin development build; image size; runtime libraries (`ldd`) | Pass/fail and MB |
| Browser bundle size for the engine (gzipped) | esbuild metafile |

### 3.2 D2: editor harness (browser)

- **The block-by-block bridge, shared by E-MD and E-TT:**
  - Map each top-level editor block to a byte range of the Markdown text.
  - A local edit re-serializes only the changed blocks and sends a text change.
  - A remote or agent edit re-parses only the affected blocks, replaces those nodes and maps the cursor.
  - **Property test:** 10,000 random edits made through the bridge (local, remote and agent, interleaved) must give the same text as applying them directly. Untouched blocks must stay byte-identical.
- **Section mode:** the same bridge bound to the text range of one top-level-heading section, with an outline to move between sections.
- **Prototype each WYSIWYG candidate (E-MD, E-TT) to these must-haves:**
  - headings, emphasis, links, images, task lists, block quotes, code blocks and tables;
  - comment highlights;
  - IME composition;
  - per-user undo through the engine;
  - read-only mode;
  - binding to the D1 engine through the bridge.

  Leave out toolbar polish, the slash menu and math, but record the effort to add each to both libraries.
- **Markdown fidelity:** round-trip F1 and a Markdown stress file (nested lists, mixed list markers, tables, HTML, footnotes, front matter, wiki links, hard breaks, escapes) through each library's parser and serializer, and list every byte-level change.
- **E-CM6 Source mode:** CodeMirror 6 with `@codemirror/lang-markdown`, bound to the engine directly. Measure it against **E-TA** (current `<textarea>`).
- **Today's code as a further baseline:** run the current Visual mode (`ui/markdown-editor.mjs`, `frontend/milkdown.mjs`, whole-document replace) on the same F1 document.

| Metric | Method |
| --- | --- |
| Time to interactive when opening F1 | `performance.now()` from navigation to first editable frame |
| Keystroke latency at the start, middle and end | Event Timing API (`PerformanceEventTiming` for `keydown`/`input`, duration to next paint); 500 keystrokes from T1; p50/p95 |
| Remote update cost | Inject 1 remote update every 100 ms during typing; long tasks (`PerformanceObserver('longtask')`); local keystroke p95 while updates arrive |
| Scrolling the full document | Frames over 50 ms during a scripted scroll of the whole document |
| Memory | CDP `Performance.getMetrics` `JSHeapUsedSize` after opening and after 10 minutes of typing |
| Exact round trip | Bytes after typing and undo equal the expected bytes, with no list-marker or spacing changes |
| Bundle size | Gzipped editor chunk(s), compared with Milkdown at 209 KB |
| Mitigations (full document) | Repeat keystroke and scroll metrics with `content-visibility: auto` on top-level blocks, and with spellcheck on and off (Chrome is reported to slow down with spellcheck) |
| Hands-on check (the maintainer) | 20 minutes writing in E-MD and E-TT on a laptop, full document and section mode: comfort, how section switching feels, any blocking gaps |

### 3.3 D3: store harness (Go and Postgres)

Use a disposable cluster from the existing `internal/pg/pgtest` and the existing `internal/blob` directory backend.

| Metric | Method |
| --- | --- |
| Save or checkpoint changing one paragraph of F1: latency and storage growth | p50/p95; bytes added to Postgres plus blob |
| Multi-file operation (move 50 notes): latency | p95 |
| Open F1 (warm and cold read model); list and status on F2 | p95 |
| Per-note history (F1h, 5,000 revisions); `ChangesSince` over 1,000 revisions | p95 |
| Read-model memory for F2 | RSS difference |
| Import F2 and F3 from Git: time | Wall clock |
| **Reproducibility:** export F2 and F3; compare head SHAs to the originals; `git fsck --strict`; export → bundle → import → export SHA identity | Pass/fail; any mismatch blocks progress |
| Incremental export of one revision; full export of F2 | Latency and wall clock |
| Idle database load with a gateway running | Transactions/s from `pg_stat_database` over 10 minutes |
| **Purge completeness and cost** | Plant a unique fake secret in F2 notes across 300 revisions, including a live note with pending edits. Purge it with text redaction. Then scan every stored byte for it: Postgres rows (`pg_dump` text), blob store, decompressed export objects (`git cat-file --batch-all-objects`), live snapshots and updates, and index tables. Also check: references resolve through the mapping; the export round trip still holds after the purge; a force-push to a local bare "GitHub" works only with the purge flag. Record the time for a notebook with 20,000 revisions. |
| **Baseline G-now:** the same operations on today's workspace (Git subprocesses) | Same metrics, for before/after |

## 4. Pass/fail budgets

These come from the design (§13). A candidate must meet every row marked **must**.

| Area | Budget | Level |
| --- | --- | --- |
| Store: open F1 warm / cold | p95 < 50 / 300 ms | must |
| Store: one-paragraph checkpoint of F1 | p95 < 30 ms; < 16 KiB growth | must |
| Store: per-note history of 5,000 revisions | p95 < 50 ms | must |
| Store: reproducible export and round trip on F2 and all F3 | 100% identical SHAs; `fsck` clean | must |
| Store: idle database load | < 5 tx/s | must |
| Store: purge completeness | Zero occurrences of the planted secret in any stored byte; references resolve; round trip still holds | must |
| Store: purge time for 20,000 revisions | < 60 s | should |
| Editor: keystroke p95 in the middle of F1, full document or section mode | < 16 ms unthrottled; < 50 ms at 4× throttle | must |
| Editor: remote and agent updates while typing | No long task over 50 ms per update; cost proportional to the change, not the document | must |
| Editor: bridge property test | 10,000/10,000 rounds; untouched blocks byte-identical | must |
| Editor: Source mode (E-CM6) on the full F1 | Same keystroke budget as above | must |
| Engine: interop fuzz | 10,000/10,000 rounds converge with Go and JS agreeing | must |
| Engine: static builds | linux/amd64 and arm64 through the existing Dockerfile | must |
| Engine: F1 with full history, RSS | ≤ 2× Y | should |
| Engine: agent patch from a 1-day-old base | Succeeds without a retained snapshot | should (it shows what Loro adds) |
| Bundle (engine plus editor, gzipped) | ≤ today's Milkdown and Yjs chunks combined | should |

## 5. Decision rules

- **D1 engine.**
  - Choose **Loro** only if one Loro option passes every **must** row in §4, and it meets at least two of the **should** rows or shows working blame and time travel that Y does not.
  - Between the two Loro options, prefer L-wasm if it is within 2× of L-cgo on load and apply, because it keeps the build CGO-free. Choose L-cgo only if the cgo build works in the existing multi-platform Dockerfile with no runtime library dependencies.
  - Otherwise choose **Y**, with blame computed from revision diffs.
- **D2 editor.**
  - **WYSIWYG library:**
    - Keep **E-MD** (Milkdown) unless **E-TT** (Tiptap) passes more **must** rows, has clearly better Markdown fidelity, or makes the missing features clearly cheaper.
    - Whichever wins must also pass the hands-on check with no blocking gaps.
  - **Section mode:**
    - Not needed if the chosen library meets the keystroke budget on the full F1 document with mitigations.
    - Otherwise ship section mode for notes above a size threshold. Set the threshold at the largest document size that still meets the budget, with headroom.
  - **Source mode:** adopt **E-CM6** if it meets its keystroke budget on F1. This is expected; the `<textarea>` is only the baseline.
- **D3 store.**
  - Proceed to M1 if every store **must** row passes.
  - A reproducibility failure on any real notebook blocks progress until its cause is fixed and covered by a test.
- **Record each decision** in design §16/§17 and in project memory, with the numbers behind it.

## 6. Order of work and stop rules

There is no calendar. Each step starts when the previous one is done, and decisions are made as soon as the numbers exist.

| Step | Work | Stop rule |
| --- | --- | --- |
| 1 | Fixture scripts (F1, F1h, F2); the P-v2 store with in-process Git hashing, read model and exporter; reproducibility on F2; G-now baseline | If Git hashing can't reproduce F2's SHAs, stop and reassess D3 before anything else |
| 2 | Engine adapters Y, L-cgo and L-wasm; interop fuzz; build checks; history and patch measurements; F3 round trip, if authorized | A Loro option that won't build in the existing Dockerfile, or fails its smoke test after a reasonable attempt, is marked failed. The other options continue. |
| 3 | The block-by-block bridge and its property test; E-MD and E-TT prototypes, full document and section mode; fidelity round trips; E-CM6 Source mode; hands-on check; results report and decisions | If a library can't render a must-have, measure what exists and estimate the rest. If the bridge's property test can't pass, stop and reassess D2 before anything else. |

## 7. Deliverables

1. `proto/storage-v2/RESULTS.md`: every table in §3 filled in, with budgets marked pass or fail, decisions D1–D3 with reasons, and an estimate of lines deleted and added for M1–M6.
2. Updated design doc: answers to the open questions, budgets adjusted if the data justifies it, and the chosen engine and editor.
3. A recorded MCP contract suite (design §4.2) against the current server, kept for M1 onwards.
4. The prototype branch, for reference only.

## 8. Out of scope

- Production code changes and the migration tools beyond what the reproducibility check needs.
- UI polish.
- Purge.
- Multi-gateway live fan-out. Measure on one gateway; NOTIFY fan-out is a known pattern already used for revision announcements.
