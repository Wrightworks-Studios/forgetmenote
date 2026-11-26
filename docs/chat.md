# Integrated notebook chat

The browser separates conversation requests from transcript rows, run activity,
unsent composers, and attachment/export dialogs. Refreshing unchanged rows keeps
their controls in place, preserving keyboard focus and open disclosures. Independent
request lifetimes prevent older configuration, defaults, lists, or conversation
responses from overwriting the current selection. Retired conversations keep unsent
text and attachments as private, reachable drafts; account or notebook changes clear
that session's chat state.

Transcript Markdown continues through the shared Goldmark and Bluemonday renderer,
then the browser applies pinned citation navigation and removes inline media. A client
parser alone would not preserve this sanitizer, frontmatter, or CSP contract, so it
does not replace the renderer. A conversation-scoped cache keeps at most 64 rendered
Markdown requests, reuses identical text, and clears on selection or authority changes.
It stores no transcript in browser storage and makes no extra provider call. Public
raw module entry URLs remain available for already loaded clients alongside bundled
assets.

Open or close chat with the floating bubble at the top right. It highlights while open and is available from the root, folders, empty notebooks, and individual notes. Chat opens from the side. Opening chat closes comments and temporarily hides the sidebar. Closing chat restores its prior visibility; opening comments closes chat without discarding either unsent message. Chat stays open with the same conversation, ongoing response, and unsent message while you browse within that notebook. On mobile, tap the bubble again or press **Escape** to return to the notebook; reopening Chat restores the conversation and unsent message. Reloading the tab restores the open panel and selected saved conversation, but does not save unsent chat text in browser storage. Switching notebooks or accounts clears the composer and changes the conversation scope.

Sign in with a personal account; recovery administrator credentials cannot own or read personal conversations. Conversations belong to the stable account and selected notebook, remain private from other users including notebook Owners, and follow that account across devices. Every read, stream frame, model run and workspace action checks present notebook access. Removing access or disabling the account blocks retained transcripts and citations too. Those checks include every other notebook the conversation drew on: each run's selected note and source notebooks and every cited notebook or source. Losing read access to any one of them makes the whole conversation unavailable (403) for reading, streaming and **Save to note**, rather than showing a redacted copy; **Delete conversation** still works, and restoring access restores the conversation. A protected quick-note Inbox can be the chat notebook or part of a corpus only for its own account, with that account's personal cloud consent; Admin or Owner authority over someone else's Inbox does not allow it. Already downloaded data cannot be recalled.

An operator must configure a connection before Send is available. The browser receives only named connection/model choices, capabilities and limits. It cannot set an endpoint, credential or arbitrary model. Removing a saved connection requires an explicit replacement choice; there is no cloud fallback.

## Working with chat

The conversation is the main scrolling area. The current notebook and **Ask / Suggest / Edit** mode are visible beside the composer, so each message’s scope is apparent without opening settings. The gear opens a **Chat settings** modal containing the choices available to your account: model/connection selection when alternatives exist, permitted action scope, and optional research or review controls. Fixed operator configuration, usage counters and explanatory boilerplate do not occupy the conversation. Tool details and sources remain available on demand; active Stop, errors, Continue and required action approvals stay accessible. Final answers use the notebook's sanitized Markdown renderer for headings, lists, tables and code.

