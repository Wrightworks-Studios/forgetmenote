# Repository-connected notebooks

Instance administrators can import an already-authorized Sources App repository
through `POST /api/v1/actions/import_repository` (or
`forgetmenote action import_repository --file import.json`):

```json
{"id":"source","name":"Source","installation":123,"repository":456,
 "shareWithNotebook":true,"externalProcessing":false}
```

Use installation and repository IDs from the configured App. The server verifies
current App access before creating a read-only notebook. Repeating the same request
resumes it; this cannot change an ordinary notebook's authority or substitute a
different source. It requires an instance Admin, management consent and an
unrestricted administrative credential. Notebook Owners, notebook-scoped credentials
and embedded chat cannot use the instance App this way. The normal GitHub
user-authorization flow remains available in the UI.

Connect one read-only GitHub source repository to a notebook, browse its default branch locally, and retain exact source citations. Ordinary notes and the connected repository have separate Git histories. This integration never pushes code, runs repository code, or changes an external wiki.

Repository-wide keyword search, optional incremental embeddings, and source-aware chat retrieve original evidence directly. See [architecture](architecture.md) for code ownership.

## GitHub App setup

Repository access is separate from the identity-only GitHub OAuth connection used for commit attribution. Reusing `GITHUB_CLIENT_ID` does not enable source access.

Create or configure a dedicated GitHub App with repository **Contents: read-only** and **Metadata: read-only**, installed only on selected repositories. No organization, administration, issues, or pull-request permissions are needed. Configure these addresses, replacing the origin with `PUBLIC_URL`:

| Setting | Address |
| --- | --- |
| Homepage | `https://notes.example.com` |
| User authorization callback | `https://notes.example.com/repository/github/callback` |
| Webhook | `https://notes.example.com/repository/github/webhook` |

Leave “Request user authorization during installation” off. Start authorization from **Notebook settings → Sources → Connect GitHub**, which establishes the user, notebook, state, and PKCE verifier. Keep expiring user tokens enabled. Subscribe to push and repository events; installation events also prompt access revalidation. Webhook signatures are required and delivery IDs are deduplicated. Polling checks the actual default branch every five minutes while available, so missed webhooks do not lose source changes.

For a local instance, use its exact `http://localhost:PORT` callback. GitHub cannot deliver webhooks to loopback; leave webhook delivery inactive and use polling or Refresh now. A webhook secret is still required by the local configuration. See GitHub's [registration instructions](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app) and [user authorization flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app). Organization SSO may require an active GitHub SAML session before authorization.

Mount the App configuration and secrets read-only, outside notebooks. Set `REPOSITORY_APP_CONFIG_FILE=/run/secrets/repository-app.json`:

```json
{
  "appId": "YOUR_APP_ID",
  "slug": "YOUR_APP_SLUG",
  "clientId": "YOUR_APP_CLIENT_ID",
  "clientSecretFile": "/run/secrets/repository-client-secret",
  "privateKeyFile": "/run/secrets/repository-app-key.pem",
  "webhookSecretFile": "/run/secrets/repository-webhook-secret",
  "limits": {
    "history": 100,
    "cacheBytes": 536870912,
    "evidenceBytes": 268435456,
    "fileBytes": 1048576,
    "files": 10000,
    "seconds": 120
  }
}
```

Use the App's PEM RSA key, client secret, and a random webhook secret of at least 32 characters. These limits are the defaults. Mounts must be readable by the container user; protect host copies. `compose.yaml` includes commented examples. Never put keys in notes, source URLs, Git config, or this source repository.

An Owner/Admin chooses from repositories the authorizing GitHub user can access and explicitly accepts sharing source with that notebook's members. Possession of an installation ID is insufficient. Installation tokens are ephemeral and restricted to the selected repository. Revocation stops future reads and processing; it cannot retract published prose or earlier exports.

## Browse and cite

**Notebook settings → Sources** shows the actual default branch, downloaded revision, last refresh, cache/evidence sizes, and errors. Status updates while the panel is open. **Refresh now** fetches source without creating a notebook commit; concurrent work gets an explicit busy response. **Pause** stops automatic refresh and **Disconnect** stops new source reads and refreshes. Notes and retained evidence are preserved.

**Browse source files** lists files at a fixed commit. Filter by file or folder name; the count and browsing revision remain visible. Background refresh does not change the commit behind that list. Opening a file displays its regular UTF-8 text. **Copy citation** retains original bytes before copying a full-commit GitHub permalink. Read members may inspect source; retaining evidence requires Comment or greater. Symlinks, submodules, binaries, and LFS pointers are not followed or downloaded.

