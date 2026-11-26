# Agent memory

Agents you connect to ForgetMeNote can keep memory for you: the decisions,
facts, preferences, commitments and lessons that a later conversation would
otherwise have to ask about or rediscover. Memory is Markdown in private
notebooks, so you can read it, correct it and see every change in the notebook's
history. Every client you connect uses the same memory, and so does a direct
message conversation that is yours.

Memory does not need a [personal assistant](personal-assistant.md). It is also
separate from the assistant's own profile files (`AGENTS.md`, `SOUL.md`,
`CONTEXT.md` and `commitments.md`), which that guide describes.

## What memory looks like

A memory file is a note named `memory.md`. Once an agent has remembered
something, **Your assistant → Memory → Open memory** opens yours.

```markdown
# Memory

Items your agents remember. Edit or remove any item; an item you write or change is yours, and agents never rewrite it.

## Preferences
- Wants weekly figures grouped by ISO week, Monday first
  `m3f9a1c2 · preference · from: agent, 2026-03-02 · checked: 2026-03-02 · recheck: 2028-03`
- Always cite the note and revision an answer came from

## Facts
- The orders table is refreshed nightly at 02:00 UTC
  `m7b20e4d · fact · from: agent, 2026-03-04 · checked: 2026-03-04 · recheck: 2026-06 · via: slack`

## Retired
- The orders table is refreshed hourly
  `m51c08aa · retired: 2026-03-04 · replaced by: m7b20e4d · reason: replaced by a newer observation`
```

Each item is a bullet under a topic heading. An item an agent wrote is followed
by a metadata line with its ID, kind, author, dates and, optionally, the
messaging client it came from and an anchor naming the source it depends on. The
second preference above has no metadata line because a person wrote it.

An item's kind is `decision`, `fact`, `preference`, `commitment` or `lesson`. A
preference is what you want from an agent, whether you said it or the agent
inferred it. An item is one line of at most 500 characters, and an agent
remembers 1 to 20 items in one call.

## Turn memory on or off

**Your assistant → Memory** holds the setting that lets agents remember what
they observe. It is on by default, and it stays available when an administrator
has not enabled assistants. With the setting off:

- Agents save only the items you ask them to remember (`userDirected`).
- Agents can still search your memory, by text only.
- Direct messages neither read nor write it.

A client learns the setting when it connects and reads it again only when it
reconnects.

Settings saved before the default was on stored off whether or not the person
chose it, so database migration 0015 turned every stored setting on once; a
choice saved since then holds, including off.

## Where memory lives

Core memory is `memory.md` in your assistant notebook. Each project's memory is
`memory.md` in its own private notebook, `assistant-<account ID>-<hash>`, named
after the project. A project key is a git remote or a project name: the HTTPS
and SSH remotes of one repository share memory, and credentials in a remote URL
are dropped.

Memory notebooks are protected like the
[assistant notebook](personal-assistant.md#private-notebooks-and-shared-responsibilities):
you cannot share them, and other people cannot read them. Instance
administrators and the server operator retain their existing authority; this is
not encryption against the operator.

The first item an agent remembers creates your assistant notebook with the
assistant paused and no AI processing consent. `assistant_status` reports memory
apart from the assistant's `enabled` and `available`, and `capabilities` names
the memory tools, so an agent does not take a disabled assistant for disabled
memory.

A credential limited to selected notebooks can use only memory notebooks that
already exist and that it includes, so one project's memory can be given to a
client tool without the rest; recall marks the others `restricted`. Project
memory notebooks need catalog version 10 (see
[storage transitions](storage-migrations.md)).

## How agents use memory

Agents connected over MCP or the API use three tools:

| Tool | Scope | What it does |
| --- | --- | --- |
| `recall_memory` | `notes:read` | Searches your core memory and, given a `projectKey`, that project's memory, or returns all of both without a query |
| `remember` | `notes:write` | Adds items to core memory, or to a project's memory with a `projectKey` |
| `confirm_memory` | `notes:write` | Reports whether a recalled item still holds: `holds` renews it, `changed` replaces it and `wrong` retires it |

An MCP client learns the setting when it connects: the server's instructions (in
the `initialize` or `server/discover` reply, which is private to the account)
end with a memory section. When memory is on, it tells the agent to keep memory
on its own judgment so you need not explain yourself again in a new
conversation: to search memory with a query describing what it is about to do,
at the start of a task and before a step that queries data or uses other tools,
keyed on the repository's origin remote or the project's name; to remember as it
learns, including how you like data retrieved and analyzed and which tools,
notebooks and parameters worked; and to file what you want from it as a
preference, whether you said it or it inferred it. When memory is off, it says
so and names the setting page. This reaches any client that passes server
instructions to its model, without the companion; Claude Code does.

The Claude Code companion adds a session-start hook. It reads only the
repository's git remote, drops credentials in the URL, and asks the agent to
search that project's memory at startup and after `/clear`; after compaction it
also asks the agent to remember durable items from the work so far. Hooks cannot
call the model when a session ends, so remembering at the end of work still
relies on the agent following these instructions.

