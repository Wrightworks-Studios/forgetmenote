# Storage v2 performance budgets: results

- **Status:** measured on `storage-v2` after the Git backend's deletion. The server, store, export and idle budgets are met, the checkpoint budget after a fix made here. The purge-time budget (a *should*) is missed on this loaded host even though the export rebuild now takes less than half as long. The browser budgets for typing in the F1 manuscript and for remote edits were missed; their causes were outside the store ([Missed budgets](#missed-budgets)). Stream livewire then made the live wire incremental, stopped the draft record from copying the note and sent edits at once: keystroke to paint at 4× and remote edits now meet their budgets, and keystroke to paint at 1× stays under 2 ms above the frame floor, which itself is at the budget ([Browser budgets after the incremental live wire](#browser-budgets-after-the-incremental-live-wire)). Since stream lp-s5 (2026-11-24) Visual is a CodeMirror live preview over the whole note, and the editor and engine bundle is within its budget ([Browser budgets on the live preview](#browser-budgets-on-the-live-preview)).
- **Date:** 2026-11-23
- **Brief:** wave 3 item 4: every budget of the design (§13) and of the bake-off (§4) on the fixtures F1 (with its F1h history) and F2.
- **Code:** branch `v2/budgets` from `storage-v2` at `5ff896d`; the export of one revision was measured again on the final export code, branch `v2/exportops` at `82529a8`. The benchmarks are `internal/workspace/budgets_test.go`, `internal/gitexport/budgets_test.go` and the existing `internal/notestore/bench_test.go`; the browser measurements are `scripts/browser/editor-budgets.spec.mjs`.
- **Fixes:** live snapshots are stored uncompressed (migration `0019`), which brings a manuscript checkpoint from 46 ms to 19 ms. The export writes one pack instead of loose objects for a purge's rebuild and a first export of 100 revisions or more, then computes its deltas: on the same slow disk, F2's rebuild went from 187–192 s to 69–74 s and its first export from 156–157 s to 69–70 s.

## Results

Each figure is the median over 3 runs of that run's p95 (or of the one-shot time), with the range of the 3 runs in brackets. "Met" and "missed" compare that median with the budget. The disk of this host slowed down sharply from about 05:40 (see [Variance](#variance)), so fsync-bound figures are taken from the runs before that where both exist.

### Design §13