- **Ask** reads committed evidence. **Suggest** prepares reviewable changes/findings. **Edit** additionally permits the selected categories of user-directed edits, through the existing roles, locks, revision guards and grouped writes.
- A run that draws on another notebook's corpus is read-only. A conversation that has used your personal assistant context (injected memory and preferences, `assistant_context` or `read_personal_source`) can still change notebook content, but outside your own assistant notebook every change, including a proposal, waits for your approval. Because memory is injected whenever your assistant is enabled with external processing, Edit then asks before each change in shared notebooks. The approval card shows the exact arguments, so personal content reaches other notebook readers only in a change you have reviewed. Your own assistant notebook accepts these changes directly.
- The attachment icon is available when a note is open; opening or browsing notes never attaches them automatically. Attach **Committed**, **Shared live**, or **Private draft** deliberately. Committed attachments come from Git; shared live content comes from the accepted Yjs document; a private draft sends the exact browser text only after that explicit choice. Ordinary messages never send the editor draft. Attachment kind, revision and hash remain in the transcript. Committed attachments retain their original note and reference fingerprints as answer dependencies. Live/private attachments retain the identity and hash of the exact accepted snapshot; follow-ups label claims about those snapshots and do not sample newer live or editor content. Missing or inconsistent original inputs disable affected answer reuse.
- Citations open pinned source previews without replacing the editor or its draft. Search results and model answers are untrusted content. A cited passage supports only what it contains; ranking scores are not confidence scores.
- **Stop** cancels future work. A disconnected browser can reopen the persisted stream without restarting it. **Continue** grants another explicit bounded allowance after interruption. Provider or network failure does not trigger automatic retries or model escalation.
- Destructive/administrative notebook actions, and research fetches of URLs you did not supply, show the exact prepared arguments and require a separate approval. Empty Trash additionally binds the current Trash revision and confirmation manifest. A stale approval fails; inspect the notebook and prepare a new action. A model cannot approve its own action or silently unlock a note. An approved move also updates links in notes in the Trash, which no chat selection includes, wherever an ordinary move could; it skips the other notes there instead of failing, and the model learns only how many it updated and skipped ([Trash](trash.md)).
- **Delete conversation** is the trash action beside the conversation selector. Its confirmation names the selected chat. Stop queued, running or approval-waiting work and wait for stopping to finish first. Deletion removes the saved transcript and its attachments from your account on every device; it does not undo notebook edits, applied proposals or notes you explicitly exported. Unsent text and attachments remain available as **Unsent draft (deleted conversation)** in the selector. A refresh also exposes drafts from conversations deleted on another device while unselected. Explicitly selected drafts stay selected, including those containing only attachments. A new-message draft and drafts in other conversations stay separate. Drafts remain in memory only. Existing operator backups retain their prior copies.
- **Save to note** in a message's action menu opens a preview with that message selected. It exports explicitly selected messages and citations through a guarded Git change, rechecking Edit access immediately before the write. Conversations themselves are not notebook notes and do not appear in notebook Git, exports or ordinary search.

Owner-managed root `AGENTS.md` guidance is read from the committed notebook snapshot and supplied separately from retrieved note bodies. It applies within the user-selected task and cannot authorize actions or override permissions. Missing guidance is normal; guidance that cannot fit the configured context produces an explicit error rather than silently losing instructions. Reviews include this guidance in each batch and invalidate reused analysis when it changes.

The same typed workspace handlers and schemas serve embedded chat and MCP. Chat uses the signed-in identity directly, without a public MCP/OAuth round trip or administrator token. Cross-notebook and instance administration stay outside the notebook model scope. A distributed notebook still executes through its assigned owner's workspace transport; chat does not write that owner's files directly.

## Operator configuration

Set `CHAT_CONFIG_FILE` to a read-only mounted JSON file. Mount each credential separately and make it readable only by the service identity. Keep configuration and credentials out of notebook Git, prompts, logs, frontend bundles and browser storage. The configuration is loaded at startup; a failed or invalid file fails startup explicitly. Names, endpoints, formats and capabilities are operator declarations, not discovered from note content.

The initial deployment target is DeepSeek V4.1 Flash, whose documented API identifier is `deepseek-flash`. This example deliberately disables thinking and uses only explicitly supported Chat Completions fields:

```json
{
  "defaultConnection": "deepseek",
  "localOnly": false,
  "connections": [{
    "id": "deepseek",
    "label": "DeepSeek V4.1 Flash",
    "format": "chat-completions",
    "baseURL": "https://api.deepseek.com",
    "models": ["deepseek-flash"],
    "defaultModel": "deepseek-flash",
    "secretFile": "/run/secrets/deepseek-api-key",
    "local": false,
    "capabilities": {
      "tools": true,
      "stream": true,
      "streamUsage": true,
      "jsonOutput": true,
      "thinking": "disabled"
    },
    "limits": {
      "contextTokens": 1000000,
      "outputTokens": 393216,
      "runSeconds": 180,
      "connectionConcurrent": 2
    }
  }]
}
```

