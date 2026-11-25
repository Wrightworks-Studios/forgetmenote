# Live collaboration

Chat can deliberately attach accepted shared live content or issue scoped live operations through the same workspace transport and guards. Browser-private draft content is sent only by the explicit attachment action. See [integrated chat](chat.md).


Saved Markdown notes up to the ordinary note size limit (`FMN_LIMIT_FILE_BYTES`,
32 MiB by default) join a live Yjs document when opened, so a book-length
manuscript edits live too: the editor draws only what is on screen, in every
mode. Visual, Source, and Side by side are one CodeMirror editor over the same
plain `Y.Text("markdown")`; front matter remains Markdown.
The editor never diffs the note: each of its edits leaves as the text changes
it made, so every other character keeps its Yjs identity. Each input event is
one Yjs transaction. Remote and agent changes, and this user's undo, reach the
editor as text changes, which it applies directly, so selections map and the
document is never replaced. Undo tracks the local user's Yjs changes. Text
inside an open IME composition stays private until the composition ends; its
changes are then applied, followed by any remote changes that waited for it.
New notes, recovered legacy drafts, and historical versions remain private drafts
until explicitly saved or reconciled. A chosen historical version remains private
even when it equals the last Git checkpoint but differs from accepted live text.

Typing or composition during live-session initialization stays in a private draft
instead of being replaced by a delayed response. Saved browser Yjs state records
the plaintext it represents; a newer private draft (including unfinished composition)
is retained for explicit reconciliation instead of replaying the older state over it.
Older browser recovery records without that plaintext binding also require review.
Each browser tab owns one draft record containing its plaintext, reconciliation
scratch, and recovery choices. Live-replay packets are stored separately.
Draft adoption and resolution publish the complete record in one browser-storage
write; a failed write leaves the previous complete draft recoverable. Older
separate records and session metadata remain readable during recovery. New
recovery records identify legacy source snapshots with compact fingerprints,
so source guards do not duplicate their full plaintext.
A browser lock prevents duplicated tabs from sharing a destructive owner
identity; reload reuses that identity when available. Without browser locks, a new
document gets a new identity and can recover earlier candidates. Clean polling,
Save, and Discard never erase another tab's owned draft. Opening a foreign or
legacy candidate copies it into this tab; differing candidates remain available
through **Other retained drafts**, with a full-text preview before choosing.
Unapplied reconciliation text also distinguishes candidates, even when their
note text is identical; choosing another variant preserves the current scratch.
The preview shows both texts. An
explicit choice can dismiss the reviewed foreign version in this tab without
removing its owner's copy. Switching between candidates archives this tab's
different retained text. These local candidates are not published by recovery.
Large source replay records remain read-only and bound to the chosen candidate
until this tab writes its own state; opening a candidate does not duplicate a
multi-megabyte retry. Retiring a retry updates the same draft record before
optional packet cleanup, so it cannot resume after comparison or deterministic
rejection, including after reload.
Comparing a live draft pauses its
outgoing requests and retires its automatic replay payload. Discard adopts the
latest shared text, including edits already accepted when an acknowledgement was
lost; it does not undo accepted changes. Reconciliation checks that the reviewed
live revision has not changed before applying a choice. The editor maps its
selection through remote changes in every mode.
Comparison uses the latest committed note to check live eligibility:
oversized notes keep revision-based comparison, while eligible notes include
accepted pending text. Apply freezes the combined-text input during validation and adoption;
failed adoption retains the reconciliation scratch text for retry.

Opening another note or folder checks private-draft retention both before loading
and immediately before replacing the displayed editor. Failed browser storage
cancels navigation and keeps the current text and warning visible; switching
notebooks and installing new drafts require the same retention. A pending route
does not detach the displayed live session. Unfinished IME composition must finish
before navigation can replace its editor. Explicit history replacement and
reconciliation remain intentional private choices, not navigation autosaves.

