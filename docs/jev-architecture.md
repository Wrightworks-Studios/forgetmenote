# Decision service and TypeSafe integration

Jev supplies typed judgments for retrieval, citation checks, consistency review,
extraction audits, organization suggestions, and optional Quicknotes routing. The
Go gateway owns consent, permissions, freshness, durable work, and guarded
mutations. A judgment never supplies authorization. Remote processing requires
operator configuration and personal consent; automatic notebook reviews also
require an Owner opt-in.

## Operator configuration

Set `DECISION_CONFIG_FILE=/run/secrets/decisions.json` and mount this operator-owned file
read-only. No chat connection is required:

```json
{
  "provider": "typesafe",
  "enabled": true,
  "model": "jev-1.13.0",
  "endpoint": "https://api.typesafe.ai/v1/systemone",
  "secretFile": "/run/secrets/typesafe-api-key",
  "limits": {
    "requestBytes": 65536,
    "responseBytes": 65536,
    "questions": 32,
    "timeoutSeconds": 20,
    "retries": 0,
    "concurrent": 2,
    "ledgerRecords": 20000
  }
}
```

The optional `jev` member of `CHAT_CONFIG_FILE` still accepts the same object for
existing deployments. Supplying `DECISION_CONFIG_FILE` and a `jev` member together is
an error, including when either definition disables Jev. Both configurations are
validated before replacing the shared service; there is no precedence fallback.

Absent or disabled means no Jev service and no credential reads. An explicit model
is required; pin the evaluated version. The documented `jev-latest` and
`jev-preview` aliases also accept a returned versioned Jev ID, retaining both names
in accounting; feature caches must not treat an alias as an immutable model. The endpoint is an operator-selected HTTPS
URL with the exact `/v1/systemone` path and no credentials, query or fragment. The
server reads at most 4096 credential bytes from a regular mounted file. Mount
`secrets/typesafe-api-key` read-only at `/run/secrets/typesafe-api-key`; the developer
credential is independent. Credentials, endpoints and raw provider errors never
enter public connection metadata. A differently named secret, including a
Kubernetes key named `TYPESAFEAI_KEY`, works by setting `secretFile` to its mounted
path. The key name alone does not configure the service. Restart to load changed
configuration or credentials.

`localOnly` in the chat configuration prohibits enabling Jev through either file
and is checked again on each evaluation. Feature
consent must describe sending the selected excerpts to TypeSafe. It does not enable
web research. The default and maximum request size is 64 KiB, with a separate
32 KiB bound on the shared state plus any one question. These are conservative
byte bounds for Jev's distinct combined-request and per-question context limits,
not token estimates. Explicit smaller operator limits remain effective. No
invented token or generation options are sent.

## Concrete boundary

`internal/decision` exposes Boolean, Choice, and Score questions with normalized
answers. TypeSafe is the only implemented provider. Its adapter owns endpoint
validation, credentials, request encoding, native answer validation, usage parsing,
and retryable HTTP statuses. Boolean questions become TypeSafe Noul questions;
application code consumes `Answer.Probability` instead of a vendor primitive.
The shared service owns authorization callbacks, concurrency, cancellation,
retry scheduling, and durable accounting.

An omitted `provider` defaults to `typesafe` for existing configurations. Unknown
providers fail configuration; changing the endpoint does not make an unrelated API
compatible. A new provider needs a concrete adapter, tests for the required typed
answers and usage accounting, evaluated feature thresholds, and consent that names
the new recipient. There is no automatic provider fallback or synthesized confidence.

Existing `/api/jev` routes, saved consent, the `/data/jev` ledger, and organization
checkpoint encodings remain unchanged. The provider-neutral Go names do not require
a data migration.


`internal/decision.Service.Evaluate(ctx, Evaluation)` is shared by interactive and
background consumers. The call contains:

- A `Scope`: stable actor, notebook, feature, run, optional chat and operation IDs.
- A `Request`: structured untrusted state and application-authored typed questions.
- Explicit local-only and background flags.
- A mandatory `Check(context.Context) error` closure owned by orchestration.

The closure rechecks the active account, current notebook permissions, relevant
feature consent, source containers, evidence hashes/revisions and any explicit
source selections. It runs after admission and before every attempt, and again
before a successful result returns. A caller must recheck before publishing or
applying anything after return; the service cannot lock a remote notebook across
a network call. Failed authorization/freshness is not a provider fallback. Current
consistency checks exclude historical/archived/draft/trash content; historical
retrieval retains each version's classification.

