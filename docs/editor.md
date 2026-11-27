# Using ForgetMeNote

[Writing tools](writing.md) provides prose statistics, local checks, detailed
outlines and tracked terms, including read-only repository Markdown.

## Settings

**Your settings** groups **Appearance**, **Account**, and **AI connections**. Appearance preferences apply to this browser; account changes apply to your account. Shared AI controls live in Administration. Sign-in identifiers and Git authorship are under **Sign-in and authorship details**; optional GitHub linking is under **GitHub attribution**. A connected GitHub account is summarized outside the expanded section.

Your settings, notebook settings, and Administration are pages with a left category
list, a fixed heading, and a breadcrumb back to the notebook. Their `page` and
`category` query parameters support reload, links, and browser Back/Forward while
retaining the note's path, collection, and pinned revision. Opening a page preserves
the current private draft. Escape or the breadcrumb returns to the notebook;
creation, properties, rename, and confirmations remain short modal tasks.
Existing `account=github`, `account=oidc`, and `account=security` return links open
the account category and transition to the canonical page URL.

**Notebook settings** groups **Sharing**, **Search defaults**, **Instructions**, and **History & export**, and names the current notebook at the top. Owners and Admins can manage its members and locks. Readers, Commenters and Editors can open its history and export. Project access does not create a local or identity-provider sign-in account.

**Administration** separates **People** and **Notebooks** and is available to instance Admins for project creation, local account provisioning, user authority and revocation. These category controls do not change permission checks.

## Chat

The bubble at the top right opens and closes the notebook chat panel, restoring the sidebar visibility you had before opening Chat. The conversation selector uses the same chevron as the notebook selector; its adjacent trash action asks for confirmation before deleting a saved conversation. Stop active work first. Unsent drafts remain available in the selector, and notebook edits remain saved. See [integrated notebook chat](chat.md).

## Appearance and editing

Choose **System**, **Dark**, **Light**, or **Parchment** from the theme swatches in Your settings. System is the first-visit default and follows device theme changes; an explicit choice stays selected across those changes and reload. Onest is the default reading and interface font; Source Serif 4 and system sans remain available for notes. Reading size and theme choices are remembered in this browser. Fraunces is reserved for the brand and welcome headings.

Read and Visual editing share the same document measure, heading scale, and paragraph spacing. Tables can use the wider document surface. **Edit** is the primary reading action; **Save** appears while editing. The note header uses the same display title as the sidebar, with the full file path secondary. **Details** groups Properties, Sources, Comments, History, protection, and **Rename file** on phones and desktops. The persistent editor bar combines the Visual / Source / Side by side switch with formatting, undo/redo, and insertion controls in every mode; it scrolls horizontally when space is limited. In Read, selecting a passage reveals **Comment on selection** for users with commenting permission.

Settings headers stay visible while their contents scroll. Project members show a role badge and their stable identifier below the name when a local username is available.

## Install as an app

ForgetMeNote can open in its own window with the instance's name and icon. In
Chrome or Edge on a computer, use the install icon in the address bar or the
install command in the browser menu. In Safari on macOS, choose **File → Add to
Dock**. The title bar follows your theme.

The app is the same site in its own window, not an offline copy: it needs a
connection to the instance, and notes are never stored for offline use, so
signing out leaves no notebook copies behind. Settings, Your account, Connect
your AI and My assistant stay in the app window and link back to the notebook.
Links to other sites open in your usual browser; downloads and exports save as
they do in the browser. Chrome may also open links to the instance from other
pages in the app; the app's settings in Chrome control this.

Chrome and Edge show your identity provider's sign-in page in the app window and
return to the notebook. A Safari app keeps its own sign-in, copied from Safari
when you add it, so sign in before choosing **Add to Dock**; signing out of one
does not sign out the other. Chrome and Edge pick up a new instance name or logo
when they next check the site, and may ask you to confirm it. Safari keeps the
name and icon it was added with; remove the app from the Dock and add it again.

