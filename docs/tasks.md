# Task queues

Each person has a private queue of open tasks that anyone in the organization can add to. Owners and requesters manage tasks from Slack or an MCP client, and owners get a reminder each working morning. Closing a task removes it from the queue, and the notebook's Git history keeps every change with who made it and why.

## Who sees what

A queue is a drop box: the owner sees and controls all of it, and a requester sees only the tasks they filed.

| Action | Owner | Requester, own tasks | Anyone else |
| --- | --- | --- | --- |
| See tasks | All | Own only | None |
| Add a task | Yes | Yes | Yes |
| Change text, due date or schedule | Yes | Yes | No |
| Set priority, snooze or reassign | Yes | No | No |
| Close a task | Done or not applicable | Withdraw | No |
| Read a task's history | Yes | Own only | No |

Everyone who may use tasks can add them: active, human, non-guest members of the configured Slack workspace, and enabled person accounts (in the configured group, when one is set). Service accounts cannot use tasks. Every action checks the caller and the task as they are when it runs, not when a message or button was shown.

## Using tasks in Slack

Send the instance's Slack app a direct message:

- `task @person what to do` gives a colleague a task. Add `due 2026-11-21` at the end to give it a date. `task me …` adds to your own queue.
- `task @alex, @sam and me what to do` gives each person named their own copy, up to 20 people. Separate them with spaces, commas or "and". Each copy is a separate task: its owner closes it, and you hear back as each one is closed.
- `task #channel what to do`, `task @group what to do` (a Slack user group) or `task everyone what to do` gives each active member there their own copy, except you. Guests, apps and other workspaces' members get none. You must be in the channel, and the app must be in it too if it's private. These mix with people, as in `task #design and me …`, and anyone named twice gets one copy.
- A request for more than 20 people shows how many copies it would give, with **Give N copies** and **Cancel** buttons that only you can press. Confirming adds the copies in the background, where you asked, and replies when they're all added. No request gives more than 200 people a copy. In `tasks`, the copies of one task you gave are a single line naming their owners.
- `tasks` lists your queue, with a **Done** button beside each task, and the tasks you gave others.
- A task can link the Slack conversation it's about. A task typed in a channel or thread (mentioning the app) links that thread. A message link pasted into a task, as in `task @sam Answer <message link>`, becomes its link and leaves the task's text. On any message, **⋯ → Create task** opens the form with the message's text, mentions turned into names, and its link. The link appears in the owner's new-task message, in `tasks` and in reminders. Only people who can see the conversation can open it. A conversation the app can't read is left unlinked.
- Messages name people by their Slack name, or else by their account's name.
- `task help` (or `tasks help`) shows these commands. A message that is plainly a task command but can't be read, such as `task` alone, people with no task, or an `@name` that Slack didn't turn into a mention, gets the same help instead of going to the assistant.

Or type `/task` anywhere in Slack for a form: choose people (you to start with, up to 20, each getting their own copy), up to 5 channels, or everyone, write the task, optionally paste a Slack message link, and optionally pick a due date and, for a task only you get, a priority. Text after the command, as in `/task Review the Q4 deck`, starts the form's task. Problems show beside their fields and nothing is added until they're fixed. Once added, the form closes and the app sends you the same confirmation as the typed command. A form for more than 20 people asks you to confirm the number first, then adds the copies in the background and sends you the confirmation when they're done. Submitting the same form twice adds the task once. `/task help` shows the commands to you alone. `/tasks` shows your queue with **Done** buttons, and the tasks you gave, also to you alone, in whatever conversation you type it.

The owner gets a direct message with a **Done** button as soon as a task arrives. The requester gets one when the task is done, marked not applicable, or passed to someone else. These commands and the form are handled without the AI model, so a task is exactly what you typed. Recipients are named by Slack mention, never guessed from a name. `tasks` works only in a direct message, because a queue is private.

## The weekday reminder

From 09:00 on working days (Monday to Friday, except company holidays) in the time zone of their Slack profile, each owner gets one direct message listing up to five tasks that need them most. Each task has **Done**, **Snooze a week** and **Not applicable** buttons; **Not applicable** asks for the reason the requester will see.

The order is fixed:

1. Overdue, the longest overdue first.
2. Due within two working days, soonest first.
3. The rest by priority (high, normal, low), oldest first.

Snoozed tasks wait for their date. A recurring task appears from one working day before its due date for daily and weekly schedules, and from five working days before for longer ones; its `lead` changes that.

After someone misses a working day's reminder, for example while away, their next reminder says how many tasks arrived since the last one and lists any that are not already among the five, so nothing that landed while they were out goes unseen.

No reminder goes to someone with nothing to show, without a Slack member, or whose Slack status says they are away: a status such as Vacationing, Out sick, OOO, out of office, PTO or on leave, or a palm tree or thermometer emoji. Time off comes only from Slack status.