A note's Sources panel recognizes code citations and offers **Inspect code**. Captured files survive cache rotation, upstream history rewrites, and repository renames. An uncaptured historical file may be unavailable. Reading retained evidence still requires a configured, connected source and current notebook/installation authority. Disconnecting preserves the evidence on disk; it does not bypass these checks.

Refresh rebuilds a shallow bare repository and atomically promotes it. The cache cap includes both previous and replacement caches, so leave replacement headroom. A filesystem monitor cancels oversized fetches; it is not an OS disk quota. A missing comparison checkpoint or changed branch requires a fresh baseline; it does not imply examination of missing history.

The existing MCP tools remain available: `repository_source_status`, `repository_source_tree`, `repository_source_file`, `repository_source_changes`, `refresh_repository_source`, and `retain_repository_citation`. Full-file reads retain the one-MiB default/four-MiB operator maximum. For larger text, pass both `startLine` and `endLine` to `repository_source_file`. A response contains at most 200 lines and 64 KiB; repeat the same coordinates with its `cursor` to continue, including within a long UTF-8 line. Cursors bind the source, commit, path and range. Missing historical bytes never fall back to HEAD.

## Search source and notes

In search, choose **Notes**, **Source**, or **Notes and source**. Source/Notes and source enables selection of up to ten readable notebooks. Source results show the repository, file, original line range and commit; opening one uses the read-only source viewer. Notes retain their existing editor behavior. Search does not copy source into Markdown or create notebook commits.

MCP clients use the explicit read-only tool:

```json
{"projects":["example-service"],"query":"database migration startup","mode":"auto","pathPrefix":"","limit":12}
```

Call `search_repository_sources` with those arguments. `projects` is required; `mode` is `auto`, `text`, or `meaning`. Text makes no provider requests. Auto fuses keyword and available semantic results; missing semantic coverage is reported. Meaning returns explicit unavailable/partial coverage instead of presenting keyword matches as semantic results. Exact paths, symbols and identifiers retain priority. Results contain original text, content hashes, byte/line bounds, repository identity, commit permalinks, match methods and per-notebook coverage. At most twelve excerpts, four per file, fit within a 6,000-estimated-token evidence allowance.

Browser and MCP use the same retrieval coordinator. `POST /api/retrieval` additionally accepts `corpora: ["notes", "repository"]` (either or both), with the same explicit notebooks/query/mode/path prefix/limit and existing note history/container preferences. Existing note-only `hybrid_search` is unchanged. Each selected notebook is authorized before provider work and again before delivery. A cross-notebook link or model tool argument never grants access.

## Index settings and processing

Keyword indexing is local and starts after a verified default-branch refresh. **Sources → Source search** exposes downloaded/indexed revisions, file/chunk counts, exclusions, embedded coverage, index bytes and errors. Owner changes use an expected settings generation, preventing stale settings from overwriting newer ones. MCP exposes the same owner-only operation as `configure_repository_index`.

Admins enable embeddings, search reranking and map classification in
**Administration → AI features** for existing and future notebooks. See
[instance AI controls](ai-features.md) for consent and legacy migration. Notebook
settings retain path filters and processing status. Embeddings use the operator
endpoint and chosen model/dimensions; no model or provider service is installed.
Search reranking judges relevance on at most twelve excerpts, in batches of at
most eight. Decision features do not generate documentation or citation coordinates.

Changed blobs are deterministically chunked; unchanged payload hashes reuse compatible vectors, including after restart. A question uses at most one embedding per compatible configuration (up to four configurations in a retrieval attempt). Background batches contain at most 32 payloads and yield to foreground demand at batch boundaries. Failed or unsavable batches stop until source settings are saved again. Request/payload-byte/chunk counters are available; payload bytes exclude transport prefixes and are not token or dollar estimates. Once pending work is complete, an unchanged refresh makes no document-embedding, judgment or large-model calls.

For the Ollama model `embeddinggemma` (including tagged versions), the transport applies Google's [retrieval query and document prefixes](https://ai.google.dev/gemma/docs/embeddinggemma/model_card#prompt-instructions). This preprocessing is part of the derived vector identity, so older unprefixed vectors rebuild instead of mixing with new queries. Original excerpts and citations are unchanged. Other model names retain their existing input format.

Tracked UTF-8 text includes code, tests, migrations, configuration, Markdown and Sphinx `.rst` sources. Nothing is executed or built. Go uses declaration boundaries, Markdown headings, and Python/RST conservative structural hints; other text uses bounded windows. Small adjacent declarations are grouped. Chunks target 1,600 Unicode characters with an 80-line/4,096-byte contextual-payload ceiling. Long lines retain UTF-8 byte coordinates.

