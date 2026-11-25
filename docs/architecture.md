# Architecture and code ownership

"Everything is a notebook" is the organizing principle for user and AI content.
A notebook supplies identity, discovery, authority, permissions and revisions
whether it contains editable notes, retained imports or read-only repository
evidence. Browser, API, MCP, chat and automation operations use those same
notebook boundaries. Accepted AI output becomes notebook content through an
explicit, authorized workspace save; derived indexes and maps point back to the
original notebook and revision.

Operational state remains with its concrete owner: credentials and session
epochs, instance AI policy, private conversations, paid-request receipts and
temporary source jobs. These support notebook work without introducing another
editable content system. Source jobs are bound to their initiating subject,
notebook, credential restrictions and authority generation; their originals and
results enter notebook history only when explicitly saved.

ForgetMeNote is one Go binary. Each gateway process of it serves every notebook
of the instance through browser, API, MCP, chat and automation. Instance state,
notebook content included, lives in Postgres and the blob store, and every
notebook change is one Postgres transaction, so several gateways of the same
binary can serve one instance with no owner process between them. The default
deployment runs one gateway. Production assets are embedded in Go. Node is a
build and browser-test dependency only.

## Notebook storage

A notebook is a revision log in Postgres that keeps Git's object model
(`internal/notestore`, migrations `0017` to `0019`):

- **Revisions are Git commits.** Each revision stores its exact Git commit
  object and its trees byte for byte. A notebook's revision is its head commit
  SHA, and a note's `Version` is the SHA of the last commit that changed it.
  `internal/gitobj` computes commit, tree and blob IDs in the Go process when a
  write happens, so reads and writes run no Git subprocess; Git itself runs only
  for export maintenance, bundles and imports.
- **Content blocks are in the blob store.** File contents are kept once per
  instance by Git blob ID: a file under 8 KiB in its database row, a larger one
  as SHA-256-addressed blocks under `blocks/` in the blob store. Markdown is cut
  at blank lines into blocks of 1 to 64 KiB and other files every 4 MiB, so a
  small edit to a book-length note stores one new block.
- **One transaction per change.** Every change, from a save, upload or lock
  change to a move, Trash or organization operation, live checkpoint or bundle
  import, is one transaction. It first locks the notebook's head row, checks the
  expected head or note versions, and writes the revision, the paths it changed
  and the new head. A multi-file operation is one revision. The database
  serializes each notebook's writers across every gateway; there is no owner
  process, write journal or workspace lock.
- **Reads come from memory.** A per-notebook read model (paths, entries,
  versions and tree IDs) answers listings, status and lookups. Local writes,
  `NOTIFY notebook_revisions` from other gateways and a five-second poll keep it
  current. A note's history is one index range on its entry ID, its `fmn-id`, so
  it follows renames.
- **Live editing is Yjs on notestore.** `internal/workspace/db_live.go` and
  `liveengine.go` keep each note's Yjs document as a snapshot and an update log
  (`live_docs`, `live_updates`); other gateways hear each change on
  `notebook_live`. A checkpoint is an ordinary revision.
- **The Git export is reproducible.** `internal/gitexport` writes each notebook
  into a bare repository, `data/exports/NOTEBOOK.git` with the one branch
  `main`, after each revision and on a five-minute catch-up. The same revisions
  always give the same commits, and imported history keeps its original SHAs,
  so existing remotes continue by fast-forward. The external backup worker
  pushes the exports; ForgetMeNote never pushes. Only an authorized
  [purge](purge.md) rewrites history; it rebuilds the export and flags it for
  one force-push.

Comments, locks and the Trash index remain files in each revision
(`comments/<sha256(path)>.json`, `.gitnotes-locks.json`, `.gitnotes-trash.json`
and `Trash/`), so an export has the layout of an earlier release's notebook
repository. Repository notebooks keep their read-only Git mirrors in
`internal/reposource`.

### Performance budgets

Storage changes are measured against these budgets on the generated fixtures
of `internal/benchfixture`: F1, a 1.2 MB manuscript note, with F1h, its 5,000
edits, and F2, a notebook of 2,000 notes and 200 images over 20,000 revisions.

| Operation | Budget |
| --- | --- |
| Open the manuscript on the server, warm / cold | p95 < 50 ms / < 300 ms |
| Checkpoint changing one paragraph of it | p95 < 30 ms; storage growth < 16 KiB |
| Keystroke to paint, middle of the manuscript | p95 < 16 ms unthrottled, < 50 ms at 4× CPU throttle |
| Remote edit visible on another client, same gateway | p95 < 150 ms |
| Per-note history, 5,000 revisions | p95 < 50 ms |
| Notebook status and listing, 2,000 notes, warm | p95 < 10 ms |
| Export of one new revision; full export of 20,000 revisions | < 50 ms; < 2 min |
| Idle database load per gateway | < 5 transactions/s |

`internal/notestore/bench_test.go`, `internal/workspace/budgets_test.go`,
`internal/gitexport/budgets_test.go` and `scripts/browser/editor-budgets.spec.mjs`
measure them and report numbers rather than fail. A bare contenteditable
already takes about 15 ms from keystroke to paint, so the browser spec records
that frame floor beside the editor's numbers.

