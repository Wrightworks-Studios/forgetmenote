# ForgetMeNote storage v2: a revision store, a live editing engine and a Git export

- **Status:** draft for review
- **Date:** 2026-11-22
- **Base:** `next` @ `e6d89ec`
- **Companion:** [bake-off plan](storage-v2-bakeoff.md)
- **Background:** the 2026-11-22 code audit, §9–10

## 1. Summary

ForgetMeNote currently keeps a Git working tree for each notebook and uses it as its live database. Every request reads it through Git subprocesses. One writer process owns each notebook, protected by a write journal, ownership handoff and generation fencing. Yjs live editing sits beside Git as a second versioning system.

Storage v2 keeps **Git's data model**: content-addressed objects, commits and 40-hex commit SHAs used as revision IDs. It moves **storage and querying** into Postgres and the existing blob store:

- **Revisions** are Postgres rows. Each one stores the exact Git commit object and its changed paths. Commit and tree SHAs are computed in the Go process, with no Git subprocess, when the write happens.
- **File contents** are split into blocks and stored by content hash in the blob store (local directory or S3). A small edit to a 1.2 MB manuscript stores one block, not a new copy of the file.
- **Live editing** uses a CRDT engine behind a narrow Go interface. Yjs (current) or Loro is chosen by the bake-off. Checkpoints become ordinary revisions.
- **The Git export** writes each notebook's objects into a bare repository. The existing `deploy/git-backup/watcher.py` pushes it to GitHub unchanged. Imported history keeps its original commit SHAs, so old citations, GitHub history and fast-forward pushes carry on.
- **The editor** is CodeMirror 6 in every mode: Visual is an inline live preview over the Markdown text, beside Source and Side by side (§10; this replaced the planned Milkdown editor and its block bridge on 2026-11-23).

The MCP and `/api/v1` contracts stay the same. Revision strings stay Git SHAs.

## 2. Goals and non-goals

**Goals (the product requirements, 2026-11-22):**

1. **Portable.** Users can always get all their data out as Markdown with full history in a standard Git repository. Free private GitHub backups can be browsed on GitHub.
2. **Versioned history.** History at checkpoint level for export and browsing, and fine-grained history for blame by person and agent.
3. **Multi-user and agent editing.** Concurrent edits by people and agents merge; overlapping rewrites are the only conflicts.
4. **Top performance.** Budgets in §13.
5. **Very large documents.** A novel manuscript of about 1.2 MB (about 200k words) edits, saves and searches within budget.
6. **Stable contracts.** MCP tools and `/api/v1` behave identically for the agents of existing instances and Claude sessions.
7. **Simple operation.** One Go binary, Postgres and the blob store, self-hostable, CGO-free unless the bake-off justifies otherwise.
8. **Erasure.** Sensitive content that got into history can be purged from every version, the derived data, the live document and the GitHub copy (§11).

**Non-goals for v1:**

- Two-way sync, meaning importing edits made to the GitHub copy.
- Offline-first native apps.
- A multi-region deployment.
- Changes to plugins (Slack, data connections, automations) beyond the notebook interface they already call.
- Changes to repository notebooks (connected GitHub sources), which keep their read-only Git mirrors.
- Replacing Go or rewriting the frontend outside the editor.
- A structured document model (rich text, a chapter tree). Notes stay plain Markdown text.

## 3. Current state and its costs

These are measured or verified in the audit:

- **Subprocesses on every request.** Per-note history walks the whole repository history (`workspace/history.go:13-24`). `Status()` runs `git status` to learn HEAD (`workspace.go:543-557`). Hybrid search reads the full snapshot three times (`knowledge/hybrid.go:114,173`).
- **Five Git runners.** One of them mixes stderr into output that is parsed on NUL bytes.
- **Single-writer machinery.** Owner processes with a 73-method RPC surface (`workspace/owner_methods.go`, about 2.3k lines including the generated backend), a write journal, ownership handoff and generation fencing.
- **Two versioning systems.** Live text is a Yjs `Y.Text("markdown")` (server: `reearth/ygo`, about 2.2k lines) and is checkpointed into Git separately.
- **Large documents.**
  - Notes over 1 MiB are excluded from live editing (`docs/collaboration.md:6`).
  - Source mode is a `<textarea>` that is reassigned whole on every remote change. The prefix/suffix scan in `ui/markdown-editor.mjs` is O(document).
  - The visual editor (Milkdown, 209 KB gzipped) replaces its whole document on remote updates (`frontend/milkdown.mjs:241`).
  - The browser rewrites the full draft to local storage on every keystroke.