Default exclusions cover binary/non-UTF-8 files, symlinks, submodules, LFS pointers, dependency/build directories, minified bundles and credential filenames such as `.env`, `.env.*`, `.pem`, `.key`, `id_rsa`, `id_ed25519`, `.npmrc`, `.netrc` and names containing `credentials`. Owner patterns are exact paths or Go path globs; a trailing `/` selects a directory. Explicit exclusions win over includes. Includes can override default filename/directory exclusions, but cannot enable non-regular files, binary contents or oversized blobs. Filename filtering does not inspect arbitrary source for embedded secrets; external processing therefore requires administrator enablement.

Indexed blobs follow `FMN_LIMIT_FILE_BYTES` (32 MiB by default). Serialized chunks, manifests and completed index generations follow `FMN_LIMIT_INDEX_BYTES` (512 MiB); there is no fixed 50,000-chunk ceiling. Disk room is also needed for replacement. A single cached lexical snapshot uses a 256 MiB estimated allowance; other repositories are searched sequentially. This is a cache-admission estimate, not a process memory limit. Excluded large files remain browsable through range reads. Narrow included paths when an index exceeds its limit. Only the current source snapshot is searchable; retained historical evidence remains separate.

## Incremental repository map

Every successful index refresh also builds a local navigation map, one entry per admitted text file. Entries contain the Git blob ID, directory component, up to twelve symbols, a structural role, and up to eight literal links to other admitted files. Known entry-point filenames and files referenced by a Procfile receive startup hints; build files, tests, configuration and documentation have their own roles. These are conservative navigation heuristics, not a call graph or proof that a command executes. No repository command, Sphinx build or documentation generator runs.

Unchanged blobs reuse map entries. Changed files update their entries; additions, removals and renames also recheck literal links. Structural role changes invalidate affected classifications. The map is derived from the same excluded-path policy and commit as the source index, and never enters the notebook's content Git history. Adding a map to an existing index preserves compatible vectors. A refresh still walks repository metadata and atomically publishes an index generation; incremental processing avoids repeated extraction and AI work, not all repository-wide I/O.

Read the map with `repository_source_map` or `GET /p/NOTEBOOK/api/repository/map`. Both accept `source`, `revision`, `query`, `pathPrefix`, `limit` and `after`; MCP additionally requires `project`. The default page has at most 24 entries, the maximum is 40, and serialized entries fit an 8,000-byte page budget. Follow `next` as `after` with the returned `revision`. A changed source revision, map classification generation or query fails explicitly rather than silently continuing a changed result set. Query terms rank paths, symbols and role aliases, with exact paths and explicit role names ahead of incidental matches. Use the resulting paths with `repository_source_file` or source search to obtain original evidence and exact citations. Structural roles also help local keyword retrieval find entry points when the user's wording does not occur in the script.

**Repository map classification** is an instance Admin setting. It uses the
configured decision provider under current instance policy and existing execution
limits. It classifies only files without a known structural role, using up to 1,400 source bytes, the path and symbols per file. First enablement processes the currently unclassified files; later runs reuse the decisions. Batches contain at most eight files and shrink to fit the configured request limit. A small fixed choice set includes an explicit “other” answer. Low-confidence and “other” results are cached as abstentions, without a human review queue. The relevance/confidence thresholds are provisional navigation filters, not calibrated guarantees of correctness.

Classifications are cached by file inputs and classifier/model version, including after restart; only affected entries are purchased again. The existing judgments ledger reserves each batch under a stable identity so an uncertain request is not silently charged twice. Settings changes, pause and lost authority prevent stale results from publishing. A provider failure leaves deterministic navigation and source search available; processing resumes on the next changed source revision or settings save. If that batch was already purchased but its result is unavailable, its entries cache an explicit `unavailable` decision, retain their local roles and allow subsequent batches to proceed. Unknown usage stays in the ledger. Status reports map entries, cached decisions (including abstentions and unavailable results), and a separate classification error. Inferred labels are map-search hints; they do not replace original source text or trigger embedding rebuilds. There are no background large-model calls or generated map descriptions.

## Ask about repositories

Chat settings offer **Repository notebooks (optional)**. The selected notebooks must allow external source processing because excerpts may reach the configured chat provider. Each run freezes notebook IDs and their actual source bindings. Tools may narrow that scope but cannot widen it, switch to a newly connected repository, read excluded files, or substitute historical source. Source citations include the exact commit and line range. Current note context keeps its existing saved container/history policy.

Source permissions, connection identity, consent, index exclusions and cited revision are rechecked before model requests and before delivery. Source tools include the repository map alongside search, tree and original-file reads, restricted to the selected notebooks. Source context from another notebook makes that chat run read-only for notebook mutations; publishing an explanation needs a separately authorized destination. Retrieval itself never writes a wiki or silently saves an answer. Code matches describe repository evidence, not current production database values, deployments or telemetry.

