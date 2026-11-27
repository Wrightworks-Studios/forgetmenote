# MCP action parity

Embedded notebook chat registers the same typed handlers and schemas through an in-process authorized dispatcher. Its private lifecycle routes are explicitly marked `interactive private chat` in the HTTP/MCP parity test; they are not cross-user MCP conversation endpoints. Current identity, notebook grants and action-specific workspace checks still apply on each call. See [chat action controls](chat.md).


Start with `capabilities` and `list_projects`. Tools use trusted authenticated identity for permissions and Git attribution; supplied user IDs in administration are targets, never the acting identity. Each call rechecks account and project access. Capability flags describe authority, not a guarantee that a particular locked or stale edit will succeed.

`browse_folder` provides the browser's folder contents with titles, previews, cover metadata, and a pinned Git revision; it requires Read access. `set_note_properties` accepts optional `metadata.cover` (an existing project-root image path under `assets/`). Omission preserves the cover, and an empty string clears it. Upload images through `upload_attachment`; no external image fetching or new credentials are involved. View preferences remain browser-local.

## Content actions

All mutations below require `notes:write`; they reuse workspace/knowledge methods called by HTTP.

| Tool | Role | Inputs and behavior |
| --- | --- | --- |
| `set_note_lock` | Owner/Admin | `project`, `path`, `locked`, current project `revision`. Inherited locks remain visible through `note_locks`. |
| `set_project_instructions` | Owner/Admin | `project`, `content`, `revision` from `project_instructions`. Writes root `AGENTS.md`; locks apply. |
| `set_semantic_config` | Owner/Admin | `project`, full `config` from `knowledge_status`, optional `rebuild`. Supports pause/resume, endpoint selection, model and splitting; model changes require explicit rebuild even while paused. |
| `set_notebook_wiki` | Owner/Admin | `project`, `openAsWiki`. Chooses whether members open a shared notes notebook in the [wiki layout](wiki.md) by default; `list_projects` reports it. Refused for repository notebooks and the private Inbox. |
| `resolve_comment` | Comment, or Edit for another author's comment | `project`, note `path`, comment `id`, discussion `revision`. Use `review_decision` for findings/proposals. |
| `delete_comment` | Comment for own content; Owner/Admin to moderate | `project`, note `path`, parent comment `id`, optional `replyId`, current discussion `revision`. Removes only the selected content, preserves other replies and Git history. Separate from Resolve; embedded chat requires explicit action approval. |
| `restore_note` | Edit | `project`, historical note `path`, `sourceRevision`, current destination note `revision`. Commits historical bytes as a new version; never rewrites Git history. To restore a version from before a rename, use `read_note` at the old path/revision then `save_note` to the current destination. |
| `upload_attachment` | Edit | `project`, supported `assets/` path, base64 `data`, existing `revision` or empty for a new file. Uses normal save/lock/conflict checks and attribution. |

Read tools require project Read access:

- `list_attachments`: supported media under `assets/`.
- `read_attachment`: `project`, `path`, optional pinned `revision`, `offset`, and `limit`. Returns a base64 chunk, MIME type, full-file checksum, total bytes, and next offset.
- `preview_markdown`: `project` and `content`; returns the same sanitized HTML as the app. It does not execute or save Markdown.
- `export_project_backup`: `project`, optional `offset`, `limit`, and pinned `sha256`. Returns base64 chunks of a complete Git bundle, using the same export as the browser. Read access includes export access.

## Binary transfers

