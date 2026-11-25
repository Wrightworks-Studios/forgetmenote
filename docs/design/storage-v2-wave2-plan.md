# Wave 2 plan: `*dbNotebook` backend (coordinator-approved)

Source: a read-only Plan agent's map of `storage-v2` @ `1bd36b8`, plus the coordinator's decisions (§0). Paths are repo-relative.

## 0. Coordinator decisions (binding)

1. **Shadowing guard.**
   - `dbNotebook` embeds `*Store`. Shared `*Store` helpers that call `s.read`, `s.head`, `s.gitRaw` or `s.snapshot` would still run Git, with an empty `cmd.Dir` that falls back to the process's current directory.
   - The skeleton makes `git`/`gitInput` (`workspace.go:176/179`), `gitRaw` (`:411`) and `readBlobsBounded` (`snapshot.go:217`) **return an error when `s.notes != nil`**.
   - `db_*` code never calls a Git-touching `*Store` helper; it writes a `*dbNotebook` twin. Only the owning group defines a given twin.
2. **Fencing.** For DB stores, `checkAuthority` (`authority.go:61`) compares against `s.authorityGeneration`, not `s.authority.Generation`.
3. **Notebook authority handoff (ForgetMeNote ↔ GitHub) is a user-facing feature and is KEPT.**
   - Users: `internal/notebooks/authority.go`, `web/authority.go` (MCP `notebook_authority_status`, preview/apply/cancel), `catalog/authority_transfer.go`, `reposource/handoff.go`.
   - Group **H** therefore **ports** `AuthorityStatus`, `PrepareAuthority`, `StageAuthority`, `FinishAuthority`, `NotebookSnapshot` and `ValidateNotebookSnapshot` to the DB backend. Authority state moves from `authority.json` to Postgres (a small additive table or a notestore record).
   - Only the **remote-owner** machinery is deleted: `remote.go`, `remote_notebook.go`, the owner RPC in `owner_methods.go`, `owner_import.go`, `NewOwner*`, the `owner` command, `OWNER_MAP_FILE`, `catalog/move.go` (move-notebook between owners) and `RemoteProjects`/`LocalVolume`.
4. **Additive notestore APIs**, added in wave 2a by the skeleton agent and recorded in `v2-interfaces.md`:
   1. Find a commit by `Gitnotes-Operation` trailer: an indexed `operation_id` column, filled on Apply and on import.
   2. A `RequestID` field on `ChangeSet`, written as the `Gitnotes-Request` trailer, with replay comparison.
   3. List receipts by kind.
   4. `HistoryQuery` by path prefix and for the whole notebook, with a limit.
   5. Message search (`MessageContains`) for TaskHistory's `Task t-…`.
   6. `PathVersion(rev, path)` counting deletions, the equivalent of `git log -1 rev -- path`.
5. **Revision channel.** `catalog/owners.go:144` announces bare notebook IDs on `notebook_revisions`. notestore uses the same channel, and the **payload is the bare notebook ID**; listeners re-read the head. The catalog's remote-owner listener (`owners.go:152-176`) is removed with remote owners in c1.
6. **`Root`** stays a non-Git state path `<data>/projects/<id>/workspace`, because knowledge uses it as a cache key and directory (`storage.go:44`, `index.go:145`, `map.go:224`). `OpenDB` takes the state directory. `web/runtime_config.go:22-34` must not require that the directory exist.
7. **Live engine:** decided by the D1 bake-off. Group F owns `db_live.go` and `liveengine.go`.
8. **Backups:** `deploy/git-backup/watcher.py:58` needs a working tree (`--show-toplevel`), so group G adapts it to the bare export repositories.

9. **Commit time format.** `Commit.AuthoredAt`/`CommittedAt` and every other ISO time derived from commits are formatted the way production's Git (Debian bookworm) prints `%aI`/`%cI`: Go layout `2006-01-02T15:04:05-07:00`, numeric offset, never `Z`. Git 2.54 prints `Z` for UTC; the contract tests accept both spellings, but production output must not change.

## 1. Method → group map