`jsonOutput` declares provider support for native JSON output: Chat Completions uses `response_format: {"type":"json_object"}` and Responses uses `text.format: {"type":"json_object"}`. Automatic query repair requests this mode when configured; ordinary chat remains free-form. The application still validates the response fields and proposed SQL. See [OpenAI JSON mode](https://developers.openai.com/api/docs/guides/structured-outputs#json-mode) and [DeepSeek JSON Output](https://api-docs.deepseek.com/guides/json_mode/).

The example uses the documented DeepSeek V4.1 Flash context and maximum output capacities. Declare capacities for the actual endpoint/model; other providers do not inherit this connection’s values. A larger context is available when needed and does not cause retrieval to fill it.

Use the actual mounted secret path, not its contents. The host's ignored `secrets/` directory is excluded from source builds; Docker must mount the required file separately. The app never returns `baseURL` or `secretFile` in `/api/chat/connections`.

Supported formats:

| Format | Base URL example | Native contract |
| --- | --- | --- |
| `responses` | `https://api.openai.com/v1` | `/responses`; native function calls and call IDs; returned reasoning/continuation items remain private. |
| `messages` | `https://api.anthropic.com/v1` | `/messages`; native content blocks and tool results; indexed streaming blocks. |
| `chat-completions` | `https://api.deepseek.com` | `/chat/completions`; assistant tool calls and matching tool results. |
| `chat-completions`, `local: true` | `http://ollama:11434/v1` | Ollama's supported compatibility subset; install and name a local model explicitly. |

Capabilities default to false/absent. Configure `tools`, `stream`, `streamUsage`, `jsonOutput`, `strictTools`, `storeFalse`, `cacheControl`, `encryptedReasoning`, `reasoningContent`, `thinking`, `thinkingBudget`, `reasoningEffort`, and `outputTokenParameter` only when supported by that exact endpoint/model. Invalid format/capability combinations are rejected. Strict tool mode converts a private copy of each schema, representing omitted optional fields as nullable required fields; execution restores only those omitted fields before validating the original action schema. Native tool-call arguments remain unchanged for provider continuation. All currently exposed actions support this conversion; an unrepresentable future schema fails explicitly rather than changing its accepted arguments. Messages thinking requires an explicit budget; DeepSeek thinking requires `reasoningContent` replay. No speculative beta headers or provider search tools are assumed. Ollama context size belongs in its model/Modelfile; it is not an invented compatibility request field. Models without tool support still receive automatic committed passage retrieval.

A fully local deployment sets `localOnly: true`, permits only connections declared local, and disables research. `embeddingsLocal: true` is the operator's explicit declaration that the separately configured embedding endpoint is local. Without that declaration chat disables query embeddings and historical retrieval that could start background embedding work. The operator-wide local-only flag pauses any undeclared embedding endpoint, including shared search and background indexing, while retaining its configuration. Declare a genuinely local endpoint explicitly to enable it. Previously in-flight external work cannot be recalled.

Official contracts: [OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling), [Responses streaming](https://developers.openai.com/api/docs/guides/streaming-responses), [OpenAI caching](https://developers.openai.com/api/docs/guides/prompt-caching), [Messages tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools), [Messages streaming](https://platform.claude.com/docs/en/build-with-claude/streaming), [DeepSeek V4.1 Flash identity](https://www.deepseek.com/en/news/deepseek-v4-1-flash/), [DeepSeek thinking continuation](https://api-docs.deepseek.com/guides/thinking_mode/), [Ollama compatibility](https://docs.ollama.com/api/openai-compatibility).

## Retrieval, execution limits and reviews

Chat has its own committed passage retrieval. The sidebar's explicit Text search and MCP `search_notes` retain literal substring/phrase behavior. Chat uses an in-memory BM25 index (`k1=1.2`, `b=0.75`, body/title/path weights 1/3/2), exact title/filename priority, and equal reciprocal-rank fusion (`k=60`) with available positive semantic matches. The configured `perNotePassages` ceiling applies to primary candidates in automatic context, tool search and review comparisons; optional same-section neighbors add at most two. Candidate diversity is applied before top-100 truncation. It packs useful passages and neighbors, deduplicates content and bounds per-note concentration. Indexes bind Git revision, visibility and embedding configuration; a stale or unavailable owner snapshot cannot authorize cached results. History uses existing reachable-history coverage and tombstone protections and reports partial coverage.

Chat query embeddings receive a bounded cold-start allowance of at most 30 seconds and one third of the remaining run time, including queueing, so time remains for an answer. Cancellation stops that wait. BM25 ignores common English question framing while preserving exact names and quoted phrases. Weak topical coverage is explicitly distinguished from proof that information is absent. A successful semantic query with no candidates may receive one focused reformulation within the same remaining allowance; a timeout or BM25-only result is not automatically retried. Retrieval diagnostics record mode, semantic outcome, duration and counts without question text, note content or endpoint details.

Ordinary context combines a stable instruction/tool prefix, bounded prior conversation excerpts, the recent turns and selected original evidence. Each committed citation retains immutable whole-note and reference fingerprints from its original snapshot, with passage checksums kept separate. Notebook reads through tools also record pinned citations and carry them into the answer. Answers with untracked tool provenance remain in the local transcript but are omitted from reusable model context. A follow-up also inherits the immutable dependencies of every retained assistant excerpt, including compact memory and any earlier inherited dependencies. Uncited snapshot dependencies remain attached even when a later answer has its own citations. These validation dependencies stay separate from displayed citations; an oversized dependency set disables answer reuse rather than growing without bounds. Prior answers with changed/unavailable fingerprints are omitted from both recent and compacted model context, including after later retrieval refreshes; legacy answers without verifiable provenance are treated conservatively. Full transcripts and tool results remain local. Compact memory is explicitly unverified navigation text; it cannot authorize changes. No additional model call is made for ordinary retrieval or excerpt compaction. Review analysis is reused only for matching purpose/model, explicit attachments, action/research scope, instructions, note hash and linked-source/backlink fingerprint. Original pinned passages are supplied for each new analysis batch and candidate comparison. Reusable review records retain bounded provenance for primary, comparison and tool-read sources, attached snapshots and inherited navigation summaries. Changes to any consumed evidence invalidate affected summaries, coverage and prepared batch messages; unavailable or excessive proof cannot certify complete coverage. Retired text stays in the durable transcript even when omitted from model context.

The exact initial source envelope is retained privately, separately from later tool citations. Admission may discard older conversation history, but source fitting changes only request copies: a later final-answer request can recover original excerpts when removing tools frees space.

Before every model round, context fitting measures the configured native request encoding, including tool schemas and opaque provider continuations. The same estimate governs provider context admission and uncertain-usage accounting. One shared evidence budget covers complete source entries, including text, URLs, paths, revisions and hashes; separate per-tool text limits cannot bound that metadata. Equivalent original excerpts are deduplicated across initial context and read/search results, including matching historical evidence. Selection keeps substantive original reads and ranked passages with breadth across notes, preserving exact pinned citations and whole-note hashes for guarded edits. Older source results can become concise acknowledgements when their evidence is retained elsewhere or omitted. These acknowledgements explicitly distinguish omitted evidence from absent evidence and never certify complete coverage. Native message order, reasoning and every call/result pair remain intact. Full original results, transcripts, citation metadata and receipts remain durable; compaction makes no model call.

Ordinary **Ask** reserves a final answer after at most two exploration requests. After successfully using recipe discovery or a data connection tool, it can use the configured round allowance for dependent discovery, schema inspection and execution, with the last request still reserved for an answer. An exhausted tool allowance, a smaller configured round ceiling, an approaching deadline can bring that answer forward. If an already-returned batch of read calls exceeds the remaining tool allowance, only permitted calls within that allowance execute. Excess ordinary Ask calls receive durable `omitted` receipts and explicit non-executed budget results paired with their original native IDs; they are not counted as executed tools or silently replayed after restart. The assistant then answers from retained evidence, with the omitted work stated as unexamined. The final request preserves the configured output capacity and fits its evidence to the model context. Both ordinary and final instructions ask for concise, natural answers with Markdown links using supplied titles and exact pinned URLs next to supported facts or table entries. Uncertainty, missing evidence and source distinctions should be discussed when they affect the question; unrelated retrieval narration is discouraged. These instructions do not guarantee model compliance. Reviews, Suggest and Edit retain their existing bounded workflows. Native Responses uses documented `tool_choice: "none"`; native Messages uses `tool_choice: {"type":"none"}` while retaining tool definitions and native history. Compatible Chat Completions endpoints receive no offered tools and no assumed `tool_choice` field. Unexpected final-answer tool calls are rejected before dispatch, with their response and usage retained. See the [Responses contract](https://developers.openai.com/api/reference/resources/responses/methods/create), [Messages tool controls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools), and [DeepSeek completion contract](https://api-docs.deepseek.com/api/create-chat-completion/).

Existing interrupted conversations can continue from their completed read/search receipts. An older notebook-only Ask that already explored several rounds proceeds directly to its final answer. Legacy reads must match their original metadata and revision; repeated tool IDs alone cannot identify a receipt. Ambiguous clipped legacy results remain unchanged rather than borrowing another revision. A **Model context limit reached** message means protected inputs still cannot fit after optional evidence was reduced: shorten explicit inputs or start a focused conversation. Continuation does not increase configured model context. The task, notebook guidance, explicit attachments and native history are never silently truncated to force a fit.

Server logs include one terminal outcome and a structural record for each model request: internal chat/run IDs, configured connection ID/format, mode, state, rounds, tool/request counts, fitted input estimates, retained source counts and fixed outcome categories. Failure records distinguish context, allowance, provider, access, cancellation and internal failures even when the initial HTTP request returned 200. State transitions and diagnostic categories use typed failure reasons, independent of displayed error wording; unclassified failures use `operation_failed`. Research search/fetch limits retain their interrupted state and now report `research_budget` instead of the generic failure category. Logs exclude notebook paths/text, prompts, tool arguments/results, raw provider errors/endpoints, credentials and opaque reasoning. A recovered runner panic records a generic saved failure without logging its value or stack. Public assistant turns identify intermediate tool planning by deriving the flag from private native tool calls. The new flag is not written into durable turns, preserving compatibility with earlier strict record decoders; opaque event payloads still carry the stream projection. Native provider state remains private.

Default execution safeguards (connection values can lower them). Each connection may explicitly declare its own model context and output capacity; the global context/output values are defaults, not ceilings:

| Resource | Default |
| --- | --- |
| Concurrent work | 4 runs per instance; 2 per connection; 1 active run per user |
| Stream subscribers | 8 per user; 64 per instance |
| One allowance | 180 seconds; 8 model rounds; 16 tool calls |
| Model context/output | 16,384 / 2,048 tokens |
| Evidence | 6,000 estimated tokens; 12 passages; 4 primary candidates per note |
| Transcript storage | No default conversation-count quota; configurable `maxRecordBytes` (64 MiB) and `maxEvents` (20,000) per conversation |
| Browser transcript page | 100 finalized turns plus unfinished streamed output |
| Attachments | Combined content/metadata uses `FMN_LIMIT_SOURCE_TEXT_BYTES`; no attachment-count quota |
| Tool arguments | Provider wire bounds follow declared model context/output capacity; originals are reduced only when the complete request cannot fit |
| External research | 3 searches and 5 fetches per run; shared 2-capture concurrency |

Chat imposes no run, user, daily, connection or instance token-spending quota. Legacy `runTokens`, `dailyTokens` and `budgetTokens` settings are accepted and ignored. Continue renews only execution safeguards such as time, model rounds and tool calls; prior reported or uncertain usage never reduces that allowance. Model context/output capacity and relevance-based evidence selection still apply.

Provider totals are recorded when supplied. Input totals include Messages cache-read/cache-write tokens without double counting; cache/reasoning counters remain visible subsets where applicable. Missing or interrupted totals are labeled estimates, with uncertain output recorded conservatively. Each model attempt counts, including failed attempts and explicit continuations. Durable UTC day buckets attribute each request to its actual request day, including a continuation or a request that finishes after midnight; changing a run’s creation date is never used to move past costs. Known usage is settled with reserved record capacity before transcript additions, so a full transcript cannot erase paid work. The expandable Usage view shows conversation totals and account totals from the last explicit Refresh. The safe connection metadata also exposes personal aggregate usage across the current account’s chats, with an explicit count of unavailable records whose costs are unknown. Counts are usage accounting, not a currency estimate or billing guarantee. EOF without a native terminal event is an interrupted provider response, not successful completion.

**Review the notebook in bounded batches** captures visible committed note entries within the configured index and transcript storage budgets, excluding Trash. Each model batch examines one original passage of at most 1,200 Unicode characters, plus bounded original comparison evidence and compact navigation summaries. Coverage stores every note's revision/hash/reference fingerprint, passage cursor/total, summary and unfinished state. Deliberately attached committed/live/private content is included as separately labelled evidence and never changes the committed inventory. The normal explicit run allowance applies to reviews too; there is no hidden larger review budget.

A successful batch advances only its examined passage. A short answer does not certify the rest of the notebook. Resume refreshes the inventory and requeues changed notes and notes whose linked sources/backlinks changed. Coverage completion describes those recorded revisions, not every conceivable pairwise comparison or external factual verification. Unchanged matching analysis may be reused, with its original examined revision retained. With Edit and the Reviews action scope enabled, completed coverage enters a bounded finalization phase for remaining user-requested workflow operations. It retains the original task, checkpoint receipts and examined snapshot; it never writes a checkpoint automatically. Finalization shares the remaining rounds, time and tool calls, and allowance exhaustion preserves completed examination for explicit continuation. Workflow checkpoint operations use the existing compare-and-swap contract and must name the examined snapshot; retries cannot silently change the previous-checkpoint/reset preconditions to bypass a conflict. Incomplete review coverage must not be reported as a complete workflow. When both controls are explicitly enabled, research and review work together within the current bounded allowance: successful fetches preserve the tool continuation and fresh original-note examination. Fetched URL/date/hash metadata stays separate from committed dependencies; search results retain their observation date and unverified-candidate label. This external evidence can inform the current allowance, but its later freshness is not verified. Explicit continuation or a new review re-examines externally informed summaries instead of reusing them; ordinary later answers also omit that cached analysis. This never triggers an implicit search, fetch or archive. The full transcript, dated results and review progress events remain durable. Tool evidence without tracked provenance, or dependency records beyond their storage bounds, still leaves coverage incomplete and stops the allowance. Archive useful sources explicitly when they should become committed notebook material.

## Optional research

Research is off unless both operator configuration and the current user run enable it. It is prohibited by `localOnly`. Choose the documented Brave search contract or OpenAI web search on an existing Responses connection. Brave uses an independent credential:

```json
"research": {
  "enabled": true,
  "format": "brave",
  "baseURL": "https://api.search.brave.com/res/v1/web/search",
  "secretFile": "/run/secrets/brave-search-key",
  "maxSearches": 3,
  "maxFetches": 5
}
```

OpenAI search reuses the configured connection's mounted key and default model:

```json
"research": {
  "enabled": true,
  "format": "openai",
  "connection": "openai",
  "maxSearches": 2,
  "maxFetches": 4
}
```

Do not set `baseURL` or `secretFile` in OpenAI research; those belong to the selected connection. Each request forces one `web_search` tool call, limits generated output to 1,024 tokens, disables response storage and streaming, and returns consulted/cited public URLs. Generated summaries are not fetched evidence. Token usage is retained separately from search attempts; OpenAI search also incurs its provider tool fee. Unknown paid search outcomes stop automation effects for operator reconciliation. No automatic retries or provider fallback occur. See the [OpenAI web search contract](https://developers.openai.com/api/docs/guides/tools-web-search).

`research_search` supplies only a query and bounded result count. The server adds the mounted `X-Subscription-Token`, `result_filter=web` and `text_decorations=false`; it follows no search redirects and sends no ambient proxy credentials. Search snippets remain unverified candidates. [Brave's search contract](https://api-dashboard.search.brave.com/api-reference/web/search/get) defines the endpoint and response fields.

`research_fetch` fetches a URL without confirmation only when it appears verbatim in your own message for that run or exactly in that run's completed `research_search` results. Any other URL may have been chosen by injected note, email, data or web text and could carry private context in its path or query, so the run pauses with **Approval required** in every mode and shows the full URL in the prepared arguments. Approve fetches exactly that URL once and counts it against the fetch limit; the run then waits for an explicit **Continue**, like an approved notebook control. Decline sends no request. Fetches use shared source URL validation, DNS/IP pinning, every-address and redirect checks, public HTTP/HTTPS ports, extraction limits and a 30-second outer deadline. It returns bounded text, source URL, capture metadata and checksum, without changing notebook Git. Attempts are persisted before network work, so interruption cannot reset request limits. An uncertain research request is not silently retried. Preserve useful evidence with the separate explicitly approved `archive_source` action; that action uses the existing provenance and guarded import path.

## Storage, recovery and backup

Each conversation is one `chat_conversations` row in the instance database, keyed by the SHA-256 of the stable account ID and a random conversation ID. A mutation rewrites the whole record only over the row version it read, so a write over another gateway's newer record is refused, never merged or lost; the row version is also what a stream polls, so a stream on any gateway sees every write. An `active` flag marks conversations with unfinished runs for admission. Each run records the gateway executing it. A gateway that opens the store, and the gateway holding the [chat recovery lease](postgres.md#leases-and-presence), interrupt only the unfinished runs of gateways that are gone, and a cancellation on any gateway stops the run within a second. Records include full user/assistant content, explicit attachments, citations, private native provider continuation, operation arguments/results, run state, review coverage, compact memory, usage and ordered stream events. Provider reasoning continuation is never included in the browser transcript or SSE.

Model-call reservations and operation identities are persisted before external work. Interrupted queued/running runs become interrupted at startup and need explicit continuation. Initialization on resume always uses that run’s original user turn, attachments and preceding history; later exchanges cannot replace its task. Grouped Git writes, live patch IDs and finding/proposal keys use deterministic operation IDs. Live checkpoints currently stop as uncertain after a lost acknowledgement because the existing path-only tool does not bind the original accepted live version; recovery never checkpoints newer live edits automatically. A stored receipt resolves a lost acknowledgement without a duplicate commit; operations lacking such a receipt stop with an explicit uncertain outcome. Acknowledged action outcomes are recorded privately even when permission changes immediately afterward; current authorization still controls all disclosure and model continuation. Approved controls then pause for explicit continuation. A cancelled request or unavailable owner with an ambiguous acknowledgement retains its incomplete operation receipt; replay policy determines whether continuation can reconcile it or must report uncertainty. Before admitting work or accepting an action decision, inaccessible paused approvals are durably cancelled, their confirmations invalidated and their unused reservations released, while incurred usage remains. A new login cannot approve a confirmation from a revoked run epoch. Starting the same submitted message twice reuses its run; reusing an ID with different content fails.

Writes reserve recovery headroom. A valid legacy record already at capacity can use a small hash-bound recovery overlay beside the record, preserving its original transcript bytes while marking unfinished work interrupted. A damaged conversation appears unavailable; healthy siblings remain readable. Damaged usage is explicitly reported as unknown and does not prevent work in healthy conversations. Restore damaged records to recover their transcripts and accounting.

Deletion atomically replaces the private record and its recovery overlay with an accounting-only receipt, then clears any overlay again before acknowledging success. The receipt retains the stable owner, opaque conversation/run IDs, dates, connection IDs and usage/day buckets; it has no title, messages, attachments, citations, tool arguments/results, provider continuation, review text, compact memory or stream events. Cached streams are retired as soon as the matching receipt replaces the conversation, even if later synchronization or cleanup fails; the pending deletion dialog keeps its original endpoint and error available for an explicit retry. A transcript 404 does not acknowledge cleanup or cancel an unresolved deletion request. After an error or lost acknowledgement, choose Retry deletion in that dialog; no automatic DELETE retry occurs. The pending operation ends after a successful acknowledgement or dismissal, and is cleared when the account or notebook changes. A wrong-notebook 404 does not retire another conversation's stream. Startup finishes interrupted cleanup. Receipts use only existing version-1 fields and the reserved `deleted:<notebook>` project namespace, which the notebook catalog cannot create. Current lists and conversation-count limits exclude them; UTC day, connection and lifetime usage records still include them. Conversation records now use version 2 with saved per-run corpus authority. Version-1 records remain readable without migration; intentionally starting a scoped turn atomically preserves old turns, receipts, citations and accounting while upgrading the conversation. Legacy runs with unproven scope cannot resume or approve an action. An image that predates version-2 chat support cannot safely read all version-2 records: rollback to that image requires disabling integrated chat with `CHAT_CONFIG_FILE=''` for every user while retaining current runtime data for forward recovery. A preceding image that already supports version 2 still requires exact image/data compatibility verification. Accounting-only deletion receipts retain their existing version-1 compatibility; older images may count them toward conversation limits. Never restore older runtime data merely to roll back an image. Damaged receipts remain unavailable and contribute to the unknown-accounting count until restored.

`DELETE /p/<notebook>/api/chat/<id>` requires the stable owner and current notebook Read access, including on retries. The original account/session and present notebook Read access are checked again after lock waits at the storage mutation boundary. Successful deletion and its retry return `200 {"deleted":true}`; normal reads, streams and mutations then return 404. Active or unsettled work returns actionable 409. A concurrent read, replay frame, list response or explicit export can also produce a brief busy 409; retry explicitly. Requests use conversation-specific fences, so another conversation's slow transcript or export cannot queue an instance-wide deletion lock. Deletion does not start provider work or grant another allowance.

A notebook Git bundle does **not** contain chats, identities, live collaboration state or usage. A full stopped-service backup must copy the instance database, which holds chat, and all of `/data`, including notebook repositories, identity/permission state, workflow data and collaboration files, preserving ownership and modes. Provider configuration and credential mounts are separately managed operator assets; restore their mounts deliberately. See [complete backup and independent restore](backups.md).

The SSE response sets `X-Accel-Buffering: no` and no-store, flushes complete durable frames, rechecks the session and notebook authorization per frame, uses bounded writes and sends idle heartbeats. Reconnect uses the durable event sequence; it does not resubmit the message. Text recovered from an interrupted process keeps its original transcript position, citations and pagination, and can be explicitly saved to a note once generation has stopped. A run that a recheck refuses while the model answers (a revoked session, removed access, a disabled account or a publishing authority handoff) ends failed and keeps none of that answer: its streamed frames become `retracted` frames in place, so sequences and reconnects hold, and one more `retracted` frame tells open pages to drop the turn. Existing proxy infrastructure is retained. Direct application/Docker/browser streaming is tested; verify actual proxy delivery/idle timeouts in the deployment environment rather than assuming a header proves end-to-end behavior.

## Validation and limits of live verification

Synthetic native HTTP/SSE provider tests cover Responses, Messages and Chat Completions fields, tool continuation, malformed input, partial streams, native terminal events, cancellation, usage/cache accounting and secret-bearing diagnostics. Real temporary Git/HTTP/MCP SDK tests cover scope/roles/revocation, guarded edits, persistent receipts and recovery. Chromium covers desktop/mobile, acknowledgement retry, reconnect, exact approval, model removal, citations, paging and private drafts against both synthetic UI fixtures and an actual embedded application. The release runner additionally performs candidate Docker smoke and full stopped-volume restoration into an independent disposable instance.

Synthetic retrieval fixtures cover exact and quoted names, natural questions, paraphrases, cross-note comparisons, irrelevant queries, missing evidence, slow embedding startup and cancellation. Source recall and evidence-token estimates measure evidence selection, not answer quality, billing or token savings. Live release probes use generated notebooks and separately inspect actual answers and provider-reported usage.

Chat gear fixes Archives, shared Drafts and previous-version preferences for each
run. Notebook AI defaults exclude both containers; preferences are defaults, not
restrictions on a deliberate new selection. The wire object is
`{"version":1,"archives":"exclude","drafts":"exclude","history":false}`;
`history` is always serialized. Its SHA-256 identity binds run, assistant, memory
and review provenance. Current, historical and explicit-attachment evidence keep
separate identities. See [Notes, Drafts and Archives](containers.md) for scope,
review completion, Owner guidance and exact attachment semantics.


## Optional connected repository context

Chat settings can select up to ten repository notebooks in addition to the current
notebook's saved note corpus. The optional `sourceProjects` field is resolved to
server-owned `sourceBindings` at each new run. Missing fields preserve existing
single-notebook chat behavior and corpus identities. Continue uses the original
run's bindings; reconnecting a notebook to another repository cannot silently
substitute its content.

Owners must enable external source processing before selecting a repository for
chat. Current Read access, connection authority, processing consent, cited commit
and owner path exclusions are checked at provider boundaries and before delivery.
Explicit file tools obey the same exclusions as retrieved excerpts. Search and
range reads return original source with deterministic line/byte coordinates;
repository citations have `kind`/`scope` set to `repository` and retain project,
source binding, commit and content hash. They do not pretend to be Markdown notes
or permanently retained evidence. Explicit citation capture keeps its existing
Comment-or-greater authority and full-file size limits.

A run containing another notebook's repository context cannot mutate notebook
content. Reading does not grant permission to publish that source into a different
shared notebook. Existing note/container permissions remain authoritative. See
[repository notebooks](repository-notebooks.md#ask-about-repositories).
