# Trash

Each notebook has a system-managed `Trash/` directory. Owners and Admins can
move notes there from the note’s Properties modal or the note actions menu,
undo that move, and restore notes.
Trash is read-only; its directory cannot be renamed, moved, locked or used as an
ordinary save destination. Readers and Commenters may inspect Trash within the
notebooks they can already read. Editors retain their ordinary writing rights;
organizing and removing notes requires Owner/Admin, including grouped deletions
and acceptance of deletion proposals.

Trash moves preserve Markdown metadata and discussions. Relative links inside
the moved note are adjusted, and shared attachments remain in place. Incoming
links keep their original destination, so restoring to the original path repairs
them. Moving or renaming a note or folder also rewrites the links of notes in
the Trash, so a note restored later still reaches what it linked to; the move's
result counts them (`trashLinksUpdated`). Such a move stops when a note in the
Trash came from a location that is now locked; unlock that location first. An
[organization](organization.md) move updates them too, as the one change it
makes outside its selected scope, but on a best-effort basis: it leaves a note
it may not change, such as one from a locked location, as it is, names it and
moves anyway, and its Undo reverts the notes it updated unless they changed,
left the Trash or were locked since. A move a chat assistant makes (`move_notes`
in [chat](chat.md)) updates them on the same best-effort basis, as the one
change it makes outside the chat's selection, which never includes the Trash: it
skips a note an ordinary move would stop for and moves anyway. It reports only
how many notes in the Trash it updated and skipped (`trashLinksUpdated`,
`trashLinksSkipped`), never their paths, titles or text. The assistant cannot
otherwise change a note in the Trash except through the Trash actions
themselves. Restore refuses occupied destinations and offers another path.
Locked notes must be unlocked before trashing, restoring or emptying them.
Private browser drafts must be saved or reconciled before the UI trashes their
note; accepted server-side live edits are checkpointed under the notebook lock
before a move. Late live updates cannot recreate a trashed note.

Normal listings and text, semantic and historical searches exclude Trash.
**Include trash** opts in; **Include previous versions** alone does not include
trashed notes. Results are filtered before ranking, snippets, facets and history
provenance. A new note reusing an old path does not inherit that note's historical
Trash status. Emptying removes those notes from search even with Include trash.

**Empty Trash** previews the affected count and requires a confirmation modal.
The server binds confirmation to the complete Trash manifest and notebook Git
revision, rejecting stale confirmations. Cancel and review again if the notebook
changes. Emptying deletes current note/discussion files, not old Git commits or
backups. Shared attachments are retained, avoiding damage to other notes. To
remove content from every version, such as a leaked secret, use a
[purge](purge.md) instead.

MCP exposes `list_trash`, `trash_notes`, `restore_trash`, and `empty_trash`.
Trashing takes 1–50 paths and the current `project_status` revision. Restore takes
an entry ID, current revision and optional destination. Before `empty_trash`,
call `list_trash`, show the count and retention explanation, obtain explicit user
confirmation, then pass that preview's `revision` and `confirmation`. Do not
silently refresh and retry a rejected confirmation. `search_notes` accepts
`includeTrash`; `hybrid_search` and `semantic_search` accept `query.includeTrash`.
`submit_change_group` deletion flags now move notes to Trash atomically with the
rest of the group; they do not bypass recovery.

The versioned `.gitnotes-trash.json` manifest records original locations and
historical occurrence tombstones. It is reserved from ordinary content APIs and
travels with Git bundles and full instance backups. Emptying retains tombstones
so history search cannot resurrect removed results, and restoring retains one so
a note trashed again keeps its earlier history. A [purge](purge.md) gives the
revisions it rewrites new IDs but leaves other notes' tombstones as they were;
they are read through the purge's mapping of old IDs to new ones, so they keep
hiding what they recorded. A manifest or archive that fails its checks is
damage to the notebook's stored data, not a bad request: the notebook does not
open, and the error names the file; restore the notebook from a backup. A
bundle import holding such a file is refused as invalid. The manifest holds at most
10,000 records in 8 MiB. When a change would exceed that, the tombstones of
restored and emptied notes move to numbered archive files
(`.gitnotes-trash-1.json`, `.gitnotes-trash-2.json`, …) in the same format,
which keep serving history search and later Trash moves, so emptying the Trash
keeps working for the life of the notebook. Only notes in the Trash then fill
the manifest: with 10,000 of them, moving another note to Trash is refused
(`Trash index capacity reached; empty the Trash or restore notes first`), and a
single request whose notes would not fit even an empty Trash is refused as
invalid (`move fewer notes at a time`). A note whose history the Trash cannot
record (more than 100,000 revisions or 1,000 names) is refused for good, as HTTP
413, not as a retryable capacity error. Moving notes to Trash,
restoring them or emptying Trash is one revision, committed in one transaction
with the manifest like any other notebook change. Complete instance backups must
still include live documents, which are outside the notebook's history.
Existing user-created `Trash/` content must be relocated before adopting the
reserved directory; the application must not silently adopt or overwrite it.

Trash and restore preserve committed Draft/Archive membership. A restored shared Draft returns to the Drafts view at its original path; an Archive returns to Archives. These virtual collections are [separate from Trash](containers.md).