- **Size.** `internal/workspace` is about 11.9k non-test lines.

## 4. Contracts that define success

### 4.1 Export contract (the product promise)

- **Layout.** The export layout is today's repository layout, exactly:
  - notes `*.md` with front matter, including `fmn-id`;
  - `assets/`;
  - `comments/<sha256(path)>.json`;
  - `.gitnotes-locks.json`;
  - `.gitnotes-trash.json`;
  - the `Trash/` directory;
  - root `AGENTS.md` instructions.
- **Commit trailers are preserved:** `Gitnotes-Source`, `Gitnotes-User`, `Gitnotes-Client`, `Gitnotes-Operation`, `Gitnotes-Request` and `Collaboration-Receipt`.
- **Reproducible.** The same revisions always produce byte-identical objects and the same commit SHAs.
- **Round trip.** export → `git bundle` → import → export gives identical commit SHAs. CI enforces this with `git fsck --strict` on every exported repository.
- **Imported history keeps its original commit bytes and SHAs.** GitHub history continues with fast-forward pushes, and existing citations stay valid. That includes revision pins in knowledge references, chat citations and memory anchors.
- **Existing bundle import keeps working.** New notebook → Import bundle, and `import_project_bundle`.

### 4.2 Interface contract

- **Stable surface:** MCP tool names, inputs, outputs and error classes, and `/api/v1`.
- **Revision strings:**
  - the notebook revision is the head commit SHA;
  - a note's `Version` is the SHA of the last commit that changed it;
  - both stay 40-hex Git SHAs.
- **Contract suite.** Before any storage code changes, record every notebook-touching MCP tool against the current server on a fixture notebook, with normalized outputs. The same suite must pass against v2. Because imported history keeps its SHAs, read results compare exactly.
- **Consumers:**
  - people's agents on the hosted instance, through its MCP;
  - Claude sessions;
  - the `forgetmenote` CLI (`internal/client`, `/api/v1`).
- **Versioning without URLs** (the maintainer, 2026-11-22). No new versioned paths: `/api/v1` stays frozen as it is, and there is no `/v2`.
  - **MCP:** clients discover tools when they connect. Changes are additive only (new tools, new optional fields) and the meaning of existing fields never changes. Deprecations are announced in tool descriptions and removed only once usage stops.
    - `capabilities` already reports `"apiVersion": 1` (`web/mcp_actions.go:82`). Make it a date-based contract version such as `"contract": "2026-11-22"` so agents and support can see which behaviour they get.
    - MCP protocol versions keep being negotiated at connect (`web/mcp_compatibility.go`).
  - **HTTP API:** any future breaking change is selected by a date header, `FMN-Version: YYYY-MM-DD`. A request without the header gets the version pinned to its credential when the credential was issued, the model Stripe and GitHub use. Existing clients therefore never break.
  - **Storage v2 needs no contract change**, because revisions stay Git SHAs.

## 5. Architecture

