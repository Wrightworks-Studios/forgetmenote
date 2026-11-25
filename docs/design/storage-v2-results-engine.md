# Storage v2 bake-off: D1 engine results

- **Status:** measured; decision recorded below
- **Date:** 2026-11-22
- **Plan:** `storage-v2-bakeoff.md` §3.1 (engine harness), §4 (budgets) and §5 (decision rules)
- **Code:** `proto/storage-v2/engine/` on branch `v2/engine` (a separate Go module, never imported by `forgetmenote`); raw results in `proto/storage-v2/engine/results/`. Paths below are relative to that directory.

## Decision D1: Y (Yjs in the browser, `reearth/ygo` on the server)

The §5 rules, applied as written:

1. **"Choose Loro only if one Loro option passes every must row in §4, and it meets at least two of the should rows or shows working blame and time travel that Y does not."**
   - **L-cgo fails a must row.** The build stage of the existing Dockerfile cannot build it for linux/amd64 or linux/arm64. With `CGO_ENABLED=1` it stops at `cgo: C compiler "gcc" not found`. With gcc available, Go 1.27.4 rejects loro-go's prebuilt archives: `disallowed LTO section ".llvmbc" in #cgo LDFLAGS`. Static binaries for both architectures do build, but only after five changes to the build stage (see [Builds](#builds)), so the row "through the existing Dockerfile" fails.
   - **L-wasm passes both must rows** (10,000/10,000 interop rounds, and unchanged static builds for both architectures). It meets **one of three should rows**: patching from a 1-day-old base without a retained snapshot.
     - **RSS: fail.** Ready with F1 and full history it reaches 84.6 MiB VmHWM, 4.16× Y's 20.3 MiB. With a warm compilation cache it is 52.7 MiB (2.59×).
     - **Bundle: fail.** Loro's engine alone is 1,012 KiB gzipped, against 232 KiB for today's Milkdown and Yjs chunks together.
   - **The blame and time-travel clause does not apply.** Y passed the same correctness checks: blame 10/10, materialize 9/9 and diff 7/7 against the ground truth. What Loro adds is that it needs no retained base, which the should row already credits.
2. **Between the two Loro options:** not reached. For the record, L-wasm is within 2× of L-cgo on steady-state apply (27.3 vs 21.4 µs p50, 1.28×; 1k batch 1.77×). It is not within 2× on time to ready (415 vs 120 ms, 3.5×) or on mean typing cost including the first update (2.10×). L-cgo's build does not work in the existing Dockerfile, so neither tie-break clause would pick it either.
3. **"Otherwise choose Y, with blame computed from revision diffs."** → **Y.**

**How sensitive the decision is.** Two inputs could flip it:

- **The RSS row.** It is measured as the plan specifies: one process per run, VmHWM. That includes wazero's compiler and compiled code, about 24–57 MiB fixed per process. Measured per document instead (ready minus before-load), the costs are:
  - L-wasm (with cache): 21.4 MiB
  - Y: 12.9 MiB
  - L-cgo: 28.7 MiB

  On that reading L-wasm is 1.66× Y, so it would pass. It would then meet two should rows and be eligible. The tie-break would not select L-cgo, so L-wasm would be the Loro choice.