## Cross-notebook links

Both editor modes offer **Notebook link** with notebook/page/heading selection. Links are ordinary Markdown URLs. Titles, headings, backlinks, and resolution use current access; linking grants no permission. Historical references preserve original paths/revisions. Missing headings produce explicit errors.

Optional immutable `fmn-id` frontmatter preserves identity across renames. Browser-created notes receive IDs. Copies of identified notes receive new IDs. Imports preserve valid unique IDs and reject collisions. Existing notes and raw/MCP saves retain their byte contract and work by path unless an authorized client explicitly assigns a 32-character lowercase hexadecimal ID. Reading/selecting a note never rewrites it. No migration runs automatically.

Backlinks use a derived committed-Markdown index and recheck access on every response. Scans are bounded to 20 accessible notebooks and a 16 MiB request budget; incomplete results are reported. Cross-notebook source search has its own explicit selection, coverage and per-notebook authorization.

## Storage, backup, and upgrades

Gateway `/data/repository-sources/NOTEBOOK/SOURCE/` contains `source.git`, `index/current.json`, `index/generations/`, `evidence/blobs` and `evidence/refs`. Each completed index generation has a manifest, checksummed original chunks, a checksummed `map.json` (within the configured index budget), and compact float32 vectors. Map-only updates reuse chunk and vector files. Publication stores a complete generation before the notebook's source document names it, and readers pin one generation. Each notebook's `repository-sources` document in the instance database holds connection state, source-index consent/configuration/coverage, the Owner who enabled map classification, refresh checkpoints/retries, and webhook delivery IDs; every gateway reads it before acting, and a write over state it has not seen is refused. Include the complete tree and the database in stopped-instance backups alongside the catalog, notebook owners, collaboration, and existing knowledge/chat state. Index generations and retained evidence are durable: they are objects in the instance's blob storage under these paths, which with the default store are this directory's own files. Gateways with disks of their own fetch the generation the source document names, and the evidence they read, into their copy of this tree; `source.git`, `index/current.json` and other generations are rebuildable caches, so losing a gateway's copy costs a fetch, never a provider charge. Protect App/provider secrets separately. See [full backups](backups.md).

The source Git cache, derived search index and incomplete staging directories are reconstructible. Evidence, provenance and connection/consent state are durable data. Validate an independent restore before removing caches; runtime access still requires current notebook and GitHub installation authority. Rebuilding removed vectors can incur embedding charges when processing remains enabled.

After optional map classification purchases work, the shared judgments ledger contains
`repository-map` receipts. Older releases that do not recognize this feature cannot
open that ledger. Keep a stopped-instance backup before upgrading and use a
compatible image for forward repair; do not delete receipts to make an older image start.

A notebook Git bundle preserves Markdown, ordinary assets, Git history, IDs, and external links. It excludes separate source evidence, run records, cache, and credentials. Host-managed notebook backup pushes remain unchanged. Portable evidence export and multi-notebook export are not implemented; Markdown-only export is not a complete offline code-evidence backup. LFS remains deferred.

Identity-sensitive mutations carry `note-identity-v1`. Do not roll back to an identity-unaware writer after creating identified notes. Preserve current data and use compatible images or forward repair.

## Verification

Optional map classifications are not established to improve answers.

Hybrid retrieval is a local baseline with remaining evidence-discovery gaps. The former wiki generator commands and synthetic writing rubric are no longer part of the supported implementation.

## Unified notebook experience

New notebook offers Blank, Connect GitHub repository, and Import bundle. The GitHub creation grant belongs to the account and proposed notebook ID before content exists. Final selection creates one stable repository notebook; interrupted connections can be retried from Notebook settings. Existing mixed notebooks retain their notes and source bindings unchanged.

Repository files now open in the main reader: sanitized Markdown, syntax-highlighted read-only text, progressive line ranges, pinned relative links/images, and copyable citations. A refresh reports a newer revision without replacing the open document. Source files cannot be edited, renamed, deleted, or changed through collaboration/MCP.

`search_notebooks`, `list_documents`, `read_document`, and `notebook_map` share notebook/origin/revision references. Cross-notebook chat explicitly selects additional notes and repositories; tools cannot widen that selection. Maps for ordinary notes contain bounded titles, headings, tags, links and backlinks, derived deterministically from visible committed content. Structural refresh performs no AI calls. Optional embeddings and judgments stay opt-in. Meaning search reports missing vectors without silently substituting keyword matches.

For operator registration, manifests, and separate read-only Sources versus writable Backups Apps, see [GitHub configuration](managed-backups.md). New catalog state refuses older binaries after repository notebook creation; migration preserves a pre-upgrade catalog snapshot. Full-instance backups remain necessary before upgrades.
