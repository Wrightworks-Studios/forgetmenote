# Notes, Drafts and Archives

Drafts and Archives are saved notebook collections. Their sidebar destinations
show the notes and folders in that collection; Notes shows ordinary notes.
The sidebar footer has one row ordered Drafts, Archives, Trash. Clicking the
selected Drafts or Archives button returns to Notes; choosing the other switches
collections. Opening Trash returns to Notes behind its existing modal, so closing
Trash leaves Notes selected. These transitions retain private drafts and honor
cancelled navigation.
Folders with no notes in a selected collection disappear from that view without
being deleted. Breadcrumbs, links and browser back/forward preserve the collection.
A note keeps its original path, links, discussion, attachments and Git history.

Editors can use Properties or the note actions menu to **Move to Drafts**,
**Move to Archives**, or **Return to Notes**. A folder's actions menu offers
the same choices for every note under it, after confirming how many notes will
move. These actions create an ordinary
Git commit and check the note revision, permissions and locks. Archives remain
editable. A concurrent save conflicts; the app never refreshes the revision and
silently retries. Save or reconcile an affected private draft before changing its
collection. Accepted live edits still use the existing checkpoint/conflict rules.

Membership is portable Markdown frontmatter: `fmn-container: draft` or
`fmn-container: archive`. Use the unquoted, top-level `fmn-container` key exactly
once; quoted or escaped key spellings are reported as invalid for repair.
No field means Notes. Ordinary property saves preserve
this field, and an Editor may change it by editing and saving Markdown. The
saved badge is separate from pending Save/live synchronization. Incoming committed
membership updates the saved badge, sidebar and counts even when newer typing
remains pending; metadata refresh never replaces that typing or its recovery copy. Malformed,
duplicate or unsupported values show an error and remain readable for repair.
Invalid imported classification appears in Notes for correction; it does not
silently become valid ordinary classification. New writes reject malformed
reserved fields, including unsupported continuations separated by blank/comment
lines and declarations beyond the 64 KiB frontmatter parsing limit. A rejected
live checkpoint retains accepted pending text.
Unrelated malformed Markdown remains editable.

Existing folders named Drafts or Archives and free-form properties such as
`status: draft` do not assign membership. Capturing an external source is also
independent of the Archives collection. Recapturing a classified source preserves
its collection. Nothing automatically moves or classifies existing notes.

A **shared Draft** is intentionally saved notebook content with ordinary member
permissions. Choose Drafts when creating a note, then use **Save to Drafts** to
share it in that collection. Before Save, the text is a private draft on this
device. Its reading preview is rendered in the browser; it is not sent to the
server preview endpoint. Raw HTML stays inert and remote media does not load
automatically in this private preview. Large drafts (over 1 MiB of UTF-8 text),
previews whose estimated expanded text, attributes and element overhead exceed
4 MiB, or a browser unable to load the local parser show plain text instead. Opening
a collection never publishes or enumerates private draft text.
Browser recovery drafts, accepted shared live text awaiting checkpoint, and
committed shared Drafts remain distinct. The existing explicit private chat
attachment is another intentional way to send a snapshot of private text.

Trash retains its separate Owner/Admin authority and read-only behavior. A
trashed and restored note keeps its original Draft/Archive classification. Old
pinned citations continue opening their exact committed bytes under ordinary
Read permission. Historical collection links retain their revision through
back/forward navigation and reload. Classification alone is neither a confidentiality boundary nor
a promise about embedding providers; notebook access and embedding settings
still apply.

Browser APIs: `POST /p/{project}/api/container` accepts `path`, `revision` and
`container` (`draft`, `archive`, or empty for Notes). `POST /p/{project}/api/folder-container`
accepts `folder`, `container` and the `revision` the caller viewed; it moves every
note under the folder in grouped commits of up to 50 notes, and conflicts if any of
them changed since that revision. Locks and live-edit checks still apply. `GET /api/notes` and
`GET /api/browse` accept `collection=notes|drafts|archives|all` for collection
listing; omitted collection retains their complete ordinary browse contract.
`GET /api/collections` returns authorized counts, current revision and owner
visibility. These project routes use the existing authentication and Edit/Read
checks. MCP exposes `set_note_container`, `note_collections`, and the
`browse_folder` collection argument through the same workspace methods.

Membership is kept in each note's committed front matter, in the notebook's
history; browser and MCP operations use the same workspace methods on every
gateway. An unavailable database returns unavailable rather than cached counts. Git
bundles preserve membership and historical versions. Complete backups must also
retain gateway settings/chat and every owner's pending collaboration state.

