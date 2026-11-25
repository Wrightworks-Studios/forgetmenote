# Storage v2: binding interfaces between work streams

This is the coordinator's contract. Streams build against these names and semantics. If you need to change one, keep the change additive and report it under `interfaceChanges`.

## 1. `internal/gitobj` (pure Go, no subprocess, no I/O beyond writers)

- `type ID [20]byte`, with `ParseID(string) (ID, error)` and `(ID) String()` giving 40 lowercase hex. The object format is SHA-1, matching today's repositories and GitHub.
- **Hashing:**
  - `BlobID(content []byte) ID` is `sha1("blob <len>\x00" + content)`.
  - `HashObject(kind string, body []byte) ID` covers kinds `blob`, `tree` and `commit`.
- **Trees:**
  - `type TreeEntry struct{ Name string; Mode uint32; ID ID }` with modes `0100644`, `0100755`, `040000`, `0120000` and `0160000`. Unknown modes are kept verbatim when parsed.
  - `EncodeTree([]TreeEntry) []byte` uses Git's sort order: a directory sorts as `name + "/"`.
  - `ParseTree([]byte) ([]TreeEntry, error)`.
- **Commits:**
  - `type Signature struct{ Name, Email string; When int64; TZ string }`. `TZ` is like `+0000`.
  - `type Commit struct{ Tree ID; Parents []ID; Author, Committer Signature; Message string; Extra []byte }`. `Extra` holds any header lines kept verbatim, such as `gpgsig`.
  - `EncodeCommit(Commit) []byte` and `ParseCommit([]byte) (Commit, error)` must round-trip byte-for-byte for every commit Git produces, signed and merge commits included.
  - `Trailers(message string) []Trailer`, with `type Trailer struct{ Key, Value string }`, parses the final trailer block like `git interpret-trailers --parse`.
  - `CleanMessage(subject string, body string, trailers []Trailer) string` produces exactly what `git commit -m subject -m body --trailer ...` would store with default cleanup.
- **Loose objects:** `WriteLoose(dir string, kind string, body []byte) (ID, error)` writes a zlib-compressed loose object into `<dir>/objects/xx/…`, durably and idempotently.
- **Tests** compare against the real `git` executable: `hash-object`, `mktree`, `commit-tree`, `cat-file`, `fsck --strict`.

## 2. `internal/notestore` (Postgres + `internal/blob`; all notebooks of one org)

- **Schema:** migration `internal/pg/migrations/0017_notestore.sql`.
  - Tables: `notebook_heads`, `revisions`, `git_trees`, `file_versions`, `revision_changes`, `entries`, `live_docs`, `live_updates`, `operation_receipts`, `revision_rewrites`, `purges`.
  - Every table carries `org` and follows the conventions of the existing migrations.
  - Indexes support:
    - history by `(org, notebook, entry_id, seq desc)`;
    - by `(org, notebook, path, seq desc)`;
    - changes by `(org, notebook, seq)`;
    - commit lookup by `(org, notebook, commit_sha)`.

### Content

- **Where it lives:** file content is keyed by Git blob ID in `file_versions(org, blob_sha, size, inline bytea null, blocks text[] null)`.
  - Files under 8 KiB are stored inline.
  - Larger files are split into blocks stored in the blob store under `blocks/<sha256>`.
- **Markdown (`.md`) splitting:** cut after `\n\n` boundaries, packing paragraphs greedily into blocks of 1–64 KiB. A paragraph over 64 KiB is cut at 64 KiB.
- **Other files** split into fixed 4 MiB blocks.
- **Concatenating the blocks must reproduce the exact bytes.** Blocks are written before the transaction commits, so they are idempotent. `SweepBlocks(ctx, olderThan)` removes blocks that no `file_versions` row references.

### API

Final names are up to the stream, but the semantics are binding:

