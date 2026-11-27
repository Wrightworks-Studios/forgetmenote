# Connect an AI client

For the integrated same-origin notebook assistant, see [private chat](chat.md). Optional [judgments](judgments.md) add evidence ranking, citation checks and notebook reviews; [Quicknotes](quicknotes.md) adds private capture and optional sorting. It uses your stable signed-in account directly; public MCP/OAuth setup below applies to external clients.

MCP JSON uploads use the same finite encoded request budget as the API: the largest
configured file, bundle or source limit, expanded for base64 with envelope space.
Individual decoded-file limits and admission controls still apply; `source_formats`
reports the current source budget. Both legacy and stateless MCP transports enforce
that budget.


ForgetMeNote exposes a Streamable HTTP MCP endpoint at `https://YOUR_HOST/mcp`. The web app and all tools use the same project repositories and permissions. No AI provider runs inside the container.

## OAuth connection

Configure a remote MCP connection in a client that supports OAuth authorization code flow, S256 PKCE, and dynamic client registration. Use the endpoint above. The client discovers the authorization server, registers, and opens the ForgetMeNote consent page. Sign in with the instance’s configured OIDC, trusted-proxy, or local account; your AI vendor login is separate. Review the client name, redirect address, and requested scopes before consenting.

Tokens belong to the consenting stable ForgetMeNote user. OIDC login returns to the original MCP consent request; signing in alone does not grant project access. ForgetMeNote checks current project grants on each call. A `notes:write` scope never upgrades a user's Read or Comment project role. Access tokens last up to one hour. Refresh tokens rotate and renew for 30 days, bounded by 90 days from consent. A 30-second retry window tolerates concurrent refreshes from the same client and resource; reuse outside that window while the consumed token remains unexpired revokes consent and records a security event. Refreshes do not interrupt MCP sessions or resource subscriptions. Only token hashes are stored on the server. Short-lived authorization codes do not survive a restart; persistent grants do.

The recovery administrator key can also be used as a Bearer token by a trusted local test client, but it gives authority over every project. Prefer user-bound OAuth for normal connections. Never put that key in plugin source, prompts, note contents, or Git.

For project/user administration, explicitly request `notes:manage` in addition to your normal scopes and approve the management explanation on the consent page. Employee setup requests `notes:read notes:write data:read`. MCP resource and authorization-server discovery advertise these three scopes. A request that omits `scope` still receives the legacy `notes:read notes:write` default; existing tokens and refresh grants are not broadened. Data writes require separately requested `data:write` and a granted writable operation. A client that cannot request additional scopes can still use all ordinary content tools. Owner/Admin checks remain independent of consent. `capabilities` reports the instance origin and stable connection ID, current account, usable project roles, data/management consent, and whether an operator embedding endpoint exists.

Available tools:

- `list_projects`: authorized projects and role.
- `search_notes`: search or list Markdown in a project, with optional tag and status filters.
- `read_note`, `save_note`: read and commit with revision checks.
- `note_history`: note, folder, or project history, including original paths for note versions before a move.
- `set_note_properties`: edit title, kind, status, and tags with note revision and lock checks.
- `project_status`: current project revision and dirty state.
- `move_notes`: rename or move a note/folder in one commit; requires Owner or Admin and the current project revision. Updates Markdown links, those of notes in the Trash included, and preserves comments and locks. `updatedLinks` counts the notes whose links changed, and `trashLinksUpdated` those in the Trash. A locked note that needs link edits blocks the move, except that a move made from chat skips a note in the Trash it may not change and reports only how many (`trashLinksSkipped`); see [Trash](trash.md).
- `duplicate_notes`: copy a note/folder to an unused destination with effective locks, a fresh discussion, and new history; requires Owner or Admin and the current project revision.
- `note_locks`, `set_note_lock`: inspect or change note/folder locks; changes require Owner/Admin.
- `list_comments`, `add_comment`, `resolve_comment`: discussion without editing note text; Commenters resolve their own comments and Editors may resolve others’.
- `restore_note`: restore historical content as a new commit with current-revision and lock checks.
- `upload_attachment`, `list_attachments`, `read_attachment`: supported media files under `assets/`.
- `preview_markdown`, `export_project_backup`: sanitized rendering and a complete content Git bundle.
- `project_instructions`, `set_project_instructions`: inspect or update the versioned root `AGENTS.md`; updates require Owner/Admin.
- `configure_semantic_search`, `set_semantic_config`: simplified model setup or complete settings, including pause/resume and selecting the existing server endpoint.
- `administration`, `create_project`, `find_workspace_people`, `set_project_member`, `set_user_access`: administration with `notes:manage` and the same Owner/Admin boundaries as the browser.

