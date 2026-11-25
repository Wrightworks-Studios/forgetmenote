# Notebook language

A **notebook** is the shared unit of organization, discovery, access, and history.
Notes, imported material, read-only repository files, and saved AI output belong
to notebooks. Operational credentials, instance policy, private processing state,
and execution receipts keep their own concrete owners.

| Term | Meaning |
| --- | --- |
| Notebook | A named workspace with members, permissions, storage ownership, and history. |
| Note | Markdown content saved with its attachments and Git history. |
| Inbox | An account's private notebook for quick notes; AI access requires consent. |
| Quick note | A short capture into the private Inbox. |
| Drafts | A notebook collection of shared draft notes. An unsent editor draft remains private to its browser until shared or saved. |
| Archives | A notebook collection of retained notes, distinct from Trash. |
| Sources | Imported material or verified, read-only repository snapshots. |
| Review | Checks and suggestions about saved notebook content; changes need approval. |
| Chat | Questions and proposed edits using explicitly selected notebook evidence. |
| Assistant | Opt-in help using selected sources and private memory in its notebook. |
| Storage & sync | Notebook storage ownership and backup/handoff controls, previously labeled Authority. |
| Administration | Instance policy, accounts, groups, connections, and automation management. |

Saved status describes the current state; its tooltip and History retain the
exact revision. People appear by their available display name, with timestamps
that retain a complete date on hover. Technical IDs remain available where an
operator must configure or audit an integration.

Settings and Administration are URL-addressable pages. Short tasks such as
creation, renaming, and confirmation use dialogs. The note context toggle exposes
Contents, References, and Comments, with counts from the actual authorized reads.
The notebook map appears in the library. Notebook creation uses separate buttons;
the notebook picker only navigates. Search scopes use named checkboxes and display
excerpts are derived without changing pinned evidence or its coordinates.

Public notebook API/MCP names reuse existing operations and permission checks.
Opaque Git and browser-storage identifiers remain implementation details; this
release does not rename them or rewrite notebook content/history.