Notation:
- **S** skeleton;
- **A** read;
- **B** history;
- **C** mutate;
- **D** trash, locks and comments;
- **E** capture, sources, instructions and findings;
- **F** live;
- **G** export, backup and import;
- **H** authority (port).

| Group | Methods (current file:line) | Git used today | Reusable pure code |
| --- | --- | --- | --- |
| **S** | Read `workspace.go:312` → readRevision :359 → readCommitted :382; Save :428 → save :451 → applyChanges → commitChanges `group.go:291` → commit `commit.go:11`; Status :543; Close :163; Watch `watch.go:7` | ls-tree, cat-file, log -1, status, merge-base, add, commit -F - | MaxReadBytes, `committed_reads.go`, validateContainerWrite, watchChanges and changed |
| **A** | List/Find `workspace.go:514/575` → committedListing `listing.go:24`; ListScoped/FindScoped/BrowseScoped `content_scope.go:105/111/172`; Browse/BrowseCollection `browse.go:31/34`; Collections/ListCollection `containers.go:181/146`; Snapshot/SnapshotScoped `snapshot.go:83`/`content_scope.go:98`; ChangesSince `snapshot.go:266`; AssertSnapshot :360; KnowledgeRead `instructions.go:10`; HistoryVisibilityID/VisibleHistoryPaths `history_index.go:17/211` | ls-tree, cat-file, grep, merge-base, diff --name-status | filterFoundEntries, cloneEntry, ParseMetadata, NoteSummary, ContentScope.*, browseEntries, InCollection, ParseContainer, ContentHash, visibleFile, historyVisibilityID |
| **B** | History/HistoryScoped `workspace.go:539`, `history.go:166` → committedHistory :179 → scopedHistoryRevision :13 (follows `Move %q to %q` subjects :97 plus R status); HistoryRevisions `history_index.go:31`; HistoryNotes :87; TaskHistory `task_history.go:15` | log, cat-file, diff-tree | EntryID follows renames; imported history still needs the subject parse |
| **C** | Move/Duplicate `move.go:33/40` → transfer :45; Organize/OrganizationReceipt/OrganizationSources `organization.go:112/103/81`; UndoOrganization `organization_undo.go:31`; ApplyGroup/GroupReceipt `group.go:38`, `instructions.go:38`; ValidateMutationGroup `mutation_scope.go:234`; SetContainer `containers.go:62`; SetMetadata `metadata.go:297` | ls-files, ls-tree, show, diff-tree, log --grep | movedPath, rewriteLinks, rewriteCover, rewriteSourceOriginal, NewNoteID/WithNoteID, commentPath, organization helpers, undoOperation, MutationScope. **Moves must pass `FileChange.From`.** |
| **D** | TrashPolicy/ListTrash `trash.go:165/174`; TrashNotes/RestoreTrash/EmptyTrash :365/:424/:487 (prepareTrash :203, stageTrash :238, stageTrashDiscussion :314, trashHistoryFrom :634); FindIncludingTrash :561; Locks/SetLock `locks.go:123/136` (loadLocks :31); Comments/AddComment/ResolveComment/DeleteComment `comments.go:51/72/138/162` | log --raw --find-renames -100001, show, ls-tree | cloneTrashIndex, validateTrash, stageTrashIndex, lock policy JSON, commentPath, anchors, saveComments. `TrashRecord.History` is file format read by `knowledge/trash.go:34`. **Ship prepareTrash, stageTrash and stageTrashDiscussion first, because C needs them.** |
| **E** | Capture* `capture.go:341..609` (records in `<data>/captures` move to notestore receipts of kind `capture`); ImportSource/VerifySource `sources.go:103/317`; WriteInstructions `instructions.go:21`; AddFinding/Findings/DecideFinding `findings.go:49/111/164` | log --grep, merge-base, ls-tree comments | validCaptureMove, captureKey, captureOperation, checkCaptureChanges/Lock, ParseSource, SourceURL, source_formats; uses A's `list` and D's Comments |
| **F** | LiveRead/LivePoll/LiveApply/LivePatch/LiveCheckpoint `collaboration.go:314/329/416/514/782`; WaitLive `live_wait.go:28`; RollbackLiveStorage (delete) | log --grep, show; files under `<data>/collaboration` | `collaboration_wire.go`, liveDoc/seedLive, utf16Slice, liveSnapshotBase, liveCheckpointIdentity, trimLive. Keep `s.live[path].Uncommitted` and LiveRevision `<generation>:<seq>`. |
| **G** | Backup/BackupLimited `workspace.go:558/622`; ImportBundle `import.go:27`; internal/gitexport; cutover import/verify tool; watcher.py | bundle create, bundle verify, fetch, fsck | notebookFile, MaxBundleBytes, ErrBundle* |
| **H** | AuthorityStatus/PrepareAuthority/StageAuthority/FinishAuthority/NotebookSnapshot `authority.go:137/142/179/203/310`; ValidateNotebookSnapshot :358 (today it commits into a throwaway Git `Open` :427-438; rewrite it on the pure lock and trash parsers) | — | keep the NotebookSnapshot type, MaxAuthorityBytes, WithAuthority, SetAuthorityGeneration, ErrOwnerUnavailable |