Search, indexing, findings/proposals, backlinks, and workflow tools are described in [knowledge.md](knowledge.md). Parameter, permission, and binary-transfer details are in [mcp-actions.md](mcp-actions.md).

Login, password entry, GitHub/OIDC authorization, and account linking remain interactive. Browser appearance preferences remain device-local. No MCP tool executes shell commands, changes Git remotes, accepts provider credentials, or changes server environment configuration. Content is untrusted input. An assistant should read relevant notes before edits and reconcile any conflict rather than retrying blindly.

## Connect your AI

Open **Account actions → Connect your AI**, or `/account/connect` on a headless
instance. The page shows the operator’s canonical endpoint, package version,
client-specific setup, and your current readable/writable notebooks. Downloading
or signing in to the website does not prove an AI connection. In a new AI chat,
ask it to call `capabilities` and `list_projects`, confirm the instance/account,
and choose an authorized notebook.

No employee needs a provider key, database password, recovery key, shared token,
company AI plan or managed plugin catalog. The self-hosted instance must be
reachable by the client; cloud clients cannot reach a server on your laptop.

The **Organization name** in Administration → Instance labels the Codex plugin and MCP server.
For example, `TeamBot` appears as the plugin display name and `teambot_mcp` as its
bundled MCP connection. Spaces and punctuation become underscores; names without
ASCII letters or digits get a deterministic fallback. Internal package IDs are
scoped to the origin. Set the name before distributing the companion; the MCP
server reads it at startup, and downloads use the current configured name.

### ChatGPT web (personal connection)