- **The cost of going that way.** It means:
  - 0.2–0.5 s agent patches from day-old bases;
  - 0.8–4.5 s to materialize an old revision of F1, and up to 9 s to diff two;
  - about 0.4 s of first-apply work after each load;
  - a 1 MiB gzipped browser engine;
  - a Rust toolchain in the release path.

  Y has known, fixable costs (see [Findings](#findings-for-the-s8-engine-stream)) and is today's integration.

## Setup

- **Machine:**
  - The development server: Intel Core i7-12700K (20 threads, hybrid P/E cores), 46 GB, Linux 6.1.0-56-amd64. It also runs other workloads.
  - Every run is in a container with `--cpus 4` and `GOMAXPROCS=4`.
  - Go 1.27.4 (`fmn-dev-go:local`, Alpine, for builds; `fmn-dev-browser:local`, Ubuntu 24.04, for runs that also need Node 24.20.0).
- **Candidates and pins (all exact):**

  | ID | Browser | Server | Notes |
  | --- | --- | --- | --- |
  | **Y** | `yjs@13.6.32` | `github.com/reearth/ygo v1.49.5` | Same versions as the main module and `package.json` |
  | **L-cgo** | `loro-crdt@1.13.6` | `github.com/aholstenson/loro-go v0.5.0` | uniffi over loro-ffi v1.13.6; prebuilt static archives; cgo |
  | **L-wasm** | `loro-crdt@1.13.6` | `loro = "=1.13.6"` built for `wasm32-wasip1` + `github.com/tetratelabs/wazero v1.12.0` | `shim/` (C ABI, about 400 lines of Rust); its `Cargo.lock` is seeded from loro-go's, so both Loro routes run the same crate versions. Built with `rust:1.99.1-slim-bookworm@sha256:f2c004e3…`: 2,472,928 bytes, SHA-256 `0917864a…f95d` |

  Loro 1.13.6 is the core loro-go v0.5.0 ships. The browser uses the same version, so the fuzz compares like with like (npm's latest is 1.18.0).
- **Fixture:** `f1-moby-dick.md` (1,206,841 bytes, 1,192,095 UTF-16 units, SHA-256 `15bf4dd6…77dd`), read-only.
- **History (F1h):**
  - `hist.Generate` with seed 2026112201 produces 5,000 revisions: 1,762 word edits, 1,692 sentence edits, 388 paragraph inserts, 358 paragraph deletes, 234 chapter moves, 278 front-matter changes and 288 renames (no text change). The mix is the plan's 70/15/5/5/5, with a few unplaceable moves turned into word edits. About 4% of inserted words are non-ASCII or astral (`🐋`, `日本`, `é`).
  - Three people edit through their own client documents: a local edit, then server `Apply`. Two agents edit through server `Patch`.
  - A ground-truth model gives every UTF-16 unit an identity and an inserting actor. The server text was checked against it at 11 checkpoints, and all matched.
  - Simulated time is 500 revisions per day, so patch bases of 1 minute, 1 hour and 1 day are 1, 21 and 500 revisions before the head.
- **Repetition:**
  - Loads use 5 separate processes (the median is shown).
  - Typing and batch applies are 5 runs, each on a fresh load (median); p50/p95 come from run 1.
  - Patches are 100 attempts per base age.
  - History queries use 7 to 10 samples.
- **Browser-side updates:**
  - The 10k typing updates and the 1k batch are produced by the browser libraries (`js/trace.mjs`) from the server's full snapshot, as a browser session would.
  - ygo's own transaction updates carry the whole delete set: 6.4 KB on average for the people's edits made by Go client documents in the history replay, against 16 B per browser keystroke. Using them would have penalized Y.

## Results

### Load with full history: time and RSS

Each run is a separate process. It reads the snapshot, then measures:

- `Load`;
- the first `Text()`;
- one keystroke `Apply`;
- VmHWM.

"Ready" is the sum of the three timings. Loro defers decoding to first use, so a load that stops early understates its cost.

| Engine | Mode | Compile (ms) | Load (ms) | First `Text` (ms) | First apply (ms) | Ready (ms) | VmHWM before load | VmHWM ready | × Y |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Y | process | – | 29.0 | 1.9 | 0.0 | **31.4** | 7.4 MiB | **20.3 MiB** | 1.00 |
| L-cgo | process | – | 28.5 | 3.7 | 87.9 | **120.0** | 8.4 MiB | **37.1 MiB** | 1.83 |
| L-wasm | process (cold compile, mmap memory) | 756.1 | 13.3 | 4.0 | 397.6 | **414.8** | 64.3 MiB | **84.6 MiB** | 4.16 |
| L-wasm | wazero compilation cache | 27.3 | 12.4 | 4.1 | 396.8 | 413.6 | 31.2 MiB | 52.7 MiB | 2.59 |
| L-wasm | wazero default (linear memory on the Go heap) | 754.4 | 26.2 | 4.2 | 394.7 | 427.0 | 63.6 MiB | 100.5 MiB | 4.94 |

- **L-wasm memory.** By default the harness gives L-wasm one reserved anonymous mapping per instance (`lorodoc/lorowasm/mmap_linux.go`). Growing the memory then never copies, and it stays outside the Go GC's headroom. With wazero's default heap-backed memory, RSS rises by 16 MiB (35 MiB in the first full run).
- **L-wasm compile cost.** The first document in a process pays 0.76 s to compile the module. A compilation cache needs a writable directory and cuts this to 27 ms.
- **Run-to-run variation.** The first full run (results at commit `5051826`) measured Y at 24.1 MiB and L-cgo at 38.5 MiB (1.60×). Go's GC timing moves Y's high-water mark by a few MiB, so L-cgo's ratio sits between 1.6× and 1.8×. Both are under 2×.

### Snapshot sizes (bytes)

| Engine | Full history | Compacted | Other | Text (UTF-8) |
| --- | ---: | ---: | ---: | ---: |
| Y | 3,755,838 | 1,266,800 (GC'd re-encode, same IDs) | 1,166,066 (text only, new identity) | 1,166,043 |
| L-cgo | 3,741,481 | 832,115 (shallow snapshot) | 832,115 (state only) | 1,166,043 |
| L-wasm | 3,658,991 | 816,252 (shallow snapshot) | 816,238 (state only) | 1,166,043 |

Every compacted form loads back with identical text and accepts a later update from an up-to-date peer (tested in `enginetest`).

### Apply: 10k single-character updates and one 1k-operation batch

The server applies each browser update through `Apply`, which:

1. checks the size limits;
2. checks the peer or client ID against the authenticated actor;
3. validates the update (Y) or imports it and checks its containers (Loro).

The 10k typing updates are measured in two ways:

- **ns/op:** the total over all 10k updates divided by 10,000, with Go allocations taken from `runtime.MemStats`. Allocations inside Rust are not counted.
- **Steady state:** the median total minus the first update after load (run 1), over the other 9,999.

| Engine | Update bytes (mean) | ns/op | allocs/op | B/op | p50 µs | p95 µs | max µs | Total ms (median of 5) | Steady ns/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Y | 16.1 | 2,778 | 56 | 3,339 | 2.6 | 3.6 | 40 | 27.8 | 2,775 |
| L-cgo | 95.0 | 32,280 | 109 | 5,573 | 21.4 | 30.5 | 88,650 | 322.8 | 23,418 |
| L-wasm | 95.0 | 67,868 | 29 | 522 | 27.3 | 29.2 | 402,711 | 678.7 | 27,600 |

The max for each Loro engine is the first update after load (the deferred decoding above).

The batch is one browser update of 1,000 single-character inserts and deletes spread over the whole document. It is applied to a live document (warmed with one keystroke):

| Engine | Batch update bytes | ms (median of 5) | Go allocs | Go bytes |
| --- | ---: | ---: | ---: | ---: |
| Y | 7,686 | 1.22 | 9,252 | 571,456 |
| L-cgo | 5,870 | 3.59 | 121 | 41,152 |
| L-wasm | 5,870 | 6.34 | 28 | 416 |

### `EncodeSince`, `Text` and `Version` on F1 (after history and 10k typing; `testing.Benchmark`)

| Operation | Y | L-cgo | L-wasm |
| --- | ---: | ---: | ---: |
| `EncodeSince`, peer 1 update behind | 268.2 µs (122 allocs, **9,830 B out**) | 10.0 µs (47 allocs, 95 B out) | 10.7 µs (18 allocs, 95 B out) |
| `EncodeSince`, peer 10k updates behind | 1,011.9 µs (139 allocs, 110,948 B out) | 32.4 µs (48 allocs, 10,385 B out) | 49.2 µs (18 allocs, 10,385 B out) |
| `Text()` | 710.0 µs (23 allocs) | 971.9 µs (35 allocs) | 419.1 µs (6 allocs) |
| `Version()`, a patch base | 143.4 µs (78 allocs, **9,836 B**) | 749 ns (10 allocs, 5 B) | 244 ns (5 allocs, 5 B) |
| `SyncVersion()`, a sync token | 301 ns (6 allocs, 21 B) | 1.2 µs (11 allocs, 33 B) | 493 ns (5 allocs, 33 B) |

- **Yjs `EncodeSince`** always carries the whole delete set (about 9.8 KB at the F1h head).
- **Y's `Version()`** has to be the Yjs snapshot (state vector plus delete set). Deletions do not advance a state vector, so a state vector cannot name a version (see [Findings](#findings-for-the-s8-engine-stream)).

### Agent `Patch` from bases 1 minute, 1 hour and 1 day (500 revisions) old

How the harness runs it:

- An agent makes 1–3 word replacements or insertions, in distinct paragraphs of the base text.
- The ground truth decides each expected outcome:
  - a patch succeeds when every original target unit survives contiguously with no new text inside it;
  - an insertion needs both of its original neighbours alive.
- Patches accumulate at the head.
- Every success must leave exactly the expected text.

| Engine | Base age | Base token | Expected / got success | Expected / got conflict | Wrong | p50 ms | p95 ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Y | 1 min (1 rev) | 9,829 B | 100 / 100 | 0 / 0 | 0 | 47.1 | 52.9 |
| Y | 1 h (21 rev) | 9,781 B | 99 / 99 | 1 / 1 | 0 | 48.3 | 54.3 |
| Y | 1 day (500 rev) | 9,614 B | 65 / 65 | 35 / 35 | 0 | 43.1 | 50.1 |
| L-cgo | 1 min | 7 B | 100 / 100 | 0 / 0 | 0 | 12.7 | 18.3 |
| L-cgo | 1 h | 6 B | 99 / 99 | 1 / 1 | 0 | 27.9 | 32.7 |
| L-cgo | 1 day | 5 B | 65 / 65 | 35 / 35 | 0 | 169.7 | 272.1 |
| L-wasm | 1 min | 7 B | 100 / 100 | 0 / 0 | 0 | 27.1 | 42.5 |
| L-wasm | 1 h | 6 B | 99 / 99 | 1 / 1 | 0 | 70.7 | 79.6 |
| L-wasm | 1 day | 5 B | 65 / 65 | 35 / 35 | 0 | 500.4 | 508.5 |

**Y needs the base's Yjs snapshot retained.**

- **Cost per base:** 9.6–9.8 KB, the whole delete set.
- **Cost of keeping a day of per-revision bases:** 4,768,401 bytes per document (Loro: 2,773 bytes of frontiers).
- **Retention today:** live bases are kept for 15 minutes and 16 MiB.
- **Without a retained snapshot,** Y cannot patch from an older base at all, because the deletions since then are not recoverable from a state vector.
- **Why its latency is flat:** it rebuilds the base document twice per patch (today's algorithm), at about 43–48 ms on F1.

**At the current version** (the agents' history-replay edits, 1,872 patches), the costs are:

| Engine | p50 µs | p95 µs |
| --- | ---: | ---: |
| Y | 1,759 | 3,041 |
| L-cgo | 36 | 123 |
| L-wasm | 73 | 229 |

Y validates against the whole text and captures two snapshots, while Loro compares frontiers and reads slices.

**Loro patches from the frontiers alone.**

- **Method:** one diff from the head back to the base gives both the base text and the position mapping.
- **Cost:** it grows with the history in between. A day back (500 revisions, about 23 chapter moves) costs 170 ms (cgo) or 500 ms (wasm) at p50.

### History: materialize, diff and blame (correctness against the F1h ground truth)

- **Materialize** samples revisions 500, 1,000, … and 4,500.
- **Diff** samples revision pairs (N, N+500) and (head−1, head) and (head−21, head). A diff is correct if applying it to text N gives text M byte for byte.
- **Blame** reads 10 random 10 KiB (10,240-unit) ranges at the head and compares each unit's inserting actor.

| Engine | Materialize p50 / p95 ms (correct) | Diff p50 / p95 ms (correct) | Blame 10 KB p50 / p95 ms (correct) |
| --- | --- | --- | --- |
| Y | 11.1 / 19.4 (9/9) | 43.0 / 65.8 (7/7) | 337.2 / 1,147.6 (10/10) |
| L-cgo | 789.8 / 3,140.1 (9/9) | 801.3 / 4,682.6 (7/7) | 6.30 / 6.59 (10/10) |
| L-wasm | 2,116.6 / 4,539.1 (9/9) | 2,363.8 / 9,020.8 (7/7) | 4.82 / 5.12 (10/10) |

**Y:**
- **Materialize** rebuilds a document from the revision's Yjs snapshot, the version token. So Y's time travel works only for versions whose token was kept: 9.8 KB each at the head.
- **Diff** is a line diff of two materialized texts, as with revision diffs.
- **Blame** reads each unit's item ID through a relative position, then maps it to an actor through the server's client and stamp tables. ygo exposes no item walk, so the cost is per character.

**Loro:**
- **Materialize** checks out the frontiers and returns. Checking out is the fastest of three methods tried; the others were forking and diffing.
- **Diff** is `LoroDoc.diff`.
- **Blame** uses `get_editor_at_unicode_pos`: a loop inside the shim for wasm, and one cgo call per character for cgo.
- **Cost:** materializing and diffing grow with the distance through the history, and reached 3.1–9.0 s at p95 on F1h.

### Interop fuzz: 10,000 rounds, 2 JS peers and 1 Go peer, concurrent edits in the same paragraph

Seed 20261122 for every engine (`results/fuzz-*.json`).

| Engine | Rounds | Byte-identical every round | Failures | Go patches | Syncs through `EncodeSinceSync` | Duplicate deliveries | Snapshot cross-loads | Go calls | Seconds |
| --- | ---: | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| Y | 10,000 / 10,000 | **yes** | 0 | 10,000 (3,658 from older bases, 223 conflicts) | 1,031 | 505 | 10 | 51,557 | 1,609 |
| L-cgo | 10,000 / 10,000 | **yes** | 0 | 10,000 (3,734 from older bases, 206 conflicts) | 1,056 | 519 | 10 | 51,596 | 14 |
| L-wasm | 10,000 / 10,000 | **yes** | 0 | 10,000 (3,734 from older bases, 206 conflicts) | 1,056 | 519 | 10 | 51,596 | 22 |

**How each round runs (`js/fuzz.mjs` with `cmd/enginepeer`):**

1. Both JS peers make 1–3 random edits in the hot paragraph:
   - inserts and deletes;
   - non-ASCII text and astral `🐋`;
   - each peer works without seeing the others.
2. At the same time, the Go peer patches 1–2 edits as an agent:
   - half from its current version;
   - half from a version 0–3 rounds old, which exercises base mapping and conflicts.
3. The updates are exchanged in random order:
   - 10% of the time, a JS peer syncs through Go's `EncodeSinceSync` instead of relayed updates;
   - 5% of rounds deliver an update twice.
4. The round passes only if all three texts are byte-identical (UTF-8).
5. Every 1,000 rounds, snapshots also cross the boundary both ways: a Go snapshot loads in JS, a JS snapshot loads in Go, and both must give the same text.

### Builds

**Build results.** Every binary below ran the post-build smoke test (`smoke/`). The test loads, patches, syncs a client edit, does a snapshot round trip and an old-base patch. amd64 binaries ran in `alpine:3`; arm64 binaries ran under `qemu-aarch64-static` inside a Debian container. No host binfmt was changed. Logs: `results/builds/`.

| Build | Y | L-wasm | L-cgo |
| --- | --- | --- | --- |
| Existing Dockerfile build stage (`golang:1.27.4-alpine@sha256:0a5f7fe7…`, `$BUILDPLATFORM`, `CGO_ENABLED=0`, cross `GOARCH`): linux/amd64 | **pass**, static, 3,166,368 B | **pass**, static, 8,544,416 B | **fail**: with `CGO_ENABLED=1`, `cgo: C compiler "gcc" not found` |
| Same, linux/arm64 | **pass**, static, 3,014,816 B | **pass**, static, 8,257,696 B | **fail** (same) |
| Alpine with gcc but no workaround | – | – | **fail**: `disallowed LTO section ".llvmbc" in #cgo LDFLAGS: …/libs/x86_64-unknown-linux-musl/libloro.a` (Go 1.27.4 checks linker inputs in the module cache) |
| Modified build stage (`build/Dockerfile.cgo`, see the changes below): linux/amd64 | – | – | pass, static musl, 9,379,416 B, no runtime libraries |
| Same, linux/arm64 | – | – | pass, static musl, 8,812,320 B (the first try failed with undefined `_Unwind_*` until `-lunwind` was added) |
| `golang:1.27.4-bookworm` (glibc) with gcc and the strip workaround: amd64, dynamic | – | – | pass, 9,351,896 B; runtime libraries `libgcc_s.so.1`, `libc.so.6` and `ld-linux-x86-64.so.2`. It runs on the current runtime base `python:3.12-slim-bookworm` but not on musl |
| Same: amd64 static and arm64 static (`gcc-aarch64-linux-gnu`) | – | – | pass, 10,158,872 B and 9,129,976 B; no runtime libraries |
| darwin/arm64 and darwin/amd64 development build | pass (cross, `CGO_ENABLED=0`; not run): 3,021,810 B and 3,227,728 B | pass (cross; not run): 8,348,978 B and 8,658,944 B | **not verifiable on the development server**: cross-linking with zig lacks the Apple SDK (`unable to find dynamic system library 'resolv'`); the Mach-O archives also need their `__LLVM` bitcode stripped before Go 1.27.4 accepts them, so a Mac needs the same workaround |

**The five changes L-cgo needs** in the build stage (`build/Dockerfile.cgo`):

1. `apk add gcc musl-dev`;
2. `llvm20`, whose `llvm-objcopy` strips `.llvmbc`/`.llvmcmd` from both architectures' archives (GNU objcopy cannot read the aarch64 objects);
3. a `go.work` replace that points at the stripped copy of the module;
4. `zig` as the aarch64 musl cross compiler, with `CGO_LDFLAGS=-lunwind`;
5. `-linkmode external -extldflags -static`.

**Image size.** Today's image is 296.4 MB (`forgetmenote:local`). Only the binary changes with the engine. Relative to the Y smoke binary, L-wasm adds about 5.4 MB (the 2.47 MB module plus wazero), and static L-cgo adds about 6.2 MB.

### Browser bundle size

esbuild 0.28.2 with the app's settings (ESM, minified, browser). Sizes are taken from the metafile and gzipped at level 9 (`js/bundle.mjs`, `results/bundle.json`).

| Bundle | JS bytes | JS gzip | Wasm bytes | Wasm gzip | Total gzip |
| --- | ---: | ---: | ---: | ---: | ---: |
| Yjs (the functions `collaboration.mjs` uses) | 78,587 | 23,825 | – | – | **23,825** |
| Loro (`LoroDoc`; browser build fetches `loro_wasm_bg.wasm` by URL) | 76,551 | 11,846 | 3,112,556 | 1,024,184 | **1,036,030** |
| Today: `milkdown-BA5HKI7C.js` + `collaboration-NDPWEN3X.js` (built app at the base commit) | 745,643 | 237,778 | – | – | **237,778** |

## Budgets (§4, engine rows)

| Row | Level | Y | L-cgo | L-wasm |
| --- | --- | --- | --- | --- |
| Interop fuzz: 10,000/10,000 rounds converge, Go and JS agreeing | must | **pass** (10,000/10,000) | **pass** (10,000/10,000) | **pass** (10,000/10,000) |
| Static builds: linux/amd64 and arm64 through the existing Dockerfile | must | **pass** | **fail** (passes only with the five-change build stage) | **pass** (needs the shim `.wasm` in the build context: committed with a reproducibility check, or a pinned Rust stage) |
| F1 with full history, RSS ≤ 2× Y | should | baseline (20.3 MiB) | **pass** (37.1 MiB, 1.83×; 1.60× in the first run) | **fail** (84.6 MiB, 4.16×; 2.59× with a compilation cache) |
| Agent patch from a 1-day-old base succeeds without a retained snapshot | should | **fail** (needs the 9.6 KB base snapshot) | **pass** (65/65, 35/35 conflicts, 0 wrong; p50 170 ms) | **pass** (same outcomes; p50 500 ms) |
| Bundle (engine plus editor, gzipped) ≤ today's Milkdown and Yjs chunks combined | should | **pass** on the engine side (23.3 KiB, unchanged; the editor is D2) | **fail** (engine alone 1,012 KiB > 232 KiB) | **fail** (same) |

## Findings for the S8 engine stream

These hold whichever engine ships, unless marked.

1. **`Apply` needs the authenticated actor** (a change to the §9 signature). Both engines take attribution from IDs that clients choose: Yjs client IDs and Loro peers. The server must bind each ID to the connection's actor and reject an update that reuses another actor's ID or the server's own. The harness does this before integrating: from `ContentIDsFromUpdateV1` for Y, and from the blob metadata for Loro. It is tested in `ApplyRejectsAnotherActorsPeer`.
   - **Only new operations count against the sender.** Today's frontend resends its whole state for a pending draft (`Y.encodeStateAsUpdate`), and that carries other actors' items. The Y harness checks only items beyond the server's state vector (`TestApplyAcceptsResentFullState`). The Loro harness still checks every peer in the blob, so Loro clients must export only their own changes; S8 would need a post-import check of the newly applied spans instead.
2. **(Y) `Version()` cannot be a state vector.**
   - **Problem:** a delete-only change does not advance a Yjs state vector, so two different versions share one.
   - **Split the token in two:**
     - for sync, the state vector (21 B);
     - for patch bases, the encoded snapshot, which is the state vector plus the delete set: 9.8 KB at the F1h head, costing 143 µs to capture.
   - **Storage:** the server can keep the short revision IDs it hands out today and map each to a stored snapshot.
3. **(Y) ygo transaction updates contain the whole delete set.** `OnUpdate` encodes structs since the transaction's start plus the document's full delete set. Over the F1h replay, the agents' patch updates averaged 6,509 B for Y against 745 B for Loro, and the Go client documents' edits 6,416 B against 593 B; browser Yjs sends 16 B per keystroke. `EncodeSince` carries the whole delete set by Yjs design. S8 should either encode the transaction's own delete set, which is a small change upstream or in a wrapper, or accept the cost.
4. **(Y) Raise or replace today's `WithMaxPendingItems(1024)`.**
   - **Failure:** with that setting, ygo cannot load F1 with its 5,000-revision history, neither the full nor the GC-compacted state. It fails with `crdt: invalid update` after parking 1,024 items, because it decodes one client at a time.
   - **Why it does not show today:** checkpoints reset the live state.
   - **What the harness did:** it uses 1<<20. Size limits on stored state, and the validator on incoming updates, already bound the work.
5. **(Y) The GC'd re-encode is the compaction.** It is 1.27 MB, against 3.76 MB with full history, and keeps every item ID, so connected peers keep syncing. The text-only "new identity" form is 1.17 MB but forces every peer to reload.
6. **(Y) Attribution and the interop fuzz.**
   - **Native blame:** it works through relative positions plus a server stamp table for the server's own client ID; ygo fixes a document's client ID at creation, so patches cannot use per-agent IDs. It is slow: 0.34–1.15 s per 10 KB, because there is no item iterator. Per §5, blame should come from revision diffs; native attribution is an optimization for later.
   - **The fuzz gets slower as it runs, for Y only.** A timed 3,000-round rerun (`results/fuzz-Y-profile.json`) took 6.4, 19.0 and 38.4 ms per round in successive thousands, 87–94% of it waiting on the Go peer. The full 10,000 rounds averaged 161 ms per round. Loro stayed flat at 1.0–2.2 ms (cgo) and 1.5–3.2 ms (wasm) per round in every thousand.
   - **Why:** the server document keeps every tombstone with GC off, and ygo's `Version`, patch rebuilds, transaction updates and `EncodeSince` all walk or carry the whole delete set (items 3 and 4). This is the cost of keeping history inside the live Yjs document. Compaction at checkpoint (item 5) bounds it, so S8 should compact on the checkpoint schedule (Q1) rather than let a live document grow for days.
7. **(Loro) Updates cannot be validated before import.**
   - **What the harness does:** it imports, then checks the containers the new operations touched (`get_changed_containers_in`) and the text length. On a violation it reloads the stored snapshot plus the accepted log.
   - **Gap:** rich-text marks on the markdown text would pass this check.
   - **WebAssembly risk:** a malformed update that makes Loro panic traps the instance, because there is no unwinding. The instance must then be rebuilt from storage.
8. **(Loro) Deferred work moves cost to the first apply.** It takes 88 ms for cgo and 398 ms for wasm after a 12–29 ms load. Warming the document at load, off the request path, avoids a stall on the first keystroke.
9. **(L-wasm) wazero settings that matter:**
   - an mmap-backed linear memory, which saves 16–35 MiB of RSS on F1;
   - a compilation cache directory: 0.76 s → 27 ms per process;
   - one instance per document, so memory is returned when the document closes.

## Reproducing

From `proto/storage-v2/engine` (Docker required; outputs land in gitignored `build/out`, `results/tmp`):

```sh
shim/build.sh                                   # loro_shim.wasm in the pinned Rust image
~/forgetmenote-design/bin/fmn-go . sh -c './build/cgo-workspace.sh'   # stripped loro-go copy + build/cgo.work
~/forgetmenote-design/bin/fmn-go . sh -c 'CGO_ENABLED=0 go test ./... && GOWORK=$PWD/build/cgo.work CGO_ENABLED=1 go test ./lorodoc/lorocgo'
(cd js && npm ci)                               # inside fmn-dev-browser
~/forgetmenote-design/bin/fmn-go . sh -c 'CGO_ENABLED=0 go build -o build/out/enginebench ./cmd/enginebench && CGO_ENABLED=0 go build -o build/out/enginepeer ./cmd/enginepeer &&
  GOWORK=$PWD/build/cgo.work CGO_ENABLED=1 go build -ldflags "-linkmode external -extldflags -static" -o build/out/enginebench-cgo ./cmd/enginebench &&
  GOWORK=$PWD/build/cgo.work CGO_ENABLED=1 go build -ldflags "-linkmode external -extldflags -static" -o build/out/enginepeer-cgo ./cmd/enginepeer'
FMN_IMAGE=fmn-dev-browser:local build/run.sh build/out/enginebench run -engine Y -fixture /fixtures/f1-moby-dick.md \
  -out results/engine-Y.json -snapdir results/tmp -tracecmd "node js/trace.mjs"          # likewise L-cgo (enginebench-cgo), L-wasm
FMN_IMAGE=fmn-dev-browser:local build/run.sh node js/fuzz.mjs Y build/out/enginepeer 10000 20261122 results/fuzz-Y.json
node js/bundle.mjs <app dist dir> results/bundle.json
docker buildx build -f build/Dockerfile.purego --build-arg PKG=./cmd/smoke-lwasm --platform linux/arm64 --output type=local,dest=build/out/docker/lwasm-arm64 .
docker buildx build -f build/Dockerfile.cgo --platform linux/arm64 --output type=local,dest=build/out/docker/lcgo-arm64 .
```
