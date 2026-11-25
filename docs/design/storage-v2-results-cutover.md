# Storage v2 cutover rehearsal results

- **Status:** rehearsed; one import bug found and fixed, runbook corrected
- **Date:** 2026-11-23
- **Brief:** wave 3 item 3, a cutover rehearsal on read-only copies of the single-host instance's notebooks
- **Code:** branch `v2/rehearsal`. The first runs used the base commit `c985618`; the final run used `f60c92c`, which has the fix below.
- **Runbook:** [storage-cutover.md](../storage-cutover.md), corrected where the rehearsal showed it wrong or unclear (see [Runbook corrections](#runbook-corrections))

The rehearsal followed the runbook as an operator would, on copies of the production data directory's notebooks, against a disposable database. Every notebook and every commit was imported. Commit SHAs, trees and file bytes are identical between each source repository and its export. The server served every note and attachment byte for byte. A side-by-side run of the previous Git backend and the imported store agreed on all 11,619 comparisons once the import fix was in. The old storage was left untouched.

Notebooks are called N1 to N5 here, in their sorted directory order. This document gives only counts, sizes, timings and match results, never notebook IDs, titles, paths or content.

## The copy

The rehearsal's `data-ro` directory is a read-only copy of the production data directory's catalog files and `projects/`. Secrets, chat, Jev and OAuth files are left out. All five repositories pass `git fsck`.

| Notebook | Commits on `HEAD` (= all refs) | Files at `HEAD` | Notes at `HEAD` | Git objects | Live snapshots / append logs |
| --- | --- | --- | --- | --- | --- |
| N1 | 1 | 0 | 0 | 2 | 0 / 0 |
| N2 | 10 | 1 | 1 | 29 | 0 / 0 |
| N3 | 1 | 0 | 0 | 2 | 0 / 0 |
| N4 | 30 | 62 | 34 | 227 | 33 / 0 |
| N5 | 133 | 712 | 685 | 2,947 | 77 / 66 |
| **Total** | **175** | **775** | **720** | **3,207** | **110 / 66** |

- `projects/` takes 3,392 KiB.
- Every repository has one branch, `main`, and no merges. Every working tree is clean.
- No notebook has a `write-journal.json` or a `captures/` directory.
- Each `collaboration/` directory also holds a `receipts` entry.
- The file-era `catalog.json` (version 9) lists three of the five notebooks, N3 to N5, and not N1 or N2.

## Setup and deviations from the runbook

Everything ran inside `fmn-dev-full:local`, through `fmn-node` with 4 CPUs. The image has Go 1.27.4, Git 2.43.0, Postgres 16 and Python 3.12. The containers ran with `--network none`. The final run also had `--read-only --tmpfs /tmp:size=256m,mode=1777`, the root filesystem constraints of the Compose service.

- **Binary.** Built as the Dockerfile builds it: `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/forgetmenote`.
- **Database.** A disposable cluster inside the container, under the work directory, reachable only through its Unix socket:
  - `initdb -D pg -U forgetmenote -A trust -E UTF8 --no-locale`;
  - `postgres -D pg -k run -c listen_addresses=`;
  - `DATABASE_URL_FILE` holding `postgres://forgetmenote@/forgetmenote?host=<socket dir>&sslmode=disable`.
- **Work directories.** `rehearsal/work-1` to `work-3`, each `cp -a` of the copy followed by `chmod -R u+w`. All of them, their databases and the extracted base source tree were removed at the end.
- **No Compose.** The binary ran directly. Like the Compose entrypoint, `forgetmenote migrate` ran before every command.
- **Server environment:**
  - `DATA_DIR`, `DATABASE_URL_FILE`;
  - `OUTGOING=paused`;
  - `PUBLIC_URL=http://127.0.0.1:18080` and `LISTEN_ADDR=127.0.0.1:18080`;
  - no OIDC, embedding endpoint or integrations.

  Reads used the recovery key (`data/admin.token`) as a bearer token, over HTTP (`/p/ID/api/…`, `/p/ID/assets/…`) and MCP (streamable HTTP).
- **The Postgres precondition.** Production's catalog lives in its Postgres, which the copy does not include. So the file-era catalog was loaded into the disposable database with `forgetmenote import-files`, as the earlier Postgres cutover did. Two changes were made to the work copy:
  - The copy has no `activity.jsonl`, so the catalog's `activitySequence` was set to 0.
  - For the main runs, N1 and N2, which the file-era catalog does not list, were added as plain notebooks owned by its admin, so that all five could be served. The refusal runs used the catalog unchanged.

## Commands and timings

The final run used work-3, the fixed binary and a read-only root filesystem. Times are wall-clock times inside the container on the development server, which also ran other workloads and agents' test runs.

| Step | Command (adapted to the copy) | Time |
| --- | --- | --- |
| Copy | `cp -a data-ro work-3/data` | 0.30 s |
| Disposable database | `initdb …`, `createdb forgetmenote` | 0.84 s |
| Precondition | `forgetmenote migrate` (all 18 migrations) then `forgetmenote import-files -data DATA` | 0.089 s + 0.135 s |
| §2 database dump | `pg_dump -U forgetmenote forgetmenote > backup/pre-v2/forgetmenote.sql` | 0.066 s (20.5 MB) |
| §2 bundles | `git -c core.hooksPath=/dev/null bundle create … --all` and `git bundle verify`, per notebook | 0.315 s for all (7.8 MB) |
| §2 archive | `tar -czf backup/pre-v2/data.tar.gz data` | 1.04 s (31.3 MB) |
| §4 claim, first | `forgetmenote` (serve) before the import | refused after 0.064 s |
| §3 import | `forgetmenote migrate && forgetmenote admin storage import` | 0.010 s + **1.474 s** |
| §3 import again | the same | 0.376 s, all `already imported` |
| §3 verify | `forgetmenote admin storage verify` | **0.321 s** |
| §3 export | `forgetmenote admin storage export` | **2.989 s** |
| Source vs export comparison | (harness, below) | 1.01 s |
| §4 start | `forgetmenote` (serve) until `/readyz` | 0.102 s |
| §4 checks | every file, note, history, MCP read, live read, sampled old revision, backup download | 45.8 s |
| Title searches | 75 searches | 2.6 s |
| Stop | SIGTERM | 0.007 s |
| Old vs new parity | the previous Git backend against the store, all five notebooks | 59 s (N5: 55.5 s) |
| Rollback inputs | restore the dump into a fresh database; unpack the archive | 0.27 s; 0.25 s |
| Backup watcher | `watcher.py --config … --once` per export | 0.06–0.24 s each |

Per-notebook import, verify and export, with the import's own report:

| Notebook | Commits | Trees | Blobs | Import | Verify | Export |
| --- | --- | --- | --- | --- | --- | --- |
| N1 | 1 | 1 | 0 | 0.013 s | 0.003 s | 0.015 s |
| N2 | 10 | 10 | 9 | 0.019 s | 0.003 s | 0.042 s |
| N3 | 1 | 1 | 0 | 0.006 s | 0.002 s | 0.014 s |
| N4 | 30 | 85 | 112 | 0.294 s | 0.028 s | 0.432 s |
| N5 | 133 | 1,044 | 1,770 | 1.131 s | 0.275 s | 2.474 s |

`notImported` was empty for every notebook, since no repository has other branches or tags. The first runs, with the base binary, took 1.56–1.77 s to import, 0.38–0.54 s to verify and 2.9–3.1 s to export.

The dump is large for an instance of this size because `import-files` brought over the file-era knowledge state, embedding vectors included.

## Results

### Import, verify and export

All five notebooks were `imported` with their head equal to Git `HEAD`. A second import reported `already imported` for each and verified them again. `verify` and `export` passed for each. The export command's own check ended every export at its repository's `HEAD`.

The harness then compared each source repository with its export, outside ForgetMeNote:

| Check | Result |
| --- | --- |
| `git rev-list --topo-order HEAD` (source) vs `main` (export) | identical lists for 5/5 notebooks (175 commits) |
| `git log --format='%H %T'`: each commit's tree | identical for 175/175 commits |
| Raw commit bytes (`git cat-file`) | identical for 175/175 |
| Full file list (`ls-tree -r`: mode, type, SHA, path) of **every** revision | identical for 175/175 revisions |
| All objects `HEAD` reaches (`rev-list --objects`), compared byte for byte | identical, 3,207/3,207 |
| Checkout of `HEAD` (`git archive`: path, mode, bytes) | identical for 5/5 |
| `git fsck --strict --no-dangling` | passes on all exports and all sources |
| Export refs | only `refs/heads/main`, and `HEAD` points to it |

Comparing the file list of every revision, and every object byte for byte, covers the brief's "HEAD and a sample of older revisions" completely.

### The server on the imported store

The server started in 0.10 s, logged `outgoing actions are paused`, and listed all five notebooks (`GET /api/projects`).

| Check | Result |
| --- | --- |
| `GET /api/status` revision = Git `HEAD`, not dirty | 5/5 |
| MCP `notebook_status` revision = Git `HEAD` | 5/5 |
| Every file at `HEAD` through `/assets/PATH`, byte for byte | 766/775 identical. The other 9 are refused as the previous backend refuses them: hidden paths and executable files ([parity](#previous-backend-vs-imported-store)) |
| Every note through `/api/note`: content | 720/720 identical |
| …its `revision` is `HEAD`; its `version` is `git log -1 --format=%H HEAD -- PATH` | 720/720; 720/720 |
| Every note through MCP `read_note` | 720/720 identical |
| Every note's `/api/history` contains every commit `git log HEAD -- PATH` lists | 720/720 |
| Each history entry's author, committer, emails, `authoredAt` (numeric offset, as production's Git prints `%aI`) and message, against the raw commit | 1,880/1,880 |
| Sampled older revisions (8 per notebook, evenly spaced): every file through `/assets/PATH?revision=` | 5,846/5,917 identical. The rest are the same policy refusals |
| …every note's `version` at that revision is `git log -1 REV -- PATH` | 5,514/5,514 |
| Live read (`/api/live`) of up to 5 notes per notebook: content = `HEAD` bytes, not pending | 9/11. The other 2 are `AGENTS.md`, which is not live-editable; the previous storage code refuses it the same way |
| `/api/backup` bundle: names `HEAD`, holds exactly `HEAD`'s commits | 5/5 |
| Whole-title search (`/api/notes?q=`, archives and drafts included) finds the note | 75/75 |

After the server stopped, `admin storage verify` found every head still equal to Git `HEAD`.

### Previous backend vs imported store

A temporary helper, never committed, opened each notebook twice in one process:

- through the previous release's Git backend (`workspace.Open` on the copy, the storage code still in this tree);
- through the imported store (`workspace.OpenDB`).

It compared what each returns, as JSON, error text included. With the fixed import:

| Operation | Compared | Differ | Refused by both |
| --- | --- | --- | --- |
| `Read` at `HEAD` (path, content hash, revision, version), every file | 775 | 0 | 9 |
| `Read` at up to 12 evenly spaced revisions, every file | 8,845 | 0 | 106 |
| `History`, every note | 720 | 0 | 0 |
| `Comments`, every note | 720 | 0 | 0 |
| `Browse`, every folder | 235 | 0 | 2 |
| `Find` and `FindIncludingTrash` (titles and title words) | 124 + 124 | 0 | 0 |
| `Snapshot` at `HEAD` and at the sampled revisions | 5 + 36 | 0 | 0 |
| `Status`, `List`, `ListCollection`, `Collections`, `ListTrash`, `TrashPolicy`, `Locks` | 35 | 0 | 0 |
| **Total** | **11,619** | **0** | |

With the base binary, one N5 `History` differed. That was the bug fixed below. Separately, `Browse` differed intermittently in the order of two notes whose titles differ only in letter case; see [Not fixed](#found-not-fixed).

### Old storage untouched

The harness hashed 2,673 files before the import:

- every file under each notebook's `workspace/`, `.git` included;
- `collaboration/`, with its 110 snapshots, 66 append logs and receipts;
- any `captures/` or `write-journal.json`.

It hashed them again after import, verify, export, serving every read above, and verifying again. **0 files changed.** The 110 snapshots and 66 append logs were all read by the import's live-edit check and found settled.

### Collaboration and live records

Production's release keeps live edits in `projects/ID/collaboration/` files, not in Postgres. The import read every snapshot and append log of the copy and refused none.

On a second copy, a live edit was accepted through the previous storage code (`LiveRead`, then `LivePatch` of an ordinary N5 note), leaving it pending in a version-2 snapshot and append log. The import refused N5 with `live edits are not checkpointed (PATH)`. After a `LiveCheckpoint` through the same code, the import took N5's new commit (134 commits). The server then served the checkpointed text, with the checkpoint commit as the note's `version`.

After the cutover, live reads create live documents in the store (9 `live_docs` rows after the reads above) and never touch `collaboration/`.

### Refusals

This ran on work-2, with the file-era catalog unchanged: N3 to N5 in the catalog, N1 and N2 outside it.

**S1, startup.**

1. Serving before any import failed in 0.07 s: `cannot read project catalog: notebook N3 is still the Git repository …/projects/N3/workspace/.git, which the notes store lacks: stop the instance and run forgetmenote admin storage import …`.
2. After `admin storage import -notebook` for N3, N4 and N5 only, the server **started** with N1 and N2 still un-imported Git repositories.

So startup refuses a *catalog* notebook the store lacks, not any repository under `data/projects`, as the runbook said. The runbook is corrected.

**S2, unsafe old storage.** This started from a fresh database. Three notebooks were changed:

- an untracked file in N3's working tree;
- a `write-journal.json` (`{}`) beside N4's repository;
- the pending live edit in N5 described above.

`admin storage import` exited 1 (`failed for 3 of 5 notebooks`). The refused notebooks were left out and the others imported:

| Notebook | Import line |
| --- | --- |
| N1, N2 | `imported` |
| N3 | `the notebook repository has uncommitted changes; commit or remove them first` |
| N4 | `an interrupted write is pending (write-journal.json); start the previous release once to recover it` |
| N5 | `live edits are not checkpointed (PATH); checkpoint them with the previous release first` |

- `admin storage verify -notebook` on N3, N4 and N5 failed with `file does not exist`: nothing of a refused notebook was stored.
- The server refused to start, naming N5.
- Then the runbook's remedies:
  - remove the untracked file;
  - remove the journal, standing in for the previous release's recovery;
  - checkpoint N5's live edit.

  The next import reported N1 and N2 `already imported` and N3 to N5 `imported`, each at its Git `HEAD`. The server started and served the checkpointed edit.

### Backup watchers on the exports

For each notebook, a bare mirror of the source repository stood in for the GitHub remote after the freeze push. `deploy/git-backup/watcher.py --once` ran with `repository` set to the export and `remote` set to the mirror.

- Each run ended `synced` at Git `HEAD`, with the mirror unchanged: already up to date.
- A note then saved through the API in N2 reached N2's export in the background.
- The next `--once` pushed it as a fast-forward from the old head.

### Search and knowledge

Store search (`/api/notes?q=`, MCP `search_notes`) works at once, and parity shows it identical to the previous backend.

`hybrid_search` ran in text mode, because semantic search was `unavailable` (no embedding endpoint, outgoing paused). Querying each note's title, it found the note for:

- 1/1 notes in N2 and 10/10 in N4;
- 1/10 in N5 by default, and 10/10 with `archives: include`. The nine misses are notes in archive containers, which the default policy excludes.

`knowledge_status` showed each notebook's semantic index as carried over by `import-files`:

- N4: 448/448 passages at its `HEAD`;
- N5: 2,041/2,235 at an older revision, because the file-era state predates N5's last commits.

Revisions keep their SHAs, so a carried-over index stays valid. It catches up only when embedding runs again, after the pause, which calls the embedding provider.

### Rollback inputs

- The dump restored into a fresh database with 0 notebook heads, revisions and live documents, against 5, 175 and 9 in the cutover database, and the same 4 `documents` rows.
- The archive unpacked to a data directory whose old storage matched all 2,673 hashed files. It had no `exports/` or `blocks/`.
- Each bundle names its repository's `HEAD`.

The previous release itself was not run on them (see [Not covered](#what-the-rehearsal-could-not-cover)).

### Large-notebook timing (F2)

The fix changes the import's Git stream from `rev-list` to `git log --name-status --find-renames`. So the full F2 round trip (`FMN_GITEXPORT_F2=full go test ./internal/gitexport -run '^TestF2RoundTrip$'`: 20,000 revisions, 2,000 notes, 200 images) ran at the base commit and with the fix, one after the other on the same host:

| Step | Base `c985618` | With the fix |
| --- | --- | --- |
| Import | 1 min 33.9 s | 1 min 23.8 s |
| Verify | 14.6 s | 11.5 s |
| Export | 1 min 18.6 s | 2 min 23.2 s |
| Repack and check | 43.6 s | 40.4 s |

The export code is unchanged, so its spread is the shared host's load. Reading renames costs the import nothing measurable.

## Issue found and fixed

**An imported note without an `fmn-id` lost its identity at a rename Git detects with edits.** As a result its history named a path that does not open at that revision.

- **Found by:** parity. One N5 note's `History` differed. Its history has a `Move "A" to "B"` commit and a later "Restore note from Trash" commit that moved it back with an edit.
  - The previous backend follows Git's renames (`git log --find-renames`) and names `B` for the move.
  - The import gave a new path the entry of a deleted file only for identical content or a declared `fmn-id`. So the restore started a new entry, and the store's history named `A` for the move, a path that does not exist at that revision. Clicking that history entry would fail.
- **Fix:**
  - `notestore.Notebook.ImportCommit` takes Git's rename pairs for the commit as an optional argument (`5b5f1a6`). An added file keeps the paired deleted file's entry unless its content declares another identity. A pair whose source the commit keeps, or never had, passes nothing on.
  - `gitexport.ImportGitDir` reads commits and their renames from one `git log --reverse --topo-order -z --name-status --find-renames --diff-merges=first-parent` stream instead of `rev-list` (`f60c92c`). That covers the cutover import and bundle imports.
- **Tests** (synthetic data, red before the fix):
  - `TestImportGitDirFollowsGitRenames` (gitexport): a note and an attachment moved with edits keep their entries; a note that declares an identity keeps it even where Git pairs it with a deleted file.
  - `TestStorageImportedHistoryFollowsRenamedNotes` (cmd): after `admin storage import`, a note moved to Trash and restored with edits has the history paths `plan.md`, `Trash/plan.md`, `plan.md`, and each entry opens at its revision. Before the fix: three times `plan.md`, and the second entry did not open.
  - `TestImportFollowsGitRenamePairs` (notestore): the argument's guard rails.
- **Confirmed on the copy:** with the fix, all 685 N5 histories match the previous backend.

## Runbook corrections

In [storage-cutover.md](../storage-cutover.md):

- **Startup refusal.** It is per catalog notebook. A repository under `data/projects` that the catalog does not list neither stops startup nor is served. The runbook claimed any repository there did.
- **What the import covers.** It imports every repository under `data/projects`, catalog or not. Other files, and notes without an `fmn-id`, keep their identity through the renames Git detects.
- **The search check.** It now says what works at once (store search, `hybrid_search` text) and what waits for outgoing actions (embedding catch-up, a provider cost), and that archive containers stay excluded by default.
- **Timings.** Now from this rehearsal and the F2 runs above.
- **The optional rehearsal.** It now says to restore both the dump and the data directory, to serve the copy on loopback only, and to run the step 4 checks.

[backups.md](../backups.md) gets the same identity sentence.

## Found, not fixed

**`Browse` order is not stable when two items' lowercased titles tie** (`internal/workspace/browse.go:79`). This is pre-existing, in code both backends share, and outside this stream's scope.

- **Cause:** `browseEntries` sorts with `sort.Slice`, which is not stable, by lowercased title and nothing else. Folder items are appended from map iteration, so the input order varies between calls.
- **Effect:** two N5 notes whose titles differ only in letter case swapped places between calls, in both backends (2 of 5 parity runs).
- **Suggested fix:** break the tie by path.

## What the rehearsal could not cover

- **Production's Postgres control plane.** The copy does not include it: the current catalog and the notebooks' real bindings, accounts and grants, knowledge state current to the freeze, receipts, sessions and the rest.
  - The rehearsal rebuilt a catalog from the file-era file and added the two notebooks it lacks as plain notebooks. Catalog bindings such as private assistant or memory notebooks were therefore not exercised.
  - The migration ran over an empty schema rather than over production's (through 0016). Migrations 0017 and 0018 only create tables.
- **Live records in production Postgres.** Production's release keeps notebook live edits in `collaboration/` files, which the copy includes and the import checked. Postgres state that refers to notebooks was not in the copy: chat citations, memory anchors, knowledge references and operation receipts. Their revision SHAs stay valid because the import keeps every SHA, but this was not checked against production rows.
- **The previous release itself.**
  - The live-edit remedy used the previous storage code still in this tree, not the deployed image.
  - The journal remedy only removed a synthetic `{}` journal.
  - Rollback ran up to restoring the dump and unpacking the archive; the previous image was not started on them.
- **Compose and production's images.** The binary ran directly with the Compose entrypoint's `migrate` step and a read-only root filesystem, on Postgres 16 and Git 2.43. Compose uses Postgres 17.6, and the production image uses Debian bookworm's Git.
- **Pushing to GitHub.** No network, so a local mirror stood in for the remote; see [Backup watchers on the exports](#backup-watchers-on-the-exports). The managed backup worker and SSH credentials were not exercised.
- **Semantic search and S3 blob storage.** Semantic search was off (no embedding endpoint) and the blob store was the data directory.
- **The hosted instance.** Its cutover is assessed separately (design §12).

## Reproducing

The harness and the two temporary Go helpers were kept outside the repository and the worktree. They printed only labels, counts, timings and match results.

- The harness copied the data and ran each step above in order, inside the container, with the commands listed.
- The parity helper and the live-edit helper were built from this tree against `internal/workspace`, `internal/notestore`, `internal/pg` and `internal/blob`.

To rerun on fresh copies:

1. Copy the data and serve it with nothing published, as in [Setup](#setup-and-deviations-from-the-runbook).
2. Run the runbook's commands in the order of the timing table.
3. Compare as described in each section.