The request sends only `model`, `state`, `questions` to the native API. Noul returns
a probability; Choice and Score include distributions and confidence. Responses
must contain exactly the requested answers, matching types, valid values, complete
probability keys and matching Score legends. Rounded probability intervals must be able to contain a distribution summing
to one; Scores are not reconstructed from rounded values. Missing or malformed
answers fail the whole evaluation. Missing usage is an invalid result with retained
unknown cost. No note or model content is logged. There is no shared result cache. Durable checkpoints belong to their recorded
account, notebook and source inventory.

Interactive calls receive priority over queued background work. With the default
two slots only one background call can run. The whole evaluation, including queue
wait and retries, shares a 20-second deadline and caller cancellation. Shutdown or
reconfiguration cancels old service work and waits for settlement. Retries are off
by default; if enabled, only 429/529 responses are retried, at most twice, with
backoff. Each attempt rechecks authority and durably records its estimated usage. Redirects are
never followed, and transport failures are not blindly retried.

## Durable accounting

The ledger is the `jev_usage` table of the instance's Postgres database, one row
per reserved attempt, shared by every gateway. Each
record contains actor/notebook/feature/run/chat/operation identity, attempt number,
UTC request day, request hash, requested/returned model, conservative reservation,
status and optional actual input/output usage. It contains no source text,
question text, prompts, credential or answer. Notebook/chat deletion must not erase
these accounting records.

Before each paid attempt its row is inserted under the attempt's identity, so an
attempt is reserved once across every gateway; only the ledger that reserved it
settles it. Storage failures fail closed.
Settlement occurs before returning results, including invalid provider responses
with independently usable token counts. Invalid answer fields cannot discard those
reported counts, including on non-200 responses or duplicate answer fields.
Case aliases in typed fields are rejected without changing case-sensitive question
and option IDs. Missing or null persisted counters are corruption, never
a zero-token bill. A reservation whose ledger stopped, or that another gateway
still holds, is an unknown cost; unknown calls retain their estimates without preventing later work. A stable operation/attempt already
in the ledger cannot silently call again after restart. Consumers must reconcile
its checkpoint and, if needed, schedule an explicitly new bounded operation.

There are no per-run, per-user/day or instance/day token quotas, and no cumulative
request allowance. Legacy `dailyTokens`, `userDailyTokens`, `runTokens` and
`runRequests` operator keys are accepted and ignored. Reported usage and unknown
estimates are accounting only; neither requires a fresh allowance to continue.