## Folder browsing and covers

Click a folder's name in the sidebar to browse its contents; its chevron expands or collapses the tree. **Notes** browses the project root. **Inbox**, **Notes**, **Drafts**, and **Archives** are collection destinations; Trash is visually separated below the note tree. Breadcrumbs navigate parent folders, and **Overview** opens the folder's `index.md` when present. List and Cards use the same contents and action menus. The view preference is remembered per user and project in this browser. Search opens a roomy main results surface with readable excerpts and secondary paths. The installed editor draft is retained while searching; **Back to notes** returns to it. On phones, **Show results** closes the sidebar and focuses the results.

Cards display committed titles, short text previews, tags/status, and an optional cover. Previews omit front matter and are derived in memory; no separate content database is introduced. Notes without covers use a text card. Opening another folder preserves browser drafts with the same confirmation as opening another note.

In a saved note's **Properties → Cover image**, choose an existing project image, upload an image, or select **No cover**, then save Properties. Uploads use the existing 32 MiB attachment limit and create a Git commit; saving Properties links the image in another commit. Cancelling Properties after uploading leaves the attachment available for reuse. Uploading does not save an unsaved note draft.

Covers are stored as a project-root image path in front matter, for example `cover: assets/covers/landscape.webp`. PNG, JPEG, WebP, GIF, and AVIF are supported; remote URLs and SVG covers are not loaded. Broken images fall back to text cards. A cover also appears as a banner when reading the note. Cover changes follow existing Edit permissions, note locks, optimistic revision checks, and Git authorship. Moving notes preserves their root-relative cover; moving or copying images updates affected cover fields, with locked-note protections. Other front matter and body text are preserved.

MCP clients can use `browse_folder` with `project` and optional `path` (empty for the root) to get direct children, previews, metadata, and a pinned revision. Use `upload_attachment`/`list_attachments` for images, then `set_note_properties` with `metadata.cover` to set the cover. Supply the existing title, kind, status, and tags as usual. Omit `cover` to preserve an existing cover, or send an empty string to clear it. `read_note` and `search_notes` also expose cover metadata. Choosing List or Cards is a browser preference, not a content mutation.

## Note and folder locks

Use **More → Note protection** to lock that note or any parent folder. Owners and Admins can add or remove locks. The note or folder’s **⋯ → Lock / unlock…** menu opens the same protection control directly on that item. Existing lock paths remain available in the protection dropdown for recovery. Folder locks apply to descendants and cannot be overridden by a child; select the locked parent to unlock it. Read and Comment roles remain effective while a note is locked.

Locks are stored in the reserved `.gitnotes-locks.json` file in each project repository and committed like content, so Git bundles and remote backups preserve them. Ordinary browser and MCP content APIs cannot write this policy file. Lock updates require the current revision and a clean workspace; failed commits restore the previous policy. There is no separate server-level read-only setting, and new projects have no implicit locks.

## History and ownership

Each project has one repository. **More → History** offers the current note, its parent folders, and the entire project. Note history can load an old version for inspection or restoration; folder and project scopes show a timeline without rolling back multiple files. Project history is also available in Project settings.

Note saves compare changes to that specific file since the supplied Git revision. Unrelated note, attachment, comment, and lock commits do not invalidate a draft. Concurrent edits to the same file still conflict, including edits subsequently reverted; current locks and permissions are checked for every save. Restoring a version creates a new commit. Drafts retain their original base and record the note version separately for recovery.

Admin has authority over every project and instance users. Owner is a per-project role that can edit unlocked notes, manage project membership (including other Owners), and lock/unlock paths. Owners cannot grant instance Admin rights or manage projects they do not own. Editors cannot change locks or membership. Backup scheduling and credentials belong to the host operator. Read and Comment permissions are unchanged. Admins manage instance users and can grant notebook-creation permission. Authorized creators become Owner of each new private notebook. See [Notebooks and people](workspaces.md).

## Properties, tags, and search