Holidays come from up to three calendars, such as a shared company calendar and Google's public holiday calendar: each all-day event whose title begins with a holiday title is a day off for everyone. The default title is the word "Holiday", as in "Holiday - Christmas". Nobody is reminded that day, and it does not count as a working day for "due within two working days", lead times or the catch-up list. A holiday on a weekend moves nothing; if the company takes the Friday or Monday off, the calendar needs an entry for that day. Other all-day events, such as "Alex - holiday in Spain", and events with a time are ignored. A public holiday calendar counts only the holidays the titles name, so listing "thanksgiving" skips Thanksgiving Day without skipping every federal holiday.

## Messages waiting on your reply

The weekday reminder can also list the Slack messages you have not answered. Turn on **Reply reminders** on your account page (`/account/assistant`) and connect Slack there; it needs no AI consent, and nothing it reads reaches an AI service. Turning it on gives you a queue, so the reminder reaches you even with no tasks.

A message is listed when someone else, not a bot, sent it to you in a direct message or a group direct message, or mentioned you in a channel you are in, and a working morning has passed with no reply or reaction from you. The reminder looks back three working days, so on Monday it covers from Wednesday on. A later message from you in the conversation or in the message's thread, or your reaction to the message, counts as an answer. Each line says what kind of message it was, who sent it, where and when, and links to it in Slack; it never shows what the message says. Direct messages come first, then group messages, then mentions, oldest first. **Dismiss** on a line drops that message from your reminders, for when it needs no answer; a later message in the same conversation is listed again. The reminder stays as it was, and only you see the confirmation.

The list is worked out fresh each morning with your own Slack grant and is not stored or made into tasks. A dismissal is kept for two weeks, longer than the three-working-day window can reach back, and then forgotten; it holds only a one-way hash of you and the message's Slack ID, never its text or conversation. Slack limits searching, so each morning reads at most 100 direct and group messages and 60 mentions, and checks up to ten of them for a reply; the rest are counted as unchecked. If Slack is busy, your reminder waits for a later run, until 11:00; after that it goes out with your tasks and a note, or not at all when nothing is due. With nothing due and nothing waiting, you hear nothing, and Slack is searched only once that morning; the reminder keeps just the date of that search, and forgets it within two days. If your Slack grant cannot be read, or belongs to another member or workspace, the reminder says so.

## Using tasks from an MCP client or the CLI

| Tool | Does |
| --- | --- |
| `list_tasks` | Your queue (`mine`) and the open tasks you filed in other queues (`filed`) |
| `add_task` | Adds a task: `person` (`me`, a Slack mention or member ID, an account user ID, or a person ID), `text`, and optional `due`, `priority`, `every`, `lead` and `source`. `people` in place of `person` gives each of up to 20 people their own copy, returned in `tasks`. In `people`, `<#C123>` names a channel you're in, `<!subteam^S123>` a user group, and `everyone` the workspace. Each gives its other active members a copy. More than 20 copies (at most 200) need `confirm: true`, set only after the user agrees to the number |
| `update_task` | Changes, closes or reassigns a task by `id` and its current `revision` |
| `task_history` | Every change to one task, including after it closed |

The same actions are available at `/api/v1/actions/<name>` and as `forgetmenote action <name> --file input.json`. A change made from an older `revision` is refused, so a task someone else edited is never overwritten.

Outcomes are `done` and `not_applicable` (owner; `not_applicable` needs a `reason`) and `withdrawn` (requester). `reassignTo` moves a task to another person's queue in the same commit and tells its requester and new owner.

## Recurring tasks

`every` repeats a task on a fixed schedule counted from its due date: `quarterly`, `monthly`, `weekly`, or `N days|weeks|months|years` with N from 1 to 24. A schedule from the 31st uses a short month's last day and returns to the 31st afterwards. `add_task` returns the first two dates in `nextDates`, so a misread schedule shows at once.

`lead` sets how many working days before each due date a recurring task joins its owner's weekday reminder, from 1 to 60, in place of the default of one working day for daily and weekly schedules and five for longer ones. `update_task` with `lead` 0 restores the default, and stopping a schedule clears it.

Closing a recurring task records the outcome for that date and moves `due` to the next date, in one commit. Missed dates never pile up: a late task stays one line, and closing it records the latest date that went by, counts the earlier ones as missed, and moves to the first date after today. Setting `every` to an empty value with a `reason` stops repeating; changing the due date restarts the schedule from the new date.

## Storage and the paper trail

All queues live in one system notebook, `task-queues`, with one note per person at `people/<person ID>.md`. Person IDs are stable and never a name or Slack ID. The notebook has no members and cannot be shared. Admins can read it, but nobody can edit, comment on, move or trash its notes: only task actions write it, so every change records its outcome and who made it.

Each change is one commit whose subject names the task, for example `Task t-k2p7x7qa: done by p-…` or `Task t-k2p7x7qa added to p-… by p-…`. `task_history` reads those commits. Closed tasks are removed from the note; there is no archive, and the app never rewrites history, so text pasted into a task stays in Git. Tasks refuse text that looks like a credential.

