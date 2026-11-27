# Knowledge workflows

Integrated chat adds a separate committed-passage BM25/semantic retrieval path and bounded review inventory. Existing sidebar Text and `search_notes` retain literal behavior. See [chat retrieval, coverage and budgets](chat.md#retrieval-execution-limits-and-reviews).


ForgetMeNote stores evidence and versioned notes. Optional integrated chat and connected external agents can reason over authorized originals; model inference runs through an operator-configured service. Ordinary editing and literal search work without embeddings or an agent. There is no embedded model runtime or vector database service.

## Chat passage retrieval

Chat uses committed-passage BM25 plus available semantic ranks. BM25 removes a small English vocabulary of question framing and function words, so words such as “tell,” “me,” and “the” cannot alone select an unrelated note. Unicode words remain unstemmed; exact titles/filename stems and explicitly quoted names retain their original matching behavior. Sidebar Text and `search_notes` still use literal substrings.

Query embeddings have a maximum 30-second wait, including the endpoint gate, and always honor cancellation. Chat additionally caps that wait at one-third of the remaining run time so inference leaves room for an answer. If the wait expires, retrieval uses topical BM25 matches with an unavailable-embeddings status. It does not retry a timed-out endpoint or switch providers.

Initial chat retrieval can try one deterministic focused query only when a successful semantic query returns no candidates and removing question framing produces a different query. This attempt shares the unused original embedding allowance. Nonempty results, BM25-only retrieval and unavailable embeddings do not trigger this recovery. It adds no model-generation request or exploratory tool round.

A `weak` result means selected originals do not cover every substantive query term. This is a lexical coverage check, not a confidence score: paraphrases may have useful semantic candidates without literal overlap, and a matching topic note may lack the requested detail. Chat includes this limitation in context; missing matches cannot establish absence from the notebook. Complete selected passages and pinned revisions/whole-note hashes reach the existing shared request budget; fitting can mark or omit evidence when that actual budget requires it. Durable originals and tool receipts remain intact. Weak-topic warnings also survive tool-result fitting.

Semantic coverage checks every required passage hash. Identical passages may share a vector without making the index incomplete; unrelated cached vectors cannot compensate for a missing required hash.

The server emits one structural `chat retrieval` log for each initial/tool retrieval: stable chat/run and configured connection IDs, effective mode, fixed semantic outcome, elapsed milliseconds, query/passages counts, evidence-token estimate and weak-coverage flag. `ready` identifies a completed query embedding; `timeout` and `unavailable` identify fallback without claiming a particular outage cause. Logs omit queries, note paths/text, endpoints and raw errors.

## Browser

- **Sources** opens the current note's outgoing Markdown references and **Referenced by** links. A recorded citation opens its historical Markdown without replacing the editor or its draft.
- **Review** shows agent findings in the existing note discussions, proposals with expandable line diffs (complete before/after views for large notes), decisions, and the last externally reported consumer checkpoints. “Source changed” is a mechanical content comparison, not evidence of a contradiction. An open subscription does not mean that a review is running.
- **Notebook settings → Instructions → Agent instructions** reads or edits the optional root `AGENTS.md`. Owners and Admins manage it; generic note saves cannot overwrite it. Instructions are portable Git content, subject to locks. Imported notes, quoted text and ordinary document bodies remain untrusted source data.
- The sidebar has one search box. **Options** reveals **Automatic · Text · Meaning**, historical scope, and folder/tag/status filters. Automatic is the default: it ranks independent keywords with the same committed-passage BM25 index chat uses (one result per note, at its best passage), follows those with literal substring matches so partial words still match while you type, and blends that with semantic rankings when available. Text matches the literal phrase; Meaning searches the indexed passages through the server embedding endpoint. Results show unique notes in ranked order, with paths and short excerpts. When the instance enables **Search reranking** and you have consented, the status line offers **Press Enter to rank with judgments**: Enter sends one judgment request for up to 32 leading current-note results, each represented by the passage that ranked it, and the current results stay visible until the ranked order arrives. Typing never asks for a judgment, and Text mode, previous-version search and local-only installations never do. If the judgment fails, the local order remains with a message; changed notes or revoked access fail the request instead of showing a stale order. Clearing the query returns to the folder tree. Active non-default options stay visible as removable chips, including when Options is closed.
- **Notebook settings → Search defaults → Index status** shows progress/errors. Admins configure indexing in **Administration → AI features**. Meaning is unavailable in the browser without a configured query endpoint. Automatic still returns text matches with an explicit status if embeddings are off, unavailable, or not indexed; partial indexing is also shown.

### Searching history

Search starts with current notes. Under **Options**, enable **Include previous versions** to search removed paragraphs, renamed or deleted notes, and earlier wording with the same Automatic/Text/Meaning modes. History includes every ancestor of the current Git HEAD, including merged side branches and merge resolutions. It does not include unmerged branches, dangling commits, or unsaved drafts. Path, tag and status filters use the metadata of the historical version; historical filter values become available as the scan discovers them. Trashed notes require **Include trash** in addition to **Include previous versions**. Notes removed through **Empty Trash** remain excluded even with both options enabled; their old Git commits and backups are retained. See [Trash and restore](trash.md).

Historical results group identical passage text across revisions and paths. Each result retains its actual committed path, revision, note hash, citation and recorded occurrences. Unchanged commits do not add duplicate occurrences. A phrase crossing passage boundaries, or matching only note metadata, uses an explicitly note-level citation instead of claiming an unrelated passage hash. The result's `current` flag means its selected whole-note version matches the current note at that path. Commit dates describe recorded versions, not the exact period during which a paragraph existed.

Opening a historical result shows a read-only dialog with Copy passage/Markdown, Copy citation and Compare with current note. When another note is open, it also offers Compare with open note. Comparisons read committed text and leave the editor and its unsaved draft intact. A moved or deleted path remains readable at the pinned revision; the dialog reports when no current note exists at that path. Opening a result in another tab preserves its revision. Restoring or saving old text is a separate explicit editing workflow.

History starts indexing on the first historical search or indexing request. Initial requests scan bounded batches and report partial coverage; the existing worker continues in the background, including when embeddings are disabled. A persisted within-commit cursor resumes large imports after restart. Later commits scan only new reachable history, while existing blobs and compatible passage vectors are reused. A rewritten/nonancestor HEAD invalidates the old catalogue before results are served. History scanning does not commit, reset, merge or change notebook content.

The derived `cache/history.json` catalogue is separate from durable configuration. It is safe to remove while the service is stopped; the next history request rebuilds it from the notebook's history. `FMN_LIMIT_HISTORY_BYTES` (64 MiB by default) bounds unique note text and derives metadata scan/occurrence budgets. Reaching a bound or encountering an oversized/invalid historical note reports incomplete results without breaking current search. Embeddings share the configurable index budget. Result provenance reports `occurrenceCount`; at most 256 occurrence records are returned, with `occurrencesTruncated` identifying a shortened list.

The embedding worker starts with batches of up to 32 distinct passages. When an endpoint rejects or times out a multi-passage batch, the worker halves that batch size and retries the still-pending passages, retaining the smaller size across engine eviction for the instance processing lifetime. Single-passage failures remain visible and retry periodically; they are never silently marked indexed. This lets slower local models make progress without increasing request timeouts or changing the embedding model.

### Historical API and MCP

`POST /p/PROJECT/api/knowledge/hybrid-search` and MCP `hybrid_search` accept `scope: "history"` in the existing query object. Omitted scope, or `"current"`, preserves current-note search. Example body:

```json
{"scope":"history","mode":"text","text":"removed wording","limit":20}
```

The response includes `scope`, effective `mode`, `message`, matches and a nested `history` status with current/target/completed revisions, scanned/total commits, versions, unique passages, indexed vectors and state. `scanning`, `indexing`, `limited` or `unavailable` require checking the accompanying coverage/message; a scan-complete catalogue can still have missing vectors. Historical Text works without embeddings; All falls back to Text on endpoint unavailability. Meaning returns only indexed passages and reports incomplete indexing.

For agent-produced vectors, request `GET .../knowledge/passages?scope=history` or call `get_indexing_passages` with `scope: "history"`. Pass the returned status revision/configId and passage id/hash/noteHash back to `submit_embeddings` with `submission.scope: "history"`. Historical passage IDs include the note-version identity. Immutable historical submissions may survive ordinary descendant commits, but rewritten ancestry or changed model configuration rejects them. The same passage content is requested once across current/history indexing for one active configuration. `semantic_search` also accepts `query.scope: "history"`; use the status endpoint or hybrid response to inspect partial coverage.

Historical search, status and pending passages require project Read permission; vector submissions still require Edit, and configuration still requires Owner/Admin. Browser and MCP requests use the same engine and recheck access on each request.

Optional frontmatter conventions are `kind: source`, `kind: human-note`, and `kind: maintained-summary`, plus the existing `status` and `tags`. These do not impose folders or grant permissions. To find unprocessed sources, inspect snapshot metadata or changes after your ingest consumer checkpoint and select the source kinds/tags your project instructions declare.

For a manuscript project, Owners can state in `AGENTS.md` that manuscript wording must remain human-authored, source priorities must be observed, and new ideas must stay provisional. Instructions guide the external agent; use note/folder locks for server-enforced protection. No real manuscript content is included in the implementation or test fixtures.

## Semantic search setup

Configure an operator embedding endpoint, then enable **Background embeddings** in
**Administration → AI features**. This policy covers existing and future notebooks.
Choose the exact model and vector dimensions; changing an existing model requires
rebuild confirmation. Notebook settings show index status without a separate
configuration form. See [instance AI controls](ai-features.md) for permissions,
privacy boundaries, API/MCP actions and legacy migration behavior.

## Embeddings: choose either or both paths

There is one index and one active embedding configuration per project. Compatibility includes the exact model identifier, dimensions, `markdown-heading-v1` splitter and maximum passage characters. Changing those settings requires an explicit rebuild, including while the index is paused. Endpoint-managed `embeddinggemma` models also version their document/query preprocessing; switching these models between endpoint and agent-produced vectors requires a rebuild. Other models retain the same compatibility identity when endpoint use changes. Rebuilding also works with an unchanged configuration.

### Operator-managed endpoint

Set `EMBEDDING_URL` to one of these complete URLs:

- OpenAI: `https://api.openai.com/v1/embeddings`. Set `EMBEDDING_SECRET_FILE` to a mounted file containing the OpenAI API key. For example, mount the Kubernetes Secret key `OPENAI_KEY` at `/run/secrets/providers/OPENAI_KEY` and use that path. The secret key's name is arbitrary; the application reads the configured file.
- Ollama: an **`/api/embed`** URL such as `http://embedding-host:11434/api/embed`. Set `EMBEDDING_SECRET_FILE` if the endpoint requires a bearer credential.

Compose exposes both variables; add the read-only secret mount if needed. The address and credential are private server configuration, never note content, project configuration or a tool argument. Redirects and URL credentials are rejected. Use HTTPS across untrusted networks.

Have an Admin enable **Background embeddings** in **Administration → AI features**. For OpenAI, use model `text-embedding-3-small` with **1536** dimensions, or `text-embedding-3-large` with **3072** dimensions. The client requests the configured dimensions for these models, so reduced dimensions are also supported. OpenAI requests use `encoding_format: "float"`, and response indices associate each vector with its input. See [OpenAI's embeddings API](https://developers.openai.com/api/reference/resources/embeddings/methods/create).

For Ollama, prepare the model yourself; ForgetMeNote does not install a model or embedding container. Requests contain `model`, an array of `input` strings, and `truncate: false`. Both transports require one finite, nonzero vector of the configured size per input. See [Ollama's embed API](https://docs.ollama.com/api/embed).

For `embeddinggemma` and its tagged versions, the endpoint adds Google's [retrieval prefixes](https://ai.google.dev/gemma/docs/embeddinggemma/model_card#prompt-instructions): `title: none | text: ` for documents and `task: search result | query: ` for queries. That input contract is part of the vector configuration identity, so older unprefixed vectors are rebuilt. Original passage text and citation hashes remain unchanged. Other model names keep their existing input format. Agent-produced vectors retain their caller-selected model identity.

Committed changes wake one fair instance indexing processor. It performs one bounded notebook batch before yielding to the next notebook; a notebook does not retain its own background goroutine. Inputs are a fixed Git snapshot, never unsaved working-tree content. Model requests run outside the Git write lock, have a 30-second HTTP timeout and one shared in-flight request per configured endpoint. Failures leave the previous index intact; commit hints and a 30-second retry/refresh attempt retry missing work. The status reports unavailable/model/batch failures without returning remote diagnostics or credentials. Restart cancels outstanding model work. Browser queries use the same configuration and validation as indexing.

### Agent-produced vectors (existing API consumers)

1. Read `knowledge_status` and use its exact configured model and dimensions. Before instance-policy adoption, existing Owner/Admin clients may still configure an agent-only index through `configure_semantic_search`; after adoption, only the Admin global policy selects the model.
2. An authenticated external agent calls `get_indexing_passages`.
3. The agent sends the exact returned passage **text** to its actual embedding model service. Do not invent vectors or silently add instructions/prefixes to the input. Model deployments that require different document/query preprocessing must use a compatible endpoint contract and an explicit distinct model identity.
4. Submit the returned values through `submit_embeddings`, including `revision`, `configId`, and each passage's `id`, `hash`, and `noteHash`.
5. For retrieval, embed the query with the compatible model and call `semantic_search` with `query.vector` and `query.configId`. Document vectors alone cannot provide browser query embeddings.

Both paths use the same vector validator and cache. Structural checks cannot establish which model produced a vector: shared-index submission is an authorized **Edit** write. Readers may request passages and search, but cannot modify the index. Models with mutable tags should use a new explicit configuration identity when the deployed weights change.

Text-only semantic requests fail clearly when there is no compatible server query endpoint; `search_notes` always supplies literal retrieval. `semantic_search` accepts `literal`, path prefix, tag, and status filters; all filters are applied before snippets are returned. Matches include path, heading, excerpt, snapshot revision and a reproducible citation. Results are from the indexed portion of the current committed snapshot; pending passages are omitted, not silently searched using old content.

## Hybrid ranking and MCP

Browser requests and the MCP `hybrid_search` tool use the same engine. Send `query.text` and optional `query.mode` (`all`, `text`, `meaning`), `tag`, `status`, `path` prefix, `literal` filter, and `limit` (default 20, maximum 50). An agent without a server endpoint can also supply a real compatible `vector` and `configId`, along with the text used to generate it. Mismatched or malformed supplied vectors fail validation. The response contains `matches`, the effective `mode`, and a `message` for fallback or incomplete indexing. Each match includes its path, title, excerpt, revision, and ranking score. Scores are ordering values, not confidence percentages.

The text branch uses case-insensitive literal substring matching in the path, title, or committed content. It ranks exact titles and filename stems first, then title/path matches, then body matches, with deterministic path ties. It is deliberately a simple lexical ranker, not BM25. The meaning branch uses each note’s highest-scoring passage, deduplicating **before** limiting candidates. All merges the top 50 unique notes from each branch with equal-weight reciprocal rank fusion: `sum(1 / (60 + rank))`, starting ranks at 1. Exact title or filename-stem matches retain first priority in All. Meaning keeps semantic order without that override. No score threshold is imposed; low-ranked semantic results may be weak matches.

All allows up to two seconds for the semantic branch before falling back to text. Explicit Meaning uses the endpoint’s existing 30-second timeout and reports unavailability without substituting text. Search requests are canceled when the browser query changes. If notes change during model inference, the engine returns current text matches and asks for another meaning search rather than combining rankings from different snapshots. Unindexed notes remain eligible for text matching.

Existing `search_notes` (literal retrieval) and `semantic_search` (passage-level semantic retrieval and citations) remain compatible. `hybrid_search` requires only project Read permission and a read-scoped MCP token; permissions are checked on every call. No search mode changes content, configuration, or permissions.

## Storage and bounds

```
/data/projects/PROJECT/
  cache/history.json      # derived history catalog, rebuilt when missing
```

Notebook content itself (Markdown, media, locks and comments/findings, with
their history) is in the instance database and blob store
([notebook content](postgres.md#notebook-content)).

Each notebook's knowledge state lives in Postgres, keyed by notebook ID, so every
gateway reads what any of them wrote: the `knowledge` document holds the
configuration, active proposals and decisions and consumer checkpoints;
`knowledge-retrieval` holds retrieval settings; one `knowledge-proposal`
document per archived proposal keeps completed proposals and retry identities;
and `knowledge_vector_sets` with `knowledge_vectors` hold the derived vectors.
The note map and history catalog stay in each gateway's cache directory. See
[vector storage](#vector-storage).

Backlinks and parsed passages are in-memory derived data. They rebuild from Git on refresh/restart. Vectors are normalized float32 values keyed by passage content hash, with configuration identity and indexed revision. Unchanged passage text is reusable across note revisions and paths. Snapshot reads, passage IDs and note hashes reject stale submissions. A cache can always be deleted and rebuilt; a review checkpoint does not move when indexing finishes.

`FMN_LIMIT_INDEX_BYTES` (512 MiB by default) bounds committed snapshots, passage metadata and vector-cache serialization. Scoped snapshots can scan up to four times that budget before selecting eligible notes. Vector accounting retains the conservative 16 bytes per value reserve for transient representations and compatibility; this is a resource estimate, not a process memory guarantee. There are no fixed note/passage counts or four-million-vector ceiling. Embeddings support 1–4096 dimensions and 128–4096 characters per passage; ingestion remains paginated with up to 64 vectors and 131,072 vector values per batch. Request at most 32 missing passages through MCP; split large-dimension batches further if needed. Search returns up to 50 matches. The binary cache streams float32 values with a checksum rather than rewriting JSON floats. Larger budgets require corresponding host memory and disk.

There are no new module or frontend dependencies. Parsing uses the existing Goldmark library; MCP transport uses the existing Go SDK. Similarity is a local exact cosine scan. A separate approximate-nearest-neighbor index is not justified without measurements beyond these limits.

## Sources and citations

Use normal Markdown links, including reference-style definitions:

```markdown
[Evidence](../sources/experiment.md#measurements)
[Recorded evidence](../sources/experiment.md?fmn-rev=FULL_40_CHARACTER_COMMIT#measurements)
```

Use a real commit from `read_project_changes` or `read_note`, not the placeholder above. Semantic passage results supply a ready-made URL with `fmn-rev`, `fmn-passage` (content hash), and `fmn-chars`. Ordinary live links cannot tell which version an earlier author relied on.

Comparison uses a unique recorded passage when possible, otherwise the referenced heading, otherwise the whole note with the broader scope displayed. A missing recorded version or unavailable/excluded path is explicit. Historic URLs retain their original paths and revisions: a renamed source may need a new live citation while the old snapshot remains inspectable. Markdown/reference links and [`[[Page title]]` page links](wiki.md#page-links) are parsed; raw HTML attributes are not resolved. External HTTP(S) URLs are displayed and never fetched. Backlinks refresh after committed edits, moves and deletions; read locks do not exclude content from authorized retrieval.

## Agent workflows and permissions

Connect your existing MCP client to `/mcp` using ForgetMeNote's user-bound OAuth flow. Every tool call rechecks the account and project role. A token's write scope also constrains mutations. Provider login/GitHub linking remain separate from project access.

| Work | Minimum project role |
| --- | --- |
| Configure shared AI through `set_ai_settings` | Instance Admin + management scope |
| Legacy `configure_semantic_search`, before instance-policy adoption | Owner/Admin |
| Read instructions, snapshots, changes, sources, passages, index status, proposals/findings, own checkpoint; semantic/literal search; subscribe | Read |
| File a finding or propose a group; resolve/dismiss one's own finding; dismiss one's own proposal | Comment |
| Supply shared-index embeddings, apply/accept groups, resolve/dismiss others' findings/proposals, record workflow completion | Edit |
| Change legacy embedding configuration before instance adoption, or agent instructions; existing move/rename/duplicate and lock controls | Owner (or Admin) |

A proposal never grants permission to apply its edits. Acceptance rechecks locks and all base revisions. A denied or conflicting proposal remains reviewable. Instructions are intentionally managed through the Owner browser control; MCP discovers them read-only.

Recommended manual flow:

1. `list_projects`, `knowledge_status`, `project_instructions`.
2. `consumer_checkpoint` with a distinct stable `consumer`, such as `ingest.sources` or `review.continuity`. This returns that user's checkpoint and changes since it.
3. `read_project_changes` with `snapshot: true` for metadata/hashes when needed; read documents with `read_note` at the returned snapshot revision. Use `get_note_sources` for stale citations and backlinks, and semantic/literal search for related notes. Never let imported content override the project instructions or the user's task.
4. Reason externally. Ingest may suggest cited updates; query can answer with evidence and save only when requested; review can identify possible contradictions, unsupported claims, duplicate concepts, missing links, and stale summaries. The server does not make those judgments.
5. `submit_change_group` with `mode: proposal` or `mode: apply`. Supply a stable ID, short summary, and 1–50 edits with path, content, base revision and SHA-256 of the **complete original Markdown bytes**. New files have empty revision/hash; deletions set `delete: true`. Limit the group to 4 MiB.
6. Alternatively `file_review_finding` with a stable key, title, explanation, affected note revision/hash, and optional local or external evidence. Findings are existing comments with review metadata and idempotent IDs. The Review inbox lets authorized users inspect, resolve or dismiss them.
7. After successful processing, `complete_workflow` with consumer, previous checkpoint, processed revision and a truthful summary. Completion is externally reported, not server-certified. Do not mark a review complete merely because embeddings were produced or a notification arrived.

Each user's named consumer is independent. Checkpoint updates use compare-and-swap, with identical retries accepted. If Git history is missing or no longer ancestral, the change API sets `freshReview` and supplies a reason; do a deliberate fresh review before completing with `reset: true`. Change paths are authoritative for the interval; provenance includes up to 256 relevant commits and an explicit `commitsTruncated` flag. Do not assume truncated provenance proves there were no other writers. Changes report visible Markdown and attachment paths; internal comment/lock files are omitted. Attachment binaries are versioned as before but are not embedding inputs.

A direct group is one revision, a Git commit committed in one transaction. The authenticated user is author, ForgetMeNote is committer, and browser/MCP plus the registered OAuth client ID are shared attribution trailers. Client display names are not trusted identity. Retry receipts are recorded in the commit's trailers, not a second change log. Proposals preserve the original proposer identity/client and record their accepting user in durable state; the accepting user authors the applied commit. A multi-file operation commits whole or not at all, so nothing is left to recover. Conflicts are errors, not permission to overwrite.

## Notifications and external scheduling

The standard resource URI is `forgetmenote://projects/PROJECT/revision`. Authorized resource lists show only accessible projects. Reading returns the current committed revision; subscribing requires current Read access. Subscription limits are 32 projects per session, 128 subscribed sessions per gateway, and one bounded/coalesced project hint queue. Notifications contain the standard resource URI only (plus SDK-required subscription metadata in the newer protocol); they never carry note text or private paths.

Negotiated transport:

- **2025-11-25 legacy clients:** `resources/subscribe` / `resources/unsubscribe`, with standard `notifications/resources/updated` on Streamable HTTP's GET SSE stream. The session is bound to the exact bearer credential, and has a five-minute idle timeout. It is stored in Postgres, so any gateway serves it and a deploy does not end it; a stream on a gateway that shuts down is asked to reconnect. DELETE closes it. A transient GET disconnect may leave the logical subscription until reconnect, DELETE or expiry.
- **2026-07-28 clients:** the SDK uses `subscriptions/listen` and its acknowledgement on a long-lived POST SSE stream; canceling that stream releases its subscriptions. Discovery and non-subscribing tools stay stateless for this protocol. No custom WebSocket or event schema is used.

The existing SDK handles protocol negotiation and required metadata. [MCP resources](https://modelcontextprotocol.io/specification/2025-11-25/server/resources), [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports), and [stateless subscriptions](https://modelcontextprotocol.io/seps/2575-stateless-mcp) describe the relevant contracts. Clients that never subscribe continue using tools normally.

Hints are emitted only after a successful Git commit. Burst hints coalesce over approximately 100 ms; delivery has bounded time rather than an unbounded per-client queue. Slow or invalid sessions are closed. Credentials/roles are rechecked before delivery and active subscriptions are swept every second for revocation, expiry or disabled accounts. Subsequent reads/writes independently check authorization. Out-of-band **committed Git changes** are detected by the normal HEAD refresh, also run every five seconds while a project has listeners. Uncommitted filesystem edits do not create content notifications and still block writes.

A listening consumer should:

1. Establish its subscription, then fetch its checkpoint and current changes (this order closes the subscribe/read race).
2. Coalesce hints into “work may be pending”; fetch Git changes after its last successfully processed revision.
3. Run only its declared external trigger policy. Use commit source/client/user provenance and stable operation IDs to avoid processing its own completed output again. Do not globally ignore MCP commits; other users or agents may need them.
4. Commit/propose/file findings with stable retry IDs, then record completion. If processing itself changes content, explicitly include that known output revision in the checkpoint only after checking no unrelated input arrived during the run. Otherwise retain the input snapshot checkpoint and filter known outputs on the next run.
5. On reconnect or restart, repeat the checkpoint query. Missing, duplicate or delayed notifications cannot lose work because Git owns catch-up. Receipt of a hint never advances a checkpoint.

For periodic use, an operator's cron job or an agent product's scheduler can run exactly the same checkpoint workflow without subscriptions. For manual use, ask the connected agent to ingest or review a named project. ForgetMeNote neither launches agents nor knows whether any particular client product will wake an agent after an event. No “Ask agent” control or claimed wake-up integration is provided.

### Reverse proxy

Forward `/mcp` and OAuth discovery/authorization/token routes to the app, retaining Authorization and `Mcp-Session-Id` / `MCP-Protocol-Version` headers. The MCP and OAuth public routes must bypass any proxy's interactive-login redirect; ForgetMeNote performs their own OAuth checks. Preserve the configured public Host/Origin.

Allow streaming for both GET and POST. Disable response buffering and proxy caching on `/mcp`, and use a long read timeout. For nginx/SWAG's `/mcp` location, use `proxy_buffering off; proxy_cache off; proxy_read_timeout 1h;` alongside the existing forwarding configuration. For Caddy, `reverse_proxy` supports SSE; `flush_interval -1` makes flushing explicit. Configure HTTP keepalive/timeouts to permit long streams. The app removes its ordinary write deadline only for authenticated MCP traffic; notification writes have their own bounded context. A proxy disconnect requires catch-up and resubscription, not a guarantee of delivery.

## Backup and restore

Stop the instance before a full backup. Back up the database (catalog, accounts and permissions, OAuth/browser sessions, knowledge state and every notebook's revisions, comments and live documents), the blob store (content blocks and media) and the **entire `/data` volume** together ([complete instance backup](backups.md#complete-instance-backup)); also back up deployment configuration and mounted secrets separately with restricted access. Restore into an independent database and volume and run one instance on them. Ordinary Git remotes and the project's Git bundle contain content/history/comments, but **not** account/permission state, consumer checkpoints or unaccepted proposals.

Back up the database with the volume: it holds knowledge state, retrieval settings, archived proposals and the computed vectors. If vectors are deliberately dropped, reconnect the authorized embedding source and supply missing vectors; that regeneration can require new provider work. Backlinks/passages rebuild automatically. This must not erase review decisions or advance consumer checkpoints. Restore the database from the same point as the volume and blob store when restoring the full instance. Each notebook change is one transaction, so there is no interrupted write to recover on startup.

Export tools and existing permissions are unchanged. Do not edit notebook rows, export repositories or other durable state by hand.

### Disposable protocol verification command

`scripts/mcp-smoke` is a small SDK client, not an agent runner. Against **disposable data only**, run:

```sh
go run ./scripts/mcp-smoke -endpoint http://localhost:18491/mcp \
  -project default -token-file /private/path/to/disposable-token -write
```

Add `-legacy` to exercise 2025-11-25. Without `-write` it only reads instructions and subscribes/unsubscribes. With `-write` it creates a synthetic note in one group, receives its resource update, checkpoints that known output, and checks that the checkpoint does not trigger another content event. It reads credentials from a private file and never prints them. Normal consumers should use their own user-bound OAuth token. This test proves protocol delivery; it does not prove that Codex or ChatGPT starts agent turns on notification.

Durable workflow bounds are currently 200 active/unarchived proposals, 1,000 consumer checkpoints and a 32 MiB workflow-state file per project; discussions and the Review inbox each limit findings/comments to 1,000. The server returns an actionable limit error instead of discarding durable decisions. Back up and assess collection size before adopting high-frequency automated review.

Editors can archive completed proposals in Review or with `review_decision` (`decision: archive`). Full records remain in the instance backup; the active inbox omits them. See [retention and recovery](production-readiness.md).


Container-aware searches use ordinary notebook defaults Archives **Lower priority**
and Drafts **Include**, with explicit per-search overrides in Search options.
Lower priority is a strict tier before branch/candidate/evidence limits, not a
score multiplier. History applies each committed version's own classification;
excluded versions cannot supply snippets, facets or provenance through an identical
passage hash. The derived history cache format is version 3 and reparses validated
content for classification. Existing vector hashes/model configuration are unchanged.
A bounded partial scan remains partial even with no eligible results; use the
structured hybrid result for its history coverage. See [collections](containers.md)
for settings, bindings, browser scope and limits.

## Browser Review and search

The permanent **Review** sidebar destination groups **Findings**, **Proposed changes**, **Notebook checks**, and **Activity** in one dialog. Its count reflects unresolved findings and pending groups; an unavailable count is shown explicitly. Notebook checks use the existing judgments consent, notebook authority, and review actions.

Search results appear in the main reading area with plain Markdown-derived excerpts and secondary paths. **Folder**, **Tag**, and **Status** are the first filters; matching, history, Trash, and content ranking live in **Advanced options**. **Back to notes** restores the installed editor and its draft. Readable excerpts are display-only; evidence text, revisions, hashes, and citation identity remain intact.

## Index lifetime and recovery

Requests acquire an explicit engine lease and release it after their last use.
Idle derived corpora are retained in an LRU bounded by
`FMN_LIMIT_IDLE_ENGINES` (default 4) and `FMN_LIMIT_IDLE_INDEX_BYTES` (default
64 MiB). The byte value is a conservative estimate including projections,
writing arrays, passages, vectors and lexical postings, not measured heap usage.
Active and nested requests remain protected even when they exceed the idle
budget; request admission bounds concurrent work. Shutdown rejects new leases,
cancels and joins processing, then drains active requests before closing caches.

Eviction keeps only operational recovery categories, history intent and learned
batch capacity. Durable configuration and compatible vectors remain on disk.
The processor starts after instance configuration, retries failures every 30
seconds even during continuous work or an active foreground lease, and preserves
notebook-local settings failures without blocking ordinary notebook startup.

A cold, fully processed notebook retains an advisory revision/file checkpoint.
Periodic refresh checks the current notebook revision, visibility, configuration/cache
file metadata, instance policy and permitted endpoint before skipping a heavy
reopen. Commit hints, history requests and configuration changes invalidate it.
The checkpoint supplies no search results or authorization; every caller still
acquires an engine and passes current permissions and visibility checks.

An unchanged complete vector cache is read without being republished on reopen.
Primary-state writes enforce the same state budget as reopen and preserve the
previous state on rejection. Reads check the file budget before allocation and bind file identity to the
actual descriptor read. A vector or history publication failure remains a
recovery requirement across eviction, including a reported final directory-sync
failure. Success requires another durable publication. The former vector files
are described in
[storage versions and recovery](storage-migrations.md#semantic-vector-version-2).

## Vector storage

A notebook's vectors are one `knowledge_vector_sets` row, naming the
configuration, indexed revision and dimensions, and one `knowledge_vectors` row
per SHA-256 passage content hash holding its little-endian float32 vector. The
set's version changes on every write. An engine that last read or wrote the set
adds and removes only the rows that changed, so a batch of 64 vectors writes 64
rows rather than the whole cache; any other engine rewrites the set. Before it
reads or writes, an engine adopts rows another engine added since the version it
knows, and rereads the set when rows were removed, so no gateway asks to embed
vectors another stored or writes a set without them. A rebuild keeps the set row
with an empty configuration, so the version never restarts. The same
model/splitter/configuration identity, content reuse, ranking and citations
remain authoritative.

A stored vector that fails validation fails closed for this notebook's retrieval
and is retained for diagnosis. Ordinary notebook reads and other notebooks
remain available. Restore the database from a backup, or rebuild the index to
authorize regenerating derived coverage.

The former `cache/semantic.bin` version-2 file and legacy `semantic.json` are no
longer read when serving; `forgetmenote import-files` brings them across once.
`admin rollback-semantic-storage` is retired: a file-based release cannot read
vectors from Postgres.
