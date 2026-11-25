# Repository automation packages

Complicated jobs can use `kind: "script"` in the existing automation engine.
Simple checks, monitors, digests, imports, maintenance and learning jobs keep
their existing implementations. There is one job definition, configuration
owner, service account, scheduler, queue and run history. Importing a repository
does **not** install or execute its contents.

## Develop and pin a package

A package directory contains an `automation.json` manifest and ordinary Python
source. Its entrypoint exports `main(host)` and returns a JSON-compatible result.

```json
{
  "apiVersion": "forgetmenote.automation/v1",
  "id": "document-summary",
  "name": "Document summary",
  "entrypoint": "summary.py",
  "runtime": "python-v1",
  "files": ["summary.py"],
  "capabilities": ["notebooks.read", "notebooks.publish"],
  "notebooks": {"notes": "edit"},
  "parametersSchema": {
    "type": "object",
    "properties": {"source": {"type": "string"}, "output": {"type": "string"}},
    "required": ["source", "output"],
    "additionalProperties": false
  }
}
```

```python
def main(host):
    params = host.call('info', 'run.info')['parameters']
    note = host.call('source', 'notebooks.read', notebook='notes', path=params['source'])
    content = '# Summary\n\nWords: ' + str(len(note['content'].split())) + '\n'
    host.call('output', 'notebooks.publish', notebook='notes', path=params['output'],
              content=content, revision='')
    return {'output': params['output']}
```

An empty publication revision creates a **new** note; supply the current revision
to update one. Conflicts and notebook locks use the ordinary workspace checks.
Each package declares all runtime files, logical connection operations, notebook
roles, capabilities and optional parameter schema. Paths are relative regular
files: traversal, symlinks, submodules, excluded/credential files and Git LFS
pointers cannot become executable package content. The manifest itself is
included in the content digest. Tests need not be runtime files.

`optionalCapabilities` and `optionalConnections` declare optional requested
authority. Installation starts with required grants only; an administrator must
explicitly approve optional capabilities, bindings and operation grants. This
allows one package to run read-only without being granted send or model access.
Parameters and policy notes cannot add authority.

The common runner/client is embedded from `internal/automation/python`, uses
Python 3.12+ in the Python 3 series, and provides `host.call(step, capability, **arguments)`.
Packages do not vendor provider clients or the ForgetMeNote implementation.
Run package unit tests with synthetic host results from their own checkout.
Do not place credentials, downloaded private documents or production run data
in the package repository.

## Install, preview, activate and update

1. Configure the local Python runtime below. Import the GitHub repository through
   the normal repository-notebook workflow. Grant the service account Read on
   that notebook and the required role on each bound notebook.
2. In **Administration → Automations → Repository packages**, enter the source
   notebook ID and select **Discover**. Inspect the manifest, exact source
   commit and requested capabilities. Select **Prepare paused installation**.
