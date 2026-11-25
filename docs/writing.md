# Writing tools

Open **Writing tools** from the editor toolbar, word count, or a note's **More**
menu. Repository Markdown has an entry in its read-only toolbar. The panel sits
beside the document on desktop and opens as a sheet on mobile.

**Overview** counts prose and shows the heading/scene outline, 100 sections at a
time. Each section has its own word count and a total including descendants.
Front matter, code, images, comments, link destinations and bare URLs are
excluded. Link labels and emphasis remain prose. Words are Unicode letter/number
sequences including internal apostrophes; languages without word separators are
estimates. Sentence counts use English punctuation heuristics; reading time
assumes 225 words per minute.

**Checks** detects adjacent repeated words, repeated spacing and spaces before
punctuation. Optional hints flag nearby word/phrase repetition, sentences over
35 words and paragraphs over 150 words. At most 200 findings are shown. These
are heuristics, not grammar judgments or automatic rewriting. A **Fix** appears
only for a literal replacement in an editable draft. It applies one undoable
edit and refuses stale content. Saving uses the normal permissions, locks,
collaboration and Git flow. While editing, a finding's passage is selected in
whichever mode is open.

**Terms** accepts one name per line, with literal aliases separated by `|`:

```text
Elizabeth Bennet | Elizabeth | Lizzy
Netherfield
```

Matches are case insensitive with Unicode word boundaries. They identify text
occurrences, not entities or inferred relationships. Up to 32 terms with eight
aliases each are supported; each name is at most 100 characters. Document scope
includes the displayed draft. **Notebook · Notes collection** searches committed
Notes only after pressing **Search**. Continue until **Search complete** appears.
Changing terms or scope cancels the search. Repository code is excluded;
repository Markdown is supported without write access.

To retain a term list, choose an explicit `.md` path under **Glossary note**.
The portable format is an ordinary Markdown note:

```markdown
# Writing glossary

- Elizabeth Bennet | Elizabeth | Lizzy
- Netherfield
```

Existing front matter is preserved. An unrelated note is never overwritten.
Revision conflicts, locks, repository authority and pending shared edits block
the save. There is no hidden database or new metadata field for terms.

## API, CLI and MCP

Both read-only actions use the shared action registry and document reader. They
work in headless mode without an AI provider:

```sh
forgetmenote action document_outline --file - <<'JSON'
{"project":"novel","path":"chapter.md","limit":100}
JSON

forgetmenote action find_term_mentions --file - <<'JSON'
{"project":"novel","terms":[{"name":"Elizabeth","aliases":["Lizzy"]}],"collection":"notes","limit":100}
JSON
```

HTTP uses `POST /api/v1/actions/document_outline` and
`POST /api/v1/actions/find_term_mentions` with the same JSON and a scoped API
credential. MCP exposes the same names and schemas. Set `origin:"repository"`
for repository Markdown and supply `source` and `revision` to pin a snapshot.

`document_outline` returns statistics, sections, total section count and numeric
`next`. Pass `next` as `after` with the same document reference and revision.

`find_term_mentions` accepts an optional `path`/`noteId` for one document, or
`pathPrefix` and the existing container selection for notebook scope. It scans
up to eight files and returns at most 100 matches per page. Pass its opaque
`next` as `after`, retaining the returned `revision`, `source`, terms and scope.
Only `complete:true` means the search has ended. Results include revision, file
hash, citation, UTF-8 byte offsets and one-based lines. Cursors reject changed
terms, visibility and notebook snapshots. Historical reads still require current
permissions.

## Performance

Draft analysis is debounced and runs in a browser worker without server or AI
requests. Changed content cancels obsolete work; every result is generation
fenced. The worker retains only the active document's projection. Server
projections have a bounded in-memory cache keyed by content hash and analyzer
version, with permissions checked anew. The compact retrieval map is unchanged.

Run `node scripts/writing_benchmark.mjs` for a 100,000-word local fixture, or
`go test ./internal/knowledge -run '^$' -bench Writing100KWords -benchmem` for
server extraction. These report measured times, not a universal latency promise.
Existing notebook snapshot/file limits apply. Incomplete repository reads return
an explicit error instead of presenting a partial count as complete.
