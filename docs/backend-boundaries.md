# Backend request boundaries

The instance remains one Go process. Notebook content, permissions, authority and
revision checks retain their existing concrete owners; HTTP handlers do not open
repositories or choose filesystem roots from request arguments.

`internal/web` composes the instance and adapts browser, action API and MCP
authentication. `internal/webcore` holds shared HTTP mechanics:

- `NotebookRoutes` registers method patterns together with their catalog role.
  Missing or invalid role metadata fails closed. Compatibility rules for rejected
  methods select only the permission check before a 404, never a handler.
- Notebook context keys carry the store and identity selected by middleware.
- One error classifier supplies HTTP status, stable code and message. Browser
  routes retain a string `error` with additive `code` and `message`; the existing
  action API retains its nested `error` object. These wire adapters preserve
  supported consumers while sharing classification.
- Standard API middleware and router errors become JSON too. This applies to
  `/api/…` and `/p/{notebook}/api/…`, not assets with an `api` directory. HTML,
  OAuth, MCP, health checks, successful responses and event streams retain their
  contracts. Only plain error text is buffered, bounded to 8 KiB.
- `Renderer` owns the Markdown parser and sanitizer together. Note, source and
  AI previews use that boundary; raw Markdown and note metadata remain available
  in the existing note response. No content becomes an operational instruction.
- `MonitorAccess` cancels work on revocation and is joined before releasing its
  invocation's admission or runtime lifetime. Final disclosure checks remain
  required; monitoring alone does not establish authorization.

The Inbox HTTP transport lives in `internal/inbox`; private capture content,
consent, recoverable filing and notebook checks remain in `internal/quicknotes`.
It registers through the same instance middleware and role table used before
extraction.

A notebook operation that both the browser and MCP offer is defined once in
`internal/web` as `func(ctx, store, input) (output, error)`. `notebookJSON` serves
it as a browser route and `addProjectTool` as an MCP tool (and action). Both
authorize the caller for the notebook immediately before the operation runs, the
browser again after reading the request body, and both classify its errors with
`webcore.ErrorOf`: an HTTP status in the browser, and the same status and code
under the tool result's `_meta["forgetmenote/error"]` in MCP, with `retryable`
for capacity and timeouts. Retrieval engine policy, provider configuration and lifecycle
remain in `internal/search` and `internal/knowledge`.

`internal/sourcehttp` owns source preview/import and job HTTP handlers, shared
input schemas, signed previews and the typed submission/wait pipeline used by
MCP. It uses the same instance-wide source I/O admission pool as research and
automation. The four required policy bindings supply credential-restricted job
scope, notebook edit access, hosted-processing consent and the instance signer.
The durable job manager remains in `internal/sources`; its scope serialization,
recovery, explicit resume and provider guards are unchanged. Preview signatures
remain bound to the requesting identity, notebook, expiry and captured bytes.

`internal/dataops` owns the typed data invocation shared by browser, action API,
MCP, chat and automation. It checks profile scope, records intent and outcome,
and binds a current-authority callback before connector admission. The connector
manager rechecks it after admission and at outbound request boundaries, so a
queued call cannot send after its grant is revoked. Already submitted writes
retain their existing durable recovery and honest acknowledgment semantics.
Credentials and provider I/O remain with `internal/connectors`; managed-write
submission and usage learning retain their concrete operational owners.

`internal/backuphttp` owns the external worker's credential hashes and HTTP
dispatch. It checks the existing worker identity and body limit before handing
leases, drains and receipts to `internal/backups`. Consent, notebook generation
and actor authority stay with that manager and the catalog; transport extraction
does not create another backup writer or a new persistent representation.

`internal/taskqueue` owns task queues: the task actions, the `/task` and
`/tasks` Slack commands, forms and buttons, task notices and the weekday
reminder. Web registers the task tools through the shared action dispatcher,
and checks a command's Slack signature and workspace before it reaches task
queues. The required hooks bind the personal-credential check, automation
authority, messaging clients and the Slack app, data operations and
connections, reply reminder authority and the origin. Those configured after
startup are read at each call.

`internal/jsonfile` provides the shared strict operator loader. It checks regular
file size before decoding, rejects unknown fields and trailing values, and bounds
the reader even if the file grows. Individual owners retain semantic validation,
path authorization and credential handling. The existing configuration formats,
filenames and section-specific size limits remain supported.

`internal/agentmemory` owns agent memory: the `recall_memory`, `remember` and
`confirm_memory` actions, the Markdown memory format, memory search and its
per-process vectors, the memory instructions connected agents receive, and
the memory a direct message reads and writes. Web registers the actions
through the shared action dispatcher and decides whose memory a direct message
uses, which is the messaging link. The required hooks bind the
personal-credential check, client notebook access, Jev and its authorization,
the chat embedding policy and the origin, each read at each call.

`internal/citationchecks` owns citation-check sidecars, bounded queue/read
admission, worker cancellation and shutdown. Pending check identity is durable
before work starts; publication checks the current check ID and cannot recreate a
deleted conversation's sidecar. The HTTP callback retains current account,
notebook, consent, source-freshness and evidence checks. Personal/organization
settings decode request bodies before acquiring transaction locks, then recheck
current identity, policy, authority and revision inside the mutation boundary.

