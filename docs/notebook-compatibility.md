# Notebook API and identifier transitions

Notebook names describe user content. Account identities, execution receipts and private credentials keep their existing operational owners. These transitions preserve current consumers and existing data; they do not rewrite Git history.

## HTTP and MCP

Use `/api/notebooks` for notebook listing, creation and import, and `/notebooks/{id}/api/...` for notebook operations. `/api/projects` and `/p/{id}/api/...` remain aliases into the same authentication, strict JSON, permission, admission and revision checks. The versioned action API stays under `/api/v1/actions/{name}`.

| Preferred action | Existing alias |
| --- | --- |
| `list_notebooks` | `list_projects` |
| `notebook_status` | `project_status` |
| `create_notebook` | `create_project` |
| `import_notebook_bundle` | `import_project_bundle` |
| `export_notebook_backup` | `export_project_backup` |
| `set_notebook_member` | `set_project_member` |
| `notebook_groups` | `project_groups` |
| `set_notebook_group` | `set_project_group` |
| `notebook_instructions` | `project_instructions` |
| `set_notebook_instructions` | `set_project_instructions` |
| `read_notebook_changes` | `read_project_changes` |

Both names use the same operation and policy. Arguments retain the established `project`/`projects` fields and output schemas for current SDKs and the companion plugin. Neither alias grants consent, broadens notebook scope, bypasses locks, or authorizes instance management.

## Browser sessions

New logins issue only the canonical session cookie: `__Host-forgetmenote` on HTTPS, `forgetmenote` on plain-HTTP loopback. It is HttpOnly, SameSite=Lax and scoped to `/`, and it is Secure on HTTPS. A session issued under the earlier `forgetmenote` name on HTTPS moves to the `__Host-` name on authenticated use, without extending its original lifetime. Missing, empty or conflicting cookie values fail closed; an invalid value under one name never falls back to a session under the other. Logout deletes the ledger session and expires every session cookie name. Chat execution guards use that same resolved token.

The `gitnotes` session alias is retired: it is no longer issued or accepted, and logout still expires it in browsers that kept one. Every release since the canonical name was introduced issued both names and moved legacy-only browsers on their next request, and sessions last at most 12 hours, so no live session depends on `gitnotes` alone. A one-step rollback to the previous release keeps sessions, because that release reads `__Host-forgetmenote`. On HTTPS, rolling back further, to a release from before the `__Host-` name (`2adf4d0` or earlier), signs everyone out once.

## Trusted proxy

Trusted proxies send `X-ForgetMeNote-Proxy-Secret`. The `X-Gitnotes-Proxy-Secret` alias is retired and ignored, so a proxy that sends only the old header no longer signs anyone in. Repeated values of the current header must agree. Socket peer trust and the configured private secret remain required; the header alone never enables SSO from an untrusted peer.

Releases before this retirement still accept the old header, so proxies should keep clearing any client-supplied `X-Gitnotes-Proxy-Secret` while such a release remains a rollback target. Never expose the secret in an ordinary browser request or notebook.

## Internal identifiers

Notebook content and Git history remain intact. Internal Git trailers, lock/Trash filenames and browser-storage keys do not need vocabulary migrations for this release; they retain their current opaque identifiers. The deployment has no active application users requiring additional cookie, draft or API migration machinery. Public notebook aliases share existing owners rather than creating parallel implementations.

MCP accepts both `forgetmenote://notebooks/{project}/revision` and the existing `forgetmenote://projects/{project}/revision`. Reads use the same authorization; subscriptions retain the exact requested URI, including when a session subscribes to both aliases. Unsubscribing one alias leaves the other active.
