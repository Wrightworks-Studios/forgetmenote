# Headless notebooks, API, and CLI

ForgetMeNote remains the central service for identity, permissions, Markdown and
Git history, repository snapshots, search, maps, and optional AI processing.
The browser, CLI, and MCP access the same content and workspace operations.
`note edit` downloads one note for an editing session and submits its final text;
it does not synchronize a folder or run a client background service.

## Start a headless instance

Use the ordinary image and persistent `/data` volume with `HEADLESS=true`, or run:

```sh
forgetmenote serve --headless --data /srv/forgetmenote --url https://notes.example.com
```

The default, no-command startup still starts the ordinary browser application.
Headless mode serves a small JSON response at `/`, disables notebook application
assets, and keeps health/readiness, API, MCP, configured workers, signed webhooks,
login/consent, and GitHub callbacks. Local, OIDC, and trusted-proxy authentication
remain available. Use HTTPS except for localhost testing.

For a fresh local-auth instance, provision the account while the server is stopped:

```sh
forgetmenote admin bootstrap --data /srv/forgetmenote \
  --username owner --password-stdin < /run/secrets/initial-password
```

The command returns the account ID. Issue an initial credential for that account:

```sh
forgetmenote admin credential --data /srv/forgetmenote --user ACCOUNT_ID \
  --name laptop --projects default --scopes notes:read,notes:write \
  --audiences api --days 30 --output /run/secrets/laptop.token
```

Both commands acquire the existing instance lock. The credential file must be a
new file outside notebook repositories and is created with mode `0600`. Bootstrap
does not enable a password bypass when OIDC or proxy authentication is configured.
For existing notebook creation/management actions, deliberately choose `--all-projects`
and include `notes:manage`; normal clients should use a notebook allowlist.
Credentials act as their existing account and cannot exceed its current roles.

For the new organization administration actions, use a human Admin OAuth session
(`auth login`) or operator recovery authentication. Data credentials cannot issue
invitations or manage groups/service accounts. See [organization administration](instance-administration.md).

## Use the CLI

Build the same executable with `make build`; `make build-clients` produces
macOS/Linux arm64/amd64 executables in `bin/`. Client commands work without Git,
a local notebook checkout, or a writable server data directory.

```sh
forgetmenote auth login --server https://notes.example.com
# Notebook creation and other management commands need management consent:
forgetmenote auth login --server https://notes.example.com --manage
# Or provision a scoped credential without putting the secret in arguments:
forgetmenote auth login --server https://notes.example.com --token-stdin < laptop.token
forgetmenote notebook list
forgetmenote notebook create personal --name Personal --authority forgetmenote
forgetmenote note write personal ideas.md --file ./ideas.md --create
forgetmenote note edit personal ideas.md --editor 'code --wait'
forgetmenote document read personal ideas.md
forgetmenote search 'deployment settings' --notebook personal --notebook source
forgetmenote notebook map source
forgetmenote notebook refresh source
forgetmenote notebook export personal --output personal.bundle
```

Human login uses the existing browser login/consent and PKCE with a loopback
callback. `--no-browser` prints the authorization URL for opening manually.
An ordinary sign-in requests `notes:read notes:write` only. Add `--manage` to
also request `notes:manage` consent for notebook creation, access, credential,
backup, organization and automation-package commands; a management command
refused with 403 suggests signing in again with it. The
API audience is `/api/v1`; existing MCP OAuth grants retain their `/mcp` audience.
`auth logout` revokes the stored grant before removing the local session.

The editor defaults to `$VISUAL`, then `$EDITOR`, then `vi`; graphical editors
need a wait option. It opens a private per-edit file, preserves exact unchanged
content without committing, and submits a changed note once. The save checks the
base revision, notebook authority generation, identity, permissions, locks, and
pending live edits. A change to another note need not cause a conflict.

On failure the command reports the retained draft path and ID. Use:

```sh
forgetmenote edit list
forgetmenote edit resume EDIT_ID
```

Each recovery folder contains `note.md`, `base.md`, and `session.json`; a save
conflict also records `current.md`. Resolve the text yourself, then use
`edit resume EDIT_ID --rebase` to explicitly accept that captured current revision
as the save base. This does not merge text automatically, and a subsequent change
can still conflict. A lost response is read back and reconciled when the same note
identity and exact accepted bytes prove success; otherwise the draft is kept.
GitHub-authoritative documents are read-only. Checkpoint pending collaboration
through `checkpoint_live_note` before a whole-file edit.