`internal/purgehttp` owns authorized purge ([purge](purge.md)): the Owner
routes, the plan an agent prepares, running a confirmed plan, cleaning what
quoted the purged content outside the notebook, receipts, finishing purges
that stopped, and the guard that maps revisions a purge rewrote, which the
shared action dispatcher applies to every read-only action. Web opens the
notebook as its Owner through the shared tool store before an agent's plan is
made. Purge plans and history rewrites stay in `internal/workspace` and
`internal/notestore`. Chat transcripts, Jev records, Knowledge proposals and
bundle exports are cleaned through required hooks to their owners, read at
each call.

`internal/chat.Runtime` owns chat admission and deletion; `internal/reposource`
owns repository processing state. Web retains composition, instance policy and
cross-owner orchestration through concrete methods. These seams do not expose
mutable queues or add parallel processing lifetimes.

Every notebook store is backed by notestore: `workspace.OpenDB` and
`workspace.CreateDB` open a `*workspace.Store` over one `*notestore.Notebook`,
and the `db_*.go` files implement each method group on it. The public
`*workspace.Store` contract remains stable, including lifecycle and bounded
binary backups, which come from the notebook's Git export
(`internal/gitexport`). There is no owner RPC: every gateway calls the same
workspace methods, and each change is one notestore transaction.

Chat chooses one wire-format boundary when opening a configured provider.
`format_responses.go`, `format_messages.go` and `format_completion.go` each own
native request encoding, headers, response parsing and streaming. Credentials,
admission, cancellation, capability validation and usage accounting retain one
owner in `Provider`; application messages still expose semantic text and typed
tool calls independently of private native continuation data.

Existing conversation versions and opaque continuation bytes remain unchanged.
Native block kinds identify which protocol can replay that history: signed
thinking, encrypted reasoning and completion reasoning are never reinterpreted
through another wire format. Invalid or mismatched continuations fail before a
provider request. This requires no transcript rewrite or rollback migration;
continuing valid existing histories preserves signatures and encrypted data.

Notebook and repository BM25 now share `internal/retrieval.Lexicon`. The shared
owner builds bounded immutable postings and scores fields; notebook ranking
retains quoted framing, exact names and container order, while repository ranking
retains identifier tokenization, path scope and structural map roles. Citations
and visibility remain at their domain owners. Malformed chunks whose compact
frequency counter would overflow are rejected before they become evidence.

A synthetic 1,000-document build on an Apple M4 Pro (three iterations) changed
notebook allocation from 3,767,778 to 2,731,224 bytes and repository allocation
from 3,456,885 to 3,455,088 bytes. Notebook build latency was about 2.02 ms in
both versions; repository latency was about 3.36–3.45 ms. These small samples
support reduced duplication and notebook allocation, not a latency claim.

Grouped edits hash selected current preimages through one immutable batch and
prepare all deletions together with their discussions and retained history.
Historical bases keep Git's individual path-version semantics, including its
merge simplification; equal text after a later revert still conflicts. A
synthetic 12-note current-base group changed from 101 to 19 Git processes and
about 1.68 to 0.64 seconds holding the notebook lock. The deletion group changed
from 221 to 22 processes and about 3.30 to 0.66 seconds. These are small local
fixtures, not production latency guarantees.

Opening a note in the browser lists the notebook three times and reads the note
for its display, its live snapshot and its comments. Listings now take paths from
the committed HEAD tree their metadata already comes from, instead of running
`ls-files` each time. A file read is one `ls-tree -l`, one `cat-file` and the
version's `log`, and each notebook keeps its last 32 reads (up to 1 MiB) by
commit and path. Those are immutable, so writes need no invalidation; path
access, authority generation and the current byte budget are still checked on
every read. `TestNoteOpenGitProcesses` replays the browser's start-up requests:
the first open after a commit changed from 21 to 9 Git processes and a reopen at
the same commit from 18 to 2.

Engine consumers now use explicit `Indexes.Acquire` leases. `internal/search`
owns idle LRU retention and one fair processor; `internal/knowledge.Processing`
keeps only recoverable operational state while heavy corpus/vector/projection
memory is evicted. See [index lifetime and recovery](knowledge.md#index-lifetime-and-recovery).

A three-iteration serialization comparison of 1,000 synthetic 768-dimensional
vectors on the same Apple M4 Pro measured JSON at about 26.3 ms, 9.50 MB output
and 27.0 MB allocated per iteration. A float32 prototype measured about 1.68 ms,
3.14 MB output and 10.4 MB allocated. The prototype is an in-memory encoding
comparison without a manifest, checksum or fsync; it supports evaluating binary
persistence, not a production storage or request-latency claim.

Knowledge vectors now use one checksummed atomic binary file, with bounded
streaming read/publication and a pure stopped-state JSON export. Unlike repository
indexes, this single artifact does not need generation directories, a separate
manifest or a pointer swap. The compatible model/content-hash contract stays
unchanged. Actual synced publication and load measurements, migration recovery
and rollback limits are recorded in [storage migrations](storage-migrations.md).

Chat run admission keeps a volatile per-owner set of active conversations, rebuilt
from validated records during startup recovery. Successful publication and
validated reads reconcile the set; an ambiguous failed filesystem publication
fails conservatively. Completed sibling transcripts are no longer decoded by
`Start` or `Resume`. A synthetic 100-conversation/6.9 MB history fixture changed allocation
from 57.5 MB to 53.5 KB and measured Start from 43.6 ms to 10.2 ms on the same
host. These are small local fixtures; creation and usage summaries still scan
history when they need its contents. Runtime admission and Continue reconcile
only indexed active records, so the browser path does not reintroduce that scan.
