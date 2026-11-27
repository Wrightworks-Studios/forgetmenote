# Data connections and automation

One ForgetMeNote process runs these optional features. The UI, API, CLI, MCP and
built-in assistant use the same connection handlers and current grants.
Administrators configure endpoints, credentials, and workflow packages for their
instance.

## Administrator jobs

Jobs can now be edited in Administration → Automation or through the shared
API/CLI/MCP actions. See [administrator automations](../../docs/automations.md)
and [organization package configuration](automations/README.md). Mounted definitions
remain supported and read-only in the UI; no jobs are enabled by adding API keys.

## Configure an instance

Mount operator-owned JSON and credentials outside **all notebook repositories**:

| Environment variable | Example | Purpose |
| --- | --- | --- |
| `DATA_CONNECTIONS_CONFIG_FILE` | `/etc/forgetmenote/connections.json` | Explicit endpoints, operations, limits, secret references |
| `AUTOMATIONS_CONFIG_FILE` | `/etc/forgetmenote/automations.json` | Optional file-managed jobs; UI-managed definitions need no mounted job file |
| `SLACK_CONFIG_FILE` | `/etc/forgetmenote/slack.json` | Slack workspace, secret references and optional initial chat policy |
| `CHAT_CONFIG_FILE` | Existing chat configuration | Existing model providers; reused by automation |
| `DECISION_CONFIG_FILE` | `/etc/forgetmenote/decisions.json` | Optional typed decisions (`provider: typesafe`); independent of chat providers; see [judgments configuration](../../docs/judgments-architecture.md) |

Keep `/data` on a persistent volume and retain a **single instance owner**. All
features work with `--headless`. The existing Docker image includes the Go
adapters; no new sidecar, daemon or application database is required. PostgreSQL
is an optional external data source, not ForgetMeNote's storage.

Copy only the connections you need from `data-connections.example.json`, replace
the endpoints and mount their credentials. Restart to load operator configuration.
API users cannot change a destination, supply a credential, or dynamically register
remote tools. Secrets are reread at each invocation; mounted secret rotation does
not need a restart. `secretEnv` is an alternative to `secretFile`, not an additional
credential source. Errors omit upstream response bodies and database statements.

The HTTP adapter supports fixed GET/POST/PUT/PATCH/DELETE operations with input
and output JSON schemas. `{argument}` placeholders fill individual path segments
or query values. `body: true` sends the validated arguments as JSON. Redirects
and ambient HTTP proxies are disabled. `authScheme` supports `Bearer`, `Basic`,
or `raw`, with a configurable `authHeader`. Basic secret files contain the
base64-encoded service credential pair, or use `usernameFile`/`usernameEnv` with
`secretFile`/`secretEnv` for separate username and password values. Do not put credentials in operation
schemas or arguments. The examples illustrate vendor APIs; validate endpoints,
versions and service permissions against your installations before enabling them.

HTTP connections can use `aws: {"region":"us-east-1","service":"es"}` with
the existing AWS SDK runtime credential chain instead of a static HTTP credential.
For Google Drive-style OAuth, `oauthRefresh` supplies a fixed `tokenURL`,
`clientID` and runtime `clientSecretFile`/`clientSecretEnv` plus
`refreshTokenFile`/`refreshTokenEnv`. Credentials stay outside notebooks; refreshed
access tokens are cached only in memory. See the migration connection examples.
`response: "file"` returns bounded base64 bytes for source imports;
`response: "status"` includes HTTP status/body for health checks.
`bodyArgument: "submission"` sends only that validated object as the body.

For a read-only JSON POST code executor, `executionDiagnostics: true` recognizes
a failed execution envelope (`exitCode`, `stderr`) on HTTP 400/422. It reports
only a built-in Python exception type and the last `<shell-ro>` submitted-code
line number. Exception messages, tracebacks, stdout and upstream metadata remain
private. Other HTTP errors retain their existing behavior. This is operator
policy; callers cannot enable diagnostics or change the execution endpoint.

For renewable sessions, configure `login` with a fixed `path`, `usernameFile` or
`usernameEnv`, scalar `fields` (for example `{"provider":"db","refresh":true}`),
`tokenField: "access_token"`, and `ttlSeconds: 300`; `secretFile` holds the password.
Login stays at the fixed origin. Credential rotation or expiry invalidates the
cache. A 401 is reported without replaying the operation; the next call logs in again.

