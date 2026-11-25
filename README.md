# ForgetMeNote

A self-hosted notebook service for people and AI. Editable notebooks keep Markdown, attachments and their Git commit history in the instance's Postgres database and blob store, and each is exported as a standard Git repository; connected repository notebooks expose verified upstream snapshots read-only. The browser, API, CLI and MCP use the same notebook permissions and revision checks.

One Go binary serves an instance, and several gateway processes of it can share one database. Optional Chat, retrieval, imports, data operations and automations run under instance policy and explicit notebook scope. Credentials and execution receipts stay private and outside notebook content. Backups run through an external worker.

**Under active development.** The repository includes release verification and production recovery tooling. Local tests use disposable data and synthetic providers; they do not establish a security audit or certify a particular live identity provider, AI service or deployment. Every release still needs its own verification and recovery assets.

## Run

Requires Docker with Compose and Git to obtain the application source. From this repository:

```sh
cp .env.example .env
mkdir -p data
# Set PUID and PGID in .env to the owner of ./data; use id -u and id -g.
docker compose up -d --build
docker compose exec forgetmenote forgetmenote token
```

Open **http://localhost:8080**, sign in under **Recovery administrator** using that key, and complete **Create your administrator account**. This creates your personal local account with Admin authority and signs you in immediately. Keep the recovery key for emergencies. Setup is available only before any user record exists; existing installations keep their accounts and permissions.

For a server deployment, follow the short [deployment guide](docs/deployment.md). It covers durable storage, HTTPS, startup, and upgrades. Authentication is a separate choice: local accounts work by default; [OIDC or trusted-proxy sign-in](docs/authentication.md) disables local password login. The [Authelia test/operator preset](docs/standalone.md) stays optional.

Use **Your settings** for appearance, your account, and AI connections; **Notebook settings** for members, locks, history, and export; **Administration** for instance users and notebook-creation grants. Authorized creators use **Create notebook** beside the notebook picker. Newly created local or OIDC users receive no shared-notebook access until an Admin or notebook Owner grants it. Each user can capture notes in a protected private Inbox without an ordinary notebook-creation grant. This does not create identity-provider accounts.

| Role | Read/export | Comment | Edit unlocked notes | Manage notebook members and locks | Manage all notebooks and instance users |
|---|---|---|---|---|---|
| Read | Yes | | | | |
| Comment | Yes | Yes | | | |
| Edit | Yes | Yes | Yes | | |
| Owner | Yes | Yes | Yes | Own notebooks | |
| Admin | Yes | Yes | Yes | All notebooks | Yes |


Users without a grant see no other users’ notebook content. Protected personal Inboxes cannot be shared through membership settings. Admin is global; there is no public sharing link. Disabling a user or removing a grant affects subsequent browser and MCP requests. An already downloaded export cannot be recalled. Trusted-proxy identities use exact usernames; OIDC uses issuer and subject. Instance-managed groups grant notebook roles and explicit data operations.

## Included

- Independent notebooks with a notebook picker and browser-based permission management.
- Markdown editing, safe rendered previews, search, relative links, Light, Dark, and Parchment themes, bundled Onest and Source Serif 4 reading fonts, custom CSS, and device-local draft recovery.
- Folder browsing with List/Cards views, note previews, and optional Git-backed cover images.
- Optional display titles independent of filenames; Owners/Admins can click a saved, unlocked note's filename in the header to rename it without changing its folder. The UI handles `.md` automatically.
- Owner/Admin organization controls for rename, move, and duplicate, with link maintenance, note/folder locks, and draft protection.
- Recoverable note deletion from **Properties → Move to Trash** or the note actions menu; restore, Undo, and confirmed **Empty Trash**. **Include trash** opts into trashed notes in current and historical search.
- Live co-editing in Visual (an inline live preview), Source, and Side by side, plus concurrent MCP edits. Accepted live edits persist on the server; Save or an agent checkpoint creates a Git version.
- Book-length notes: live editing up to the note size limit (32 MiB by default) in one CodeMirror editor that opens and edits the whole note in every mode.
- Committed-history search, archived sources with pinned citations, and review proposals; optional semantic search through an external embedding service.
- Read-only connected repository snapshots, code search across selected notebooks, exact source citations, and optional source-aware chat.
- Explicit saves recorded as Git commits; optimistic conflict detection, history browsing, and restore as a new commit. Owners and Admins can [purge](docs/purge.md) sensitive content from every version.
- Note-level discussions with author attribution, resolution and permission-checked comment/reply deletion, stored in each project's Git history.
- Attachments up to 32 MiB per file, kept as ordinary Git blobs in the notebook's history. No Git LFS.
- A Git export of each notebook and downloadable project bundles. Optional [managed private GitHub backups](docs/managed-backups.md) and existing [operator backups](docs/backups.md) push the exports; saved content remains local when backup services are unavailable.
- Complete instance backup and restoration instructions.
- Shared permission enforcement for the web editor and MCP; OAuth authorization code + PKCE, refresh rotation, and owner consent.
- Non-root Docker runtime and embedded assets, with no Node process at runtime. Notebook content, the instance catalog (users, password hashes, roles, identity links, policy) and sign-in state live in Postgres, beside the app in the compose presets; see [Postgres](docs/postgres.md).

