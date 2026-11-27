# Administrator automations

Administration → Automation manages background jobs for this instance. The form
handles identity, service account, notebook, schedule, inputs, output and AI
consent, and each task type's own settings: data calls (a connection, one of its
read-only operations, argument fields from the operation's input schema and time
arguments), independent checks with assertions or a reference call, a monitor's
condition and incident alerts, Slack notices, recipe usage rechecks, Google Drive
folders, a repository package's connection and notebook bindings, optional
capabilities and parameters (from its manifest), and source events. Arguments a
schema cannot describe as flat fields stay in a JSON field beside them. The **JSON
definition** shows the same definition for review, import and duplication; an
unchanged form save keeps what the JSON saved. The server validates every
definition however it was built. Changes are saved immediately with revision
checks.
All tasks run inside the existing ForgetMeNote process; job definitions cannot
supply shell commands, scripts, dynamic endpoints or credentials.

## Ownership and setup

1. Create a service account and grant it the required notebook roles and data
   operations. Jobs never inherit the administrator's permissions. Input notebooks
   in `readProjects` are read-only, even if the account has broader permissions.
2. Create a paused automation. Select a maintenance task or import a single job
   JSON file. `Validate` checks its structure and account access. Enabling also
   checks configured providers, connections, operations and Slack destinations.
3. Use `Test`, inspect its retained result and receipts, then enable the job.
   A test performs real reads and may buy model calls. It suppresses note writes,
   Drive checkpoints, Slack delivery and script submissions.
   Learning/event tests require a retained evidence run; they do not invent input.
4. Use Details for failures, results, evidence receipts, token use and delivery
   state. Pause stops future work and cancels superseded queued/active work.
   Delete removes the definition, preserving retained run history.

Instance-managed definitions live in the instance database as one versioned
`automation-definitions` document; runs, effect and repair checkpoints, alerts
and learning state are `automation_records` rows (see [Postgres](postgres.md)).
Operator-managed definitions can instead be mounted through
`AUTOMATIONS_CONFIG_FILE`, using `{"version":2,"jobs":[...]}`. They appear read-only
in the UI; restart after editing the mounted file. Version 1 files remain supported.
An ID cannot belong to both sources. To transfer ownership, pause/drain it, export
it, remove it from its original owner, then import it into the other owner. Do not
run both copies of profile learning under different IDs.