`--json` provides structured output. Diagnostics go to stderr; exit codes are
0 success, 2 other API rejection, 3 authentication/permission failure, 4 conflict
or a retained draft, 5 server failure, and 1 other errors. Server sessions and drafts use the OS user configuration
directory under `forgetmenote`; override it with `FORGETMENOTE_CONFIG_DIR` (0700).
Use `--server` to select a saved session, or `FORGETMENOTE_SERVER` together with
`FORGETMENOTE_TOKEN` for a provisioned environment. Secrets never belong in URLs,
Git, or command arguments.

## HTTP API and complete command coverage

All `/api/v1` endpoints require a bearer grant with the API audience. Browser
cookies do not authenticate this API, and browser mutation CSRF checks are unchanged.

| Endpoint | Response |
| --- | --- |
| `GET /api/v1/capabilities` | Identity, authority/generation and allowed notebook capabilities |
| `GET /api/v1/actions` | Accessible typed actions and their input/output schemas |
| `POST /api/v1/actions/NAME` | The action's JSON result |

Actions reuse the MCP schemas/handlers, without chat truncation. Existing
pagination, file/range limits, provider consent, and admission limits still apply.
Errors have the form `{"error":{"code":"conflict","message":"…"}}`.
Other codes include `unauthorized`, `forbidden`, `read_only`, `live_pending`,
`dirty_workspace`, `local_only`, `not_found`, `invalid_request`, `too_large`,
`capacity`, `owner_unavailable`, and `unavailable`. Do not blindly retry mutations.

`forgetmenote action list` exposes discovery. Invoke any supported operation with
`forgetmenote action NAME --file request.json` (or `--file -` for stdin).
The read-only `document_outline` and `find_term_mentions` actions also expose
[Writing tools](writing.md) without the browser or an AI provider.
This covers attachments, membership, instructions, settings, imports, Trash,
history, live checkpoints, connection setup, and credentials alongside the named
daily commands. Creation requires an empty revision; updating requires the expected
revision. CLI `note write` enforces `--create` versus `--expected-revision REV`.
CLI saves also send `requireCheckpoint:true` to `save_note`. This checks pending
collaboration under the notebook's lock before any write/checkpoint. Other
API/MCP clients can select the same constraint; omitted retains existing save
behavior for older clients.

Credential issue/list/revoke actions are `create_api_credential`,
`list_api_credentials`, and `revoke_api_credential`. Issue through a human
management grant, **Your settings → API credentials** in the browser, or the
stopped-instance command. An API credential cannot mint
other credentials. Each has an account, expiry (1–365 days), scopes,
`projects` **or** explicit `allProjects`, and `audiences` (`api`, `mcp`, or both).
Secret hashes and grant metadata live in private authentication state, not notes.
Revocation, account disable, role changes and notebook restrictions apply again
on each request, including discovery, links and cross-notebook retrieval.

## GitHub setup without the notebook UI

Configure the existing Sources/Backups Apps as described in
[repository notebooks](repository-notebooks.md) and [managed backups](managed-backups.md).
Then:

```sh
forgetmenote notebook connect-github OWNER/REPO --id source --name Source
```

The CLI opens a short-lived same-account browser authorization flow, obtains
verified installation choices, then selects the named repository. This explicitly
shares it with notebook members; external AI processing stays disabled.
GitHub access/installation must be granted by the account owner. An installation
ID by itself is not authorization.

For source reconnection or backup setup, use the shared actions:

1. `begin_github_connection`: `project`, `purpose` (`source` or `backup`), and,
   for a new source notebook, `creating:true` plus `name`.
2. Open its `url`, then call `github_connection_status` with `connection:id`.
3. Pass a verified `installation` and `repository` to `finish_github_connection`.
   Sources require `shareWithNotebook:true`; backups require `shareHistory:true`,
   current backup `generation`, and selected `branch`. New private backup creation
   also requires operator `allowCreate` and explicit `createOwner`/`createName`.

Use `backup status`, `backup run|pause|resume|disconnect --generation N` to operate
an existing binding; add `--kind gdrive` or `--kind dropbox` for a drive
destination, with that destination's generation. Only the separate backup worker
pushes Git or uploads snapshots.

## Notebook authority and safe switching

`authority.type` is derived from persisted `kind`: `notes` → `forgetmenote`,
`repository` → `github`. A GitHub notebook can contain code, Markdown, or both.
Its default branch is read-only in ForgetMeNote and changes through verified
upstream refresh. Source disconnection or backup configuration never changes
authority. Personal Inboxes cannot switch.

Switching requires Owner/Admin, a preview, and explicit application, available
in Notebook settings → Authority and through the API/MCP/CLI:

