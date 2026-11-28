# Feature modules

Every ForgetMeNote instance runs the same binary with the same features. An
operator can turn feature modules off in [`instance.json`](instance-configuration.md)
with `modules.off`. Every module is on unless the list names it. With every
module off, the instance still runs its notebooks: storage and editing, live
collaboration, comments, history, locks and Trash; accounts, roles and
sign-in; keyword and semantic search; Inbox and Quick notes; the Git export and
backups; purge; and the browser, `/api/v1`, the CLI and MCP.

Modules are not plugins. The list is fixed in code, nothing is loaded at
runtime, and the release image runs every module. To leave an integration's
code out of the binary, see [optional integrations](integrations.md).

```json
{"version": 1, "modules": {"off": ["tasks", "messaging", "personal"]}}
```

## The modules

| Module | What it covers | Requires |
| --- | --- | --- |
| `chat` | [Notebook chat](chat.md): conversations, the chat engine's runs and research | |
| `automations` | [Automations](automations.md): scheduled and event jobs, [repository packages](automation-packages.md), script runs | |
| `judgments` | [Judgments](judgments.md): source ranking, citation checks, notebook reviews and organization | |
| `data-connections` | Database, HTTP, MCP, [email, Sheets and Calendar](google-integrations.md) connections and their calls | |
| `sources` | [Archived sources](sources.md): originals of PDFs, office files, email, images and recordings, with text the server extracts or a connected agent supplies | |
| `repositories` | [Repository-connected notebooks](repository-notebooks.md) and their map | |
| `tasks` | [Task queues](tasks.md), `/task` and `/tasks`, reminders | `automations` |
| `messaging` | The Slack app and message delivery | `automations`, `tasks`, `memory`, `chat` |
| `personal` | [Personal employee assistants](personal-assistant.md) and their personal connections | `automations`, `messaging` |
| `memory` | [Agent memory](memory.md) | |
| `managed-backups` | [Managed private backups](managed-backups.md) | |

A module that requires another cannot run without it. Turning a module off
therefore means turning off every module that requires it too: `automations`
off takes `tasks`, `messaging` and `personal` with it, and `memory` or `chat`
off takes `messaging` and `personal`. Startup refuses a list that leaves a
module on without one it requires, and names both. It also refuses an unknown
name, so a misspelt module never runs while its operator believes it is off.

Other uses are soft: a module that uses another while it is on carries on
without it, and what it asks of the off module answers that it is not
available. Chat installs no judge while `judgments` is off, for example.

## What turning a module off removes

An off module is absent wherever a person or an agent could reach it:

- **Tools.** None of its actions is offered over MCP, `/api/v1/actions`, chat
  or automations. A call by name answers that the action is not found.
- **Guidance.** The MCP instructions an agent receives, the descriptions of the
  tools that remain and the companion packages the instance offers for download
  name none of its tools. The Claude Code package's memory hook comes only with
  `memory`.
- **Routes.** Its routes answer 404, including the endpoints that authenticate
  their own callers: the GitHub webhook (`repositories`), the backup worker
  protocol (`managed-backups`) and the Slack event, interaction and command
  endpoints (`messaging`). The GitHub App connection flow that repositories and
  managed backups share runs while either is on.
- **Workers.** It runs no worker and takes no lease. No automation run that
  needs it is scheduled or claimed.
- **Settings.** Its settings stay unread, along with the credentials and
  worker secrets they point to.
- **Browser.** Its controls and settings are left out, and Administration's
  instance status and AI settings say it is off.

| Module | Unread settings | Duties not taken |
| --- | --- | --- |
| `chat` | none: the AI connections in `ai.chat` are still read, since judgments, archived sources and semantic search use them | `chat-recovery` |
| `automations` | `integrations.automations`, `integrations.python` | `automation-scheduler` |
| `judgments` | `ai.decisions` and chat's judgment settings | `jev-scheduler` |
| `data-connections` | `integrations.data` | |
| `sources` | none of its own | none: its source jobs are not resumed |
| `repositories` | `integrations.repository` | `repository-refresh` |
| `tasks` | none of its own | |
| `messaging` | `integrations.slack` | |
| `personal` | `integrations.personal` | |
| `memory` | none of its own | |
| `managed-backups` | `integrations.backups` | `backup-expiry` |

## What stays

An off module keeps its data. Migrations still run, its records and files stay,
and turning the module on again finds them as they were. Cleanup still reaches
it: deleting an account, removing someone's notebook access and purge clear an
off module's private data. For example, a purge still redacts chat transcripts
while `chat` is off.

- **Archived sources** stay readable notes with their originals, but they are
  neither updated nor verified while `sources` is off. Startup retires the
  source jobs left from when the module ran and removes their uploads at once,
  unless another gateway that still runs the module holds them.
- **Repository notebooks** stay listed, but their files cannot be read or
  refreshed while `repositories` is off. Disabling a person still pauses the
  repository sources they connected, and their offboarding report still lists
  their repository and backup connections.
- **Managed backups** receive nothing while `managed-backups` is off.
- **Automations.** A run queued while a module ran stays queued until the
  module is on again. Service accounts are part of the core. So is resolving an
  unknown effect, since data and personal connections record effects too.
  Recipe notes are still found (`find_recipes`), without the health of the
  automated checks that verify them.
- **Task queues** work without reminders or notices while `messaging` is off.
- **Slack links** can still be removed while `messaging` is off.
- **Assistant settings** stay as each person saved them. While a module is
  off, a person can still turn off what is theirs: pause their assistant or
  withdraw its AI processing consent (`personal`), and turn off memory (`memory`)
  or reply reminders (`tasks`). Turning any of these on, or changing anything
  else about the assistant, is refused until the module is on again.
- **Data watches** a person saved are skipped and kept while
  `data-connections` is off, and cannot be changed until it is on again.
- **Authority handoffs.** A notebook authority handoff needs its repository
  source and, to GitHub, its managed backup. It cannot start while either
  module is off, and a gateway does not start while a handoff is pending and
  a module it needs is off.

## Limitations

- **Reply reminders.** Turning reply reminders on needs the personal module's
  assistant policy, and listing the replies a person is waiting on needs a
  personal Slack connection. So with `personal` off, nobody can turn reply
  reminders on, and those already on list no waiting replies, though `tasks`
  runs.
- **The packaging command.** `forgetmenote plugin package` writes a
  companion for an instance that runs every module. The companion a person
  downloads from the instance follows its modules.

## Changing the set

`modules.off` is read once at startup, before recovery. To change it, restart
every gateway, since a configuration reload with a different set is refused.
The startup log and Administration's instance status list every module and
whether it is on.

Gateways that serve one instance should run the same set. Each gateway records
the modules it turns off. While live gateways disagree, each logs a warning,
and Administration's instance status lists every gateway with its set.