Open **More → Properties** on a saved note to edit its **Display title (optional)**, kind, status, tags, and cover. The display title labels the note in the header, sidebar, and browser tab; changing it does not rename the file. Leave it blank to use the first Markdown heading, then a short plain-text body preview when there is no heading. Empty notes use the filename without `.md` (an empty `index.md` is **Overview**). An explicit display title remains unchanged when the file is renamed. Tags are entered one per line and are stored as a YAML string list in Markdown front matter. Tag duplicates and filters are case-insensitive. Editors can change properties on unlocked notes; Read and Comment users can inspect them.

The properties editor supports flat text values, quoted strings, inline tag lists, and simple block tag lists. Other front matter is preserved verbatim. Complex values in these supported fields must be edited in Markdown; the panel explains this rather than rewriting them. No YAML dependency or metadata database is required.

The sidebar keeps the notebook picker, search, **New note** and **Import source** buttons, collection destinations, and the note tree. The library has the same creation actions. **Quick note** captures directly into your private Inbox and remains available without ordinary notebook access. The protected Inbox is a system destination, excluded from the ordinary notebook picker. Separate **Create notebook** and **Import notebook** buttons appear for authorized users. **Notebook settings** opens notebook settings and sharing. Personal settings and Administration live in the account menu. Menus work with touch and Tab/Enter; Escape dismisses a disclosure and returns focus to its opener.

