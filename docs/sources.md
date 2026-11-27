# Archived sources

Source processing stays bound to the selected notebook and current permissions.
A job's original, page checkpoints and result are private objects in the
instance's blob storage, so any gateway serves, resumes and retires it. Jobs
recover interrupted admission and retirement (see
[source job files](postgres.md#source-job-files)), and cancel execution by their
absolute 24-hour expiry. Expired work
remains cancelable while its processes drain. Saving accepts the existing result
into ordinary notebook Markdown, attachments and Git history.

The operator budgets `FMN_LIMIT_SOURCE_JOB_BYTES` (default 2 GiB) and
`FMN_LIMIT_SOURCE_JOB_COUNT` (default 100) bound all retained jobs, independently
of the one worker and four waiting slots. Queued and running jobs reserve space
for originals, conversion copies, encoded checkpoints/results and rendering
scratch; terminal jobs charge the objects they stored, each charged before it
is stored. Reaching these limits
refuses admission without deleting another user's pending work. Increase the
budgets explicitly when the instance's storage supports it.

Use **+ → Source** in the sidebar's creation dialog, or **Sources → Archive source** beside an external reference. Upload a supported document, image, saved email, or recording, or capture a public URL that serves one. The file picker and `source_formats` expose the current formats. Preview the extracted content, confirm the title, and choose **Archive source**. **Upload file** shows the file picker; expand **Source URL (optional)** to record where the file came from. **From URL** shows a required page or file URL to fetch. Switching to the **Note** tab and back preserves selected files and source inputs. An uploaded file remains recorded as an upload, not as a server-verified retrieval from its optional URL. Replacing an uploaded source preserves that URL in the form.

**Save in** chooses the source note’s destination from the project’s existing note folders or **Project root**. It starts in the folder you are viewing, or the current note’s folder. The name `sources` is not reserved. Refreshing a source keeps its existing path; use the normal Move action to relocate it. Original files continue to live under `assets/sources/`, independently of the note’s folder.

The original and a searchable source note are committed together using the authenticated user's Git authorship. Uploads and completed local pages stay in private processing jobs outside notebook repositories for at most 24 hours. Nothing is committed during preview. Preview signatures expire after 15 minutes; jobs and previews are bound to the user and project. Retrying the same confirmed preview does not create a second source.

After import, **Insert citation** adds a revision-pinned Markdown link to the current editable note's draft. Save that note to persist the citation. **Copy citation** works independently, and **Open source** opens the source note. Existing drafts are retained. No existing external reference is rewritten automatically.

## Portable storage and provenance

A new capture stores:

- `<chosen-folder>/<random-id>.md` (or `<random-id>.md` at the project root): ordinary Markdown containing provenance in flat `source_*` frontmatter and the extracted text.
- `assets/sources/<random-id>/<sha256>.<format-extension>`: original bytes, without sanitizing or rewriting them. HTTP captures preserve the decoded response body; they are not full HTTP/WARC archives.

Source records contain the original and final URL, UTC capture time, stable internal importer ID, capture method, MIME type, original SHA-256, extraction method/version, extraction status and warning, and extracted-text SHA-256. The extraction status is `extracted`, `partial`, `no-text` or `failed` for the server's own extraction, and `client-supplied` for text an agent uploaded with the original ([agent extraction](#agent-extraction)); then the extractor names that agent and model. Git history records authorship and browser/MCP origin. `status: unreviewed` is separate from successful extraction: change the note's status in Properties after reviewing its evidence.

Originals and text remain in the project's existing local Git repository, including Git bundle exports and full-instance backups. Repeated identical originals within the same source reuse the same file. A changed original gets a different hash-named file; earlier files and Git revisions remain available. Binary captures grow Git history, so the import limit is intentionally smaller than the general attachment limit.

Expand **Archived source** above the note to download the original, visit the original website, inspect provenance and checksum comparisons, or choose a page, heading, chapter, sheet, or row section and copy a pinned citation. The source can be renamed like an ordinary note; pinned citations keep their historical path and revision. History remains the authority for earlier names and content.

Hashes, extractor details, and verification results appear in collapsed **Technical details**. Successful verification stays quiet; detected changes and unavailable verification remain visible outside that disclosure and in the archived-source summary. Extraction warnings remain alongside the source actions.

Checksums detect differences from the recorded bytes. They do not establish factual accuracy, independent authenticity, or tamper-proof custody: people who can edit the repository can edit records, and those edits appear in Git. Editing extracted text causes its checksum comparison to report a change. Keep commentary and conclusions in separate notes linked to the source.

## New versions and citations

**Update source** fetches the URL again for a web capture, or accepts a new file for an uploaded source. Refreshing requires the existing source note's current revision and respects its locks. It replaces that source note's generated content and starts it as unreviewed; preview warns about the replacement. A refresh keeps the note's title unless it is given another. Keep annotations you want to retain in separate linked notes. Concurrent edits conflict instead of being overwritten.

Use a pinned citation for evidence that must refer to a particular capture:

```markdown
[Grammar, page 12](/sources/<id>.md?fmn-rev=<40-character-commit>#page-12)
```

Page numbers refer to physical PDF pages or image frames, not printed page labels. Presentation anchors use physical slide numbers, including blank slides. An agent's transcript of a recording uses numbered segments, whose titles may give their time ranges; these are not speaker labels or video-frame citations. Webpage and DOCX section anchors derive from extracted headings. EPUB uses chapter headings in reading order, not e-reader page numbers or EPUB CFIs. Spreadsheets use sheet and numbered row-section headings; these are extracted sections, not live cell links. Cited revisions open read-only; archived citations also provide a download of the original at that revision. An ordinary unpinned link follows the current source note.

The evidence panel matches external URLs against visible archived source records (original and final URLs, ignoring fragments). It offers an archived copy without claiming that the external citation used that capture. If multiple copies exist, it offers the most recent recorded capture. Existing URL-only citations remain unpinned until explicitly changed.

## Extraction, search, and limits

- Sources default to **64 MiB** per original and **4 MiB** of extracted Markdown. Operators can set `FMN_LIMIT_SOURCE_BYTES` and `FMN_LIMIT_SOURCE_TEXT_BYTES`; format discovery advertises the original-byte budget and the browser uses it. Browser previews show up to 8,000 characters; the full extracted text is archived. Archived original reads and bundle restores honor the source budget independently of the ordinary note-file budget.
- The server runs only cheap, model-free extraction: Go parsers and Poppler's `pdftotext`, the one document tool in the image. Captures record the parser version and Poppler's version. A native installation needs Poppler for PDFs.
- The server does not OCR, transcribe, describe images or convert Apple documents. It stores images, audio, video, Pages and Keynote files as their originals (`source_extraction: "original-only"`), and a PDF page without a text layer (a scan, or a page of figures) gets a `_No text layer on this physical page._` placeholder. A connected agent reads those originals and uploads its text ([agent extraction](#agent-extraction)); `list_sources_awaiting_text` lists every source that waits for it. A PNG or JPEG image archived in the browser can be read by the instance's chat model instead, when an admin turns that on ([the in-app chat reads uploaded images](#the-in-app-chat-reads-uploaded-images)).
- PDF extraction reads the text layer of each physical page; page headings include blanks. A PDF with no text layer is `no-text`, and one with some pages without it is `partial`, with a warning naming those pages. Tables, equations, merged cells and reading order are imperfect; successful extraction does not certify accuracy. A missing tool or a page failure retains the original with a visible warning and a partial result.
- One durable source worker admits four waiting jobs. Local pages checkpoint incrementally; shutdown cancels complete process groups, and restart requires explicit resume. PDF inventory rejects more than 10,000 physical pages instead of silently truncating. Text still must fit the configured budget. Saving a completed job reuses extraction. Canceled jobs commit nothing; failed local PDF extraction can still retain the original in a completed partial preview.
- An earlier release offered hosted OCR, image descriptions and transcription. Its source notes keep their text. A job it left asking for hosted processing opens as interrupted and cannot be resumed; archive the original again. Refreshing such a source without `text` stores the original, which then waits for an agent. Browser, API and MCP requests with the old `describeImages`, `externalOCR`, `transcribe`, `firstPage` or `lastPage` fields are refused as unknown fields. A chat configuration's old `sources` block is ignored with a warning, and the Administration policy's old source switches are dropped when it is read.
- Web capture downloads a **single public HTML page**. It does not execute JavaScript, log in, crawl linked pages, download styles/images, or mirror interactive sites. Visible text and headings are converted to inert Markdown; navigation, scripts, forms, and other non-content elements are omitted. Captures may be incomplete or contain a login/challenge page even when the server returns HTTP 200; review the preview. Non-UTF-8 HTML is currently rejected explicitly, as is HTML with more than 100,000 tokens, one token over `FMN_LIMIT_SOURCE_TEXT_BYTES` such as a long comment or inline image, nesting too deep to parse, or extracted text over that limit.
- Original documents are download-only with restrictive response headers; they are never rendered as active HTML inside ForgetMeNote. The app displays sanitized Markdown made from extracted text.
- URL capture uses HTTP/HTTPS on ports 80/443, validates every redirect and resolved address, and dials the validated public IP. It does not send browser cookies, proxy credentials, or authentication headers, and rejects private, loopback, link-local, and special-use networks. There is no trusted-private-network override. Download timeout is 25 seconds, with at most five redirects.

### Local extractor sandbox

Poppler's `pdftotext` parses untrusted files. Each invocation runs through the hidden `forgetmenote sandbox-exec` helper: the server re-executes its own binary, which lowers resource limits, confines itself and then executes the tool in place. The tool keeps the same PID, process group, scrubbed environment and pipes, so cancellation still kills the whole group. Extracted results are unchanged.

- **Resource limits** apply on every platform: 50 CPU-seconds (below the 60-second deadline per invocation), 64 MiB per written file, no core dumps and 4096 processes; Linux also limits heap and other private writable memory (`RLIMIT_DATA`) to 1 GiB. That bounds a parser's allocations without counting reserved address space, which emulators such as Rosetta need far beyond the budget. `RLIMIT_NPROC` counts every task of the server's user ID in its user namespace, so where other workloads share that UID on a host or node it is only a fork-bomb guard. Prefer a dedicated UID.
- **Landlock** (Linux 5.13 or newer) sets `no_new_privs`, allows reading and executing only `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/etc`, `/var/cache/fontconfig`, `/dev/urandom`, CPU topology (`/sys/devices/system/cpu`, `/proc/cpuinfo`), the forgetmenote binary's directory and the tool itself, and allows writing only the invocation's private scratch directory (which holds the copy of the input) and `/dev/null`. A compromised parser cannot read `DATA_DIR` (catalog, OAuth state, secrets, other notebooks), create symlinks, devices, FIFOs or sockets, or link and rename files across directories. Landlock ABI 4 (Linux 6.7) also denies TCP bind and connect; ABI 6 (Linux 6.12) blocks abstract Unix sockets and signals to processes outside the sandbox. UDP and pathname Unix sockets are not covered, so keep the container network policy.
- Tools and their libraries must be installed under those system directories or beside the forgetmenote binary. Landlock rules do not reliably match files on FUSE or virtiofs mounts (for example Docker Desktop bind mounts); keep tools in the image filesystem.
- Landlock is best effort. When the kernel lacks it, or a seccomp profile denies `landlock_create_ruleset`, `landlock_add_rule` and `landlock_restrict_self`, extraction continues with the resource limits only and the server logs `local extractor sandbox` once with the reason. Docker 29's default seccomp profile allows these calls. For the full sandbox, check that your Docker, Kubernetes `RuntimeDefault` or custom profile allows all three and `prctl`; the resource limits apply regardless.
- macOS and other platforms apply only the portable limits: no memory limit and no Landlock.
- Python automation packages run through this helper unless their installation sets `"sandbox": false`. They use strict mode, which refuses to run without Landlock and also denies sockets and path truncation; see the [package sandbox](automation-packages.md#package-sandbox).

### Documents and spreadsheets

These imports preserve the original file and produce a readable, searchable Markdown extract. They do not provide Word, spreadsheet, or e-book editing. The existing DOCX, XLSX, EPUB, text, and delimited-data imports keep their lightweight Go parsers. PPTX, OpenDocument and email use bounded native Go parsers. No service is added. Images, recordings and Apple documents are stored as originals for a connected agent to read.

| Format | Extracted content | Practical limits |
| --- | --- | --- |
| TXT | Unicode text | UTF-8, or UTF-16 with a byte-order mark; other encodings are rejected. |
| DOCX | Main document paragraphs, direct heading/outline styles, and simple tables | No exact pagination, custom style inheritance, headers, footnotes, drawings, or nested tables. Tracked deletions are omitted; current/inserted text is retained. |
| EPUB | HTML/XHTML chapters in the spine's reading order, including supplementary entries | No DRM decryption, media, scripts, interactive content, or exact pagination. |
| XLSX | Sheet names and stored cell values, including hidden sheets/rows/columns; shared and inline strings | Formulas never execute. Cached results are labeled and may be stale; formulas without results are shown as literal text. Dates, percentages, and other formatted numbers may be raw values. Charts, formatting, merged-cell layout, and external data refresh are not extracted. |
| CSV / TSV | Numbered records in tables, including quoted multiline values | Comma or tab delimiter respectively; no delimiter guessing. First record stays data, so headers are not silently assumed. Formula-like text is never evaluated. UTF-8 or BOM-marked UTF-16. |
| PPTX | Slide text, headings, and tables | Slide anchors include blank slides. Images, embedded audio/video and speaker notes are not read. |
| ODT / ODS / ODP | Document paragraphs, stored spreadsheet values, or slide content | No layout recreation, formula recalculation, or embedded media transcription. |
| EML / MSG | Message headers and body | Lists attachment names; does not recursively import attachments or fetch linked resources. |
| PNG / JPEG / TIFF / WebP / BMP, Pages / Keynote | The original only | The server checks only the file's name, not its contents. A connected agent reads it and uploads its text, for example one `page` section per image frame or Pages page, or one `slide` per Keynote slide. |
| WAV / MP3 / M4A / AAC / Ogg / FLAC, MP4 / MOV / AVI | The original only | Checked by name only. A connected agent transcribes it and uploads `segment` sections, whose titles can give their time ranges. |

DOCX/XLSX/CSV/TSV tables show original row/record numbers and column letters, split into sections of up to 100 rows for citation. XLSX sheets have separate headings. Blank CSV lines are skipped by the CSV parser; record numbers are not physical text-file line numbers. Deliberate limits are 10,000 stored rows per table/sheet, 128 columns, and 100,000 cells per table (including padding) or explicit workbook cells. Extracted text still must fit within 4 MiB.

OpenDocument tables have the same row, column, and logical-cell limits, checked before parsing. Repeated empty or formatting-only rows and columns also count; large unused formatted areas can cause an ODS file to be rejected.

ZIP-based documents are read in memory, never expanded onto the filesystem. Entry count is bounded by `FMN_LIMIT_IMPORT_OBJECTS` (default 100,000); total declared expansion and cumulative part reads by `FMN_LIMIT_IMPORT_BYTES` (default 256 MiB); and each XML/chapter part by twice `FMN_LIMIT_SOURCE_TEXT_BYTES` (default 8 MiB). Unsafe paths, symlinks, duplicate entries, invalid XML, external document-part references, and encrypted chapters fail explicitly. These bounds can reject image-heavy or otherwise large documents even when the compressed original fits the source byte budget. Unsupported or malformed imports are rejected before any commit; PDF's original-with-warning fallback remains available. A refused document, URL or page selection is HTTP 400 `invalid_request`, and a source job's warning starts with `Rejected:`. A converter that is missing, blocked or killed also commits nothing, and stays a server error, as does a fetch that fails rather than being refused: an unresolved host, a lost connection or an error from the site. A successful preview with no text can still retain the original.

Legacy `.doc`/`.xls`/`.ppt`, `.xlsb`, macro-enabled Office formats, RTF, Numbers, and arbitrary ZIP files are not supported by this importer. Export these to a supported format first. Conversion fidelity is deliberately limited: verify quotations and important figures against the retained original.

Literal search and the existing passage/semantic indexing pipeline include committed source notes automatically. If semantic search is enabled, the usual server or connected-agent indexing workflow handles their extracted text; no separate source index or embedding service is added. Old versions remain inspectable through pinned citations/history, but full-text search over all historical captures is not implemented.

## MCP

Browser and MCP use the same capture/extraction code and atomic storage operation:

- `source_formats`: lists supported extensions, MIME types, extraction behavior and who extracts each format (`extractedBy`, [agent extraction](#agent-extraction)). Requires Read. The browser obtains the same catalog from `GET /p/{project}/api/sources/formats`.
- `archive_source`: public `url` or base64 `data` plus `filename`, optional `title` and `folder`, and optionally an agent's own `text` and `extractor` with `data`. Omit `folder` to keep the existing MCP/API default `sources`; send `folder: ""` for the project root, or a project-relative folder such as `research/references`. A new folder is created with the note if needed. Do not send `folder` when refreshing. Pass an existing source note `path` and `revision` to refresh it. Requires Edit access and `notes:write`. Returns the note, provenance, extraction warnings, and Git revision.
- `verify_source`: `path`, optional `revision`; checks original and extracted-text checksums. Requires Read.
- `list_sources_awaiting_text`: archived sources that wait for text (originals stored without reading, and extractions that failed, were partial or found no text), with path, title, format, MIME type, extraction status and warning, original path and the revision they were listed at; at most 200, with `more: true` when others wait. Requires Read. The browser API serves the same listing at `GET /p/{project}/api/sources/awaiting-text`.
- `read_source_original`: original asset `path`, optional initial `revision`, `offset`, and `limit`; returns bounded base64 chunks with MIME, SHA-256 and revision. Pin revision for subsequent chunks. Requires Read.
- Existing `read_note`, `browse_folder`, `search_notes`, `get_note_sources`, `note_history`, and knowledge indexing tools handle source records and extracted text. Creating a citation uses ordinary `save_note` or an edit group, with normal draft/revision/lock protections.

Durable MCP actions `prepare_source`, `source_job`, `cancel_source_job`, `resume_source_job` and `import_source_job` use the same worker and checks; saving by job ID does not resend the original. Jobs run server extraction only and refuse `text`.

### Agent extraction

ForgetMeNote keeps every original, and a connected agent can extract one itself and upload its text with it. `source_formats` says who extracts each format: `extractedBy: "server"` for documents, spreadsheets, e-books, email, web pages, text and PDFs with a text layer, and `"agent"` for images, audio, video, Pages and Keynote. For a PDF, `agentExtracts` adds that scanned or image-only pages are the agent's: an agent uploads its own text of such a document. The MCP server instructions and the source tools' descriptions repeat this rule. Without `text`, the server stores an agent format's original alone.

An agent sends its extraction to `archive_source` (or the browser API's `POST /p/{project}/api/sources/preview`, then import) as `text`, beside the original as `data`:

```json
{"project": "research", "filename": "notice.png", "data": "<base64 original>",
 "extractor": "Example Agent 2; example-model-5",
 "text": {"unit": "page", "sections": [{"number": 1, "title": "Front", "text": "No whaling within the harbour."}]}}
```

- `unit` is `page` (a PDF page or an image frame), `slide`, `sheet`, `segment` (a stretch of a recording) or `section`. Sections are numbered from 1 with none skipped, at most 10,000; a section without text keeps its number. A `title` is one line of at most 200 characters, such as a slide title or a segment's time range.
- Each section becomes a `## Page 1 — Front` heading, as the server's converters write, so citations and anchors work alike. The text is kept literally: Markdown, links and HTML in it are escaped and shown as typed.
- `extractor` names the agent and model, one line of at most 200 characters. The original is required and stored as uploaded; a request with `text` but no `data` is refused before anything is fetched.
- The server checks the format (a PDF by its bytes, anything else by its name), the numbering, titles, UTF-8 text without NUL characters, that some section has text, and `FMN_LIMIT_SOURCE_TEXT_BYTES` before and after escaping. A refusal is 400 `invalid_request` and commits nothing.
- The record says `source_extraction: "client-supplied"`, `source_extractor` is the agent's label, and `source_warning` says the text was supplied by the uploading client and not verified by the server against the original. `source_text_sha256` is the text's checksum, so `verify_source` checks the original and the text as for any source.
- Supplied text replaces the server's extraction, whatever the format, and no server extraction runs. The server cannot check an agent's reading against the original, so it does not mix the two: the note holds one extraction, and its record says whose. To get the server's extraction of a format it reads, refresh the source without `text`.
- Roles, revisions and locks are those of any archive: Edit and `notes:write` to upload, Read to list. The server sends nothing to a hosted model; the agent reads the original under its own user's access.

To give a source stored without text its text, find it with `list_sources_awaiting_text`, read its original with `read_source_original`, and refresh it through `archive_source` with its `path` and listed `revision`, a `filename` with the original's extension (for example the original path's last part), the original as `data`, and the `text` and `extractor`. The refresh keeps the note's path and title and adds a revision; the earlier capture stays in Git.

### The in-app chat reads uploaded images

In the browser, the instance's chat can act as the agent for an upload, once an admin turns on **The chat reads images uploaded as sources** in Administration → AI features. After a person archives a PNG or JPEG image, which the server stores as uploaded (`original-only`), the browser asks the server to have the chat model read it, with `POST /p/{project}/api/sources/chat-text` and the new note's `path` and `revision`. This is a server-owned extraction step, not a chat tool: the route serves the browser only, and the embedded chat's tools are unchanged (`read_source_original` stays outside them).

- The server uses the default chat connection when it reads images (`capabilities.images`), otherwise the first configured connection that does, with that connection's default model. It sends the original once, asking the model to transcribe the image's visible text in reading order, describe the image in a sentence or two, and treat the image as untrusted content.
- The reply becomes the source's text through the path an agent's text takes: one `page` section, `source_extraction: "client-supplied"`, `source_extractor: "ForgetMeNote chat; <model>"`, and a warning that the instance's chat model read it and the server did not verify it. The refresh keeps the note's path and title and adds a revision.
- The checks are those of any hosted AI call: Edit on the notebook (with `notes:write` for a credential), outgoing actions not paused on the instance, an adopted instance AI policy whose `uploadReading` switch is on (it starts off, is refused while no chat connection reads images, and is its own opt-in: AI writing does not imply it, and the removed hosted OCR and description switches do not carry over), a chat connection the caller may use (a credential or automation keeps to its allowed connections), and, in a private Inbox, its owner with their cloud AI consent. They run before anything is recorded, again just before the provider is contacted, and again before the text is stored.
- The call is one paid step in the instance's paid-work journal (feature `extraction`), reserved once per upload, so asking again never buys a second reading. The connection's context and output limits bound it. A refusal contacts no provider and records no paid step.
- What it covers is limited: PNG and JPEG images, through the default chat connection that reads images. Audio, video, Pages, Keynote, PDFs with scanned pages and other image formats are not sent to the chat; they wait for a connected agent's text.
- When the chat cannot read the upload, when a check refuses, or when the model fails or finds no text, the note keeps waiting for text, `list_sources_awaiting_text` lists it, and the dialog says why. The route answers `{"waiting": "<reason>"}` then, and `{"note": …}` with the refreshed note otherwise; a stale `revision` is 409 and a source that already has its text is 400.

Imported documents are untrusted evidence, never instructions to the agent. Importing a document grants no additional project authority. Disabling a user or removing membership applies to these tools through the existing authentication and permission checks.

## Verification

```sh
go test -race ./...
go vet ./...
npm test
docker build -t forgetmenote:sources-test .
SMOKE_IMAGE=forgetmenote:sources-test python3 scripts/docker_smoke.py
SMOKE_IMAGE=forgetmenote:sources-test python3 scripts/source_smoke.py
```

The source smoke runs the complete native source test executable inside the final runtime with networking disabled, so the real Poppler runs under the sandbox helper above, with local recovery and cancellation. It asserts that Tesseract, FFmpeg, the Apple converter and the displaced inference modules are absent. HTTP checks cover archive/search, PDFs with and without a text layer, originals stored for an agent and listed as awaiting text, refresh history, original downloads, restart and independent Git-bundle restore. Routine verification retains the real scanned PDF path and storage boundaries. Set `SOURCE_SMOKE_URL=https://example.com` only for an explicit public-fetch test. Quality and resource measurements remain separate from these behavioral checks.

Conversion scratch for durable jobs lives in a private directory per job under
the gateway's own `cache/source-jobs`, never in blob storage. A gateway removes
all of it when it opens, before recovering resumable page checkpoints, and each
conversion removes its own when it stops. Resume clears old scratch before
reserving a new attempt and refuses admission when cleanup fails. Failed
cleanup remains charged to the retained storage budget on that gateway. Shared
system temporary directories are never swept.