```
Browser (WYSIWYG + CM6 source + engine) MCP / API v1 / chat / automations / plugins
          │ live updates                        │ notebook operations (query interface)
          ▼                                     ▼
┌──────────────────────── Gateway (one Go binary) ─────────────────────────┐
│ Notebook service ── write path: one Postgres transaction per change set  │
│   ├─ Read model (per notebook, in memory): tree, metadata, IDs, locks,   │
│   │   trash, directory tree hashes; LRU bounded by capacity budgets      │
│   ├─ Live engine interface (Yjs | Loro): docs in memory, update log      │
│   └─ Git object hashing (blob/tree/commit SHA-1) when each write happens │
│ Indexers (knowledge, search, history) fed by revision events             │
│ Exporter: Postgres objects + blocks → bare repo per notebook             │
└──────┬──────────────────────────────┬──────────────────────────┬────────┘
       ▼                              ▼                          ▼
 Postgres: revisions, commit/tree   Blob store (dir | S3):   /data/exports/<id>.git
 objects, changes, entries, file    blocks and large assets  → watcher.py pushes to
 versions, live update log, receipts by content hash          GitHub (fast-forward)
       │ NOTIFY notebook_revisions (existing channel) wakes other gateways
```

**Single writer by construction.** Each write locks its notebook's head row (`SELECT … FOR UPDATE`) inside one transaction. That replaces owner processes, the journal, handoff and fencing, and multi-file operations (move, trash, organize, import) become atomic. Several gateways work without extra machinery; v1 still runs one gateway per instance.

## 6. Data model

All tables carry `org`, following the existing convention (`internal/pg/migrations`). The names are provisional.

| Table | Key | Purpose |
| --- | --- | --- |
| `notebook_heads` | (org, notebook) | Head revision sequence and commit SHA. The writer row-locks it. |
| `revisions` | (org, notebook, seq) | `commit_sha` (unique), `parent_seq`, `commit_body bytea` (the exact Git commit object), plus columns for querying: actor account, actor kind (person/agent/automation/system), client, source, operation ID, request ID, time and trailers (jsonb) |
| `git_trees` | (org, notebook, tree_sha) | Exact tree object bodies, content-addressed (`ON CONFLICT DO NOTHING`) |
| `revision_changes` | (org, notebook, seq, path) | Operation (add/modify/delete/rename), old path, `entry_id`, `blob_sha`, mode, size, live engine version at checkpoint |
| `entries` | (org, notebook, entry_id) | Current path, `blob_sha`, the `Version` commit SHA, and kind (note, asset, comments or system). Notes use the `fmn-id` front-matter ID; other entries get IDs on import. |
| `file_versions` | (org, blob_sha) | Size plus an ordered block list. Files under 8 KiB are stored inline as `bytea`. |
| `live_docs` / `live_updates` | (org, notebook, entry_id[, seq]) | Engine snapshot key and the update log, which only grows until compaction at checkpoint |
| `operation_receipts` | (org, notebook, operation_id) | Idempotency for retried operations (replaces capture receipts and journal markers), with a TTL sweep |

**Blocks.**
- Markdown splits at blank-line boundaries into chunks of 1–64 KiB, so joining the chunks gives back the exact bytes.
- Assets are stored whole, because images don't delta-compress.
- Block key: SHA-256 of the bytes, kept in the blob store.
- Expected cost: a checkpoint that changes one paragraph of a 1.2 MB manuscript adds one block (about 1–4 KiB), one manifest row, a few tree objects and one commit row.

**Git hashing.**
- `blob_sha` is Git's `sha1("blob <len>\0" + content)`.
- The read model keeps a tree hash for each directory, so a change re-hashes only the directories along its path.
- Each commit body is built from the parent SHA, root tree, author and committer identities, times, message and trailers, and stored verbatim.
- SHA-1 object format stays, because GitHub does not accept SHA-256 repositories yet. Revisit after Git 3.0.

## 7. Write paths

1. **Explicit save** (`save_note`, browser Save, `upload_attachment`, metadata, locks, comments):
   1. Validate locks, roles and size limits.
   2. Write blocks to the blob store first. They are content-addressed and idempotent; a sweep removes orphans.
   3. Open the transaction: lock the head row, check the expected note `Version` or notebook revision, compute the tree and commit, insert rows, update entries and the head.
   4. Commit, then `NOTIFY`.