```go
type Options struct { BlockCacheBytes int64; ReadModelBytes int64 }
func Open(ctx context.Context, db *pg.DB, blobs blob.Store, o Options) (*Store, error)
func (s *Store) Create(ctx context.Context, notebook string, init Signature) (Head, error)   // initial empty-tree commit "Initialize notes notebook"
func (s *Store) Delete(ctx context.Context, notebook string) error                             // rows; blocks via sweep
func (s *Store) Notebook(notebook string) *Notebook
func (s *Store) Watch(fn func(notebook string, head Head))                                     // local commits + NOTIFY "notebook_revisions" from other gateways

type Head struct{ Seq int64; Commit gitobj.ID }
type File struct{ Path string; EntryID string; Blob gitobj.ID; Mode uint32; Size int64; Version gitobj.ID } // Version = last commit that changed this path

// Reads. rev == zero ID means head. All are answered from the in-memory read model and the block cache when possible.
func (n *Notebook) Head(ctx) (Head, error)
func (n *Notebook) Files(ctx, rev gitobj.ID) ([]File, error)            // all files at rev, sorted by path
func (n *Notebook) Stat(ctx, rev gitobj.ID, path string) (File, error)  // os.ErrNotExist if absent
func (n *Notebook) Read(ctx, rev gitobj.ID, path string) ([]byte, File, error)
func (n *Notebook) ReadBlob(ctx, blob gitobj.ID) ([]byte, error)
func (n *Notebook) ReadRange(ctx, blob gitobj.ID, off, length int64) ([]byte, error)
func (n *Notebook) Commit(ctx, id gitobj.ID) (CommitInfo, error)       // parsed commit + seq + changes
func (n *Notebook) History(ctx, q HistoryQuery) ([]CommitInfo, error)   // by EntryID or Path (follows renames via EntryID), newest first, Limit/Before
func (n *Notebook) Changes(ctx, from, to gitobj.ID) ([]Change, error)   // ChangesSince
func (n *Notebook) Revisions(ctx, afterSeq int64, limit int) ([]CommitInfo, error)
type Change struct{ Op string /* add|modify|delete|rename */; Path, OldPath, EntryID string; Blob gitobj.ID; Mode uint32; Size int64 }
type CommitInfo struct{ Seq int64; ID gitobj.ID; Commit gitobj.Commit; Trailers []gitobj.Trailer; Changes []Change }

// The one write primitive. One transaction: SELECT … FOR UPDATE on notebook_heads; check expectations; build trees + commit with gitobj;
// insert revision/changes/trees/file_versions; update entries + head; NOTIFY. Returns ErrConflict (wrapping detail) on any failed expectation.
type ChangeSet struct {
    ExpectHead    *gitobj.ID              // optional: whole-notebook revision check
    ExpectVersion map[string]gitobj.ID    // per-path Version check; zero ID = must not exist
    Files         []FileChange            // applied in order
    Author        gitobj.Signature        // committer is always "ForgetMeNote <forgetmenote@localhost>" at the same time
    Subject, Body string
    Trailers      []gitobj.Trailer
    OperationID   string                  // optional idempotency: a repeated ID with the same request hash returns the first result
    AllowEmpty    bool
}
type FileChange struct{ Path string; Content []byte /* nil = delete */; From string /* rename source; keeps EntryID */; Mode uint32; EntryID string /* optional explicit identity for new files */ }
type CommitResult struct{ Head Head; Versions map[string]gitobj.ID; Replayed bool }
func (n *Notebook) Apply(ctx, cs ChangeSet) (CommitResult, error)

// Receipts (capture, groups, live patches): kind/op-id keyed, request-hash bound, TTL-swept 30 days after done.
func (n *Notebook) Receipt(ctx, kind, op string) (Receipt, bool, error)
func (n *Notebook) PutReceipt(ctx, r Receipt) error

// Live storage, engine-agnostic (bytes are Yjs or Loro updates).
func (n *Notebook) LiveState(ctx, entryID string) (LiveState, error)    // epoch, base version, snapshot, updates after snapshot
func (n *Notebook) LiveAppend(ctx, entryID string, epoch int64, update []byte, actor string) (int64, error) // ErrConflict if epoch changed
func (n *Notebook) LiveCompact(ctx, entryID string, epoch int64, snapshot []byte, upto int64, base gitobj.ID) error
func (n *Notebook) LiveReset(ctx, entryID string) (int64, error)        // new epoch, drops all updates/snapshots (purge, delete)

// Import and export (stream S4).
func (n *Notebook) ImportCommit(ctx, raw []byte, trees map[gitobj.ID][]byte, blob func(gitobj.ID) ([]byte, error)) (Head, error) // verbatim bodies; verifies hashes; appends in topo order; changes vs first parent
func (n *Notebook) Objects(ctx, afterSeq int64, emit func(kind string, id gitobj.ID, body []byte) error) error // commits, trees, blobs not yet exported

// Purge (stream S5) builds on these, not on ad hoc SQL:
type RewriteFunc func(f RewriteFile, content func() ([]byte, error)) (newContent []byte, keep bool, err error) // f: path, entry, blob, mode, size; content read lazily; asked once per (entry, path, blob)
type RewriteOptions struct { ExpectHead *gitobj.ID; Message func(seq int64, message string) (string, error); Actor, Reason string; Scope any; ResetLive []string }
func (n *Notebook) Rewrite(ctx, fromSeq int64, purgeID string, fn RewriteFunc, o RewriteOptions) (RewriteResult, error) // one head-locked transaction; records the purge and old->new SHAs; remaps commit receipts; resets live docs
func (n *Notebook) PurgeLive(ctx, purgeID string, o RewriteOptions) (RewriteResult, error) // a purge only live documents hold: records it, resets o.ResetLive
func (n *Notebook) Rewritten(ctx, old gitobj.ID) (RewriteRecord, bool, error)  // old SHA -> current SHA (zero: none) and the purges on the way
func (n *Notebook) Original(ctx, id gitobj.ID, purgeID string) (gitobj.ID, bool, error)
func (n *Notebook) Purge(ctx, purgeID string) (PurgeRecord, error)
func (n *Notebook) SharedElsewhere(ctx, blobs []gitobj.ID) (map[string]int, error) // other notebooks of the org using blobs
func (n *Notebook) LiveEntries(ctx) ([]string, error)
```