| Operation | Budget | Measured | Verdict |
| --- | --- | --- | --- |
| Open the 1.2 MB manuscript on the server, warm / cold | p95 < 50 / 300 ms | Read through the workspace: **0.43 ms** [0.39–0.47] warm, **7.1 ms** [7.0–7.1] after a restart. The editor's first live read: **16.2 ms** [15.9–16.9] without a live document, **9.2 ms** [9.0–9.7] loading a stored one | met |
| Checkpoint changing one paragraph | p95 < 30 ms; growth < 16 KiB | Live checkpoint: **18.9 ms** [18.6–22.8] (was 46.0 [44.6–51.2] before the fix); revision storage **7.5 KB** [7.3–8.1]. Explicit save: 5.4 ms [5.2–5.8], 7.3 KB. With production durability (`fsync`, `synchronous_commit` and `full_page_writes` on), rerun in the budgets review: **22.5 ms** [20.6–24.3] with `0019`, 47.6 ms [47.3–51.1] without; 7.3 KB | met after the fix, with production durability too |
| Keystroke to paint, middle of the manuscript | p95 < 16 ms (1×), < 50 ms (4×) | With the live preview over the whole note (stream lp-s5, [below](#browser-budgets-on-the-live-preview)): Visual **16.9 ms** [16.8–17.0] at 1×, **19.7 ms** [19.6–20.0] at 4×; Source 16.8 [16.7–16.9] / 19.8 [19.5–19.8]; Side by side 17.2 [17.0–17.4] / 32.9 [32.9–35.3]. The frame floor (a bare contenteditable) is 14.6 / 16.5 ms. Before, with Milkdown in section mode after stream livepolish: Visual 17.1 / 19.8 ms, Source 17.9 / 18.8 ms | 4×: met; 1×: missed by 0.8–1.2 ms |
| Remote edit visible on another client, same gateway | p95 < 150 ms | After stream livewire: another browser's edit **65 ms** [59–65], an agent's patch **58 ms** [26–59]. Before: 457 ms [456–478] and 139 ms [134–158] | met |
| Per-note history, 5,000 revisions | p95 < 50 ms | F1h manuscript: a page of 50, **2.8 ms** [2.5–3.3]; all 4,992 revisions through 250 renames, **24.5 ms** [22.3–24.8] | met |
| Notebook status / listing (2,000 notes), warm | p95 < 10 ms | F2: status **0.0005 ms**, list **1.8 ms** [1.8–1.8], browse 2.2 ms, collections 1.9 ms | met |
| Export of one new revision; full export of 20k revisions | < 50 ms; < 2 min | One revision, on the final code (`82529a8`, 09:49–10:12): **27.5 ms** [6.2–40.2] p95; on the code before the pack change, which wrote it the same way: 6.4 ms [6.4–12.5] before the disk slowed, 0.09–2.8 s after. The whole F2 history as one pack, deltas computed: **50.4 s** [34.7–63.8] on the final code; 69.9 and 69.4 s on the slowed disk; as loose objects, before the fix: 53.2 s [52.0–80.7] before the disk slowed, 134.5–157.2 s after | one revision: met on the final code, missed on the slowed disk; full export: met after the fix |
| Idle database load per gateway | < 5 transactions/s | **2.6** tx/s [2.56–2.64] with nobody connected; **3.9** [3.90–3.93] with one tab open on a note | met |

### Bake-off §4

| Area | Budget | Level | Measured | Verdict |
| --- | --- | --- | --- | --- |
| Store: open F1 warm / cold | p95 < 50 / 300 ms | must | As above; the notes store alone: 0.37 ms [0.34–0.54] / 12.4 ms [11.9–12.8] | met |
| Store: one-paragraph checkpoint of F1 | p95 < 30 ms; < 16 KiB | must | As above, with production durability too (22.5 ms [20.6–24.3]); notes store `Apply` alone: 4.6 ms [4.3–4.9], 7.6 KB (3.1 KB of it a block) | met after the fix |
| Store: per-note history of 5,000 revisions | p95 < 50 ms | must | As above. The synthetic history of `BenchmarkHistoryOf5000Revisions` (no `ANALYZE`): a page 4.1 ms, all 5,000 revisions 54.6 ms [48.8–57.5] | met (F1h) |
| Store: reproducible export and round trip, F2 and all F3 | identical SHAs; `fsck` clean | must | F2, 15 runs of `BenchmarkBudgetExportF2` across the three versions below: import → verify → export (head SHA equals the fixture's) → `repack` and `fsck --strict` → 40 new revisions → (12 of the runs) purge and rebuild → bundle → import into a second notebook, whose export has the same head. 13 passed. Two runs of the first pack version, whose exports lacked deltas, failed at the bundle import on the slowed disk, most likely at its 90 s validation limit ([Found while measuring](#found-while-measuring)); the final version passed 2 of 2, and 3 of 3 more on `82529a8`, whose bundle check has time in proportion to the bundle. F3: the [cutover rehearsal](storage-v2-results-cutover.md) exported all 5 of the single-host instance's notebooks byte for byte | met |
| Store: idle database load | < 5 tx/s | must | As above | met |
| Store: purge completeness | no stored copy; references resolve; round trip holds | must | `TestPurgeLeavesNoStoredCopyOfARedactedSecret` on the full F2: 10 of 10 runs pass (3 before the pack writer, 7 after) | met |
| Store: purge time for 20,000 revisions | < 60 s | should | Plan, execute and export rebuild: 84.4 s [83.2–162.5] with loose objects before the disk slowed; 97.7 s with the final pack writer on the slowed disk (one run), where the rebuild alone took 69–74 s against 187–192 s with loose objects | missed |
| Editor: keystroke p95 in the middle of F1, full document or section mode | < 16 ms (1×); < 50 ms (4×) | must | The whole document, in the live preview (stream lp-s5): 16.9 / 19.7 ms. The same editor on a 53 KB note: 16.5 / 19.4 ms. Before, Milkdown in section mode after stream livewire: 17.5 / 28.9 ms | 4×: met; 1×: missed by 0.9 ms (floor 14.6 ms) |
| Editor: remote and agent updates while typing | no long task over 50 ms per update; cost follows the change | must | After stream livewire, an agent patch every 500 ms while typing in the shown section: no long task at 1× or 4×; keystroke p95 16.3 / 33.4 ms; live answers of 713–753 bytes. Before: 70 [65–85] long tasks up to 110 ms at 1×, 551 up to 518 ms at 4×; keystroke p95 138 / 794 ms; every live response the whole note and its whole Yjs state, 2.8 MB | met |
| Editor: bridge property test | 10,000/10,000 | must | No longer applies: the bridge was removed with Milkdown (stream lp-s5). Every mode edits the Markdown text directly; the toolbar commands have their own property test (`internal/web/ui/markdown-commands.test.mjs`) and `editor-corpus.spec.mjs` checks that looking around and switching modes sends nothing and keeps every byte. Before: `markdown-bridge.test.mjs`, 23/23 tests, including the 10,000-round property test | — |
| Editor: Source mode (CodeMirror 6) on the full F1 | as the keystroke row | must | With the live preview's editor (stream lp-s5): 16.8 ms at 1×, 19.8 ms at 4×, no long task. After stream livewire: 17.5 / 23.2 ms (before: 18.7 / 96.5 ms, 34 long tasks up to 456 ms) | 4×: met; 1×: missed by 0.8 ms |
| Engine: interop fuzz | 10,000/10,000 | must | Not re-run: the harness is on the bake-off branch. Y passed 10,000/10,000 ([engine results](storage-v2-results-engine.md)) | not re-measured |
| Engine: static builds | linux/amd64 and arm64 | must | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64 go build ./cmd/forgetmenote`: both statically linked (`file`), 44.6 and 40.7 MB. Not built through the Dockerfile | met with `go build`; Dockerfile build not run |
| Engine: F1 with full history, RSS ≤ 2× Y | | should | Not applicable: D1 chose Y | — |
| Engine: agent patch from a 1-day-old base | succeeds without a retained snapshot | should | Not met, by D1: Y keeps patch bases for 15 minutes and 16 MiB (`retainLiveVersions`) | missed (by design) |
| Bundle, engine plus editor, gzipped | ≤ 237.2 KiB (Milkdown 208.7 + Yjs 28.5 before v2) | should | **227.3 KiB** on `storage-v2` with the live preview merged (built at `6599751e`, 2026-11-24): the note editor's entry (Visual, Source and Side by side) and the collaboration entry with what they import beyond the app's own chunks ([command](#commands)). 221.5 KiB on the lp-s5 branch; before the live preview, 242.4 KiB: the Milkdown chunk and the collaboration chunk | met |

The bake-off's editor harness (its §3.2) measures the remote-update budget with one remote update every 100 ms while typing; `editor-budgets.spec.mjs` sends an agent patch every 500 ms, a fifth of that rate.

### Further server measurements

No budget names these; they are reported for comparison with later changes.

| Measurement | Result (median of 3 runs' p95) |
| --- | --- |
| F1h replay into the notes store, 5,001 revisions | 26 s |
| F1: one browser keystroke applied on the server (`LiveApply`) | 4.9 ms [3.1–5.2] |
| F2 replay, 19,999 revisions | 85 s |
| F2 list / `Find` after a restart (cold) | 359 ms [295–382] / 420 ms [411–441] |
| F2 `Find` over the cached listing: a rare word, a common word, a tag (warm) | 10.0 / 11.0 / 7.2 ms. No request path runs `Find`; searches run `FindScoped` (next rows) |
| F2 search as `find_notes` and `/api/notes?q=` run it (`FindScoped`, default search scope, at most 100 results): a rare word, a common word, a tag (warm) | **986 / 1,031 / 508 ms** [937–1,072 / 928–1,084 / 505–527]; p50 854 / 878 / 473 ms. On the 2,000-revision F2 (200 notes): 88 / 94 / 45 ms. Measured 2026-11-23 at a load average of 8.5 to 25, beside `Find`'s 13.7 / 15.5 / 12.6 ms ([raw results](#raw-results)); not a budget, and slow ([Found while measuring](#found-while-measuring)) |
| F2 search after a restart (`FindScoped`, cold) | 4,156 ms [1,472–4,843]; p50 1,555 ms [1,161–1,732] |
| F2 history: a note (page), all 60 revisions of the busiest note, the notebook (page) | 1.9 / 0.7 / 0.6 ms |
| F2 changes since 1,000 revisions ago | 10.6 ms [10.1–18.7] |
| F2 save of a note | 4.6 ms [4.0–5.6] |
| F2 move of a folder of 49 notes (one revision, links rewritten) | 732 ms [718–805] |
| F2 Git import / verify / repack and `fsck` / bundle / bundle import (04:30–04:51) | 91 s [74–94] / 12.2 s / 39.6 s / 2.1 s / 50 s [48–69] |
| Notes store listing of 2,000 files, warm / cold | 0.21 / 4.7 ms |

## Fixes

### Live snapshots are stored uncompressed

The live checkpoint of a one-paragraph change to F1 took 46 ms at p95. Timing each step of `checkpointLive` showed 27 ms in the compaction that follows the commit (`liveCompact`), almost none of it in Go: Postgres compressed the rewritten snapshot of the 1.2 MB document with pglz on every write. Reading it back cost decompression too.

Migration `0019_live_snapshot_storage.sql` sets `live_docs.snapshot` to `STORAGE EXTERNAL`: out of line, uncompressed. `TestLiveSnapshotsAreStoredUncompressed` failed before it (`pg_column_compression` was `pglz`) and passes after. `lz4` compression measured nearly the same (19.5 ms p50), but it needs a Postgres built with lz4, and the schema supports Postgres 14 and later without that guarantee.

| F1 after F1h, p95 | Before | After |
| --- | --- | --- |
| Live checkpoint of one paragraph | 46.0 ms [44.6–51.2] | **18.9 ms** [18.6–22.8] |
| First live read, no live document | 40.0 ms [38.1–42.5] | **16.2 ms** [15.9–16.9] |
| First live read, stored document | 15.2 ms [14.7–15.2] | **9.2 ms** [9.0–9.7] |

The cost is space: the snapshot of F1 is about 0.5 MB larger uncompressed (the one-time growth when the first compaction rewrote it). Snapshots written before the migration stay compressed until they are next rewritten.

### Large exports and a purge's rebuild write one pack

The bake-off budget for a purge of 20,000 revisions is 60 s. Before this change the full F2 purge took 3.7–4.5 s to plan, 8.0–8.2 s to execute and 71–150 s to rebuild the export (`TestPurgeLeavesNoStoredCopyOfARedactedSecret`, 3 runs). Timing `Rebuild`'s steps on F2 (`BenchmarkBudgetExportF2`, temporary logging) gave 67 s writing objects, 14 s for `git gc --prune=now` and 6 s for `git fsck`, 87 s in all. The rebuild wrote every object of the history again as a loose object, and each loose object waits for two fsyncs. On the 2,000-revision F2, the same writes took 5.4 s with the fsyncs and 0.9 s without them. A first export of the whole history pays the same: 50–157 s for F2.

`Rebuild`, and a first export of 100 revisions or more (`Recreate`'s too), now stream each object once, compressed on four goroutines, into one pack that `git index-pack --stdin` checks, indexes and syncs once (`internal/gitexport/pack.go`). Later exports, the usual one per commit and catch-ups alike, still write loose objects in this process. The pack is staged as `objects/pack/tmp_rebuild_*`, which Git ignores; if a process stops halfway, the repository's next maintenance (`Maintain`) or rebuild removes it.

The pack's objects are whole, and a repack reuses whole objects as it finds them: the first version of this change (`fcb1c4c`, `013306e`) left F2's export at 932 MB instead of 727 MB, and its 952 MB bundle is the one whose import twice ran out of time, most likely ([Found while measuring](#found-while-measuring)). So a first export written as one pack is repacked with fresh deltas (`git repack -a -d -f`) once the branch names it, and the rebuild runs that repack and `git prune --expire=now` instead of `git gc --prune=now` (`46b31f6`). On F2 the repack takes about 20 s and gives 727 MB. The purge flag and the final `fsck` are unchanged, and the existing export and rebuild tests pass unchanged. Two tests were added:

- `TestExportOfManyRevisionsAtOnce` exports 25 revisions as the first export (threshold lowered to 10), then 30, then one: each export verifies against the store and passes `git fsck --strict`; the first leaves one pack, with deltas, and the later ones loose objects. It fails on the loose-object code (no pack) and on the first pack version (no deltas).
- `TestRebuildOfHistoryThatBringsObjectsBack` rebuilds history in which later revisions bring objects back (an undone edit, a file deleted and written again) next to a stale staged pack, and checks that the rebuilt export stores versions as deltas. It fails on the first pack version (no deltas) and passes on the loose-object code.

Every timing (`BenchmarkBudgetExportF2` and the purge test), by version and time of day; the disk slowed from about 05:40:

| F2, 20,000 revisions | Loose objects | One pack, first version (no deltas) | One pack with the delta repack (`46b31f6`) |
| --- | --- | --- | --- |
| First export of the whole history | 53.2 s [52.0–80.7] (04:30–04:51); 50.2, 88.1, 134.5 s (05:31–06:03); 157.2, 156.0 s (07:13, 07:38) | 37.0, 45.3 s (06:25–06:51); 51.6 s (07:28) | **69.9, 69.4 s**, the 20 s repack included (08:24–08:42) |
| `repack` and `fsck` of that export (maintenance) | 39.5–43.5 s | 8.2–8.3 s | 5.3, 5.5 s |
| Export rebuild after a purge, in the benchmark | 86.6 s (05:17); 186.9, 192.1 s (07:13, 07:38) | 38.1, 115.2, 53.2 s (05:31–06:03); 76.8, 79.7 s (06:25–06:51); 71.4 s (07:28) | **73.9, 68.9 s** (08:24–08:42) |
| Whole purge: plan, execute, rebuild (`TestPurgeLeavesNoStoredCopyOfARedactedSecret`) | 84.4 s [83.2–162.5] (04:51–05:10) | 74.2, 114.1, 102.2 s with only the rebuild as a pack (06:03–06:25); 94.1, 101.0, 100.8 s (06:51–07:12) | **97.7 s**: plan 9.1, execute 7.5, rebuild 81.1 s (08:42, one run) |

The rows at 07:13–07:54 are an A/B on the slowed disk: test binaries of the same benchmark built from `1af2f85` (loose objects) and `013306e` (first pack version), run alternately. A second pack run (07:54) stopped at its bundle import before it reported ([Found while measuring](#found-while-measuring)).

Measured side by side on the same disk, the rebuild takes 69–74 s instead of 187–192 s, and the first export 69–70 s instead of 156–157 s; before the disk slowed, the first pack version rebuilt F2 in 38 s against 87 s. The whole purge still misses 60 s on this host: planning and execution took 11–23 s under the same load, and the rebuild's repack (deltas computed on one thread, as the export's configuration asks) and `fsck` are CPU- and disk-bound. On a quieter disk its parts would add up to about 4 + 8 + 38 s plus the 20 s delta repack, which is still around 60 s; that sum is an estimate from the parts measured separately, not a measured purge.

## Browser budgets after the incremental live wire

- **Stream:** livewire, branch `v2/livewire` at `8739905` from `storage-v2` at `1a7e51e`, 2026-11-23.
- **Changes** ([live collaboration](../collaboration.md)):
  1. **Incremental answers.** A long poll and an update name the live revision the tab holds; the server answers with only the Yjs update since that revision (`EncodeSince` on the patch base it keeps) and the revision and checkpoint metadata, without the Markdown. A tab's own update is acknowledged with what it lacks. The whole note goes only to a first read, a new generation, a revision the server no longer keeps, and requests that name none, which keeps the MCP tools and tabs running the previous JavaScript as they were. A live answer is now 340–760 bytes (median per run) instead of 2.8 MB.
  2. **Draft record.** Typing in a connected live note no longer writes the note into `localStorage`: the draft record is written whole once, when the note first holds unsaved text, and the live record (written after each change, once it has painted) journals the text changes made since, with only the Yjs update the server has not acknowledged and the operation in flight. Recovery folds the journal back into the draft record.
  3. **Send schedule.** A change is sent once it has painted when no update is in flight; changes made while one is in flight are sent together when it is acknowledged. Long polls follow one another 50 ms apart instead of 250 ms.
- **Method:** as [below](#method), with one difference: the spec now turns Playwright's trace off. The suite keeps a trace of failed cases, so every key press of the budget spec was traced, and the trace snapshots the whole page in the page's own thread around every action: in a CDP profile of typing 200 keys into F1's Source at 4× it took 13.3 s of 27.7 s, and it lengthened the keystrokes measured. *Before* is `storage-v2` at `1a7e51e` (a `git archive` copy) with only that spec change; *after* is the branch. Three runs each, before at 18:32–18:49 and after at 18:49–19:01 UTC, on the development server with a 1-minute load average of 2.0 [1.2–3.7] and 2.6 [1.4–4.2] (median [range] of 30 s samples).

| Budget | Target | Before | After | Verdict |
| --- | --- | --- | --- | --- |
| Keystroke to paint, F1 Visual (section mode), 1× | p95 < 16 ms | 19.0 [18.7–19.5] | **17.5** [17.3–18.2] | missed by 1.5 ms |
| Keystroke to paint, F1 Visual (section mode), 4× | p95 < 50 ms | 54.7 [41.9–55.4] | **28.9** [27.6–30.5] | met |
| Keystroke to paint, F1 Source, 1× | p95 < 16 ms | 18.7 [18.0–19.2] | **17.5** [16.6–17.6] | missed by 1.5 ms |
| Keystroke to paint, F1 Source, 4× | p95 < 50 ms | 56.6 [56.5–57.9] | **23.2** [21.6–23.5] | met |
| Frame floor (bare contenteditable), 1× / 4× | — | 15.9 / 15.9 | 15.7 / 16.3 | — |
| 53 KB note, Visual, 1× / 4× | — | 18.2 / 41.5 | 16.8 / 17.8 | — |
| Another browser's edit shown, same gateway | p95 < 150 ms | 432 [430–460] | **65** [59–65] | met |
| An agent's patch shown | p95 < 150 ms | 148 [132–149] | **58** [26–59] | met |
| Typing while an agent patches every 500 ms: keystroke p95, 1× / 4× | no long task over 50 ms per update | 164.6 / 724.6, with 90 / 100 long tasks up to 115 / 456 ms | **16.3 / 33.4**, no long task | met |
| Idle database load per gateway, one tab open on a live note | < 5 tx/s | 3.9 ([idle load](#raw-results), before this stream) | **3.94, 3.73, 3.76** (three 10-minute windows) | met |

- **1×.** Keystroke to paint at 1× is a frame: the bare contenteditable's p95 is 15.7 ms, at the budget itself, and F1 is 1.8 ms above it in both editors (the 53 KB note 1.1 ms). Nothing measured here takes a frame's time: no long task, and the work of a key press is 9–14 ms at p95 in a 16.7 ms frame. Marked missed, by its number.
- **Before at 4×.** With the trace off the spec types faster than the old 250 ms send delay, so the old JavaScript sent nothing until typing stopped (one live answer per run): its 4× keystroke figures measure typing without the live wire. The cost of the old wire shows when answers arrive: 725 ms at p95 while an agent patches.
- **With the trace on**, as the budgets stream measured and as the first runs here did: before (`1a7e51e`, 16:50–17:29, load 11.0 [7.3–15.4]) Visual 18.3 / 92.0 ms, Source 18.4 / 80.2 ms, remote edit 469 ms, agent patch 146 ms, typing with patches 151 / 795 ms with 78 / 620 long tasks; after (`c5ea021`, before the change that stores and sends after paint, 18:00–18:20, load 3.0 [1.7–6.2]) Visual 18.1 / 54.8 ms, Source 17.8 / 49.9 ms, remote edit 43 ms, agent patch 33 ms, typing with patches 18.5 / 58.8 ms with no long task.
- **Idle load** was measured on the branch at `c5ea021`; what came after it changes nothing while idle. The script is the one under [Commands](#commands), run on a copy of that commit.
- **Left in a key press** (profiles of 200 keys at 4× without the trace, per key): the editors themselves (CodeMirror's Lezer Markdown parse in Source; ProseMirror and the bridge in Visual, `getClientRects` alone 2.6 ms), and `pendingContainer` and the container badge, which read the whole note and so flatten its string on every key press (1.4–1.6 ms). The live session's share is the update's send (`fetch`, 0.8–0.9 ms) and the live record's write (`localStorage.setItem`, 0.1–0.2 ms).

## Browser budgets and typing load after the live follow-ups

- **Stream:** livepolish, branch `v2/livepolish` (measured at `e079703`) from `storage-v2` at `2b5057b`, 2026-11-23.
- **Changes** ([live collaboration](../collaboration.md)):
  1. **Keystroke path.** The note editor keeps its text in pieces of 8 Ki characters (`internal/web/ui/text-pieces.mjs`): slicing the whole 1.2 MB text for each edit made V8 copy all of it (0.6 ms and 2.4 MB of garbage a key at 1×), whichever code read the text first, so taking the container badge's reads off the keystroke alone would only have moved the copy. The badge and Save's label now follow once changes pause (250 ms, at least once a second). While live editing is connected the status line follows the session, not each key (it flipped two or three times a key, with its header layout and the focus header's observer: 0.7 ms a key at 4× in Visual), saying changes are being shared once a second passes with changes unsent and no answer, and the overlay keydown handler no longer searches the page for an open dialog on every key (0.5 ms a key at 4×).
  2. **Server work per key.** A live note's Yjs document stays loaded between requests and its state is encoded only when needed; stored updates carry a CRC-32C of their text instead of a SHA-256; a tab's own long poll is not answered with its own update; a Save answers with only the change; a gateway that lacks what an update builds on reads the stored document again instead of refusing it.
  3. **Sends.** A change waiting for its frame is sent at once when the tab is hidden; failed sends retry on their own backoff and at once when the browser comes online or a poll succeeds again.
- **Method:** as for [livewire](#browser-budgets-after-the-incremental-live-wire), trace off. *Before* is `storage-v2` at `2b5057b`: the worktree before any change, three runs at 20:36–20:48 UTC; *after* is the branch, three runs at 21:55–22:07 UTC. The 1-minute load average was 3.0 [2.0–4.5] before and 2.5 [2.0–4.5] after (median [range] of 30 s samples). The spec's global setup builds the app from the checkout it runs in, so each figure is that checkout's code.

| Budget | Target | Before | After | Verdict |
| --- | --- | --- | --- | --- |
| Keystroke to paint, F1 Visual (section mode), 1× | p95 < 16 ms | 16.9 [16.7–17.7] | **17.1** [16.8–18.0] | missed by 1.1 ms |
| Keystroke to paint, F1 Visual (section mode), 4× | p95 < 50 ms | 28.2 [27.7–28.7] | **19.8** [19.7–20.1] | met |
| Keystroke to paint, F1 Source, 1× | p95 < 16 ms | 17.3 [17.2–17.7] | **17.9** [16.9–18.3] | missed by 1.9 ms |
| Keystroke to paint, F1 Source, 4× | p95 < 50 ms | 21.8 [21.8–21.9] | **18.8** [18.6–19.1] | met |
| Frame floor (bare contenteditable), 1× / 4× | — | 16.0 / 15.9 | 15.0 / 16.3 | — |
| 53 KB note, Visual, 1× / 4× | — | 16.8 / 19.2 | 17.1 / 18.2 | — |
| Another browser's edit shown, same gateway | p95 < 150 ms | 65 [60–65] | **43** [19–55] | met |
| An agent's patch shown | p95 < 150 ms | 47 [46–48] | **25** [24–32] | met |
| Typing while an agent patches every 500 ms: keystroke p95, 1× / 4× | no long task over 50 ms per update | 17.0 / 32.5, no long task | **16.6 / 25.0**, no long task | met |
| Live responses while typing 500 keys in Visual | — | 651 [638–653] | 496 (each run) | — |

- **1×** is still missed, by about a frame's jitter: the work of a key press (keydown to the last input listener, p95) fell from 13.6 [12.5–14.5] to 7.9 [5.8–15.4] ms in Visual and from 9.3 [8.9–9.6] to 7.0 [6.7–7.1] ms in Source, and at 4× keystroke to paint fell by 8 ms in Visual and 3 ms in Source, but at 1× the paint lands in the frame after the key either way. Run to run, the 1× figures moved by more than the changes did: an intermediate build (the keystroke path without the last two changes, `b1c1b90`) measured 16.5 [16.4–16.5] in Visual and 16.7 [16.4–16.7] in Source in three full runs at 21:36–21:48 (load 5.1 [3.2–7.0]). Alternating runs of only the two keystroke tests, the last two changes were 0.1–0.5 ms slower in Visual at 1× and 4–9 ms faster at 4× (pairs A/B at 22:07–22:15, load 5.4–10.4: Visual 1× 16.6, 16.5, 16.4 against 17.0, 16.8, 16.7; 4× 30.1, 30.0, 27.3 against 24.5, 23.8, 22.7); without the status line change (the overlay fix alone) Visual 1× was 16.5, 16.4, 16.5 against 16.6, 16.9, 16.6 (22:16–22:25, load 2.9–5.4). The status line change stays: it removes work and the flicker, and every variant misses 16 ms at 1×.
- **What is left in a key press** (CDP profiles of 200 keys at 4×, before the last two changes): the editors themselves (CodeMirror's dispatch 5.0 ms a key and its Markdown parse; in Visual, ProseMirror and the bridge 10.4 ms, of which `getClientRects` 3.0 ms), and the rendering that follows (15–18 ms a key of "program" time). The live session's share is its send (`fetch` 0.9 ms a key at 4×, after the paint) and its answer (0.6 ms). None of these is in this stream's scope.
- **Remote edits** reach the other tab in 43 [19–55] ms at p95 (65 before; the intermediate build measured 20 [19–21]).

**Server load while typing** (`typing-load.mjs` below; one gateway, the browser fixture's cluster, F1 open live in Source, each tab typing a key every 125 ms for 60 s at its own place in the middle of the note; load 2.9–3.9; before at 22:26–22:28, after at 22:27–22:31 UTC):

| Typists | Version | Keys/s | Requests/s (updates + polls) | Transactions/s | Gateway CPU |
| --- | --- | --- | --- | --- | --- |
| 1 | before | 7.7 | 15.4 (7.7 + 7.7) | 48.2 | 7.0 % |
| 1 | after | 7.8 | **7.8** (7.8 + 0.0) | 48.5 | **1.8 %** |
| 2 | before | 14.8 | 34.5 (14.8 + 19.7) | 83.0 | 31.1 % |
| 2 | after | 15.5 | **30.7** (15.5 + 15.2) | 86.4 | **6.0 %** |

- A writer's key is now one request: its own long poll no longer answers with its update. With two typists each tab's long poll still answers the other's keys.
- Gateway CPU fell by four to five times: the document is no longer loaded and encoded for each key, and the text's SHA-256 (1.3–1.9 ms a key in F1 on this host) is a CRC-32C. `BenchmarkLiveTypingInTheNovel` (one key per `LiveApply`, incremental answers, 300 keys) took 5.6 [3.2–6.4] ms a key before and 1.5 [1.1–3.8] ms after (median [range] of three runs each, alternating, 22:43–22:45 UTC, load 3.1–3.5; `go test ./internal/workspace -run '^$' -bench LiveTypingInTheNovel -benchtime=300x` with F1 in `FMN_FIXTURE_CACHE`, and for *before* the same benchmark in the copy of `2b5057b`, naming the revision in `LiveUpdate.Since`).
- Database transactions per key did not change (about 6 a key). Reading the note at the head, which the record no longer repeats, was already served from the notes store's in-memory model. `LiveApply` alone makes about 5 a key (counted in `pg_stat_database` around 100 keys in a scratch test, not committed); among them are the stored update (`LiveAppend`), its announcement to other gateways (`pg_notify`) and the receipt lookup that makes a retried operation a replay. Not changed here.
- **Where keeping the document loaded stops saving work** (from the review of this stream; read from the code, not measured on such a note). Two limits fall back to the cost of a key before these changes, an encode of the whole state per key:
  - The live memory bound counts a loaded document as sixteen times its state (`liveDocBytes`). From about 7.5 MiB of state (more than 8 MiB at the latest), the record with its document counts more than the notebook's 128 MiB (`liveCapacity`) on its own, sooner beside other live notes, so `trimLive` unloads the document at every key, encoding its state, and the key's answer or the next key loads it again: an encode and a load per key, as `TestLiveDocumentPastTheCapacityIsLoadedAtEveryKey` shows with the capacity lowered. A checkpoint starts a document again only past 4 MiB plus twice its text (`dbLiveRestartBytes`), so a note of more than about 1.8 MiB of text can stay past the bound even after its checkpoints, and any note can reach it between checkpoints as edits add to its state.
  - `acceptLive` keeps a bound of the state it has not encoded (the last encoded size plus ten times each update since) and encodes the state whenever the bound passes half the state limit (`dbLiveStateLimit/2`: the file limit plus 4 MiB, 36 MiB by default); once the state itself is that large, every update encodes it.

  F1's 2.8 MB state, which counts 45 MB, reaches neither.
- **The authority fence on each key** (stream livefence, 2026-11-24 UTC): a stored update now locks the notebook's head row and reads the authority record in the transaction that stores it, sent with the update in one round trip, so a handoff another gateway prepares refuses it or sees it ([headless](../headless.md#upgrade-recovery-and-backups)). The store call alone (2,000 updates of 200 bytes, a scratch benchmark, not committed) took 0.122–0.130 ms before and 0.132–0.145 ms after (three alternating runs each, 02:09–02:11, load 4.9–7.2); with the lock in a round trip of its own it had taken 0.209–0.245 ms against 0.158–0.194 ms (02:07–02:08, load 10.5–19.2). `BenchmarkLiveTypingInTheNovel`, as above, showed no difference beyond its spread: 2.09–3.83 ms a key before (median 2.40) and 1.04–2.89 ms after (median 2.23) in four alternating runs each (02:11–02:13, load 2.5–3.8), and 1.35–2.07 ms against 1.53–2.35 ms in six runs each with the separate round trip (02:03–02:07, load 13–22). *Before* is `storage-v2` at `96bca54`.
- **Idle**, one tab left open on F1 after typing (a second one-typist run, 22:31–22:33: 7.7 keys/s, 7.7 requests/s, 48.2 transactions/s, 2.7 % CPU): **3.86** transactions/s over 10 minutes (22:34–22:43), 0.09 % CPU; budget < 5 tx/s per gateway, met.

The typing load was measured with this script, run as `FMN_FIXTURE_CACHE=$WT/.fixture-cache $B/fmn-node $WT node $WT/.fixture-cache/typing-load.mjs CHECKOUT TYPISTS 60 [IDLE_MINUTES]`, where CHECKOUT is the worktree or a `git archive` copy of `2b5057b` with its own `npm run build` and `node_modules` linked:

```js
// Server load while people type in F1: one or two signed-in Chromium tabs,
// each typing a key every 125 ms (8 keys/s) in Source at its own place in
// the middle of the note, for SECONDS; then, optionally, one tab left open
// and idle for IDLE minutes. Prints requests/s seen by the tabs, database
// transactions/s (pg_stat_database) and the gateway's CPU.
// Usage: node typing-load.mjs CHECKOUT TYPISTS SECONDS [IDLE_MINUTES]
import {execFileSync} from 'node:child_process';
import {readFileSync} from 'node:fs';
import {setTimeout as delay} from 'node:timers/promises';

const [checkout, typists = '1', seconds = '60', idle = '0'] = process.argv.slice(2);
process.chdir(checkout);
const {startApp} = await import(checkout + '/scripts/browser/app-fixture.mjs');
const {startPostgres} = await import(checkout + '/scripts/browser/postgres.mjs');
const cluster = await startPostgres();
process.env.FMN_BROWSER_DATABASE_HOST = cluster.host;
const query = statement =>
  execFileSync('psql', ['-X', '-At', '-h', cluster.host, '-U', 'fmn', '-d', 'postgres', '-c', statement], {encoding: 'utf8'}).trim();
const ticks = Number(execFileSync('getconf', ['CLK_TCK'], {encoding: 'utf8'}));
const app = await startApp({name: 'fmn-typing-', account: {username: 'typing-admin', password: 'DisposableTyping123!'}});
const path = 'Manuscripts/Moby-Dick.md';
const round = (n, d = 2) => Math.round(n * 10 ** d) / 10 ** d;
let browser;
try {
  const database = query(`SELECT datname FROM pg_database WHERE datname LIKE 'app\\_%' AND datname NOT LIKE '%baseline'`);
  const transactions = () => Number(query(`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = '${database}'`));
  const cpu = () => {
    const fields = readFileSync(`/proc/${app.child.pid}/stat`, 'utf8').split(') ')[1].split(' ');
    return (Number(fields[11]) + Number(fields[12])) / ticks;
  };
  await app.api('/p/default/api/note', {path, content: readFileSync(process.env.FMN_FIXTURE_CACHE + '/f1-moby-dick.md', 'utf8'), revision: ''});
  const {chromium} = await import(checkout + '/node_modules/@playwright/test/index.mjs');
  browser = await chromium.launch();
  const pages = [];
  for (let i = 0; i < Number(typists); i++) {
    const context = await browser.newContext({viewport: {width: 1280, height: 900}});
    const page = await context.newPage();
    page.requests = {update: 0, poll: 0};
    page.on('request', request => {
      if (!new URL(request.url()).pathname.endsWith('/api/live')) return;
      if (request.method() === 'POST') page.requests.update++;
      else page.requests.poll++;
    });
    await app.login(page, 'recovery');
    await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
    await page.locator('#document[data-live="connected"]').waitFor({timeout: 120000});
    await page.locator('#mode').click();
    await page.locator('#format-source').click();
    await page.locator('#source-editor .cm-content').waitFor({timeout: 120000});
    await page.evaluate(i => {
      const view = document.querySelector('#source-editor .cm-content').cmTile.root.view;
      const middle = view.state.doc.lineAt(Math.floor(view.state.doc.length / 2)).number;
      view.dispatch({selection: {anchor: view.state.doc.line(middle + 40 * i).to}, scrollIntoView: true});
      view.focus();
    }, i);
    pages.push(page);
  }
  await delay(5000);
  for (const page of pages) page.requests = {update: 0, poll: 0};
  const text = 'the whale rose slowly out of the grey sea and ishmael watched it from the deck ';
  let typing = true;
  const t0 = transactions(), c0 = cpu(), started = Date.now();
  const stop = delay(Number(seconds) * 1000).then(() => (typing = false));
  const keys = await Promise.all(
    pages.map(async page => {
      let n = 0;
      while (typing) {
        const key = text[n % text.length];
        await page.keyboard.press(key === ' ' ? 'Space' : key);
        n++;
        await delay(125);
      }
      return n;
    })
  );
  await stop;
  const elapsed = (Date.now() - started) / 1000, t1 = transactions(), c1 = cpu();
  const requests = pages.reduce((sum, page) => ({update: sum.update + page.requests.update, poll: sum.poll + page.requests.poll}), {update: 0, poll: 0});
  console.log(JSON.stringify({checkout, typists: Number(typists), seconds: round(elapsed, 1), keysPerSecond: round(keys.reduce((a, b) => a + b, 0) / elapsed),
    requestsPerSecond: round((requests.update + requests.poll) / elapsed), updatesPerSecond: round(requests.update / elapsed), pollsPerSecond: round(requests.poll / elapsed),
    transactionsPerSecond: round((t1 - t0) / elapsed), gatewayCPU: round((c1 - c0) / elapsed, 3)}));
  if (Number(idle) > 0) {
    for (const page of pages.slice(1)) await page.context().close();
    await delay(60000);
    const i0 = transactions(), ic0 = cpu(), idleStarted = Date.now();
    await delay(Number(idle) * 60000);
    const idleElapsed = (Date.now() - idleStarted) / 1000;
    console.log(JSON.stringify({checkout, idleTab: true, seconds: Math.round(idleElapsed), transactionsPerSecond: round((transactions() - i0) / idleElapsed), gatewayCPU: round((cpu() - ic0) / idleElapsed, 4)}));
  }
} finally {
  await browser?.close();
  await app.close();
  await cluster.stop();
}
```

## Browser budgets on the live preview

- **Stream:** lp-s5, branch `v2/lp-s5` from `v2/livepreview` at `f34258ce`, 2026-11-24 (UTC times below). Visual is the CodeMirror live preview over the whole note, with no section mode; Side by side is Source beside the rendered pane. Measured on the branch after the removal of Milkdown, the bridge and section mode and the two changes below (`72d8dcd4`); *before* is the same branch without the two changes (`421f1b8a`, its `frontend/markdown-syntax.mjs` put back for the run). The writing worker still carries Milkdown's remark until stream lp-s3's parser swap merges; it is not on the key press.
- **Changes on the key press**, found with CDP CPU profiles of 300 keys typed in the middle of F1 at 4× (`Profiler` at 100 µs sampling, an unminified build, the budget spec's pacing; figures per key, the profiler's own cost included):
  1. **The blocks around an edit.** The live preview's block field (tables, rules, video and front matter as widgets) asks for the top-level blocks around every edit and selection change (`topBlocks`, `frontend/markdown-syntax.mjs`), which read the Document's children from the first: in the middle of F1 about 1,280 blocks a call, several calls a key. The field's update took 5.0 ms of Visual's 32.1 ms a key, and was the whole difference between Visual and Source in the profiles. The cursor now enters the balanced children at the range's start: 0.22 ms a key, of it 0.03 ms in `topBlocks`.
  2. **The pane's whole-note parse.** With the pane open, the rest of the note was parsed in idle slices of 8 ms between keystrokes. Every edit gives CodeMirror a new parse that starts again from the note's start, reusing what it can, so the slices mostly walked the same blocks again: 2.1 ms a key, in tasks that can hold up the next key. The slices now start once the text has been still for 150 ms, as the pane's rendering already did: 0.0 ms a key while typing.
- **What is left in a key press**, alike in Visual and Source (profiles after the changes: 26.3 ms a key of busy time in Visual, 26.2 ms with the pane open): CodeMirror's dispatch and its incremental Markdown parse up to the viewport plus 100,000 characters (about 7.6 ms a key at 4×; Lezer's Markdown parser reuses the earlier blocks one at a time), the rendering that follows (11–12 ms of "program" time), the toolbar's state (0.4 ms) and the live session's send after the paint (1.2 ms).
- **Before against after, alternating** (one worker, Visual and Source keystroke cases and the pane case at 4×, runs B1, A1, B2, A2 at 11:00–11:13, 1-minute load 2.0–4.1): keystroke to paint p95 in Visual at 4× was 19.5 and 19.5 ms before and 19.6 and 19.6 ms after; with the pane open at 4×, 23.2 and 22.5 before and 22.9 and 22.8 after; Source 19.7 and 19.6 before, 26.3 and 19.6 after (one outlier). At this load a key's paint lands in the frame after it either way, so the work removed does not move the p95. It shows under load: in one run each at a 1-minute load of 7–9 (10:26 and 10:28), before the changes Visual measured 25.6 ms at 4× against Source's 24.7 in the same run; with the first change, 24.2 against 24.6. The S1 review's 40 ms for Visual at 4× was not reproduced in any of these runs.
- **Method:** `editor-budgets.spec.mjs` (trace off; the other client scrolls to the edited paragraph) and the F1 cases of `side-by-side.spec.mjs` (300 keys with the pane open, then the pane scrolled through the book and the editor back, then the same in Source), run as `FMN_DOCKER_ARGS="-v $F:$F:ro" FMN_IMAGE=fmn-dev-full:local $B/fmn-node $WT --cpus 4 sh -c "FMN_FIXTURE_CACHE=$F FMN_BROWSER_BUDGETS=$WT/.fixture-cache/budgets.json FMN_BROWSER_WORKERS=1 node scripts/browser-summary.mjs scripts/browser/editor-budgets.spec.mjs scripts/browser/side-by-side.spec.mjs"` with `FMN_BROWSER_GREP` selecting the budget cases, where `F` holds `f1-moby-dick.md`. Three runs at 10:43–10:59, load 6.9 falling to 2.3; the remote-update rows are from the first run only.

| Budget | Target | After: median of 3 runs [range] | Verdict |
| --- | --- | --- | --- |
| Keystroke to paint, F1 Visual (live preview, whole note), 1× | p95 < 16 ms | **16.9** [16.8–17.0] | missed by 0.9 ms |
| Keystroke to paint, F1 Visual, 4× | p95 < 50 ms (target < 30) | **19.7** [19.6–20.0]; work p95 11.0 [10.9–11.8] | met |
| Keystroke to paint, F1 Source, 1× | p95 < 16 ms | **16.8** [16.7–16.9] | missed by 0.8 ms |
| Keystroke to paint, F1 Source, 4× | p95 < 50 ms | **19.8** [19.5–19.8] | met |
| Keystroke to paint, F1 Side by side (pane open), 1× | p95 < 16 ms | **17.2** [17.0–17.4]; no long task | missed by 1.2 ms |
| Keystroke to paint, F1 Side by side, 4× | p95 < 50 ms (target < 30) | **32.9** [32.9–35.3]; no long task. When the 4× case runs alone (the alternating runs above), 22.5–23.2 | met; over the 30 ms target when it follows the 1× case |
| Frame floor (bare contenteditable), 1× / 4× | — | 14.6 [14.5–14.7] / 16.5 [16.2–17.2] | — |
| 53 KB note (F1's chapters 1–3), Visual, 1× / 4× | — | 16.5 / 19.4 (one run) | — |
| Opening F1 for editing (navigation to an editable editor), Visual / Source / Side by side, 1× | — | 0.9 / 1.0 / 0.8 s; at 4×: 4.8 / 4.8 / 4.8 s | — |
| Scrolling the pane through the whole of F1, 1× / 4× | no long task | 7.6 / 8.2 s, no long task; the editor scrolled back up, no long task | met |
| Another browser's edit shown, same gateway | p95 < 150 ms | **20** ms | met |
| An agent's patch shown | p95 < 150 ms | **24** ms | met |
| Typing while an agent patches every 500 ms: keystroke p95, 1× / 4× | no long task over 50 ms per update | **16.8 / 29.6**, no long task | met |

- **1×** stays above the 16 ms budget by about 1 ms in every mode, as before: the frame floor on this host was 14.6 ms in these runs, and a key's work at 1× (p95 2.5–3.6 ms) leaves its paint in the next frame.
- **Side by side at 4×** depends on what came before it in the run: 22.5–23.2 ms when the 4× case runs alone, 32.9–35.3 ms when it follows the 1× case in the same app, which first typed 600 keys into F1 (39.0 ms in the 10:28 run, before the second change). Not investigated further here.

**Bundle sizes** (the [command](#commands) below; gzip -9, each entry with what it imports beyond the app's own chunks). *Before* is `v2/livepreview` at `f34258ce` with Milkdown still built, *after* this branch at `72d8dcd4`, and *merged* is `storage-v2` after the livepreview merge and stream contextpanel's fixes, built at `6599751e` (2026-11-24):

| What | Before | After | Merged |
| --- | --- | --- | --- |
| Milkdown entry | 215.6 KiB | gone | gone |
| Milkdown plus the live engine (the bake-off's bundle row) | 242.9 KiB | gone | gone |
| Note editor (Visual, Source and Side by side, with the Markdown language) | 194.2 KiB | 194.1 KiB | 199.8 KiB |
| Live engine (collaboration) | 35.3 KiB (it loaded the bridge's chunk for `fromDelta`) | **27.5 KiB** | 27.5 KiB |
| Editor plus live engine: what opening a note for editing loads | 229.5 KiB | **221.5 KiB** (budget 237.2 KiB: met) | **227.3 KiB** (met) |
| Rendered pane and private preview, beyond the editor | 34.7 KiB | **27.6 KiB** (124.9 KiB with the editor's chunks, for a private draft's preview in Read) | 27.3 KiB (127.6 KiB with the editor's chunks) |
| Writing worker | 106.3 KiB | 106.3 KiB (Milkdown's remark) | 37.3 KiB (`mdast-util-from-markdown`, since stream lp-s3) |
| `style.css` / `milkdown.css` | 22.0 / 3.1 KiB | 21.5 KiB / gone | 21.5 KiB / gone |
| All JavaScript in `dist` | 724.8 KiB in 37 files | **529.6 KiB** in 31 files | 468.1 KiB in 31 files |

## The context panel while editing, and the editor's accessibility

- **Stream:** contextpanel (final-review fixes), branch `v2/contextpanel` from `storage-v2` at `c808ef3d`, 2026-11-24.
- **Typing with the context panel open (plan risk 18).** While the Contents and Comments panel was open, every typing pause sent the whole note to `/api/preview` and installed the answer in the hidden reading view, which Contents and Show passage read. On F1, the case below run on the app code of `c808ef3d` measured five uploads of 1.21 MB (1,211,998 to 1,212,022 bytes) for five pauses, and long tasks of 102, 106, 124, 102 and 148 ms at 1×; the final review measured 94–196 ms, and found that with the server's answer stubbed the uploads stayed and the long tasks went, so the tasks came from installing and walking the whole-note HTML. Editing now never renders the hidden preview; leaving edit mode renders it once, as it already did with the panel closed. While editing, Contents lists the headings of the editor's text (`internal/web/ui/note-outline.mjs`: one pass over the lines, about 1 ms on F1 in Node, median 0.95 ms and at most 1.29 ms over 30 runs, run once typing pauses) and puts the caret on the chosen heading, and Show passage asks the editor to select the passage, falling back to Read only when the editor has not placed it (the passage shows once Read has rendered the text edited; if that render fails, no later render scrolls to it). The budget spec's case *typing with the context panel open* (Visual, five words typed at the end of F1 1.5 s apart, 1×) asserts that no request reaches `/api/preview` and that no long task occurs: none of either on `bee77b63`, nor on `2a2fdb48` with the scan of the recheck fix.
- **What Contents shows while editing** is close to the reader's, not the same: ATX and setext headings outside front matter (as the server finds it), fenced and indented code and HTML blocks, without the leading level 1 title (the first element, so HTML before it, which the reader leaves out, does not count). Block quotes and list items end as goldmark ends them: a line indented less than the item's content, or unquoted, continues the paragraph they end with and ends them otherwise, or when it starts a heading, fence, thematic break, quote, list item or HTML block that may interrupt a paragraph; so a heading right after a list or a quote, with no blank line between them, is a section. HTML blocks are found as goldmark finds them (block tags interrupt a paragraph; a line of one other tag starts a block only outside one), and a table is no setext heading's text. Headings inside a block quote or a list are not listed (Read lists them), and a heading's text is its Markdown with links, code and emphasis marks dropped. The final recheck found the first version of the scan skipping every line after a list or quote up to a blank line, which dropped such headings (`# Notes\n- a\n## Todo\n- b\n` had no Contents while editing, so Context opened on Comments). A differential run of the scan against goldmark 1.8.6, configured as the server renders notes (GFM, top-level headings, HTML blocks rendering nothing), on 60,000 random documents of 2 to 10 lines drawn from block-start lines (headings at every indent, list items and quotes, nested and empty items, fences, thematic breaks, setext underlines, tables, HTML blocks) and on the unit test's 48 inputs, finds the same title and sections, level and text, in every one. Heading text can still differ from the reader's where the reader rewrites it (wiki links) or the scan's inline rules are simpler than goldmark's. The writing tools' outline (their worker's sections) was not used: it arrives from the worker on its own schedule, so its offsets can lag the text at the moment of a click, while the scan is cheap enough to run again at the click (about 1 ms on F1).
- **Open: a few one-shot whole-note previews while editing remain.** Outside the panel, some actions still render the whole note through `/api/preview` once while editing, as before this branch: opening a note with a recovered draft (`app.mjs`, `openNote`), reconciling a draft with the saved version, inserting a citation, a video or an upload (`media-insert.mjs`), and a save whose stored text differs from the editor's. On F1 each is one 1.2 MB upload and render, not one per pause; they could set `previewStale` instead, as typing does.
- **Axe on the editor (plan risk 23).** `accessibility.spec.mjs` runs axe (WCAG 2.0 and 2.1, A and AA) over `#editing` in Visual, Source and Side by side, in the light, dark and parchment themes, at 1440 × 900 on a note whose Source scroller overflows in Side by side. Visual and Source have no violation. Side by side has one in every theme, `scrollable-region-focusable` (serious) on Source's `.cm-scroller`: axe-core 4.13.0 does not count a contenteditable as focusable (its `isNativelyFocusable` has no such case), and Tab does reach the editable `.cm-content` inside the scroller. The test leaves out that rule on that node only.
- **Open: a read-only Side by side cannot focus the Markdown.** The final review's keyboard probe found that when a note turns read-only during an edit (a lock, or a refused live update), `.cm-content` is no longer editable and the scroller holds nothing focusable, so Tab skips the Markdown column and lands on the rendered pane; the pane's scrolling still moves the Markdown with it. (A note that is read-only when opened never shows the editor.) Follow-up outside this stream: give `.cm-content` `tabindex="0"` through `EditorView.contentAttributes` in `frontend/source-editor.mjs`, which would also let the test drop its exception.
- **Open: the manual phone pass of plan risk 23** was not done; it needs a person with a phone.
- **Open: no comment highlights in the Side by side pane.** Design item 6 of Side by side (highlights in the pane through the CSS Highlight API) was not built: `frontend/rendered-preview.mjs` draws none, and the highlights show on the Markdown side only. Pending the maintainer's decision.

## Missed budgets

### Typing in the manuscript, and remote updates while typing

*Resolved at 4× by stream livewire, which made the changes proposed below ([Browser budgets after the incremental live wire](#browser-budgets-after-the-incremental-live-wire)); 1× stays within 2 ms of the frame floor. Stream livepolish took the remaining whole-note copies and the status line's layout off the key press ([Browser budgets and typing load after the live follow-ups](#browser-budgets-and-typing-load-after-the-live-follow-ups)): 4× fell to 19–20 ms, and 1× is still missed by 1–2 ms. The analysis is of `storage-v2` before it.*

Keystroke to paint, p95 in ms (median of 3 runs [range]), typing 500 keys in the middle of the note:

| Where | 1× | 4× | Long tasks while typing (1× / 4×) | Opened, navigation to editable (1× / 4×) |
| --- | --- | --- | --- | --- |
| A bare contenteditable (the frame floor) | 15.4 [14.7–15.6] | 17.3 [17.2–17.6] | 0 / 0 | — |
| F1, Visual section mode, middle chapter | 18.7 [18.1–19.4] | 113.1 [104.0–117.5] | 3, up to 105 ms / 71, up to 476 ms | 1.2 s / 6.2 s |
| F1, Source (CodeMirror 6), middle of the note | 18.7 [18.4–18.9] | 96.5 [93.2–99.4] | 0 / 34, up to 460 ms | 1.8 s / 6.4 s |
| 53 KB note (F1's chapters 1–3), Visual section mode | 16.8 [16.8–16.8] | 32.5 [29.5–35.1] | 0 / 0 | 0.33 s / 1.0 s |

Typing 200 keys while an agent patches the shown chapter every 500 ms: keystroke p95 138 ms at 1× and 794 ms at 4×, with 70 and 551 long tasks.

At 1× every editor is within 3.5 ms of the frame floor, which itself is just under the 16 ms budget (the bake-off found the same floor). At 4× the 53 KB note meets the budget and F1 misses it about twice over, with long tasks of up to half a second: the cost follows the size of the note, not the edit. A CPU profile of typing 200 keys in F1's Visual mode at 4× (Chrome's sampling profiler over CDP, 17.9 s) points at work on the whole note outside the editor:

1. **The draft record copies the whole note into `localStorage`.** Its scheduled write (`DraftStorage.publish` and `localStorage.setItem`, `internal/web/ui/draft-storage.mjs`) took 2.3 s, as much as ProseMirror's handling of the 200 keystrokes themselves (2.2 s, the bridge's re-parse of each edited chunk included).
2. **The live wire carries the whole note.** Every answer of `/api/live`, to a poll that found a change and to each of the browser's own sends, is the note's whole Markdown and whole Yjs state (`LiveDocument.Content` and `Update`, filled from the whole record in `liveOutput`, `internal/workspace/db_live.go`): 2.8 MB for F1, 124 KB for the 53 KB note. The browser parses each one and applies the whole state again (`receive` in `frontend/collaboration.mjs`, 0.5 s in the profile, besides the parsing). With an agent patching every 500 ms, these answers arrive several times a second, and long tasks follow (551 at 4×). The edits themselves are small: the server applies a typed key in 4.9 ms and an agent's patch in about 13 ms (`TestLiveNovelStaysFast`).

Both are outside this stream's scope (the web handler, `db_live.go` and the frontend). What would fix them: live answers that carry only the update since the client's state vector (`EncodeSince`, which the engine already has) and the content only when the client asks for it; a draft record that is written off the main thread or holds only what the server lacks.

### Remote edits from another browser

*Resolved by stream livewire: 65 ms at p95 ([Browser budgets after the incremental live wire](#browser-budgets-after-the-incremental-live-wire)).*

An edit typed in one browser reaches another in 457 ms at p95. The sending browser waits 250 ms after the last change before it sends (`schedule` in `frontend/collaboration.mjs`), so an edit followed by a pause cannot arrive in under 250 ms; the rest is the 2.8 MB answers on both sides. An agent's patch has no send delay and arrives in 139 ms at the median of p95s (one run of three at 158 ms), close to the budget for the same reason. Meeting the budget for typed edits needs a shorter send schedule, which the incremental answers above would make affordable.

### Export of one revision on a slow disk

The exporter writes each new revision's objects (for an edit of a note one folder deep, four: the commit, two trees and the blob) as loose objects, and each waits for two fsyncs, the file's and its directory's. That took 6.4 ms at p95 on the quieter disk and 0.09–2.8 s once fsyncs slowed (see [Variance](#variance)); on the final code, later in the day, 6.2, 27.5 and 40.2 ms in three runs. The export runs in the background after each commit, never on a request path, so this delays only the backup. Not changed here: one pack per revision would start a Git process per commit, and the choice belongs with the exporter's design.

### Bundle size (should)

*Met since the live preview (stream lp-s5): 221.5 KiB on its branch, 227.3 KiB on `storage-v2` at `6599751e` ([Browser budgets on the live preview](#browser-budgets-on-the-live-preview)).*

The Visual editor and the live engine loaded 242.4 KiB gzipped (the Milkdown chunk, the collaboration chunk and the chunks they import beyond the app's own), 5.2 KiB over the 237.2 KiB of the Milkdown and Yjs chunks before storage v2. The editor bake-off predicted this (244.1 KiB). Source mode added 93.3 KiB, mostly CodeMirror, which the read-only source viewer already shipped.

### Agent patch from a day-old base (should)

Y, chosen in D1, patches only from bases it keeps: 15 minutes and 16 MiB of live versions (`retainLiveVersions`). The [engine results](storage-v2-results-engine.md) recorded this; nothing changed.

## Found while measuring

- **A large bundle import can fail as "damaged".** Two runs of the export benchmark (06:51 and 08:01, both with the first pack version's undeltified exports) failed to import F2's bundle with `cannot import notebook bundle: damaged or incomplete Git history`. The export had passed `git fsck --strict` just before. A diagnostic run on the same kind of export (08:03) imported its 952 MB bundle: `fetch` took 28 s and `fsck --strict` 8 s, under the 90 s that `checkBundle` (`internal/gitexport/bundle.go`, `checkTimeout`) gives all its Git steps together, copying the upload included. Any failure of the `fetch`, a timeout included, gets that message, so a timeout on the slowed disk is the likely cause; it was not confirmed. The limit predates storage v2, and a notebook a little larger than F2 would meet it on a quiet disk too. Changed since: `1acec91` gave the check 90 s plus one second per 2 MiB of the bundle, counted once the upload is stored (about 9 minutes for that bundle), and reports a step that runs out of the check's time or the caller's as a timeout (`ErrBundleTimeout`), never as damage. That did not yet give a browser or MCP import that time: the import as a whole kept its 90 s, the workspace import's own limit and, in the browser, the request's, which began before the upload arrived. Since `f291db5` the import as a whole has 90 s plus one second per MiB, counted once the upload is read (about 17 minutes for that bundle); the browser's upload is no longer held to the server's 60 s read timeout, nor the import's answer to its 120 s write timeout; and an import that runs out of time answers 503 `timeout`, which MCP marks retryable. A bundle that size still needs the operator to raise `FMN_LIMIT_BUNDLE_BYTES` (32 MiB by default) and, for a history that size, the import object limits, as well as the reverse proxy's body size and read timeout ([trusted proxy](../trusted-proxy.md)).
- **Notes in Trash block folder moves.** A move rewrites the links to the moved notes and refuses when one of the linking notes is locked or in Trash (`updating links would edit locked note Trash/…; unlock it first`). In F2 every folder is linked from such a note, so the move benchmark drops F2's locks and moves the folder nearest 50 notes that no note in Trash links to.
- **Search reads and parses every note on each call.** The F2 find figures first published here (10.0 / 11.0 / 7.2 ms) measured `Find`, which filters the cached listing; the searches people and agents run (`find_notes`, the notes API's `q=` and the browser's search through it) call `FindScoped`, which reads every note of the head and, for each call, renders each note's searchable text and each match's title and excerpt with the Markdown parser. On F2 that takes 0.85–0.88 s at p50 (p95 0.99–1.03 s; 0.47 / 0.51 s for a tag), 74–77 ms on the 2,000-revision F2; the same code runs on `next`. A CPU profile of the common word on the small F2, which every note matches, puts 55 % of its time in `NoteSummary` (each match's title and excerpt) and 44 % in `SearchableText`, both `goldmark` parses. Follow-up: keep each note's folded searchable text and summary by blob ID (both depend on the content alone), so a warm search compares cached strings, or answer it from the search index.
- **Full-history listing without statistics.** `BenchmarkHistoryOf5000Revisions` lists all 5,000 revisions of one note in 49–58 ms at p95 before `ANALYZE`, at the budget. Pages of 50, what the browser and MCP ask for, take 4 ms, and the full F1h listing with statistics 24 ms.

## Method

- **Machine.** The development server: 12th Gen Intel Core i7-12700K (20 threads), 46 GB, Linux 6.1.0-56. Other workloads and agents' test runs shared it: the 1-minute load average was between 5.5 and 15 during the measurements (each log records it every 15 s). Every run was a container limited to 4 CPUs (`fmn-go`, `fmn-node`).
- **Images.** Go 1.27.4, Postgres 17.12 and Git 2.54.1 in `fmn-dev-go:local`; Node 24.20.0, Playwright 1.63.0 (Chromium) and Postgres 16.15 in `fmn-dev-browser:local`.
- **Postgres.** The Go benchmarks use `internal/pg/pgtest` clusters, which run with `fsync=off`, `synchronous_commit=off` and `full_page_writes=off`. Their write latencies leave out the WAL flush a production commit waits for. The idle-load and browser runs use the browser fixture's cluster, which has the same settings. The budgets review reran the checkpoint budget with all three on, as in production; that rerun is in the results and the [raw results](#raw-results).
- **Fixtures** (`internal/benchfixture`, default seeds and sizes):
  - F1: the converted *Moby-Dick*, 1,206,841 bytes, SHA-256 `15bf4dd6…`;
  - F1h: F1 plus 5,000 edits by three people and two agents, 250 of them renames;
  - F2: 2,000 notes, 200 images of 1–5 MiB, comments, locks and Trash over 20,000 revisions.

  The workspace benchmarks replay F1h and F2 into a notebook revision by revision (`replayFixture`), with the fixture's authors, times, messages, renames and the `fmn-id` entry identities, then run `ANALYZE` as production's autovacuum would. The export benchmark imports F2 from Git.
- **Repetition.** Each benchmark ran with `-count=3`, except at the end of the session: the export benchmark's final version ran twice and the purge test on it once. The workspace benchmarks replay their fixture once per process, and each sub-benchmark then runs 3 times in a row on it; the export benchmark and the purge test redo everything 3 times. Latency sub-benchmarks take 50 iterations each (`-benchtime=50x`) and report the p50, p95 and maximum of those 50. The browser spec ran 3 times, each with its own app; keystroke figures are percentiles of 500 keystrokes, remote figures of 30 edits.
- **What each server figure covers.** Workspace sub-benchmarks call the `*workspace.Store` method a request calls (`Read`, `Save`, `LiveRead`, `LiveApply`, `LiveCheckpoint`, `List`, `Find`, `History`, `ChangesSince`, `Move`, …), so they include its checks, locks and the notes store, but not HTTP. "After a restart" opens a new notes store and workspace store on the same database and blob directory before each iteration. Storage growth sums `pg_column_size` of the org's rows in the revision tables and the blob directory's bytes.
- **Idle load.** A gateway built from the branch serves a notebook of 50 notes, from the browser fixture (`startApp`), for one minute and then three 10-minute windows; the figure is the change in `xact_commit + xact_rollback` of its database in `pg_stat_database`, per second. The tab variant keeps one signed-in Chromium tab open on a note with live editing connected.
- **Browser.** `scripts/browser/editor-budgets.spec.mjs` in `fmn-dev-browser:local`, Playwright Chromium, viewport 1280×900, 4× throttle through CDP `Emulation.setCPUThrottlingRate`. A fresh app (the browser fixture's `startApp`) holds F1 as `Manuscripts/Moby-Dick.md` and F1's chapters 1–3 (`scripts/browser/fixtures/f1-slice.md`, 53 KB). Keystroke to paint follows the editor bake-off: keydown `timeStamp` to the first task after the next animation frame; one key at a time, each waiting for its frame and at least 30 ms, every 20th a Backspace. The caret is at the end of the middle paragraph of the middle section in Visual, and at the end of the middle line in Source. Remote figures: an inserted marker word, from the writer's `insertText` (or the agent's `POST /api/live/patch`) to the moment the reader's shown section contains it (a `MutationObserver`), 30 times each with a 1 s pause. The live answers' sizes are the bodies of every `/api/live` response the page received. The CPU profile came from a scratch script, not committed, that opens F1 as the spec does and records CDP `Profiler` samples (200 µs interval) while typing 200 keys 60 ms apart. From stream livewire on, the spec turns Playwright's trace off; the profile of that stream typed 200 keys as the spec does at 4×, sampling every 200 µs.

### Variance

The server ran other workloads and agents' runs throughout; the 1-minute load average stayed between 5.5 and 15. From about 05:40 the disk's fsync latency rose sharply, and code paths this branch does not change slowed down several times over:

| Unchanged operation | 04:30–05:30 | 06:03–08:42 |
| --- | --- | --- |
| F2 Git import | 74–94 s | 145–194 s |
| F2 verify | 12.2–12.8 s | 11.3–27.5 s |
| Export of one revision as loose objects, p95 | 6.4–12.5 ms | 0.09–2.8 s |
| Import of the 2,000-revision F2 | 4.2–4.3 s | 31–57 s (06:47) |
| Purge planning (CPU and database) | 3.7–4.5 s | 4.0–10.5 s |

Production's own gateway was using about 1.25 CPUs at 06:56, and iowait was 7%. Where a later run repeated a figure on the slowed disk, the results above use the earlier runs and say so; the raw tables keep every run.

## Commands

From the host, with `B=~/forgetmenote-design/bin` and `WT=~/src/fmn-wt/budgets`. The F1 benchmarks need F1 in a fixture cache: here the ignored `.fixture-cache` directory of the worktree holds `f1-moby-dick.md` and `raw/pg2701.txt`.

```sh
# F1 and F1h through the workspace; "before the snapshot fix" is the same run
# with internal/pg/migrations/0019_live_snapshot_storage.sql moved out of the tree
$B/fmn-go $WT sh -c 'FMN_FIXTURE_CACHE=$PWD/.fixture-cache go test ./internal/workspace -run "^$" -bench BudgetManuscript -benchtime=50x -count=3 -timeout 120m -v'
# F2 through the workspace
$B/fmn-go $WT sh -c 'go test ./internal/workspace -run "^$" -bench BudgetNotebook -benchtime=50x -count=3 -timeout 180m -v'
# Find and FindScoped alone on F2 (WT=~/src/fmn-wt/ffops; add FMN_BUDGET_F2=small for the small F2)
$B/fmn-go $WT sh -c 'go test ./internal/workspace -run "^$" -bench "^BenchmarkBudgetNotebook$/^find" -benchtime=50x -count=3 -timeout 180m -v'
# The notes store's own benchmarks (F1 from FMN_FIXTURES)
$B/fmn-go $WT sh -c 'FMN_FIXTURES=$PWD/.fixture-cache go test ./internal/notestore -run "^$" -bench . -benchtime=50x -count=3 -timeout 120m'
# F2 import, export, rebuild and bundle round trip (and the one-revision export on the final code, with WT=~/src/fmn-wt/exportops)
$B/fmn-go $WT sh -c 'go test ./internal/gitexport -run "^$" -bench BudgetExportF2 -benchtime=1x -count=3 -timeout 180m -v'
# The A/B: a test binary per version, built with the worktree at that commit, run alternately
$B/fmn-go $WT sh -c 'go test -c -o .fixture-cache/prof/gitexport-loose.test ./internal/gitexport'
$B/fmn-go $WT sh -c 'cd internal/gitexport && ../../.fixture-cache/prof/gitexport-loose.test -test.run "^$" -test.bench BudgetExportF2 -test.benchtime 1x -test.count 1 -test.timeout 60m -test.v'
# Purge completeness and time on the full F2
$B/fmn-go $WT sh -c 'FMN_PURGE_F2=full go test ./internal/workspace -run "^TestPurgeLeavesNoStoredCopyOfARedactedSecret$" -count=3 -timeout 180m -v'
# Browser budgets (writes the JSON results to the file FMN_BROWSER_BUDGETS names)
$B/fmn-node $WT sh -c 'FMN_FIXTURE_CACHE=$PWD/.fixture-cache FMN_BROWSER_BUDGETS=$PWD/.fixture-cache/browser-run1.json FMN_BROWSER_RESULTS=/tmp/r FMN_BROWSER_REPORT=/tmp/p.json node scripts/browser-summary.mjs scripts/browser/editor-budgets.spec.mjs'
# Toolbar commands property test
$B/fmn-node $WT node --test internal/web/ui/markdown-commands.test.mjs
# Static builds
$B/fmn-go $WT sh -c 'for a in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags="-s -w" -o /tmp/fmn-$a ./cmd/forgetmenote && file /tmp/fmn-$a; done'
```

`FMN_BUDGET_F2=small` runs the F2 benchmarks on a 2,000-revision F2 in seconds, for checking a change before the full run.

The idle load was measured with this script, run as `FMN_DOCKER_ARGS="-v $DIR:$DIR:ro" $B/fmn-node $WT node $DIR/idle-load.mjs $WT 10 3 [tab]`:

```js
import {execFileSync} from 'node:child_process';
import {setTimeout as delay} from 'node:timers/promises';

const [worktree, minutes = '10', windows = '3', tab] = process.argv.slice(2);
const {startApp} = await import(worktree + '/scripts/browser/app-fixture.mjs');
const {startPostgres} = await import(worktree + '/scripts/browser/postgres.mjs');
const cluster = await startPostgres();
process.env.FMN_BROWSER_DATABASE_HOST = cluster.host;
const query = statement =>
  execFileSync('psql', ['-X', '-At', '-h', cluster.host, '-U', 'fmn', '-d', 'postgres', '-c', statement], {encoding: 'utf8'}).trim();
const app = await startApp({name: 'fmn-idle-', account: {username: 'idle-admin', password: 'DisposableIdle123!'}});
let browser;
try {
  const database = query(`SELECT datname FROM pg_database WHERE datname LIKE 'app\\_%' AND datname NOT LIKE '%baseline'`);
  for (let i = 0; i < 50; i++)
    await app.api('/p/default/api/note', {path: `Idle/note ${i}.md`, content: `# Note ${i}\n\nSome text for note ${i}.\n`, revision: ''});
  if (tab) {
    const {chromium} = await import(worktree + '/node_modules/@playwright/test/index.mjs');
    browser = await chromium.launch();
    const page = await browser.newPage();
    await app.login(page, 'recovery');
    await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent('Idle/note 1.md'));
    await page.locator('#document[data-live="connected"]').waitFor({timeout: 30000});
  }
  await delay(60000);
  const count = () => Number(query(`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = '${database}'`));
  for (let w = 0; w < Number(windows); w++) {
    const before = count(), started = Date.now();
    await delay(Number(minutes) * 60000);
    const after = count(), seconds = (Date.now() - started) / 1000;
    console.log(JSON.stringify({window: w + 1, tab: Boolean(tab), seconds: Math.round(seconds), transactions: after - before, perSecond: Math.round(((after - before) / seconds) * 100) / 100}));
  }
} finally {
  await browser?.close();
  await app.close();
  await cluster.stop();
}
```

The bundle sizes are each entry's static imports beyond the app chunk's own, each file gzipped at level 9 (`gzip -9`), from `internal/web/ui/dist` after `npm run build`. This script prints them, run as `$B/fmn-node $WT npm run build`, then `$B/fmn-node $WT node $WT/.fixture-cache/bundle-size.mjs $WT/internal/web/ui/dist` (or a copy of an earlier build's `dist`). On `storage-v2` at `1a7e51e` its predecessor printed 242.4 KiB (248,257 bytes: the Milkdown entry, the collaboration entry and two chunks they import) for the Visual editor and live engine, and 93.3 KiB (95,519 bytes) for Source mode; the figures of the live preview are in [Browser budgets on the live preview](#browser-budgets-on-the-live-preview):

```js
// Each entry's static imports beyond the app chunk's own, each file gzipped
// at level 9. Usage: node bundle-size.mjs DIST
import {execFileSync} from 'node:child_process';
import {readFileSync} from 'node:fs';

const dist = process.argv[2];
const {aliases, files} = JSON.parse(readFileSync(dist + '/manifest.json', 'utf8'));
const path = url => dist + url.slice('/static/dist'.length);
// An entry and every chunk it imports statically, transitively.
const loads = (url, seen = new Set()) => {
  if (!seen.has(url)) {
    seen.add(url);
    for (const [, chunk] of readFileSync(path(url), 'utf8').matchAll(/(?:from|import)\s*"(\/static\/dist\/[^"]+\.js)"/g)) loads(chunk, seen);
  }
  return seen;
};
const gz = url => execFileSync('gzip', ['-9', '-c', path(url)]).length;
const kib = bytes => `${(bytes / 1024).toFixed(1)} KiB gzipped (${bytes} bytes)`;
const app = loads(aliases['/static/app.mjs']);
const beyond = (entries, also = new Set()) => {
  if (entries.some(entry => !aliases[`/static/dist/${entry}.js`])) return null;
  return [...new Set(entries.flatMap(entry => [...loads(aliases[`/static/dist/${entry}.js`])]))].filter(url => !app.has(url) && !also.has(url));
};
for (const [name, entries] of [
  ['Milkdown entry', ['milkdown']],
  ['Milkdown plus live engine', ['milkdown', 'collaboration']],
  ['Note editor (Visual, Source, Side by side)', ['source-editor']],
  ['Live engine (collaboration)', ['collaboration']],
  ['Editor plus live engine', ['source-editor', 'collaboration']],
  ['Rendered pane and private preview', ['rendered-preview']],
]) {
  const loaded = beyond(entries);
  console.log(`${name}: ${loaded ? kib(loaded.reduce((sum, url) => sum + gz(url), 0)) : 'not built'}`);
}
if (aliases['/static/dist/rendered-preview.js']) {
  const pane = beyond(['rendered-preview'], loads(aliases['/static/dist/source-editor.js']));
  console.log(`Rendered pane beyond the editor: ${kib(pane.reduce((sum, url) => sum + gz(url), 0))}`);
}
console.log(`Writing worker: ${kib(gz(aliases['/static/dist/writing-worker.js']))}`);
for (const css of ['/static/style.css', '/static/dist/milkdown.css'])
  console.log(`${css}: ${aliases[css] ? kib(gz(aliases[css])) : 'not built'}`);
const scripts = files.filter(url => url.endsWith('.js'));
console.log(`All JavaScript in dist: ${kib(scripts.reduce((sum, url) => sum + gz(url), 0))} in ${scripts.length} files`);
```

## Raw results

Every run, in order (each cell lists the runs). Times of day are the host's (MST) on 2026-11-23.

**F1 after F1h through the workspace, before the snapshot fix (04:24)**

| Benchmark | p50-ms | p95-ms | p95-bytes |
| --- | --- | --- | --- |
| `BudgetManuscript/open/warm` | 0.26, 0.22, 0.22 | 0.41, 0.37, 0.35 |  |
| `BudgetManuscript/open/cold` | 5.76, 5.86, 5.93 | 7.01, 7.00, 7.41 |  |
| `BudgetManuscript/history/page` | 1.88, 1.90, 1.94 | 2.65, 2.39, 2.34 |  |
| `BudgetManuscript/history/all` | 24.1, 19.8, 19.8 | 25.8, 20.5, 22.7 |  |
| `BudgetManuscript/save/paragraph` | 4.72, 4.63, 4.86 | 5.77, 5.70, 6.06 | 7253, 6481, 7533 |
| `BudgetManuscript/live/start` | 37.2, 35.5, 35.5 | 42.5, 40.0, 38.1 |  |
| `BudgetManuscript/live/load` | 12.8, 12.9, 13.6 | 15.2, 14.7, 15.2 |  |
| `BudgetManuscript/live/keystroke` | 4.35, 2.88, 2.96 | 5.32, 3.32, 4.33 |  |
| `BudgetManuscript/live/checkpoint` | 46.0, 42.5, 42.2 | 51.2, 46.0, 44.6 | 7478, 7306, 8075 |

**F1 after F1h through the workspace, after the snapshot fix (04:26)**

| Benchmark | p50-ms | p95-ms | p95-bytes |
| --- | --- | --- | --- |
| `BudgetManuscript/open/warm` | 0.23, 0.22, 0.26 | 0.47, 0.39, 0.43 |  |
| `BudgetManuscript/open/cold` | 5.76, 6.04, 5.64 | 7.14, 7.08, 7.01 |  |
| `BudgetManuscript/history/page` | 2.12, 2.18, 1.99 | 2.79, 3.34, 2.53 |  |
| `BudgetManuscript/history/all` | 24.3, 19.7, 19.8 | 24.8, 22.2, 24.4 |  |
| `BudgetManuscript/save/paragraph` | 4.63, 4.78, 4.56 | 5.39, 5.77, 5.22 | 7253, 6481, 7533 |
| `BudgetManuscript/live/start` | 13.8, 13.6, 13.2 | 15.9, 16.9, 16.2 |  |
| `BudgetManuscript/live/load` | 7.94, 8.41, 8.81 | 9.01, 9.17, 9.74 |  |
| `BudgetManuscript/live/keystroke` | 4.04, 2.90, 3.00 | 4.93, 3.13, 5.18 |  |
| `BudgetManuscript/live/checkpoint` | 17.9, 15.0, 15.2 | 22.8, 18.9, 18.6 | 7478, 7306, 8075 |

**Checkpoint of one paragraph with production durability** (the budgets review's rerun with `fsync`, `synchronous_commit` and `full_page_writes` on; p95 in ms)

| Code | Run 1 | Run 2 | Run 3 | Growth |
| --- | --- | --- | --- | --- |
| With `0019` | 24.3 | 20.6 | 22.5 | 7.3 KB |
| Without `0019` | 51.1 | 47.6 | 47.3 | |

**F2 through the workspace (05:11–05:16)**

| Benchmark | p50-ms | p95-ms |
| --- | --- | --- |
| `BudgetNotebook/status/cold` | 0.0015, 0.0015, 0.0015 | 0.0031, 0.0022, 0.0020 |
| `BudgetNotebook/list/cold` | 238, 220, 216 | 295, 359, 382 |
| `BudgetNotebook/find/cold` | 225, 238, 233 | 411, 420, 441 |
| `BudgetNotebook/status/warm` | <0.001, <0.001, <0.001 | <0.001, <0.001, 0.0011 |
| `BudgetNotebook/list/warm` | 1.47, 1.53, 1.44 | 1.80, 1.84, 1.82 |
| `BudgetNotebook/browse/warm` | 1.78, 1.81, 1.93 | 2.11, 2.17, 2.27 |
| `BudgetNotebook/collections/warm` | 1.57, 1.54, 1.46 | 1.96, 1.86, 1.83 |
| `BudgetNotebook/find/rare` | 7.97, 10.1, 7.82 | 9.72, 15.0, 9.96 |
| `BudgetNotebook/find/common` | 8.15, 7.37, 7.40 | 11.6, 11.0, 8.84 |
| `BudgetNotebook/find/tag` | 6.61, 6.68, 6.87 | 7.09, 7.22, 7.46 |
| `BudgetNotebook/read/note` | 0.0017, 0.0026, 0.0016 | 0.0026, 0.0037, 0.0020 |
| `BudgetNotebook/history/note` | 1.34, 1.39, 1.40 | 1.89, 1.81, 1.87 |
| `BudgetNotebook/history/note-all` | 0.44, 0.44, 0.43 | 0.59, 0.70, 0.76 |
| `BudgetNotebook/history/notebook` | 0.38, 0.36, 0.37 | 0.73, 0.56, 0.56 |
| `BudgetNotebook/changes-since/1000` | 9.18, 9.16, 9.30 | 18.6, 10.6, 10.1 |
| `BudgetNotebook/save/note` | 2.93, 3.10, 3.00 | 5.60, 4.61, 3.97 |
| `BudgetNotebook/move/folder` | 708, 692, 680 | 805, 732, 718 |

**F2 Find and FindScoped** (`v2/ffops`, 2026-11-23 18:07–18:20, the 1-minute load average 8.5 at the start and 25.2 at the end; the replay took 1 min 42 s)

| Benchmark | p50-ms | p95-ms |
| --- | --- | --- |
| `BudgetNotebook/find/cold` | 311, 252, 257 | 342, 396, 416 |
| `BudgetNotebook/find-scoped/cold` | 1161, 1732, 1555 | 1472, 4843, 4156 |
| `BudgetNotebook/find/rare` | 10.3, 10.8, 10.9 | 13.6, 14.3, 13.7 |
| `BudgetNotebook/find/common` | 11.4, 9.99, 10.9 | 13.4, 15.5, 17.7 |
| `BudgetNotebook/find/tag` | 10.0, 7.33, 7.72 | 15.1, 12.5, 12.6 |
| `BudgetNotebook/find-scoped/rare` | 888, 854, 761 | 1072, 986, 937 |
| `BudgetNotebook/find-scoped/common` | 782, 878, 892 | 928, 1084, 1031 |
| `BudgetNotebook/find-scoped/tag` | 462, 473, 474 | 505, 527, 508 |

**The 2,000-revision F2, Find and FindScoped** (`FMN_BUDGET_F2=small`, 18:20–18:22, load average 22.5)

| Benchmark | p50-ms | p95-ms |
| --- | --- | --- |
| `BudgetNotebook/find/cold` | 35.2, 35.6, 36.7 | 45.9, 57.7, 54.2 |
| `BudgetNotebook/find-scoped/cold` | 129, 139, 115 | 149, 169, 142 |
| `BudgetNotebook/find/rare` | 1.18, 1.18, 0.891 | 1.37, 1.37, 1.23 |
| `BudgetNotebook/find/common` | 1.15, 1.05, 1.15 | 1.31, 1.20, 1.65 |
| `BudgetNotebook/find/tag` | 0.613, 0.398, 0.453 | 1.05, 0.768, 0.697 |
| `BudgetNotebook/find-scoped/rare` | 79.1, 74.4, 72.4 | 94.2, 87.9, 84.4 |
| `BudgetNotebook/find-scoped/common` | 77.4, 76.9, 77.2 | 95.6, 94.2, 93.3 |
| `BudgetNotebook/find-scoped/tag` | 40.1, 39.2, 39.2 | 44.6, 43.5, 45.4 |

**The notes store's own benchmarks (04:29; synthetic histories and listings, F1 for the manuscript)**

| Benchmark | p50-ms | p95-ms | p95-bytes |
| --- | --- | --- | --- |
| `ApplyParagraphEditOfManuscript` | 3.62, 3.67, 4.02 | 4.65, 4.26, 4.87 | 7615, 7615, 7615 |
| `ReadManuscript/warm` | 0.19, 0.17, 0.15 | 0.54, 0.34, 0.37 |  |
| `ReadManuscript/cold` | 8.02, 8.01, 8.03 | 12.4, 11.9, 12.8 |  |
| `HistoryOf5000Revisions/limit=50` | 2.69, 2.55, 2.44 | 4.13, 4.03, 4.30 |  |
| `HistoryOf5000Revisions/limit=0` | 42.8, 43.9, 40.0 | 57.5, 54.6, 48.8 |  |
| `ListingOf2000Files/files-warm` | 0.12, 0.0631, 0.0479 | 0.24, 0.17, 0.21 |  |
| `ListingOf2000Files/head-warm` | <0.001, <0.001, <0.001 | <0.001, <0.001, <0.001 |  |
| `ListingOf2000Files/files-cold` | 3.46, 3.31, 3.16 | 4.82, 4.74, 3.81 |  |
| `ListingOf2000Files/small-edit` | 1.28, 1.34, 1.23 | 2.18, 1.89, 2.07 |  |

**F2 export, all loose objects (04:30–04:51; seconds, the one-revision export in ms)**

| Benchmark | import-s | verify-s | export-s | maintain-s | one-p95-ms | bundle-s | bundle-import-s |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `BudgetExportF2` | 94.0, 74.2, 90.9 | 12.8, 12.2, 12.2 | 53.2, 52.0, 80.7 | 40.1, 39.6, 39.5 | 6.43, 6.37, 12.5 | 2.14, 2.07, 2.02 | 68.8, 47.7, 50.0 |

**F2 export, the rebuild as one pack, no deltas (05:31–06:03)**

| Benchmark | import-s | verify-s | export-s | maintain-s | one-p95-ms | purge-rewrite-s | purge-rebuild-s | bundle-import-s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `BudgetExportF2` | 94.7, 88.7, 120 | 12.7, 13.3, 13.2 | 50.2, 88.1, 134 | 40.2, 43.5, 42.4 | 6.46, 93.8, 39.5 | 4.95, 5.18, 5.30 | 38.1, 115, 53.2 | 89.8, 105, 75.4 |

**F2 export, the first pack version: the rebuild and exports of 100 revisions or more as one pack, no deltas (06:25–06:51, slowed disk; 2 runs, the third failed its bundle import)**

| Benchmark | import-s | verify-s | export-s | maintain-s | one-p95-ms | purge-rewrite-s | purge-rebuild-s | bundle-import-s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `BudgetExportF2` | 152, 174 | 26.7, 26.4 | 37.0, 45.3 | 8.32, 8.21 | 561, 2045 | 4.50, 4.82 | 76.8, 79.7 | 97.4, 91.7 |

**F2 export, A/B on the slowed disk and the final version** (seconds; the one-revision export in ms)

| Run | import-s | verify-s | export-s | maintain-s | one-p95-ms | purge-rewrite-s | purge-rebuild-s | bundle-import-s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Loose objects (07:13) | 159 | 27.5 | 157 | 39.7 | 90.3 | 4.59 | 187 | 88.8 |
| First pack version (07:28) | 177 | 27.1 | 51.6 | 8.18 | 2767 | 4.38 | 71.4 | 102 |
| Loose objects (07:38) | 194 | 24.8 | 156 | 42.2 | 158 | 4.55 | 192 | 89.1 |
| Pack with the delta repack, run 1 (08:24–08:42) | 184 | 25.2 | 69.9 | 5.31 | 1102 | 4.59 | 73.9 | 93.1 |
| Pack with the delta repack, run 2 (08:24–08:42) | 145 | 11.3 | 69.4 | 5.49 | 808 | 5.92 | 68.9 | 57.4 |
| First pack version (07:54) | stopped at the bundle import | | | | | | | |

**F2 export on the final code** (`v2/exportops` at `82529a8`, 09:49–10:12, the 1-minute load average 8.9 at the start and 15.4 at the end; seconds, the one-revision export in ms)

| Run | import-s | verify-s | export-s | maintain-s | one-p50-ms | one-p95-ms | purge-rewrite-s | purge-rebuild-s | bundle-s | bundle-import-s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 94.6 | 14.0 | 34.7 | 5.47 | 4.53 | 6.18 | 5.29 | 41.5 | 1.38 | 72.3 |
| 2 | 102 | 19.2 | 50.4 | 5.52 | 4.83 | 27.5 | 5.22 | 73.0 | 1.43 | 95.1 |
| 3 | 129 | 14.2 | 63.8 | 5.51 | 15.5 | 40.2 | 6.30 | 78.2 | 1.41 | 83.6 |

**Purge of the full F2** (`TestPurgeLeavesNoStoredCopyOfARedactedSecret`, seconds; every run passed)

| Code | When | Replay | Plan | Execute | Export rebuild | Plan + execute + rebuild |
| --- | --- | --- | --- | --- | --- | --- |
| Loose objects | 04:51–05:10, run 1 | 103 | 4.5 | 8.0 | 150.0 | 162.5 |
| Loose objects | 04:51–05:10, run 2 | 63 | 3.9 | 8.0 | 71.3 | 83.2 |
| Loose objects | 04:51–05:10, run 3 | 71 | 3.7 | 8.2 | 72.5 | 84.4 |
| Rebuild as one pack, no deltas | 06:03–06:25, run 1 | 142 | 4.0 | 7.3 | 62.9 | 74.2 |
| Rebuild as one pack, no deltas | 06:03–06:25, run 2 | 68 | 7.9 | 12.4 | 93.8 | 114.1 |
| Rebuild as one pack, no deltas | 06:03–06:25, run 3 | 113 | 8.3 | 11.8 | 82.2 | 102.2 |
| Rebuild and first export as one pack, no deltas | 06:51–07:12, run 1 | 145 | 10.5 | 12.4 | 71.2 | 94.1 |
| Rebuild and first export as one pack, no deltas | 06:51–07:12, run 2 | 141 | 8.2 | 11.6 | 81.2 | 101.0 |
| Rebuild and first export as one pack, no deltas | 06:51–07:12, run 3 | 136 | 8.4 | 12.0 | 80.4 | 100.8 |
| The same, with the delta repack | 08:42 | 102 | 9.1 | 7.5 | 81.1 | 97.7 |

**Idle database load** (transactions per second over each 10-minute window, 04:26–04:56)

| Gateway | Window 1 | Window 2 | Window 3 |
| --- | --- | --- | --- |
| Nobody connected | 2.64 (1,583) | 2.56 (1,536) | 2.57 (1,544) |
| One tab open on a note, live editing connected | 3.93 (2,359) | 3.91 (2,345) | 3.90 (2,341) |

**Browser after the incremental live wire, trace off: before (`storage-v2` at `1a7e51e`), per run** (as below)

| Measurement | Run 1 | Run 2 | Run 3 |
| --- | --- | --- | --- |
| Bare contenteditable, 1× | paint p50 2.4, p95 15.9; work p95 1.3; long tasks 0 / 0 | paint p50 2.5, p95 15.8; work p95 1.4; long tasks 0 / 0 | paint p50 2.5, p95 15.9; work p95 1.4; long tasks 0 / 0 |
| Bare contenteditable, 4× | paint p50 2.6, p95 15.9; work p95 1.7; long tasks 0 / 0 | paint p50 2.5, p95 15.9; work p95 1.5; long tasks 0 / 0 | paint p50 2.5, p95 15.8; work p95 1.6; long tasks 0 / 0 |
| F1 Visual (section mode), 1× | paint p50 12.8, p95 18.7; work p95 16; long tasks 5 / 94; opened 955 ms; 5 live answers of 2,821,467 | paint p50 12.7, p95 19.5; work p95 17; long tasks 5 / 93; opened 857 ms; 5 live answers of 2,821,462 | paint p50 13, p95 19; work p95 17.2; long tasks 5 / 93; opened 846 ms; 5 live answers of 2,821,461 |
| F1 Visual (section mode), 4× | paint p50 15.6, p95 55.4; work p95 35.2; long tasks 0 / 0; opened 4708 ms; 1 live answers of 2,821,469 | paint p50 15.6, p95 54.7; work p95 34.5; long tasks 0 / 0; opened 4750 ms; 1 live answers of 2,821,468 | paint p50 15.8, p95 41.9; work p95 34.9; long tasks 0 / 0; opened 4658 ms; 1 live answers of 2,821,467 |
| F1 Source, 1× | paint p50 12.9, p95 19.2; work p95 10.4; long tasks 6 / 98; opened 1077 ms; 6 live answers of 2,821,529 | paint p50 12.7, p95 18.7; work p95 10.4; long tasks 0 / 0; opened 1284 ms; 1 live answers of 2,821,468 | paint p50 11.9, p95 18; work p95 10; long tasks 0 / 0; opened 1282 ms; 1 live answers of 2,821,467 |
| F1 Source, 4× | paint p50 15.3, p95 56.6; work p95 31.8; long tasks 0 / 0; opened 4860 ms; 1 live answers of 2,821,536 | paint p50 15, p95 57.9; work p95 33.1; long tasks 0 / 0; opened 4818 ms; 1 live answers of 2,821,468 | paint p50 15, p95 56.5; work p95 32.1; long tasks 1 / 84; opened 4801 ms; 1 live answers of 2,821,467 |
| 53 KB note, Visual, 1× | paint p50 12.3, p95 18; work p95 12.5; long tasks 0 / 0; opened 273 ms; 3 live answers of 124,292 | paint p50 11.7, p95 18.3; work p95 12.3; long tasks 0 / 0; opened 252 ms; 2 live answers of 124,224 | paint p50 12.1, p95 18.2; work p95 12.3; long tasks 0 / 0; opened 281 ms; 3 live answers of 124,292 |
| 53 KB note, Visual, 4× | paint p50 15.2, p95 41.5; work p95 25.6; long tasks 0 / 0; opened 712 ms; 156 live answers of 124,939 | paint p50 25.3, p95 43.3; work p95 26.9; long tasks 0 / 0; opened 694 ms; 300 live answers of 125,499 | paint p50 15.9, p95 40.9; work p95 24.6; long tasks 0 / 0; opened 747 ms; 165 live answers of 124,967 |
| F1 Visual, agent patch every 500 ms, 1× | paint p50 19.6, p95 164.6; work p95 67.9; long tasks 90 / 126; 38 patches; 91 live answers of 2,825,366 | paint p50 57, p95 169.9; work p95 62.2; long tasks 112 / 115; 45 patches; 112 live answers of 2,825,159 | paint p50 16.1, p95 160.9; work p95 83.8; long tasks 79 / 103; 35 patches; 82 live answers of 2,825,055 |
| F1 Visual, agent patch every 500 ms, 4× | paint p50 110, p95 720.5; work p95 430.8; long tasks 100 / 456; 133 patches; 86 live answers of 2,829,275 | paint p50 113.4, p95 724.6; work p95 428.3; long tasks 97 / 456; 135 patches; 87 live answers of 2,829,554 | paint p50 108.1, p95 731.6; work p95 441.1; long tasks 104 / 489; 132 patches; 84 live answers of 2,829,133 |
| Another browser's edit shown (ms) | p50 420, p95 430, max 434 | p50 422, p95 460, max 463 | p50 420, p95 432, max 439 |
| An agent's patch shown (ms) | p50 124, p95 148, max 165 | p50 129, p95 149, max 150 | p50 120, p95 132, max 141 |
| Reader's live answers | 61 of 2,822,246 bytes | 61 of 2,822,179 bytes | 61 of 2,822,178 bytes |

**Browser after the incremental live wire, trace off: after (`v2/livewire` at `8739905`), per run**

| Measurement | Run 1 | Run 2 | Run 3 |
| --- | --- | --- | --- |
| Bare contenteditable, 1× | paint p50 2.5, p95 15.7; work p95 1.4; long tasks 0 / 0 | paint p50 2.2, p95 15.1; work p95 1.2; long tasks 0 / 0 | paint p50 2.4, p95 15.8; work p95 1.3; long tasks 0 / 0 |
| Bare contenteditable, 4× | paint p50 2.4, p95 15.9; work p95 1.4; long tasks 0 / 0 | paint p50 2.6, p95 16.3; work p95 1.6; long tasks 0 / 0 | paint p50 2.6, p95 16.3; work p95 1.5; long tasks 0 / 0 |
| F1 Visual (section mode), 1× | paint p50 12.6, p95 17.3; work p95 14.3; long tasks 0 / 0; opened 896 ms; 635 live answers of 344 | paint p50 12.5, p95 18.2; work p95 14.9; long tasks 0 / 0; opened 864 ms; 631 live answers of 344 | paint p50 11.7, p95 17.5; work p95 13.6; long tasks 0 / 0; opened 915 ms; 643 live answers of 344 |
| F1 Visual (section mode), 4× | paint p50 17.6, p95 27.6; work p95 23.9; long tasks 0 / 0; opened 4402 ms; 627 live answers of 440 | paint p50 20.1, p95 30.5; work p95 25.5; long tasks 0 / 0; opened 3551 ms; 592 live answers of 444 | paint p50 18.9, p95 28.9; work p95 25.2; long tasks 0 / 0; opened 4551 ms; 621 live answers of 440 |
| F1 Source, 1× | paint p50 12.1, p95 17.6; work p95 9.8; long tasks 0 / 0; opened 1093 ms; 634 live answers of 541 | paint p50 9.3, p95 16.6; work p95 3.3; long tasks 0 / 0; opened 955 ms; 666 live answers of 545 | paint p50 11.6, p95 17.5; work p95 9.2; long tasks 0 / 0; opened 853 ms; 642 live answers of 541 |
| F1 Source, 4× | paint p50 17.6, p95 21.6; work p95 14.1; long tasks 0 / 0; opened 4621 ms; 649 live answers of 641 | paint p50 18.5, p95 23.5; work p95 14.7; long tasks 0 / 0; opened 4013 ms; 633 live answers of 645 | paint p50 17.7, p95 23.2; work p95 14.3; long tasks 0 / 0; opened 4656 ms; 618 live answers of 641 |
| 53 KB note, Visual, 1× | paint p50 11.5, p95 16.8; work p95 11.3; long tasks 0 / 0; opened 277 ms; 660 live answers of 340 | paint p50 10.6, p95 16.5; work p95 9.4; long tasks 0 / 0; opened 229 ms; 679 live answers of 336 | paint p50 10.4, p95 16.8; work p95 9.6; long tasks 0 / 0; opened 284 ms; 678 live answers of 340 |
| 53 KB note, Visual, 4× | paint p50 13.7, p95 17.8; work p95 14.8; long tasks 0 / 0; opened 702 ms; 662 live answers of 440 | paint p50 13.5, p95 17.4; work p95 14.4; long tasks 0 / 0; opened 687 ms; 660 live answers of 436 | paint p50 14.3, p95 19; work p95 15.5; long tasks 0 / 0; opened 745 ms; 660 live answers of 440 |
| F1 Visual, agent patch every 500 ms, 1× | paint p50 8.9, p95 16.3; work p95 6.8; long tasks 0 / 0; 12 patches; 266 live answers of 713 | paint p50 12.3, p95 17.5; work p95 14.2; long tasks 0 / 0; 12 patches; 254 live answers of 713 | paint p50 9, p95 16.1; work p95 6.7; long tasks 0 / 0; 12 patches; 267 live answers of 713 |
| F1 Visual, agent patch every 500 ms, 4× | paint p50 21, p95 33.4; work p95 28.7; long tasks 0 / 0; 12 patches; 221 live answers of 753 | paint p50 19.9, p95 31.5; work p95 26.9; long tasks 0 / 0; 12 patches; 233 live answers of 753 | paint p50 21, p95 35.8; work p95 29.6; long tasks 0 / 0; 12 patches; 221 live answers of 753 |
| Another browser's edit shown (ms) | p50 32, p95 59, max 126 | p50 44, p95 65, max 161 | p50 41, p95 65, max 185 |
| An agent's patch shown (ms) | p50 22, p95 26, max 28 | p50 29, p95 58, max 71 | p50 26, p95 59, max 61 |
| Reader's live answers | 61 of 705 bytes | 61 of 705 bytes | 61 of 705 bytes |

**Browser, per run** (keystroke paint and work in ms; long tasks as count / longest ms; live answers' median size in bytes)

| Measurement | Run 1 | Run 2 | Run 3 |
| --- | --- | --- | --- |
| Bare contenteditable, 1× | paint p50 1.2, p95 15.4; work p95 0.6; long tasks 0 / 0 | paint p50 1.1, p95 15.6; work p95 0.6; long tasks 0 / 0 | paint p50 1, p95 14.7; work p95 0.6; long tasks 0 / 0 |
| Bare contenteditable, 4× | paint p50 3.8, p95 17.3; work p95 2.1; long tasks 0 / 0 | paint p50 3.5, p95 17.2; work p95 2; long tasks 0 / 0 | paint p50 3.9, p95 17.6; work p95 2.1; long tasks 0 / 0 |
| F1 Visual (section mode), 1× | paint p50 6.3, p95 19.4; work p95 6.9; long tasks 3 / 104; opened 1226 ms; 3 live answers of 2,821,428 | paint p50 6.6, p95 18.1; work p95 8.2; long tasks 0 / 0; opened 1204 ms; 1 live answers of 2,821,376 | paint p50 6.6, p95 18.7; work p95 6.7; long tasks 3 / 105; opened 1174 ms; 3 live answers of 2,821,429 |
| F1 Visual (section mode), 4× | paint p50 44.4, p95 117.5; work p95 67.2; long tasks 71 / 447; opened 6269 ms; 22 live answers of 2,825,019 | paint p50 37.5, p95 113.1; work p95 65.7; long tasks 72 / 449; opened 6172 ms; 33 live answers of 2,825,044 | paint p50 32.4, p95 104; work p95 64.7; long tasks 46 / 476; opened 6225 ms; 24 live answers of 2,824,861 |
| F1 Source, 1× | paint p50 9.4, p95 18.4; work p95 4; long tasks 0 / 0; opened 1755 ms; 1 live answers of 2,826,036 | paint p50 8.6, p95 18.9; work p95 4.9; long tasks 0 / 0; opened 1751 ms; 1 live answers of 2,826,036 | paint p50 10.2, p95 18.7; work p95 3.9; long tasks 0 / 0; opened 1762 ms; 1 live answers of 2,826,036 |
| F1 Source, 4× | paint p50 21.7, p95 93.2; work p95 57.2; long tasks 34 / 456; opened 6448 ms; 17 live answers of 2,829,047 | paint p50 20.6, p95 96.5; work p95 57.7; long tasks 34 / 460; opened 6806 ms; 15 live answers of 2,828,407 | paint p50 21.5, p95 99.4; work p95 58.2; long tasks 17 / 441; opened 6415 ms; 11 live answers of 2,829,047 |
| 53 KB note, Visual, 1× | paint p50 9.9, p95 16.8; work p95 3.4; long tasks 0 / 0; opened 331 ms; 2 live answers of 124,224 | paint p50 9.6, p95 16.8; work p95 3.4; long tasks 0 / 0; opened 327 ms; 2 live answers of 124,224 | paint p50 9.6, p95 16.8; work p95 4.3; long tasks 0 / 0; opened 326 ms; 3 live answers of 124,286 |
| 53 KB note, Visual, 4× | paint p50 22.5, p95 35.1; work p95 16.7; long tasks 0 / 0; opened 1158 ms; 5 live answers of 128,457 | paint p50 17.5, p95 32.5; work p95 15.4; long tasks 0 / 0; opened 1029 ms; 3 live answers of 126,573 | paint p50 15.6, p95 29.5; work p95 14.3; long tasks 0 / 0; opened 998 ms; 1 live answers of 126,501 |
| F1 Visual, agent patch every 500 ms, 1× | paint p50 7.1, p95 152.4; work p95 78.9; long tasks 85 / 110; 66 live answers of 2,834,304; 40 patches | paint p50 7.4, p95 138.4; work p95 61.1; long tasks 70 / 112; 55 live answers of 2,834,353; 35 patches | paint p50 7.4, p95 132; work p95 57; long tasks 65 / 105; 49 live answers of 2,834,317; 31 patches |
| F1 Visual, agent patch every 500 ms, 4× | paint p50 254.5, p95 793.5; work p95 376.9; long tasks 551 / 518; 267 live answers of 2,845,136; 490 patches | paint p50 182.6, p95 796; work p95 393.2; long tasks 596 / 530; 269 live answers of 2,845,298; 505 patches | paint p50 262.7, p95 748.5; work p95 379.5; long tasks 516 / 500; 286 live answers of 2,845,316; 494 patches |
| Another browser's edit shown (ms) | p50 444, p95 478, max 501 | p50 436, p95 456, max 463 | p50 439, p95 457, max 462 |
| An agent's patch shown (ms) | p50 131, p95 139, max 141 | p50 125, p95 134, max 148 | p50 135, p95 158, max 166 |
| Reader's live answers | 61 of 2,831,386 bytes | 61 of 2,831,386 bytes | 61 of 2,831,386 bytes |