The provider uses same-origin authenticated HTTP. An edit is sent as soon as it has
painted (the next frame) when no update is in flight, and at once when the tab is
hidden, since a hidden tab paints no frames; edits made while one is in flight are
sent together when it is acknowledged, so a tab has at most one update in flight.
A failed send is retried on its own backoff (1, 2, 4… seconds, up to 30 s) rather
than on every keystroke, while the long poll goes on reading; it is retried at once
when the browser comes back online or a poll succeeds after failing. Edits made
while the browser was offline therefore arrive once it is online again; after an
outage of the server alone, which raises no online event, they arrive with the
first poll that succeeds, which is itself retried on its backoff of up to 30 s.
A retry is the same operation,
so an edit the server accepted before its answer was lost lands once. An
update carries only what the server does not hold yet (Yjs encoded against the
state vector the server acknowledged). A gateway that has not yet heard of updates
another gateway stored, which the update builds on, reads the stored document
again; if the server still refuses the update for missing dependencies, the browser
sends its complete state once under a new operation ID.
Each tab holds a long poll (25 s) and starts the next one 50 ms after an answer
(5 s while hidden, 250 ms during an IME composition). While live editing is
connected, the status line follows the session rather than each key: it says the
changes are synced, and that they are being shared once a second passes with
changes unsent and no answer.

The live wire is incremental. An update, a Save and a long poll name the live
revision the tab holds (`since`), and the answer is only the Yjs update after it
with the revision and checkpoint metadata, no Markdown: about a hundred bytes for a
typed word in a 1.2 MB manuscript. The tab's own update is acknowledged with what it
lacks, its own update and whatever others added since, and a Save with only what the
checkpoint changed. The whole Markdown and Yjs
state are sent only when the tab opens the note, when the note starts a new
generation, or when the server no longer keeps the named revision (it keeps the
patch bases of the last fifteen minutes). Requests that name no revision, as the
MCP tools and tabs still running the previous JavaScript after a deploy do, get
the whole note, as before. An unchanged revision returns a small response.
A tab also names itself (`tab`): its long poll is not answered with the tab's own
update, which that update's answer already brought, so a keystroke costs the writer
one request rather than three. A tab is its user's, so another user naming it
changes nothing for it. A long poll that reaches another gateway than the
tab's update is still answered with it.

The gateway keeps each live note's Yjs document loaded between requests, so a
keystroke neither loads the note's whole state nor encodes it again; it is encoded
when a compaction, a checkpoint or a whole answer needs it. Loaded documents count
towards the live memory bound below, as sixteen times their encoded state (a
document of many small scattered edits, as two typists make, holds up to 13 times
it), and are unloaded first when it is reached. A
document another gateway changed, or one a refused update may have touched, is read
again from the database.

The tab's recovery records hold only what the server does not. The draft record is
written whole once, when the note first holds unsaved text, and again when the
page is left, on navigation or after a large journal; while live editing is
connected, typing never writes it. The live record follows every change instead
(once it has painted, and at once when the page is hidden or left): it
names the draft record it continues and journals the text changes made after it,
local and remote alike, with the Yjs update the server has not acknowledged and the
operation in flight. Recovery folds the journal into the draft record, so a crash,
reload, closed tab or offline period loses no edit, acknowledged or not: in the same
generation the unacknowledged update is replayed on the server's newer state, and
after a new generation the journaled text is retained as a private draft for
reconciliation. While an IME composition is open, the live record says its text is
the editor's but not the live document's, so recovery asks to reconcile.
This is polling, not a WebSocket server. Permissions are checked on every request
and notebook locks are checked again when the server accepts the update.
The runtime remains Go only. The pinned `github.com/reearth/ygo/crdt v1.49.5`
implements the Yjs binary protocol; browser `yjs 13.6.32` is bundled locally.
Tests exchange actual JavaScript Yjs updates with Go in both directions.

## Comments and replies

Discussion changes use the existing comment files in each revision and their revision guards.
`POST /api/comments/delete` under `/p/{project}` and MCP `delete_comment` accept
`path`, parent comment `id`, optional `replyId` for one reply, and the current
`revision` from the discussion. MCP also requires `project` and `notes:write`.
The gateway rechecks Comment access on every request; authors may delete their
own content, while project Owners and instance Admins may moderate others’
content. Editor authority to resolve a thread does not grant moderation deletion.
Every gateway runs the same guarded transaction.

Deleting a parent with replies clears its text and finding content and retains
`deleted: true` with the thread’s anchor and replies. The final reply’s deletion
removes the empty placeholder. Deletion does not resolve a thread or change note
text. Stale revisions conflict, a failed commit leaves the original discussion,
and existing history and backups retain deleted content. Browser deletion
asks for confirmation; embedded chat treats deletion as an explicit control action.

## Durability and revisions