Back up the instance database, the persistent data directory and the operator
configuration and secret references using the existing instance backup
procedure. Notebook Git backups alone do not include jobs, incident state,
accounts or in-flight work. Do not roll back to an older image against new queue
records (script runs use version 7); restore a matching pre-upgrade backup.
Several gateways may run automations. Each running run records the gateway
running it; a gateway that opens the queue returns only its own earlier runs and
those of gateways that are gone to the queue, and one gateway at a time holds
the [scheduler lease](postgres.md#leases-and-presence). A cancellation on any
gateway stops the run within a second.

## Task types

| Kind | Behavior |
| --- | --- |
| `native` | Storage/notebook/job/profile health, scoped activity summary, exact profile duplicate cleanup, chat member pairing by email, or task reminders. Repository notebooks refresh on their own, not through an automation |
| `checks` | Runs every independent assertion/reference, retains all failures, and optionally diagnoses/repairs approved SQL examples |
| `recipe` | Checks named SQL examples in a Markdown recipe; optionally repairs with AI, independently rechecks, then commits |
| `digest` | Runs fixed read-only calls and saves their results; an explicit AI connection can summarize them |
| `monitor` | Tests one JSON condition; uses incident suppression/recovery. Complex interpretation belongs in Python packages |
| `task` | Uses configured read-only tools to maintain one Markdown note from instructions and allowed notebooks |
| `learning` | Debounces opted-in MCP/API usage, optionally routes with the decision provider, creates/optimizes profiles, replay-checks and commits |
| `drive-sync` | Incrementally imports supported Google Drive documents through the existing source importer |
| `script` | Administrator-approved repository package in an isolated Python runtime |

Native health reports are retained with the run, with optional incident delivery.
Other maintenance tasks can publish `output`. Activity summaries contain aggregate
operation/outcome counts, not employee conversations or result rows; retained
coverage is explicit. Exact profile compaction considers at most 256 notes,
removes at most one safe duplicate per run, and uses the existing backlink,
discussion, lock and revision checks. It is not semantic deduplication.

`email-pairing` (`"native": {"task":"email-pairing","client":"slack"}`) pairs a
chat client's workspace members with accounts by verified email so their direct
messages use the person's memory. It runs as the client's `members.actor` and
needs `members.pairByEmail`; see
[personal assistant](personal-assistant.md#pairing-by-email). **Test** reports
what a run would pair and unpair without changing links. The report names members
and accounts by ID, never by address, and members that need review raise an
incident.

`task-reminder` (`"native": {"task":"task-reminder","client":"slack"}`) sends each
[task queue](tasks.md#the-weekday-reminder) owner their working-day reminder from
09:00 in their own time zone. It runs as the client's `members.actor`, names the
client task queues use, and should run every few minutes. It may name up to
three read-only `calendar` connections in `connections` to skip holidays; its
actor needs each connection's `events` grant. People who turned on
[reply reminders](tasks.md#messages-waiting-on-your-reply) also see the Slack
messages waiting on their reply, read with their own grant. **Test** lists, by
person ID, who a run would remind now and, under `wouldCheckReplies`, whose Slack
it would read; it reads none. Reminders that fail, and a holiday calendar that
cannot be read, raise an incident.

## Scheduling and execution

A job can run manually, on an interval of at least 60 seconds, once at an RFC3339
instant, or on a calendar with an IANA timezone. Calendar fields are `hours`,
`minutes`, and optional `weekdays` (0=Sunday). The editor picks **Run at** in the
browser's time zone and saves it with that offset; an unchanged time keeps the
saved value. Catch-up is bounded by
`catchUpSeconds` (up to one day). A repeated DST wall minute executes once; a
nonexistent wall minute is skipped. Missed intervals are not replayed as a burst.

```json
{
  "id": "weekly-maintenance",
  "name": "Weekly maintenance",
  "kind": "native",
  "actor": "YOUR_SERVICE_ACCOUNT_ID",
  "project": "profiles",
  "disabled": true,
  "schedule": {"timezone":"America/Denver","hours":[2],"minutes":[30],"weekdays":[0],"catchUpSeconds":3600},
  "native": {"task":"profile-compaction","folder":"learned"},
  "timeoutSeconds": 180
}
```

Repository events use `trigger.kind: repository.updated`, a `project`, and
`debounceSeconds`/`maxWaitSeconds`. The project must also be an allowed input.
Events coalesce to the newest verified snapshot, are checked again at execution,
and retain a durable cursor after run retention. Slack events use
`trigger.kind: slack.knowledge` and a configured `channel`. Only signed events
from mapped, currently authorized people are captured. Event text expires within
24 hours; batches contain at most 64 events. A task maintains its configured
output note rather than generating a new file for every message.

Independent jobs run concurrently in separate pools: eight AI workers and four
deterministic workers by default. Operators can set `FMN_LIMIT_AUTOMATION_AI_WORKERS`
and `FMN_LIMIT_AUTOMATION_WORKERS` for the instance resources. Provider admission
still enforces shared model capacity. The queue prevents simultaneous runs of
the same job and reserves deterministic capacity independently of AI. A run pins output revisions
before reading/generating; a user's concurrent edit wins. Configuration, grants,
source revisions, exclusions and external-processing consent are rechecked before
provider calls, publication and delivery. Global AI writing controls apply to AI
jobs; disabling AI leaves deterministic checks available. Job consent cannot
bypass instance or source policy. Private Inbox processing is not enabled by a job.

Task/learning jobs and script packages requesting AI inherit the configured instance AI connection and model
when omitted. Existing model reasoning settings remain authoritative. `maxTokens`,
`maxRounds` and `timeoutSeconds` bound individual runs; these do not reserve paid
capacity. Jev remains an optional typed-decision stage within profile learning,
not the scheduler, SQL executor, authorization layer or source of truth.

Interactive chat-client requests inherit the selected AI connection's `runSeconds`
for the whole request. A shorter explicit job timeout or caller deadline still
applies. They reserve the last model round for an answer from retained evidence,
and enter that answer phase when the tool allowance or deadline is approaching.
Automation responses stay buffered until delivery. Streaming events recheck
identity, notebook access and data grants; complete source validation runs before
and after each model call and before publication or delivery. Text fragments do
not repeatedly reread the same pinned notes through Git.
Slack uses its native thread status where supported, or an eyes reaction on the
request. Progress clears after answer delivery or terminal failure.
Interactive chat clients load the bound primary notebook's Owner-managed root
`AGENTS.md` through the same instruction reader as browser chat. Its identity,
response style and domain rules belong to that notebook; additional read notebooks
and ordinary imported documents do not become governing instructions. The run
retains the guidance identity before paid generation and rechecks it before
provider calls and delivery, including recovery of a committed answer. A changed
instruction rejects the stale answer. Instructions cannot grant tools or bypass
instance policy. Edit instructions with the existing revision-checked notebook
Instructions action; no application deployment is needed for a disposition change.

Replies use the same answer and citation guidance as browser chat. Retrieved
evidence remains in the private conversation; it is not all appended to the
message. Slack renders links, emphasis and tables using its
[Markdown block](https://docs.slack.dev/reference/block-kit/blocks/markdown-block/).
Schema-policy errors identify the allowed Redshift schemas and explain how to
discover a table without guessing its schema; the allowlist still applies.
For current measurements, the assistant uses profiles to identify definitions,
then queries permitted live sources. A paused profile does not disable independent
read-only queries. PostgreSQL and Redshift schema discovery support `detail:tables`
and a case-insensitive literal `search` over table and column names, followed by
`detail:columns` for a selected table. Existing calls still return columns by
default. Schema allowlists, excluded columns and explicit result completeness
apply to both views. The assistant reports the date range and data freshness,
and stops discovery once it has enough evidence to answer.

## Automatic query repair

A `checks` job can opt into `repair` in the form or JSON. It inherits the instance
model and reasoning configuration unless overridden. Healthy checks make no model
calls. Instance AI writing policy can disable repairs while checks continue.

Enable the connection's `jsonOutput` capability when its endpoint supports native
JSON mode. Repair still validates every response. Rejected answers and the parser
error are retained only in the administrator's run result, bounded to 256 KiB;
they do not enter notebook health or Git. A rejection leaves the recipe unchanged
and does not trigger an automatic model retry.

```json
{
  "id":"warehouse-metrics",
  "kind":"checks",
  "actor":"YOUR_SERVICE_ACCOUNT_ID",
  "project":"profiles",
  "recipe":"maintained/metrics.md",
  "connections":["warehouse"],
  "externalProcessing":true,
  "intervalSeconds":3600,
  "timeoutSeconds":600,
  "maxTokens":80000,
  "repair":{
    "cooldownSeconds":3600,
    "evidence":[{"connection":"warehouse","operation":"query","arguments":{"sql":"SELECT MAX(updated_at) AS watermark FROM public.events"}}]
  },
  "checks":[
    {"name":"freshness","call":{"connection":"warehouse","operation":"query","arguments":{"sql":"SELECT MAX(updated_at) > CURRENT_TIMESTAMP - INTERVAL '2 days' AS fresh FROM public.events"}},"assertions":[{"pointer":"/rows/0/0","equals":true}]},
    {"name":"active-count","block":"active-count","dependsOn":["freshness"],"call":{"connection":"warehouse","operation":"query","arguments":{}},"reference":{"connection":"warehouse","operation":"query","arguments":{"sql":"SELECT COUNT(*) AS active_count FROM public.events WHERE active = true"}}}
  ]
}
```

The recipe contains a named fenced example, such as `sql active-count`. A repair
may replace only the contents of eligible named SQL blocks. Prose, frontmatter,
other blocks, job configuration, references and expected values are preserved.
Checks without `block` are fixed upstream guards. `dependsOn` names fixed checks
in the same suite; failed guards prevent editing their dependent examples.

For numeric tolerances or richer comparisons, a check may supply `sqlTemplate`
with exactly one `{{sql}}` placeholder inside an operator-owned CTE. The generated
query must be a balanced, self-contained read-only source expression. The server
substitutes it with separating newlines and executes the protected wrapper through
the existing connector. The model cannot edit the wrapper or assertion. Keep the
candidate and reference in that same query for one database observation, or use
fixed, bound periods for separate reference calls. Add representative periods,
empty/nonempty cases and join-grain fixtures; one passing number is insufficient
evidence of business correctness. An operator-defined contract remains necessary.

Each failing suite buys at most one bounded model investigation for the same
recipe, check results, and configured evidence. Evidence calls should return
stable schema definitions or source watermarks, not the current clock. A private
checkpoint survives run retention/restarts; changed evidence can reopen work
after the cooldown. Changing the administrator definition invalidates the old
checkpoint. Upstream unavailability/incomplete results do not trigger generation;
the next scheduled run checks recovery. Assertions are never retried into a pass.

Prefer a check's own `evidence` array for source-specific watermarks/schema reads;
only evidence for failing checks is observed, and identical calls are deduplicated.
Use `repair.evidence` for genuinely shared prerequisites. Changing an unrelated
passing metric must not repeatedly reopen an unchanged failure.
The model receives failed queries and their required guards, with labelled source
evidence and the recipe's definitions. Unrelated passing SQL is omitted from its
prompt; it remains part of the full validation suite.

Before publication, all checks run again: every changed block must pass every
associated check, and previously passing checks must stay passing. Evidence must
remain unchanged. Independent repairs can commit while other upstream failures
remain explicitly unresolved. Publication uses the existing Git commit, revision
and permission checks. A concurrent edit wins. Preview validates without changing
the recipe or the persistent paid-attempt checkpoint. Interrupted generation is
not purchased again; saved publication is revalidated on restart.

The checkpoint retains the previous automatically changed content. If the
published content is untouched, observed upstream evidence is unchanged, and the
previous content now passes every check while the repair fails, the job can
automatically revert it. Otherwise it retains the unresolved condition; it does
not guess a rollback or modify upstream data. Whole-instance backups include the
checkpoint; notebook Git backups include committed repairs.

Administration shows Healthy, Automatically repaired, or Waiting on upstream data
with all check results in Details. API/MCP `find_recipes` and `read_document`
include optional `health` metadata for maintained recipes. It contains check
states only, without private query results. A different revision, paused job or
overdue verification is marked unverified. Clients must preserve these
qualifications; query execution itself does not establish semantic correctness.

Use one repair owner per recipe. Repair reports remain with runs (`output` is
not supported on repair jobs), avoiding a second writer or repeated status commits.
No Slack notifications or additional model provider is enabled by this feature.

Restart resumes saved publication/submission checkpoints. A crash during model
generation records an unknown outcome and suppresses another paid call. Slack
records delivery intent before sending; an unknown response is not resent
implicitly. Inspect failures and explicitly run again when appropriate. Script
submission recovery uses the exact saved UUID and payload, only while its case
revision remains valid.

Deterministic read jobs may opt into `retry: {"maxAttempts":3,"delaySeconds":300}`.
Only explicitly unavailable upstream responses are eligible, before publication
or delivery; original errors remain in `retryErrors`. Permission, schema,
assertion and ambiguous AI/delivery failures do not retry. An incident's repeat
window starts when delivery is attempted, so a failed/unknown delivery remains
visible in run history rather than being silently retried. Upstream-monitor
failures can be watched by a separate `automation-health` job; a pending next run
does not clear the previous failure.

Retention is bounded to 1,000 run files and normally seven days. Under pressure,
older terminal records can be pruned sooner, preserving the current schedule slot
and signed-event retry window. Sensitive learning inputs use their shorter
configured expiry. The queue is private operational state, not a content archive.

## API, CLI and MCP

All clients use the same administrative actions and grants. API credentials need
management consent. Ordinary notebook credentials cannot manage jobs.

```sh
forgetmenote automation status
forgetmenote automation validate --file job.json
forgetmenote automation save --file job.json
# Supply the current revision shown by status when updating:
forgetmenote automation save --file job.json --revision CURRENT_REVISION
forgetmenote automation test job-id --id test-1
forgetmenote automation test learning-job --id test-2 --evidence RETAINED_RUN_ID
forgetmenote automation inspect RUN_ID
forgetmenote automation run job-id --id manual-1
forgetmenote automation cancel RUN_ID
forgetmenote automation delete job-id --revision CURRENT_REVISION
```

Shared action names: `automation_status`, `automation_run`, `validate_automation`,
`save_automation`, `delete_automation`, `test_automation`, `run_automation`, and
`cancel_automation`. API: `POST /api/v1/actions/{name}`. Save takes `spec` and the
current `revision` (empty for a new ID); run/test take `id` and a stable `requestId`.
Tests of retained events also take `evidenceRun`. Revision conflicts return 409.
Pausing is a save with `disabled:true`; duplicating is a new ID and empty revision.

## Drive and Slack

Drive uses three fixed read-only operations (`list`, `download`, `export`) and
runtime refresh-token references. The existing OAuth client and refresh token can
be reused; no credentials belong in jobs or notebook files. Refresh tokens that
rotate require an operator credential manager; this adapter deliberately refuses
to overwrite an external secret. A 401 fails that call and invalidates the cached
access token for the next one.

Drive folder traversal and per-file revision checkpoints survive restarts. Each
run handles at most 32 listing pages and its configured changed-file budget.
Docs export Markdown; Sheets export the first sheet as CSV; Slides export text;
drawings export PNG. Other supported attachments use the existing native source
pipeline. Unsupported files are counted. Updated imports preserve human edits by
failing on conflict. Upstream removals are counted and retained, not deleted.
Moving the configured destination does not relocate already imported notes.

Slack `knowledgeChannels` maps channel IDs to notebooks. `users` maps Slack IDs to
existing accounts. Shared notification channels additionally require a
`broadcasts` entry with an explicit notebook, allowed service `actors`, allowed
`connections`, and `audienceApproved:true`. A job chooses only a configured
`notifyChannel`; it cannot approve its own audience. Script packages granted
`messages.post` can post several messages to that channel in a run and reply in
threads ([Slack posts](automation-packages.md#slack-posts)). The signed Events API uses
the existing Slack handler. Socket Mode is not added by this change.

## Organization installation configuration

Keep organization-specific job definitions and connector bindings in the package
repository alongside its installation checklist.
Installing the server does not install or activate organization workflows.

## Repository packages

Complex workflows use ordinary versioned Python source through `kind: "script"`.
See [automation packages](automation-packages.md) for the host contract, supervised
Python processes, administrator installation and migration. Uninstalled packages
remain inert. Installed packages stay pinned to their approved commit and report a
newer primary-branch digest as an available update for explicit review; an
instance-managed installation can opt into following compatible updates with
`script.followPrimaryBranch`. Packages are sandboxed by default: on Linux with
Landlock they run confined to their own files and scratch directory without network
access, and elsewhere they are refused. `"sandbox": false` in `script` is the
explicit opt-out; see the [package sandbox](automation-packages.md#package-sandbox).

## Package monitors and repository summaries

Health-report parsing, log correlation, task-completion interpretation and source
curation are package-owned. There is no Go `analysis` configuration. Use the
[host contract](automation-packages.md#version-1-host-contract) for structured
findings and revision-aware repository access. Missing evidence must not clear an
incident. The instance owns thresholds, recovery, delivery and optimistic writes.
Organization definitions and threshold values live alongside their packages.

## Unsupported definitions

Only the documented task kinds are accepted for new work. Unsupported definitions
or invalid resumable runs stop startup without rewriting those records. Resolve
them through the version that created them before upgrading, retaining a
restorable backup. Completed, failed and cancelled runs remain readable as
historical snapshots even when their saved definitions predate current rules;
they cannot execute again.