## Storage and ownership

Each notebook is a revision log in Postgres with Git's object model: every
revision is an exact Git commit, revision IDs are commit SHAs, and file contents
of 8 KiB or more are content blocks in the blob store. Live edits are Yjs
documents in the same database. ForgetMeNote writes a bare Git repository for
each notebook that reproduces its history byte for byte, and the external backup
worker pushes it. See [architecture](docs/architecture.md#notebook-storage).

```text
postgres/                     # the instance database: notebook revisions, live edits, catalog, sign-in state
data/
  admin.token                 # recovery administrator key; outside notebook content
  proxy.secret                # optional trusted-proxy secret
  custom.css                  # optional instance-wide appearance overrides
  blocks/                     # notebook content blocks (the default blob store is the data directory)
  exports/
    default.git/              # Git export of notebook "default", branch main:
                              #   notes/example.md, assets/image.webp,
                              #   comments/<note-path-hash>.json, Trash/, .gitnotes-trash.json
  projects/default/           # derived search and history caches for notebook "default"
```

The export has the layout of a notebook repository: notes, `assets/`, discussion
files under `comments/`, the reserved `Trash/` directory and its
`.gitnotes-trash.json` manifest, and `.gitnotes-locks.json`. Git records content,
not access-control changes. Back up the database, the blob store and the data
directory together from a stopped instance to preserve the instance, including
pending live edits and operation receipts. A project bundle contains only that
project's committed files and history; checkpoint pending live edits before
exporting it. See [backup and restore](docs/backups.md).

Every notebook change is one database transaction that locks the notebook's head
row, so each notebook has one writer at a time and several gateways can serve
one instance ([several gateways](docs/deployment.md#several-gateways)). Instances
upgrading from Git repositories under `data/projects/NOTEBOOK/workspace` move
them once with the [storage v2 cutover](docs/storage-cutover.md).

## AI clients

See [MCP and plugin setup](docs/ai.md). The endpoint is `/mcp`; content tools require explicit notebook scope. The included plugin packages connection configuration and a skill; the server also works without the plugin.

General AI uses configured hosted or local providers; no general model or inference server is bundled. Local PDF processing uses Poppler and Tesseract. Audio transcription uses configured hosted processing. Use operator-configured connections for integrated Chat, or a connected AI client through MCP. ChatGPT client availability, account entitlements, mobile support, and live OAuth interoperability must be verified with your account; they have not been tested here.

## Development

To build from source, run a local instance and open a pull request, follow [CONTRIBUTING.md](CONTRIBUTING.md).

Go 1.27.4, Git, Node 24, and Python 3.12. Go dependencies are pinned in `go.mod`/`go.sum`; browser dependencies are locked in `package-lock.json`. Operator and Python test dependencies are pinned in `deploy/requirements.txt`; font conversion has separate build-only requirements. On Linux, install `openssl` and `inotify-tools` for the operator and backup-watcher tests; Docker with Compose is required for smoke checks.

```sh
python3 -m venv .venv
. .venv/bin/activate
make setup-deps setup-browser
make check-fast check-browser

docker build -t forgetmenote:poc .
make smoke-full SMOKE_IMAGE=forgetmenote:poc
```

Read [AGENTS.md](AGENTS.md) and [SKILL.md](SKILL.md). Changes follow test-first development. Storage tests use disposable Postgres clusters, and real Git repositories where Git is the boundary; HTTP and MCP tests exercise authorization and conflict behavior. The Docker smoke test uses disposable synthetic data and independently restores an exported bundle.

The backend uses the Go standard library for HTTP, templates, files, and cryptography; the official MCP Go SDK for protocol handling; Goldmark for Markdown; and Bluemonday for HTML sanitization. The application UI uses vanilla JavaScript and CSS. CodeMirror 6 with its Lezer Markdown parser supplies the note editor in every mode (the Visual live preview, Source, and Side by side, with a persistent ForgetMeNote toolbar) and the read-only source viewer; micromark through `mdast-util-from-markdown` 2.0.3, with the GFM table, strikethrough, autolink and task-list extensions, renders the side-by-side pane and private drafts in the browser; Yjs 13.6.32 carries live editing; esbuild 0.28.2 bundles the browser assets. Node is used only during development/builds; the production container runs the Go application with Git, Python for approved automation packages and the external backup utility, Poppler/Tesseract for local OCR, FFmpeg and the iWork converter. Docker builds the frontend automatically. After `make setup-deps`, `make frontend` builds the embedded assets without reinstalling dependencies. `make check-fast` runs format/vet, frontend, fixture and Python checks plus affected Go package race tests; it is PR feedback and does not replace the full release gate. `make check-browser` runs the full disposable browser suite in Chromium, with the editor's caret cases also in WebKit; `make smoke-full` runs eight synthetic Docker boundaries: core, headless, resources, chat, OIDC, required-TOTP, source/OCR and decision providers. `make check` remains an alias for `check-fast`, and `make smoke` runs the basic Docker smoke. Set `PYTHON` to an existing virtual environment executable when needed.

## Current boundaries

No word-processor-style track changes, notebook deletion, folder-level roles, outbound invitation email, full audit UI, Git LFS, or automated wiki migration. Administration supports groups and invitations without sending email. Proposed changes can be reviewed before acceptance through the existing Review workflow. Comments attach to a note and follow moves made through ForgetMeNote. Large notebooks and repositories need measurement before adoption. Search derives indexes from committed notebook Markdown or the connected repository snapshot; notebook revisions remain authoritative, and coverage/size bounds are explicit. See [architecture](docs/architecture.md) for ownership and [repository search](docs/repository-notebooks.md#search-source-and-notes) for source-index limits.

`BROWSE_EXCLUDE_PREFIXES` optionally hides comma-separated path prefixes from navigation and search, including MCP search. It preserves direct reads, history, and backups; it is not an access-control rule. It applies across the instance.

Private drafts and edits that have not synced remain in the browser, scoped by user, project, and tab. Accepted live edits are durable on the server but remain separate from the notebook's history until checkpointed. Full server backups include that live state; they cannot include browser-only drafts. Source code is in this repository; runtime notes live in the separate Docker data directory. Do not commit secrets or personal notes to the application source repository.

## Feature guides

Start with the [documentation index](docs/README.md) for user, operator, developer and historical guides.

- [Headless operation, CLI, HTTP API, and notebook authority](docs/headless.md)
- [Repository-connected notebooks, exact code citations, and notebook links](docs/repository-notebooks.md)
- [Jev evidence ranking, citation checks and notebook reviews](docs/jev.md)
- [Private Quicknotes Inbox and optional sorting](docs/quicknotes.md)

- [Editing, display titles, filename renaming, history, locks, and passage comments](docs/editor.md)
- [Trash, restore, search visibility, and confirmed emptying](docs/trash.md)
- [Live collaboration and draft recovery](docs/collaboration.md)
- [Authentication, account linking, and session revocation](docs/authentication.md)
- [Organization administration, groups, invitations and automation accounts](docs/instance-administration.md)
- [MCP and optional plugin setup](docs/ai.md)
- [Optional GitHub authorship attribution](docs/github-attribution.md)
- [Images, attachments, and video embeds](docs/media.md)
- [External backups and full restoration](docs/backups.md)
- [Storage v2 cutover from Git repositories](docs/storage-cutover.md)
- [Upgrading an existing Gitnotes installation](docs/rebranding.md)

See [architecture and code ownership](docs/architecture.md) for the backend boundaries, derived search indexes, browser modules, and compatibility archive.

## Optional knowledge workflows

[Sources, review proposals, semantic search and external-agent MCP workflows](docs/knowledge.md) extend ordinary Markdown notes. Embeddings are optional; notebook history remains the source of truth. Current retrieval behavior is documented in that guide.

Archive documents, presentations, spreadsheets, saved email, images, and short audio/video recordings with original files, provenance, searchable Markdown, and pinned citations. Docker includes local English OCR with Poppler and Tesseract; a self-hosted build can leave it out with the other [optional integrations](docs/integrations.md). Transcription and image descriptions use configured hosted processing with consent and execution budgets. Size memory for your measured document workload and concurrency; the former local-model estimates do not apply. See [Archived sources](docs/sources.md).

See [production hardening and the release gate](docs/production-readiness.md) for recovery, token rotation, resource limits, and browser checks.

Notebook ownership and account controls: [Notebooks and people](docs/workspaces.md).

## Collaboration

Live browser co-editing and targeted concurrent MCP edits share one Yjs document
per Markdown note. Durable updates are acknowledged immediately; Save or an agent
checkpoint creates Git versions (existing ordinary notebook writes also checkpoint
pending notes in that notebook). Idle time and restarts do not commit.
Stale overlapping AI rewrites return conflicts. Every editing mode edits the
Markdown text directly. See [live collaboration](docs/collaboration.md) for protocol,
limits, and complete-backup semantics.

See [Personal employee assistants](docs/personal-assistant.md) for private assistant notebooks, delegated Google/Slack connections, Jev triage, and Luna drafts.

## License

Copyright 2026 Wrightworks Studios LLC. ForgetMeNote is licensed under the
[Functional Source License, Version 1.1, ALv2 Future License](LICENSE.md). You may
use, modify and self-host it for any purpose except offering a competing commercial
product or service. Each release also becomes available under the Apache License 2.0
two years after it is published.
