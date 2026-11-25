# Storage v2: MCP contract audit results

- **Status:** done; no regressions found, five differences classified as intended, one test added for the one spelling the goldens cannot see
- **Date:** 2026-11-22
- **Plan:** [storage-v2.md](storage-v2.md) §4.2 (interface contract) and §8 (reads); [wave 2 plan](storage-v2-wave2-plan.md) §0.9 (commit time spelling)
- **Code:** `internal/web/mcp_contract_test.go` and `internal/web/testdata/mcp-contract/`

The MCP contract suite is the compatibility boundary for the agents of existing instances and Claude sessions. It was recorded on the Git backend at `bdb35fe` and has changed since. This audit:

1. classifies every change to the suite since that recording;
2. adds cases for what storage v2 rewrote, recorded first on the Git backend;
3. runs everything on the notes store (database notebooks) and classifies each difference.

**Result.** The 309 steps recorded before this audit match on database notebooks, except the one step `d65e30f` accepted. Of the 252 new steps, 229 match exactly. The other 23 show five differences, none of them a regression. No code needed fixing.

## How the new cases were recorded

1. **A temporary worktree on the Git backend.** `git worktree add --detach …/contract-pre 3196c3e` checked out the last commit that served notebooks from Git (the wiring merge `9a92d37` switched the app to the notes store), followed by `npm ci` and `npm run build` there.
2. **Record there.** The extended `mcp_contract_test.go` was copied in, then `go test ./internal/web -run '^TestMCPContract$' -count=1 -update-mcp-contract`. Every earlier golden re-recorded byte for byte, including `tools.json`, so the extension changes nothing the suite already recorded. Three more runs without `-update` passed, so the new goldens are stable on Git.
3. **Commit the Git answers.** The goldens were committed unchanged (`deea1a2`).
4. **Run on the notes store.** The same test ran in this worktree; five goldens differed (`ea1e39f`).
5. **Clean up.** The temporary worktree was removed with `git worktree remove --force`; it held the copied test file and its `node_modules`.

Both runs used the `fmn-dev-go` image (Git 2.54, `TZ=UTC`).

## 1. Changes to the suite since the recording