To create a note, enter only its **Note name**, then select a **Destination folder**. The folder chooser starts in the folder being browsed (or the open note's parent), supports typing to search and Arrow/Enter selection, and offers **Notebook root**. The app adds one `.md` extension. Invalid names, existing notes, and locked destinations report errors inside the dialog. **Start writing** opens the editor with **Unsaved on this device** status; **Save** creates a saved Git version. Choosing **Shared draft** places the note in the shared Drafts collection only after Save. Accepted live changes show **Live changes synced · Save to create a version**; they are shared but are not yet a saved version. Switching between Note and Import source keeps both forms' inputs, including selected files, until the dialog closes. Source preview/import and draft validation disable switching while a request is in progress.

The filter icon beside search opens **Search options**; its accessible label and tooltip identify it.

Search **Options** puts **Folder**, **Tag**, and **Status** first. **Advanced options** contains matching (**Automatic**, **Text**, or **Meaning**), previous versions, Trash, and Archives/Drafts ranking. Automatic combines text and meaning. Meaning requires a configured embedding endpoint and an index; Automatic falls back to text when meaning is unavailable. Search results are ranked and limited to 50 notes. Display excerpts use the existing Markdown parser to remove frontmatter and formatting; this does not edit note content or alter retrieval evidence, hashes, or citation references. Folder includes nested folders, and Clear filters resets all three without clearing the search text. Clicking a tag on a note applies its filter. Filters also work without a search query. Filtered browsing without a query returns up to 100 notes; the unfiltered tree lists all visible notes. Active filters and non-default search choices remain visible as removable chips even with Options closed. Switching notebooks resets these choices.

Search defaults to the latest committed Markdown. Enable **Include previous versions** in **Options → Advanced options** to find text or ideas across committed versions reachable from the notebook's current revision, including merged history. Trashed notes require **Include trash** as well; notes removed through **Empty Trash** stay excluded even when both options are enabled. An empty current search also offers **Search previous versions**. Repeated passages are grouped, and results open a separate read-only preview with copy and compare actions that preserve your current draft. Initial historical indexing may be partial; its progress and limits are shown explicitly. See [historical search](knowledge.md#searching-history) for scope, embedding reuse, and limits. Unsaved drafts, external uncommitted edits, and unrelated branches are excluded.

**Review** is always visible as one sidebar destination, with the count of unresolved findings and proposed change groups when nonzero. Its single dialog has **Findings**, **Proposed changes**, **Notebook checks**, and **Activity** sections. Notebook checks reuses the existing judgments controls and permissions. The count refreshes on notebook load, local changes, review decisions, and returning to the browser tab; a failed check is shown explicitly. Healthy repository status stays out of the sidebar; external changes and connection-check failures retain a visible message.

The note's **More → History** action still lists up to 50 changes. For Editors, choosing a version there loads it as a draft; only saving restores it. Historical search previews never restore content automatically.

## Rename, move, and duplicate

The note header shows the note’s folder and display title as a breadcrumb; the complete filename/path remains in the header for assistive technology. Saved status appears only while editing or when it needs attention. Owners and Admins choose **More → Rename file** on a saved, unlocked note to rename the file independently of its display title. The directory stays fixed, and the app adds one `.md` extension. Press Enter or **Rename** to save; Escape, **Cancel**, or clicking outside cancels without renaming. Escape and Cancel return focus to More. Invalid names, collisions, stale revisions, and unsaved drafts report errors beside the input. This uses the same rename transaction and protections as the sidebar menu. Save a new note before renaming its file.

Owners and Admins use the **⋯** button beside a note or folder to open a compact context menu with **Rename**, **Move**, and **Duplicate**. The menu stays beside the button; Escape or clicking outside dismisses it. Arrow keys select an action, and Enter opens its form. Opening the menu or choosing an action never requires saving drafts. Before a move or rename is submitted, browser drafts in the project must be reconciled because link maintenance may affect other notes. Duplication only checks drafts within the source or destination, plus any unsaved note currently open in the editor. A blocked sidebar action provides an **Open draft** button for each affected note; drafts are never discarded automatically.

There is no drag-and-drop. Rename changes only the name in the current folder; the `.md` extension is handled automatically. Move keeps the name and uses a folder picker with breadcrumbs. Duplicate suggests an unused copy name and allows choosing a destination folder. The dialog previews the resulting location and blocks invalid names, collisions, and recursive destinations. Titles set in Properties remain content metadata; renaming a file does not rewrite its title.

A move or rename is one local Git commit. It updates relative Markdown links, reference definitions, and images, including outbound links in moved notes and links in notes in the Trash (an [organization](organization.md) move leaves those), and migrates discussions and lock paths. Inherited locks follow content moved out of a locked folder. If link maintenance would alter a locked note, the operation stops and names that note; explicitly unlock it before trying again. A note in the Trash counts as locked while the location it was trashed from is locked. Code examples, front matter, raw HTML, and external URLs are not rewritten. The supported cover field is updated when its target moves; other front matter, HTML attributes, and wiki-style `[[links]]` are not managed by these operations.

Duplicate creates a separate copy in one commit. Content and attachments are copied, relative links within the copy are adapted, and effective lock protection is preserved. The source and incoming references remain unchanged. Copies keep their metadata and begin with an empty discussion and new history.

Destination collisions, unsafe paths, stale project revisions, and dirty repositories are rejected. Browser drafts must be saved or reconciled first. Note and folder history follow moves made in ForgetMeNote using the quoted paths in each move's Git commit message. Note history also follows imported Git-detected renames and supplies the original path for opening older versions. Full history remains in the project repository and Git bundle.

MCP clients use `set_note_properties`, `project_status`, `move_notes`, and `duplicate_notes` through the same workspace operations and permissions. `search_notes` accepts optional `tag`, `status`, and `path` filters. `path` is a case-sensitive prefix; use a trailing slash, such as `wiki/`, to include that folder and its descendants without matching `wiki-other/`. `hybrid_search` and `semantic_search` accept the same path prefix.

## Trash and restore

Owners and Admins can use the red **Move to Trash** button in a note's **Properties** modal or the note's **⋯** actions menu. Save or reconcile private note drafts and save pending Properties changes first. The system-managed **Trash** view offers restore, and the move notification offers Undo. **Empty Trash** requires a confirmation modal and retains old Git history and backups. Normal search excludes trashed notes; enable **Include trash** in Search options to include them. See [Trash and restore](trash.md) for locks, retained attachments, historical search, and MCP confirmation.

## Visual Markdown editing

Edit opens the note in one editor with three modes: **Visual**, **Source**, and **Side by side**. Every mode edits the note's Markdown text itself. The only changes are what you type, paste, cut, or drop and the commands you choose; nothing else in the note is rewritten, normalized, or renumbered. Opening a note, moving the caret, scrolling, selecting, and switching modes change no text. The editor draws the caret itself, as a bar in the text colour, so it shows in every mode and theme and in every browser, Safari included. It holds still while you type or move it and blinks once you pause, about once a second, and holds still if your system is set to reduce motion; a selection shows in the app's selection colour. The browser's autocorrect, auto-capitalization, and writing suggestions are off in every mode, so a word is never replaced or capitalized behind your back, and the double-space period of phone and Mac keyboards stays the two spaces you typed. Spellcheck stays on: it only underlines words, and a word changes only when you choose a suggestion. The same holds wherever else you write text that a note or a comment keeps: comments and replies, quick notes, a note's name and its **Properties** (title, kind, status and tags), the terms of a writing glossary, the title of an archived source, the notebook's agent instructions, your assistant's memory files and an employee's responsibilities, and the combined version when you reconcile a draft. The browser remembers the mode you last chose, for every note.

- **Visual** is a live preview. Headings, emphasis, links, lists, quotes, code, tables, images, and horizontal rules show formatted, and their Markdown marks appear only on the element the caret or selection touches; block marks such as `#`, `>`, and list bullets appear on the caret's line. A table shows rendered until you click into it, and then you edit its pipe Markdown; a table wider than the screen scrolls sideways in its own box while the text around it wraps at the screen. Screen readers hear headings as headings, with their level, as in Read. Front matter is a one-line **Properties** summary that opens as editable YAML when you click it or move the caret into it; **Properties** remains the structured editor. HTML, including named anchors, comments, and line-break tags, shows as dim literal text: it is never rendered, run, or loaded. Reference links and footnotes stay in Visual as styled Markdown. External images are not loaded. A video link shows its player below it; in a private draft, a YouTube or Vimeo player loads only when you choose **Load video** ([video](media.md)).
- **Source** shows the complete Markdown with highlighting, front matter included.
- **Side by side** shows Source next to the rendered note. The rendered side is read-only, scrolls together with the Markdown, and opens links as Read does. It is offered on screens at least 900 px wide.

A note of any size opens whole as one scrolling note. The editor draws only the lines near the screen, so book-length manuscripts stay responsive; move around them with the Writing tools outline and Ctrl/Cmd+F, which searches the whole note.

In Visual, a click on a link puts the caret in it to edit it; Ctrl/Cmd+click follows it, and on a touch screen an **Open** button appears while the caret is in a link. Page links such as `[[Page]]` show their title, and a link to a missing page is marked.

The toolbar works in every mode: Undo and Redo, **Text style** (paragraph or heading), Bold (Ctrl/Cmd+B), Italic (Ctrl/Cmd+I), Strikethrough, Inline code, Link (Ctrl/Cmd+K), bulleted, numbered, and task lists, **Insert** (image or file, video, table, quote, code block, divider), and **Comment**. Each command is one undo step and changes only the lines it touches. Enter continues a list, task, or quote on the caret's line, and Backspace removes that markup; other list items are never renumbered. A task's checkbox changes exactly its one character; in Visual, Ctrl/Cmd+Enter toggles the task on the caret's line, and Tab leaves the editor without stopping at the boxes. Paste inserts plain text exactly as copied, never formatting, and pastes the text even when the clipboard also holds an image, as copies of spreadsheet cells do. Use **Insert → Image or file…** to upload files; pasted or dropped files are refused, and dragging never moves text. Reading mode continues to use the server renderer.

Undo and Redo, in the toolbar, from the keyboard, and from the browser's Edit or context menu, undo your own edits: the live session's per-user undo while the note is live, otherwise the editor's own. Undo never removes other people's or agents' changes, and does nothing while the note is read-only or while an input method is still composing a word, as phone keyboards and Japanese, Chinese, or Korean input do: finish the word first. Undo of an edit out of sight scrolls to it, with the caret after the change. The same note locks, draft recovery, conflict checks, and Git saves apply in every mode.

## Passage comments

Select text in reading mode and choose **Comment on selection**, or select text in any editing mode and choose **Comment** in the toolbar. Save an edited draft first. Highlights open threads in the right-hand Comments panel, where users can reply or resolve discussions. The Comments button also supports whole-note discussions. Comment permission permits annotation of locked notes without editing them; Read permission only permits viewing. Authors can resolve their own threads; Editors, Owners, and Admins can resolve any thread they can access.

Authors with current Comment access can use **Delete comment** or **Delete reply** for their own content. Owners and Admins can delete others’ content; Editors cannot. Deletion requires confirmation and is separate from Resolve. Deleting a parent keeps a “Comment deleted” placeholder and its replies; deleting the last reply removes that placeholder. A changed discussion must be refreshed before retrying. Deleted content remains in Git history and existing backups.

Annotations live in the existing per-note `comments/*.json` files, alongside replies, and are committed separately from Markdown. Anchors retain the selected visible text, nearby context, and original Git revision. Matching text is highlighted after unrelated edits; a changed, removed, or ambiguous passage keeps its thread with a “Passage changed or removed” label. Moving a note carries its discussion; duplicating a note starts a fresh discussion. Existing whole-note comments remain supported. Highlights show in Read and in every editing mode (in Side by side, on the Markdown side); Read uses the browser CSS Highlight API, and the side panel remains available in browsers without it.

MCP `add_comment` accepts an optional `anchor` (`quote`, `prefix`, `suffix`) for a new passage thread, or `replyTo` for a reply. The server validates the passage against the saved note and records its revision. `list_comments` includes anchors, replies, and a `deleted` flag on retained parent placeholders; `resolve_comment` resolves the entire thread. `delete_comment` accepts the parent `id`, optional `replyId`, and current discussion `revision`, with the same authorship and moderation checks as the browser.

## Archived sources

Use **Add source** to preview and archive PDF, HTML, TXT, DOCX, EPUB, XLSX, CSV, or TSV files, or capture a public source URL. Choose **Save in** to place the source note in an existing folder or the project root. Expand **Archived source** on the imported note to inspect provenance, download the original, or copy a page/heading citation pinned to that Git version. [Source documentation](sources.md) covers refreshes, extraction limits, and search.

## Live editing

Existing editable notes join live Yjs collaboration in every editing mode.
Edits sync automatically to durable live state; changes by others arrive as
changes, so the editor never reloads the note or loses its selection. The status reads **Live changes
synced · Save to create a Git version** until Save commits that note, then
**Saved to Git**. Idle time, closing the note, and restarting the service do not
create versions. Existing ordinary notebook write transactions may checkpoint
pending notes in the same notebook before their revision checks. New notes and
recovered or historical private drafts still require an explicit save. See
[live collaboration](collaboration.md) for concurrent AI editing and recovery.

Saved notes can belong to virtual Drafts or Archives without changing their path. See [collections](containers.md) for reversible Editor actions and private-draft handling.

See [notebook language](notebook-language.md) for product terms. Note context is
available from the reader header and phone action bar. Its counts come from
loaded comments and references, without a second AI request. While you edit,
its Contents lists the headings of the text being edited and puts the caret on
the one you choose, and **Show passage** selects the passage in the editor;
typing never sends the note to the server to render the hidden reading view,
which is rendered once when you return to Read. Search notebook
scopes use labeled checkboxes; Markdown previews strip metadata and table syntax,
while repository code preserves its operators. The exact source text, revision,
hash, and citation coordinates remain available for verification. Wide tables
scroll inside the reading measure, and formatting groups wrap to keep Insert
reachable. Disabled personal assistants explain who can enable them and hide
opt-in controls while keeping existing notebook memory available.