3. Choose the service account, primary notebook, schedule and execution budget.
   Leave **Sandbox package code** checked (the default, see
   [package sandbox](#package-sandbox)) unless reviewed code needs server access
   or the host lacks Landlock.
   Fill the JSON definition's logical connection/notebook bindings and parameters.
   Grant matching connector operations in Administration → Connections. Source
   notebook sharing and external-processing consent still apply.
4. **Validate**, then **Save automation**. Package installation always saves a
   paused definition. **Test** performs actual reads/model calls but suppresses
   notebook publication and connector writes. Review its retained steps/results.
5. **Enable** explicitly activates this exact revision. **Run now** uses the
   normal queue and request-ID deduplication. Interval/calendar jobs use the
   existing scheduler. Scripts can use repository-update events and the existing debounce; learning usage triggers remain platform-owned.
6. Installations stay pinned to the exact commit and digest an administrator
   approved. Push access to the package repository is not review: a changed
   entrypoint or module with the same manifest and lock would otherwise run as
   server code. When verified primary-branch refreshes change the package digest,
   the job card shows **Update available** and `discover_automation_packages`
   returns the newer commit and digest; runs keep using the installed version.
   **Inspect / update package** shows available and active commits. Review the
   change, then **Prepare paused update** (`install_automation_package` with the
   definition's current revision), test it and enable it again. Mounted
   operator-managed definitions are always pinned and use the workflow below.
   Retained installed files survive ordinary Git-cache rotation; an unretained
   evicted commit is unavailable until supplied again.
7. An instance-managed installation can opt into following the primary branch with
   `"followPrimaryBranch": true` in its `script` installation (the Administration
   editor's **Adopt compatible primary-branch package changes without review**).
   Absent or `false` means pinned, including for definitions saved by earlier
   versions. With the opt-in, the scheduler checks every 30 seconds, prepares
   changed package files, and updates the exact commit, digest and environment while
   preserving grants, parameters, budgets, schedule and paused/enabled state. Active
   runs and unresolved effects finish or require reconciliation before an update;
   obsolete queued runs are cancelled with their history retained. Unrelated
   commits do not repin an unchanged package. Repository polling normally refreshes
   sources every five minutes; webhooks can request an earlier refresh. Changed
   capabilities, connection operations, notebook roles, parameter schema,
   entrypoint, runtime or dependency lock still require an explicit administrator
   update. Rewritten history and revoked/excluded sources cannot activate an
   automatic update. Failures preserve the installed version and are reported in
   server logs. A rollback to an older commit with the opt-in follows compatible
   updates again; clear the option for a sustained rollback. Enable it only when
   everyone who can push to the package's default branch is trusted to deploy
   server code.

Only instance administrators can discover/install/activate packages. Notebook
Owner/Edit does not grant execution authority. The same management actions are
available through the existing browser, API, CLI and MCP authorization boundary:

```sh
forgetmenote automation discover automation-packages
forgetmenote automation discover automation-packages --path automations/notebook-summary --revision EXACT_COMMIT
forgetmenote automation install --file installation.json
forgetmenote automation install --file update.json --revision CURRENT_DEFINITION_REVISION
```

Management uses administrator browser or authorized personal OAuth credentials,
not a service/API token widened into an organization administrator. API/MCP
actions are `discover_automation_packages`, `install_automation_package`,
`prepare_automation_package`, `validate_automation`, `save_automation`, `test_automation`, `run_automation`,
`automation_status`, `automation_run` and `cancel_automation`. Installation/update
requires the definition's current revision (empty only for a new job).

Operator-managed jobs remain in `AUTOMATIONS_CONFIG_FILE` and are read-only in
the UI. First call `forgetmenote automation prepare --file installation.json`
(`prepare_automation_package` with `spec` through API/MCP). This prepares source
and dependencies without saving a definition. Put the returned paused spec,
including `script.environment`, into the mounted file. Restart through the normal
configuration workflow, test it, then explicitly enable it in that file. Do not
also save an instance-managed job with the same ID. Preparation is not activation.

Example installation shape (replace discovery and installation values):

```json
{
  "id": "document-summary", "kind": "script", "disabled": true,
  "actor": "SERVICE_ACCOUNT_ID", "project": "notes",
  "readProjects": ["automation-packages"], "timeoutSeconds": 180,
  "externalProcessing": false,
  "script": {
    "sourceProject": "automation-packages", "sourceID": "DISCOVERED_SOURCE_ID",
    "packagePath": "automations/notebook-summary",
    "revision": "EXACT_40_HEX_COMMIT", "digest": "DISCOVERED_64_HEX_DIGEST",
    "runtime": "python-v1", "entrypoint": "summary.py",
    "capabilities": ["notebooks.read", "notebooks.publish"],
    "notebooks": {"notes": "notes"},
    "parameters": {"source": "source.md", "output": "summary.md"}
  }
}
```

Capabilities must match the selected manifest. The approved entrypoint is part
of the installation grant. Installation returns `script.environment`, the prepared
version identity; ordinary save/enable cannot bypass preparation. Existing job
concurrency, deadlines, state/source/conversion/research/model budgets apply.
CPU and memory are shared with the server and governed by its OS/container limits;
a Python child is not an independently resource-isolated container, sandboxed or not.

## Version 1 host contract

The runner owns transport. Scripts neither choose a host nor receive provider,
connector or application credentials. Requests contain `step`, `operation` and
`arguments`; a success returns `{"result": ...}`. Host errors stop the call.
There are no automatic client retries. Results and arguments must be JSON data.
The runner reserves the step ID `complete` for the final result.

| Operation / capability | Arguments and result |
| --- | --- |
| `run.info` (implicit) | Run ID, `createdAt` (stable), `now` (live), `deadlineAt`, dry-run flag, parameters, installed capabilities, package `revision`/`packageRevision` and `packageDigest`, opaque `analysisIdentity` for provider configuration, optional `analysisProvider` for an explicitly selected Cloudflare moderation connection, runtime, and a `sources` map keyed by bound notebook alias. Source entries contain project, source ID, captured revision, optional previous observed revision and policy fingerprint. Package and evidence revisions are distinct. |
| `run.id` (implicit) | A stable step produces a stable UUID-shaped submission identifier for this run. |
| `checkpoint.get`, `checkpoint.set` / `checkpoint` | Get state; set with `state`. Set steps need stable IDs. |
| `jobstate.get`, `jobstate.set` / `jobstate` | Private cross-run `{revision,data,updated}` for this job, actor and project. Set takes the previous `revision` and a bounded JSON-object `data`; conflicts fail. Exact replay after a saved write is idempotent. Preview uses an isolated overlay. Run pruning never deletes this state. |
| `notebooks.read` | `notebook` alias, `path`, optional `revision`, `origin` (defaults to `notes`), `startLine`/`endLine`. Returns original content, hash, revision and exact citation. Repository reads bind to the captured revision. `optional: true` returns `{missing:true, revision:"", hash:""}` for an absent ordinary note; access failures still fail. |
| `notebooks.map` | Bound repository `notebook`, optional `query`, `pathPrefix`, `limit`, `after`; uses the existing map at the captured revision. Entries are navigation hints, not citations. |
| `repositories.changes` | Bound repository `notebook`; returns `from`, `to`, admitted changed `paths`, `removed` paths, and `baselineRequired`. Comparison uses the captured previous observed revision; absent/rewritten history is a baseline, not a fabricated diff. |
| `incidents.report` | One finding per run: boolean `active`, `severity` (`warning` or `critical`), nonempty `summary`, optional `key`. Reuses incident thresholds, repeat suppression, recovery and delivery. Preview never updates incident state or sends a message. Do not report a healthy finding for missing evidence. |
| `notebooks.publish` | `notebook`, `path`, `content`, `revision`, optional `hash`; existing atomic workspace edit-group result. Writes are limited to the primary notebook. |
| `connectors.read` | Logical `connection`, declared `operation`, `arguments`; returns `data`, execution `receipt`, and original `receivedAt`. Only configured read operations; incomplete results fail. |
| `connectors.submit` | Same binding/arguments plus the guard and acknowledgment described below. Only configured write operations. |
| `messages.post` | `text`, optional `thread`, and optional `image` (`url`, `alt`), `images` (up to 4, shown in order) or `cards` (a carousel of up to 10); posts one Slack message to the job's `notifyChannel` and returns `{channel, ts}`. Pass a returned `ts` as `thread` to reply under that message. See [Slack posts](#slack-posts). |
| `ai.structured` | `prompt`, JSON `schema`, optional inline `images`; returns one schema-validated object using existing instance/provider settings, model budgets and receipts. Images additionally require `ai.images`. Optional `research: {maxSearches: 1}` combines image extraction with required OpenAI web search; see below. |
| `ai.decisions` | `prompt`, 1–64 named questions, optional inline `images` and `cacheKey`. Responses connections accept `choice` with string descriptions. Explicit Cloudflare Clef connections also accept structured choice descriptions, `noul` without choices, and `score` with ordered `criteria`; return types preserve native probabilities, confidence, refusals and usage. No model tools or generated prose. Images additionally require `ai.images`. Same consent, authority, capacity, token limits and paid-step recovery as structured generation. |
| `decision.evaluate` | Existing decision request: `state` plus named `questions` with typed judgments and criteria. Returns named `answers` through the existing decision service, concurrency and usage ledger. |
| `web.fetch` | Public `url`, optional `image: true`; returns extracted `text`, `title`, provenance `record`, or inline `image` and final `url`. Uses existing source restrictions. No JavaScript/browser execution. |
| `web.search` | `query`, `limit`, optional `diagnostics`; configured research provider and search budgets. With Cloudflare, `diagnostics: true` returns `{results,diagnostics}` with HTTP status, sanitized request ID and returned/accepted URL counts; the default remains the link list. An explicitly selected Cloudflare moderation connection uses Ceramic and returns URLs only, without retaining raw search output, snippets, titles or rankings. OpenAI research uses one bounded Responses web-search call on an operator-selected connection, records usage and returns source candidates for separate fetching. Paid or interrupted searches are never automatically retried. No implicit provider or key. |
| `documents.extract` | `name`, base64 `data`; returns extracted `text`, `title`, `record` directly through the existing extraction/OCR owner. No temporary notebook notes. |
| `run.complete` (runner-owned) | Persists the return `result` only when no external outcome is unresolved. |

Clef Noul answers carry `noul` (0–1) without a confidence field. Score answers carry the probability-weighted level index (0 through `len(criteria)-1`), full level probabilities, the original legend and native confidence. `batches` reports actual provider requests when capacity requires splitting; sibling membership can affect scores, so retain the executed request when evaluating changes. These native types are not converted into policy verdicts by the host.

With `research`, `ai.structured` requires inline images, `web.search` and `ai.images`
grants, external-processing consent, and enabled operator research configuration
using the automation's same OpenAI Responses connection. `maxSearches` is 1 or 2
and bounds all hosted search/page tool calls; at least one completed search is
required. These calls share the run's configured search budget with `web.search`.
Started or failed research reserves its maximum. Completed research charges the
actual tool count. The returned envelope is `{value, sources, calls, searchCalls,
toolCalls, model, usage, elapsedMilliseconds}`: only `value` is validated against
the requested schema. Sources and call records come from provider tool output,
not generated JSON; source links are candidates for separately fetching originals.
Web search rejects JSON mode. The host requests `json_schema` with `strict: false`
and validates the unchanged caller schema locally, preserving supported schemas
with optional fields and local references. Plain `ai.structured`
retains its existing object response and never enables research implicitly.

Required research rejects a cross-run `cacheKey`; repeating the same completed
step within a run replays its receipt without another paid call. Interrupted paid
calls retain token reservations and suppress automatic repetition. Search context
uses the provider's `low` setting, which is not an exact token cap; reservations
include image/search estimates and bounded output, actual usage is charged, and an
over-budget result is refused. Rejected results retain their paid research metadata
in the private failed-step receipt without releasing the result to the script.
Unknown charges require reconciliation.

### Cloudflare moderation connection

Add this operator-owned connection to the existing chat configuration and bind
only the moderation script's `aiConnection` to `cloudflare-moderation`. Preserve
other jobs and the global default. `run.info.analysisProvider` reports the exact
extraction, decision and search providers only for an explicit binding.

```json
{
  "id": "cloudflare-moderation",
  "label": "Cloudflare moderation",
  "format": "cloudflare-mistral",
  "models": ["@cf/mistralai/mistral-small-3.1-24b-instruct"],
  "defaultModel": "@cf/mistralai/mistral-small-3.1-24b-instruct",
  "secretFile": "/run/secrets/providers/CLOUDFLARE_BROWSER_RUN_TOKEN",
  "capabilities": {"images": true},
  "limits": {"contextTokens": 32768, "outputTokens": 8192},
  "cloudflare": {
    "accountIDFile": "/run/secrets/providers/CLOUDFLARE_ACCOUNT_ID",
    "decisionModel": "@cf/cloudflare/clef",
    "gatewayID": "default",
    "searchProvider": "ceramic"
  }
}
```

The account and token come from private runtime files; the endpoint is derived
from the account rather than stored in notebooks or package code. Existing
operator research settings still supply run search/fetch bounds. Ceramic needs a
funded AI Gateway or configured provider key. Mistral uses native non-streaming
inference with one original inline image and `guided_json`. The complete schema
also appears in the prompt; schema guidance alone did not enforce the output in
live probes. ForgetMeNote independently validates every result. The native
Moondream format remains supported for existing bindings and rollback, without an
automatic fallback. The explicit `decisionModel` may select `@cf/cloudflare/clef`
or `@cf/cloudflare/clef-omni` for typed text/image decisions. Their preflight context
limits are 65,536 and 64,000 tokens respectively; audio and video are not exposed.
No fallback provider is selected. Search logging and gateway
caching are disabled for transient discovery.

Cloudflare inference waits within the configured model and enclosing run deadlines
rather than a separate 30-second response-header cutoff. With a current authority
callback, explicit capacity rejections (codes 8005 or 3040) permit at most three
HTTP attempts, with bounded backoff and an authorization check before each one.
Search, billing/authentication errors, malformed responses and ambiguous network
failures are not automatically repeated. Diagnostics expose numeric HTTP/provider
codes, not upstream messages or private authority failure details.

For native Cloudflare structured extraction, a completed answer that fails the caller's
JSON schema permits one additional inference over the same original image. Both
inferences count toward usage and budgets. The second answer must pass the entire
schema; the host never fills missing values or coerces invalid types into facts.
One exact outer Markdown `json` fence is accepted around the complete object;
surrounding prose and additional fenced blocks still fail validation.

Clef permits four images, 4 MiB and 16 megapixels each, 8 MiB decoded total and a
13 MiB request. Its context is 65,536 tokens and it silently truncates long text
state. Without an exact tokenizer, the host conservatively uses a byte-per-token
upper bound and batches questions when needed, preserving the full state and
original images in every batch. All batches are preflighted before inference;
source and authority checks run before and after each paid request. Actual usage
is summed and no partial answer set is released on failure. This safeguard can
require extra calls compared with native token counting. A state or single
question that cannot fit safely stops without buying inference.

Every call rechecks current account/project authority, source installation and
exclusions, job and manifest grants, connector configuration and applicable AI
policies. Publication checks authority again at the actual write boundary.
Source/notebook policy is not bypassed by copying content into a checkpoint.
Local-only providers and processing consent remain authoritative. Host calls
cannot select an arbitrary filesystem root, external credential or connector URL.

For generic HTTP operations, `connectors.submit` requires:

```json
{
  "connection": "logical-api", "operation": "submit", "arguments": {"body": {}},
  "guard": {
    "read": {"connection": "logical-api", "operation": "read", "arguments": {}},
    "equals": {"/revision": "ORIGINAL_REVISION"},
    "expiresAt": "ORIGINAL_RFC3339_EXPIRY"
  },
  "acknowledgment": {"/id": "EXPECTED_ID", "/submission_id": "STABLE_ID"}
}
```

The fresh read is performed immediately before submission; JSON pointers must
match, the original expiry must still hold, and current permissions/configuration
are rechecked. The complete write response must satisfy acknowledgment assertions.
Dry runs perform the read/guard but send no write. The host stays generic: field
names, business rules and endpoint mappings belong to package/operator files.

The reusable [Gmail/Sheets adapters](google-integrations.md) instead own their
operation-specific preflight and actual provider acknowledgment validation. Supply
their operation arguments, including a stable cross-run `actionID`; do not fabricate
an expected provider-generated ID. These writes use the same durable submission
owner through Python, API and MCP. Preview suppresses provider writes.

`ai.structured`, `ai.decisions` and `decision.evaluate` accept an optional `cacheKey` for reuse
across runs. Key it by source evidence and policy/schema, not a run ID. The host
also binds actual arguments, job/actor/project and provider/model configuration,
and rechecks live permissions and consent before reuse. A failed or interrupted
call keeps an unknown-outcome barrier; it is not silently purchased again.
Preview does not populate or reuse the live cross-run cache.

### Slack posts

`messages.post` lets a package post several messages per run and thread replies
under them. It posts only to the job's `notifyChannel`, which must have an approved
Slack `broadcasts` entry for the job's notebook and service account. A package
cannot choose another channel. Before every post the host checks that the
broadcast's `connections` cover every connection the job may read and every
connection it has read in this run. Add each data connection the package reads,
such as a database or code-execution connection, to that list. A job definition
that grants `messages.post` without a `notifyChannel` is rejected.

```python
parent = host.call('post-' + ad_id, 'messages.post', text='*New ad* ' + name,
                   image={'url': image_url, 'alt': name})
host.call('inputs-' + ad_id, 'messages.post', text=inputs, thread=parent['ts'])
```

Text is limited to 3,500 characters and is never split, so each call returns
exactly one message ID. Slack mention syntax such as `<!channel>` is escaped. Each
image is a Slack image block: Slack fetches the HTTPS URL when the message posts,
so the URL must be publicly readable then. To show several pictures in one
message, pass `images=[{'url': ..., 'alt': ...}, ...]` (at most 4) instead of
`image`. The host adds no text, footer or name of its own: Slack shows the post
under the bot's app name. Keep earlier `ts` values in `jobstate` to
reply in the same thread from a later run.

To show many pictures compactly, pass `cards` instead: one carousel of at most
10 cards that scrolls sideways under the text. Each card has a plain-text
`title` and `subtitle` (at most 150 characters each), a `body` (at most 200)
and an optional `image`, and needs a title or an image. Slack shows a card's
image in a fixed 4:3 frame and crops other shapes, so pad pictures to 4:3
first. A card image is either `{'url': ..., 'alt': ...}`, fetched from a public
HTTPS URL like an image block, or `{'data': 'data:image/jpeg;base64,...',
'alt': ...}`: a PNG or JPEG of at most 2 MB that the host uploads to Slack
privately (shared with no channel) and shows by its Slack file ID, so the
picture needs no public address. A failed upload posts nothing.

```python
host.call('resizes-' + ad_id, 'messages.post', text='*Resizes* · ' + name,
          cards=[{'title': '1200×300', 'subtitle': 'passed',
                  'image': {'data': padded_jpeg_uri, 'alt': name + ', 1200×300'}}])
```

Each post is an external write. A Slack refusal (`ok:false`, including an image
Slack cannot load, or a rate limit) is a settled failure: nothing was posted,
the host error names Slack's code and later runs are not blocked. A network
failure, an HTTP error from infrastructure in front of Slack or an unreadable
response leaves the outcome unknown, and the job needs reconciliation as for
other writes. Posts are never retried automatically, and a job with
`messages.post` cannot use `retry`. A preview checks the destination and returns
`{dryRun: true, posted: false}` without posting.

## Recovery and uncertain outcomes

Each run pins source commit, all-file digest, prepared environment and connector/job
contract. A step records exact inputs and `started` **before** executing; a
completed step stores its result. The same step and inputs reuse that result
after reauthorization. Reusing an ID with different input is rejected. Code or
connector changes cannot silently reuse a checkpoint.

Read-only interrupted steps can be read again. An interrupted/failed model,
decision, notebook publication or external write is conservatively retained as
an unresolved effect; another attempt is not purchased/sent automatically. The
job enters `action-required` and new manual/scheduled runs are blocked. This also
covers missing acknowledgments and failed persistence after a call. A rejected
write guard may conservatively need reconciliation even when no request was sent.

Inspect `automation_run` (exact inputs, results, receipts and any error), confirm
the upstream outcome, then use **Reconciled · cancel run**. This permits a new run;
it does not erase evidence, reverse side effects or replay the old submission.
Cancellation during an uncertain call leaves that reconciliation requirement.
Catching a host error and choosing a different step ID cannot start another paid
call or write while an earlier outcome remains unresolved.
Changing definitions, pausing or rolling back source cannot dismiss it.

A definitive Cloudflare inference rejection, or a completed Cloudflare extraction answer that
still fails JSON validation after the one correction, is a settled failed attempt.
Its step retains a completed error: replay returns that error without purchasing
another inference, and the failed run does not need unknown-outcome reconciliation.
Cancellation while waiting after a definitive rejection preserves that rejected
outcome; cancellation with inference in flight still retains uncertainty. Optional
cross-run analysis caching records settled failures as rejected, so they do not
block unrelated cache identities; the same attempted identity is not reused as a
successful result or silently purchased again.

Google submissions and cached analysis additionally retain cross-run effect
records. Inspect/reconcile these with the actions described in
[Google integration recovery](google-integrations.md), then reconcile the run.
Changing a run or step ID does not clear a connection's unresolved outcome.

Set `script.retentionHours` (1–168) for sensitive workflows. Inactive expired
journals lose raw arguments/results, checkpoint state, outputs and errors; hashes,
operation states and unresolved barriers remain. An expired journal cannot resume.
Paid analysis cache results use the same retention (24 hours when unspecified).
Durable job state, selected notebook publications, Git history and backups have
separate lifetimes. Zero preserves the existing run-history policy for older jobs;
it is not an appropriate sensitive-workflow default. See the privacy and recovery
details in the Google integration guide.

## Execution and trust

The existing Go scheduler starts the configured Python interpreter directly with
an explicit argument vector. It never builds a shell command. Structured JSON
requests and replies use stdout/stdin; ordinary `print` output goes to diagnostic
stderr. There is no executor service, per-run Docker container, Kubernetes Job,
callback HTTP endpoint, Docker socket, or automation image registry.

The normal application image includes Python and sets:

```sh
AUTOMATION_PYTHON=/usr/local/bin/python3
```

A native Linux/macOS installation sets `AUTOMATION_PYTHON` to an installed
Python 3.12+ executable (with `venv`/pip available if a package needs dependencies). Without this setting, existing native jobs still work,
but Python package preparation/execution is unavailable. Changing the interpreter
requires restarting the server; each process then prepares installed packages for
the new interpreter on first use (see [dependency preparation](#dependency-preparation)). There is no
fallback to another execution backend.

Go supervises the child with the job's context and deadline. Cancellation kills
its Unix process group, including ordinary descendants. A parent-liveness pipe
also terminates that group when the server exits abruptly; exiting the entrypoint
cleans up descendants; container PID 1 also reaps adopted children. Packages must not daemonize or detach into new process
groups. Recovery uses the existing persisted queue/checkpoints, never an orphan
child process. Startup removes abandoned run scratch while retaining prepared versions. There is no Windows process-tree implementation.

**Packages are trusted administrator-installed server code.** Notebook membership,
source pulls and agent edits alone do not authorize an initial installation,
expanded grants or new package code. An installation runs only the digest an
administrator approved, unless that instance-managed installation explicitly set
`followPrimaryBranch`; it then follows compatible primary-branch updates within its
existing installation grant.
The child receives a minimal environment, private disposable working directory,
and no application secrets or provider keys in its environment. Host calls enforce
service-account grants, current notebook permissions and processing policy.

Those checks protect the host API, not arbitrary Python. An installation that opts
out of the [package sandbox](#package-sandbox) runs as the server's OS identity and
can access files, networking and other resources available to that identity,
including secrets stored on accessible filesystems. A subprocess, read-only package
files and a venv are not a sandbox. Sandboxed or not, install only reviewed code
from trusted administrators. Do not expose package installation to notebook editors.

### Package sandbox

Installations are sandboxed by default. A `script` without `sandbox`, including
every definition saved by an earlier version, runs sandboxed. The only opt-out is
an explicit `"sandbox": false`: clear **Sandbox package code** in the Administration
editor, and the installation's card then reads **Not sandboxed**. That runs the
package as described above. The setting is part of the installation grant:
**Prepare paused update** and automatic primary-branch updates keep it, and only an
explicit definition change alters it.

A sandboxed run re-executes through the same `forgetmenote sandbox-exec` helper as
the [local extractors](sources.md#local-extractor-sandbox), in strict mode. Python
keeps the run's PID, process group, environment, pipes and parent-liveness
descriptor, so cancellation, deadlines and cleanup after a server crash are
unchanged. Subprocesses inherit the sandbox.

- **Read-only:** the prepared version (package files, runner and any venv), the
  interpreter's installation (`sys.base_prefix`) and the extractor system paths:
  `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/etc`, `/var/cache/fontconfig`,
  `/dev/urandom`, CPU topology (`/sys/devices/system/cpu`, `/proc/cpuinfo`) and the
  forgetmenote binary's directory. Tools such as `tesseract` under `/usr/bin` work.
- **Writable:** only the run's private scratch directory (its working directory,
  `HOME` and `TMPDIR`, removed after the run) and `/dev/null`. The prepared version
  stays unwritable even if the package changes its permissions.
- **Unreachable:** the rest of `DATA_DIR` (notebooks, catalog, OAuth and session
  state, other packages and other runs' scratch), mounted secrets such as
  `/run/secrets`, home directories, `/tmp` and the rest of `/proc` and `/sys`. If any
  grant would contain the prepared-package directory, for example a forgetmenote
  binary or Python installation in a parent of `DATA_DIR`, the run is refused.
- **No network:** a seccomp filter denies `socket(2)`, datagram `socketpair` and
  `io_uring_setup`, so TCP, UDP, Unix and netlink sockets are unavailable whatever
  the Landlock ABI. The host contract needs only stdin/stdout: notebooks,
  connectors, web fetch/search and AI calls go through `host.call`. Stream socket
  pairs, such as asyncio's event-loop wakeup, still work.
- **No truncation by path:** the filter also denies `truncate(2)`, which Landlock
  ABI 1 and 2 (before Linux 6.2) do not handle; `ftruncate` on scratch files works.
- **Processes:** at most 4096 tasks for the server's user ID, as a fork-bomb guard.
  The job deadline and container limits still bound CPU and memory.

The sandbox fails closed. It needs Linux with Landlock (5.13 or newer) allowed by
the container's seccomp profile, the same requirement as the
[extractor sandbox](sources.md#local-extractor-sandbox) but without its fallback.
On macOS, on a kernel or profile without Landlock, and in an amd64 image emulated
on Apple silicon (whose emulator returns `ENOSYS` for Landlock), every installation
that has not opted out fails before package code starts with
`package sandbox refused the run: Landlock unavailable ...`, followed by the
opt-out. The helper
refuses the same way if it cannot install its seccomp filter. No run falls back to
running unconfined. On such hosts, set `"sandbox": false` only for code you would
run as the server. **Test** an installation on its target host before enabling it.

Landlock does not mediate metadata. A sandboxed package can still `stat` a path it
can name, and change the mode or timestamps of files the server's user owns,
without reading or writing their contents, so keep secrets out of the granted
system directories (container deployments mount them under `/run/secrets`).
Before Linux 6.12 (Landlock ABI 6) it can also signal processes outside the
sandbox, including the server. Packages that need direct network access,
`multiprocessing` locks (which use `/dev/shm`), `/proc`, devices or files outside
their package do not work sandboxed; use the host operations instead. ONNX Runtime
also logs that GPU discovery cannot read `/sys/class/drm`; CPU inference is
unaffected.

#### Upgrading to the sandbox default

Installations saved before the default changed become sandboxed when the server
upgrades. Package code that stays within its grants, and does all its I/O through
`host.call`, runs unchanged. Code that reaches outside them gets ordinary Python
errors, which fail the run unless the package handles them:

- Reading, listing or writing a path outside the grants, including writing into
  its own package or `/tmp`: `PermissionError: [Errno 13] Permission denied`.
  `tempfile`, `HOME` and the working directory resolve to the scratch directory
  and keep working.
- Creating a socket, directly or through `urllib`, `requests`, `http.client` or a
  `multiprocessing` manager: `PermissionError: [Errno 13] Permission denied`.
  Resolving a hostname, the first step of most HTTP requests, fails earlier with
  the misleading `socket.gaierror: [Errno -3] Temporary failure in name resolution`,
  because the resolver cannot open its socket. HTTP libraries wrap either error,
  for example as `urllib.error.URLError`.
- `multiprocessing` locks, queues and pools: `PermissionError` from `/dev/shm`.

Completed host steps keep their usual checkpoint and retry semantics. The failed
run's error ends with the package's stderr, as for any other failure. To restore
the earlier behavior for one reviewed package, set `"sandbox": false` and save its
definition.

## Dependency preparation

Prefer the standard library and the supplied small host client. Reuse host OCR,
AI, decision, research, connectors and notebook operations. Standard-library
packages do not create a virtual environment or load the host's site-packages.

When additional libraries are necessary, declare `requirements` in the manifest,
include that lock file in `files`, and list all direct/transitive dependencies with
exact versions and SHA-256 wheel hashes. For example, with real verified values:

```json
{"requirements": "requirements.lock", "files": ["main.py", "requirements.lock"]}
```

```text
package-name==1.2.3 --hash=sha256:VERIFIED_64_HEX_WHEEL_HASH
```

Only hashed wheels are accepted. Unpinned requirements, VCS/editable/local URL
inputs, nested lock files, source builds and package-index directives are refused.
This intentionally small format has no platform markers: choose compatible wheel
hashes for the target installation. No package code runs as part of syntax checking.

Explicit install/update or prepare creates a package-specific venv once, installs
with `--require-hashes --only-binary=:all: --no-deps`, and checks dependencies before
publishing a ready marker. Pip uses PyPI unless the operator sets
`AUTOMATION_PYTHON_WHEELHOUSE` to a local directory, which selects offline
`--no-index --find-links` installation. The active job is not changed if any
preparation step fails. No dependency resolution occurs, and package code never
installs anything.

Prepared versions live under `DATA_DIR/automations/python`. Their identities bind
all package files, the Python interpreter/version and the embedded runner/client.
Ready versions are reused and never rewritten by updates. Running work continues
using its existing version while a new version is prepared. Old versions remain
available for an explicit rollback; they are not garbage-collected automatically.
Preparation is serialized; ordinary runs still follow configured queue concurrency.

A prepared version is a local copy, not the approval. The approval is the
installation's digest. A process that lacks the version for its own Python
prepares it from the same digest-checked files and hashed lock before running
or enabling the job. That process can be another gateway, a gateway restarted
on ephemeral storage, or a server whose image changed the interpreter. A lock
with no wheels for that interpreter fails the run until an administrator
updates the package.

## Organization workflows

Organization-specific parsing, prompts, schema selection and installation settings
belong in a package repository. ForgetMeNote retains generic checks/repair, activity
aggregates, Drive import, profile maintenance, incidents and permission enforcement.
Installing the server does not install organization workflows. Store package
source and installation settings in your organization’s repository.

Repository-triggered scripts freeze the latest coalesced source revision and the
observed revision before that batch. This is not necessarily the last successfully
published revision. A first observation or unavailable Git history reports a
baseline. Source identity, revision and policy are checked before every operation,
including replayed receipts. A superseding update aborts the stale run; it does not
publish against a different snapshot. Notebook evidence uses the ordinary citation
and consent checks; publication uses the revision/hash captured before synthesis.

Read-only deterministic scripts may retain configured transient connector retries.
They replay completed receipts and repeat only failed non-effect calls; providers,
notebook publication and external submissions cannot opt into automatic job retries.
Interrupted paid calls and writes retain the existing unknown-outcome barrier.
A completed monitor run means the workflow finished, not that its finding was healthy.

When replacing a deployed specialized workflow, stop its scheduler, cancel old
queued/running work using the existing management actions, and reconcile uncertain
external effects. Replace the definition under the same ID and configuration owner.
Do not translate checkpoints or start old and new definitions together. Apply the
new binary and replacement definitions while stopped; retain backups of ordinary
content, credentials and catalog grants. Install/prepare the reviewed package using
the existing administrator flow before activation. Historical execution is not a
compatibility requirement. See the package repository's deployment checklist.

## Backup and restore

GitHub preserves package source, prompts, schemas and tests. Ordinary notebook
exports are not an operational automation backup. Instance backups preserve
catalog grants/accounts/consent, definitions and their owner revisions, schedules,
queue/run history, checkpoints, receipts and retained source evidence. Preserve
operator-mounted configuration separately in the existing protected backup flow.
Prepared source/environments are included with the instance data, but they are
only a local copy. A restored venv is only reusable with its matching
interpreter, runtime/client identity and filesystem paths. After a platform,
interpreter or installation-path change, each process prepares the installed
packages again on first use. A run interrupted before the change still refuses
to resume on a different environment. Restore credentials from
the secret system; discard `run-*` scratch directories from a stopped restore.
Unknown paid/write outcomes still require reconciliation after restoration.

### Cloudflare website evidence for moderation

`web.fetch` accepts `render: true` for public HTML landing-page captures. It additionally requires the existing `ai.images` grant, external-processing consent and non-local-only configuration. `image: true` and `render: true` are mutually exclusive. The same per-run web-fetch budget, durable step receipt, authority rechecks and source-work semaphore apply. Static `web.fetch` and original creative-image fetching retain their existing behavior.

Configure the optional operator-owned `chat.websiteCapture` section (or the same field in `CHAT_CONFIG_FILE`):

```json
{
  "websiteCapture": {
    "accountIDFile": "/run/secrets/providers/CLOUDFLARE_ACCOUNT_ID",
    "secretFile": "/run/secrets/providers/CLOUDFLARE_BROWSER_RUN_TOKEN"
  }
}
```

Mount both keys from the existing instance secret. Use an account-scoped Cloudflare token with Browser Run Edit permission restricted to the configured account. Neither account value nor token belongs in a package, notebook or tool argument. Configuration installs a concrete standard-library HTTP client for Cloudflare's documented `/browser-run/snapshot` endpoint; no new dependency or local browser process is required. The credential goes only to Cloudflare, never to the advertised site.

Capture performs a bounded public HTTP preflight, follows up to three single-frame frameset wrappers, then requests rendered HTML and a full-page PNG from Cloudflare with a bounded load/image wait. It returns `text`, `images` and `record` (advertised URL, final URL, navigation, screenshot SHA256, capture time, dimensions and coverage warning). The original full-page PNG is passed intact, with no host width, height or pixel-count cap. Unsuccessful pages, missing provenance, invalid images fail explicitly; there is no silent static fallback. The warning is evidence for a separate coverage judgment, not a guarantee that lazy, nested-frame, video or interactive content was captured. No login, payment, age-gate or other interaction occurs. Existing source capture/import behavior is unchanged.

OpenAI Decisions uses the endpoint’s documented 128-image limit; the Cloudflare moderation connection uses Clef’s limits described above. Image payload validation follows OpenAI’s documented 512 MiB request limit; OpenAI owns model-specific pixel limits and resizing. Moderation preserves every captured screenshot and does not discard visuals to fit a local count or dimension cap. Existing execution and context budgets still apply. Private execution receipts retain exact visual inputs and capture hashes; the submitted review details retain textual retrieval evidence and all typed questions/answers, without screenshot bytes.

### Application catalog credentials and code-only reads

An operator HTTP connection can use `oauthClientCredentials` with a fixed
`tokenURL` and private `clientIDFile`/`clientSecretFile` or
`clientIDEnv`/`clientSecretEnv` references. It exchanges application credentials
using OAuth Client Credentials with HTTP Basic authentication. The host caches
short-lived bearer tokens, invalidates them on credential rotation, expiration
or HTTP 401, and rechecks authority before outbound calls. A 401 is not retried.
Credentials and tokens are excluded from learning capture and redacted from
connector errors. No user refresh token or provider SDK is required.

`codeOnly: true` requires read-only operations and `externalProcessing: false`.
It permits explicitly granted reads from trusted Python scripts, including
scripts with AI capabilities, while denying ordinary API/MCP, chat and assistant tool access.
It does not track Python data flow: the approved package is responsible for
keeping catalog output out of AI prompts, images and learning evidence. This
setting is not a sandbox or permission to send provider data to AI. Operators
must review package changes and explicitly grant expanded capabilities and
connection operations; source auto-refresh cannot grant them.

For Spotify, place `SPOTIFY_CLIENT_ID` and `SPOTIFY_CLIENT_SECRET` in the existing
instance secret and mount them as private runtime files. Use
`https://accounts.spotify.com/api/token` as the token URL. Do not put secret
values in notebooks, packages, connector arguments or installation examples.

Public website snapshots use a bounded eight-second scroll pass to activate lazy
images and entrance content, settle images/fonts, and return to the top. The
snapshot waits for the host's per-request loading report before collecting rendered
HTML and the original full-page PNG. Optional `record.loading` observations include
bottom traversal, deadline/error state, unfinished/broken images, font readiness and
document dimensions. The host compares the original screenshot against those dimensions
to identify clipping; it imposes no new image size limit or resizing. Missing or invalid
loading reports retain an error observation instead of claiming successful loading.
These are page observations, not proof of semantic coverage or trusted instructions.
The moderation package uses known traversal/time-budget/error/clipping gaps to prevent a
clean destination finding while preserving visible violations. Other loading signals
inform the semantic material-coverage question together with rendered text and pixels.
Historical captures without loading observations retain their existing semantic check.

Creative image fetches may opt into `detail: true` alongside `image: true`. The response retains the original `image` and returns overlapping unscaled `regions` with image data and coordinates. This uses the existing image authority, cancellation and source-work concurrency boundary, with no extra network request or grant. Regions are omitted if they cannot fit Clef media capacity; original bytes remain unchanged. Website capture leaves overlays untouched by default. A separately receipted `render: true, dismissPromotions: true` retry may close only clearly promotional overlays and records `loading.dismissedOverlays`; the caller retains and assesses the original view too. Access, age and payment gates remain untouched.