## 2. Skeleton (wave 2a) must provide

**Store struct fields.**
- Add `notes *notestore.Notebook`, `noteStore *notestore.Store`, `notebook string`.
- Keep the cache and live fields.
- Delete remote, lock and journal at c2.

**Commit construction, reproduced exactly (`commit.go:11-35`):**
- **Subject:** the summary with `\r` and `\n` replaced by spaces.
- **Message:**
  1. `subject + "\n\nGitnotes-Source: " + Source`
  2. then `"\nGitnotes-User: " + ID` if set
  3. then `"\nGitnotes-Client: " + Client` if set
  4. then `"\nGitnotes-Operation: …\nGitnotes-Request: …"` when the context has `operationKey{}` (`group.go:36`)
  5. then `"\n" + audit`, the `Collaboration-Receipt: <json>` lines (`collaboration.go:650-662`).
- **Cleanup** as `git commit --allow-empty -F -`. Mind the edge case where Source is empty.
- **Identity:**
  - author = `attribution.From(ctx)`, default ForgetMeNote/system;
  - committer = `ForgetMeNote <forgetmenote@localhost>`;
  - checkpoints use `ForgetMeNote collaboration`, source `collaboration`.
- **Subjects stay byte-identical:**
  - `Update <path>`
  - `Move %q to %q`, `Duplicate %q to %q`
  - `Move notes to Trash`, `Restore note from Trash`, `Empty Trash`
  - `Organize note: <kind>`, `Undo organization: <kind>`
  - `Archive source: <title>`
  - `File Quicknote`, `File Quicknote to another notebook`
  - `Checkpoint live edits to <path>`
  - the group summary.
- **Skeleton test:** `gitobj.CleanMessage` output must equal what `git commit -F -` stores for the same input.

**Revisions and versions:**
- `Document.Revision` = the head or requested revision; `Version` = `PathVersion(rev, path)`. After Save, Version == Revision == the new head.
- Ancestry = `Commit(id)` exists and its seq ≤ head seq.
- Commit records use `%aI`/`%cI` ISO times and trimmed trailer values.
- LockState, Discussion and TrashListing revisions are the head SHA. The trash confirmation hash binds the head.

**Caches.** Identity (`identity.go:98/141`), metadata (`metadata.go:390/405`, `listing.go:24`), locks and trash are keyed by head or blob ID, and are ported using `Files(rev)` blob IDs. `committed_reads.go` is unchanged.

**Concurrency and watch.**
- Keep `s.mu`. Reads release it during I/O and recheck the generation.
- Writes use `ExpectHead` (the head seen under `mu`) plus `ExpectVersion`.
- Add a per-`notestore.Store` registry that dispatches `Watch` to `store.changed`.