Agents write only items you directed (`userDirected`) or, with the Memory setting
on, items they observed. Text that looks like a credential is refused before it
reaches notebook Git. Notebook chat cannot use these tools yet.

## Correcting memory

Edit or delete items like any note. A bullet you write or change is pinned:
agents never rewrite, replace or retire it, and remembering its old text again
counts as a duplicate, not a new item. Every write re-reads the newest version
and never overwrites it.

A preference an agent inferred is filed as the agent's (`from: agent`), so it
stays replaceable and is never taken for your own words; edit it to pin it. When
an agent disagrees with one of your pinned items, the change is never applied:
the new item is added with `disputes` naming yours, and recall marks both
`disputed` for you to resolve.

## How memory stays current

Memory stays current in four ways:

- **Recheck dates.** Each item gets a recheck month by kind: preferences after
  24 months, decisions and lessons after 6, facts after 3, commitments after 1.
  Recall marks overdue items `stale`; nothing is deleted for age alone.
- **Confirmation.** After checking a recalled item, an agent calls
  `confirm_memory`: `holds` renews it, `changed` replaces it, and `wrong` retires
  it. Replaced and retired items move to the Retired section with their reason,
  so remembering them again is a duplicate. Recall lists replacements from the
  last 30 days under `changed`.
- **Tidying as agents go.** Agents are told to fix what a search shows them:
  confirm stale items, retire wrong ones and replace an item rather than add a
  second version, without reviewing all of memory unless you ask. Under the
  embedding policy, `remember` compares each new item with the current items in
  its file. A new item with a similarity of 0.9 or more to one is still added,
  and the result names that item as `similar`: the agent replaces the old one with
  `confirm_memory` `changed` when the new one updates it, or retires the new one
  with `wrong` when it only repeats it. Embedding the file's items there also
  prepares them for later searches.
- **Jev review.** When you have consented to Jev and the instance allows reviews,
  `remember` asks Jev whether each observation replaces an existing item under the
  same heading, and retires that item only on a confident answer. Usage is
  accounted to the personal assistant. The result's `review` is `checked`,
  `skipped` or `failed`; the item is saved either way. A failed review adds
  `reviewReason` (`capacity`, `timeout`, `provider`, `permission` or `error`), and
  the gateway logs `memory review failed` with the failed step, provider status
  and question count, never memory text. A change to one of your pinned items is
  never applied: the new item is added with `disputes` naming yours, and recall
  marks both `disputed` for you to resolve.

## Searching memory

Given a `query`, `recall_memory` returns only the most relevant current items
(10 by default, at most 50), ranked across core and project memory, with each
item's `rank`; `search` reports the mode and how many items were searched. An
item that shares a word with the query always counts. An item found by meaning
alone counts only when its similarity is within 0.1 of the best match, since
scores differ by embedding model; a small memory is not padded with unrelated
items.
Retired items and recent changes are left out. Without a query it returns every
current item, which agents are told to do only when you ask what they remember.
Each item is ranked on its own, by its heading and text, never as part of a
larger passage.

Text ranking always runs. When an administrator has adopted the instance's AI
policy with embeddings on, and your Memory setting is on, search also ranks by
meaning with the policy's embedding model, so an item is found without sharing
words with the query. The query and each item's heading and text are then sent
to the instance's embedding provider; local-only operation withholds them unless
the embedding endpoint is local. The endpoint rechecks the instance policy around
every request. Item vectors are derived: each server process keeps them in
memory, keyed by model and text, and embeds an item again after a restart or an
edit. A search embeds at most 512 items it has no vector for; any others rank by
text. With Memory off, or without the policy, search ranks by text and sends
nothing.

## Direct messages

Direct messages use your core memory too. A Slack DM, or another messaging
client's direct conversation, that is yours reads your core memory as context
and can remember items from the conversation. A DM is yours when it runs as you,
because the Slack configuration maps your Slack user to your account or your
assistant answers your DMs through your connected Slack account, or when you
linked it. Items remembered there carry the client in their metadata line
(`via: slack`), and recall returns it as `via`; Jev reviews them as yours, as it
does for connected agents. Channels, shared conversations and unlinked DMs never
see personal memory. With the Memory setting off, direct messages neither read
nor write it. A conversation that used memory is marked as carrying personal
context, like one that used `assistant_context`. An older release reads a `via`
item as one you wrote and pins it; nothing is lost.

A workspace member whose DMs run as the members' service account links them
without an administrator: they send `link` to the bot in a DM, open the signed
link in the reply within 15 minutes, sign in and confirm. Only that Slack user
sees the reply and only the account holder can confirm, so the link proves both.
The bot then tells the Slack user which account is linked; `unlink` in the DM,
or **Unlink** under **Your assistant → Memory**, removes it. A Slack user links to
one account at a time; another account cannot take over a link until it is
removed. A link carries memory only: the DM still runs as the service account
with its notebooks and data, and what the person may read does not change. An
administrator's mapping takes precedence, and those DMs already use memory. The
reply's link is built when it is sent and never stored. Links live in the
catalog (`messageLinks`); an older release drops them when it rewrites the
catalog, and people link again.

An administrator can also pair workspace members with accounts automatically;
see [pairing by email](personal-assistant.md#pairing-by-email).