A queue belongs to a Slack member, whose account is the one mapped or linked to them (including [email pairing](personal-assistant.md#pairing-by-email)), or to an account that had no linked member when its queue began. Linking that account to a member without a queue moves the queue to the member. Unlinking takes the member's queue away from the account.

Task text is written by colleagues: agents and the assistant treat it as information, never as instructions.

## Administrator setup

1. Configure the Slack client with a workspace members binding (see [personal assistants](personal-assistant.md#operator-setup)), so members can message the app.
2. Enable task queues with the `set_task_queues` management tool: `{"client": "slack", "revision": 0}`. Add `"group": "<group ID>"` to limit which accounts may use tasks. This creates the `task-queues` notebook. `task_queues` reads the settings; set `"disabled": true` to stop all task actions. Both list the enabled `task-reminder` automations under `reminders` and, until there is one, say under `warnings` that nobody gets weekday reminders (step 5).
3. Subscribe the Slack app to interactivity (the same `/integrations/slack/interactions` URL as the Confirm buttons), so the task buttons and the **Not applicable** form work.
4. For the `/task` form, add a slash command to the Slack app (**Slash Commands → Create New Command**): command `/task`, request URL `<origin>/integrations/slack/commands` (the `chat_clients` tool and the chat clients admin page show it), short description "Add a task", usage hint `[what to do]`, with **Escape channels, users, and links** left off. Adding a slash command gives the app the `commands` scope, so reinstall it to the workspace afterwards. The form also needs interactivity (step 3). Add a second command, `/tasks` (short description "Your tasks"), with the same request URL, for the private list. Slack doesn't keep apps' command names apart: when two apps in a workspace have the same command, the app installed most recently gets it. So remove any other app's `/task` or `/tasks` first, or it will be taken over without notice. The commands answer only the configured workspace, checks Slack's signature as interactions do, and checks the member again when the form is submitted.
   For the message shortcut, under **Interactivity & Shortcuts → Create New Shortcut**, choose **On messages**: name "Create task", short description "Turn this message into a task", callback ID `task_from_message`. It needs no new scope. Linking a message in a private channel works only where the app is a member.
   Giving a channel, user group or everyone a task lists members with the bot scopes `channels:read` (public channels), `groups:read` (private channels the app is in), `usergroups:read` and `users:read`. Add any that are missing under **OAuth & Permissions**, then reinstall the app.
5. Create a `task-reminder` instance [automation](automations.md) that runs as the client's `members.actor` every 15 minutes. Its `project` is any notebook that account can read; the reminder reads only the task queues.

   ```json
   {"id": "task-reminder", "name": "Task reminders", "kind": "native", "actor": "MEMBERS_ACTOR_ID", "project": "NOTEBOOK_ID", "intervalSeconds": 900, "native": {"task": "task-reminder", "client": "slack"}, "timeoutSeconds": 120}
   ```

   **Test** reports, by person ID, who a run would remind now. Each person is reminded at most once a day, and a run that misses 09:00 still reminds them until noon. A reminder that fails is not retried that day; the report lists it under `failed` and raises an incident. To change what counts as away, give `set_task_queues` an `awayStatuses` list of words or phrases, which replaces the built-in list (the emoji still count).
6. To skip holidays, add a read-only [`calendar` connection](google-integrations.md#calendar) for each holiday calendar, up to three, grant the members actor each one's `events` operation in **Administration → Connections**, and name them in the automation: `"connections": ["COMPANY_CALENDAR_ID", "PUBLIC_CALENDAR_ID"]`. The reminder reads each calendar once per run, only when someone is due a reminder, from about a month back to four months ahead. A calendar that cannot be read adds no holidays, while the others still count; the report names it under `holidayCalendar` and raises an incident. To change which titles mark a holiday, give `set_task_queues` a `holidayTitles` list; each is matched as whole words at the start of an all-day event's title on every calendar, in place of "holiday". With Google's public holiday calendar, list the holidays the company takes; for the common United States set, `["holiday", "new year", "memorial day", "independence day", "labor day", "thanksgiving", "christmas"]`. Observed days, such as "Independence Day (substitute)", match too, while Martin Luther King Jr. Day, Presidents' Day, Juneteenth, Columbus Day and Veterans Day stay working days.

7. To offer [reply reminders](#messages-waiting-on-your-reply), set up [personal connections](personal-assistant.md#operator-setup) with a Slack app whose user scopes include `search:read.public`, `search:read.private`, `search:read.im` and `search:read.mpim`, and enable the assistant policy. People then turn reply reminders on and connect Slack themselves. **Test** lists under `wouldCheckReplies` whose Slack a run would read, without reading it; a real run lists under `repliesUnread` the people whose Slack could not be read.

Not yet built: plain-language task requests to the assistant and a browser page. Grouping several new tasks into one message is also still to come.