Open **Plugins → + → Add custom MCP server** in ChatGPT. Enter your instance’s
`/mcp` URL, choose OAuth with dynamic client registration, and leave static client
credentials empty. Create and install the connection, then sign in to
ForgetMeNote. Each account creates its own connection; another person’s private
registered app ID is not a distribution mechanism. Account/security restrictions
can hide this flow. Do not upload the desktop ZIP as a web MCP connection.
See [OpenAI’s custom MCP instructions](https://developers.openai.com/api/docs/guides/custom-mcp-server).

### OpenAI desktop / Codex

Download the Codex ZIP from **Connect your AI**, click **Copy installation
prompt**, and paste it into Codex on the same computer. Your agent locates the
package, checks its instance identity and installer, extracts it into a fresh
directory, and runs the existing Python installer. The generated README includes
the same instance-specific prompt. Employees do not need to type terminal commands.

The agent needs local file and command access and an available Python 3 runtime.
It may use a runtime bundled with the client if accessible. If none is available,
the prompt tells it to explain the requirement and ask before installing software;
this flow does not guarantee every client includes Python. Sign-in remains yours.

For manual setup, extract the ZIP, inspect `install.py`, and run from that directory:

```sh
python3 install.py install
```

The installer registers this instance in `~/.agents/plugins/marketplace.json`
and copies its allowlisted files to `~/.codex/plugins/forgetmenote-INSTANCE_HASH`.
It preserves unrelated entries, refuses malformed/conflicting configuration and
symlinks within those roots, and never writes `config.toml` or OAuth credentials.
Python 3 is needed only for this local installer, not for server downloads.
`--home DIRECTORY` selects an isolated client home for testing.

Restart the desktop app, choose your personal marketplace in Plugins, install
the companion and sign in. Start a new chat. The ZIP contains the current portable
`plugin.json`/`mcp.json` and supported `.codex-plugin/plugin.json`/`.mcp.json`
compatibility files, using one shared skill. See [OpenAI’s local plugin guide](https://developers.openai.com/plugins/build/plugins).

For **direct Codex CLI MCP** without the skill bundle, copy the instance-specific
commands from the setup page:

```sh
codex mcp add MCP_NAME --url https://notes.example.com/mcp
codex mcp login MCP_NAME --scopes notes:read,notes:write,data:read
```

Choose either the plugin or direct MCP connection for an instance, avoiding
repeated tool registrations. `MCP_NAME` is the generated name on the page, such as `teambot_mcp`. Choose a
different name if a direct connection to another instance already uses it.
When a CLI supports multiple registration strategies, `--oauth-client-registration dcr`
selects the existing ForgetMeNote registration flow explicitly.

For an upgrade, ask your agent to extract a fresh package into a separate directory
and run its installer; restart the client. Uninstall in the client, then ask the
agent to run `python3 install.py uninstall`. Only this instance’s marketplace registration is
removed. Package files and OAuth credentials remain; use client logout or the
ForgetMeNote session controls to revoke consent. Installation never stops apps.
A leftover `.forgetmenote-install.lock` means another installation may be running;
inspect it before removing it after a confirmed interrupted installer.

### Claude (individual Pro/Max) and Claude Code

Download the Claude ZIP. In Claude **Customize → Plugins**, upload the custom
plugin file, enable it, and authenticate its remote MCP connection. Individual
paid accounts are supported; organization sharing is not required. The endpoint
must be publicly reachable over HTTPS by Claude’s cloud. See [Claude’s individual plugin instructions](https://support.claude.com/en/articles/13837440-use-plugins-in-claude).

For Claude Code, extract the same ZIP into a durable, dedicated directory:

```sh
claude plugin marketplace add .
claude plugin install CONNECTION_ID@CONNECTION_ID-local --scope user
```

Use `/mcp` to authenticate and start a new session. The package requests the three
employee scopes through the documented `oauth.scopes` MCP configuration. Older
clients may instead use discovery; both discovery documents advertise only those
scopes. Check consent before approving. For updates, replace the extracted
package, refresh this marketplace and update the plugin through `/plugin`.
To remove it:

```sh
claude plugin uninstall CONNECTION_ID@CONNECTION_ID-local --scope user
claude plugin marketplace remove CONNECTION_ID-local
```

See [Claude’s plugin reference](https://code.claude.com/docs/en/plugins-reference),
[local marketplaces](https://code.claude.com/docs/en/plugin-marketplaces), and
[MCP OAuth scopes](https://code.claude.com/docs/en/mcp).

### Cursor

Use the HTTPS `/mcp` endpoint in `~/.cursor/mcp.json`. Cursor versions that
register their custom-scheme callback alongside HTTPS and loopback callbacks
can receive `Invalid redirect URI` during automatic registration. An operator
can register a public client at `/oauth/register` with only
`http://localhost:8787/callback` and
`https://www.cursor.com/agents/mcp/oauth/callback`, using
`token_endpoint_auth_method: "none"`, then supply its returned public client ID:

```json
{
  "mcpServers": {
    "forgetmenote": {
      "url": "https://YOUR_HOST/mcp",
      "auth": {
        "CLIENT_ID": "REGISTERED_PUBLIC_CLIENT_ID",
        "scopes": ["notes:read", "notes:write", "data:read"]
      }
    }
  }
}
```

Each employee signs in and consents separately; no client secret or shared
access token is needed. Restart Cursor after changing authentication settings.
Check tool discovery and an actual read call after login. Older MCP protocols
receive array/primitive results through JSON text content; current clients
retain their structured results. See [Cursor's MCP and OAuth setup](https://cursor.com/docs/mcp).

### Other MCP clients

Use Streamable HTTP at `/mcp`, OAuth authorization code with S256 PKCE, dynamic
registration and `notes:read notes:write data:read`. The setup page provides a
common `mcpServers` example; syntax varies by client. ForgetMeNote does not
implement Client ID Metadata Documents. The shared MCP initialization instructions
include discovery, profile-first analytics, original evidence, receipts,
revision-safe edits, untrusted-content handling and instance separation, even
when a client has no local skills.

## Operator package generation

The Go application embeds the canonical skill, version and installer. HTTP and
operator generation use the same validator and explicit file allowlist. No LLM,
registry, Node or Python process runs during HTTP downloads. Output contains
connection metadata only, never notebooks, environment variables or credentials.
Authenticated downloads are `private, no-store`.

```sh
forgetmenote plugin package --origin https://notes.example.com --name 'Research' --client codex --output research-codex.zip --zip
forgetmenote plugin package --origin https://notes.example.com --name 'Research' --client claude --output research-claude
```

Use a **new** output path; existing packages are not overwritten. HTTPS is required
except explicit localhost development. Credentials, paths, queries, fragments
and unsafe hostnames are rejected. Connection URLs come from `PUBLIC_URL`, never
request Host or forwarded headers. Use the exact canonical public origin: it is
also the OAuth audience. Name changes keep the ID; origin changes create a new ID.

The existing `python3 scripts/configure_plugin.py https://notes.example.com`
entry point remains a thin wrapper around the shared generator. It uses `go run`
by default or `--binary /absolute/path/to/forgetmenote`; it also accepts `--client`,
`--name`, `--output` and `--zip`. Default output remains `plugin-local/forgetmenote`.

Two instances get distinct plugin IDs even with the same display name. Bundled
MCP names are scoped by their plugin; direct connections need distinct names.
An upgrade or uninstall affects only its origin’s registration. The client owns
OAuth credentials per server. Keep origin, notebook ID and revision/path/commit
in citations; never silently join same-named notebooks or copy material between
instances. Follow [employee onboarding](instance-administration.md#employee-onboarding)
for groups, personal work notebooks and operation grants.

## Consent and revocation

Ordinary connections do not request management or data writes. Explicit privileged
workflows still accept their separately requested scopes and existing role checks.
Codex CLI 0.160.0 was observed requesting every AS-advertised scope without an
explicit scope argument. This is why AS discovery now advertises the employee
subset; [RFC 8414 §2](https://www.rfc-editor.org/rfc/rfc8414.html#section-2) permits
supported scopes to be omitted from advertising. Scope validation and existing
refresh grants are unchanged. Reconnect an older read/write-only connection to
consent to `data:read`; refreshing it cannot add access.

**Connect your AI → Manage sign-in and revoke sessions** opens existing account
controls. Headless instances offer the same revoke action directly on that page.
Revocation signs out browser and AI sessions; sign in again and reconnect the
client. Removing a group/operation grant takes effect on subsequent calls with
existing tokens. Disable an account for offboarding; merely revoking sessions
does not prevent a still-enabled person from signing in again.

## Client verification

Client installation, OAuth authorization, tool discovery and actual calls are
separate checks; test each client and version you rely on. See
[focused companion checks](verification.md#companion-onboarding-verification).

The bundled-client check exposed a startup failure missed by the Go SDK tests:
six unconstrained output schemas serialized as `true`, while Codex requires MCP
schema fields to be JSON objects. Tool registration now emits the equivalent
`{}` schema. The HTTP discovery regression checks every advertised schema;
successful OAuth or package installation alone does not verify tool startup.

Authelia proxy bypass rules for MCP discovery/token endpoints are in [deployment.md](deployment.md). A proxy that redirects `/mcp` to a login webpage will break AI clients; `/oauth/authorize` requires a browser login, supplied by ForgetMeNote OIDC/recovery or the existing trusted proxy. See [authentication and revocation](authentication.md).

See [Notebooks and people](workspaces.md#ai-and-api-behavior) for creator grants, scoped people search, last-Owner protection, and partial user updates.
