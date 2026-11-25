# Storage versions, recovery and rollback

Audience: instance operators. Take a completed, complete stopped-volume backup
and protect the preceding image before this storage release. These changes are
migrations, so a pending EBS snapshot is insufficient. Follow the existing
[production recovery procedure](aws-recovery.md) and
[complete restoration procedure](standalone.md#backup-and-full-restoration).

## Storage v2: notebook content in Postgres

Migrations `0017_notestore.sql` and `0018_notebook_authority.sql` add the
notebook tables ([notebook content](postgres.md#notebook-content)), and
`0019_live_snapshot_storage.sql` stores one of their columns,
`live_docs.snapshot`, out of line without compression (`STORAGE EXTERNAL`): a
live checkpoint of a long note takes about half as long, and a stored
snapshot takes more space, about 0.5 MB more for a 1.2 MB manuscript. It
rewrites no rows; snapshots written before it stay compressed until they are
next rewritten. These migrations only add tables and set how one of their
columns is stored: `forgetmenote migrate` applies them, and until the cutover
finishes a binary of the previous release still serves a database that has
them, ignoring them, as the cutover's refusals need.
Content moves only in the stopped [storage v2 cutover](storage-cutover.md):
`forgetmenote admin storage import` stores each notebook's Git history in the
database and blob store, every commit and tree byte for byte, and verifies it.
The storage v2 release refuses to start while a notebook repository under
`data/projects` has not been imported, or has commits its stored history lacks.
Once the new release serves the imported notebooks,
`forgetmenote migrate -finish-rollout` raises the database's reader version to
2, the version of this release: a binary of the previous release (reader
version 1) then refuses the instance (`database requires reader version 2`)
instead of serving the notebooks from their old repositories, where storage v2
never looks. Restoring the database dump taken before the cutover restores
reader version 1, which the [rollback](storage-cutover.md#rollback) relies on.

After the cutover:

- A notebook's revision and a note's `Version` are the same 40-hex commit SHAs
  they were. Imported history keeps its SHAs, so knowledge references, chat
  citations, memory anchors and GitHub remotes continue unchanged.
- Each notebook's Git export, `data/exports/NOTEBOOK.git`, is the Git form of its
  history: bundles are made from it and backup workers push it.
- The previous release's files beside each notebook are no longer read: the
  working tree and its `.git`, `write-journal.json`, the `collaboration/`
  snapshots and append logs, `captures/` and `authority.json`. One transaction
  per change replaces the journal and the workspace lock; `live_docs` and
  `live_updates`, `operation_receipts` and `notebook_authority` replace the
  others.
- Notebook owner processes, the owner map (`OWNER_MAP_FILE`,
  `notebooks.owners`), `forgetmenote owner`, `admin move-notebook` and
  `admin rollback-live-storage` are gone.

Nothing writes storage v2 back into Git working trees. To return to the previous
release, restore the backups taken at the cutover's freeze and start its image
([rollback](storage-cutover.md#rollback)); revisions saved since then are in the
exports and must be reconciled by hand.

## Catalog version 10

Version 10 admits private project memory notebooks. Each is a protected notebook
`assistant-<account ID>-<16 hexadecimal characters>` owned by one person, with
the assistant notebook's protections: it cannot be shared, transferred or given
to a group, and its ID derives from the account so no one can pre-claim it. The
layout is version 9's; only the version number and these notebooks are new.

The first successful write publishes version 10. A version-9 file is read as
before and retains `catalog.pre-storage.json` if no baseline exists yet. A
version-9 file that already lists project memory notebooks is refused, and
earlier binaries refuse version 10.

On the Postgres branch the catalog is one versioned document in the instance
database (see [Postgres](postgres.md)), always written at version 10, and
`forgetmenote admin rollback-storage` is retired: a file-based release cannot
read Postgres state. The file versions below describe what a stopped instance's
`catalog.json` may hold when its state is brought across.

## Catalog version 9

`catalog.json` remains the authority for accounts, membership, policy, epochs
and notebook routing. Version 9 stores its audit sequence and current logo hash,
with activity in `activity.jsonl` and logo pixels in a content-addressed
`catalog-logo-<sha256>.bin`. Activity and logo values have concrete catalog
ownership and are immutable between transactions; permission clones do not
copy their retained history or pixels. Public readers receive independent copies.
These operational files are outside notebook Git history and are not editable
notebook content.

The application reads legacy versions 0 and 2–8. The first successful write
materializes version 9 and retains `catalog.pre-storage.json` as a migration
baseline; version-0 input also retains `catalog.v1.json`. Versions it does not
understand are refused before recovery files are changed. Inline legacy audit
IDs must be positive, contiguous within the retained window, and end at the
recorded sequence. Corrupt files are refused before migration.

A permission change and its audit receipt use one recoverable transaction:

1. Prepare durable logo bytes, if changed, and a journal binding the previous
   and next catalog hashes and the new activity entries.
2. Append and sync activity before publishing the next catalog.
3. Atomically replace and sync `catalog.json`, which is the commit point.
4. Remove the replayable journal. Retire obsolete owned regular logo blobs.

At startup, a journal matching the committed catalog completes its audit
append. A journal matching the previous catalog discards the prepared append.
An unmatched journal fails closed. Recovery can repair a partially written
final append from the journal; it never invents missing committed audit history.

The public audit window remains 5,000 entries. Its append file compacts before
exceeding 10,000 entries or 64 MiB. An entry must be smaller than 1 MiB. Catalog,
transaction and materialized rollback files are capped at 64 MiB and validated
before publication, so a successful write cannot strand the next reader.
Oversized changes fail without changing live permissions or audit state.

If publication fails and the old catalog cannot be positively established,
authentication, notebook authority, protected Inbox access, external connection
grants and AI processing are fenced until restart/recovery. An uncertain import
retains its restored Git data. Do not delete that data or replace the catalog
with an earlier permission file.

## Live collaboration version 2 (before storage v2)

Releases before storage v2 kept live documents in each notebook's
`collaboration/` directory, described here. Storage v2 keeps them in
`live_docs` and `live_updates` instead, and the cutover reads these files only
to refuse a notebook whose live edits were not checkpointed
([live collaboration](collaboration.md#durability-and-revisions)).

A per-note `<path-hash>.json` snapshot uses a version-2 envelope. The adjoining
`<path-hash>.jsonl` contains accepted CRDT deltas, receipts, content hashes and
version metadata. The first empty log is synced before publishing a version-2
snapshot. Older binaries reject the envelope instead of silently ignoring edits.
Legacy unversioned records remain readable and migrate on their next write.

Every accepted update is appended and synced before acknowledgement. Replay
validates sequence, receipt identity, content hash and CRDT snapshot state.
A partial final line was not acknowledged and remains retryable with the same
client operation ID. Complete corrupt entries fail closed and are retained
for diagnosis. A missing version-2 log also fails closed: treating it as empty
could erase an acknowledged edit from an incomplete backup. Nonempty orphan
logs likewise fail closed; an empty first log interrupted before snapshot
publication is safely retired before reseeding.

Snapshots remain bounded to 64 MiB, with a separate append log of at most 4 MiB.
The log compacts on checkpoint, at 128 entries, or before exceeding 4 MiB.
Compaction publishes the full snapshot first, then resets the log. If interrupted
between those steps, entries already represented by the snapshot are skipped.
The existing 1 MiB Markdown, 8 MiB CRDT state, retained-version and notebook
memory budgets remain authoritative. Checkpoints still require an explicit
save; append storage never creates background Git commits.

A failed live write fences notebook writes until owner restart/recovery.
Preserve browser drafts and retry the original operation after recovery.

## Semantic vector version 2

Before Postgres, each notebook's private `cache/semantic.bin` was one atomic
version-2 file: a magic/version marker, strict bounded configuration/revision/
dimension header, sorted content-hash rows, little-endian float32 values and a
SHA-256 integrity checksum, with legacy `cache/semantic.json` migrating on
engine open. Vectors now live in `knowledge_vector_sets` and
`knowledge_vectors` rows (see [knowledge vector storage](knowledge.md#vector-storage)),
and serving no longer reads either file. `forgetmenote import-files` (slice 7 of
the [Postgres transition](postgres.md)) reads them once with the same validating
readers: count, key shape, dimensions, finite nonzero values, file budget,
framing and checksum are checked before acceptance, and unverified row counts do
not preallocate.

`admin rollback-semantic-storage` is retired, as `rollback-storage` is: a
file-based release cannot read vectors from Postgres. Knowledge configuration,
proposals, decisions and consumer checkpoints moved to Postgres with the
vectors; Git history keeps its owner and format.

## Complete backups and recovery

Back up the database (`pg_dump`), the blob store and the whole data directory
together, from a stopped instance: the database holds notebook revisions, live
documents, receipts, the catalog, knowledge state and vectors; the blob store
holds content blocks and other large files; the data directory holds the
recovery key, the Git exports and their purge flags. See
[backups](backups.md#complete-instance-backup). A Git bundle is a checkpoint
export, not an instance or pending-edit backup. Restore into an independent
directory and database before starting exactly one instance on them.

Restore the database and the blob store from the same point: a file version
whose blocks are missing cannot be read. Do not edit notebook rows or export
repositories by hand. `catalog.pre-storage.json` is historical evidence, not a
current rollback target; restoring it alone would lose later grants,
revocations and audit receipts.

## Returning to an earlier image

There is no materialization for an earlier image. `admin rollback-storage` and
`admin rollback-semantic-storage` are retired, because a file-based release
cannot read Postgres state, and `admin rollback-live-storage` is gone with the
collaboration files it rewrote. Return to a release before storage v2 only by
restoring the backups taken at the [cutover's freeze](storage-cutover.md#rollback);
anything saved after the cutover must be reconciled by hand. Images older than
version-8 notebook authority semantics need their own migration or a matching
complete restore, as described in [collaboration](collaboration.md).

## Measurements and verification

Storage v2 measurements on development hardware, against the Git backend where it
has an equivalent: listing a notebook 1.2 ms (Git 12.5 ms), finding notes 7 to
11 ms (34 to 43 ms), the history of a note with 5,000 revisions 1.4 ms at the
median, and the first live read of a 1.2 MB manuscript about 30 ms. On a
synthetic notebook of 20,000 revisions, 2,000 notes and 200 images, the import
took 1 min 15 s, verification 15.7 s, a full export 54.5 s, repack and fsck
38.7 s, and exporting one new revision 8.2 ms. These are not production
throughput claims.

The measurements below are of the Git backend before storage v2.

On the warm development Apple M4 Pro, a three-iteration 5,000-entry audit benchmark
allocated approximately 0.54 MB and 116 objects per update, versus 13.6 MB and
10,072 objects when materializing the preceding whole-catalog writer. That
counterfactual used the same current owners with the preceding persistence
function. Latency was approximately 20 ms versus 16 ms: the prepared transaction
adds durability work while sharply reducing serialization and allocation.
These are local microbenchmarks, not production throughput claims.

A small edit to a synthetic large Markdown note appended 724 bytes instead of
serializing its 763,646-byte full live record. Its measured 132 ms included Git
validation and the race detector; the byte reduction does not imply an equal
reduction in request latency.

Focused real-Git race tests cover durable updates, restart, independent restore,
conflicts, corrupt and missing logs, bounded compaction, uncertain publication,
legacy fixtures, future-version refusal and current-state rollback. The broad
live suite passed in approximately 141 seconds. Full release verification and
production acceptance remain separate required gates.

For 1,000 synthetic 768-dimensional vectors, three native iterations on the same
Apple M4 Pro measured actual synced JSON publication at about 40.80 ms and
26.96 MB allocated, compared with binary publication at 13.56 ms and 3.23 MB.
Bounded JSON load measured 53.11 ms and 30.30 MB allocated; checksummed binary
load measured 3.38 ms and 6.61 MB. This compares disk codec publication/load,
not complete engine construction, notebook refresh or request latency. The
binary encoder still publishes the complete cache per batch; measured savings
justify that simpler boundary without introducing an append store or database.