Per-request HTTP sizes, provider/context-compatible batches, elapsed time,
concurrency and finite retries remain bounded. Ledger storage has a record-count
limit and a hard 16 MiB size bound; those bounds protect persistence, not spending.
Reservation estimates use request bytes plus question/output allowances because
the API offers no token cap. Estimates are not guarantees about billing. Reported
usage is retained exactly, including usage above estimates. No monetary price is
invented. The record-count limit (`ledgerRecords`, at most 20,000, the file-based
release's capacity) is kept by compaction, never by deleting accounting. When a
reservation finds the ledger full, the oldest settled attempts from before the
last seven days become daily totals in `jev_usage_totals` (attempts, input and
output tokens and estimates per day, actor, notebook, feature and requested and
returned model) and leave the ledger, until it is three-quarters full. Reserved
and unknown attempts never leave it, so an uncertain call is never purchased
again; a settled operation older than a week may be called again if a consumer
retries it. When only recent or uncertain attempts fill it, reservations still
fail closed with `ErrLimit`. Before November 2026 there was no compaction, and a
full ledger stopped every feature until a release. `export-files` writes only
the ledger; the file-based release has no place for the totals.

`Evaluate` errors keep their text and `errors.Is` meaning, and `decision.Failed`
names the step that failed (`scope`, `prepare`, `queue`, `check`, `reserve`,
`provider`, `settle` or `deadline`) and the provider's HTTP status, so callers can
log why without request content.

Reserve writes leave settlement headroom. Failed writes disable that ledger owner
until restart. Sidecar state must be included in complete instance backups and restored
with the notebooks. Derived caches remain separate and independently rebuildable.
Existing conversation schemas remain unchanged. Older images ignore Jev sidecar
files, but they do not enforce the protected Inbox policy added to the notebook
catalog. Once an Inbox exists, rollback to an older binary is prohibited; see
[Quicknotes](quicknotes.md) and [backup and rollback policy](backups.md). Rollback
also needs a compatible operator config because older versions reject `jev`.
Never restore older content or usage merely to roll back an image.

## References and validation

Contract checked on 2026-09-23 against official [API](https://docs.typesafe.ai/api),
[models](https://docs.typesafe.ai/models), [Choice](https://docs.typesafe.ai/primitives/choice),
[Score](https://docs.typesafe.ai/primitives/score), [Noul](https://docs.typesafe.ai/primitives/noul),
[confidence](https://docs.typesafe.ai/confidence),
[reranking](https://docs.typesafe.ai/cookbooks/rerank_typesafe) and
[citation checking](https://docs.typesafe.ai/cookbooks/citation_check) documentation.
Synthetic HTTP tests cover wire shape, mounted credentials, malformed answers,
authority/freshness, local-only, retries, cancellation, priorities, resource bounds,
restart recovery, single ownership, parent-sync failure and incomplete/corrupt
persistence. Unknown accounting does not prevent a fresh authorized operation. Feature quality,
full-volume restore, browser regressions and complete release checks remain separate
acceptance gates; mocked answers do not establish live model accuracy.


## Retrieval and citation consumers

`knowledge.RelevanceRequest` builds the native Score questions used by actual
retrieval and synthetic quality evaluation. The authenticated gateway injects a
concrete Jev ranking binding before final passage packing. `knowledge.ScorePassages`
judges the leading candidates for both notes and repositories, using one independent
question per passage. The shared state contains only the query; each question's
structured instructions contain its exact path, heading, and excerpt. Callers
select at most `RelevanceCandidateLimit` (32) before
final packing and preserve the unscored tail. It sizes batches against the service's byte
and question limits and runs them concurrently under the existing admission gate.
The defaults usually fit the entire shortlist in one API request. The service
owns authorization immediately before each paid attempt and after its response.
A single successful batch relies on those checks; multiple batches receive a
final combined check, and provider failures recheck authorization before fallback.
Identical passage inputs (path, heading, and text) share one judgment before
batching, with the score restored to every original occurrence. Different paths
or headings remain distinct; no cross-request result cache is added.
Exact matches and source priority retain precedence. A provider failure cancels
queued work and discards the partial ranking, falling back to the original pool.
Authorization and source-freshness failures return an error. The final existing
packer still owns multi-passage evidence, neighboring passages and context fit.

Personal `/api/jev/settings` consent defaults off. The operator must configure
Jev and local-only must be false. Reranking and citation checks have independent
personal preferences. Notebook auto-check settings require Owner authority.

After a completed answer, all three citation gates (operator, personal consent,
personal citations) are checked before a durable pending sidecar is created.
The pending state precedes the completed chat event; checks then run independently
of streaming. A service-owned queue retains only authorization and durable
chat/run/check identifiers. At most two complete citation pipelines load and
prepare conversations at once; request-side conversation reads are separately
serialized. Queued checks remain pending, and the 60-second execution deadline
starts on admission. Session, consent, and access are rechecked before loading
queued content. Chat deletion removes its queued identifiers and sidecar. After
restart, interrupted checks require an explicit request with fresh authority. GET/POST `/p/{project}/api/chat/{chat}/runs/{run}/citations` inspect or
explicitly rerun the check. Paragraph-local single-link claims are mapped to their
original citation records. Ambiguous associations and tables remain unchecked.
Direct quotations must occur in the cited excerpt before Jev is called. Native
Choice judgments retain supported, partial, contradicted, unsupported and uncertain
outcomes; low-confidence choices are shown uncertain. Partial work is incomplete.

The citation sidecar binds content hashes and visibility of the currently permitted
corpus, plus each immutable original citation. Permissions, session, personal
consent, and source freshness are rechecked before and after paid work. Shared
review bookkeeping may advance Git HEAD without invalidating unchanged content.
Private citation claims are deleted with their chat under its content fence;
metadata-only usage remains in the shared ledger. No transcript schema changes
are needed. Citation checks provide no authority to mutate note content.

Quicknotes uses the same native service and ledger with feature `quicknotes`;
operator reload updates that borrower before retiring the previous service.


## Durable notebook reviews

The project APIs are `POST /api/jev/jobs`, `GET /api/jev/jobs`,
`GET /api/jev/jobs/{id}`, and `POST /api/jev/jobs/{id}/continue|cancel`, under the
usual `/p/{project}` prefix. Creation requires Comment permission, personal
remote-processing consent, `kind` (`consistency`, `audit`, or `organization`), and
an idempotent `operationId`. Optional inputs are `sources: [{path, revision}]`,
`destinations: [path]`, and `tags: [string]`. Reusing an operation with a different
request conflicts. Job control belongs to the initiating account or a current
notebook Owner; execution always rechecks the original stable account identity.
Read permission governs job summaries and result disclosure.

Jobs have queued, running, paused, completed, cancelled, or failed states. One
concrete execution slot loads and advances a review at a time; source preparation
is also serialized. A two-minute execution batch yields before starting new work,
and queued work resumes with a fresh batch deadline. This is elapsed-time and
memory control, not a total request or token allowance. Paid-call failure pauses
at the durable checkpoint. Explicit Continue records a new attempt only for an
unfinished judgment. A saved judgment or mutation intent is reconciled without
buying the same judgment again. Restart leaves an interrupted in-flight review
available for explicit continuation; already queued work is picked up by the
scheduler. Model changes require a fresh review instead of mixing model versions.

Jev's orchestration records live by name in the `jev_records` table. The job
record `jobs/{id}.json` is a version-2 metadata record with stable identity,
source hashes/classifications, inventory fingerprint, ordered work, cursor,
verdicts, and the current mutation intent. It contains no accumulated result
excerpts. Each completed comparison first writes a version-1 checksummed
`results/{id}-{index}.json` record, then advances the metadata cursor. Both have a 4 MiB per-record resource bound. Lost or malformed result records fail disclosure instead of
inventing successful output. Result storage survives restart and belongs in a
database restore. The shared usage ledger remains independently authoritative
for attempted remote calls, including unknown costs.

Job list responses contain summaries. Job detail accepts `offset` and `limit`
(default 50, maximum 100) and returns `results`, `resultOffset`, `resultTotal`, and
`resultNext` (null at the end). Paging limits response and browser size without
dropping completed work. Source changes hide obsolete results and ask for a new
review. Cancelled or interrupted reviews retain their completed evidence and
accounting; no automatic cleanup discards paid progress.

Consistency and organization use only committed active Notes, excluding Drafts,
Archives, Trash, policy files, and accepted-but-uncommitted live edits. Candidate
preparation reuses the existing BM25 index, explicit links, and nearby passages.
Consistency checks each selected passage and up to four related passage candidates,
deduplicating pairs. This is useful candidate coverage, not all-pairs verification.
The exact work count, examined count, and remaining count are visible. The current
inventory includes visibility and note hashes; bookkeeping-only Git commits do
not invalidate it, while added, edited, moved, or reclassified notes do.

Choice confidence below 0.6 becomes uncertain. A shared possible-contradiction
finding additionally needs confidence at least 0.8 and contradiction probability
at least 0.9. It records original evidence and uses the existing guarded
`AddFinding` operation with a stable duplicate receipt. Canonical receipt identity
includes sorted passage IDs and whole-note source hashes: changed sections or
metadata get a fresh finding, while reversed pairs and bookkeeping-only commits
retain the same identity. Findings never decide
which note is correct. Resolve, dismiss, or mark intentional uses the existing
review decision boundary; intentional is a dismissal disposition. Any correction
requires the existing reviewed proposal flow.

Automatic consistency is off by default. Owner opt-in records the current active
inventory and its revision as its baseline rather than launching an initial full
review. A 30-second scanner waits for about two minutes of stable committed
changes, then checks only new passages against current candidates under the
enabling account's current Owner role and personal consent. A passage is new when
its text was not in a changed or removed note at the baseline revision, so a
one-line edit checks the passage holding it, not the whole note, and a moved or
reordered note with unchanged text checks nothing and just advances the
baseline. A baseline recorded before its revision was kept checks its changed
notes whole once. Successful coverage advances the
baseline; paused failures await an explicit retry. Disabled/deleted accounts,
revoked Owner authority, revoked consent, disabled operator service, local-only,
or changed source content prevent transmission or publication.

## Extraction and organization

Extraction requires explicitly selected original source revisions and current
active destination notes. Sources may be historical or archived, but never Drafts
or Trash. Original passages are compared against every selected destination
passage, in relevance-ordered batches of two. No relevant-looking shortlist is
used to claim missing coverage. Repeated canonical source/revision selections
are deduplicated. Before allocating the source-by-destination work graph, a
conservative serialized-size preflight reserves checkpoint headroom within the
4 MiB record bound; oversized selections receive HTTP 413 with guidance to
choose smaller source or destination selections. Cancellation interrupts
preparation. This memory/storage safeguard does not limit tokens or requests
within an admitted review. Results merge per source passage and stay unchecked
until every recorded destination batch is complete. Conflicting evidence or
multiple partial results remain uncertain. Each merged result reports completed
and total batches, destination passage count, and outcome counts. The preview
shows at most eight original excerpts, prioritizing useful matches; evidenceShown
and evidenceTotal disclose that limit. Full batch evidence remains durable. Both
job reads and notifications validate all dependencies, including excerpts outside
the displayed preview.

Organization judgments identify duplicate, complementary, versioned, related,
unrelated, or uncertain passage pairs. Related/complementary/versioned pairs can
create a guarded link proposal. Duplicate judgments remain suggestions for a
separate reviewed consolidation; they never merge or archive a note. Independent
Noul questions consider only existing or explicitly supplied tags, including a
none-fit question. Suggestions require tag probability at least 0.85 and none-fit
below 0.2. Multiple tags may fit. Tag proposals merge through the existing metadata
parser, preserve other frontmatter and body bytes, and never write the note until
the user accepts in Review. Existing hash/revision checks reject stale acceptance.

## Notifications and Quicknotes

`GET /api/notifications` returns currently authorized items, `unreadCount`, `total`,
`offset`, and `next`; paging uses the same default 50/maximum 100 as jobs.
`POST /api/notifications/read` accepts `{ids:[...]}` and reauthorizes every ID.
Private version-1 read state is keyed by stable account identity. Shared findings
and decisions stay in the notebook's existing Git review data. Counts, previews,
and result detail are filtered before disclosure; resolved/dismissed or stale
findings disappear. The implementation reads one job metadata record at a time.

When its notification preference is enabled, Quicknotes also contributes private
filing events showing the current destination. Current permission, note version,
and hash are checked; Undo withdraws the event. Capture bodies remain ordinary
Markdown in the protected private Inbox, not in Jev metadata. Manual, Suggest, and
Automatic routing use existing permitted destinations and the shared native
service/ledger. See [Quicknotes](quicknotes.md) for protected Inbox provisioning,
explicit shared-destination approval, guarded transfers, and Undo.

These interactive personal APIs are deliberately enumerated in the MCP route
inventory exception list. They do not expose a new MCP mutation bypass. Notebook
findings and proposal acceptance continue through the existing shared workspace
owners, including distributed owners.

## Organizer contract

The browser-only organization grant is separate from existing review proposals:
`POST /p/{project}/api/jev/jobs` accepts `kind: "organize"` and an `organization`
object with `mode: "preview"|"apply"`, `scope: "all"|"folder"|"selected"`, optional
`folder`/`paths`, and `policy: {tags, links, moves, duplicates, vocabulary, folders}`.
An optional `reconsider` flag explicitly overrides evidence suppression. Applying
requires current Owner/Admin authority; preview requires Comment. Models never
submit this object or supply paths; the server enumerates all action candidates.

`GET /p/{project}/api/jev/organization` returns eligible destinations, notes, tags
and the saved automatic policy without inference. Owner-only POST accepts
`{enabled, generation, policy}` with optimistic generation matching. Owner-only
`POST /p/{project}/api/jev/jobs/{id}/organization/undo|dismiss` accepts optional
`{index}`; omitting it applies the control to the run. Undo returns per-action
outcomes, including conflicts. These routes are explicit interactive exceptions
in the MCP parity inventory; no existing model or OAuth write scope gains an
organizer grant. Their file operations use the same locked workspace owner and
its link, journal, discussion and managed Trash mechanisms.

No operator token budget is added. Existing Jev request-size, question, retry,
concurrency and storage settings remain authoritative; unsupported complete
verification becomes a suggestion, never a truncated destructive judgment.
Read [organization](organization.md) for exact defaults, versioned state,
transaction/disable boundaries, activity and backup compatibility.
