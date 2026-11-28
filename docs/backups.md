Managed private GitHub destinations can also be configured in Notebook settings. See [managed GitHub backups](managed-backups.md); all remote pushes still run in the external worker.

# Git remotes, backup, and restore

Repository connections and refresh checkpoints live in the instance database, and retained source evidence and index generations in its blob storage, by default under `/data/repository-sources`. Include the database and the entire tree in stopped-instance backups; App keys and provider configuration are separate protected assets. Source Git caches and derived search indexes are reconstructible, but retained-evidence manifests, connection/consent state and receipts are not. Rebuilding vectors can incur provider charges when embeddings remain enabled. Notebook bundles and host-managed notebook Git pushes exclude this tree. See [repository notebook recovery](repository-notebooks.md#storage-backup-and-upgrades).

Private AI conversations, stream events, provider continuation, review coverage, action receipts and usage are `chat_conversations` rows in the instance database, outside notebook Git. Include the database in every full stopped-instance backup. A notebook bundle does not preserve chat. Operator configuration and mounted provider secrets are separately backed-up deployment assets; see [chat storage and recovery](chat.md#storage-recovery-and-backup).


Judgment settings, review jobs, citation checks, notification state and usage, and
Quicknotes settings and workflow references, are rows of the instance database
([Postgres status](postgres.md#status)). Capture transfer reservations and
receipts are each notebook's operation receipts there too. The catalog also
preserves protected Inbox ownership; never restore captures without it.

An application image that predates protected Inboxes must not run after an Inbox
has been provisioned, even if its current folder is empty: its Git history remains
private, and the older image ignores the sharing restriction. The release helper
refuses that rollback and retains stopped data for a forward repair. See
[Quicknotes recovery](quicknotes.md#backup-upgrade-and-rollback).

## Optional host-managed Git backup

ForgetMeNote keeps notebook history in its database, writes a Git export of each notebook, and never pushes or pulls notebook history. In operator SSH mode, the host owns remote configuration, credentials, scheduling, and status. Optional managed GitHub mode adds in-app consent and short-lived credentials; its external worker still owns every push. Optional repository connections fetch upstream code into a separate read-only cache. GitHub authorship linking is independent and only sets commit attribution.

Each notebook's Git export is the bare repository `data/exports/NOTEBOOK.git`, on the one branch `main` ([Git export repositories](#git-export-repositories)). On the Docker host, use Git with a dedicated SSH deploy key or an HTTPS credential helper configured for the backup operator. Keep credentials outside `/data` and outside the application container; never put tokens in URLs. Verify SSH host keys before accepting them. Use the same Unix UID that owns the project data, or a deliberately configured backup account with the required access. No repository-wide `safe.directory=*` workaround is needed.

Create an empty private repository at your preferred provider yourself. This example runs **on the host**, with an absolute path to the bind-mounted project and an SSH URL you control:

```sh
PROJECT_REPO=/srv/forgetmenote/data/exports/my-project.git
BACKUP_URL=git@github.com:your-account/my-project-backup.git
GIT_TERMINAL_PROMPT=0 GIT_SSH_COMMAND='ssh -o BatchMode=yes' \
  git -C "$PROJECT_REPO" -c core.hooksPath=/dev/null -c gc.auto=0 \
  push "$BACKUP_URL" main:refs/heads/main
```

The explicit URL avoids needing to configure `origin`. The command disables repository hooks and automatic maintenance and pushes only committed history to `main`, without force. It can run while ForgetMeNote is saving: a revision exported after Git reads `main` will be included on the next push. This is a content backup, not a consistent snapshot of the whole instance. Never write to an export repository or run Git maintenance in it; ForgetMeNote maintains it.

For scheduled backups, run the same command from your host's existing scheduler under the backup account. Configure that scheduler to prevent overlapping jobs, retain failure logs, and notify the operator on failure; the app does not track backup status. Authentication and network errors leave local saves intact. A divergent remote is an error requiring operator review, never a reason to add `--force`. The app does not need the backup credentials or network connectivity to the Git provider.

For event-driven backups, the optional [host-side Git watcher](../deploy/git-backup/README.md) debounces committed revisions and runs as a separate systemd user service. It includes startup/periodic reconciliation, retry backoff, and a local status file. It does not change the app image or require Git credentials inside the app.

Verify a backup by cloning the remote into a separate directory and checking notes, attachments and `git log`. A remote on the same machine is not protection against loss of that machine. For Docker named volumes, use an operator-managed backup job with access to the volume rather than relying on Docker's private host paths; the example above assumes a bind mount.

### Git export repositories

With storage v2, notebook history lives in the instance database and blob store,
and ForgetMeNote keeps a bare Git repository for each notebook at
`data/exports/NOTEBOOK.git`, on the one branch `main`. It writes every new revision
there in the background (and catches up every five minutes), repacks and checks
each repository with `git fsck --strict` at most hourly, and never pushes. Commit
IDs and contents are those of the notebook's history, byte for byte, including
history imported from a previous release, so existing remotes continue by
fast-forward. Point the backup command above or the
[host-side Git watcher](../deploy/git-backup/README.md) at the export repository.
Only one process exports a data directory at a time; do not write to the export
repositories yourself. An export is reproducible from the database: one that is
lost, diverged or damaged is written again from scratch, with the instance
stopped, by `forgetmenote admin storage export -recreate` (below).

Every revision is one Git itself would write. A write that names a file or folder
`git fsck` takes for `.git` on any platform (NTFS's short name `git~1`, `.git`
followed by spaces, dots or a `:` stream name, or `.git` with code points HFS+
ignores) is refused, and so is a commit message with a NUL. A notebook that an
earlier storage v2 build let hold such a name or message still opens and reads.
Such a file can be moved elsewhere, or moved to the Trash (alone, or by an edit
group that deletes it) and restored from there under another name, but the
revisions that hold it stay in its history: its export fails `git fsck --strict`,
which maintenance reports every hour, and its backups do not import.

The export's branch only ever moves forward. A purge, the one operation that
rewrites notebook history, rebuilds the export, drops every object the rewritten
history does not reach, and writes `forgetmenote-purge.json`, which lets the
watcher replace the remote branch once for that purge (see
[purges](../deploy/git-backup/README.md#purges)). The flag is replaced, and the
watcher removes it, only under the lock of `forgetmenote-purge.lock` beside it.
Without that flag the watcher never force-pushes. A purge cannot reach copies
made before it: database dumps, blob store and data directory copies, bundles and
anything already pushed or downloaded still hold the purged content
([purge](purge.md)).

### Rebuilding the Git exports

With the instance stopped, `admin storage export` brings each notebook's Git
export (`data/exports/ID.git`) up to its head, repacks it, checks it with `git
fsck --strict` and verifies it against the notes store:

```sh
forgetmenote admin storage export -data /data   # every notebook the catalog lists
forgetmenote admin storage export -data /data -notebook ID -recreate
```

It prints one JSON line per notebook and fails if any notebook fails. A serving
gateway keeps its exports current by itself, so the command is for repair.

`admin storage export` does not repair an export: one whose branch names history
the notebook no longer has, or that fails `git fsck --strict`, fails the
command. The exception is the export of a notebook whose history a
[purge](purge.md) rewrote without rebuilding the export, because its gateway
stopped in between or because another gateway than the one that runs the export
ran it: when a purge the notebook recorded rewrote the commit the export's
branch names, `admin storage export`, with or without `-recreate`, rebuilds the
export for that purge and flags it, as the purge would have, so the backup
watcher replaces the remote's purged commits once. A running instance does the
same on its export's catch-up, within five minutes, so this needs no command ([a
purge that stopped before it finished](purge.md#operators)). When a purge's
receipt says its export could not be rebuilt, `forgetmenote admin purge rebuild`
rebuilds it with the instance stopped, as [after a purge](purge.md#operators)
says.

Otherwise add `-recreate` to delete such an export and export the notebook again
from scratch. The new repository is a new directory, so restart a backup watcher
that follows it; a remote that holds commits the notebook no longer has then
blocks as diverged, for review. A purge flag the watcher has not pushed yet is
kept.

### Upgrading from built-in remote pushing

The legacy `remote` CLI command, remote/push API endpoints and synchronization controls, `GIT_REMOTE`, and `PUSH_INTERVAL` have been removed. New managed backup controls use a separate authorization/worker boundary. Old environment variables no longer configure or schedule anything. Remove them from deployment configuration and arrange an external backup schedule if desired. Existing `.git/config` remotes and all commits are preserved, but ForgetMeNote does not use those remotes. Remove any backup SSH/HTTPS credential mounts from the application; move credentials to the operator's host configuration. GitHub identity-linking secrets remain separate and are still supported.

## Snapshot destinations and instance snapshots

Instead of pushing to a Git remote, the [backup worker](../deploy/git-backup/README.md)
can keep versioned snapshots. The application never uploads them; the worker does.

- **Notebook snapshots** ([snapshot destinations](../deploy/git-backup/README.md#snapshot-destinations)):
  verified Git bundles of one notebook's export in a folder or an
  [rclone](../deploy/git-backup/README.md#rclone) remote, at most one per interval,
  with daily and weekly retention. Each one restores with `git clone` or
  **Import notebook** ([restoring snapshots](../deploy/git-backup/README.md#restoring-snapshots)).
- **Drive snapshots** that people connect in Notebook settings run in the
  [managed worker](../deploy/git-backup/README.md#managed-drive-snapshots), with a
  short-lived token for one Google Drive or Dropbox folder.
- **Instance snapshots** ([instance snapshots](../deploy/git-backup/README.md#instance-snapshots)):
  a daily archive of the whole instance, taken without stopping it through the
  capture command: the database dump, the capture of the data directory and the
  configuration paths you list. The destination must be an rclone `crypt` remote,
  since the archive holds the instance's secrets. Restore it as
  [complete instance backup](#complete-instance-backup) describes.

After a [purge](purge.md#what-a-purge-cannot-do), the worker deletes the snapshots
taken before it once a newer one verifies. Snapshots of `inbox-*` and
`assistant-*` notebooks send a person's private history off the machine.

## Notebook export and import

Notebook settings offers **Download complete Git bundle**. It contains that project's committed Markdown, ordinary Git assets, discussion JSON, and Git history. Read access includes export access. It excludes user permissions, OAuth state, keys, custom CSS, and unsaved drafts.

In the notebook dropdown, choose **Import notebook…**, or select **Import notebook** in the creation dialog. Enter a new name and ID, choose the `.bundle` file, and submit. Admins and people with **Can create notebooks** can import; the importer becomes Owner. Existing notebooks are never replaced, and other people receive no access until granted separately. The server stays online during import.

Browser imports accept complete SHA-1 Git bundles with `HEAD`, up to 32 MiB uploaded, 256 MiB of expanded Git objects, and 100,000 objects. Individual current files must fit the app's 32 MiB limit. The current tree may contain only notes, `assets/`, discussion files under `comments/`, and the `.gitnotes-locks.json` and `.gitnotes-trash.json` indexes with the Trash's numbered `.gitnotes-trash-N.json` archives; other files (including `.gitattributes` and hidden directories), symlinks, submodules, unsafe paths, unsupported refs, incomplete or damaged history, and an invalid Trash index or archive are rejected before publication. Only one import runs at a time. The import stores the bundle's history with every commit ID kept, in one notebook the instance does not have yet. For a larger bundle, raise `FMN_LIMIT_BUNDLE_BYTES`, `FMN_LIMIT_IMPORT_BYTES` and `FMN_LIMIT_IMPORT_OBJECTS` ([resource budgets](deployment.md#operator-resource-budgets)) and restart before importing. Git LFS objects are not included in Git bundles.

Restore independently, with no ForgetMeNote software required:

```sh
git clone project.bundle restored-project
git -C restored-project log --oneline
```

Git LFS objects and untracked files are not in a Git bundle. This version does not support Git LFS; uploads are ordinary Git blobs, at most 32 MiB each. Do not assume an externally imported LFS repository is fully backed up by a bundle.

## Complete instance backup

A complete and consistent instance backup has three parts, taken together while
the application is stopped, or while it serves with a
[live capture](#live-capture):

1. **The database** (`pg_dump`). It holds every notebook's revisions (commits,
   trees, changed paths and files under 8 KiB), live documents and their pending
   updates, operation receipts, authority and purge records, and the whole
   control plane: catalog, sign-in state, chat, knowledge, automations and the
   rest ([notebook content](postgres.md#notebook-content)).
2. **The blob store.** It holds the content blocks of every notebook file of
   8 KiB or more (`blocks/`), source jobs, repository source generations and
   evidence, and export bundles. Without `server.blobs` it is the data
   directory itself; with S3, back up the bucket prefix through its versioning
   and recovery points ([blob storage](postgres.md#blob-storage)).
3. **The data directory**, preserving permissions: the recovery key
   (`admin.token`), `proxy.secret` and `custom.css`, the Git exports
   (`exports/*.git`) with any purge flag not yet pushed, and derived caches.

```sh
docker compose stop forgetmenote
docker compose exec -T postgres pg_dump -U forgetmenote forgetmenote > forgetmenote.sql
tar -czf forgetmenote-instance.tar.gz data
docker compose start forgetmenote
```

Backups taken before the storage v2 cutover hold the previous release's notebook
repositories (`data/projects/*/workspace`), with every notebook's content and
history as it was then, which no [purge](purge.md) reaches.

Also preserve `.env`, `secrets/`, any `instance.json`, host backup credentials and scheduler configuration, and relevant SWAG configuration separately and securely. The backups contain authentication secrets and permission metadata; encrypt off-host copies. Git remotes and exports alone are not a full instance backup, and a remote on the same server is not disaster recovery.

To restore, stop ForgetMeNote, load the dump into an empty database, put the saved `data/` at the Compose volume path (and the bucket prefix back, with S3), ensure ownership matches PUID/PGID, restore the relevant private configuration, and start the service. Restore all three parts from the same backup: a file version whose blocks are missing cannot be read, and an export whose branch is ahead of the restored database stops as diverged until `forgetmenote admin storage export -recreate` writes it again; a remote that already received the newer commits then blocks as diverged, for review. Verify users, project grants, recent notes, images, and history. Do not unpack over a live data directory.

To recover one notebook from a bundle into a fresh instance, use **Import notebook** with a new ID, as above. Notebook permissions stay in the instance catalog and must be assigned separately. An import never replaces an existing notebook.

### Live capture

`forgetmenote admin capture -out DIR` takes the same three parts while the
instance keeps serving. The backup worker's instance mode runs it
([Git backup worker](../deploy/git-backup/README.md)); run it in a gateway
container that has the data directory with the Git exports, with the
instance's configuration. DIR must not exist and must be outside the data
directory and the blob directory; the capture creates it `0700` and every file
in it `0600`:

- `data/`: `admin.token`, `proxy.secret`, `custom.css`, purge receipts the
  database has not taken yet (`purge-receipts/`), and each Git export
  (`exports/<notebook>.git`) with its purge flag. The capture never runs Git in
  an export: it copies the branch, then the objects, and has Git repack, prune
  and check the copy, copying again when a repack or a purge's rebuild replaced
  files meanwhile.
- `blobs/blocks/<sha256>`: exactly the content blocks the snapshot's file
  versions use, each checked against its hash and size.
- `blobs/repository-sources/`: the index generation each source state in the
  snapshot names, and the retained evidence.
- `capture.json`: `format` (1), `created` (the snapshot's time), `snapshot`,
  `purge` (the latest purge in the snapshot, or `-`), the `blocks` and `bytes`
  copied, `version` (the build's revision, the schema and the reader version),
  and the `included` paths and `excluded` ones, each with its reason. It holds
  no secret.

The exports and files are copied first, so no export is ahead of the database
copy. Then the capture takes the `notestore-block-sweep` lock as a Postgres
session lock, begins a `REPEATABLE READ READ ONLY` transaction and exports its
snapshot (`pg_export_snapshot()`), and keeps both until it ends. While it holds
the lock no block sweep runs and a purge's removal of the blocks it freed
waits, so every block the snapshot uses is still there to copy, and no block a
purge or a later write freed is copied. Newer blocks need no lock: a write puts
its blocks before its transaction commits, and a block never changes. An export
whose branch is no revision of the snapshot (a purge rewrote the notebook
before its export was rebuilt, or the notebook was deleted) is removed from DIR
and listed in `excluded`, so a capture holds nothing a purge in its snapshot
erased from a notebook. A capture whose snapshot fell while a purge ran may
still hold chats, proposals and judgment records that quote what it erased
(the purge redacts them after its rewrite commits); capture again once its
receipt is kept.

Once DIR is complete the capture prints one line,
`snapshot <ID> purge <ID or ->`, and waits up to 30 minutes for one line on its
input, while the caller dumps the database with that snapshot
(`pg_dump --snapshot=<ID>`). On `done` it ends the snapshot, releases the lock,
prints `ok` and exits 0. Any other line, the end of its input, the wait running
out or a signal end the snapshot and release the lock too; it exits 1 with a
short reason on standard error and leaves DIR, which the caller deletes. A call
it cannot run exits 2. A capture waits up to 15 minutes for a running sweep or a
purge's block removal before it fails; a purge whose removal outwaits a capture
leaves its blocks to the next sweep and says so in its warnings. The open
snapshot holds back vacuum on the instance database until it ends.

A capture leaves out, and lists in `excluded`:

- source jobs (`blobs/source-jobs/`): private, temporary state that expires
  within 24 hours, which cannot be copied consistently with the snapshot without
  pausing conversions. A restored instance cannot return these jobs' results;
  prepare those sources again.
- bundle downloads (`blobs/exports/`), which last 15 minutes.
- derived caches, rebuilt without provider charges: each notebook's keyword,
  history and map caches (`data/projects/`, with the files there from before
  the storage v2 cutover, which no purge reaches), each gateway's repository
  mirrors and lexical
  indexes (`data/repository-sources/`), working space (`data/cache/`) and
  prepared Python environments (`data/automations/python/`).
- a source's index generation a refresh retired during the capture: the
  restored instance builds that index again, which embedding and map
  classification charge for when they are enabled.

Configuration and mounted secrets outside the data directory are not in a
capture; preserve them as above. Restore a capture as a stopped backup: load
the dump, put `DIR/data/` at the data directory and the contents of
`DIR/blobs/` into the blob store (the data directory without `server.blobs`,
the bucket prefix with S3).

## Recovery credentials

`docker compose exec forgetmenote forgetmenote token` displays the recovery key. It has full instance authority. To rotate it, stop the service, replace `data/admin.token` with a newly generated random secret (at least 32 characters, 0600), then restart. Existing recovery administrator browser sessions become invalid. Personal OIDC browser sessions remain independent. Existing OAuth grants are independent: to revoke all browser sessions and every AI connection, stop the app, preserve a database backup, delete the stored sign-in state as [authentication](authentication.md) describes, then restart; clients must reconnect.

## External Git maintenance

After the storage v2 cutover, ForgetMeNote does not read notebooks from Git working trees, so editing, pulling or merging in a previous release's repository changes nothing it serves. Never write to the export repositories or run Git maintenance in them: ForgetMeNote repacks and checks them itself, and an export whose branch it did not write stops as diverged. Change notebook content through the app, the API or MCP, or import a bundle as a new notebook. The database, blob store and filesystem still need adequate free space, durable storage, and independent backups.

For bundled Authelia, a full backup also includes its user database, SQLite state and encryption/signing secrets. Follow [standalone restoration](standalone.md#backup-and-full-restoration). Revoking a user in ForgetMeNote and disabling their identity-provider account are separate actions; see [session revocation](authentication.md#sessions-logout-and-offboarding).

## Collaboration state

Live documents and their accepted updates that are not yet checkpointed are rows
of the instance database (`live_docs` and `live_updates`), so the database backup
preserves them together with the revisions they are based on; see
[storage migrations and recovery](storage-migrations.md#complete-backups-and-recovery).

The browser/MCP Git bundle remains a checkpoint export. It refuses to export while
live updates are pending and cannot restore CRDT identities or the operation retry
index. Export does not save pending edits: explicitly Save or checkpoint each
pending note first. Idle time and restarting the service do not create
revisions; backup watchers include changes only after a checkpoint. See [live collaboration](collaboration.md) for full backup and recovery
semantics.

## Trash

Git bundles preserve `Trash/` notes, the reserved `.gitnotes-trash.json` manifest and its archives. Emptying Trash removes current files but preserves older versions and search tombstones; only a [purge](purge.md) removes content from older versions. Shared attachments remain available to other notes. See [Trash](trash.md).

Integrated chat corpus selections use version-2 conversation records. Reads of
version-1 conversations do not migrate them; a new scoped turn preserves their
private transcript and accounting during atomic upgrade. Old or unknown-scope
runs remain readable but cannot continue or approve pending actions.

An image that predates version-2 chat support must not run integrated chat
against version-2 conversations. For rollback to such an image, disable chat
for every user with `CHAT_CONFIG_FILE=''`. Images that already support version 2
still require exact image/data compatibility checks. Retain current data,
retrieval settings, notes and Git history for forward recovery. Do not restore
an older snapshot over newer accepted work. Verify the exact image/configuration pair and an independent synthetic restore before a
release; the application format alone does not establish deployment compatibility.

Organization policies, consent generations, baselines, suppression, paged activity
and Undo intents are judgment records in the database. Preserve them with the catalog's
access epochs and each notebook's commit receipts, Trash and discussions, which a
complete backup holds together. The independent judgments Docker
restore verifies an applying duplicate cleanup and its later Undo without new
inference. New optional sidecar fields are rejected by older strict decoders;
[organization rollback guidance](organization.md#storage-and-rollback) requires a
compatible image or forward recovery, never discarding newer accepted work.
