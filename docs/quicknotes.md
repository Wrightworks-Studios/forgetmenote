# Inbox and Quick notes

Quicknotes first commits the original Markdown to the signed-in user's private
Inbox and acknowledges that save. Sorting happens afterward. Browser drafts must
remain recoverable until the save acknowledgement arrives. No Jev call is part
of saving, and a provider failure cannot discard the capture.

**Inbox** is a permanent system destination beside Notes, Drafts, and Archives. It opens a main-area list of captures, filing actions, Undo, and Preferences; Trash is separate. The labelled **Create → Quick note** action and Inbox's **Quick note** button open a small private capture dialog. **Create → Note** starts in the current notebook, and **Create → Import source** opens source import. Capture no longer has a separate floating button. The Inbox route survives reload and browser history. It is available even before the first capture and without ordinary notebook grants; listing an empty Inbox does not provision a repository.

Each Inbox uses the existing catalog/Git notebook storage, named `Quicknotes Inbox`, with an ID
`inbox-<stable-account-id>`. The catalog's `protectedInbox` property binds it to
that account and its sole Owner membership. Provisioning this notebook is the
explicitly approved exception for accounts without ordinary notebook creation
rights; it does not grant those rights. Owners and Admins cannot share or transfer
its catalog membership. Protected Inboxes are excluded from the ordinary notebook selector; opening a capture still uses the ordinary authorized note editor and saved history. The list only requests the signed-in account’s captures, including for Admins. Protection remains after filing every note because its
Git history still contains private originals. Existing instance Admin read/write
authority remains unchanged, but it does not extend to model processing: every
Jev feature (explicit reviews, organization, citation checks, reranking, and
automatic checks, whether enabling them or running a retained setting),
notebook chat in the Inbox, and chat corpora that select it are refused for
everyone except the Inbox's own account, which also needs its personal cloud
consent. Ordinary notebook creation, provisioning and bundle import refuse IDs
beginning with `inbox-` or `assistant-`, so no one can pre-claim another
person's Inbox.

Manual mode does no inference. Suggest is the default mode, but sending capture
text to TypeSafe still requires personal cloud consent and permitted operator
configuration. A suggestion requires explicit acceptance. Automatic additionally
requires individual approval of each exact notebook/folder destination, including
its current sharing status. A destination is shared when anyone besides you can
open it, as a direct member or through a group, or when any group has access at
all, since later group members gain it. Instance Admin authority, including the
bootstrap identity's membership of notebooks it created, is not sharing.
Approval of a private destination does
not silently approve it after it becomes shared, including through a group grant. Local-only configuration prevents inference.

The judgment chooses among server-enumerated, existing destinations and Leave in
Inbox. It never chooses a filename, creates a folder, changes roles, tags a note,
or rewrites its content. Only Owner destinations are currently offered, matching
existing move and Undo authority. Protected Inboxes are not filing destinations,
and neither is another employee's assistant notebook, even for an Admin.
The saved note receives an opaque filename; its displayed title comes from the
original Markdown. The header, sidebar, search, and folder cards share display-title precedence: explicit metadata title, first Markdown heading, a short readable body preview, then filename for empty notes. Opening an opaque capture filename keeps its recognizable title. Drafts, Archives, Trash, pending live edits and stale revisions
cannot become automatic filing evidence. Capture previews are readable text derived with the existing Markdown parser; original Markdown bytes remain intact. Recent capture previews, suggestions,
filing actions and Undo recheck current account/notebook access. Stored suggestions
are displayed only while their exact destination folder, notebook name and sharing
status still match the current permitted destination inventory. Revoked access,
removed folders or changed destination metadata hide the suggestion and show the
capture in Inbox. Each recent-captures request shares one lazy inventory lookup;
listing neither reclassifies notes nor changes their saved content or history.

Automatic filing currently requires selected probability at least 0.95,
probability margin at least 0.5 and Choice confidence at least 0.9. These are
conservative application policy, not guarantees of model correctness. A narrow
synthetic live evaluation of the actual request builder on Jev 1.13.0 produced
correct routing/review outcomes for eight cases, including one empty inventory
handled in code. Seven requests used 4,153 input and 334 output tokens and took
0.14–0.62 seconds each. Three positive cases selected the expected destination;
only one met automatic-filing thresholds. Four ambiguous, unrelated, missing-
destination or mixed-topic cases stayed in Inbox. This small sample supports
keeping the conservative thresholds; it does not establish general accuracy or
broad automatic-filing coverage. The private release evidence records exact
source binding and synthetic cases.

## Storage and recovery

`internal/catalog` owns provisioning and privacy. `internal/quicknotes` owns small
private, versioned records in the instance's Postgres database, one `captures` row
per capture and one `capture_settings` row per account: settings, capture
references, source revisions/hashes, classification operation identities and
transfer checkpoints. Every gateway serves them; a change written over a stale
read conflicts rather than overwriting another gateway's change. Note plaintext stays in the
notebook's history. A stable capture operation ID rejects changed payloads and
replays the original successful commit after a lost response. Creation recovery
can recover the receipt directly from the notebook's history without a gateway plaintext copy.

