# Postgres transition

ForgetMeNote is moving its instance control plane from files on one volume into
Postgres, so that several gateway processes of the same binary can serve one
instance and gateway deploys can roll without downtime. With storage v2 notebook
content moves too: each notebook's revisions keep Git's object model in Postgres,
with large file contents in blob storage ([notebook content](#notebook-content)).
Self-hosting runs every role in one process beside a Postgres container from the
standalone compose preset.

This is Phase 1 of the scalable architecture design. Every instance has since
moved onto Postgres, and the deployment rules in
[engineering rules](engineering-rules.md) apply unchanged.

## Shape

- `internal/pg` owns the connection pool, the embedded schema migrations and the
  reader-version check. It is the only package that opens database connections.
- Each owner package keeps its types and rules and stores its records in its own
  tables. On this branch a store moves outright: its file I/O is replaced by
  Postgres I/O, and an instance requires its database. `forgetmenote
  import-files` brings an existing instance's file state across once, while it
  is stopped, and `export-files` writes it back for a file-based release (see
  [moving file state](#moving-file-state)).
- The catalog stays one document: the versioned `catalog`/`state` row of
  `documents` per org, with its activity receipts in `catalog_activity` (the
  newest 10,000, as the file-based log kept) and the
  instance logo in `catalog_logos`. A change runs the existing rule code under the
  process's catalog lock, then writes the document, its new receipts and its logo
  in one transaction, and only over the version it was computed from. A change
  that another gateway committed first fails as unavailable, and the next call
  reloads. An activity receipt (every data call records two, reads included) or
  a first-use account decides nothing from the catalog, so it reloads and is
  written over the stored document instead of failing. Committed changes are
  therefore serialized just as the single lock serializes them today. Before
  deciding, a gateway checks the stored version at
  most once per 250 ms freshness window. So another gateway's change, a revocation
  included, takes effect here within that window; changes made through this
  gateway take effect at once. While the database is unreachable the catalog
  grants nothing. Text Postgres cannot store (a NUL character) is refused as bad
  input. Splitting the document into tables comes later, where size, write
  contention or a shared cell needs it.
- Smaller stores that kept one JSON file become one versioned document through
  `pg.Doc`: each call under the store's lock rereads the document only when
  another gateway changed it, and a write succeeds only over the version it was
  computed from. Such a store fails closed while the database is unreachable.
- Fences are rows. Background loops hold lease rows with a generation that each
  state change checks in the same transaction. Session advisory locks fence
  nothing and are used only to serialize `forgetmenote migrate`.

## Leases and presence

The `leases` table names what one gateway does at a time. `pg.DB.Duty` runs a
background loop only while this gateway holds the loop's lease: it renews the
lease every quarter of its period and stops the loop at half the period without
a renewal, before the database would let another gateway take it over. A
takeover raises the generation, and `Lease.Fence` checks it in a write's own
transaction. Duties today: `automation-scheduler` (scheduling, events and
recovery; every gateway's workers claim runs), `quicknotes-routing`,
`jev-scheduler`, `repository-refresh`, `backup-expiry` and `chat-recovery`.

Each gateway also keeps its presence, `gateway/<holder>`, renewed every ten
seconds for thirty and removed when it closes. Work a gateway takes on records
it as the holder: a running automation run, a queued or running source job, a
chat run, a capture being saved, filed or routed, and a personal grant being
refreshed. A store that opens interrupts or requeues only its own gateway's
earlier work and that of gateways whose presence is gone; another gateway's live
work keeps running there. After a crash, others recover its work within thirty
seconds of its last renewal.

`pg.DB.Exclusive` holds a transaction-scoped advisory lock on a connection of its
own, so a critical section that one process mutex guarded is serialized across
gateways. The automation queue takes it for every change. Knowledge embedding
takes `TryExclusive` per notebook: a gateway whose batch is already being
embedded elsewhere skips it and adopts those vectors on its next pass. These
locks are no fence: if their connection drops, the section continues unlocked,
so writes that must not overlap still check their own versions.

The locks that once guarded one process now hold for the instance:

- A notebook's authority handoff runs under `TryExclusive` on
  `authority-handoff <notebook>`, so one gateway at a time applies, cancels or
  recovers it, and a second call is refused as a conflict. Startup recovery
  leaves a pending handoff whose lock another gateway holds to that gateway, and
  rereads the catalog under the lock before acting. The notebook's
  `notebook_authority` row is the durable fence: each write of it names the
  version it read, so no gateway replaces another's record unseen.
- A notebook import, through the browser or MCP, takes `notebook-import`; while
  another gateway imports, the request is refused with 429 as before. The
  import stores the bundle's history only into a notebook ID the store does not
  hold yet.
- Creating or importing a notebook takes `TryExclusive` on `notebook-id <ID>`:
  a creation until it published the notebook, an import from before it checks
  that the store lacks the ID until it published or removed what it stored.
  Another creation or import of the ID on any gateway meanwhile is a conflict,
  so a failed import never removes a notebook another gateway created, and a
  creation never takes over a notebook an import is still storing. As a second
  safeguard, a failed import deletes only the incarnation of the notebook its
  own first commit created, and a publication that failed keeps a notebook the
  reloaded catalog lists.
- The scheduled sweep of unused content blocks takes `TryExclusive` on
  `notestore-block-sweep`, so one gateway at a time sweeps
  ([notebook content](#notebook-content)).
- A conversation's deletion takes its conversation lock with `TryExclusive` and
  a citation check holds it with `pg.DB.Shared`, so a deletion through one
  gateway answers 409 while another checks that conversation's citations, and
  a check that starts after a deletion elsewhere finds the conversation gone
  and records nothing. The in-process fences still order this gateway's own
  readers.
- A personal grant's refresh takes `personal-refresh <grant>` once the token is
  due, so a request on another gateway waits for the refresh in progress and
  then uses its token. A per-process mutex keeps one request per gateway waiting
  on the database; the in-flight marker still guards a refresh whose gateway
  stopped.
- Every write of a judgment record takes a transaction-scoped advisory lock on its
  name in the writing transaction, which also covers a record not yet written,
  and a read-modify-write of judgment settings, jobs, results, read notifications or
  citation checks reads the record in that same transaction. So updates of one
  record apply one after another on every gateway, and a removed record stays
  removed. The lock ends with the write's transaction, so unlike `Exclusive` it
  fences the write. Changes computed from an earlier read check what they read:
  a job's checkpoint is refused once the job was cancelled anywhere, and an
  organization settings change, Undo or dismissal, or receipt reconciliation
  over a changed record is refused as a conflict, so the next call reads it
  again. A job's result at its cursor is still written whole: nothing reads it
  until the job's checkpoint moves past it.

Each held lock keeps a pool connection for as long as the work it guards, up to
a minute for a citation check, so a `DATABASE_URL` without `pool_max_conns`
gets a pool of at least 16 connections rather than pgx's default of four.

A cancellation written on another gateway reaches the runner within a second:
automation and chat runners watch their run's row while they run.

## Pending flows and request limits

A provider flow, between its start and its callback, is a `pending_flows` row,
so the provider may return the browser to any gateway. Its key is the digest of
the flow's state, or of its binding for a grant awaiting selection, and it
expires with the flow. A kind's group, before its first `/`, bounds its live
flows at 256: a subject's new flow replaces its earlier one in the group, and a
full group refuses new flows, except anonymous OIDC sign-ins, which evict the
soonest to expire. A callback completes only a flow whose browser binding
matches, then removes it, so a flow completes once. Kinds: `oidc/login`,
`github-link/pending`, `repository/pending` and `repository/grant`,
`backup/pending` and `backup/grant`, `connection/ticket`, and
`personal-<provider>/pending`. GitHub user tokens in grants are sealed under
their App key and personal flows under the personal credential key; OIDC and
GitHub PKCE verifiers and nonces are stored as they are, for at most their ten
minutes. A ticket starts once and a grant records its notebook creation under a
row lock; repository creation and import take one instance-wide exclusive lock.

Password, OAuth and OIDC-start limits are `rate_windows` rows: one-minute
windows per digest of the limited key, in groups `passwords`, `oauth-requests`,
`oauth-revocations`, `oauth-grants` and `oidc-starts`, each holding at most 1024
windows. Every gateway charges the same windows, so a limit holds for the
instance. The bound on concurrent password checks stays per process.

## Legacy MCP sessions

A 2025-11-25 MCP client's session is an `mcp_sessions` row keyed by the digest
of the session ID the client holds, so any gateway serves its requests and a
deploy does not end it. The row records the credential that began the session
(its OAuth consent family or credential digest), the client's initialize
parameters, its revision subscriptions and its expiry: five minutes after the
last request, extended every second while a GET stream is open on any gateway.
A credential keeps at most 64 live sessions, and a new one evicts the one idle
longest. A gateway that does not hold the session in memory recreates it from
the stored initialize parameters before serving the request. Every gateway
answers another credential's request with 403 and a missing or expired session
with 404, and DELETE removes the row for all of them.

ForgetMeNote serves the GET stream itself. It delivers
`notifications/resources/updated` for the row's subscriptions, rereads the row
every second so it delivers subscriptions made through any gateway, and on
shutdown asks the client to reconnect after a second, which the load balancer
sends to another gateway. Clients of the 2026-07-28 protocol stay stateless.

A client's `notifications/cancelled` may reach a gateway other than the one
running the request it names. The gateway that receives it, once the session
and credential checks above pass, cancels the request if it runs it and
announces `pg_notify('mcp_cancellations', '<org>/<session digest> <request ID
as JSON>')`; no session ID or credential travels. Each gateway listens on a
connection of its own outside the pool, named `forgetmenote listener <holder>`
like its revision listener, and posts an announced cancellation to the SDK
session it holds for that stored session, which cancels the request if it runs
there. The announcement adds one database round trip, so the call sees its
cancellation well within a second. A request ID longer than 1 KiB as JSON,
which keeps the payload far below Postgres's 8000-byte limit, is cancelled only
on the gateway that receives the cancellation. Announcements are hints: a
gateway hears nothing while its listener reconnects, so a call whose
cancellation was announced then runs to its end, as it would had the client
never cancelled.

## Revision announcements

Every notebook write commits in one transaction and then announces the notebook
with `pg_notify('notebook_revisions', '<org>/<notebook>')`. Each gateway
listens on a connection of its own outside the pool, named
`forgetmenote listener <holder>` in `pg_stat_activity`, and its notes store
rereads an announced notebook's head and updates that notebook's read model.
The revision hub, search and knowledge watches then see the new revision as they
do a local one, so an announcement is only a prompt and never a revision. The
store still checks every five seconds for revisions nobody announced, and after
reconnecting a listener has every read model check once, for announcements it
missed.

Live documents announce each accepted update, compaction and new epoch on
`notebook_live`, as `<org>/<notebook>/<entry>/<epoch>/<seq>/<base>`. A gateway
holding that document reads the updates it lacks and wakes its waiting live
readers; the stored document stays the authority.

## Chunked exports

An export of a notebook's Git bundle in base64 chunks is an `exports` row and
the bundle as one object, `exports/<id>`, in blob storage, so the next chunk
may come from any gateway: each chunk is a ranged read of the object. The
gateway that begins it writes the bundle to a temporary file under
`cache/exports`, commits the row, stores the bundle and removes the file. An
export belongs to the user, notebook and bundle digest that began it, lasts
fifteen minutes and ends with its last chunk. At most two live exports per
instance, of at most 1 GiB each, are counted under one lock. Expired exports
are removed by the gateway that began them, every minute while any is live, by
the next export, and by each gateway as it starts. Each sweep lists the stored
bundles before it reads the rows and removes the bundles with no row; since a
bundle is stored only after its row commits, it never removes a live export's.
The `export_chunks` table of the first Postgres release is no longer written
and is dropped by a later migration, once no binary reads it.

## Source job files

A source job is a `source_jobs` row holding its manifest, and its original,
page checkpoints and result are objects in blob storage at the paths the
file-based release kept them at: `source-jobs/<id>/original`,
`source-jobs/<id>/page-00001.json` and so on, and `source-jobs/<id>/result.json`.
So any gateway reads a job's result and original, resumes it from its last
checkpoint once its gateway is gone, and expires it. Conversion scratch is a
gateway's own working space, under `cache/source-jobs/<id>` in its data
directory, which the gateway empties when it opens.

Each start of a job is a numbered attempt that the row records with its
gateway. A run changes the row only over the version it read while the row
named its gateway and attempt, so a gateway whose presence lapsed while it
still runs the job, as in a network partition, has its next write refused once
another gateway resumed the job, and stops; it leaves its scratch to its own
cleanup. Every object is charged in the row before it is stored. The first
run stores at the paths above; each later run stores under
`source-jobs/<id>/attempt-<n>/`, and the row names the run whose checkpoint
each page is, the run that downloaded the original, and the result of the run
that finished the job. So the one object a lost run may still store after
losing, the checkpoint, original or result it was storing, never replaces an
object the row names and is never read; it stays charged until the sweep
removes it with its job. The export writes the objects the row names at the
file-based paths.

Objects are stored only while their job's row exists. An upload's row commits
first, in a `staging` state no gateway serves or exports, then the original is
stored and the job is published as queued; every later object is written by
the run that holds the job. Retirement deletes the row first and the
objects after. Job ids are random and never reused, so a sweep that lists the
objects before it reads the rows, and removes the objects whose id then has no
row, removes only a retired job's objects, never those of a job another gateway
is creating. An interrupted upload leaves a `staging` row, which recovery
retires once its gateway is gone; an interrupted retirement leaves objects
without a row. Each gateway sweeps when it opens, after each retirement and
every hour, apart from its worker, so admission never waits on removing a
job's objects. Admission charges a finished job the bytes its manifest records
as stored, and any scratch a failed cleanup left on the admitting gateway.

## Blob storage

Files too large or too opaque for rows, such as source job originals, pages and
results, repository source generations and retained evidence, and export
bundles, are objects in blob storage (`internal/blob`), keyed by the path the
file-based release keeps them at. Without `server.blobs` the store is the data
directory itself, so a single host keeps its files where they always were. A
`directory` names another one. `s3` names a bucket and a prefix under it that
holds this instance's objects; gateways that share an instance need it, since
each has only its own disk.

- Objects are written whole: a directory store writes a dot-named temporary
  beside the object, syncs it and renames it over the object, and S3 replaces
  an object atomically. A reader sees the old object or the new one.
- Requests to S3 are signed with Signature Version 4 over each payload's
  SHA-256, which S3 checks, using the AWS default credential chain: keys in the
  environment first, then a pod's web identity, then the node's role. No
  credential is configured in the instance. Where the environment's keys
  belong to something else, such as a Redshift connection, `roleArn` names the
  IAM role the store assumes instead with the pod's web identity token
  (`AWS_WEB_IDENTITY_TOKEN_FILE`, which EKS mounts for a service account with
  an IAM role). The exchange is unsigned and its sessions are named
  `forgetmenote-blobs`; a role without that token refuses to open. Throttled and
  failed attempts are retried three times with backoff. The client is the REST
  API over the already-pinned AWS signer, rather than the SDK's S3 module,
  because the store needs only five calls; the web identity exchange uses the
  SDK's own STS client and provider, already pinned for the default chain.
- The bucket keeps the objects safe: enable default encryption, versioning and
  the public access block, and grant the instance's role or IAM user
  `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket` on its
  prefix only. The store never reads old versions, so add a lifecycle rule that
  expires noncurrent versions, 30 days for example, or they accumulate. That
  rule sets how long an overwritten or deleted object stays recoverable outside
  a recovery point.
- An `endpoint` addresses an S3-compatible service path-style, for tests and
  self-hosted object stores; plain HTTP needs `allowHTTP`.

## Notebook content

With storage v2 each notebook is a revision log in these tables
(`internal/notestore`, migration `0017_notestore.sql`) and in blob storage.
They keep Git's object model, so the
[Git export](backups.md#git-export-repositories) reproduces every commit byte
for byte and a revision ID is still its 40-hex commit SHA.

| Table | Holds |
| --- | --- |
| `notebook_heads` | Each notebook's head: its newest revision's sequence number and commit SHA. Every write locks this row first. `incarnation` grows each time a notebook ID is created, so a gateway tells a re-created notebook from an older revision of the same one. |
| `revisions` | Every revision in the order it was written or imported: the exact commit object, its tree, first parent and time, and the author and trailers that queries use (actor, source, client, operation and request IDs). |
| `git_trees` | Tree objects byte for byte, shared by the revisions that contain them. |
| `revision_changes` | The paths each revision added, modified, deleted or renamed against its first parent, under each file's stable entry ID (a note's `fmn-id`), so a note's history is one index range that follows renames. |
| `entries` | Each file at the head: its path, blob, mode, size, kind and `Version`, the commit that last changed it. |
| `file_versions` | File contents by Git blob ID, once per org: inline when under 8 KiB, otherwise the list of the content segments that hold its blocks. |
| `content_segments`, `content_blocks` | Runs of a large file's block list, and every block a write put into blob storage. |
| `live_docs`, `live_updates` | Each live note's Yjs snapshot, epoch and base revision, and the updates accepted after the snapshot, with who made each. |
| `operation_receipts` | Results of operations a client may retry (captures, edit groups, live checkpoints), bound to the request they answered and kept until 30 days after they finish. |
| `revision_rewrites`, `purges` | For each [purge](purge.md), every old commit and the commit that replaced it, so references to old SHAs keep resolving, and who ran it, when, why and over what scope. Nothing derived from the purged content is kept. |
| `notebook_authority` | Migration `0018`: a notebook's authority handoff record and, while a handoff to ForgetMeNote is prepared, the validated snapshot it installs. |

A file of 8 KiB or more is kept as blocks in blob storage under
`blocks/<sha256>`. Markdown is cut after blank lines, packing paragraphs into
blocks of at least 1 KiB and at most 64 KiB, and any other file every 4 MiB. The
block list is itself cut into content-defined segments, so a change to one
paragraph of a 1.2 MB manuscript stores about one new block and one segment,
not a new copy of the file. A write records each block in `content_blocks` and
puts it into blob storage before the transaction that references it commits, so
blocks are idempotent and a sweep (`SweepBlocks`) finds the ones no file version
uses without listing the store. It never removes a block a write touched in the
last ten minutes.

A [purge](purge.md) removes the blocks only it used right after its transaction
commits, even if the request that started it has ended, and tries again twice.
Blocks that removal could not delete, those a write put before it was refused,
for example as a conflict, and those of a deleted notebook (a failed import, or
a creation the catalog did not publish) stay in blob storage until the scheduled
block sweep: every gateway tries it about five minutes after it starts and then
once a day, and one gateway of the instance at a time runs it (a Postgres
advisory lock). A delete brings that gateway's next sweep forward to about
eleven minutes later, once the blocks it freed are past the grace period. A
purge whose block removal failed says so in its warnings: the purged content is
then still in blob storage, and in any copy of it, until that sweep has run;
restarting a gateway runs it about five minutes later. The log records each
sweep that removed something (`notes store block sweep removed …`) and each that
failed. A sweep reads the block lists of every file version and segment of the
instance into the gateway's memory: on F2 (2,000 notes, 20,000 revisions, 20,181
blocks in a blob directory), one that found every block in use took 16 ms and
about 9 MiB more heap, and one that removed every block of the deleted notebook
took 0.75 to 0.91 s and about 29 MiB (`BenchmarkBudgetSweepF2`). With S3, each
removed block is one request.

A blob directory writes each object through a temporary `.blob-*` file beside
it, and each notebook's knowledge caches (`DATA/projects/ID`) are written
through `.knowledge-*` and `.vectors-*` files. A crash can leave one, holding
notebook content; a gateway that starts, and every purge, removes those in
`blocks/`, `exports/` and the knowledge caches that have not changed for a
minute. Temporary files under the blob directory's other prefixes, such as
`repository-sources/` and the document conversion jobs' `source-jobs/`, are not
swept: they hold no notebook history a purge rewrites, and an operator can
delete a `.blob-*` file there that has not changed for a day. A bundle import's
upload over 1 MiB is spooled to the system's temporary directory, which the
shipped Compose files mount as a tmpfs (`/tmp`, 256 MiB): it is never written to
the data directory and does not survive the container stopping. Keep `/tmp`
larger than `FMN_LIMIT_BUNDLE_BYTES` if you raise that limit.

A write is one transaction: it locks the notebook's head row, checks the head or
note versions the caller expected, stores the new trees and commit computed in
the gateway, the changed paths and file versions, then moves the entries and the
head. A failed expectation is a conflict and stores nothing. Reads come from a
per-notebook read model in memory and a block cache, both bounded, which local
writes and [revision announcements](#revision-announcements) keep current.

`pg_dump` holds every table above; the blocks are in blob storage. Back up and
restore both from the same point: a file version whose blocks are missing
cannot be read. See [backups](backups.md#complete-instance-backup).

## Moving file state

`forgetmenote import-files [-data DIR]` copies a stopped file-based instance's
control-plane state into its database, and `forgetmenote export-files -output
NEW_DIRECTORY` writes the database's state back in the file-based release's
formats. Each owner package converts its own files (`ImportFiles` and
`ExportFiles` beside its store) through `internal/filestate`; notebook
repositories and attachments stay on their volumes. An area's blob files move
with it: the import stores them in blob storage when that is not the data
directory, and the export copies them into its directory.

- The import runs in one transaction into a database whose tables hold no row
  of the instance's org, including a gateway's presence, and holds the data
  directory's `catalog.lock`, so a running file-based process is refused. A
  failed import leaves the database empty. It reads every file the file-based
  release reads, with that release's checks, and refuses state only that release
  repairs, such as an interrupted catalog change: start it once, stop it, and
  import again.
- The export reads one read-only snapshot after checking that no gateway is
  present, and creates its directory, never replacing a file. Runtime state that
  only gateways keep (holders, attempts, leases, active flags, row versions) is
  translated to what the file-based release expects after a restart.
- Both print, for each area, the number of files and a SHA-256 digest of their
  canonical content (JSON with sorted keys, or the area's own deterministic
  form). An export of a fresh import prints the import's report, so an operator
  compares the two instead of trusting either. Personal grants count by an HMAC
  under a key derived from the personal key, so a kept report reveals nothing
  about their tokens.

Areas run in order: the catalog, then knowledge, which finds each notebook's
engine directory (`projects/<id>`, or `owner-state/<id>` for a notebook on an
owner) in the catalog document, then sign-in state, chat, automations, source
jobs, repository sources, Quicknotes, judgments, personal connections and managed
backups. Some refusals ask the operator to act first:

- A judgment call still reserved, or an interrupted catalog change, is repaired only
  by the file-based release: start it once, stop it, and import again.
- Personal connection grants open with `integrations.personal`'s key file, so
  an instance with `personal-connections/credentials.enc` needs that
  configuration when it imports or exports.
- Files the file-based release never reads, such as lock files and the
  temporaries an interrupted write left, stay behind, as do a source job's
  conversion scratch, an interrupted admission or retirement (`.staging-<id>`,
  `.retired-<id>`) and a job directory without a manifest; a gateway whose
  data directory holds them removes them when it opens, as that release did.
  Any other unexpected entry in an area's directory is refused rather than
  skipped: move it out of the data directory and import again.
- Text with a NUL character cannot be stored and fails the import, naming its
  file.

The cutover from a file-based release: complete a backup of every volume, stop
every process, run `forgetmenote migrate` and `forgetmenote import-files`, keep
the report, run `export-files` into a new directory and compare its report, then
start the gateways. The import reads a volume mounted read-only, so the stopped
release can restart on it untouched. The later storage v2 cutover moved
notebooks out of their Git repositories into the database; this release no
longer has its import, so an instance still keeping notebooks in Git upgrades
through an earlier release with `forgetmenote admin storage import` first.

To roll back after writes resumed, stop every gateway and run `export-files`.
On a copy of the file-based release's volume, first remove the state files the
status table below lists for each area, keeping notebook repositories,
`automations/python` and the blob files beside repository source state, then
copy the export over it. Removing first matters:
a record deleted after the cutover would otherwise return from the files the
import left in place. The export does not revoke a managed backup lease that is
still active; its GitHub token expires within the hour it was minted for.

## Outgoing paused

A copy of an instance restored beside the live one, for example to rehearse a
cutover with testers signed in, holds the live instance's integration state:
Slack bindings, personal OAuth grants, backup bindings and in-flight lease
tokens, queued automation runs and judgment reviews. Set `server.outgoing` to
`"paused"` (legacy `OUTGOING=paused`) on that copy so it cannot act outside as
the live instance. `"enabled"` or no value is the ordinary mode; any other value
stops startup. The mode is installed before every integration, so none runs
unpaused even briefly. A paused gateway logs `ForgetMeNote outgoing actions are
paused` at startup, and Administration and `organization_status` report
`outgoingPaused` ("Outgoing actions: Paused").

A paused gateway takes none of these duties:

- `automation-scheduler`, its workers and package refresh: no schedules, events,
  digests, alerts, Slack deliveries, personal assistant runs or publication.
- `jev-scheduler`: no queued or automatic judgment reviews or organizer runs.
- `repository-refresh`: no source refresh, and none of its paid embedding and
  map enrichment.
- `backup-expiry`: copied leases are not revoked; their tokens expire on their
  own.

It keeps `chat-recovery`, which only interrupts the runs of gone gateways, and
`quicknotes-routing`, which still recovers interrupted saves but sends no capture
to judgments.

These refuse with HTTP 503, code `outgoing_paused`, "outgoing actions are paused
on this instance", before any outside request, recorded intent or token:

- Slack: signed events, so no reply is queued and no knowledge event observed
  (URL verification still answers), and every call with the bot token.
- Automations: `run_automation`, `test_automation` and every run, including one
  copied while queued.
- Personal connections: reads, writes, drafts, connect and callback. A refresh
  can rotate a token the live instance still uses, and a new grant mints another
  for the same OAuth client. Disconnect is local and stays available.
- Data operations that are not read-only: webhooks, Gmail and Sheets
  submissions, MCP writes.
- Managed backups: worker leases, so no installation token is minted, lease
  checks and receipts, destination setup, repository creation and controls; the
  browser setup refuses before GitHub authorization. A copied token is never
  validated or revoked; an authority handoff still fences the copy's own leases.
- Judgment reviews: starting or continuing one; cancelling stays available.
- Background document embedding, for notebooks and repository sources.
  Knowledge status reports the pause; vectors restored with the copy still serve
  search.

Deliberately left running, because each is a tester's own request or only reads:

- Chat model calls, research, website capture and the citation checks a chat run
  starts.
- Search query embeddings and judgment reranking, memory review when an agent calls
  `remember`, and an explicit Quicknotes Sort.
- Read-only data operations, including a connection's static OAuth refresh;
  connections that rotate refresh tokens are already refused.
- GitHub reads for Sources: the installation-token check on each repository
  read, connect, import and a manual refresh, whose paid indexing is background
  and paused.
- Sign-in through OIDC or the trusted proxy, and GitHub identity linking.
- Local and public-URL source extraction, automation package installation,
  exports, and inbound GitHub webhooks, which only mark a source pending.

The mode does not move notebooks: give the copy its own data directory,
database, blob store prefix and `PUBLIC_URL`. Slack and GitHub keep delivering events to
the live instance's URLs. Remove the setting only on the instance that becomes
live: an unpaused copy resumes the runs and reviews it copied as queued.

## Self-hosting

The compose file runs a pinned official `postgres` image beside the app, on the
compose network only, with its data in `./postgres`. Create its two secrets once,
before the first `docker compose up`, as the user that runs the containers:

```sh
mkdir -p secrets postgres && chmod 700 secrets postgres
openssl rand -hex 32 > secrets/database-password
printf 'postgres://forgetmenote:%s@postgres:5432/forgetmenote?sslmode=disable\n' "$(cat secrets/database-password)" > secrets/database-url
chmod 600 secrets/database-password secrets/database-url
```

The app container runs `forgetmenote migrate` before it serves. The standalone
preset (`deploy/standalone/operator.py`) generates both secrets and the service.
Back up the database with the volume: `docker compose exec postgres pg_dump -U
forgetmenote forgetmenote > forgetmenote.sql` while the app is stopped, or with the
volume copy of a stopped stack.

## Migrations

Migrations are numbered SQL files in `internal/pg/migrations`, embedded in the
binary and applied by `forgetmenote migrate`. Serving never migrates: it refuses a
database that lacks any of its migrations, so a rollout, or a single-process
install's startup command, runs `migrate` first. Each one must
work with both the running and the next binary: add, switch readers, then remove
in a later release. A change of meaning that an older binary would not enforce,
such as a new privacy rule, raises `min_reader_version` once every replica runs
the new binary (`forgetmenote migrate -finish-rollout`); a binary below it
refuses to start.

## Tests

Tests that need Postgres call `pgtest.Database(t)`, which returns a fresh database
cloned from a migrated template, or `pgtest.Org(t)`, a shared migrated database
under an org of the test's own, from a package whose `TestMain` calls
`pgtest.Main`. `pgtest.FailWrites` makes Postgres refuse one kind of write, as a
full disk or lost primary would. Each test binary starts one disposable cluster in
a temporary directory with the host's `initdb` and `postgres`, listening only on a
Unix socket, and stops it when the binary exits; `FMN_TEST_DATABASE_URL` points
the tests at an existing server instead. Missing Postgres binaries fail these
tests, as a missing Git executable would. Browser tests start one cluster per
Playwright run and give each app a database that `forgetmenote migrate`
prepares and that baseline and reset snapshot with `CREATE DATABASE … TEMPLATE`.

## Status

| Area | Store today | Postgres |
| --- | --- | --- |
| Foundation | — | Pool, migrations, reader version and test clusters (slice 1) |
| Sign-in state | `oauth.json` | One versioned `oauth` document: clients, codes, tokens, sessions and API credentials (slice 2) |
| Self-hosting | — | Compose and standalone preset run a pinned `postgres` service; the app migrates before serving (slice 2) |
| Catalog | `catalog.json`, `activity.jsonl`, `catalog-logo-*.bin` | One versioned `catalog` document with `catalog_activity` receipts and `catalog_logos` (slice 3) |
| Notebook content | A Git working tree per notebook, `projects/<id>/workspace`, with its write journal, collaboration files and capture records, on the volume of the gateway or of a notebook owner process | `notestore` tables (migration `0017`): revisions as exact Git commits and trees, file versions and content blocks in blob storage, head entries, live documents and updates, and operation receipts. Every write is one transaction that locks the notebook's head row, so every gateway serves every notebook and owner processes, the owner map and `move-notebook` are gone. `admin storage import` moves each repository across once ([storage v2 cutover](storage-cutover.md)) |
| Notebook authority | `authority.json` and `authority-stage.json` beside each working tree | `notebook_authority` rows (migration `0018`), written only over the version read |
| File state | Every file above | `import-files` copies a stopped file-based instance into an empty database in one transaction, and `export-files` writes the rows back in the file-based formats, each reporting per-area file counts and digests (slice 7) |
| Quicknotes | `quicknotes/<account>/` record and settings files, `quicknotes.lock` | `captures` rows, one per capture, and `capture_settings` rows, each written only over the version or generation it read; the routing scan runs on the lease holder and leaves captures another live gateway is saving, filing or routing (slices 4 and 5) |
| Judgments | `jev/usage.json` under `usage.lock`, and named records under `jev/` | `jev_usage` rows keyed by attempt, so an attempt is reserved once across gateways and settled only by its reserver; a full ledger moves its oldest settled attempts into daily `jev_usage_totals`, which `export-files` leaves behind. `jev_records` by name. A record's read-modify-write runs in one transaction under the record's lock, which every write of it takes, so no gateway overwrites another's committed change; the scheduler runs on the lease holder (slices 4 and 5) |
| Automations | `automations/config/jobs.json` and the run, effect, repair, alert and learning files of `automations/` | One versioned `automation-definitions` document and `automation_records` rows by the same names, each written only over the version read, so a write that outlives a dropped queue lock is refused rather than overwriting another gateway's; prepared Python environments stay in `automations/python` on each gateway, which prepares an installed package on first use. Queue changes are serialized across gateways by an advisory lock, each running run records its gateway, and only the runs of gateways that are gone return to the queue. Each gateway's claimer hands runs to its workers when a queue or job change is announced on `automation_queue`, with a five-second fallback scan; one gateway schedules (slices 4 and 5) |
| Source jobs | `source-jobs/`: each job's `job.json` beside its original, page checkpoints, result and scratch | `source_jobs` rows hold manifests, and admission counts every gateway's jobs under one advisory lock. Each job's original, page checkpoints and result are objects in blob storage under their file paths, so gateways with disks of their own share them through S3, and any gateway reads, resumes and expires another's job; scratch stays in each gateway's `cache/source-jobs`. An upload's row commits, unserved, before its original is stored and retirement deletes rows before objects, so sweeps remove only retired jobs' objects (see [source job files](#source-job-files)). Each queued or running job records its gateway and attempt; opening and a periodic sweep interrupt only the jobs of gateways that are gone, and a run's writes are refused once another gateway resumed its job, whose later runs store their objects under their own attempt (slices 4 and 5) |
| Repository sources | `repository-sources/<notebook>/state.json` beside each source's Git cache, index and retained evidence | One versioned `repository-sources` document per notebook, reread under the notebook's lock so another gateway's fence or release is seen before acting, and written only over the version read. The refresh scheduler, with its paid embedding and map enrichment, runs on the lease holder (slices 4 and 5). Each source's index generation, whose vectors and map labels were paid for, and its retained evidence are objects in blob storage under their file paths; with the default store they are the directory's own files, copied nowhere. A generation is stored whole, manifest last, before the document names it, and the one it replaced is removed once the document names its successor. A gateway lacking the named generation fetches it, checks it against its manifest and serves it; it fetches its own Git mirror of the named commit. Git mirrors, `index/current.json` and lexical indexes are each gateway's rebuildable cache. The import stores the generation `index/current.json` names and the evidence, and the export writes them back with that pointer |
| Chat | `chat/users/<account>/conversations/<id>/state.json` and `recovery.json`, under a gateway-exclusive `.lock` | `chat_conversations` rows holding each record's exact bytes and recovery overlay, written only over the version read. The row version is the stream generation and an `active` flag replaces the in-memory admission index, so admission and streams hold across gateways. Each run records the gateway executing it; opening and the `chat-recovery` duty interrupt only the runs of gateways that are gone, and a cancellation from another gateway reaches the runner (slices 4 and 5) |
| Knowledge | `knowledge.json`, `retrieval.json`, `proposal-archive/<id>.json` and `cache/semantic.bin` in each notebook's engine directory | `knowledge`, `knowledge-retrieval` and per-proposal `knowledge-proposal` documents keyed by notebook, written only over the version read, and `knowledge_vector_sets` with one `knowledge_vectors` row per vector. An engine writes only changed vector rows and first adopts rows other gateways added, so none embeds or drops another's vectors. Retrieval settings are reread before every search or AI scope. The note map and history catalog stay in each gateway's cache directory and every gateway indexes in the background, but one embeds each notebook's batch at a time and the others adopt its vectors. `admin rollback-semantic-storage` is retired (slice 4) |
| Personal connections | `personal-connections/credentials.enc`, one file sealed under the personal credential key | `personal_credentials` rows, one per account and provider, each sealed under the same key and bound to its account and provider, so a row moved to another account does not open. The key stays a mounted file outside the database. Writes go only over the version read, linking a provider identity takes an advisory lock, and a refresh takes the grant's refresh lock and claims its marker with a conditional write, so one request refreshes it and requests on other gateways wait for its token. A refresh records its gateway, and only one whose gateway is gone becomes reauthorization. Pending OAuth flows are sealed `pending_flows` rows, and a disconnect ends them on every gateway (slices 4 and 5) |
| Managed backups | `managed-backups/bindings.json`; leases in gateway memory | One versioned `backups` document holding bindings and active leases, whose tokens are sealed under the Backups App key. The lease holder expires lapsed leases and revokes a token only after its removal is stored (slices 4 and 5) |
| Leases | — | `leases` rows for duties and gateway presence, `pg.DB.Duty`, `Lease.Fence`, `Exclusive` and `TryExclusive` (slice 5) |
| Pending flows | OIDC, GitHub link, connection tickets, repository and backup pending flows and grants in gateway memory | `pending_flows` rows by kind and digest, bounded per group (slice 5) |
| Request limits | Password, OAuth and OIDC-start windows in gateway memory | `rate_windows` rows per group and key digest, shared by every gateway (slice 5) |
| MCP sessions | Legacy sessions and their subscriptions in gateway memory | `mcp_sessions` rows holding the credential, initialize parameters and subscriptions; any gateway restores a session and serves its stream (slice 5) |
| Revision hints | Each gateway polls a watched notebook's owner every second | Every notebook write announces itself over `notebook_revisions` and every live document change over `notebook_live`; each gateway's read model follows them, with a five-second fallback poll |
| Process locks | Process mutexes for authority handoffs, notebook imports, chat deletion and personal refreshes | Transaction-scoped advisory locks shared by every gateway (slice 5) |
| Exports | Bundle files and a map in gateway memory | `exports` rows with the bundle in blob storage, at most two per instance (slice 5) |