## Content and derived data

| Owner | Authoritative state | Derived or temporary state |
| --- | --- | --- |
| `internal/notestore` | Notebook revisions (exact Git commits and trees), file versions and content blocks, head entries, live documents and updates, operation receipts, authority handoff records, purges and their SHA rewrites | Per-notebook read models; block, manifest and tree caches |
| `internal/workspace` | Notebook rules and file formats over notestore: paths, front matter and `fmn-id` identity, locks, Trash, comments, collections, captures, commit messages and trailers, live editing and purge plans | Committed metadata, identity, lock and Trash caches; loaded live documents |
| `internal/gitexport` | Purge flags until the backup worker pushes them | Bare Git export of each notebook, reproducible from notestore, and its bundles; Git history import and verification |
| `internal/sources` | Private, subject/notebook-bound processing manifests and paid-attempt fences until expiry | Originals, local page checkpoints and prepared results awaiting an explicit notebook save |
| `internal/auth` | Credentials, sessions and OAuth grant families outside notebook content | Detached identity/restriction snapshots for current request checks |
| `internal/actions` | One typed operation table and notebook argument policies | Transport schemas and discovery views |
| `internal/backups` | Managed destination consent, authority-bound leases and verified receipts | No content copying or remote pushes; external worker owns uploads |
| `internal/githubapp` | Concrete shared Sources/Backups App HTTP transport | Separate App registrations and fixed read/write token boundaries |
| `internal/catalog` | Accounts, notebook roles and session epochs | No alternative content store |
| `internal/knowledge` | Source references, proposals and workflow receipts | Note passages, current/history keyword and semantic indexes |
| `internal/reposource` | Connection/consent state and captured source evidence | Read-only Git snapshot, source chunks, navigation map, keyword cache and vectors |
| `internal/citationchecks` | Private citation-check sidecars and publication identity | Bounded check/read admission, cancellation and workers |
| `internal/chat` | Private conversations, run scope, provider continuation and usage receipts | Bounded model context and reusable analyses with provenance |
| `internal/connectors` | Operator-owned endpoints, operation contracts and runtime credential references outside notebooks | Bounded external calls and execution receipts; no local copy of upstream data |
| `internal/automation` | Durable job admission, retry and publication/delivery checkpoints outside notebook Git | In-process scheduling and bounded run context |
| `internal/decision` and `internal/quicknotes` | Judgment accounting, jobs and notifications; capture/transfer receipts | Bounded candidates and intermediate judgments |

Notebook history and connected source Git have different owners. Notebook writes
are revisions committed through workspace methods; their Git form is the export.
Source snapshots change only through a verified upstream default-branch refresh.
Source indexing never commits code or generates a wiki. Explicit note edits and
citation capture remain separate actions.

Full stopped-instance backups include the database, the blob store and the data
directory with its exports, which together hold every durable owner above. A
notebook's Git export or bundle alone does not contain private chats,
permissions, pending live edits or separately captured repository evidence. See
[backup and restore](backups.md).

## Retrieval and AI

`internal/retrieval` owns shared tokenization/ranking math and evidence budgets.
It has no filesystem, permissions, network client or background worker. Notes and
source retain their own snapshot, chunk, cache and persistence lifecycles.

`internal/search/coordinator.go` coordinates explicit notebook/corpus selection
for HTTP, browser source search and MCP source search. It authorizes the entire
selection before provider work and rechecks it before returning evidence. Shared
packing applies count, per-file and token bounds. Chat interleaves source with
already diversified notebook passages under the same budget primitive. Note-only
retrieval retains its priority tiers, neighbor/dedup logic and complete serialized
passage estimates; ordinary note search retains distinct-note results.

Owners opt connected sources into external processing separately from ordinary
notebook settings. Embeddings reuse the existing configured endpoint. Jev may
rank the leading 32 retrieved source candidates before final packing or optionally
classify small changed map entries; the conversational model interprets original
excerpts and answers. Citation coordinates, permissions, freshness and byte hashes
are deterministic. There is no repository documentation generation or claim-checking
loop. General notebook Jev reviews, extraction and citation checks remain supported.

Source candidate discovery combines lexical and semantic branches with bounded
map hints and unambiguous same-file helper excerpts. Repeated symbol names do not
promote every possible definition; ordinary retrieval still searches those files.
Identifier-shaped terms in questions
protect matching declarations. Python module/class fields and shebang scripts
receive structural indexing; arguments, locals and string examples are not field
declarations. These are navigation hints, never synthesized source evidence.
Index upgrades re-chunk existing snapshots while reusing unchanged embedding
payloads. All returned excerpts retain original bytes and pinned citations.

`internal/reposource/map.go` derives the compact navigation map from admitted files
and existing chunks. Map queries use the same pinned snapshot and access policy as
source search. `map_enrichment.go` shares the embedding worker's cancellation and
publication lifetime; `internal/web/repository_map.go` owns the optional Jev boundary,
enabling Owner's consent and existing usage ledger. Uncertain purchased results fall
back to local roles without repurchasing them or blocking later entries. No large
model maintains the map, and no map entry is treated as citation evidence.