One service-owned worker wakes after capture/configuration changes and on a
30-second reconciliation timer. Each batch attempts at most eight captures and
resumes after the last attempted account/capture key, wrapping across accounts.
Failed recovery attempts retain their operation identity and back off from
30 seconds to five minutes; the retry timing survives restart. A submission with
no committed receipt remains marked as awaiting its original client draft. It is
not displayed as a saved capture, and periodic receipt checks still find a late
commit. Explicit client retries bypass backoff without changing the
original payload binding. A successful save or recovery clears the retry state.
The worker respects current consent and permissions and uses the shared Jev
service and usage ledger.
Classification intent is persisted before the paid call. An interrupted paid
classification is not silently retried with a fresh identity. An explicit Sort
action can record a fresh bounded attempt after failure or interruption. Model failures or
uncertainty leave the note in Inbox. Settings or source changes invalidate prior
judgments. Runtime byte, concurrency, record and elapsed-time safeguards remain;
there are no Quicknotes token quotas. Capture mutation locks use a fixed array
of stripes, so rejected or nonexistent capture IDs cannot grow retained lock
storage. Capture IDs are validated before building their lock keys.

The source notebook checkpoints accepted live edits before comparing the exact
note version and content hash under its existing mutex. An idempotent durable
reservation then prevents saves, live updates, comments, moves, Trash operations,
backlink additions and covering lock changes from changing that source while
filing is unresolved. Unrelated notes remain editable. Reservations are the
source notebook's operation receipts in the instance database, so every gateway
sees them and a restart keeps them; a finished one is kept for 30 days.

A destination preflight checks an existing safe folder before
source reservation. The destination repeats its checks and commits unchanged
bytes with an attributed operation receipt. Only then does the source notebook
complete removal in one transaction of its own. Source history remains in the
notebook's history. Unknown responses are resolved using the same operation identity. Before
releasing a source on failure, the destination durably fences that receive so a
late network request cannot publish a canceled transfer. A completed destination
receipt wins over cancellation and recovery finishes the source transaction.

Undo performs the inverse guarded transfer and requires the original filed
revision, current Owner access on both notebooks, and an unoccupied Inbox path.
Newer committed or accepted live edits conflict; neither side is overwritten.
Undo cannot erase a shared notebook's Git history, backups or earlier reads.

Completing Undo, including recovery of an interrupted Undo, records a routing
hold bound to the exact restored Markdown content hash. Background sorting and
automatic filing leave those bytes in Inbox across repeated scans, restarts,
settings changes and unrelated Git commits. An explicit Sort or File action can
supersede the hold after the normal permission and revision checks. Changing the
capture's actual Markdown bytes, including frontmatter, makes it new routing
work under the current mode and consent; a bookkeeping commit with unchanged
bytes does not. A successful new routing or filing attempt clears the old hold.

The current transfer is deliberately limited to single captured Markdown notes
whose bytes can move safely. Notebook-relative links/media, source packages,
embedded HTML, discussions or existing incoming links require manual resolution
and remain in Inbox. No dependency folder or silent link rewrite is generated.
Captures may contain up to 1 MiB of UTF-8 Markdown. Sorting handles at most 8 KiB
of capture text and 64 complete destination choices, also subject to the shared
request-byte limit; larger inputs remain saved for manual filing. Inventories
are not silently truncated to force a choice.

## Backup, upgrade and rollback

A complete stopped-instance backup includes the database, which holds the
catalog, Quicknotes records, capture reservations and receipts, notebook
revisions, live documents and the shared Jev ledger, together with the blob store
and the data directory. Restore them together. The test suite
restores a separate synthetic copy after destination commit and before source
completion, then recovers and performs Undo without duplicating content.

Pending transfers retain reservations until they can be reconciled; do not
delete their receipts to unblock writes.

**An old application image does not enforce `protectedInbox`.** Merely draining
pending transfers is not sufficient for rollback, because completed Inboxes and
retained history would become shareable. The release helper must refuse automatic
rollback to an older image whenever any catalog project has a nonempty
`protectedInbox` field. Keep the stopped data and use a compatible forward fix or
an explicitly reviewed migration. Do not remove the protection marker to make an
old image start.

Focused tests use synthetic notebooks and TLS TypeSafe servers. They cover role isolation, idempotency, live and
committed revision conflicts, failed commits, source reservations, cancellation
fencing, newer-target Undo refusal, revoked previews, cloud consent/local-only,
save-before-sort behavior and complete independent recovery. Provider mocks test
protocol/application behavior; the separate small live evaluation measures only
its labeled synthetic routing examples.