2. **Save against an older version:**
   - If the note has no live edits, run a three-way text merge between the base `Version`, the current content and the submitted content. It merges at paragraph or line granularity. Overlapping changes return today's conflict.
   - If the note is live, compute the diff from base to submitted content, apply it as engine operations rebased from the base version, then checkpoint.
   - Loro can branch from any recorded version. Yjs needs a retained base snapshot (today: 15 minutes, 16 MiB). The bake-off measures both.
3. **Live editing:**
   1. The browser turns edits into engine updates: the editor sends the text changes it made.
   2. The server checks them: a single text container, a size bound and the role.
   3. It appends to `live_updates` and applies them to the in-memory document.
   4. A `NOTIFY` wakes other gateways, which read the log and fan the update out to their clients.
4. **Agent patch** (`patch_live_note`): same contract as today (base, operation ID, edits with expected text). Each person and each agent gets its own engine peer or client ID, which provides attribution.
5. **Checkpoint:** read the engine text; if it differs, write a revision with a `Collaboration-Receipt` trailer, record the engine version, and compact the update log into a new snapshot. When checkpoints happen is open question Q1.
6. **Multi-file operations** (move, duplicate, trash/restore/empty, organize/`ApplyGroup`, import source, capture): one transaction and one revision. Comments, locks and the trash index stay as files in the revision (`comments/…`, `.gitnotes-*.json`), so the export layout doesn't change. The read model parses them for queries.

## 8. Read paths

- **Read model.**
  - Contents: path → entry (ID, `blob_sha`, size, parsed front matter, `Version`), plus directory tree hashes, locks, the trash index and collections.
  - Built from `entries` on first access, then updated from `revision_changes` when a `NOTIFY` arrives.
  - `Status()`/HEAD, listings, browse, `FindScoped` and collections are answered from memory with no notebook lock held.
- **Content.** Read through an LRU block cache from the blob store or the inline row. Attachment reads become ranged reads over blocks, which removes the quadratic chunked-read finding.
- **History.** Per note, a SQL query on `revision_changes` by `entry_id`. Renames follow automatically through the stable ID, with no rename detection. `ChangesSince(rev)` is a sequence range query.
- **Search and knowledge.** Indexers consume revision events and re-split only the notes that changed. Embeddings are reused by passage hash, as today. History search reads versions from `revision_changes` instead of walking Git.

## 9. Live engine interface (Yjs or Loro)

This is a narrow Go interface at a real third-party boundary, which the engineering rules allow:

```go
type liveEngine interface {
    Load(snapshot []byte, updates [][]byte) (liveDoc, error)
}
type liveDoc interface {
    Apply(update []byte, limits liveLimits) error            // validate: one "markdown" text, size, no other containers
    Text() string
    Version() []byte                                          // Yjs state vector | Loro frontiers
    EncodeSince(version []byte) ([]byte, error)
    Patch(base []byte, edits []textEdit, actor peer) ([]byte, error) // conflict on expected-text mismatch/overlap
    Snapshot() ([]byte, error)
    Attribution(start, end int) ([]span, error)               // optional: actor spans for blame (Loro native)
}
```

- **Browser side:** `frontend/collaboration.mjs` implements the same operations with the engine's JS library.
- **Interop requirement:** updates produced in Go and in JS must be exchangeable and must converge byte-for-byte. A fuzz test in the bake-off checks this.
- **Engine choice is decision D1 in the bake-off.** For Loro, the Go options are:
  - the community `loro-go` (cgo over loro-ffi; pre-1.0 API);
  - Loro compiled to WebAssembly and run with `wazero`, which keeps the build CGO-free.
- **Fallback:** if Loro fails the gate, Yjs stays. Every goal above is still met, and blame comes from diffs between revisions.

## 10. Editor: a live preview over Markdown text

**Decision (the maintainer, 2026-11-23), replacing the one of 2026-11-22 below:** Visual mode is a CodeMirror 6 inline live preview, as in Obsidian, beside Source and a side-by-side mode. Every mode edits the Markdown text directly, and no code path changes text the user did not edit. Reason: seven review rounds of the Milkdown block bridge each found new cases where Visual silently changed text the user had not edited.