Details and limits: [knowledge](knowledge.md), [repository notebooks](repository-notebooks.md),
[chat](chat.md), [Jev](jev.md) and its [integration contract](jev-architecture.md).

## Requests and permissions

`internal/actions` owns schemas, action policy and notebook argument binding.
`internal/web/actions.go` registers one typed operation for API, MCP and chat;
MCP adapts request/session metadata at its transport boundary. Authentication
owns complete token/session/credential transactions and detached snapshots.
Chat owns admission with run persistence, reconciliation, configuration replacement,
stream capacity and deletion fences. Transport callers do not acquire either
owner's locks. Ordinary chat tools bind to the conversation notebook;
repository reads may use only the run's explicitly selected source notebooks.
Scope metadata does not grant access. Original handlers and workspace/catalog
boundaries still recheck current roles, locks, revisions, consent and source bindings.

Data calls share the typed invocation in `internal/dataops` across API, MCP, chat
and automation. `internal/web/data_connections.go` adapts identity and transport. Connection grants are independent of notebook roles; current grants
are checked before execution and before returning results. Each connector owns
its protocol, while one contract check defines schema identity and configured
pins for all adapters. Data discovery and calls do not acquire notebook RPC work
slots or enter the automation queue. Input decoding remains a separate short
phase and releases its permit before upstream I/O. Connections forward calls
independently by default; an explicitly configured positive `concurrent` value
retains the operator's chosen per-connection cap. Shared runtime-file validation belongs to
`internal/web/runtime_config.go`, independent of any enabled integration.

The source allowlist check is pure: repository search calls it while holding its
own notebook lock. Connection state and processing checks remain at their existing
manager/provider boundaries; calling manager status from that callback would
re-enter the same lock.

See [authentication](authentication.md), [MCP contracts](mcp-actions.md) and
[saved chat scope](chat.md#optional-connected-repository-context).

## Browser owners

| Module | Responsibility |
| --- | --- |
| `app.mjs` | Navigation, displayed note/editor, live session and private draft ownership |
| `search.mjs` | Search scope, filters, request cancellation, result callbacks and rendering |
| `retrieval-settings.mjs` | Personal search preferences and notebook retrieval defaults |
| `repositories.mjs` | Source settings, status polling, pinned file viewer and explicitly requested legacy history |
| `chat.mjs`, `chat-runs.mjs`, `chat-transcript.mjs` | Private conversation UI, streaming ownership and transcript projection |
| `knowledge.mjs`, `jev.mjs` | References, proposals and notebook judgment workflows |
| `settings.mjs`, `administration.mjs`, `settings-pages.mjs` | Personal settings and routable instance/notebook administration |
| `dom.mjs`, `overlay.mjs`, `feedback.mjs`, `state.mjs`, `request.mjs` | Shared browser interaction and rendered-HTML installation, overlay lifetime, feedback, polling/state primitives and the JSON request boundary |

Modules use the app's request helpers and explicit navigation/editor callbacks.
Search result callbacks remain bound to the account, notebook and request that
produced them; changing notebooks invalidates old results.

## Verification

[Feature guides](README.md) describe current behavior.
The [release gate](production-readiness.md#release-gate) and Makefile
define current verification commands.

Repository notebooks use the same document/search/map contracts as editable notebooks. Catalog kind and shared workspace guards enforce read-only content across browser, MCP, chat, and collaboration. The historical version-2 catalog transition introduced repository notebook authority and made old binaries reject that envelope. Current catalog writes publish version 10; see [storage transitions and rollback](storage-migrations.md). An unversioned/version-0 transition preserves `catalog.v1.json`; versions 2–8 retain `catalog.pre-storage.json`. This is a migration snapshot, not a current backup. Never restore it over newer account, permission, or notebook state. Derived maps and query embedding caches do not become authoritative content.

Managed backups are an optional control-plane boundary: `internal/backups` stores private bindings and leases in the instance's Postgres database, outside Git, and the existing external worker performs every remote upload. See [managed backups](managed-backups.md) for worker scoping, leases, revocation, and independent recovery.

The same executable runs the browser/headless server and CLI. `internal/client`
owns only the HTTP boundary, per-server credentials, and temporary individual-note
editing. `/api/v1` shares the typed action registry with MCP; chat retains its
own scope/result-budget wrapper. Scoped API grants extend existing OAuth private
state and do not replace local, proxy, or OIDC identity.

Catalog Kind remains the sole notebook authority type. Authority generation and
one catalog transfer record coordinate staging in the notebook's authority record
(the `notebook_authority` row of migration `0018`), source pause, and backup
drain. The historical catalog v3 transition introduced API credential and handoff
semantics and rejected unsafe downgrades; the current version is documented in
[storage transitions](storage-migrations.md). Recovery runs before HTTP admission;
publication completes forward, while unfinished preparation aborts. The source
notebook layout reuses Markdown metadata/containers; retained evidence remains
authorized and distinct from current retrieval. See [headless and authority
operations](headless.md) for protocol, upgrade and recovery details.