**Twins on `*dbNotebook`:**
- `db_tree.go`: head, isAncestor, fileVersion, read/readRevision, files, exists/isDir (replacing the `os.Lstat` uses in `move.go:66-75`, `trash.go:275/345/459/534`, `organization.go:203-215`, `capture.go:332`), the content iterators (`committed_contents.go`), readBlobs, snapshotRevision/snapshotWithScope/readSnapshot, loadLocks/lockState, loadTrash, identityIndex/prepareIdentities.
- `db_policy.go`: the four `checkMutation*` checks (`mutation_scope.go:76/103/144/254`) and `clean`, which checkpoints pending live edits through F's `checkpointLive`. The stub errors only when edits are pending.
- `db_commit.go`: applyChanges taking ordered changes plus renames, the message builder, the signature, operationCommit/operationReceipt/groupReceipt, and commitRecord.
- `resolve` (`workspace.go:223`) keeps its syntactic checks and skips the file-system checks for DB stores.

**Files and helpers.**
- `db_notebook.go`: OpenDB/CreateDB/DeleteDB and `var _ notebookBackend = (*dbNotebook)(nil)`.
- One stub file per group, each returning `errNotImplemented`.
- `db_testing_test.go`: a `TestMain` using `pgtest.Main`, plus `newTestStore`, `reopenTestStore`, `commitFixture` and `failCommits` (through `pgtest.FailTable`).
- An exported `internal/workspace/workspacetest` (`New(t, dir, excluded)` → Store plus `*notestore.Notebook`) for other packages' tests.
- Pre-split mixed test files. Move every Backup/ImportBundle test into `*_backup_test.go` (owned by G).

## 3. Test map (internal/workspace)

Shorthands:
- **swap:** `openTest` → `newTestStore`.
- **fixture:** `commitFixture` instead of writing into Root and running git commit.
- **fail:** `pgtest.FailTable` instead of `.git/index.lock` failure injection.
- **hold:** `pgtest.HoldWrites` instead of git-shim barriers.
- **reopen:** `reopenTestStore`.
- **del:** delete the test, with the reason.

| Group | Files and conversion |
| --- | --- |
| **S** | **workspace_test.go:** swap; fail for TestCommitFailureRestoresOriginal; del TestDirtyWorkspaceIsNeverDiscarded, TestOnlyOneProcessOwnsWorkspace and TestHostBackupPreservesIndependentHistories; move the backup tests to G. **revisions_test.go, watch_test.go:** swap; they specify Version and ancestry. **identity_test.go:** fixture; the bundle case goes to G; del the remote case :120. **mutation_scope_test.go:** call directly instead of `remoteFixture`. **policy_cache_test.go:** keep the change detection, drop the git-shim counts. **head_test.go:** del, keeping the cancelled-request case. **Deleted at c2:** commit_outcome, save_crash, changes, durability, git_lifetime. |
| **A** | **search_test.go, literal_pathspec_test.go:** swap. **content_scope_test.go:** call directly. **metadata_test.go:** fixture; del the dirty and symlink cases. **browse_test.go:** del the spawn counter; fixture for the 0100755 case; the bounded export goes to G. **snapshot_test.go:** fixture. **snapshot_concurrency_test.go, read_concurrency_test.go:** hold or del. **performance_test.go:** fixture. |
| **B** | **history_index_test.go:** hold. **batch_policy_test.go:** keep the filter-before-limit assertions, drop the git-call counts. **task_history_test.go:** swap. |
| **C** | **move, duplicate, group, organization, containers:** fail; del the symlink, dirty, untracked and owner-RPC cases. **git_batch_test.go:** del. **cover, organization_regression:** swap. |
| **D** | **trash_test.go:** fail and reopen; del TestTrashOwnerRPC; the bundle restore goes to G. **locks_test.go:** fail. **comment_delete_test.go:** fail. **comments_test.go, anchors_test.go:** swap. **mutation_lock_scope_test.go:** call directly. **snapshot_paths_test.go:** keep. |
| **E** | **capture_test.go:** fail, reopen, call directly. **sources_test.go:** fail; restore cases go to G. **findings_test.go, source_replay_test.go:** swap. |
| **F** | **collaboration_test.go:** fail, reopen, read commit messages through `nb.Commit`. **collaboration_oversized_test.go:** fixture. **collaboration_capacity_test.go:** reopen. **collaboration_rejection_test.go:** call directly. **live_wait_test.go:** del the git and remote parts. **live_storage_test.go:** del, and restate append, restart and compaction against the `Live*` APIs. **collaboration_wire_test.go:** unchanged. |
| **G** | **import_allowlist_test.go:** build the bundle with real Git in a temporary repository. Also owns the moved backup tests. |
| **H** | **authority_test.go:** port. **remote_test.go, owner_dispatch_test.go, owner_import_test.go:** delete. |
| **Pure** | **links, notebook_id, search_excerpt:** unchanged. **views_test.go:** swap. |