**Earlier decision (the maintainer, 2026-11-22), superseded:** a WYSIWYG editor (Milkdown/Crepe, chosen over Tiptap in the bake-off) bound to the text by a block-by-block bridge that re-serialized only the blocks an edit changed, with section mode for notes over 16 KiB, because ProseMirror draws the whole document. The bridge, its tests and its review history are kept on `wip/storage-v2-editor` and in [storage-v2-results-editor.md](storage-v2-results-editor.md); none of it is in the product.

**Design:**
- **The stored content stays Markdown text** in the live engine, one text per note. This keeps three things true:
  - agents keep editing text (`patch_live_note`, `save_note`);
  - the export stays exact;
  - the engine choice (D1) doesn't depend on the editor.

  Rejected: binding the engine to an editor's structured model (`y-prosemirror`/`loro-prosemirror`), and any editor that serializes a document model back to Markdown.
- **One CodeMirror view per note serves every mode** (`frontend/source-editor.mjs`, `internal/web/ui/markdown-editor.mjs`). Visual, Source and Side by side are compartments of the same view: switching mode reconfigures them, keeps the caret and scroll, and changes no text. The mode is remembered per browser.
- **Visual is the live preview** (`frontend/live-preview.mjs`). Formatting is drawn over the text: headings, emphasis, links, lists, quotes, code, tables, images, rules and front matter look like the reader, and an element's Markdown marks are hidden unless a selection touches it. Inline decorations come from a view plugin over the visible ranges only; tables, rules, video and front matter are block widgets from a state field covering the viewport plus 20,000 characters, rebuilt only where an edit or the selection touched. Decorations are frozen during an IME composition. HTML is dim literal text, never rendered.
- **Side by side** shows Source next to a read-only rendered pane (`frontend/rendered-preview.mjs`): micromark and mdast with the GFM pieces goldmark uses, built into an allowlisted DOM, block by block from the editor's Lezer tree, windowed, cached, and rendered in idle time after edits. The same renderer renders private drafts, which never leave the browser. Read stays server-rendered and canonical.
- **No automatic edits.** The only changes are typed input, paste (plain text only), cut, and explicit commands (`frontend/editor-commands.mjs` over the pure text functions in `frontend/markdown-commands.mjs`). There is no closeBrackets, auto-indent, list renumbering, input rule or normalization, and no CodeMirror history: the live session's per-user undo owns undo, and each command is one undo step.
- **Large documents.** CodeMirror draws only the viewport, so a note of any size is one editor; there is no section mode. Whole-note work (comment placement, writing tools) runs in the analysis worker, never in the input task.
- **Features carried over:** the toolbar and keys, tables (`ui/tables.mjs` numeric columns), media insert and the video player, comments in every mode, page and notebook links, focus mode, writing tools, IME composition handling, read-only state, the private-draft overlay and the private-draft preview.
- **Language packs** in the source viewer load lazily, per language.

## 11. Git export and backups

**Exporter.**
- Writes loose objects in the Go process (zlib of the stored commit and tree bodies, and blob bytes rebuilt from blocks) into `/data/exports/<notebook>.git`. It then updates `refs/heads/main`.
- A background `git repack -d` and `git fsck --strict` run in the export repository only, never on the request path.
- *Implemented in `internal/gitexport`:*
  - The branch is the durable record of what was exported: each export looks its commit up and writes the objects of the revisions after it, then replaces the branch (`main.lock`, fsync, rename, directory fsync).
  - The branch only moves to a descendant of the commit it named. History that no longer extends the export (a purge, a notebook created again) stops the export with `ErrDiverged` until `Rebuild`.
  - Exports follow `Store.Watch` and a five-minute catch-up over the export repositories, the notebooks seen and an optional list. Maintenance is `git repack -d --geometric=2`, so packs do not pile up, plus `git fsck --strict`, at most hourly per repository with new objects. One process holds an export directory (`flock`).
  - Loose objects are written by four writers at once, overlapping their fsyncs; the branch moves only after every object is durable.
  - Measured on F2 at the bake-off size (20,000 revisions, 2,000 notes, 200 images; `FMN_GITEXPORT_F2=full go test ./internal/gitexport -run F2`): import 1 min 15 s, verification 15.7 s, full export 54.5 s (2 min 2.8 s with one writer), repack and fsck 38.7 s, one new revision 8.2 ms.

