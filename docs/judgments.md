# Judgments and notebook reviews

Judgments are an optional remote decision service. A judgment scores evidence
and selects from choices supplied by ForgetMeNote; TypeSafe's Jev is the
supported decision model. Your configured chat model, such as DeepSeek,
continues to write answers and proposed changes. A judgment cannot choose a provider,
change permissions, create folders, or execute note instructions.

## Configuration and privacy

Set `DECISION_CONFIG_FILE` to a read-only mounted JSON file with
`"provider": "typesafe"`; see the exact
[configuration contract](judgments-architecture.md). Judgments work without a chat provider
or `CHAT_CONFIG_FILE`. Mount the TypeSafe key read-only at the configured
`secretFile` path, such as `/run/secrets/providers/TYPESAFEAI_KEY`. A Kubernetes
secret key or environment variable named `TYPESAFEAI_KEY` is not read automatically.
Keep credentials outside Git. Neither the browser nor notebook content receives
the key. Restart after changing operator configuration or credentials.

Existing `jev` objects inside `CHAT_CONFIG_FILE` remain supported. Configure judgments
in one file only; defining it in both fails startup. A chat configuration with
`localOnly: true` also prohibits standalone judgments. Enabling judgments does not enable chat
or web research, or enable instance AI features.

**Administration → AI features** controls reranking, citation checks, notebook
reviews and configured organization/automation jobs for the whole instance.
Notebook and personal views show status without separate opt-ins. See
[instance AI controls](ai-features.md) for migration of existing settings.
Local-only installations cannot use this remote service. Private editor drafts
and pending live text are not background-review input. Private Quicknotes retain
their sorting settings and remote-processing consent.

There are no application token allowances per run, day, user, or connection.
Reported usage and uncertain usage remain recorded. Actual provider context and
output capacities, finite retries, cancellation, concurrency, and transport/storage
bounds still apply. Large reviews checkpoint their progress; continuing work does
not require a new token allowance.

## Retrieval and citations

Reranking judges up to 32 retrieved BM25/semantic candidates before the final
evidence selection. Chat uses it for every answer. Notebook search uses it only
on an explicit request: pressing Enter in the search box, or `rerank: true` in
`POST /p/NOTEBOOK/api/knowledge/hybrid-search` or the MCP `hybrid_search` query.
Search-as-you-type, Text mode and previous-version search stay local; responses
report `reranked` and whether an explicit request is available (`rerankAvailable`). Each independent question contains one excerpt; only the query
is shared. The default 32-question, 64 KiB request usually fits the shortlist in
one parallel API call. Larger inputs or smaller operator limits split into batches
using the operator's configured concurrency. Identical path/heading/text inputs
share one judgment while retaining their original citations. The unscored tail
retains its local order. It does not search every possible passage or prove that missing evidence
does not exist. Provider failure retains ordinary authorized retrieval.
Current Archives, Drafts and Trash follow the selected search scope;
historical results use the classification in that historical version.

Citation checking happens after an answer, so it does not delay its streamed
text. Exact source references are checked in code; a judgment decides whether identifiable
claims are supported by their associated evidence. Ambiguous claims or unfinished
checks remain unchecked. A supported judgment is not a guarantee that the source
itself is true. Corrections require an explicit chat action and use the normal
proposal/edit guards; checks do not silently replace an answer.

Canonical notebook links retain the cited notebook, revision, content hash and
original excerpt. Checks revalidate the saved notebook selection and current
read permission before and after a judgment, and before displaying it. Current
sources must still match; explicitly selected history and exact same-notebook
committed attachments retain their pinned versions. Inspect links and correction
prompts preserve the source notebook identity. Tables and live data receipts stay
outside this note-passage verifier. New quoted text in a matching successful
proposal receipt remains unchecked rather than being labeled an invalid quotation
from the old note; ordinary unsupported source quotations retain their strict check.

## Consistency and notifications

Start a consistency review explicitly. Automatic checking is off by default and
requires a notebook Owner to enable it. Automatic work is attributed to that
account and rechecks its current access. Committed content changes are debounced;
review bookkeeping alone must not trigger another scan. Only passages whose text is
new since the last automatic review are checked, so a small edit to a long note
checks that part, and moving a note checks nothing.

Consistency reviews examine active committed notes, excluding Archives, Drafts
and Trash. They compare candidate passages in context, including dates, entities,
exceptions and tentative alternatives. A possible contradiction asks for review;
it does not establish which note is correct. Coverage shows examined and remaining
work. Results and notifications load in pages; use Load more to inspect further
items. Candidate comparison is not an exhaustive proof of consistency.

The notification center collects review results and optional Quicknote filing
updates across notebooks you can currently access. Reading a notification is a
personal action. Resolving or dismissing a finding is a shared notebook action
with the existing Comment/Edit rules. Changed or excluded evidence must be
rechecked before acting. Inspect original passages before requesting a correction.

## Extraction audits and organization

An extraction audit compares explicitly selected source revisions with selected
destination notes. An archived source is allowed only when explicitly selected.
The result distinguishes preserved content, partial coverage, missing content,
meaning changes, contradictions, supported supersession, and uncertainty. An older
source is not automatically superseded. Review qualifiers, exceptions, names and
numbers before considering an extraction complete.

Organization review uses an existing or explicitly supplied tag vocabulary and
compares related, duplicate, complementary and versioned material. No suitable
tag is a valid result, and several tags may apply. Changes require acceptance
through guarded proposals. Neither workflow automatically archives or deletes a
source, merges notes, or creates folders.

## Recovery and limits of verification

Notes and their Git history remain authoritative. Durable review jobs, personal
settings, notification state and usage records are filesystem sidecars; full
instance backups must include them. A notebook Git bundle alone cannot restore
those workflows. See [backup and restore](backups.md) and the
[integration contract](judgments-architecture.md).

Interrupted paid requests can have unknown usage; the app retains that uncertainty
rather than counting them as free. A retry must retain its durable operation
identity. Changed evidence, permissions or settings invalidate results before
publication or edits. An unavailable owner or remote service never permits an
administrator-token fallback.

Synthetic interoperability and evaluation results belong in the release report.
Mocked service tests establish application behavior, not remote model quality.

## Applying organization

Review → Checks now includes **Organize existing notes**, with separate preview
and applying controls. Shared AI permission is managed in Administration. Owners/Admins may
authorize additive tags/links, same-notebook moves and explicitly opted-in duplicate
cleanup to Trash. Existing automatic organization policies remain compatible; the instance writing/automation switch gates their execution after adoption. Activity
provides before/after details, guarded Undo and persistent suppression. See
[organization](organization.md) for permissions, full-note verification, thresholds,
privacy, coverage and recovery limits. The existing proposal-only organization
review remains available with its existing acceptance workflow.
