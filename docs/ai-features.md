# Instance AI controls

Admins manage shared AI processing in **Administration → AI features**. Provider
credentials stay in operator configuration and mounted secret files. Adding a key
does not switch on features.

- **Background embeddings** uses the configured endpoint and model for existing
  and future notebooks. Defaults are `text-embedding-3-small`, 1536 dimensions.
  Changing an existing model, dimensions or embedding input format requires explicit rebuild confirmation.
  Compatible vectors survive policy changes.
- **Repository map classification** classifies changed, otherwise unclassified
  source files with the configured decision provider.
- **Search reranking** and **Check answer citations** use the configured decision
  provider at request time.
- **Notebook reviews and automatic checks** permits manual reviews and debounced
  consistency checks of subsequent committed changes in ordinary notebooks.
  Enabling establishes a baseline; it does not review the entire existing library.
- **Allow organization, writing and profile automations** permits existing
  configured jobs and organization operations. Each job still needs its service
  account, notebook/connection permissions and its configured policy. This switch
  does not create jobs or grant new data access.

Notebook settings retain index status, instructions, search defaults, repository
path filters and source refresh/pause controls. They do not configure AI providers
or feature opt-ins. Source pause and path exclusions still apply. Private Inboxes
retain their capture-specific consent and are excluded from shared background
indexing and automatic reviews. Unsaved drafts are not background input.

Provider availability, local-only restrictions and current notebook access are
checked independently of feature enablement. Queued embedding requests recheck
policy immediately before sending data; an in-flight request cannot be unsent,
but its result is rejected after revocation. Disabling features preserves content,
Git history, vectors and job checkpoints. Existing workers reconcile the saved
policy; repository processing can start on their next scheduled scan.

## API, CLI and MCP

The same authenticated management actions serve every client:

- `ai_settings` with `{}` returns policy revision, provider availability and
  per-notebook processing status. It never returns credentials.
- `set_ai_settings` accepts `{"config": {...}, "rebuild": false}`. Read the current
  configuration first, preserve fields, and supply its `revision`. Concurrent
  stale saves fail with a conflict. Model changes require `rebuild: true`.

HTTP uses `POST /api/organization/action/ai_settings` or `set_ai_settings`.
Both actions require an instance Admin and a client authorized for management
(`notes:manage`); notebook ownership is insufficient. Policy writes are audited.
A scoped MCP token cannot acquire broader notebook visibility through these tools.

## Existing installations

Until the first administrator save, persisted legacy opt-ins and existing API/MCP
configuration consumers continue to work. The browser exposes only the new
central controls. The first save explicitly replaces legacy notebook and personal
AI policies for the whole instance; no legacy opt-in can override it afterward.
The old configuration writers return a conflict directing clients to
Administration. Repository index writes can still update path filters, while AI
fields are always taken from the instance policy.

The catalog stores policy outside notebook Git history. Saving upgrades its format
to version 6 so an older server cannot silently ignore the policy and revive old
opt-ins. Back up the instance before upgrading; downgrading requires restoring a
compatible pre-upgrade instance backup. Notebook exports remain ordinary files and
Git history. No credentials are added to them.
