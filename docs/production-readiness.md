# Production hardening and verification

Integrated chat release checks and limitations are documented in [chat.md](chat.md#validation-and-limits-of-live-verification). Provider/model authentication alone does not establish successful generation. Retrieval, native tool continuations and final answers require separate live verification; keep their synthetic-data receipts with each release. Full backups must include the instance database, which holds chat; standalone notebook bundles cannot restore it. Optional [judgment reviews](judgments.md) and [Quicknotes](quicknotes.md) add durable usage, review, notification, capture and transfer state that also requires a complete instance backup.


The application is one Go binary over Postgres and the blob store, with embedded browser assets; every notebook change is one database transaction. No new runtime service or database is required. Playwright is a pinned development-only dependency for real Chromium regression tests; it is not shipped in the runtime image.

## Recovery and shutdown

SIGTERM stops accepting requests and allows 15 seconds for draining. Requests that wait for changes (the live polls and chat events of open pages, and MCP subscription streams) end at once, and their clients reconnect to the next process; every other request in progress, such as a save, an upload, an import or an MCP tool call, keeps the whole 15 seconds to finish. Then the remaining connections close, and the server waits for their handlers before closing stores. Compose permits 30 seconds before forced termination. Operations must honor cancellation; SIGKILL is never graceful. Every content save and multi-file mutation is one Postgres transaction, with its content blocks stored in the blob store before it commits, so a crash leaves the notebook at the revision before or after the change and restart has nothing to recover. Postgres durability, the blob store's atomic writes and fsync of export objects strengthen durability, but disks, filesystems, and host power protection still determine physical guarantees.

A write that fails, for example on a full disk or an unreachable database, changes nothing and returns an error; retry it once the cause is fixed. If an export stops as diverged or fails `git fsck --strict`, stop the application, take a complete backup, and repair the export with `forgetmenote admin storage export -recreate`; never write to an export repository by hand. After a [purge](purge.md#operators) that rewrote the notebook, `-recreate` rebuilds the export for that purge, a damaged one included, with the flag that lets the backup worker replace the GitHub copy once. If notebook rows or blocks are damaged, restore a known-good complete backup of the database, blob store and data directory together rather than editing rows.

## OAuth upgrade behavior

New MCP grants have a token-family ID and a fixed 30-day lifetime. Rotating a refresh token retains its hash until that family expires. Reuse revokes the family's current refresh and access credentials; the client must reconnect through consent. Records are bounded to 32,768 used hashes and 4,096 active refresh grants. Capacity errors request retry after expiry; they never silently remove live replay evidence. Browser sessions, user identity, permissions, GitHub links, and note history remain unchanged. Public revocation has a separate admission budget from token exchange/consent, so invalid revocations cannot spend the supported reverse proxy’s token budget.

Existing refresh credentials acquire a family on their first rotation, preserving login continuity. Already-consumed credentials from before this upgrade cannot retroactively acquire replay evidence. Existing access credentials still obey their recorded expiry and user session epoch. Administrators who need a clean security cutoff should revoke browser/AI sessions explicitly after the upgrade. Back up the full instance before upgrading; restoring an old OAuth backup can also restore old authorization, so revoke sessions after security-related restoration.

## Bounded work and retained records

Source previews/imports use one durable sources-owned worker with four waiting jobs, local page checkpoints and private 24-hour retention. Browser jobs poll progress; synchronous compatibility callers retain a 110-second deadline. Poppler's `pdftotext` reads physical pages' text layers, independently of notebook write locks; images, media and Apple documents are stored as originals for a connected agent to read. Shutdown cancels process groups and drains the worker; restart requires explicit resume. HTML accepts up to 100,000 tokenizer events before DOM construction, in addition to byte limits. XML/decompression and HTML readers check cancellation. Exact layout and other format limitations remain in [Sources](sources.md).

MCP exports hold at most two snapshots per instance, each at most 1 GiB, for 15 minutes, stored in Postgres so any gateway serves the next chunk. Subsequent chunks use the same snapshot despite new commits and recheck the caller's permissions. Completion removes the snapshot; a periodic expiry sweep, the next export and gateway startup remove abandoned ones, and startup removes temporary bundle files. Transfer expiry/restart requires beginning at offset zero. Larger projects use the operator's full-instance backup process, not unlimited MCP staging.

Editors can **Archive** an applied or dismissed proposal in Review, or call `review_decision` with `decision: "archive"`. Active proposals cannot be archived. Archived full records are `knowledge-proposal` documents in Postgres, outside the active 200-proposal map, and are included in database backups. The active Review list omits them. Retrying an identical proposal ID returns its prior decision; changed content with the same ID conflicts. Archives are intentionally retained for audit and idempotency and consume database storage over time: monitor its capacity and retain them with your backups. A project Git bundle does not include these instance records. No automatic destructive archive purge is provided.

## Operational visibility

`/healthz` is process liveness. `/readyz` checks writable instance storage and at least 32 MiB free, caching its result for five seconds. It does not certify all projects, external OIDC, or the embedding endpoint. Project status reports dirty workspaces; semantic status reports index/provider problems. Readiness failure should remove traffic and alert an operator, not trigger endless destructive recovery attempts. Proxy requests to `/readyz` with the configured public Host header.

Structured request logs contain registered route, method, status, and duration, without note content, search query strings, credentials, or user identifiers. Server errors (5xx) add an `error` attribute: the handler's cause, at most 300 bytes, with URLs reduced to scheme and host, email addresses replaced by `<email>`, and the values of bearer/basic authorization and credential-like `name=value` pairs removed. A cause can still name a notebook file path or database object, so retain request logs as operator-confidential. MCP tool logs additionally record dispatched tool name, success/error (including `isError` inside HTTP 200 responses), and duration; a tool error classified as a server failure (5xx) adds the same redacted `error` attribute. Rejected dispatches use `unknown`; arguments, result bodies, and the text of every other tool error, including rejected arguments, are excluded. Background indexing status remains available in project semantic settings and MCP status tools. Collect these logs through the container platform. Monitor disk capacity, HTTP failures/latency, container restarts/OOMs, failed backups, and the last successful full restoration drill. The app does not schedule host backups or assume a Git remote backs up authentication.

## Release gate

Use the [verification workflow](verification.md) and
[local release command](../deploy/production/releasing.md#local-image-release).
Run focused checks during development, then the selected gate once per committed
candidate. Pass the deployed source revision with `--since`; reviewed single-feature
changes use critical browser journeys plus affected suites and compact source/decision
checks. Shared, security, storage, dependency and unknown changes escalate to full
regression. Use `--mode full` before the first release each week. The runner records reusable successes, original failures and timings
against the source inputs/toolchain and immutable image; it can resume without
rebuilding or rerunning unchanged successes. Publication also records the private
registry digest. Explicitly investigate and report intermittent failures.

The complete gate covers every Go package with the race detector, vet/format,
frontend and real-process fixture tests, standalone/production operator tests,
Linux backup watcher tests, the full Chromium suite, and eight Docker scenarios:
core, headless, source conversion, resource limits, chat, typed decisions,
OIDC password and OIDC with required TOTP. All existing assertions and security
checks remain in the full/affected suites; all Go race, authentication and backup
coverage remains required in routine releases. Independent host checks run within a concurrency limit;
Docker scenarios remain sequential. The checked-in CI and manual release workflow
invoke the same runner. Dependency/image vulnerability scans remain separate
operator checks; no vulnerability scanner is included. Triage findings before release.

The chat smoke verifies synthetic provider HTTP, private scope, durable stream/actions/usage and independent full-volume restore. `SMOKE_RESTORE_IMAGE` optionally selects a different restored app image (default: `SMOKE_IMAGE`); the final `CHAT_RESTORE_IMAGES` receipt contains inspected image IDs after all checks pass. Use both image directions to verify a proposed rollback pair. `SMOKE_PROVIDER_IMAGE` can select an available synthetic Node provider image.

The judgments smoke (`scripts/jev_smoke.py`) uses a synthetic HTTPS service and generated notebooks to exercise remote consent, active-only consistency review, notifications, protected capture, filing and Undo. It restores an independent complete volume and checks durable operation replay without repeated provider calls. Its `JEV_RESTORE_IMAGE` receipt binds that verification to the inspected candidate image. Synthetic responses verify workflow behavior, not model accuracy.

The browser suite runs its own temporary binary, data directory, and accounts; it must never target an existing instance. Test-results files are ignored. Storage tests use real repositories and disposable child processes to kill writes before staging and after commit. Full-instance restoration is tested separately from a project export. Verify users, permissions, source originals, attachments, history, and a new save after restoring into a separate stopped instance.

Before upgrading a real host: stop the writer, take a full backup plus deployment secrets/configuration, record the image digest, then upgrade and verify readiness/login/read/save. If rollback is required, retain current runtime data and verify the exact previous-image compatibility first. Images that predate version-2 chat support require integrated chat to be disabled for all users with `CHAT_CONFIG_FILE=''`; keep the records for forward recovery. A preceding image that already supports version 2 still requires verification of the exact image/data pair. Do not overwrite newer accepted user work with an older backup. A full data restore is a separate recovery operation after assessing what would be lost. Retain the failed instance copy for diagnosis. See [Backups](backups.md).

A protected Inbox makes rollback to any image that ignores `protectedInbox` unsafe, even after every capture is filed: private Git history remains. Keep the new application or perform a forward repair. Do not start the older binary or restore a stale catalog to bypass this guard. Previous images also reject the new `jev` operator configuration member; configuration compatibility alone does not establish data/privacy compatibility.

Performance budgets are measurements, not guarantees: benchmark cold/warm/post-edit browsing at 1,000 and 10,000 notes, mixed reads/writes, concurrent imports within the host's memory limit, and export/restore at the intended attachment volume. Record CPU/RAM/filesystem, latency, peak RSS, disk usage, and restore duration. No Raspberry Pi/VPS throughput SLA is inferred from a Mac test.

The repository has not selected an application redistribution license. Running a private instance and a public open-source release are separate decisions; the owner must choose a license and enable a working private vulnerability-reporting channel before distributing a supported open-source product.