Each note's live document is stored in the instance database: a Yjs snapshot and
the updates accepted after it (`live_docs` and `live_updates`;
[notebook content](postgres.md#notebook-content)). An incoming Yjs update is
bounded and restricted to plain Markdown text, applied to a candidate document,
then stored in the database before it is published or acknowledged. Invalid
updates cannot change the accepted document. A gateway announces each accepted
update on `notebook_live`, so peers polling another gateway see it there too,
and any gateway loads the stored document after a restart. A document's epoch
fences updates made for a document that a save, move, import, purge or delete
has since replaced. Yjs client IDs identify CRDT structures, not users:
authenticated actor, source, and OAuth client come from the server and
accompany operation receipts and each stored update.

Browse exclusions hide notes in navigation and search. Authorized direct live
access and recovery of accepted edits remain available across exclusion changes.

`Live changes synced · Save to create a Git version` means accepted text is durable
in the database but is not a revision yet. Save or MCP
`checkpoint_live_note` checkpoints only that note. Editing, idle time, opening or
closing a note, disconnecting, shutdown, and restart do not create revisions.
Pending text recovers on restart and remains live until explicitly checkpointed;
the first write after a restart checkpoints stored pending edits first, as any
write does, even when nobody has opened their notes since.
`Saved to Git` identifies the committed version.

Existing ordinary notebook write transactions (including ordinary saves, uploads,
source imports, moves, duplication, and lock changes) first checkpoint accepted
live edits throughout that same notebook, then run their original revision checks.
This can commit pending notes even if a later revision check rejects the requested
write; it prevents overwriting newer live content. It never flushes another
notebook. Read, status, history, and export do not checkpoint pending edits.

A checkpoint is one revision: ordinary Markdown with structured
`Collaboration-Receipt` trailers, committed in one transaction like any other
write. Its deterministic checkpoint operation finds a checkpoint that committed
before its gateway recorded it in the live document, so a retry or a restart
never commits it twice. Each accepted operation has a receipt, kept with the
pending document and then for 30 days after its checkpoint; identical retries
return the current document without applying the edit again, while a changed
payload using the same actor/client/operation ID conflicts.

Operation IDs are scoped to the authenticated actor/client within the entire
notebook, across both pending notes and stored receipts. Reusing one for a
different note conflicts. Checkpoint identities bind the note and its exact state;
recovered commit markers must match both the request hash and Markdown content.

A failed checkpoint leaves accepted live text durable and visible. Retry Save or
`checkpoint_live_note` after resolving the failure; there is no background
retry. The UI distinguishes unsynced local drafts from durable pending edits.
Search, source provenance, history, and ordinary `read_note` read revisions and
see changes after checkpoint. Backup watchers follow those revisions through each
notebook's Git export.

## AI and MCP edits

Use `read_live_note`, then `patch_live_note`. The live read includes pending text
and a `liveRevision` separate from the Git revision. Tool responses contain readable
Markdown, not binary CRDT state. Patch edits have UTF-16 `start`/`end` offsets,
the exact `old` text, and replacement `text`:

```json
{
  "project": "default",
  "path": "notes/idea.md",
  "base": "<liveRevision from read_live_note>",
  "operationId": "a-stable-unique-operation-id",
  "edits": [{"start": 8, "end": 13, "old": "draft", "text": "idea"}]
}
```

The server retains compact Yjs snapshots for up to fifteen minutes, bounded by
16 MiB per note. Relative CRDT positions rebase independent edits made since that
base. Expected text and overlap checks run before one atomic Yjs transaction.
An expired base or overlapping rewrite returns a conflict; read the current live
note and reconcile, or use the existing review-proposal workflow. Never retry a
rejected rewrite blindly with a new base. An identical uncertain request must
keep its operation ID and complete original payload.

Browser endpoints under `/p/{project}` are `GET /api/live?path=...` (with
`since=liveRevision/version/pending&wait=ms` for a long poll, `incremental=true`
for incremental answers and `tab` naming the browser tab), `POST /api/live`
(base64 Yjs V1 update, generation, operationId and path), `POST /api/live/patch`,
and `POST /api/live/checkpoint`. An update and a checkpoint name the live revision
the tab holds, for an incremental answer, in the `X-ForgetMeNote-Live-Since`
header, and the tab in `X-ForgetMeNote-Live-Tab`; neither is part of the operation.
They are headers so that a gateway still running the previous release, which
decodes bodies strictly and would refuse an unknown field with 400, ignores them
and answers with the whole note, which the browser takes too. An incremental
answer has `incremental: true` and an empty `content` (`"content": ""`).
Each gateway serves at most 128 long polls of a notebook at once (a notebook that
several gateways serve can have more); one more is refused with HTTP 429 and
`code: "capacity"`, which the browser retries. No MCP tool long-polls
(`read_live_note` answers at once), so MCP clients do not meet this limit.
`checkpoint_live_note` is the MCP equivalent of the last endpoint.
The older APIs and MCP tools retain their committed-revision contracts.

## Limits and recovery

Live Markdown is limited to the note size limit, `FMN_LIMIT_FILE_BYTES` of UTF-8
(32 MiB by default); larger notes retain ordinary guarded saves.
The server enforces the limit; the browser does not repeat it. Invalid live
operations return HTTP 422 with `code: "live_operation_rejected"`. This deterministic rejection retires automatic replay
and retains the current displayed text as a private draft for correction or
reconciliation. Recovery also checks an older pending payload independently of a
newer corrected draft. Unknown server errors, outages, and lost acknowledgements
keep their exact original operation ID and payload for idempotent retry.
If an ordinary save outgrows this limit, startup skips its obsolete committed
live document while preserving its retry receipts. Shrinking the note
allows a new live generation; pending edits still require strict reconciliation.
A document's encoded Yjs state, and each update a browser sends, are limited to
twice the note size limit plus 8 MiB. The stored update log compacts into a new
snapshot after 128 updates or 4 MiB, and retained live-document data is bounded
to approximately 128 MiB per notebook in each gateway's memory. Idle committed
documents are evicted and loaded again from durable state, including during
startup when pending records nearly fill the cache. Pending edits are never
evicted. A refused edit's private intent is retained even when its text equals
the last Git checkpoint but differs from accepted live text.
If browser storage fails, keep the tab open and copy the draft before leaving.
Save failures retain the server error alongside that warning; draft-retention
assurances require a successful write of the currently displayed text.
Once a document's
state passes 4 MiB plus twice its text, a completed checkpoint starts it again in
a new generation. Offline clients
from an old generation retain their drafts and must reopen and reconcile.
Operations are never silently dropped to satisfy a capacity limit.

A normal save, import, restore, rename, or link rewrite also changes the live
generation when the file version changes. Old client updates cannot resurrect
an obsolete file. A [purge](purge.md) starts each live document it touches again
in a new epoch and deletes its old updates and snapshots.

Live documents and their pending updates are database rows, so a complete
backup's `pg_dump` holds them with the revisions they are based on; restore the
database, blob store and data directory together
([backups](backups.md#complete-instance-backup)).

A stored live document must replay exactly: its snapshot, then each update, and
the last update must leave the text its writer had (each update carries a CRC-32C
of that text; updates stored before carry its SHA-256). A document that does not
is refused, and so is every write to its notebook until the notebook is restored
from a complete backup: the first write after a gateway starts loads every stored
document with pending edits, to checkpoint them before the write, and a document
that does not replay could hold accepted edits that the write would then
overwrite.

The browser and MCP `.bundle` export is a portable **Git checkpoint export**,
not a complete live-session backup. It refuses export while live updates await
checkpoint. A Git bundle restores Markdown, media, Git history, and committed
audit, but not pending updates, CRDT identities, browser-local drafts, or the
retry index. Use complete instance backups to preserve those server states.
No server backup can include edits that have not yet reached the server.

## Downgrading

Releases before storage v2 cannot read live documents from the database, and
nothing writes them back into their collaboration files. Return to such a
release only by restoring the backups taken at the
[storage v2 cutover's freeze](storage-cutover.md#rollback). Anything saved or
edited live since then is lost unless reconciled by hand from the exports, so
checkpoint live notes first to get their edits into the exports.

During a rolling deploy, a tab running this JavaScript against a gateway still on
the previous release gets whole answers (the gateway ignores the headers that ask
for less) and works as before. The previous JavaScript does not fold a live
record's journal into its draft record: after a rollback, or in an older tab beside
a newer one, a draft this JavaScript left recovers with the text its draft record
was first written with, without the typing journaled since, and asks to be
reconciled; the edits the server acknowledged are in the live note.

Shared Drafts are committed notes classified with `fmn-container: draft`; they are separate from browser-private recovery records and pending live text. Collection navigation uses the same draft-retention guards. Invalid collection frontmatter prevents checkpointing but retains accepted live edits for correction. See [collections](containers.md).
