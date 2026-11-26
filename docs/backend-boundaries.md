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

An invocation's identity and scope travel on its context, each with the package
that owns its type:

- `chat.RunScope` and `chat.RunAuth` for a chat run
- `automation.Identity` for an automation run
- the credential, its scopes and the action API mark in `internal/auth`

Any package may read them. The checks that compare them with current sessions,
catalog access and instance policy stay in web: `checkRun`,
`checkAutomationAuth`, `checkInvocation` and `clientAccess`. Other packages reach
these checks only through Hooks.

The chat run's authorization, the credential readers and the action API mark
only narrow what an invocation may do. A chat run's scope and an automation's
identity decide what a run may reach:

- an automation's identity covers its own writes and its code-only data
  connections
- a person's approval of a chat operation lifts the personal-context refusal
- a wider or missing chat scope lifts the run's corpus limits

Their context keys are unexported. The functions that store, replace or clear
them can be used only where a test allows it, and anything may read them.

Two tests in web hold these boundaries:

- `TestContextAuthorityHasKnownSetters` lists every production file that uses
  one of those functions, each after its run's own check.
- `TestOnlyWebServesMCP` keeps tool registration in web, so `addAction` stays
  the one guard for MCP, the action API, and chat and automation tool calls.

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
through the shared action dispatcher, and `internal/messaginghttp` decides whose
memory a direct message uses, which is the messaging link. The required hooks bind the
personal-credential check, client notebook access, Jev and its authorization,
the chat embedding policy and the origin, each read at each call.

`internal/jev` owns Jev reviews and the organizer: review and organization
jobs, their scheduler, runner, retention and results, notifications, and each
person's and notebook's Jev settings. Web keeps the shared Jev admission, the
protected Inbox consent check and the instance AI policy; `internal/chatrun`
makes the citation checks a chat run asks for. The required hooks bind the browser identity, the instance
AI refusal, the configured judge, that admission and the Inbox check, the
policy's preferences, the instance AI sync and Quick Notes, each read at each
call. A run reaches notebooks only through the catalog, so it rechecks its
owner's access there. Its tests run on a real catalog, database and search
indexes without the instance's guard, so they cover Jev's behaviour; who may
use Jev is tested in web through the real routes.

`internal/messaginghttp` owns conversations in external chat clients: the
Slack app's events and interactions, the assistant runs a message starts, their
replies and sends, file artifacts and data write proposals with their
confirmation. Each run re-resolves the client's policy, the destination's
binding and the actor's notebook access in the catalog before provider calls,
tool calls and delivery. Web keeps the Slack app's configuration, its transport
(and the `fmn_omit_slack` split) and the MCP tools that configure chat clients.
It keeps `slackApp`, the one gate every Slack app request passes: `slackRequest`
checks the signature and timestamp before any handler reads the body. And it
keeps what binds a messaging user to an account: account links
(`message_link.go`) and email pairing (`message_pairing.go`). The required
hooks bind automation policy and identity, the answer a run gives, the chat
store and source checks, the personal assistant's message policy, linked owner
and memory capture, link commands and member events, data access, the origin,
the transports and the Slack app, each read at each call. Messaging's tests
stay in web, since a message reaches automation and chat; the few exports only
tests use are in `testexports.go`, and a boundary test fails on any production
reference to them.

`internal/personal` owns the personal assistant's worker: collection from
connected mail, calendars, documents and Slack, the run that judges items and
drafts replies, publishing drafts to the assistant notebook, profile memory
upkeep, Slack notifications, retention and the briefing. Web keeps every
consent, connection and account-binding check. The connect, callback and
disconnect routes and their binding cookies stay in `personal_connections.go`,
with the authority every provider call rechecks. The run's spec and its check
stay in web, and so do the checks the worker reaches through hooks
(`personal_access.go`): profile context, notebook access, source evidence and
freshness, grant generations, discarding work that became unavailable, mailbox
drafts and automation authority before each state save. The MCP tools and
account pages stay in web too. The required hooks bind those checks, the
automation answer and calls, Jev, the connections, the origin and the Slack
app, each read at each call. The worker's tests stay in web, where a run passes
those checks. The few exports only tests use are in `testexports.go`. Boundary
tests fail on a production reference to them, on a grant begun, finished or
removed outside the connection routes, and on a cookie handled outside web.

`internal/chatrun` owns embedded chat: its routes, the runner that starts a run
and executes the model's tool calls, approvals and the other controls,
deletion, the rounds of a run, fitting each model request to its connection,
retrieval and research, the evidence and provenance of tool results, review
workflows, the run's terminal state and the Jev citation check of an answer.
Web keeps every authorization check: the request's identity, the run's session
and its recheck (`chatRunAuth`, `checkRun` and `runContext`), client notebook
access, data citations, source scope, Jev authorization and personal context.
The chat leg of the guard (`scopeChatActionArguments`, `guardChatAction`,
`chatPersonalApproval` and `chatReadOnly`) stays beside the dispatcher. A
model's tool call reaches an action only through the `DispatchAction` hook,
which web binds to its dispatcher; the run's executor rechecks the run before
and after it, and the dispatcher applies the guard. The executor also asks the
dispatcher for canonical arguments and the guard's preflight of a change. A
boundary test fails if the engine reaches the action registry or another hub's
action methods. The required hooks bind those checks, the registered tools a
run may offer, knowledge engine access, scoped retrieval, Jev preferences,
source work admission, the chat runtime, citation checks, data connections,
repositories and the origin; those set after startup are read at each call. The
engine's server-level tests stay in web; the exports only tests use are in
`testexports.go`, and a boundary test fails on any production reference to
them.

`internal/automationrun` owns the automation run engine: the dispatcher that
runs each queued run by its kind, and the runtime's start; the model turn of an
answer, with its tool loop, provider and capacity; publication to the run's note
and notification through the messaging hub; and the task, recipe, digest,
monitor, checks, native and Drive kinds. Web keeps every authorization check:
a run's identity and its recheck (`automationContext` and
`checkAutomationAuth`), the instance AI policy (`automationPolicy` and
`instanceAIWriting`), the recheck of a run's evidence
(`checkAutomationEvidence`), client notebook access, data connection grants,
and personal context and consent. It also keeps definition validation, which
the automation tools and the package installer run, and the MCP and
organization tools with their administrator and `notes:manage` checks. An
automation answer's tool call reaches an action only through the
`DispatchAction` hook, which web binds to the same dispatcher as chat, and a
boundary test fails if the engine reaches the action registry or another hub's
action methods. The engine binds a run's source scope (`scope.go`) after its
actor's read check on each notebook, and clears it to recheck a direct
message's recipient as that recipient (`publish.go`);
`TestContextAuthorityHasKnownSetters` lists both. The required hooks bind those checks, the
dispatcher, content scope, source bindings, knowledge engine and source work
admission, the personal assistant's jobs and email pairing. The chat runtime,
data connections, repositories, backups, messaging, the personal assistant,
task queues and the Slack app are read at each call. Packages, scripts,
learning and repair stay in web for now: the engine reaches them through
temporary hooks, and they use the engine's exported answer, call, publication,
notification and check methods. The engine's tests stay in web; the export
only tests use is in `testexports.go`, and a boundary test fails on any
production reference to it.

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
