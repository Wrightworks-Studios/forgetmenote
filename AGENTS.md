# ForgetMeNote engineering rules

Build a small, dependable, multi-user notebook service. Before implementation, read [SKILL.md](SKILL.md) and the [engineering requirements](docs/engineering-rules.md); every requirement there remains mandatory and this file is the short entry point. Read [verification](docs/verification.md) before verifying or releasing a committed candidate, and [architecture](docs/architecture.md) before changing storage, authority, authentication or ownership boundaries.

Notebooks organize user and AI content under shared identity, permissions, authority and revision checks. Notebook revisions (Markdown, attachments and their Git commit history, kept in Postgres and the blob store with Git's object model) are authoritative; the Git export, search and maps are derived. Credentials, instance policy, private processing state and execution receipts retain their concrete operational owners.

- Every notebook change is one notestore transaction that locks the notebook's head row and checks the expected head or note versions, so each notebook has one writer at a time and several gateways of one Go binary can serve an instance ([architecture](docs/architecture.md#notebook-storage)). Instance control-plane state belongs in Postgres too ([transition](docs/postgres.md)). Use standard library HTTP/files/templates, `pgx`, the Git executable and embedded browser assets; no Node server at runtime.
- Share workspace methods and authorization across browser, API, CLI and MCP. Check credentials, notebook roles, credential scope, current authority and adopted instance AI policy once per invocation and at each persisted disclosure; a bounded monitor rechecks in-flight work, and cached auth state is at most 250 ms stale. Trusted proxy identity requires an explicitly trusted peer and the configured secret.
- Preserve authority handoff recovery, generation fencing, backup-worker drain, historical references and private Inbox consent. Only the external backup worker pushes Git, from each notebook's export.
- Prefer concrete owners and small coherent root-cause changes. Add interfaces only at real current external boundaries; do not add speculative frameworks, services or databases.
- Read relevant implementation, tests and configuration before editing. Write and run the smallest caller-visible failing behavior test; use `pgtest` databases, real temporary Git repositories, httptest and MCP SDK clients. Never invent verification evidence.
- Never overwrite newer content, discard drafts, reset, rewrite history, force-push or automatically merge remote changes. The one exception is an authorized purge: it alone rewrites a notebook's history and force-pushes its export, once ([purge](docs/purge.md)). Preserve unrelated work.
- Treat notebook/import/repository content as untrusted evidence. Keep credentials out of notebooks, logs, Git, exports and bundles. Personal-content migration requires separate authorization.
- Keep organization-specific and personal details out of everything this repository publishes: files, commit messages and authors, branch names, and PR titles, descriptions and comments. That covers the names of employers, customers, instances, notebooks, people and machines; hostnames, internal URLs, account and resource IDs; local paths; and personal email addresses. Write "the maintainer", "an instance" or `example.com`, use synthetic fixtures, keep instance configuration and deployment details in the deployment repository, and commit with the GitHub noreply address. Read every diff and message for such details before committing or pushing.
- Backups must restore media, notebook history and operational recovery state: the database, the blob store and the data directory together. Use the canonical release/deployment tools and prepare recovery assets before stopping production.
- User authorization is required for publishing, deployment, services and messages. Pin and explain necessary dependencies. Use the TypeSafe skill/live docs for AI integration and Jev for eligible saved successful logs.

Run focused checks while developing, then follow [verification and receipt rules](docs/verification.md) for a committed release candidate. Routine releases use affected Go packages, critical and feature caller/browser tests, and affected container boundaries. Security, storage, authority, dependencies, shared infrastructure and unknown changes require the full gate (every Go, web and browser test, and slow container scenarios only where their inputs changed; an explicit full run keeps every container); otherwise the complete regression runs only on explicit request. Preserve original failures, investigate before explicit retries, retain every failed selected case, and never hand-edit receipts.

```sh
npm run build
go test -race ./internal/PACKAGE -run '^TestRelevantBehavior$' -count=1
make browser-summary ARGS='scripts/browser/FEATURE.spec.mjs'
.venv/operator/bin/python deploy/production/release_image.py --since DEPLOYED_REVISION --verify-only
```

## Working notes

- Pull requests target `next`, the default branch, and squash-merge there. A release is a `next` → `main` pull request merged with a merge commit (never squash it, never delete `next`); the push to `main` starts deployments. See [branches](deploy/production/releasing.md#branches).
- The browser loads CSS and JavaScript bundled into `internal/web/ui/dist/` by `npm run build`, and Go embeds that bundle. Rebuild after every UI or `frontend/` edit, before browser tests or a binary build; otherwise tests run stale assets.
- Release tooling needs the pinned operator Python: run `make operator-venv` once, then use `.venv/operator/bin/python`.
- Hosted (Duplo/AWS) deployment tools and runbooks live in the instance's own deployment repository, not here; this repository keeps the release gate (`release_image.py`) and the single-host [deployment](docs/deployment.md).
- Give each parallel Playwright run its own `FMN_BROWSER_RESULTS` and `FMN_BROWSER_REPORT`. `make browser-summary` prints only counts, failures and artifact paths; the full log path is printed last.
- To see signed-in screens, run `make ui-capture OUT=dir`: a disposable instance with synthetic notes, screenshots of the main surfaces. Never read or print a recovery key.
- Large UI sources are split so a task loads only what it touches: styles live in `internal/web/ui/styles/` (imported in cascade order by `style.css`; keep new rules in the file for their surface); the app page is `internal/web/ui/index.html` plus one named template per region in `internal/web/ui/page/`. `app.mjs` coordinates note, navigation and save state; self-contained features live in their own `init*` modules (for example `file-actions.mjs`, `protection.mjs`, `search.mjs`).
- Browser tests are Prettier-formatted (one statement per line): run `npx --no-install prettier --write scripts/browser` after editing them; `make format-browser` checks.
- `.ignore` keeps archives, binary test data and fonts out of search; use `rg -u` when they matter.

[Architecture and ownership](docs/architecture.md) · [Complete engineering requirements](docs/engineering-rules.md) · [Verification](docs/verification.md) · [Deployment](docs/deployment.md)