Search options independently select **Include**, **Lower priority**, or
**Exclude** for Archives and Drafts. Ordinary notebook defaults leave Archives
out and include Drafts; Trash is left out unless **Include trash** is chosen. Lower priority is a separate ranking tier:
all eligible included results come first, before exact-title ranking, semantic
ranking, BM25 candidate limits and evidence packing. Excluded or invalidly
classified content is omitted before these candidate limits and filter facets.
Saved browser choices are personal to this account/notebook on this device;
choosing **Default** follows the notebook setting. Owners/Admins save shared
starting choices under Notebook settings → Search defaults using conflict checks.
These are defaults, so an explicit search choice can override them.

Browsing Notes hides classified content, but an ordinary query from Notes is
global and honors those search choices. A query from Drafts or Archives has a
visible removable Collection chip. Remove it to search globally; clearing the
query returns to the selected browse destination. Changing a search preference
never moves notes or modifies private browser drafts.

Previous versions remain an explicit search option. Each committed version uses
its own `fmn-container` classification: promoting a Draft does not retroactively
include its earlier draft text, and archiving a current note does not retroactively
exclude an older ordinary version. Identical passage hashes never transfer one
version's classification to another. Eligible history occurrences and facets are
filtered before their bounds; included occurrences precede lower-priority ones.
Old derived history projections are rebuilt and metadata is reparsed from their
validated committed content, rather than treating an absent cached field as Notes.

Historical processing retains its bounded catalogue (64 MiB, 10000 versions and
100000 occurrence records). A scan/candidate limit is explicitly partial, including
when no eligible match is found. No matches in scanned versions does not establish
absence from the notebook. Hybrid search and passage retrieval return coverage;
the legacy semantic-only array returns an error when historical catalogue coverage
is partial, directing callers to `hybrid_search`. Search exclusions do not change
embedding configuration, provider processing, or content-addressed vector identity.

Gateway settings are each notebook's `knowledge-retrieval` document in Postgres,
beside its `knowledge` state, including for remote-owned notebooks, so every
gateway applies the same settings. Each search or AI scope rereads them first,
and a failed read refuses the scope. A missing document supplies defaults without
writing a migration. Malformed/unknown settings fail affected retrieval closed
and need repair; the gateway still starts with `EMBEDDING_URL` configured, and
healthy notebooks, ordinary Read and collection browsing remain available. The
bad document is preserved, and retrieval retries after it is repaired. Unrelated
invalid knowledge configuration retains its existing startup error. The version 1 document contains `version`,
`search` and `ai`; each policy has `version: 1`, `archives` and `drafts`, using
`include`, `lower` or `exclude`. AI defaults are both `exclude`. Chat gear offers independent Archives, Drafts
and Include previous versions selections. Each admitted run saves its complete
selection; Continue and action confirmations retain that selection even after
notebook defaults change. A new turn can deliberately select another corpus.
`GET /p/{project}/api/knowledge/retrieval-settings` returns `{settings,id}` to Readers;
`POST` accepts `{settings,expectedId}` from Owners/Admins and returns the new state.
The ID hashes the canonical settings; stale saves return 409. MCP provides
`get_retrieval_settings` and `set_retrieval_settings` through the same handlers.

Literal, semantic and hybrid query arguments accept `archives`, `drafts` and an
optional `collection`. The gateway resolves omissions to complete versioned policies.
Typed `SnapshotScoped`, `ListScoped`, `FindScoped` and `BrowseScoped` owner methods
require that complete policy and return its identity together with revision and
actual owner visibility. Missing/unsupported scope support never falls back to
unfiltered owner methods. Scoped snapshots scan at most 256 MiB / 100000 Markdown
blobs, filter before their 64 MiB / 10000 eligible-note return bound, and report
unavailable coverage when a bound is exceeded. Visibility is revalidated even for
warm derived caches. Ordinary browse RPCs remain separate from search preferences.

Integrated chat applies the saved selection to initial evidence, search tools,
direct reads, source/backlink expansion, retained answers and full reviews.
Changing classification or Owner guidance revalidates evidence before another
model request. Original transcripts and completed operation receipts remain
private history; refreshing context never repeats a completed write. Unknown or
missing saved scope cannot authorize Continue or approval: start a fresh turn
with an explicit supported selection.

Explicit committed, live and private attachments authorize only the exact accepted
snapshot for their originating run. They do not authorize other revisions,
related files, discussion or mutations, and are not automatically reused by a
later run. Browser citation reads still use ordinary Read permission. AI writes
cannot promote an excluded target or change `fmn-container` through generic note
edits; the explicit classification control requires an eligible current target.
Groups, proposals, moves and shared assets retain complete target/source checks.
Comments and live writes recheck classification inside the notebook's write.

A full review covers eligible current committed notes, including Lower priority
notes. It excludes Trash and root `AGENTS.md`. Historical comparisons are bounded;
completion never claims that every historical version was examined. Root
`AGENTS.md` is separate Owner-managed guidance even when imported frontmatter has
Archive, Draft or malformed container metadata. It cannot widen model authority.
Empty Trash retains its exact manifest, single-use user confirmation; the model
receives only an aggregate control acknowledgement, never the Trash preview.