**Push.**
- `deploy/git-backup/watcher.py` is pointed at the export repositories, unchanged: fast-forward only, and it stops on divergence.
- Managed backups (GitHub App bindings) are unchanged.

**GitHub limits.**
- Files over 50 MB are flagged.
- Files over 100 MB are left out of the pushed tree and replaced with a manifest entry. They stay in the blob store and in full backups.
- The notebook is flagged if the repository approaches 1 GB.

### Purge (v1; decided by the maintainer, 2026-11-22)

Purge permanently removes sensitive content, such as a leaked credential, customer data or personal data, from **every version**, not just the current one. It is the only authorized rewrite of history. The engineering rules ("never rewrite history or force-push") need an explicit exception for it.

**What can be purged:**
- **A whole note:** removed from every version, along with its comments file. Assets it alone references are offered for removal too.
- **Specific versions** of a note.
- **Text redaction:** an exact string or pattern removed from every version of the selected notes or of the whole notebook, replaced with `[purged]` so the surrounding document keeps its shape. This is the tool for a leaked API key.

**Who and how:**
- An Owner of the notebook or an instance Admin, in the browser or through the admin CLI.
- MCP agents can only *prepare* a purge plan for a human to approve. No agent purges on its own.
- **Plan first.** Before running, the plan shows:
  - the affected notes and versions;
  - other places in the notebook where the same text appears, which you can choose to include;
  - how many references will be remapped;
  - whether the GitHub copy will be force-pushed.
- Running it requires a confirmation that is tied to that exact plan, together with a reason. The reason is stored, never the purged content.

**What it does (one notebook-locked operation):**
1. **Rewrite history.** Build new trees and commits from the first affected revision onward. Every later commit gets a new SHA, and each rewritten commit carries a `Gitnotes-Purge: <id>` trailer. Each old SHA → new SHA pair goes into `revision_rewrites`.
2. **Delete content blocks** that nothing references any more. Blocks are shared by content hash, so a block still used elsewhere stays, and the plan reports that occurrence.
3. **Reset the live document.** The engine's history (Yjs update log or Loro operations) still holds the content, so the live document starts again from the purged text: a new live epoch, with old updates and snapshots deleted. Connected browsers are told to drop their local state for that note, including private drafts saved before the purge, on reconnect.
4. **Clear derived data:**
   - passages, embeddings and history-search entries that came from the purged content;
   - in-memory caches on every gateway, through a `NOTIFY` purge event.
5. **Clean other stores:**
   - Knowledge proposals, Jev records and receipts are scanned and redacted.
   - Private chat transcripts that quoted the text are redacted by the system without anyone reading them. The plan reports counts only.
   - Agent memory is ordinary notebook content: it is covered if it's in the same notebook, and otherwise reported.
6. **Remap references.** Citations, history pins and memory anchors that point at old SHAs resolve through `revision_rewrites`. Reading a pin to a version whose content was removed returns "purged". MCP tools accept old SHAs and map them, so agents don't break.
7. **Rebuild the export** from the first affected commit, then `git gc --prune=now` in the export repository. The notebook is flagged so that the next push is a force-push for that purge ID. The watcher force-pushes only with that flag, and never otherwise.
   - *Implemented:* `Exporter.Rebuild(ctx, notebook, purgeID)` writes every revision's objects again, then the flag `forgetmenote-purge.json` (`purge`, `head`: the newest commit with the trailer `Gitnotes-Purge: <purgeID>`, which must exist, and `replaces`: every commit the rewrite dropped from the export), then the branch; it removes other refs and reflogs, runs `git gc --prune=now`, removes interrupted temporary files and fails if `git fsck --unreachable` still finds anything. The repository keeps its place, so a running watcher keeps it.
   - The watcher replaces the remote branch (`--force-with-lease`) only after a refused fast-forward, when the remote is at a commit in `replaces`, the pushed revision contains `head` and `head` carries the trailer, and the purge was not pushed before. After any verified push containing `head`, a fast-forward to a remote that never had the dropped commits included, it records the purge ID as pushed and removes the flag. Rebuild replaces the flag and the watcher removes it only under the `flock` of `forgetmenote-purge.lock` in the export, so a newer purge's flag is never removed.
   - An operator repairs an export that diverged or fails `git fsck` with `forgetmenote admin storage export -recreate` (`Exporter.Recreate`): the export is deleted and written again from scratch, keeping an unspent purge flag.