**Tests elsewhere that build stores directly** (converted at c1 to use `workspacetest`):
- **catalog:** owners_test, import_test, move_test.
- **web:** owners_test, import_test, chat_guidance_test, comment_delete_test, organizer_owner_test, knowledge_owners_test, collaboration_rejection_test, locks_test.
- **search:** indexes_boundaries_test, indexes_quiescent_test.
- **knowledge:** retrieval, history, files, benchmark, index, restore, owner_cache, history_owner_timeout, snapshot_policy_recovery, history_sync_recovery.
- **quicknotes:** suggestion_preview_test.
- **cmd:** admin_test.

**Tests that run Git against `store.Root`** must use notestore fixtures instead:
- **knowledge:** history_test, container_policy_test, history_integration_test, trash_test, workflows_test (`reset --hard`).
- **web:** attribution_test, export_large_test, organization_undo_regression_test, authority_test, client_edit_test, mcp_actions_test:514, backup_config_security_test, instance_config_test.
- **quicknotes:** gateways_test, remediation_test, undo_routing_test.

## 4. Callers outside internal/workspace (c1)

| Area | Locations | Change |
| --- | --- | --- |
| Opening and creating | `catalog/owners.go:25-61`; `catalog.go:326` (createNotebook; on failure also delete the notestore notebook); `assistant.go:234,362`; `quicknotes.go:26`; `tasks.go:103`; startup opens at `catalog.go:209-219` | Use OpenDB/CreateDB |
| Catalog setup | `catalog.go:106-170` (owners parameter, ValidateOwnerConfig :143, listenForRevisions :168); `app/runtime.go:20,45`; `cmd main.go:159-175`; `admin.go:138` | Catalog gets a `notestore.Store`. `catalog.Open(data, excl, db)` keeps building a default `blob.OpenDir` store so its 32 test users compile. |
| Import | `catalog/import.go:43,74-77,97-142` | Import through ImportBundle into notestore |
| Remove | `catalog/move.go`; `admin.go:73-117` (move-notebook, rollback-live-storage); `cmd/forgetmenote/owners.go`; the `owner` command (`main.go:128`); `instanceconfig` OWNER_MAP_FILE (`config.go:101,189`); RemoteProjects/LocalVolume (26 references); `notebooks.go:52`; Makefile:57 `internalgen -check` | Delete |
| Root as a directory | `web/runtime_config.go:22-34` | Drop the existence requirement |
| Backups | `web/attachments.go:89`, `web/exports.go:169`, `cmd main.go:194`, `watcher.py:58` | Handled in G |
| Authority | `web/projects.go:91`, `actions.go:176`, `automation.go:106`, `source_jobs.go:54` (WithAuthority) | Keep |

## 5. Conflict rules for wave 2b

- Each group exclusively owns its `db_*.go` files and its converted test functions. Only the owner defines a given `*dbNotebook` twin.
- Skeleton files are frozen during 2b. New shared primitives go through the coordinator.
- **Cross-group helpers** must be agreed:
  - D ships prepareTrash, stageTrash and stageTrashDiscussion first; C uses them.
  - A owns `list` and the metadata cache; E uses them.
  - D owns Comments; E's findings use it.
  - F owns checkpointLive; S's `clean` calls it.