| Commit | Change | Classification | Evidence |
| --- | --- | --- | --- |
| `89acafd` | Mismatches print a line diff | Test output only | No golden changed |
| `c0fd622` | `<TMP>` now replaces the fixture's data directory (and its symlink-resolved spelling) instead of a regexp over `os.TempDir()/TestMCPContractNNN`; masking no longer reaches past a missing key; flag renamed `-update-mcp-contract`; mismatches name a non-UTC zone | Normalization only; identical behaviour | One golden line changed, `structure.json` "moving a missing note": `stat <TMP>/001/projects/…` became `stat <TMP>/projects/…` because `<TMP>` now ends one directory deeper. The server's text is the same on both backends; the notes store builds that `stat` error on purpose to keep it (`workspace/db_mutate.go:70`). |
| `ce82adf` | New `resolve_notebook_link` and `find_notebook_link_targets` cases in `notes`, `structure`, `locks` and `trash`; both tools move from deferred to called in `tools.json`; `set_task_queues` gets a deferral reason | Identical behaviour | Recorded on the Git backend during wave 1. They pass unchanged on the notes store and re-recorded byte for byte at `3196c3e`. |
| `e14ad39` | The backup is exported in three chunks, so a mid-stream `nextOffset` (5120) is recorded | Identical behaviour | Recorded on the Git backend and passes unchanged on the notes store |
| `6c31356` | `contractTime` reads `+00:00` as `Z` | **Accepts two behaviours.** A documented decision. | Production's Git (Debian) prints `+00:00`, while Git 2.54, which recorded the goldens, prints `Z`. Storage v2 keeps production's spelling ([wave 2 plan](storage-v2-wave2-plan.md) §0.9, `workspace/db_commit.go:244`). Because the normalizer equates the two, the goldens cannot catch a drift to `Z`, so `TestMCPContractCommitTimes` (`f502e7e`) now checks the raw `authoredAt` of `note_history` and `read_project_changes`. It passes on the notes store and fails at `3196c3e` on Git 2.54. |
| `d65e30f` | `notes.json` "read at an unknown revision": `git ls-tree failed: exit status 128` became `revision REV7 is not in this notebook's history` | Improvement | See [D1](#d1-unknown-revisions-are-named-not-a-leaked-git-failure) |

**Places the suite accepts two behaviours.** There are only two:

- **Two zone spellings,** `+00:00` and `Z` (`6c31356`, above).
- **Two spellings of the data directory** (`c0fd622`). These are spellings of one directory, not two server behaviours.

The masked fields are unchanged since the recording: the bundle bytes, `total` and the final `nextOffset`, the imported bundle data, and `capabilities.instance`. The new `export` scenario masks the same chunk fields and the chunk's `sha256`. Its checks then compare what the bundle contains.

## 2. What the suite covered, and the new cases

`tools.json` lists 166 tools:

- **78 called.** The new cases call only these, so `tools.json` does not change.
- **29 deferred, each with its reason.** The ones that reach notebook storage are tested elsewhere on database notebooks:
  - the task tools: `internal/web/tasks_test.go`, `internal/workspace/task_history_test.go`;
  - the authority preview, apply and cancel tools: `internal/web/authority_test.go`;
  - the source jobs: `internal/web/sources_test.go`; `archive_source` covers the same commit path in the suite;
  - the assistant memory tools: `internal/web/agent_memory_test.go`.

  The repository-notebook tools are read-only upstream snapshots outside notebook storage.
- **59 not called.** These are instance control-plane tools: accounts, credentials, groups, AI and assistant policy, automations, connectors and GitHub connections. Their state lives in Postgres control tables or the catalog, not in notebook revisions, and storage v2 did not change them.

**Capture receipts** are now TTL-swept notes store receipts. No MCP tool reaches them: Quicknote filing runs through `internal/quicknotes`. They are covered by:

- `TestReceiptsAreBoundToTheirRequestAndSwept` (`internal/notestore`);
- `TestCaptureReceiptsBoundOnlyFilingsInFlight` and its neighbours in `internal/workspace/db_capture_test.go`.

The retries that MCP does make keep their replay:

- a change group is found again by its `Gitnotes-Operation` commit, which is never swept. The new case "retrying an applied change group after later commits replays it" covers this.
- a live patch is found again through its live receipt.

### New scenarios

All new scenarios run after `backup`, on notebooks of their own (`contract-history`, `contract-limits`):

| Scenario (golden) | Steps | Covers |
| --- | --- | --- |
| `renames` | 42 | History of a note with and without an fmn-id through renames, a folder move, a chain of renames, a copy, Trash and a restore to a new path, and a deletion through a change group. The vacated and reused paths. Each note's `version` after a move, a link rewrite, a copy and a restore. |
| `livereads` | 43 | `read_note`, `read_document`, search, status and history while live edits are pending. Saves with and without `requireCheckpoint`. A patch retried after its edits were committed. Moving and trashing a note with pending edits. A stale base after a move. Live reads at the vacated path, in Trash, after a restore and on a locked note. Roles. |
| `cursors` | 24 | `read_project_changes` across a rename, a folder move, Trash and a restore, a deletion, a reversed range, a foreign revision and an unknown revision; snapshots at old revisions; consumer checkpoints at older, unknown and foreign revisions; a delete and an add of the same text in one group; a change group retried after later commits. |
| `discussions` | 26 | Discussions across a rename, a copy, Trash and a restore. Replies in a locked folder. Trash refused in a locked folder. Locks for missing paths. Unlocking an unlocked path. Comments on missing notes and threads. Emptying an empty Trash. |
| `unknowns` | 73 | Unknown and foreign (another notebook's head) revisions across 19 tools that take a revision, and the all-zero, abbreviated and upper-case spellings on `read_note` (`cursors` covers `read_project_changes` and `complete_workflow`). Path spellings (`./`, `//`, a leading `/`, `..`, a different case). Missing paths across 11 tools. The `/api/v1` status, code and message of 10 of those calls. |
| `derived` | 15 | Browse, collections, link targets, backlinks, hybrid and history search after the moves; knowledge reads (`read_document`, `document_outline`, `list_documents`, `notebook_map`, `find_term_mentions`, `resolve_notebook_link`, `read_attachment`) at old revisions. |
| `limits` | 14 | The 50-commit limit of note and notebook history over 52 revisions. 51 commits in a changes-since listing, not truncated. An older revision as the base of a save, comment, properties change, lock, move and Trash. |
| `export` | 15 | Export the audit notebook, then compare: the bundle's head with `project_status`; every commit object, with author and committer times replaced by `<EPOCH>`; `git ls-tree -r`; `note_history` against `git rev-list`; and each note's `version` against `git log -1 -- path`. Re-import the bundle, then read history, discussions and a save in the restored notebook. |

## 3. Differences on the notes store

The 23 differing steps fall into five differences, D1–D5 below.

### D1. Unknown revisions are named, not a leaked Git failure

- **Steps (11, plus `d65e30f`'s one).** `read_note` at an unknown, all-zero or foreign revision (3), `read_attachment` and `restore_note` at an unknown or foreign revision (4), and the four matching `/api/v1` checks.
- **Git:** `git ls-tree failed: exit status 128`.
- **Notes store:** `revision REV is not in this notebook's history` (`workspace/db_tree.go`, `unknownRevision`).
- **Classification: improvement.** The error class is unchanged: it is still an MCP tool error, and `/api/v1` still answers 400 `invalid_request` on both backends. The design keeps error classes stable (§4.2); the old text was a subprocess failure, not a message.
- **Unchanged:** every other tool that takes a revision refuses an unknown or foreign revision exactly as Git did:
  - conflict for each write;
  - `this path is not accessible` for `read_document`, `document_outline` and `resolve_notebook_link`;
  - the notebook-changed conflict for `list_documents`, `notebook_map` and `find_term_mentions`;
  - `snapshot commit is unavailable` for snapshots and changes.

  A foreign revision is never found in another notebook.

### D2. History follows a note through Trash

- **Steps (2).** `note_history` at a trashed note's `Trash/…` path, and after `restore_trash` with a new `to` path.
- **Git:** only the move into Trash, or only the restore. Its history follows `Move %q to %q` subjects and renames, and stops at `Move notes to Trash`.
- **Notes store:** the note's whole history (creation, link rewrites, renames, the folder move, Trash and the restore), each entry with the path it had then.
- **Classification: improvement, by design.** History is a query on the entry ID, and "renames follow automatically through the stable ID, with no rename detection" ([design](storage-v2.md) §8; wave 2 plan group B).
- **Unchanged:** a restore to the original path already showed the whole history on Git ("history across live edits, a move, Trash and a restore" matches). The notes store now gives the same answer for a restore to a new path.

### D3. Changes since a cursor report a rename by identity

- **Step (1).** `consumer_checkpoint`, changes since the folder-move revision.
- **Git:** `chapters/beta-renamed.md` deleted, `chapters/beta-restored.md` added. The note was renamed through Trash, and its link was rewritten, Git rates the two texts 11% similar, under the 50% threshold of `diff --find-renames` (`workspace/snapshot.go`). Running `git diff --find-renames=10%` on the same two texts reports `R011`.
- **Notes store:** `renamed` from `chapters/beta-renamed.md` to `chapters/beta-restored.md`.
- **Classification: improvement, by design** (§8, as in D2). `workspace/db_read.go` `gitDiff` restates the store's per-entry changes in Git's diff shape.
- **Unchanged:**
  - The kinds and fields are the same; a `renamed` entry could already carry edits on Git (any rename above 50% similarity).
  - Renames that Git detected, and the delete-and-add of the same text in one group, match.

### D4. A new live generation keeps the store's update sequence

- **Steps (8).** Every live read or patch after a move or a restore.
- **Git:** `liveRevision` `<new generation>:0`.
- **Notes store:** `<new generation>:2` and `:3`. A move or restore still starts a new generation. Its sequence is the notes store's per-note live update sequence (`workspace/db_live.go`, `liveStart`: `Sequence: st.SnapshotSeq`), which does not restart.
- **Classification: no contract change.** `liveRevision` is an opaque base ([collaboration](../collaboration.md): "base: <liveRevision from read_live_note>"). Both backends:
  - refuse a base from the old generation ("live base expired or was replaced");
  - accept the new base;
  - refuse patches to a locked note.

### D5. The export's checkpoint receipt carries that live revision

- **Step (1).** The export's commit objects: one `Collaboration-Receipt` trailer says `"revision":"<gen>:3"` where Git said `:1`, following D4.
- **Classification: follows D4.** Every other part of all 35 commit objects matches, apart from times:
  - tree SHAs;
  - parents;
  - author and committer identities with `+0000` zones;
  - subjects;
  - the `Gitnotes-Source`, `-User`, `-Client`, `-Operation` and `-Request` trailers.

  So do the exported tree (`.gitnotes-locks.json`, `.gitnotes-trash.json`, `comments/…`, `assets/…` and the notes), `note_history` against `git rev-list`, and every note's `version` against `git log -1 -- path`. A save in the re-imported notebook follows the imported head.

## Identical on both backends, recorded for later

None of these are regressions: each is now pinned by a golden. They may be worth a deliberate, additive contract change later.

- **`move_notes` and `duplicate_notes` of a missing note** answer `stat <data>/projects/<id>/workspace/<path>: no such file or directory` (`/api/v1` 404 for `move_notes`). That leaks a server path, which on the notes store is synthetic.
- **The same unknown revision is classed differently by tool:**
  - `read_note`: `invalid_request` (400);
  - `read_document`, `document_outline` and `resolve_notebook_link`: forbidden (403);
  - `list_documents`, `notebook_map` and `find_term_mentions`: conflict (409). These three only read the head, so any older revision conflicts too.
- **`set_note_lock` accepts a path that does not exist** and records a lock rule for it.
- **A note deleted through `submit_change_group`** goes to Trash with `deletedBy` set to the account ID. `trash_notes` uses the `@handle`.
- **Live edits:**
  - `move_notes` with pending live edits conflicts: it checkpoints first, which moves the head past the caller's revision. `trash_notes` with pending edits succeeds and keeps them.
  - `save_note` from the committed revision conflicts and checkpoints the live edits; with `requireCheckpoint` it is refused as `live edits are waiting for a Git checkpoint`.
- **Path spellings are refused as inaccessible:** `./`, `//`, a leading `/`, `..` and `./` inside. A different case is "file does not exist". `note_history` of a missing note or folder is an empty list; `chapters/` and `./chapters` are refused as inaccessible.

## Final runs

On `v2/contractaudit` at `f502e7e` (the goldens of `ea1e39f`):

- `fmn-go … go test ./internal/web -run '^TestMCPContract' -count=3 -v`: `TestMCPContract`, `TestMCPContractHelpers` and `TestMCPContractCommitTimes` passed in all three runs (9/9).
- `fmn-node` (`FMN_IMAGE=fmn-dev-full:local`) `go test ./internal/web -count=1 -v`: `ok` in 96 s; 842 top-level tests passed, 0 failed, 4 skipped.

At `3196c3e` with the same test file:

- `-update-mcp-contract`: every earlier golden re-recorded byte for byte.
- `-count=3` without `-update`: passed.
- `TestMCPContractCommitTimes` fails there, as it should on Git 2.54: `note_history authoredAt = "…Z", want … +00:00`.

## Adapter changes

Stream `p2adapters` (audit §5, "Define each operation once; make transports adapters").

### What changed

- **One definition per operation.** Each notebook operation the browser and MCP both offered is now one function, `func(ctx, store, in) (out, error)`. The browser route (`notebookJSON`) and the MCP tool (`addProjectTool`, which also feeds the action API and chat) are thin adapters around it. This covers:
  - Trash: list, trash, restore, empty.
  - Comments: list, add, resolve, delete.
  - Live notes: read, patch, checkpoint. The browser's long poll keeps its own handler around the shared read.
  - Repository sources: status, refresh, tree, map, file, retain citation, changes, index configuration.
  - Retrieval (formerly `internal/knowledgehttp`, now removed): retrieval settings (read and set), semantic status, snapshot, changes, indexing passages, embeddings, semantic search, hybrid search.
  - Knowledge review: semantic configuration, instructions (read and write), note sources, review inbox, findings, finding and proposal decisions, change groups (apply and propose), consumer checkpoint, complete workflow.
  - Notes: read and save.
- **Order of checks, the same on every transport.**
  1. Authorize the caller for the notebook.
  2. Validate the input.
  3. Check authority, existence and revisions.

  A caller who may not use the notebook learns nothing about its state.
- **Browser requests are authorized again after the body is read.** Route selection authorizes before the body arrives. MCP authorizes after decoding its arguments, and the browser routes above now do too, immediately before the operation runs.
  - Before this, only comment deletion rechecked. Against the base commit, a slow request body trashed a note even though the caller's owner role was revoked while the body was being read.
  - Browser routes that this stream did not convert (for example move, duplicate, metadata, container, locks and upload) still authorize only at route selection.
- **Typed MCP errors.** A failed tool call whose error the server classifies now carries the same classification the browser gets as its HTTP status, under `_meta["forgetmenote/error"]`: `{"status": 409, "code": "conflict"}`. A capacity refusal adds `"retryable": true` (`{"status": 429, "code": "capacity", "retryable": true}`).
  - The message text is unchanged.
  - Unclassified errors carry no type.

### MCP contract differences

Each difference below is deliberate. The first three change recorded case goldens; `tools.json` (tool names and schemas) is unchanged.

| Tool | Before | After | Why |
| --- | --- | --- | --- |
| `read_note` with a non-Markdown path (`notes.json`, "read_note refuses a non-Markdown path") | `this path is not accessible` (forbidden) | `notes are Markdown (.md) files; read media as an attachment`, typed 400 `invalid_request` | The path is accessible through `read_attachment`. Calling it a permission failure was wrong, and the browser already answered 400. |
| `save_note` with a non-Markdown path (`notes.json`, "save_note refuses a non-Markdown path") | `this path is not accessible` (forbidden) | `notes must be Markdown (.md) files`, typed 400 `invalid_request` | The same drift: invalid input, 400 in the browser. |
| `review_inbox` (`comments.json`, `changes.json`) | `{checkpoints, findings, proposals}` | adds `completionMeaning: "Reported by the external consumer; not independently verified."` | The browser's review inbox always qualified the checkpoints it lists; MCP clients now get the same qualification. Additive. |
| `save_note` from a caller without Edit who passes a stale `authorityGeneration` | `the notebook changed; reload and reconcile your draft` (conflict) | `this path is not accessible` (forbidden) | The fence ran before authorization, so anyone could probe a notebook's authority generation. The browser already authorized first. |
| Every classified tool error | text only | text plus `_meta["forgetmenote/error"]` `{status, code}`; capacity adds `retryable: true` | Clients can tell a retryable capacity refusal (HTTP 429) from invalid input or a conflict without parsing text. Additive. |
| `repository_source_file` with a partial or oversized line range (for example only `startLine`, only `cursor`, or more than 200 lines) | forbidden (`reposource.ErrDenied`) | `startLine and endLine are required together and select at most 200 lines`, typed 400 `invalid_request` | Invalid input, not a permission failure. The browser already refused a partial range with 400. |
| `repository_source_status` | the raw source state, or `{configured: false}` | the browser's shape: the state plus `configured`, `installUrl` and `limits`, or `{configured: false, connected: false}` | One status definition. Additive. The tool is deferred in the contract suite. |
| `submit_change_group` with an unknown mode; `review_decision` with an unknown kind | untyped error | the same text, typed 400 `invalid_request` | Validation errors are typed on every transport. |

### Browser API differences

- An out-of-range or malformed `wait` on `GET /api/live` is now 400 `invalid_request`; it was a 500.
- Changed messages:
  - a non-Markdown `GET /api/note` or `POST /api/note` uses the messages above (still 400);
  - a malformed integer query parameter says `<name> must be an integer` (still 400; this replaces `Invalid map limit`).
- An oversized repository line range is now 400 (it was 403). A cursor without line bounds is still 400.
- A `project` field in a request body, or a `project` query parameter, must name the notebook in the URL; a mismatch is 400.
  - Before, a body with `project` was refused as an unknown field, except by `POST /api/note`, which ignored it.
- The consumer checkpoint's `completionMeaning` is now spelled like MCP's: `Externally reported, not independently verified.`
- Repository routes check that the repository manager is configured (503 when it is not), as MCP did. Before, they checked only the GitHub connection.

### Not changed here (open)

- Several engine and workspace errors are invalid input or capacity limits, but they are plain errors. The browser answers them with 500 and MCP leaves them untyped. Examples:
  - `embedding configuration changed; request current passages`;
  - `live notebook capacity reached; …`;
  - `live subscription limit reached`;
  - `Trash index capacity reached`.

  Typing them needs sentinels in `internal/knowledge` and `internal/workspace` and in `webcore.ErrorOf` (the `p2respond` sweep).

## Trash links, permanent limits and Trash archives

Stream `trashlinks`. Each difference below is intended. No recorded answer in `internal/workspace/testdata/recorded` changes: they have no move that a note in the Trash blocks, their locked-note case ("a locked backlink") is unchanged, and below its limits the Trash index is written exactly as the Git backend wrote it.

| Behaviour | Git backend (`14efbd3`; `next` alike) | Notes store | Why |
| --- | --- | --- | --- |
| A move or rename whose link rewrites reach a note in the Trash | Refused: `updating links would edit locked note Trash/<id>.md; unlock it first` (`move.go:140`; `editable` excluded every Trash path) | The Trash note's links are rewritten like any other note's. They resolve from its Trash path, as the Trash move and the restore rewrite them, so a restored note reaches the new locations. | The maintainer's decision (option a). In F2, notes in the Trash and locked notes together blocked every folder move. Still refused: a read-only notebook, a lock rule over the Trash path or over the location the note was trashed from (`updating links would edit locked note <original>, now in the Trash as Trash/<id>.md; unlock it first`), and a scoped chat action, whose corpus leaves out the Trash. Organization moves keep skipping notes in the Trash, as on Git: their link edits must fit the run's selected scope, which never holds the Trash ([organizing notes](../organization.md)), so a note restored after one keeps a link to the old path. An organization duplicate whose Trash copy a later move rewrote no longer undoes (a conflict; restore it from the Trash), where on Git that move was refused. |
| A Trash index with 10,000 records or 8 MiB | Every further move to the Trash refused (`Trash index capacity reached`), even right after Empty Trash, because emptied and restored records stay as tombstones | The tombstones move to numbered archives (`.gitnotes-trash-1.json`, …, same format) that history search (through `TrashPolicy`), the Trash walk of a restored note, purge, bundle imports and handoffs read; opening a notebook, a bundle import and a handoff refuse archives that do not parse, hold a note still in the Trash, leave a gap in their numbering, have no index beside them or repeat a record. Only notes in the Trash fill the index: `Trash index capacity reached; empty the Trash or restore notes first` (429, retryable), or `Trash transaction exceeds the Trash index; move fewer notes at a time` (400) for one request too large for an empty Trash. | Empty Trash must keep working for the life of a notebook. No tombstone could be dropped: an emptied note's keeps its old versions out of history search, and a restored note's keeps its history when it is trashed again. |
| `note history exceeds Trash capacity`; a fresh change set over 10,000 paths | Plain errors (the wsfix stream made them 429 capacity, retryable in MCP) | `workspace.ErrLimit`: 413 `too_large`, not retryable | The limit belongs to the note or the notebook; no retry can succeed. A change set from a checkpoint stays 400 (`use a fresh snapshot`). |
