# Personal employee assistants

Personal assistants are optional and disabled until an administrator enables
them. Each employee chooses features and AI-processing consent, then connects
their own accounts at **Account → My assistant**. Logging in through Google or
an OIDC broker does not grant mailbox, calendar, Drive, or Slack access.

## What runs

The existing Go automation queue schedules each opted-in employee. There is no
new service, database, or worker container. Rules exclude spam, bulk mail, the
employee's own latest replies, and cancelled events. Changed items are judged in
one parallel judgment request. Luna investigates relevant items with read-only host
tools and writes proposed replies, reminders, or sourced remediation plans to the
employee's assistant notebook. An unchanged item does not purchase another AI
generation; Gmail label/history changes alone do not count as changed content.

The first provider adapters support:

- Gmail: incremental history with a checkpointed initial lookback, complete
  threads, attachments, proposed replies, and optional automatic Gmail drafts.
- Slack: the employee's DM/group-DM conversations through a **user** grant;
  paginated complete threads, suggested replies to copy, and optional private
  bot reminders with suggestion titles and private links. The bot grant alone
  cannot read an employee's other DMs.
- Google Calendar: upcoming primary-calendar events, reschedules, and meeting
  preparation. Reminders are derived from the event start, 30 minutes ahead
  (all-day events use 09:00 in the employee's time zone).
- Google Docs/Drive and Sheets: on-demand search and reading for contextual
  answers and drafts. These are not complete background copies of Drive.
- Existing data connections: explicit employee-configured read-only watches for
  logs or other operational results, using the current connection permissions.
  **Your assistant → Data watches** offers the connections the employee may use
  that allow AI processing, their read-only operations, and a field for each
  flat argument in the operation's input schema; other arguments are entered as
  JSON. Saving checks every watch again.

Daily briefings after 08:30 in the employee's time zone reuse existing grounded
suggestions without another LLM call. Follow-up reminders require an explicit
source-backed due date. Quiet hours defer delivery. Briefing/reminder times are
product defaults; provider polling and batch size are administrator controls.

The model receives only the tools permitted to that employee, the enabled
features, current responsibilities, and concise memory. Job titles never grant
permissions. Source repository AI consent, exclusions, local-only policy,
provider concurrency/deadlines, and shared AI controls still apply. Turn on
**Administration → AI features → Writing** as well as the assistant policy.

## Private notebooks and shared responsibilities

An opted-in employee gets a protected notebook `assistant-<account ID>` with:

- `AGENTS.md`: work habits and responsibilities.
- `SOUL.md`: communication preferences.
- `CONTEXT.md`: current confirmed priorities.
- `commitments.md`: outstanding promises, sources, and resolution status.
- `drafts/` and `briefings/`: committed suggestions and daily digests.

These remain ordinary Markdown with notebook Git history. Employees cannot share
the assistant notebook or grant a service account ownership. Other employees
cannot read it. **Instance administrators and the server operator retain their
existing authority**; this is not encryption against the operator. Tokens are
encrypted separately and never appear in notebooks, exports, prompts or logs.

Once a notebook chat has used your memory or connected accounts, each change it
makes outside your own assistant notebook waits for your approval of the exact
change (see [chat](chat.md)).

Memory maintenance is a separate opt-in. It consolidates direct employee/assistant
interactions from private UI chat and verified Slack DMs. Incoming source text
cannot authorize actions or alter application policy. AI preference extraction
still needs quality evaluation; prompt instructions are not a guarantee against
all semantic mistakes. Employees can inspect and edit all four memory files.
Concurrent edits are preserved and the generated proposal remains recoverable.

Administrators may designate an ordinary shared responsibilities notebook. Each
employee has `people/<stable account ID>.md` containing confirmed role, team,
owned systems, current projects, contact routes, source links, and confirmation
date. Give employees read access through normal notebook roles. Do not populate
the directory from private emails/DMs; personal interactions never publish to it.
MCP `set_assistant_responsibilities` writes a note, so it requires `notes:write`
consent in addition to `notes:manage` and directory Owner or Admin authority.

Assistant notebook IDs are `assistant-<stable account ID>`; ordinary notebook
creation and provisioning refuse IDs beginning with `assistant-` or `inbox-`, so
no one can pre-claim another person's notebook. Quick note filing never offers
another employee's assistant notebook as a destination, even to an Admin, nor
any project memory notebook.

The UI assistant, verified personal Slack conversation, and MCP
`assistant_context` use the same profile files and permissions. The companion plugin
instructs a local agent to load it when relevant. It is context, not an override
of the local agent's own instructions. An MCP credential restricted to selected
notebooks must explicitly include the assistant notebook to read it.

## Agent memory

Agents connected over MCP or the API, and direct messages that are yours, keep
memory for you in private notebooks. Memory does not need the assistant, and
**Your assistant → Memory** holds its setting. See [agent memory](memory.md) for
where it lives, how agents use it, how to correct it and how it is searched.

### Pairing by email

An administrator can pair members with accounts automatically when both carry
the same company address. Name the Google Workspace domains under the chat
client's `members.pairByEmail` (for example `["example.com"]`), give the Slack app
the `users:read.email` scope, and create an `email-pairing` instance automation
that runs as `members.actor` on a schedule. Each run lists the workspace's
active, human, non-guest members and pairs one with an account only when exactly
one active account's latest sign-in verified that address with one of those
Workspace domains, and no other member shares the address. A paired DM then uses
memory exactly as a [link](memory.md#direct-messages) does, and
**Your assistant → Memory** shows it as paired by email. With Slack's `team_join` and `user_change` events subscribed, a member
who joins, changes address or leaves starts a run at once; a change that cannot
affect pairing, such as a new status, starts nothing.

Pairing never changes a link someone made, and an administrator's `users`
mapping is skipped. Members it cannot pair unambiguously (a shared address, two
accounts with one address, or a member or account already linked elsewhere) are
reported for review instead. A pairing whose address no longer matches, whose
account is disabled, or whose member left the workspace is removed on the next
run. **Unlink**, or `unlink` in the DM, removes a pairing like any link, and
pairing does not restore it until the person links again themselves. Personal
Google accounts carry no Workspace domain and never pair. Addresses are kept with
the sign-in identity only; pairing reports and links hold IDs. An account pairs
only after the person has signed in with the `email` scope requested (see
[authentication](authentication.md)). A link or pairing also gives the account the member's
[task queue](tasks.md).

## Operator setup

1. Configure the existing judgments decision connection and Luna chat connection.
   Personal policy chooses the AI connection/model and a workload reasoning
   override. The initial routine default is `low`; it does not change the chat
   connection's default (for example, Luna `max`). This is a starting point,
   **not a measured quality floor**. Evaluate role-specific drafts before broad
   activation; raise reasoning in the assistant policy if needed.
2. Register/configure a Google OAuth **web** client and a Slack OAuth application
   with user scopes. Existing OIDC/Cloudflare Access login credentials are not
   substitutes for these delegated data grants. Enable the Gmail, Calendar,
   Drive, Docs, and Sheets APIs in the Google Cloud project. Organization policy
   or provider app verification may require an administrator's approval.
3. Copy `deploy/integrations/personal-connections.example.json` outside notebook
   storage. Omit either provider block if unused. Mount client secrets as files.
4. Generate a distinct 32-byte encryption key (or 64 hexadecimal characters),
   store it in the runtime secret store, and mount it at `keyFile`. Restrict key
   and secret files to the server identity. Set
   `PERSONAL_CONNECTIONS_CONFIG_FILE=/run/config/personal-connections.json`.
5. Register exact callbacks for the configured `PUBLIC_URL`:
   - `https://YOUR_HOST/account/connections/google/callback`
   - `https://YOUR_HOST/account/connections/slack/callback`
6. Enable the assistant policy from **My assistant → Administration**, selecting
   the existing Luna connection and optional responsibilities notebook. Users
   then consent to processing and complete their own provider grants.

Requested Google scopes are `openid email profile`, `gmail.modify`,
`calendar.events`, `calendar.calendarlist.readonly`, `drive.readonly`, `documents`,
and `spreadsheets` (Google API names use the
`https://www.googleapis.com/auth/` prefix). Slack uses `channels:read`,
`channels:history`, `groups:read`, `groups:history`, `im:read`, `im:history`,
`mpim:read`, `mpim:history`, `users:read`, `chat:write`, `search:read.public`,
`search:read.private`, `search:read.im` and `search:read.mpim` as **user
scopes**. The search scopes serve [reply reminders](tasks.md#messages-waiting-on-your-reply);
someone who connected Slack before they were requested reconnects to use them.
Providers may grant fewer scopes; operations requiring missing scopes fail closed.

Reply reminders are the one use of a personal connection that needs no AI
processing consent or enabled assistant: a person who turns them on may connect
Slack, and only the task reminder reads it, for who wrote and where. Every other
personal read still needs the assistant enabled with consent.
Receiving bot mentions/DMs and sending private reminders continue to use the
existing configured bot transport and its own event subscriptions/scopes.

Grants are rows of the instance database's `personal_credentials` table, each
sealed with AES-256-GCM under the key and bound to its account and provider. The
database backup carries them; protect the **matching encryption key separately**
in the recovery secret store, since the database never holds it. Without the key,
users must reconnect. Never put the key in Git, notebook backups, or the image.
Every gateway reads the same grants. A refresh claims the grant for its gateway
before calling the provider, so one request on one gateway rotates it; another
request waits on the same gateway and is asked to retry on another. Rotation is
saved before it is used. A refresh whose gateway is gone requests
reauthorization rather than reusing a possibly consumed refresh token. A refresh
the provider refuses as revoked (`invalid_grant` or `invalid_client`) requests
reauthorization too. A rate limit, an unreachable provider, a server error or an
unusable answer keeps a Google grant, and the next request refreshes it again.
Slack rotates refresh tokens, so only its rate limits and requests that never
reached it keep a Slack grant; any other failed Slack refresh may have spent the
old token and requests reauthorization. Disconnect
fences in-flight work. Authorization flows in progress live in the gateway that
began them, so a connection must finish on that gateway.

## Approval, recovery, and budgets

Monitoring consent does not authorize sending mail or Slack messages. Automatic
Gmail draft creation has its own opt-in. Only the employee's own signed-in browser
request on the account page can call `submit_personal_action`,
`prepare_assistant_draft` or `recover_assistant_work`. MCP clients (OAuth or `fmn_`
credentials, whatever scopes they hold, including `data:write`), `/api/v1`, chat,
models and background jobs are refused, and MCP `tools/list` and `/api/v1/actions`
discovery omit these actions. The authenticated account UI owns explicit
external submissions and paid regeneration. Current UI drafts are reviewed in Gmail or copied into Slack before
the employee sends them. Calendar create/update and explicit mail/Slack submission
contracts also exist at the account API, with fresh-source checks and receipts;
they are not exposed as autonomous model tools. There is no automatic calendar
booking, Slack native-draft API, or spreadsheet editing workflow.

Writes use the existing durable effect ledger. Unknown outcomes block additional
writes in that account/provider scope until an administrator reconciles the
retained receipt. Gmail and Slack cannot atomically condition a send on a thread
version; the application checks immediately before submission but cannot promise
to eliminate the provider-side race. Calendar updates use `If-Match` etags.

Paid generation intent, results, cursors, and publication state are checkpointed.
An interrupted generation is not silently retried. **Recent activity** provides
explicit regenerate/discard controls; regenerated work can incur another charge.
Expired previews (30 days by default via policy `retainDays`, never shorter than
the configured lookback plus a day) leave published Markdown and Git intact.
Uncertain generation and external-write receipts are retained for recovery.

Advanced policy fields include `batchItems`, `lookbackHours`, `triageChars`,
`memoryChars`, and `retainDays`. Existing application capacity budgets remain
authoritative. The judge sees a small source excerpt plus responsibilities; Luna sees
the selected evidence and may retrieve additional permitted context. Source
excerpts can miss nuance, so the synthetic regression suite does not establish
real-world triage recall, prose quality, or the cheapest acceptable reasoning.

## Verification

Focused checks cover provisioning/restart, employee isolation, revision conflicts,
OAuth state and browser binding, encrypted storage/tamper rejection, serialized
token rotation/revocation, full Slack thread freshness, paid-call deduplication,
model output isolation from operational state, consent revocation, memory edit
preservation, and real-browser setup/layout.

```sh
go test -race ./internal/catalog ./internal/connectors ./internal/chat ./internal/automation ./internal/decision
go test -race ./internal/web -run '^TestPersonal'
npm run test:browser -- personal-assistant.spec.mjs
```

Synthetic HTTP providers use temporary accounts, Git repositories, and local
fixtures. Real Google/Slack approval, Luna/judgment quality and cost evaluation, live
notification delivery, and production deployment require a separately authorized
pilot; passing fixtures does not verify those external systems.

Provider contracts: [Google OAuth](https://developers.google.com/identity/protocols/oauth2/web-server),
[Gmail incremental synchronization](https://developers.google.com/workspace/gmail/api/guides/sync),
and [Slack user OAuth](https://docs.slack.dev/authentication/installing-with-oauth/).