To sign in as a GitHub App installation, set `githubApp` with the numeric `appId`
and `installationId` and an absolute `privateKeyFile` holding the App's PEM key.
Each call uses an installation token from the connection's endpoint, kept in
memory until a minute before it expires; a replaced key file takes effect on the
next call. `repositories` (names within the installation's account) and
`permissions` (`read` or `write` for each GitHub permission) narrow every token
below what the installation grants; give them only what the configured operations
need. A 401 drops the token without replaying the call. The `github` entry in
`data-connections.example.json` reads, searches and triages issues; change `owner`
in its schemas to your organization.

Public endpoints require HTTPS. An operator may set `allowClusterHTTP: true` for
an existing private endpoint of the form `http://SERVICE.NAMESPACE.svc.cluster.local:PORT`.
This does not enable arbitrary HTTP destinations, redirects or caller-selected endpoints.
HTTP connections can set a fixed `hostHeader` (DNS hostname, no port) when the
upstream uses virtual hosting. It applies to operation requests; the configured
endpoint still determines the network destination and TLS certificate validation.
Callers cannot override it through operation arguments.

`queryLists: {"e":"events"}` serializes an argument array into repeated query
parameters (objects become JSON). An operation can enforce a date window with
`range: {"start":"start","end":"end","format":"date","maxSeconds":2592000}`.
`date` means inclusive `YYYYMMDD` days; `rfc3339` measures elapsed seconds.
Schemas should also constrain filters, pagination, response size and allowed fields.

Redshift connections may set `schemas`, `deniedColumns`, and `maxCells` inside
`redshift`. Policy-controlled queries require explicit schema-qualified relations
or in-scope CTEs and explicit selected columns (except `COUNT(*)`). Unsupported
relation syntax fails closed. Schema discovery applies the same exclusions;
row/cell clipping reports `complete: false`. This conservative SQL guard supplements
the independently read-only database identity; database permissions remain
authoritative for views and functions.

The MCP adapter discovers schemas but exposes **only** operation names listed by
the operator. Remote read-only annotations never grant authority. An optional
`schemaHash` pins an expected operation contract; a changed schema fails closed.
This is the extension boundary for additional data systems. It uses runtime
service credentials.

Compose forwards the four optional configuration variables. Uncomment its
integration mount (or add an equivalent read-only bind mount), put the files in
that operator-owned directory, and set the variables to their container paths.
Bootstrap service accounts and grants before restarting with scheduled jobs.

## Access

In Administration → Connections, select a configured connection, person or group,
then grant individual operations. Membership in a notebook grants no data access.
Deleting a group removes its connection grants. Disabled users and revoked grants
are rejected on subsequent calls and before returning results. Grant administration
does not contact upstream services; the UI can remove existing access even when
operation discovery is unavailable.

API credentials need both `data:read` (and `data:write` for mutations) and an
explicit `connections` list. Existing notebook-only credentials gain no data
access. Administrators can select data access and connections in the service
account credential form or use `issue_service_credential`. Browser OAuth login can
request additional consent with `forgetmenote auth login --data-access read`.
Connection grants still apply. Built-in chat and automated model calls only use
connections marked `externalProcessing: true`, and only run read-only operations.
Chat client conversations are also shown the write operations their actor is
granted, so the model can propose one for the requester to confirm.

Examples after authentication:

```sh
forgetmenote data list
forgetmenote data operations warehouse
forgetmenote data call warehouse query --file query.json
forgetmenote recipe find profiles --query revenue
forgetmenote automation status
forgetmenote automation run database-recipe --id manual-check-1
```

`query.json` contains `{"sql":"SELECT $1::text AS example","params":["hello"]}`.
The corresponding MCP/API actions are `list_data_connections`,
`list_data_operations`, `call_data_operation`, `find_recipes`,
`automation_status`, `run_automation`, and `cancel_automation`. API actions are
POST requests to `/api/v1/actions/{name}`. Administrative actions require explicit
management consent and cannot be invoked by ordinary API credentials.

Every successful operation returns a receipt identifying the connection,
operation, schema hash, input/output hashes, UTC capture time, latency and
completeness. `verification: executed` means the call ran; it is not a claim of
semantic correctness. An intact HTTP/MCP response can still describe a page or a
partial provider search; inspect the provider's pagination and completeness
fields. PostgreSQL row clipping explicitly sets `complete: false`. Oversized
responses fail instead of silently dropping bytes. Built-in chat shows dated
live-result citations and does not reuse those results as fresh notebook evidence.

## PostgreSQL

Use a dedicated, minimally privileged SELECT-only database role. Do not use a
database owner or superuser. Revoke unwanted schema/function execution privileges
and grant only approved tables/views; SQL functions and database extensions are
part of the operator's trust boundary. Remote connections require TLS certificate
verification (`sslmode=verify-full`); loopback fixtures can use `disable`.

Only `schema` and `query` operations exist. Queries execute as a prepared SELECT
subquery inside a read-only transaction and are rolled back. PostgreSQL rejects
multiple statements, transaction control, DDL and modifying CTEs. Bind parameters
with `$1`, `$2`, etc. Server row limits, statement/lock timeouts, caller cancellation
and response byte limits bound the work. `pgx/v5` is pinned in `go.mod` solely for
this external protocol; no database is introduced for application state.

## Redshift Data API and curated application helpers

`kind: redshift` uses the pinned AWS SDK v2 for signing, refreshing IAM credentials,
polling, cancellation and pagination. Configure `redshift.region`, `database`, and
exactly one `cluster` or `workgroup`. Provisioned clusters can specify `dbUser`;
`secretArn` is an alternative AWS Secrets Manager reference. Use a pod IAM role in
production. An optional `endpoint` supports private endpoints; normally omit it.

Grant ExecuteStatement, DescribeStatement, GetStatementResult and CancelStatement
plus the applicable GetCredentials/Secrets Manager permissions. Bind that identity
to a **SELECT-only database role** with approved tables and functions. The Data
API does not provide PostgreSQL's read-only transaction enforcement. The adapter
accepts one SELECT/WITH expression, rejects writes, wraps it with a row bound, and
binds `:name` parameters using `{"params":{"name":"value"}}`. Parameters are
nonempty strings; cast in SQL for other types and use SQL `NULL` for null literals.
Dollar-quoted strings and backslash escapes are rejected by the expression guard.

`query` and `schema` return columns, rows and a shared receipt. Pages are bounded
and row clipping is explicit. Interrupted polling attempts remote cancellation
using a separate three-second deadline. Verify cancellation and the read-only
database role against AWS before enabling production callers. Syntax/data errors
can trigger repair; permission, credential, outage and resource failures do not.
AWS failure bodies and credentials are never returned to callers.

`kind: broker` calls `/v1/shell-ro/run` using an operator-selected Python registry.
The example uses `application.data_tools` with `list_tool_metadata` and
`invoke_tool`; replace these with your registry module and functions. Select
exact helper names and grant them individually. Live schemas are validated before
every call, including exclusive argument groups, enums and bounds. `schemaHash`
can pin a contract. Caller
arguments travel as base64-encoded JSON in a fixed generated invocation; they
cannot provide code or module names. Execution failures, missing envelopes and
truncated stdout/stderr fail closed. The upstream broker and database role must
independently enforce read-only execution. The optional `jsonEncoder` names an
operator-selected Python class for dates/decimals. Other applications can expose
the same curated registry contract without changes to ForgetMeNote. Argument
contracts support strings, integers, booleans, dates, datetimes and typed arrays.

## Recipe maintenance and digests

Profiles remain ordinary Markdown notes, tagged `recipe` for discovery. An agent
reads them, inspects operation schemas and calls ordinary tools. There is no
executable profile language. Existing notebook retrieval, embeddings, maps and
judgment ranking continue to locate useful knowledge; a judgment does not execute SQL or decide
whether a publication is authorized.

Optional `usage: {"errors":true,"slowMs":5000,"cooldownSeconds":3600}` queues
checks after a matching live call cites `project`, `profilePath` and
`profileRevision`. Matching uses the exact notebook/path and configured
connection/operation. The durable queue enforces an elapsed cooldown and deduplicates
active work across restarts. It retains only revision, outcome, duration and receipt
metadata, never live SQL, arguments, results or the user's question. Background
checks cannot recursively trigger themselves. Queue admission failures appear as
`recipe.maintenance` activity failures; the original data call keeps its result.

The CLI supports `data call CONNECTION query --file query.json --notebook profiles
--profile counts.md --revision COMMIT [--schema-hash HASH]`. MCP/API use the same
provenance fields. Without an opt-in learning job, calls without a recipe reference
do not search for an owner or consume an LLM turn.

Create a service account using the existing administration UI. Give it read/edit
access only to the relevant notebooks and grant its exact data operations. Set
that account ID in `automations.example.json`. The example jobs need no AI. Copy
`database-health.md` into the profile notebook using the normal note API or CLI;
sample content is never installed automatically.

A recipe job runs configured checks against named ordinary SQL examples such as
` ```sql read-only `. Expected values are operator-controlled JSON Pointer
assertions, outside the note. For real profiles, supply representative independent
fixtures/known answers and edge cases. The included connection check demonstrates
the mechanism; it is not a correctness suite for your application's queries.

If checks fail and the job has an `aiConnection`, `model` and explicit
`externalProcessing: true`, the existing provider can propose a complete repair.
The candidate must pass the same independent checks before a version-checked
commit. A passing recipe needs no model call. A failed candidate, permission loss,
lock or concurrent edit causes a failed job without overwriting
content. Assertions test the configured contract; they cannot prove arbitrary
business reasoning or all prose correct. Configure application-specific checks
before granting automatic publication.

Checks can also set `maxDurationMs`; both the original and the candidate must
meet the same operator-owned correctness and latency requirements. A usage signal
starts verification, not unconditional rewriting. Sporadic slow calls that pass
the checks need no AI. Checks should use stable representative inputs and allow
normal network variation in latency bounds.

Connection outages and access failures do not trigger model repair. Automated
AI uses chat's source-consent/exclusion checks, global and per-provider concurrency,
context, time, round and tool limits. Source and data permissions are checked
again before publication or delivery, including when a staged job resumes.

Digest jobs execute a fixed list of read-only calls and save a Markdown report.
An optional configured model summarizes the captured data. A digest's destination
is explicit operator consent to share those results with that notebook's members;
choose its membership accordingly. Receipts accompany summaries.

Set `notifySlackUser` on a digest to deliver it privately after the commit. Large
reports produce a saved-note notification. A `monitor` job executes one fixed
call and only delivers when its `condition` matches. Conditions use a JSON Pointer
and exactly one of `equals`, `greaterThan`, or `lessThan`. Missing fields,
incomplete input and failed calls fail the job; they are not reported as healthy.
Example: `"condition":{"pointer":"/errors","greaterThan":0}`. Monitors need
`notifySlackUser` and do not write a notebook. Every matching scheduled interval
can notify; choose the interval to match the desired alert cadence. Recipient
permissions are checked independently of the worker's service account.

Schedules are intervals of at least 60 seconds. Missed intervals coalesce to the
current slot, without a catch-up storm. One background job runs at a time;
connection and model limits also apply. Manual run IDs deduplicate retries. Jobs
and staged publications are private atomic files under `/data/automations`;
successful commits use the existing workspace operation journal for replay after
a crash. Changed configuration prevents old pending work from executing. Completed
metadata is normally retained for up to seven days with a 1,000-run capacity.
Older terminal records can be evicted sooner to admit new work; active jobs and records within
the five-minute signed-event retry window are retained. A scheduled run's record
is retained through its current interval to prevent duplicate execution. Manual
request IDs deduplicate while their records are retained. Use the Automation panel or CLI to
inspect failures and cancel.

Persisted background runs use format version 5; interactive chat runs use version 6
to preserve their client, audience, file and delivery policy. Earlier runs remain readable. Take a full instance
backup before upgrading; rollback requires the previous binary **and** its
compatible snapshot, not just an image change.

## Learning profiles from usage

An opt-in `kind: "learning"` job discovers patterns without a predeclared recipe or
operator-authored test cases. Merge the job in [learning.example.json](learning.example.json)
into the private automations configuration. Set its service account, profile
notebook and existing chat connection. Grant that account notebook Edit and the
required read-only connection operations. Each source connection also needs
`externalProcessing: true`. `learning.jev: true` requires the existing judgments
configuration and its background budget; `false` uses deterministic shape routing.
The ordinary configured model synthesizes and writes the profile. No new worker,
database, or provider credential mechanism is required.

`captureInputs: true` explicitly permits temporary storage and model processing
of eligible data-operation arguments and optional task intent. It also permits
publication of reusable guidance into the configured notebook; choose its members
accordingly. Only callers who can read that notebook contribute. Caller identity,
credential restrictions, notebook access, connection grants, source exclusions and
service-account authority are checked again while processing and before saving.
Obvious credential-bearing inputs and reflections of the connection's injected
secret are skipped. This screening is not a general personal-data anonymizer.

The worker:

1. Collects permitted read-only `call_data_operation` activity through the shared
   MCP/API/CLI/chat boundary. Capturing a call buys no model request and cannot
   recursively capture background automation calls.
2. Debounces each job until activity settles (120 seconds in the example), with a
   maximum wait (15 minutes). A started batch is frozen; new arrivals form its
   successor. Calls with identical query shapes consolidate across literal values.
3. Combines the batch with retained observations of the same stated task. Previously
   learned patterns with unchanged schemas
   and normal successful execution are skipped. Errors, incomplete calls, schema
   changes, or materially slower execution make them eligible again.
4. Uses one parallel judgment Choice request to route up to eight patterns, prioritizing
   fresh activity, to existing profiles or new destinations. Variants with the same
   stated intent, connection and operation share a question; different intents can
   join only through a confident semantic match. At least `minExamples` successful,
   complete calls must support the selected profile, across its separate patterns.
   A daily breakdown and a period total need not repeat identical SQL to qualify.
   A judgment does not decide query correctness. Low-confidence choices defer automatically.
5. Makes one model synthesis call per selected profile with the captured activity,
   exact operation argument schemas and existing profile. Selected examples appear
   once; supporting calls and failures supply additional context. The model proposes
   Markdown guidance and argument templates without another tool-driven survey.
   Unsupported definitions remain unknown. Model concurrency, context and configured
   token budgets determine request capacity, without a separate prompt byte cutoff;
   verification runs in the server.
6. Replays up to four successful observations per pattern. Unchanged arguments need
   one fresh complete call. Rewritten arguments require original–candidate–original
   calls with identical complete output hashes and no latency regression beyond
   25% plus 50 ms. Moving, truncated, incompatible or differing results defer the
   candidate. Models cannot invent their own expected answers. Existing configured
   recipe checks must also pass; learning cannot weaken those contracts.
7. Saves passing profiles as ordinary `recipe`-tagged Markdown with Git history.
   Existing frontmatter and previously validated templates are retained. A concurrent
   edit wins; the candidate stays in private queue state until expiry. Publication
   uses the existing journal, including recovery after a commit interrupted by a
   restart. No human approval step is required.

Administration and `automation_status` distinguish published, deferred, unchanged
and failed learning runs. Summaries identify created/improved paths and explain
deferrals without exposing captured SQL or activity IDs. Preview output remains
available through `automation_run`. Interactive assistants look up relevant
recipes before new schema discovery and still execute live queries for current values.

MCP does **not** expose the client's full conversation. Clients may provide this
optional context on `call_data_operation`:

```json
"learningContext": {
  "conversation": "task-123",
  "requestId": "call-1",
  "intent": "Compare available inventory across markets"
}
```

Use a stable request ID when retrying a call. Without an explicit conversation ID,
correlation uses an identity-scoped activity window (and transport/chat ID when
available), not a claimed chat transcript. Intent is untrusted task context, never
permission to execute instructions or broaden access.

Inputs are stored only in private queue files, with no result rows or full client
transcripts. `retentionHours` is 1–168 hours (24 in the example); expired records are
pruned even when idle. Each batch retains at most 128 observations, each at most
12 KiB of arguments. Deferred patterns wait for more usage rather than entering a
human review queue. Queue capacity remains bounded; admission failures appear as
`learning.capture` activity failures without changing the original data response.
Status exposes counts, saved paths, deadlines and failures, not private inputs.
Learning runs start from usage rather than the manual Run button.

The validation establishes observed result equivalence, not universal business
correctness or the truth of every sentence. Failed-only patterns have no successful
reference and are deferred. SQL fingerprints are routing hints, not a SQL parser;
template substitutions use whole JSON Pointer values, with optional SQL literal
bindings. Profiles remain instructions for an agent to inspect and call through the
data tools, not executable notebook code. Operator-authored recipe checks remain
available for stricter known contracts.

## Employee MCP authentication

Use `PUBLIC_URL/mcp` with ForgetMeNote OAuth and each employee's own identity.
Browser sign-in can use the existing generic OIDC or trusted Authelia setup;
MCP clients then complete ForgetMeNote's consent and token flow. Connection
operations, notebook permissions and granted client scopes apply on every call.
Organization notebooks can be read-only while a person's own notebooks remain
editable according to their grants.

The former gateway adapter is removed. Remove `MCP_ACCESS_CONFIG_FILE`
from the environment; a nonempty value now stops startup with a migration error.
Its `publicURL`, `authorizationServer`, assertion issuer/audience/JWKS, trusted
proxy list and email-domain configuration are no longer application settings.
Do not remove independently configured browser OIDC or Authelia settings.
`Cf-Access-Jwt-Assertion` is not MCP authentication. `/mcp/full` and its metadata
are removed; `/` serves the ordinary website and no longer accepts MCP requests.
Standard discovery is `PUBLIC_URL/.well-known/oauth-protected-resource/mcp`.
Clients must reconnect and authorize at the standard endpoint; old gateway tokens
and scope names are not translated.

Accounts and historical issuer/subject identity links remain intact. Matching
email addresses never merge accounts. If the chosen OIDC provider has a different
issuer or subject, explicitly link it to the existing account through the supported
identity workflow. This repository change does not alter production URLs or users.

## Chat clients and Slack

External chat uses the same notebook retrieval, data operations, model configuration,
source checks and profile-learning jobs as the rest of ForgetMeNote. Slack is the
first transport. There are no `!` commands or command-specific workflows.

Mount `SLACK_CONFIG_FILE` with the workspace ID and private runtime secret paths:

```json
{
  "team": "T_REPLACE",
  "signingSecretFile": "/run/secrets/slack-signing",
  "botTokenFile": "/run/secrets/slack-bot-token",
  "users": {},
  "channels": {}
}
```

Restart once to load the transport, then open **Administration → Chat clients**.
Link Slack user IDs to active ForgetMeNote accounts, and add channel or DM
destinations. Each destination selects a primary notebook, additional notebooks
(including repository notebooks and profiles), allowed data connections (those
that allow AI processing), allowed members, and optional thread context and
files. Slack channel and user IDs are typed: the app has no listing of them.
Omit `aiConnection` and `model` to use the instance's configured default model.
`slack.example.json` also demonstrates a complete initial file-managed policy.
Replace its placeholders and explicitly review the shared audience before using it.

To admit any active human member of the configured workspace, add `members` in the
client JSON (or mounted Slack configuration):

```json
"members": {
  "actor": "service-REPLACE",
  "binding": {
    "project": "company-knowledge",
    "readProjects": ["analytics-profiles"],
    "connections": ["warehouse"],
    "externalProcessing": true,
    "threadHistory": true,
    "attachments": true
  }
}
```

Create that service account in Administration, give it Read on the selected
notebooks and only the data operations members may use. Granting it a write
operation lets members propose that change (see below). It gets no instance
Admin authority. Slack verifies the requesting user is active, human and belongs
to the configured team before model work and before delivery. Foreign Slack
Connect users, deactivated users and workspace guests (multi- and single-channel)
are rejected, and their messages are left out of shared thread context. To admit a
particular guest, give them an account binding: map their Slack ID in `users` and
bind their DMs or a channel. Each user keeps separate private
history. Default channel answers are ephemeral to the requester. To make replies
visible to everyone in any channel where the bot is invoked, set `shared: true`,
`audienceApproved: true`, and `actors: ["service-REPLACE"]` on the member binding,
using the same service account as `members.actor`. This explicitly approves channel
audiences for that service account's configured company sources. Channel threads
then share history across requesting workspace members; previously private history
is not reused. DMs remain private and isolated by member. Existing account bindings
take precedence.
Invite the bot to channels where members want to mention it. Membership access
does not read or index the entire workspace.

To give members' DMs their own memory without each person sending `link`, add
`"pairByEmail": ["example.com"]` under `members` and run the `email-pairing`
instance automation as `members.actor`; see
[pairing by email](../../docs/personal-assistant.md#pairing-by-email).

Notebooks stay read-only from chat, and a chat conversation never runs a data
write directly. When the model calls a write operation that the conversation's
actor may use, the reply ends with the exact operation, its arguments and a
one-time code. It runs once, as that actor with its current grants, only when the
same chat user replies `confirm CODE` in that conversation within 30 minutes
(mentioning the bot in a channel) or presses its **Confirm** button; `cancel CODE`
or **Cancel** drops it. Slack shows the buttons only to the requester, privately
outside a DM, and replaces them once pressed. A press is queued as that person's
reply and rechecked the same way, and pressing again is the same request. A code
from another member, an expired or cancelled code, a changed connection
configuration or a revoked grant runs nothing. A write interrupted after
confirmation is reported as unknown and never repeated. To let
the bot triage GitHub issues, configure the `github` connection with
`externalProcessing: true` (without it, chat never sees the connection), add it
to the binding's `connections`, and grant the actor only the issue operations it
may propose.

A mention or DM from someone with no binding, or in a conversation that is not
configured, gets a short notice that the bot is not set up there (privately, in
channels) instead of no answer.

The first Administration save adopts catalog-managed chat policy. Subsequent file
policy changes cannot override it or revive a removed/disabled client. Credentials
and workspace identity remain operator-managed runtime configuration. JSON editing
is available alongside the form; the same admin-only `chat_clients` and
`set_chat_clients` actions work through API/CLI/MCP, with a revision check on saves.
The form offers the Slack app's channels and people by name from the admin-only
`chat_client_directory` action: unarchived public channels, private channels the
app is in, and active members who are not guests, by ID and name only. Without a
listing scope, or when Slack refuses or the list is too long, the form takes typed
IDs for that list and says why.
Adoption uses catalog format 7: rollback to an older binary requires a compatible
instance backup. No configuration or token contents are written to notebooks.

Configure the Slack app's Events API URL as
`https://YOUR_FORGETMENOTE_HOST/integrations/slack/events`. For Confirm and Cancel
buttons on proposed changes, turn on **Interactivity & Shortcuts** and set its
Request URL to `https://YOUR_FORGETMENOTE_HOST/integrations/slack/interactions`;
without it, the buttons do nothing and people confirm by replying. Administration
shows both URLs. Invite the bot only to the configured channels. Enable the scopes
and subscriptions for the features used:

| Feature | Bot scopes | Event subscriptions |
| --- | --- | --- |
| Mentions and replies | `app_mentions:read`, `chat:write` | `app_mention` |
| DMs | `im:history` | `message.im` |
| Group DMs, after a member adds the bot | `mpim:history` | None additional; members mention the bot |
| Public-channel context and knowledge capture | `channels:history` | `message.channels` for knowledge capture |
| Private-channel context and knowledge capture | `groups:history` | `message.groups` for knowledge capture |
| Read attached files | `files:read` | Files arrive on the message events above |
| Send exports/charts | `files:write` | None additional |
| Verify workspace membership | `users:read` | None additional |
| Pair members with accounts by email | `users:read`, `users:read.email` | `team_join`, `user_change` to pair changes right away |
| Give a channel, user group or everyone a [task](../../docs/tasks.md) | `users:read`, `channels:read`, `groups:read`, `usergroups:read` | None additional |
| Choose channels and people by name in Administration | `channels:read`, `groups:read`, `users:read` | None additional |
| Working indicator on DMs/private replies | `reactions:write` | None additional |
| Native working status inside threads | `assistant:write` | None additional |

Slack does not let an app join a one-to-one DM between two people; use a group
DM or a channel instead. Check the app installation's actual grants and reinstall
it when Slack requires that after a scope change. See Slack's [Events API](https://docs.slack.dev/apis/events-api/),
[thread context](https://docs.slack.dev/reference/methods/conversations.replies/),
and [file-upload API](https://docs.slack.dev/reference/methods/files.getUploadURLExternal/).
API permission and rate-limit failures are reported; they are not silently retried.

**Conversation behavior.** Every channel request requires an explicit mention,
including follow-ups in an existing thread. Human-to-human replies and file shares
without a mention do not trigger answers. DMs share a private conversation unless
the user starts an explicit thread. Duplicate deliveries and overlapping Slack
mention/message events produce one queued request. Signatures and the five-minute
timestamp window are checked before accepting events and button presses.

An explicit `@bot stop`, `@bot stop responding to this thread`, or a direct-message
stop request cancels queued and active work in that conversation without another
model call or reply. Retained cancelled controls reject delayed older events across
restart; a newer explicit invocation resumes normally. Cancellation cannot retract
a message Slack has already accepted. Unrelated threads and other users' private
conversations are unaffected.

Channels default to private ephemeral replies for the requester. Shared replies
require `shared`, `audienceApproved`, and explicit allowed ForgetMeNote `actors`.
The operator approves everyone who can see the channel as an audience for its
configured sources; this does **not** synchronize or verify Slack channel membership.
Every allowed actor must retain notebook access, and shared data calls require
every actor's operation grant. Private Inboxes cannot be bound to chat clients.
Configuration changes invalidate queued requests, and each provider/tool call and
delivery rechecks access. Shared threads are stored separately from private chats.

Conversation history is retained within the configured chat storage budget and fitted to the model context per request. Optional Slack thread context follows pagination and the state budget; only authorized participants contribute evidence. Workspace-wide shared bindings verify each human author's team membership (guests excluded) rather than requiring a separate account mapping for every author. Files must be
shared with the destination and downloaded from Slack's file service without
redirects. Existing native source extraction is reused under `FMN_LIMIT_SOURCE_BYTES` and `FMN_LIMIT_SOURCE_TEXT_BYTES`. They remain conversation attachments;
reading one does not import or edit a notebook.

**Data analysis.** Read-only configured data operations return receipts. The model
can request CSV, XLSX, JSON, or bar/line SVG attachments from a retained complete
receipt; it cannot supply fabricated rows to the renderer. Follow-ups can export
the same result without querying again. The result cache uses half of `FMN_LIMIT_STATE_BYTES`; generated replies use `FMN_LIMIT_ARTIFACT_BYTES`. There are no independent result/file-count quotas. Source queries still honor the connection’s configured rows/bytes/time budgets. SVG is a downloadable chart, not a guaranteed Slack
inline preview. Formula-like spreadsheet strings are escaped; charts preserve
source values but do not certify the model's business interpretation. File replies
require a DM or approved shared channel. Notebook/repository links accompany answers.

Interactive data calls contribute to existing opt-in profile-learning jobs when
the caller can read the profiles notebook. The human request and bounded query
arguments are captured under the existing sensitive-input filters, consent and
retention policy. Background automation calls remain excluded to avoid learning
from the system's own work. No new judgment calls or provider framework are introduced.

Chat generation and delivery checkpoints suppress automatic repeats after an
unknown outcome. Check Administration → Automation for operational failures;
private prompts, source content and exports are omitted from that diagnostics API.
All automation checkpoints use `FMN_LIMIT_STATE_BYTES`, including encoded attachments. Known request failures send a private error with the run ID when access remains valid; ambiguous sends are never automatically repeated. Working indicators clear on completion or failure. Scheduled `notifySlackUser` and approved broadcasts
remain supported, using the current adopted policy and recipient scope.

**Adding another client.** Implement `internal/messaging.Transport`'s `Context` and
`Send` methods, authenticate its inbound events, and pass normalized `messaging.Event`
values to the shared `EnqueueMessage` path in `internal/messaginghttp`. Register the
transport at startup. Client adapters own signatures, network APIs and platform
formatting; `internal/messaginghttp/messaging.go` owns identities, permissions, scoped
conversations and execution. A second fake transport exercises shared conversations, retrieval,
restart and revocation in tests. Teams/Discord/etc. adapters are not included yet.
Tests use local synthetic Slack endpoints; no live messages or app changes occur.

## Deployment acceptance

See [production pilot and release](../production/README.md) for the manual ECR
workflow, single-writer Kubernetes template, read-only acceptance runner, migration
inventory and cutover gates.

The code can be deployed independently of the existing services. Initially use
separate DNS, a private image, a fresh PVC, runtime Secret mounts, an independently
configured GitHub Sources App and least-privilege external identities. Import
repositories through the existing repository-notebook workflow; it remains
read-only and automatically indexed. Migrate recipe Markdown only through a
separately authorized import. Take a full instance backup before accepting the
catalog's version-5 connection-grant format; an older binary cannot read it.

Local tests cover permissions, signature verification, real MCP transport, SQL
boundaries, secret rotation, recipe repair/rejection, publication conflicts and
queue recovery. Production canaries must cover your actual services, database
roles, profile fixtures, standard OAuth and Slack app. Keep existing services
unchanged until a separately authorized cutover.

Per-employee Google/Slack grants use [personal assistants](../../docs/personal-assistant.md), not the shared service credentials above. See [the runtime example](personal-connections.example.json).
