# Organizing Notes

Open **Review → Notebook checks → Organize existing notes**. Choose all eligible Notes,
a folder and its subfolders, or individual Notes. Select the actions to permit:

- Add tags from existing tags or an explicit vocabulary (one tag per line).
- Add related-note links. Existing content and tags stay intact.
- Move notes into selected existing folders, preserving filenames.
- Move verified duplicates to recoverable Trash. This is off by default and
  requires an explicit selection for each manual run or saved automatic policy.

**Preview suggestions** records proposed effects without changing notes.
**Apply eligible changes** makes guarded Git commits for qualifying actions.
Applying requires current Owner or administrator authority; a Commenter or Editor
can preview. Manual runs work with automatic organization off. Starting an
applying run authorizes its selected actions without another per-note approval.

Moves stay within this notebook. Destination choices are the root and directories
containing eligible Notes, including their parents. There are no invented folders,
renames, generated prose, archiving, sharing/lock changes, attachment deletion or
Empty Trash operations. Required backlink edits must also fit the selected scope,
with one exception the system maintains: notes in the Trash. They are never in
the scope, but a move updates their links to the moved note, as a move or rename
you make yourself does ([Trash](trash.md)), so a note restored from the Trash
later still reaches it. This is best effort: a note in the Trash that may not be
changed (a lock rule covers it or the location it was trashed from) or whose links
cannot be updated safely (wiki or HTML links to the note, for example) is left as
it is, and the move goes ahead. The preview lists the notes in the Trash that
refer to the moved note: those whose links will be updated and those that will be
left, with why. The action's activity and receipt list them the same way; a note
left as it was may need its link fixed by hand once restored. Duplicate cleanup
leaves notes in the Trash unchanged, as moving a note to the Trash does.
A selected note may use other eligible Notes as evidence; that does not authorize
editing those other Notes. Occupied destinations are never replaced.

## Automatic organization

Shared permission is controlled by **Administration → AI features → Allow organization, writing and profile automations**. Existing API-configured automatic organization policies remain independent of consistency checks and Quicknotes sorting. The browser no longer offers a per-notebook automatic settings form. See [instance AI controls](ai-features.md) for legacy migration. It starts off after upgrade. Enabling it records a
baseline of current Notes; it does not backfill them. After subsequent committed
changes settle for two minutes, the existing scheduler checks the changed Notes.
The scheduler polls every 30 seconds and advances at most four actions per batch.
Use a manual run to organize the existing collection.

The saved policy records the enabling stable account, its access epochs, personal
consent generation, destinations and policy generation. Only current Owner/Admin
authority can apply. Revocation is checked before inference, publication and the
workspace transaction. Disabling/changing policy cancels pending automatic runs.
Removing personal consent cancels the account's pending organization runs, and a
renewed consent does not revive their old authorization. After access or consent
revocation, save the automatic policy again to authorize future work.
Configuration reload cancels pending organization work. Existing applied actions
remain in activity and can be undone without remote consent.

Cancellation and policy/consent changes coordinate with a bounded transaction:
an action already committing finishes with its receipt; no later action can pass
that boundary. Runs commit each action separately, so cancellation can leave
visible, undoable partial progress. Applying runs cannot overlap in a notebook;
finish or cancel a paused run before starting another. Other notebooks receive
turns between bounded batches.

## Privacy and verification

The operator must configure Jev and the triggering account must allow remote
processing in **Evidence check preferences**. Local-only mode prevents inference.
There is no fallback to another provider. Eligible committed note bodies and
selected destination context are sent to TypeSafe. Opening the panel, listing
activity and refreshing status never run inference.

Drafts, Archives, Trash, protected Quicknotes Inboxes, excluded/protected paths and
locked targets are excluded. Any accepted pending live text blocks execution;
organization never checkpoints it. Browser drafts remain local and recoverable.
Concurrent note changes, newer backlink edits, locks, dirty files, path traversal,
symlinks or changed permission invalidate the affected action.

Candidate retrieval considers up to four related notes for each target. Completion
means that the selected actions and retrieved candidates were examined, not that
every possible duplicate was found. Duplicate cleanup has a separate full-note
judgment; excerpt similarity never authorizes removal. The retained note must
preserve all material facts, dates, uncertainty and provenance. The lexically first
candidate path is retained to avoid cycles that remove an entire duplicate group.

