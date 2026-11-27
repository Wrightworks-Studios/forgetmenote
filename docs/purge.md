# Permanently erasing content (purge)

A purge removes content from **every version** of a notebook, not just the
current one: a leaked credential, customer data or personal data that must not
stay in the notebook's history. It is the one operation that rewrites history.
Everything else in ForgetMeNote keeps old versions: saving, moving to
[Trash](trash.md) and emptying Trash all leave earlier versions in history and
in backups.

Purge is for content that must be gone. To remove a note while keeping the
ability to restore it, move it to Trash instead.

## What a purge can erase

- **A whole note**, from every version, with its discussion (comments). Its
  records in the Trash index and its archives and in the notebook's locks are
  removed too, and commit messages that name it lose its path.
- **Chosen versions of a note.** Each chosen version is replaced with a
  placeholder that keeps the note's identity, so the note's other versions and
  its history stay.
- **Text**, as an exact string or an RE2 regular expression, from every version
  of chosen notes or of the whole notebook, and from every commit message. Each
  occurrence is replaced with `[purged]`, so the surrounding document keeps its
  shape. This is the tool for a leaked API key.

## Who can purge, and how

Only an **Owner** of the notebook or an instance **Admin** can purge, in the
browser or through the operator command line. Repository notebooks and task
queues are not purged this way.

**In the browser**

1. Open **Permanently erase…** from a note's **⋯** actions menu, from
   **Permanently erase versions…** in the note's **Version history**, or from
   **Permanently erase text…** under **History** in the notebook's settings.
2. Choose what to erase: the whole note, chosen versions, text in chosen
   notes, or text everywhere in the notebook. A pattern is an RE2 regular
   expression; it must not match empty text. The text or pattern is at most
   64 KiB, and a purge names at most 1,000 notes or versions.