```sh
forgetmenote notebook authority preview personal --to github \
  --repository OWNER/REPO --operator-writers-stopped
forgetmenote notebook authority apply personal --preview PREVIEW_ID
forgetmenote notebook status personal
forgetmenote notebook authority preview personal --to forgetmenote
```

Use `cancel_notebook_authority_change` with `project`/`preview` to stop preparation.
A preview lasts 15 minutes and pins the actor, permissions, authority generation,
local revision, source binding, default branch and upstream tree. Changed facts
require a new preview. Publication is never canceled or rolled back.

**To GitHub:** first use the existing managed backup to seed the selected private
repository and authorize that same repository with the Sources App. The default
branch must contain the exact committed notebook revision/tree. Stop any external
SSH/operator writers and acknowledge that in the preview. Applying fences saves,
disables managed pushes and waits for the backup worker's drain
acknowledgement. Reapply the same preview when ready. Another notebook cannot
use that branch as a managed backup destination during the handoff or afterward.
No force push, merge, or remote overwrite occurs.

**To ForgetMeNote:** refresh is paused; the complete verified tree is staged and
committed as a new revision. Existing notebook history is retained. The source
SHA and binding are recorded and pinned source evidence remains available under
its original authorization. The upstream repository is untouched. Complete upstream
history is not claimed or synthesized from a shallow cache. Configure a new outgoing
backup deliberately; a prior writer is never automatically resumed.

Both directions retain notebook identity, memberships and processing consent.
Old originless note-ID references resolve against retained note history; pinned
source references keep their original bytes. Historical evidence is not included
in the new current corpus. A converted notebook layout understands note IDs,
Drafts/Archives and Trash; policy/discussion files are preserved but excluded from
ordinary source results.

The switching format is Markdown, `assets/` attachments, validated comments,
locks/Trash metadata, and `AGENTS.md`. Preview rejects unsupported files, invalid
or duplicate identities, symlinks, executables, submodules, LFS pointers,
ambiguous paths and configured limits rather than silently omitting content.
Current transfer bounds are 10,000 files, 64 MiB total, and 32 MiB per file
(a lower configured source file limit still applies). Normal read-only code
repositories remain supported regardless of eligibility for a writable handoff.

## Upgrade, recovery, and backups

Stop the instance and take a [complete backup](backups.md#complete-instance-backup)
(database, blob store and data directory) before upgrading. Upgrade managed backup
workers together with the gateway. Mutations require the current authority
generation; a prepared handoff keeps its staged content in the notebook's
authority record in the database, which fences writes through every gateway.
Each commit and each live edit checks that record again in its own transaction,
so a write or live edit checked before a handoff was prepared fails rather than
landing in it. A Quicknote filing still in flight in the notebook refuses a
handoff's preparation, on any gateway and after restarts; let it finish or cancel
it, then apply the preview again. Live edits not yet saved as a version refuse it
too, whichever gateway accepted them; save the note, then preview again.

Issuing API credentials or creating an authority preview activates catalog v3,
preserving `catalog.pre-headless.json`. Older binaries reject that catalog;
never copy a migration snapshot over current accounts, permissions or notebooks
as a downgrade. Restore a complete compatible instance backup instead.

The catalog owns one durable transfer record per notebook. The workspace owner
keeps a small authority receipt and a temporary import stage outside Git.
Sources/backups keep their own paused generations and drain state. Startup
recovers transfers before serving requests: pre-publication preparation aborts
and preserves original content; published transitions complete. Missing original
provider/owner configuration stops recovery and keeps the fence. Restore that
configuration and restart; do not edit receipts, clear a fence manually, or reset
the Git tree. An aborted backup may require an explicit resume.

Allow temporary disk headroom for a complete snapshot and validation repository;
Compose provides a 256 MiB `/tmp` for the 64 MiB transfer limit. Persist private
state and retained evidence along with Git. A project `.bundle` is independently
restorable using `git clone personal.bundle restored`; it contains committed
content/history, not accounts, connection state or uncommitted private drafts.
See [full backup and restore](backups.md).

Verification commands: `make check-fast check-browser`, `make build-clients`,
build the image, then `make smoke smoke-headless SMOKE_IMAGE=YOUR_IMAGE`.
The headless smoke uses synthetic data, the actual executable/editor workflow,
restart, an independent Git restore, and a full-volume restore. Unit/integration
tests cover both authority directions, the database authority record, worker drain, stale
upstream previews and recovery at durable publication boundaries. Live provider
authorization and deployment remain separate rollout checks.