Deterministic checks additionally reject differing frontmatter, source provenance,
unique discussions, unresolved HTML/wiki references and different relative link
resolution. Semantic duplicate bodies containing links require manual review;
fragment-bearing backlinks require identical full bodies. Supported incoming links
are redirected atomically with the move to Trash; pinned historical links remain
pinned. Attachments stay in place. These conservative checks can leave genuine
duplicates as suggestions.

Complete verification is limited to 24 KiB per note and 48 KiB of combined evidence,
and must fit the operator's Jev limits (64 KiB/32 questions by default, with
32 KiB for shared state plus any one question). Inputs are never silently truncated. Larger or uncertain
cases become suggestions. Vocabularies are limited to 100 tags and divided into
bounded independent judgments; there can be up to 32 approved destinations.
Thresholds are explicit in `internal/web/organizer_runner.go`: tags 0.95 with
no-match below 0.05, links 0.95, moves 0.98 and duplicates 0.995. Choice actions also
require confidence at least 0.9 and a margin of 0.5. These are conservative initial
policy values, not calibrated correctness probabilities.

## Activity, Undo and recovery

Recent checks show manual/automatic origin, account, scope, allowed actions,
progress and applied/suggested/skipped/conflicted outcomes. Each action includes
its reason, concrete before/after files, retained/destination links, history and
Trash references. Notes in the Trash a move updates or leaves are named, with
their original locations, in its reason and its result's `trashLinks`, without
their text. Suggestions can be dismissed. **Reconsider suppressed work**
explicitly overrides prior Undo/dismissal for a new manual run.

**Undo action** and **Undo run** create new restoring commits; they never reset
history. A run is undone in reverse order. Current file bytes and intervening
history are checked, including occupied paths and newer edits subsequently
reverted by a user. Undo refuses conflicts and reports which actions remain;
independent actions may still be restored. Notes in the Trash whose links a move
updated are the exception: Undo reverts each one still unchanged and leaves one
that changed since (for example when a later move or rename updated its links),
was emptied or restored, or is now locked, naming it, without refusing; a note a
[purge](purge.md) erased is no longer part of the action at all. Undo run undoes
a later move first, so a note in the Trash two moves of the run updated returns
to what it was before either. Undoing those two moves one at a time, the earlier
first, can leave a stale link: the earlier move's Undo leaves the note, which
the later move changed, and the later move's Undo then returns it to how the
earlier move left it, linking to where that move had put its note although the
note is back where it was. Managed Trash restoration preserves other entries and
refuses an entry already restored or emptied. A later move or rename that
rewrote links in a duplicate's Trash copy, a later move of the same run
included, is newer work too: Undo of that duplicate refuses as a conflict (Undo
run undoes that move first), and the note can still be restored from the Trash.
Undo retries use Git receipts and durable intents. Suppression is stored
outside Git, tied to relevant evidence rather than the repository's changing HEAD;
unrelated commits and restarts do not erase it. Meaningful evidence changes can
permit new work.

Interrupted runs are visible as paused. **Continue check** resumes recorded
answers and reconciles existing commit receipts. If the outcome of a paid request
is unknown, continuing explicitly records a new retry; automatic startup does not
silently repeat it. Organizer commits update only the affected baseline paths,
so unchanged notebooks settle without recurring inference. Completed activity
uses paged Jev result records, not the interactive proposal queue. Existing limits
remain: at most 2,000 retained job records, 4 MiB per sidecar record, a 3 MiB job
planning envelope, and 1 MiB serialized before/after data for an action. Capacity
errors require a smaller scope or operator recovery; unrelated history is not
deleted to make room. No token quotas were added.

## Storage and rollback

Organization extends the existing version-2 Jev job and version-1 settings records
with optional typed organization fields. Old records remain readable and never
enable new automatic behavior. Settings, baselines, suppression, jobs, results and
Undo intents are Jev records in the instance database; content and operation
receipts live in each notebook's ordinary history. The catalog records
per-project access epochs.

A complete stopped-instance backup must preserve the database (catalog, Jev
records, notebook revisions with their discussions and Trash, and live
documents), the blob store and the data directory. A notebook Git bundle preserves content
and commit receipts but cannot reconstruct the activity/policy/suppression state.
See [backup and restore](backups.md). The Docker smoke restores an independent
full-volume copy and then exercises organization activity and Undo.

Older executables using strict record decoding cannot read the new sidecar fields.
Do not downgrade against this data. Keep a compatible image or use forward
recovery; never overwrite newer accepted content with an older snapshot merely to
make rollback work. Operator configuration and TypeSafe secrets remain separate
backup assets and must stay outside notebook repositories.