8. **Record the purge** in a `purges` table: ID, notebook, who, when, reason, scope summary and the SHA mapping. Nothing derived from the content is kept. Even a hash of a short secret could be used to confirm a guess.

**What purge cannot do:**
- **Undo exposure.** Anything already pushed to GitHub, downloaded or sent elsewhere must be treated as leaked, and secrets must be rotated regardless. GitHub may keep cached views of old commits for a while; fully clearing them may need GitHub Support.
- **Reach earlier backups.** `pg_dump` files and bundles taken before the purge still contain the content. The purge report lists them, and an operator option deletes local backups older than the purge.

## 12. Migration (one cutover per instance)

Context: two instances (a single-host instance and a hosted instance with an owner and gateways), no browser editing in use, MCP-written content (data profiles, memory notebooks).

1. **Prerequisites:**
   - the instance on the Postgres control plane (the single-host instance already is; the hosted instance's is checked during its assessment);
   - the bake-off decisions recorded;
   - the MCP contract suite recorded.
2. **Freeze:**
   1. Pause outgoing actions (`outgoing: paused`).
   2. Put the instance in maintenance mode and checkpoint all live notes.
   3. Take final backups: `pg_dump`, the data directory, a bundle per notebook, and a GitHub push.
3. **Import each notebook.**
   - Stream the Git history (`git cat-file --batch` over `rev-list --reverse --topo-order`).
   - Store commit and tree objects verbatim.
   - Split blobs into blocks.
   - Assign entry IDs (`fmn-id` for notes; rename-following for other files).
   - Build `revision_changes`.
   - *Implemented:* `forgetmenote admin storage import|verify|export` (`cmd/forgetmenote/storage_admin.go`). The import refuses a notebook whose old storage holds work its Git history lacks: a `write-journal.json`, uncommitted files, or live edits not checkpointed, read from the collaboration snapshots in the stored format (the version 2 `{"version":2,"data":{...}}` envelope or the bare version 0 record) and their append logs. Entry IDs come from `workspace.ImportEntryID` through `notestore.Options.ImportEntryID`.
4. **Verify:**
   - every imported commit's tree recomputed in the Go process matches the original;
   - the export reproduces the original head SHA;
   - `git fsck --strict` passes;
   - the MCP contract suite passes;
   - knowledge indexes rebuild and sampled searches match.
5. **Switch** to the v2 binary as soon as verification passes.
   - The old storage code is deleted in the same release (§14); Git history keeps it.
   - Rollback is the final backups taken at the freeze: the old image tag, data directory, `pg_dump` and bundles.
6. **Order** (the maintainer, 2026-11-22): the single-host instance as soon as v2 is ready. The hosted instance's cutover needs are assessed after that, including its Postgres control-plane status.

## 13. Performance budgets (validated or adjusted by the bake-off)

| Operation | Budget |
| --- | --- |
| Open a 1.2 MB manuscript (server, warm / cold) | p95 < 50 ms / < 300 ms |
| Checkpoint changing one paragraph of that manuscript | p95 < 30 ms server; storage growth < 16 KiB |
| Keystroke to paint, middle of manuscript | p95 < 16 ms (unthrottled), < 50 ms (4× CPU throttle) |
| Remote edit visible on another client, same gateway | p95 < 150 ms |
| Per-note history, 5,000 revisions | p95 < 50 ms |
| Notebook status / listing (2,000 notes) | p95 < 10 ms warm |
| Export of one new revision | < 50 ms; full export of 20k revisions < 2 min |
| Idle database load per gateway | < 5 transactions/s (today: about 623) |

## 14. What is deleted and what stays

**Deleted:**
- the Git working trees and every workspace Git runner on the request path;
- the write journal;
- owner processes and the 73-method owner RPC;
- the generated backend and remote notebook stubs;
- notebook ownership handoff and generation fencing;
- the live-storage versions and `rollback-live-storage`;
- trash and history caches built from `git log`;
- the history indexer's Git walking;
- the file-based capture receipts;
- file-state import/export and the `release-a` fixtures (prerequisite, §12).

Expect a substantial net reduction. The bake-off produces a line-count estimate.

**Stays:**
- catalog, auth, chat, automations, connectors and plugins;
- the knowledge and search engines (fed by events);
- `reposource` (external Git mirrors);
- the MCP and HTTP handlers, through a collapsed query interface;
- the backup worker and the GitHub App.

## 15. Risks

| Risk | Mitigation |
| --- | --- |
| Postgres becomes critical for content | Back up `pg_dump` and the blob store together. The GitHub export is an independent copy. Run a restore drill before each cutover. |
| Git hashing bugs break reproducibility | Round-trip and `git fsck --strict` in CI; tree recomputation checked during import; property tests against `git hash-object` |
| Loro's Go binding is immature or its API changes | Bake-off gate, engine interface, Yjs fallback |
| The editor lags on book-length notes | CodeMirror draws only the viewport in every mode; live-preview decorations cover the visible ranges, and block widgets rebuild only where an edit or the selection touched (§10). |
| An editing mode changes text the user did not edit | Every mode edits the Markdown text directly (§10). A corpus spec opens, clicks, scrolls, selects and switches modes and asserts that nothing is sent and every byte stays; property tests cover every toolbar command and its undo |
| Purge misses a copy (a shared block, the live history, an index, a cache, the export) | A completeness test after every purge scans every stored byte: Postgres rows, blob store, decompressed export objects, live snapshots and updates, and indexes. It is a must-pass test in the bake-off and in CI. |
| Purge breaks references or agents holding old SHAs | `revision_rewrites` mapping, with MCP tools accepting old SHAs; contract tests across a purge |
| Hidden dependencies on working-tree behaviour (instructions, sources, captures) | The contract suite covers every notebook-touching MCP tool, with browser tests for the rest |
| Scope creep (chapter trees, rich text) | v1 non-goals (§2) |

## 16. Open questions for the maintainer

1. **Q1 checkpoint timing: decided.** An automatic checkpoint after 2 minutes idle or 15 minutes of continuous editing, grouped per author, alongside explicit saves.
2. **Q2 editor feel: decided.** A live preview over the Markdown text, beside Source and Side by side (§10; the maintainer, 2026-11-23, replacing the WYSIWYG decision of 2026-11-22).
3. **Q3 purge: decided.** Built in v1 (§11), because sensitive data accidentally reaching Git history has to be removable.
4. **Q4 cutover order: decided.** The single-host instance first, then assess the hosted one.
5. **Q5 contract scope: decided.** The consumers are the MCP (people's agents on the hosted instance, and Claude sessions) and the CLI. No URL versioning; see the policy in §4.2.

## 17. Milestones

There are no waiting periods between steps; each starts when the one before it is done.

1. **Decide:** the bake-off settles D1 (engine), D2 (editor) and D3 (store), and the MCP contract suite is recorded.
2. **Build v2 complete in one piece:**
   - the store, read model and in-process Git hashing behind the query interface;
   - the chosen engine and editor, with the 1 MiB live limit lifted;
   - the exporter;
   - the import and verification tools;
   - purge (§11);
   - removal of the old storage code.

   It is ready when the contract suite, the export round trip and the budgets pass.
3. **Cut over the single-host instance right away** (§12).
4. **Assess the hosted instance's cutover** and run it.
5. **Editing gains:** blame UI and merged agent saves.
