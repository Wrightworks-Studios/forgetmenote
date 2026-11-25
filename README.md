<div align="center">

<img src="internal/branding/assets/flower.svg" alt="ForgetMeNote" width="112" height="112">

# ForgetMeNote

**Self-hosted notebooks for people and AI.**

Write in Markdown, keep every version in Git, edit together in real time, and let your AI tools
read and write the same notes under the same permissions as your team.

[![Check](https://github.com/Wrightworks-Studios/forgetmenote/actions/workflows/check.yml/badge.svg?branch=next)](https://github.com/Wrightworks-Studios/forgetmenote/actions/workflows/check.yml)
[![License: FSL-1.1-ALv2](https://img.shields.io/badge/license-FSL--1.1--ALv2-3299d6)](LICENSE.md)
[![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![MCP server](https://img.shields.io/badge/MCP-server-3299d6)](docs/ai.md)

[Quick start](#quick-start) ·
[Documentation](docs/README.md) ·
[Connect an AI client](docs/ai.md) ·
[Deploy](docs/deployment.md) ·
[Contributing](CONTRIBUTING.md)

</div>

<p align="center">
  <img src="docs/images/editor.png" alt="A note open in the ForgetMeNote editor, with the Visual, Source and Side by side modes and the formatting toolbar" width="900">
</p>

## Why ForgetMeNote

Most note apps make you choose: a polished editor that owns your data, or plain files with no
collaboration, permissions or history. AI assistants add a third problem: they need access to your
knowledge, but usually get either too little or far too much. ForgetMeNote is built around one idea:
**everything is a notebook**, and a notebook is a set of Markdown notes with its own members, roles
and Git history, shared by the people and the AI agents that work in it.

- **Your notes stay yours.** Notes are plain Markdown, attachments are ordinary files, and every
  notebook exports as a standard Git repository you can clone, diff and keep without ForgetMeNote.
- **Every save is a version.** Saves are Git commits. Browse history, compare, and restore any
  version as a new commit. Conflicting edits are detected instead of silently overwritten, and
  owners can purge sensitive content from every version when they need to.
- **AI with the same rules as people.** A built-in [MCP server](docs/ai.md) with OAuth lets Claude,
  ChatGPT, Codex, Cursor and other MCP clients search, read, comment on and edit notes. Every call checks the
  user's current notebook role, the scopes they consented to, and the note's revision, so an agent
  can't see more than its user or overwrite newer work.
- **Real-time collaboration.** People and agents co-edit the same note live, with private drafts
  and device-local recovery for work that hasn't been saved yet.
- **Permissions that mean something.** Read, Comment, Edit and Owner roles per notebook, a private
  Inbox for each person, and no public links. The browser, HTTP API, CLI and MCP share one
  authorization path.
- **Simple to run, ready to grow.** One Go binary and Postgres, with no Node process at runtime.
  Start with Docker Compose on a single machine, and add gateway processes behind a load balancer
  when you need them.

## Features

**Writing and reading**

- A Markdown editor with Visual (live preview), Source and Side by side modes, built on CodeMirror 6,
  that handles book-length notes up to 32 MiB.
- Tables, task lists, code blocks, images, video embeds and attachments up to 32 MiB, stored as
  ordinary Git blobs with no Git LFS.
- Note properties (title, status, tags), folders, cover images, and Notes, Drafts and Archives.
- A wiki reading mode with `[[page links]]`, plus Light, Dark and Parchment themes.

**Collaboration**

- Live co-editing for people and agents on one shared document per note.
- Discussions and passage comments with replies and resolution, kept in the notebook's history.
- Note and folder locks, a recoverable Trash, and a Review queue for proposed changes.
- Task queues that can be assigned to one person, several people, or a group.

**Search and knowledge**

- Full-text search over current and committed history, with optional semantic search through an
  embedding service you configure.
- Archived sources: PDFs with local OCR, office documents and saved email become searchable Markdown
  with pinned citations back to the original file.
- Repository-connected notebooks: read-only snapshots of a Git repository with code search and exact
  line citations, alongside your own notes.

**AI**

- An MCP server at `/mcp` with OAuth (authorization code with PKCE, dynamic client registration,
  rotating refresh tokens) and an optional plugin for Codex and Claude Code.
- Integrated notebook chat that cites the notes it used, with any provider you configure: OpenAI
  Responses, Anthropic Messages, Chat Completions-compatible APIs, or a local Ollama model.
- A private Inbox for quick capture, with optional AI sorting into notebooks.
- Instance-wide AI policy: operators choose which providers, models and processing are allowed, and
  credentials never enter notebook content, prompts or logs.

**Administration**

- Local accounts, OIDC single sign-on, or a trusted authenticating proxy.
- Groups, invitations and automation accounts, with roles granted per notebook.
- A CLI and HTTP API for headless use, plus scheduled automations.
- A Git export of every notebook, complete instance backup and restore, and an external backup
  worker that pushes each export to a Git remote you choose.

## Quick start

You need Docker with Compose.

```sh
git clone https://github.com/Wrightworks-Studios/forgetmenote.git
cd forgetmenote
cp .env.example .env
mkdir -p data
# Set PUID and PGID in .env to the owner of ./data (see `id -u` and `id -g`).
docker compose up -d --build
docker compose exec forgetmenote forgetmenote token
```

Open **http://localhost:8080**, choose **Recovery administrator**, sign in with the key the last
command printed, and create your administrator account. Keep the recovery key somewhere safe for
emergencies.

To run ForgetMeNote on a server with HTTPS, durable storage and upgrades, follow the
[deployment guide](docs/deployment.md). Local accounts work out of the box; see
[authentication](docs/authentication.md) for OIDC or a trusted proxy.

### Connect an AI client

Add `https://YOUR_HOST/mcp` as a remote MCP server in any client that supports OAuth. The client
registers itself, you sign in to ForgetMeNote and approve the scopes, and the client can then work in
the notebooks your account can access. See [Connect an AI client](docs/ai.md) for Claude, ChatGPT,
Codex, Claude Code and Cursor.

## Roles

| Role | Read and export | Comment | Edit unlocked notes | Manage members and locks | Manage all notebooks and users |
| --- | :---: | :---: | :---: | :---: | :---: |
| Read | ✓ | | | | |
| Comment | ✓ | ✓ | | | |
| Edit | ✓ | ✓ | ✓ | | |
| Owner | ✓ | ✓ | ✓ | Own notebooks | |
| Admin | ✓ | ✓ | ✓ | All notebooks | ✓ |

New users see no shared notebooks until an Owner or Admin grants access. Removing a grant or disabling
a user takes effect on their next browser, API or MCP request. See
[notebooks and people](docs/workspaces.md).

## How it works

```text
 Browser · CLI · HTTP API · MCP clients
                  │
     ForgetMeNote gateways (one Go binary)
     identity · roles · revision checks
                  │
   Postgres ─────── blob store ─────── Git exports
   revisions,       large files        one bare repository
   live edits,                         per notebook, pushed
   accounts                            by the backup worker
```

Each notebook is a revision log in Postgres that uses Git's object model: every revision is an exact
Git commit and revision IDs are commit SHAs. Larger files live in a blob store, and ForgetMeNote
writes a bare Git repository for each notebook that reproduces its history byte for byte. Every
change to a notebook is one database transaction, so several gateways can serve one instance
safely. Search indexes are derived from committed content and can always be rebuilt. See
[architecture](docs/architecture.md) for the details.

The backend uses the Go standard library, the official MCP Go SDK, Goldmark and Bluemonday. The
interface is vanilla JavaScript and CSS with CodeMirror 6 and Yjs, bundled by esbuild; Node is used
only at build time.

## Documentation

| For | Start with |
| --- | --- |
| Writers | [Using ForgetMeNote](docs/editor.md) · [Live collaboration](docs/collaboration.md) · [Wiki mode](docs/wiki.md) · [Task queues](docs/tasks.md) |
| AI users | [Connect an AI client](docs/ai.md) · [Notebook chat](docs/chat.md) · [Knowledge workflows](docs/knowledge.md) · [Archived sources](docs/sources.md) |
| Operators | [Deployment](docs/deployment.md) · [Configuration](docs/instance-configuration.md) · [Authentication](docs/authentication.md) · [Backups](docs/backups.md) |
| Developers | [Contributing](CONTRIBUTING.md) · [Architecture](docs/architecture.md) · [Headless use, API and CLI](docs/headless.md) |

The [documentation index](docs/README.md) lists every guide.

## Project status

ForgetMeNote is under active development and runs in production for small teams. It has not had an
independent security audit. Test each release against your own identity provider, AI providers and
recovery plan before you rely on it, and see [SECURITY.md](SECURITY.md) for supported deployments and
how to report a vulnerability.

## Contributing

Bug reports, ideas and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) takes you from a
fresh clone to a running instance with sample notes and an open pull request.

## License

Copyright 2026 Wrightworks Studios LLC. ForgetMeNote is licensed under the
[Functional Source License, Version 1.1, ALv2 Future License](LICENSE.md). You may use, modify and
self-host it for any purpose except offering a competing commercial product or service. Each release
also becomes available under the Apache License 2.0 two years after it is published.