- **Read model:**
  - A per-notebook head index (path → `File`, plus directory tree IDs) is built lazily and updated by local `Apply` calls and by `NOTIFY` from other gateways, with a 5 s fallback poll. It is bounded by `ReadModelBytes` with LRU eviction across notebooks.
  - Historical trees are resolved from `git_trees` with a cache.
  - `Version` at a historical revision is the newest change to that path with `seq <=` that revision's seq.
- **Commits** are linear for revisions FMN writes. Imported merge commits keep their bodies verbatim, and their `Changes` are computed against the first parent.

## 3. Workspace layer: `internal/workspace` keeps its public API

- `workspace.Store` stays the facade used by web, MCP, chat, knowledge, search and automation. Its exported method set and semantics stay as they are, so every caller compiles unchanged except for opening and creating notebooks.
- `notebookBackend` (`notebook_backend_generated.go`) is the seam. The new backend is `*dbNotebook` in files `db_*.go`, built on `*notestore.Notebook`, with one file per method group:
  - `db_read.go`
  - `db_history.go`
  - `db_mutate.go` (move, duplicate, organize, groups, containers)
  - `db_trash.go` / `db_locks.go` / `db_comments.go`
  - `db_capture.go` / `db_sources.go`
  - `db_live.go`
  - `db_export.go`
- **Opening:** `workspace.Open(root string, ...)` is replaced by `workspace.OpenDB(ctx, ns *notestore.Store, notebook string, browseExcluded []string) (*Store, error)` and `workspace.CreateDB(...)`. `catalog.openStore` uses them.
- **Removed:**
  - remote owners: `remote*.go`, `owner_methods.go` RPC, `NewOwner*`, `owner` command, owner map config;
  - notebook authority handoff and generation fencing for owner moves;
  - the Git working tree, the write journal and `workspace.lock`.

  Repository-notebook authority and consent (catalog kind, `reposource`) stay.
- **Pure logic is reused, not rewritten:** front matter, `fmn-id` identity rules, links rewriting, metadata, the trash index format (`.gitnotes-trash.json`), the locks format (`.gitnotes-locks.json`), the comments format (`comments/<sha256(path)>.json`), validation and limits, and commit message plus trailer construction.
- **Tests:** the existing `internal/workspace` tests are the behavioural specification. A shared helper `newTestStore(t)` (pgtest DB, `blob.Dir`, `notestore`) replaces `Open(t.TempDir())`. Tests that assert Git-working-tree internals are rewritten to assert the same behaviour through the store, or deleted with a reason.

## 4. Live engine boundary (stream S8; engine chosen by D1)

`internal/workspace/liveengine.go` defines `liveEngine` and `liveDoc` as in design §9. The Yjs implementation wraps `reearth/ygo`. Updates stay restricted to one text container, `markdown`. The browser counterpart is `frontend/collaboration.mjs`. The wire protocol of `/api/live` and the MCP `read_live_note`/`patch_live_note`/`checkpoint_live_note` contracts stay unchanged.

## 5. Export (S4)

- `internal/gitexport` mirrors each notebook into `<data>/exports/<notebook>.git`, a bare repository with `refs/heads/main`, using `Notebook.Objects` and `gitobj.WriteLoose`. It runs incrementally after each commit, plus a periodic catch-up, then `git repack -d` and `git fsck --strict` off the request path.
- `Store.Backup`/`BackupLimited` (bundle) run `git bundle create` from the export repository.
- `deploy/git-backup/watcher.py` points at the export repositories. It force-pushes only when a purge flag file names an authorized purge ID.
- Import runs from a Git directory or bundle into the store with `ImportCommit` (`git cat-file --batch`), and backs `workspace.ImportBundle` and the cutover tool `forgetmenote admin storage import|verify`.