3. **Review the plan.** Nothing has changed yet. The plan shows:
   - what it erases: the whole note, the chosen versions, or the exact text
     or pattern and where; a plan an agent prepared says so;
   - the notes and how many of their versions change, and how many revisions
     get new IDs;
   - where else the text appears, outside what you chose, with **Include these
     notes too** to add them;
   - how many commits have an author or committer whose name or email address
     holds the text: a purge keeps every commit's author and committer, in
     the notebook and in its GitHub copy, so these are listed, not erased;
   - for a whole note, the other notes whose links may still name it, by its
     file name or in a wiki link, in how many versions: they keep its name,
     so add a text purge of the name to remove it there;
   - files that hold the text but are not text, which only purging them whole
     removes, and files or other notebooks that share content being erased
     and keep it;
   - notes being edited live, which restart from the erased text, and how
     many of them lose edits not yet saved;
   - how many references in your notes name a rewritten revision; they keep
     working (see below);
   - whether the GitHub copy of the notebook may be force-pushed once. The
     Git export catches up with every new revision in the background, so the
     plan says so whenever the purge rewrites a revision the export may hold
     by the time the purge runs, even one written seconds ago, and the receipt
     says whether the export actually held one;
   - how many private chat transcripts, Knowledge proposals and judgment records
     quoted the content; they are redacted by the system and nobody reads
     them, so the plan gives counts only. They are searched only for what the
     notebook holds: a text purge's text when a note, commit message or live
     edit of the notebook holds it, and for a pattern the texts it matches
     there, never the pattern itself. Only an instance Admin's plan searches
     them for the text or pattern itself. What the notebook holds is whatever
     its writers put there, so only an instance Admin is told how many of
     other people's records quote it: an Owner who is not an Admin is told
     how many of their own chats, proposals and judgment records quote it, and that
     other people's are redacted too, without a number, so a list of guesses
     written into a note tells them nothing of their colleagues' private
     records. Agents never see these counts (see below).
   A plan that cannot run says why, for example when a file *name* holds the
   text: purge that file whole first. A text purge redacts only what people
   wrote, never the notebook's own structure, which the notebook needs to
   open: the field names, IDs, paths, revisions and dates of its Trash, lock
   and comment files, a note's `fmn-id`, and the trailers ForgetMeNote
   writes into commit messages to record who made each revision and how
   (`Gitnotes-Source`, `Gitnotes-User` and the other `Gitnotes-` trailers,
   and the key of a live edit's `Collaboration-Receipt`). Text found there,
   even by chance inside an ID, blocks the plan: narrow the text or pattern,
   or purge only the notes that hold it. Commit messages are redacted in
   every revision, whichever notes the purge names, so text inside the value
   of a `Gitnotes-` trailer, such as an account ID in `Gitnotes-User` or a
   short code inside the hashes of `Gitnotes-Operation` and
   `Gitnotes-Request`, blocks every text purge of it, even one of chosen
   notes. Any other line of a message is what someone wrote, even one that
   looks like a trailer, such as `Jane: please keep these` in an imported
   commit, and is redacted, key and all.
4. Give a **reason** and confirm that you understand the purge cannot be
   undone, then choose **Erase permanently**. The confirmation is bound to
   that exact plan: if the notebook changed since, or the content turned up
   where the purge does not reach (another notebook, or the live edits of a
   note outside the purge), you are asked to review the plan again. A purge
   restarts the live documents of the notes being edited live that its plan
   lists, and a purge of the history those of every note it covers, which
   loses their edits not yet saved: if someone types into one of them
   meanwhile, you are asked to review the plan again too, and the new plan
   counts that note. A purge of the history also restarts a note someone
   only opens for live editing meanwhile, which loses nothing, and the
   receipt counts every note restarted. A plan that changes no revision,
   because only live edits or only records outside the notebook hold the
   text, restarts only the notes it lists: if someone opens another note it
   covers for live editing, or types the text into one, meanwhile, you are
   asked to review the plan again, as the purge would then also restart
   that note and rebuild the notebook's indexes. Once confirmed, the purge
   runs to its end on the server even when the page is closed or reloaded,
   or the operator command is interrupted, and keeps its receipt all the
   same.

The reason must not quote the content being erased; a reason that does is
refused, even when it quotes a single identifying word of an erased note,
such as a card number or a key: any word of four characters or more with a
digit in it, a year or a date included, that no other note holds.

A plan lists another notebook that keeps content being erased only when you
can read that notebook, and an agent's plan only when its credential also
reaches it. The purge keeps what other notebooks store either way, so a plan
never tells whether a notebook you cannot read, such as someone's private
Inbox, holds the same file; its confirmation binds only what the plan shows.
For the same reason a receipt counts, as stored versions deleted, the versions
erased from the notebook; only an instance Admin's receipt counts what storage
deleted, which depends on what other notebooks keep.

**Agents (MCP)** can only *prepare* a purge. `prepare_purge` takes the same
choices (`kind` `note`, `versions` or `text`; `path`, `versions`, `text` or
`pattern`, and `paths`), requires the agent's user to be an Owner or Admin of
the notebook, and changes nothing. It returns the plan, without what would run
it, and a `confirmUrl` for the user. An agent's plan does not count the chat
transcripts, Knowledge proposals and judgment records that quoted the content
(`recordsCountedOnReview`): they are counted only when a person opens the
plan, so an agent cannot read other people's records by writing guesses into
a note and preparing purges of them. The link opens the plan in the browser,
where a notebook Owner or Admin reviews it, gives a reason and confirms it;
it expires after an hour, when the plan and the text it holds are deleted
within a quarter of an hour, and is spent once a purge of the notebook runs. A
notebook keeps at most 64 plans that agents prepared for it; preparing more
ends the ones that expire soonest, never another notebook's. No
MCP tool and no API or MCP credential can run a purge: the routes that run one
accept only a signed-in browser session, a trusted-proxy identity or the
instance access key.

## What a purge does

One purge, holding the notebook's write lock:

1. Rewrites the history from the first affected revision onward. Every later
   revision gets a new ID and carries a `Gitnotes-Purge: <id>` trailer. The
   mapping from old to new IDs is kept. Bundle exports in progress, which
   hold the notebook as it is, are ended first, and an export that records
   itself while the history is rewritten waits for the rewrite and then
   ends itself, so no export serves a new chunk once the history is
   rewritten; their clients start again at offset zero (a later chunk
   answers 410 `export_expired`, and a first chunk the purge ended 503
   `timeout`, which agents may retry as it is). A chunk, or a browser backup
   download, already being sent completes with the notebook as it was. An
   export that would wait more than five seconds is asked to start again
   shortly (503 `timeout`, retryable for agents). Exports end only once
   the purge has passed the checks it makes before it rewrites anything,
   so a purge refused for its reason, its confirmation or a notebook that
   changed since it was planned ends nothing. A write through another
   gateway, or an authority handoff, that lands while the purge stages its
   rewrite still refuses it, after the exports in progress have ended;
   their clients start again as above.
2. Deletes stored content that nothing uses any more.
3. Restarts the live documents of affected notes from the erased text, as a
   new live epoch with the old edit history deleted: those it finds when it
   runs, including a note only opened for live editing after the plan was
   made.
   The browser that ran the purge reloads what it shows of the notebook.
4. Clears what was derived from the content on every gateway: what each
   gateway holds in memory of the old revisions and live documents, search
   passages, the keyword and semantic indexes, the semantic vectors, the
   history search index and the note map. The vectors are rebuilt from the
   whole notebook: where embeddings are configured, every note is embedded
   again, which a paid embedding service charges for.
5. Redacts what quoted the content outside the notebook, without anyone
   reading it: the private chat transcripts of the notebook and of other
   notebooks' chats that drew on it, its Knowledge proposals and consumer
   checkpoints, and its judgment records. For a text purge, every occurrence of the
   text, or of each text the pattern matched in the notebook; for a note or
   versions purge, every whole line of the erased versions (24 characters or
   longer) that no other note of the notebook still holds. Only the values
   people and agents wrote are redacted: a record's field names, IDs, dates
   and types stay, so it still reads, and the content is not looked for in
   them. A record that cannot be redacted and still read is left as it was,
   and the receipt says how many. Revisions those records cite are mapped to
   the new IDs wherever they appear. The bundles the gateway that runs the
   purge is preparing or sending, for browser backup downloads and bundle
   exports, are deleted from its disk; another gateway's are deleted when
   its request ends, and a stopped gateway's when it next starts. A backup
   download still being prepared when the purge rewrote the history is
   refused and asked to start again.
6. Rebuilds the notebook's Git export and flags it, so the backup worker
   force-pushes the GitHub copy once for that purge when the export held
   revisions the purge rewrote. It never force-pushes otherwise.
7. Keeps a **receipt**: who, when, the reason, the kind of purge, how much
   it changed, how many notes being edited live it restarted, and whether
   the next backup force-pushes the GitHub copy. Receipts never hold the
   erased content, the paths it removed or the pattern. As in the plan, a
   receipt counts other people's records outside the notebook only for an
   instance Admin: the Owner who ran the purge sees the counts of their own,
   and another Owner none. Owners see a
   notebook's receipts under **Earlier purges of this notebook** in the
   **Permanently erase** dialog, or with `GET /p/<notebook>/api/purge`, and
   operators with `forgetmenote admin purge list`. A purge that rewrote the
   history but has kept no receipt, because it is still running or its
   gateway stopped, is listed there as `unfinished` (see
   [Operators](#operators), *A purge that stopped before it finished*).

If a purge finds the content only in records outside the notebook, for
example a chat that quoted a secret already removed, an instance Admin can run
it to redact those records; the notebook's history stays as it is. The
receipt of a purge an agent prepared says so.

**References keep working.** Citations, history pins, memory anchors and
revisions an agent remembered resolve through the mapping: reading a note at an
old revision reads the revision that stands for it now, and MCP read tools
accept old revisions and map them. Reading a pin to a version the purge removed
answers **purged** (HTTP 410), not "not found". Writes never map: a save based
on a revision from before the purge is refused as a conflict, so content read
before the purge cannot be saved back.

## What a purge cannot do

- **Undo exposure.** Anything already pushed to GitHub, downloaded, exported,
  emailed or sent to an AI provider must be treated as leaked, as must a
  backup download or export chunk that was being sent when the purge began:
  it completes with the notebook as it was. **Rotate a leaked secret
  regardless.**
- **Clear GitHub's caches.** GitHub may keep cached views of old commits, pull
  requests and forks for a while; fully clearing them may need GitHub Support.
- **Reach earlier backups.** Database dumps, data directory copies and Git
  bundles taken before the purge still hold the content. Delete or re-take
  them according to your retention policy.
- **Reach the previous release's files.** After the move to the database
  (the storage v2 cutover), each notebook's old Git working
  tree and repository (`data/projects/NOTEBOOK/workspace` with its `.git`),
  its `collaboration/` and `captures/` and any `write-journal.json` stay in
  the data directory with the content from before the cutover. No purge
  changes them; while they are there, the plan and the receipt say so.
- **Change who made a commit.** Rewritten commits, including the
  force-pushed GitHub copy, keep their author and committer names and e-mail
  addresses; the plan says how many commits have the text in their
  identities.
- **Keep who wrote a comment.** A comment records its author's account ID,
  which a text purge redacts like any other text of the note's discussion.
  Commits that name the same account usually block such a purge (see step 3
  above), but when one runs, that person can no longer resolve or delete,
  as their author, the comments whose author it redacted; the notebook's
  Editors can still resolve them, and its Owners delete them.
- **Find every paraphrase.** A text purge finds the exact text or pattern; a
  note or versions purge finds whole quoted lines. A record that quoted part of
  a line, or reworded it, keeps that.
- **Redact field names.** Chat transcripts, proposals and judgment records are
  redacted in their values; content a record kept as a field name, which the
  system normally chooses, stays.
- **Reach drafts kept in browsers.** A private draft of an affected note,
  saved in a browser before the purge (the one that ran it included), stays
  there until it is saved or discarded; ask people who edited the note to
  discard their drafts.

## Operators

`forgetmenote admin purge` runs a purge on the **running** instance through
its own purge routes, with the instance access key in `DATA/admin.token`, so
the process that serves the notebook rewrites it, rebuilds its Git export and
cleans what was derived from it, exactly as the browser does. Run it on the
server, as the instance's user:

```sh
# Text to erase goes in a file (or stdin with -text-file -), never on the
# command line, which other users and shell history see.
forgetmenote admin purge prepare -notebook NOTEBOOK -kind text -text-file key.txt -plan purge.plan
forgetmenote admin purge execute -notebook NOTEBOOK -plan purge.plan -reason "Leaked staging key" -operator "Your name"
forgetmenote admin purge list -notebook NOTEBOOK
# Only when a receipt says the Git export was not rebuilt, with the instance
# stopped (a running instance rebuilds it on its own the next time it exports):
forgetmenote admin purge rebuild -notebook NOTEBOOK -purge PURGE
```

- `prepare` prints the plan, without the text or what runs it, and writes the
  plan to a new private file (mode 0600). Other kinds take `-kind note -path
  NOTE` or `-kind versions -path NOTE -versions REV,REV`; a text purge takes
  `-pattern-file` for a pattern and `-paths NOTE,NOTE` to limit it to notes.
- `execute` runs the plan in that file, prints the receipt and deletes the plan
  file, which held the text. Delete `key.txt` yourself. When the instance
  refuses the plan because the notebook changed since, the plan can never
  run, so its file is deleted too: prepare again. After any other refusal,
  such as a reason that quotes the text, the file stays for another try;
  delete it if you give up.
- `-server` is the instance's local address (default from `LISTEN_ADDR`) and
  `-origin` its public origin (`PUBLIC_URL`, or the instance configuration);
  `-data` locates `admin.token`.

The receipt records the access key's account with the `-operator` name.

**A purge that stopped before it finished**

A purge rewrites the history in one transaction, then rebuilds the Git
export, cleans what was derived from the content and keeps its receipt. If
the gateway running it stops in between (a deploy, a crash, running out of
memory), the receipt list shows the purge as `unfinished`, with what it did
not do. Then:

- A purge that finished but whose receipt the database refused, for
  example while it was unreachable, is not one that stopped: the gateway
  that ran it keeps the receipt in its data directory (`purge-receipts/`)
  and keeps it in the database once it can, within a quarter of an hour or
  when it next starts, in place of an `interrupted` receipt another gateway
  kept meanwhile. Until then it is listed as `unfinished`.
- A gateway that starts finishes what needs none of the erased content: it
  resets the notebook's search and Knowledge indexes on every gateway, ends
  the plans agents prepared for the notebook and the bundle exports in
  progress, and keeps a receipt marked `interrupted` that says what was not
  done. A purge still running on another gateway is left to finish there.
- The next time the notebook's Git export runs (its catch-up, within five
  minutes; a backup; or `admin storage export`, with or without `-recreate`),
  it rebuilds the export for that purge and flags it, as the purge would
  have, so the backup worker replaces the GitHub copy once. Only a purge the
  notebook recorded authorizes that. When the export is too damaged to
  rebuild in place, the export fails; `-recreate` then replaces it with a
  new repository rebuilt for the purge, whose flag still names the old
  branch commit and every commit the notebook's purges rewrote, so the
  worker replaces, once, a GitHub copy at any of them, even one it last
  pushed before that branch commit (restart a worker that follows the
  export). The new repository replaces the old one only once it is
  complete: a rebuild that fails, for example on a full disk, leaves the
  old export and its flag as they were, for another `-recreate`.
- The chat transcripts, Knowledge proposals and judgment records that quoted the
  erased content may not have been redacted, and nothing can find them any
  more from the notebook, which no longer holds it. An instance Admin redacts them with
  a text purge of the erased text, or of each erased line of a note or
  versions purge: a plan that finds the text only outside the notebook
  redacts it there.

**After a purge**

- Rotate any leaked secret.
- The next backup pushes the rebuilt export, force-pushing the notebook's
  GitHub copy once when the receipt says so; check that it ran (the purge
  flag in the export repository is cleared when it is pushed).
  If the export could not be rebuilt, the receipt says so and names the
  purge: stop the instance and run
  `forgetmenote admin purge rebuild -notebook NOTEBOOK -purge PURGE`, which
  rebuilds the export and flags it for that purge.
- Delete or re-take backups taken before the purge if they must not hold the
  content.
- If the plan or the receipt says the previous release's files for the
  notebook are still in the data directory, they hold the content from before
  the cutover. Check that the offline freeze backup taken at the cutover holds
  them, which is what a rollback restores, then delete them from the live
  data directory: `data/projects/NOTEBOOK/workspace` (the working tree and its
  `.git`), `collaboration/`, `captures/` and `write-journal.json`. Re-take or
  delete every complete backup of the data directory that included them.
- If the receipt warns that records outside the notebook could not all be
  cleaned, run the same text purge again as an Admin: a plan that finds the
  text only in those records redacts them.
- With several gateways, the purge rebuilds the export itself only when the
  gateway that runs it also runs the notebooks' Git export; otherwise its
  receipt says the export was not rebuilt, and the gateway that runs the
  export rebuilds it for the purge the next time it exports the notebook,
  within five minutes.
- Browsers, the one that ran the purge included, may keep a private draft of
  an affected note, saved before the purge, until it is saved or discarded;
  ask people who edited the note to discard their drafts.

## Limits

Planning reads the notebook's whole history, and running a purge rewrites
every revision from the first affected one and rebuilds the Git export: on a
notebook of 20,000 revisions that takes seconds to a minute. A plan lists at
most 200 versions per note and counts all of them. Notes, transcripts and
records are scanned in full; a note or versions purge reads at most 64 MiB of
erased versions for the lines it looks for elsewhere, and its notes at the head
for the lines they keep; a pattern purge looks for at most 100,000 distinct
matches. The plan says when some could not be looked for. Running a purge
ends every bundle export in progress on the instance, not only the purged
notebook's, since an export does not record its notebook, and deletes every
temporary bundle on its gateway's disk, which does not record it either. An
export that starts while a purge rewrites a notebook waits up to five seconds
for it, and is then asked to start again shortly.

With several gateways, a bundle another gateway is preparing or sending stays
on that gateway's disk until its request ends, and a bundle or import upload
that a gateway left when it stopped mid-request stays until that gateway
starts again, which deletes it before it serves.