`import_project_bundle` accepts a new notebook `id`, `name`, and base64 bundle `data`. It shares the browser import validator, new-notebook-only publication, ownership, and limits. Requires notebook creation permission plus both `notes:write` and `notes:manage` consent. Bundle bytes must come from an authorized file transfer; the server never accepts an arbitrary filesystem path or remote URL. See [notebook import limits](backups.md#notebook-export-and-import).

Uploads accept at most 32 MiB decoded, matching browser media limits. MCP's request-body allowance accounts for base64 overhead; store limits still apply. Reverse proxies and clients may impose smaller request limits. Uploads do not fetch arbitrary URLs; agents must obtain the bytes through their own authorized tools. Embedding an already-hosted video URL remains ordinary Markdown editing.

Reads default to 64 KiB chunks and allow up to 256 KiB per call. Decode each `data` value separately, append in offset order, and stop at `nextOffset == total`. Verify the assembled SHA-256 before using the output. Attachment reads require the returned revision after the first chunk; backups require the returned checksum. A mismatched backup checksum returns a conflict: discard that partial export and start again at zero.

No durable transfer session or extra storage service is introduced. Backup chunks regenerate a temporary Git bundle and remove it after the call, so large exports are more efficient through the browser's direct download. The checksum prevents mixing different exports. An agent with filesystem tools can assemble an ordinary `.bundle` and restore it with Git. This backs up committed notes, media, comments and history, **not** accounts, permissions, OAuth grants, knowledge configuration, proposals, or unsaved drafts. Use a full-instance backup for those.

## Management consent

Request `notes:manage` explicitly in OAuth authorization. The consent page describes project and access management separately. Defaults remain `notes:read notes:write`; existing and refreshed grants do not acquire management scope automatically. A user reconnects with the additional scope when needed. An existing role is still required:

| Tool | Authority |
| --- | --- |
| `administration` | Admin sees users and all projects; Owners see only their owned projects/members, without a global user directory. |
| `create_project` | Admin only. Creates an empty repository with no implicit member grants. |
| `set_project_member` | Owner of that project or Admin. `user` is a stable internal ID; `role` is read/comment/edit/owner, or empty to remove. |
| `set_user_access` | Admin only. Supply stable `id` and both `admin`/`disabled` flags after reading current state. Uses the existing disable/revocation behavior. |

`notes:manage` does not grant content-write scope or make an Editor an Owner. Disabling a user increments the existing session epoch, invalidating browser and MCP grants; re-enabling the account does not revive those grants. Membership changes take effect on the next tool call without reconnecting. See [authentication.md](authentication.md) for provider-side disablement and session behavior.

Identity-provider provisioning, password entry, GitHub/OIDC authorization and linking remain interactive; no credential arguments are introduced. Creating internal membership/access records does not create an Authelia or other provider account. Appearance preferences remain local to each browser. Server processes, environment variables, remote backups and provider credentials remain operator-managed.

## Verification contract

`TestMCPActionParityInventory` checks the registered MCP inventory and maps every declared HTTP API action to a tool or the explicit interactive password-provisioning exception. A new HTTP API route without a mapping fails this test. Content workflow tests call an actual MCP SDK client against temporary Git repositories; they verify restoration, media bytes, discussion ownership, lock/conflict checks, export checksums and independent Git restoration. Management tests cover role/scope boundaries, owned-project visibility, capability reporting and OAuth management consent through code exchange and refresh.


Verified on 2026-09-08: `go test -race ./...`, `go vet ./...`, all 24 frontend tests, and the Docker smoke suite passed. A separate disposable container was driven through the actual MCP SDK for capabilities, project/member/user administration, restore, lock/unlock, instructions, ordinary comment resolution, media upload/read, preview, export, and full semantic configuration. The management consent page was inspected in a browser. No real user permissions or note contents were changed by these tests. Production-host and ChatGPT phone/client support for requesting `notes:manage` and handling large binary transfers remains unverified; the protocol-level tests do not establish that every client exposes these controls.

## Historical retrieval

`hybrid_search` accepts `query.scope: "history"` with the existing `all`, `text` and `meaning` modes. Omit scope for current notes. Historical matches group distinct passage text and include pinned path/revision/citation, note hash, committed date and occurrence provenance; inspect the response's `history` and `message` for partial coverage. `semantic_search` accepts the same scope. Use `read_note` with the returned historical path and revision to preview or compare committed text without a write.

`get_indexing_passages` accepts `scope: "history"`; return its compatible model vectors through `submit_embeddings` with `submission.scope: "history"` and the supplied status revision/configId. Current and historical text share reusable embeddings. Read-scoped callers can search or request pending passages, while submitting vectors still requires Edit authority and write scope. Membership revocation applies to subsequent historical calls immediately.

See [knowledge.md](knowledge.md#searching-history) for reachable-history semantics, incremental scans, cache bounds and result provenance.

## Archived evidence

Connected repositories have a separate read-only source contract. Use
`search_repository_sources` with explicit `projects`; open returned files/ranges
at their exact source binding and commit. `repository_source_map` takes a single
`project` and finds component paths, entry points and file responsibilities before
reading the original implementation; map entries are navigation hints, not evidence.
Notebook tools continue taking a single
`project`. Source search, citation capture and Owner index settings are documented
in [repository notebooks](repository-notebooks.md). Source-repository content remains read-only.

`source_formats` discovers supported document and spreadsheet formats and whether the server or the connected agent extracts each (`extractedBy`); `archive_source` provides upload/URL capture with an optional destination `folder` (omitted: `sources`, empty string: project root), revision-checked refreshes that retain their current path and title, and an agent's own extraction as `text` with an `extractor` label and the original; `list_sources_awaiting_text` lists sources whose extraction failed or found no text, for an agent to refresh with its text; `verify_source` checks recorded checksums; `read_source_original` returns original document bytes in revision-pinned chunks. Existing note, search, history, and knowledge tools handle the source records and extracted text. See [Archived sources](sources.md) for schemas, permissions, limits, and provenance semantics.

`set_note_container` requires Edit/notes:write and accepts `project`, `path`, `revision`, and `container` (`draft`, `archive`, or empty for Notes). It preserves the path and checks ordinary locks/revisions. `note_collections` and `browse_folder` with `collection=notes|drafts|archives|all` require Read. See [collections](containers.md); an integrated chat classification request remains an explicit control action.


Container choices `archives` / `drafts` are `include`, `lower` or `exclude` on
`search_notes`, or inside `query` for `hybrid_search` and `semantic_search`.
Omitted choices use ordinary notebook defaults (Archives exclude, Drafts include).
An explicit `collection` further narrows the search. `browse_folder` is collection
browsing unless an explicit Archives/Drafts preference requests scoped browsing.
`get_retrieval_settings` is Read; `set_retrieval_settings` is Owner/notes:write
with the complete versioned settings and `expectedId` from the last read. Defaults
never restrict an explicit user choice. History uses each version's classification;
`semantic_search` returns an error for partial historical catalogue coverage, since
its legacy array cannot carry coverage. Use `hybrid_search` to inspect partial results.
