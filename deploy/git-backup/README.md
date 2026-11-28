# Debounced host-side Git backup

This optional Linux worker runs outside ForgetMeNote. It observes Git reference
changes using the host's `inotifywait`, waits for 10 seconds of quiet (at most 60
seconds of continuous edits), and pushes committed history to one explicit branch.
Startup and five-minute reconciliation cover missed filesystem events. Failed
pushes retry after 30 seconds, doubling to a five-minute maximum. A slow push is
bounded to 120 seconds; commits made during it are picked up afterward.

Requirements: Python 3.11+, Git, OpenSSH, `inotifywait` from inotify-tools, and
systemd user services. Run as the Unix account that owns the notebook. The worker
does not pull, merge, run Git hooks, modify repository configuration, or edit
notebook files, and it force-pushes only for an authorized purge (below). Its
state and credentials must live outside notebook data. Do not run external Git
garbage collection or restore operations on the live app.

Instead of pushing a branch, the same worker can keep versioned
[snapshots](#snapshot-destinations) of a notebook in a folder or a cloud drive,
leased [drive snapshots](#managed-drive-snapshots) the app's users set up, and
daily encrypted [instance snapshots](#instance-snapshots). Those need the pinned
[rclone](#rclone) for anything but a folder.

The repository is the notebook's Git export, `data/exports/NOTEBOOK.git`, a bare
repository ForgetMeNote keeps up to date with every revision. A notebook working
tree (`data/projects/NOTEBOOK/workspace`, before storage v2) works the same way.
Either must be the exact repository root: a working tree's top level, or the bare
repository itself.

## Installation

Install `watcher.py` as `~/.local/lib/forgetmenote-git-backup/watcher.py` and the
unit as `~/.config/systemd/user/forgetmenote-git-backup@.service`. Create a private
configuration at `~/.config/forgetmenote-git-backup/NOTEBOOK.json`:

```json
{
  "repository": "/absolute/path/to/data/exports/NOTEBOOK.git",
  "remote": "git@github.com:OWNER/REPOSITORY.git",
  "branch": "main",
  "state_dir": "/absolute/path/outside/notebook/backup-state",
  "ssh_key": "/absolute/path/to/dedicated-private-key",
  "known_hosts": "/absolute/path/to/verified-known-hosts",
  "debounce_seconds": 10,
  "max_delay_seconds": 60,
  "reconcile_seconds": 300
}
```

Protect configuration, key, and state with owner-only permissions. Register the
public key as a write-enabled deploy key on the chosen repository. Verify SSH host
keys independently; the worker refuses unknown hosts and ignores SSH agents and
user SSH configuration. Check repository visibility, branch rules, and existing
push-triggered workflows before enabling the backup. Keep the remote one-way;
divergence requires operator review, not automatic merging or force-pushing.

Enable lingering for the service account so it starts at boot without a login:

```sh
loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now forgetmenote-git-backup@NOTEBOOK.service
```

Headless shells may need `XDG_RUNTIME_DIR=/run/user/UID` and
`DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/UID/bus` to address the user manager.
The notebook container and authentication proxy do not need restarting.

## Verification and operations

```sh
python3 -m unittest discover -s deploy/git-backup -p test_watcher.py -v
systemctl --user status forgetmenote-git-backup@NOTEBOOK.service
journalctl --user -u forgetmenote-git-backup@NOTEBOOK.service -n 30
systemctl --user disable --now forgetmenote-git-backup@NOTEBOOK.service
```

The worker atomically maintains `status.json` in its state directory, recording
the observed revision, last successfully pushed revision and timestamps, and the
latest error. `synced` means the last push succeeded; startup revalidates the
remote rather than trusting cached state. A state-directory lock prevents duplicate
workers. Failed pushes remain in retry and are reported as ERROR in the journal;
there is no email/webhook notification transport configured by this worker.
Process health alone is not proof that the remote is current: monitor the status
file or journal errors and compare remote/local revisions when auditing backups.

`--once --config PATH` performs one push and exits nonzero on failure. It is not a
dry run. Tests use only disposable local repositories, including deliberate failed
pushes and hook-suppression checks; they never need network access or real notes.

This backs up the selected branch's committed content and history, not all Git
refs or full-instance accounts, permissions, secrets, and unsaved drafts. Maintain
separate encrypted full-instance backups as described in [backups](../../docs/backups.md).

## Purges

A purge is the one operation that rewrites a notebook's history, to remove
sensitive content from every version. ForgetMeNote then rebuilds the export and
writes `forgetmenote-purge.json` into it, naming the purge, its newest rewritten
commit (which carries a `Gitnotes-Purge: ID` trailer) and every commit the rewrite
dropped. When a fast-forward push is refused, the worker replaces the remote branch
with `--force-with-lease`, and only when all of these hold:

- the flag names a purge this worker has not pushed before;
- the remote branch is at one of the commits the purge dropped, so nothing anyone
  else pushed is lost;
- the pushed revision contains the flagged commit, and that commit carries the
  purge's trailer.

Otherwise divergence stops retries as always. After any verified push whose
revision contains the flagged commit, whether it replaced the branch or, for a
remote that never received the dropped commits, fast-forwarded it, the purge's ID
is recorded as pushed in `status.json` and the flag is removed. The worker removes
it only while holding the lock (`flock`) of `forgetmenote-purge.lock`, under which
ForgetMeNote replaces the flag, so a newer purge's flag always stays. With a
read-only repository mount the flag stays but authorizes nothing more. Anything
pushed before the purge must still be treated as leaked: rotate the secret, and
ask GitHub Support to clear cached views if needed.

The token and managed GitHub modes share this rule. A protected branch that
refuses force-pushes refuses the purge's replacement too: the worker then reports
the failure and keeps retrying, as for any failed upload, until the operator
allows the one force-push or replaces the remote branch by hand.

## Managed GitHub mode

The image also includes `/usr/local/bin/forgetmenote-backup` for external workers.

The same worker supports `mode: "managed"` with an owner-scoped gateway token and a fixed local notebook/root allowlist. See [configuration and recovery](../../docs/managed-backups.md). It reuses the pinned push, debounce, retries, and status logic; the server never pushes Git. SSH mode remains supported. Both modes verify the remote branch after uploading. Divergence stops retries; resolve it before using `--resume` in SSH mode or Resume in Notebook settings.

## Operator GitHub token mode

To reuse a repository-scoped credential without a new App or SSH key, set
`mode: "github"`, an exact HTTPS `remote`, and `token_file` outside the notebook.
The token needs Contents read/write on that private repository. The worker checks
repository identity, visibility and write access before and during transfers.
Redirects and ambient proxies are refused. A temporary credential helper supplies
the token only for the configured GitHub host and repository path, then is removed.

```json
{
  "mode": "github",
  "repository": "/data/exports/analytics-profiles.git",
  "remote": "https://github.com/EXAMPLE/analytics-profiles.git",
  "branch": "main",
  "token_file": "/run/secrets/github-token",
  "state_dir": "/backup-state/analytics-profiles"
}
```

Run the worker with `--config /run/secrets/backup.json`. Token mode polls the
exported branch every two seconds and needs no inotify package. Mount notebook data read-only
and give only the state directory write access. Verify `status.json`, then clone
the remote independently to check content and history before relying on the backup.

This is an operator backup, not a Notebook settings managed-App connection.
Stop it before authority handoffs or restores; the gateway cannot fence an
externally configured token worker. Fast-forward-only and remote verification
rules are the same as SSH mode. Instance recovery still needs full-volume backups.

## Snapshot destinations

`mode: "snapshot"` keeps versioned copies of one notebook's export in a folder
(a NAS mount or a disk) or an [rclone](#rclone) remote (Dropbox, Google Drive, S3,
SFTP and the rest, best through an encrypting `crypt` remote), instead of pushing
a branch. As in SSH mode, each notebook has its own configuration file and unit:

```json
{
  "mode": "snapshot",
  "repository": "/srv/forgetmenote/data/exports/notes.git",
  "state_dir": "/srv/backup-state/notes",
  "destination": {"rclone": "dropbox-crypt:", "path": "ForgetMeNote/notes"},
  "rclone": "/usr/local/bin/rclone",
  "rclone_config": "/srv/backup-state/rclone.conf",
  "min_interval_seconds": 3600,
  "retention": {"newest": 24, "daily": 30, "weekly": 52}
}
```

For a folder, use `"destination": {"folder": "/mnt/nas/forgetmenote/notes"}` and
leave out `rclone` and `rclone_config`; the folder must exist outside notebook data
and the worker's state. rclone sees only the configuration file `rclone_config`
names (outside notebook data) or, without it, an empty one: the worker passes it
no other environment. `notebook` defaults to the repository's name without `.git`.
Give each notebook its own destination path.

The worker polls committed `HEAD` every two seconds, so this mode needs no inotify
package, debounces like SSH mode and takes at most one snapshot per
`min_interval_seconds`; `--once` takes one at once. A snapshot is:

1. `git bundle create` of `HEAD` and `main` into the state directory. It only
   reads the export: no locks, hooks or maintenance. **Import notebook** needs
   `HEAD` in a bundle.
2. `git bundle verify` in an empty repository, which proves the bundle complete,
   and `git bundle list-heads` for the commit it actually holds, since the export
   may have moved meanwhile.
3. No upload when the newest snapshot at the destination already holds that
   commit.
4. Otherwise an upload as `YYYYMMDDTHHMMSSZ-COMMIT12.bundle.partial`, a hash check
   at the destination, a rename, and only then the manifest
   `YYYYMMDDTHHMMSSZ-COMMIT12.json`. A snapshot exists once its manifest does.

```json
{"format": 1, "kind": "notebook", "notebook": "notes", "commit": "40 hex digits",
 "created": "2026-11-27T04:00:00.000000Z", "size": 123456, "sha256": "64 hex digits",
 "purges": ["p1"], "worker": "forgetmenote-backup snapshot 1"}
```

The manifest holds no secrets. A folder upload is read back and hashed. An rclone
upload is checked against the SHA-256 the backend reports, or another hash it
reports and `rclone hashsum` computes locally, or, as for `crypt` remotes, which
report none, by `rclone check --download`, which reads it back.

After a new snapshot verifies, retention keeps the newest `newest` snapshots, the
newest of each of the last `daily` UTC days and the newest of each of the last
`weekly` ISO weeks, and always the newest of all. The worker deletes only files
that match these names and have a manifest of this format, data first and the
manifest last. A day later it removes leftovers of interrupted uploads and
deletions: `.partial` files, and data or manifests without the other. It never
touches other files.

### Snapshots and purges

The snapshot worker never reads or removes `forgetmenote-purge.json`; the Git push
worker owns that flag. Each manifest instead lists every `Gitnotes-Purge` trailer
in its history. After a new snapshot verifies, an older snapshot whose manifest
lacks one of the new snapshot's purges holds history from before that purge, and
is deleted. This works for any number of destinations backing up one notebook, and
a restore to an older database, whose history has no new trailer, deletes nothing.
Copies the destination keeps by itself (versions, trash, sync clients) are beyond
the worker's reach; see [what a purge cannot do](../../docs/purge.md#what-a-purge-cannot-do).

For a Google Drive remote, `use_trash = false` makes a deleted snapshot gone rather
than kept in the Drive's trash.

### Private notebooks

Listing an `inbox-*` (protected Inbox) or `assistant-*` (personal assistant and
memory) export sends one person's private history off the machine, beyond the
instance's sharing rules. Leave them out unless that person asked for it, and then
use an encrypted destination only you control.

### Status and verification

`status.json` records the status, the observed commit, the last snapshot (name,
commit, created, SHA-256 and size), how many snapshots the last run kept and
deleted, and the latest error. `--verify --config PATH` downloads the newest
snapshot into a temporary directory under the state directory, checks its size and
SHA-256 against its manifest, verifies the bundle and compares its commit, and
exits 0 or 1. It changes nothing, so it can run beside the worker.

## Managed drive snapshots

In [managed mode](../../docs/managed-backups.md), `GET bindings` also lists the
Google Drive and Dropbox destinations people connect in Notebook settings
(`"kind": "gdrive"` or `"dropbox"`). A worker serves them when its configuration
has [rclone](#rclone): the [worker image](#the-worker-image)'s, or the one its
configuration's `rclone` names. A worker without rclone, such as the
`forgetmenote-backup` inside the application image, logs each drive binding once,
skips it, and keeps pushing to GitHub. Each binding takes the snapshots described above, with the
worker configuration's `min_interval_seconds` and `retention`; **Back up now** takes
one at once.

- Active backups are keyed by notebook and kind: a GitHub push and a drive snapshot
  of one notebook are independent, with state in `STATE/NOTEBOOK` and
  `STATE/NOTEBOOK.KIND`. A drain during an authority handoff names the kind
  (omitted for GitHub).
- Each snapshot leases with the notebook, kind, generation and the bundle's commit.
  Every lease field must match the binding and that commit; an encrypted
  destination needs the lease's password and never receives an unencrypted upload.
- The lease defines one-run rclone remotes only through `RCLONE_CONFIG_*`
  environment variables of the rclone process: `fmnsnap:`, Google Drive with the
  `drive.file` scope (only files the app created) and the trash off, or Dropbox,
  holding the access token; and for an encrypted destination `fmncrypt:` over it,
  with the password obscured by `rclone obscure -` from standard input. No file,
  argument, status file, log or receipt holds either.
- The lease is checked before the upload and every two seconds during each rclone
  call; a failed check ends the call. Uploads are bounded to 120 seconds, within the
  three-minute lease.
- The receipt carries `snapshot` (name, SHA-256, size, the number kept and the names
  deleted), or `error: "upload failed"`.
- The worker does not revoke access tokens: it drops them when the run ends.
  Disconnecting the destination in the app revokes the grant.

## Instance snapshots

`mode: "instance"` uploads a daily encrypted copy of the whole instance without
stopping it, through the application's capture command:

```json
{
  "mode": "instance",
  "state_dir": "/srv/backup-state/instance",
  "at": "04:00",
  "capture_command": ["docker", "compose", "-f", "/srv/forgetmenote/compose.yaml", "exec", "-T",
                      "forgetmenote", "forgetmenote", "admin", "capture", "-out", "/backup/.capture/current"],
  "capture_dir": "/srv/forgetmenote/.capture/current",
  "dump_command": ["docker", "compose", "-f", "/srv/forgetmenote/compose.yaml", "exec", "-T",
                   "postgres", "pg_dump", "-U", "forgetmenote", "--snapshot={snapshot}", "forgetmenote"],
  "include": ["/srv/forgetmenote/.env", "/srv/forgetmenote/secrets", "/srv/forgetmenote/instance.json"],
  "destination": {"rclone": "gdrive-crypt:", "path": "ForgetMeNote/instance"},
  "rclone": "/usr/local/bin/rclone",
  "rclone_config": "/srv/backup-state/rclone.conf",
  "retention": {"daily": 14, "weekly": 8}
}
```

`capture_dir` is the host path of the command's output directory. It must be inside
a folder named `.capture`, because the worker deletes it after every capture, and
the worker's state must be elsewhere. The capture command refuses an output
directory inside the data directory or the blob directory, so give the gateway
container a separate mount for it, for example
`/srv/forgetmenote/.capture:/backup/.capture` in the Compose file's `volumes`. Run the worker as an account that can run
both commands and delete what the capture writes: the data directory's owner, or
root. Membership of the `docker` group, which the example needs, is equivalent to
root.

The destination must be an rclone `crypt` remote: before each capture the worker
checks that `rclone listremotes --long` reports `crypt` for it, and refuses
folders and other remotes. The archive holds the recovery key, the proxy secret,
`.env`, `secrets/` and the database with its sign-in state. Keep the crypt password
and salt apart from the backup: nothing restores without them.

The worker captures daily at `at`, local time, and at start when the last scheduled
capture was missed; `--once` captures at once. A failure retries after
`retry_seconds` (300), doubling up to `retry_max_seconds` (3600).
`capture_timeout_seconds` and `upload_timeout_seconds` (both 3600) bound the
capture and every rclone call. Retention defaults to `newest` 1, `daily` 14 and
`weekly` 8. `--verify` downloads the newest capture and checks its size, SHA-256,
`capture/capture.json` and `postgres.sql`.

The archive, `YYYYMMDDTHHMMSSZ-instance-SHA12.tar.gz`, holds `capture/` (the
capture directory with its `capture.json`), `postgres.sql`, and each `include` path
under `include/` followed by its absolute path. Modes are kept; a symlink as an
included path, or one that leads out of an included tree, fails the capture. Its
manifest has `format`, `kind: "instance"`, `created`, `size`, `sha256`, `purge` (the
capture's latest purge, or `null`) and `purges`, every purge the destination's
captures have seen. A new capture whose purge none of the older captures has seen
deletes them all once it verifies, so a purged note leaves the destination; a
restore to an older database, whose latest purge is an older one, deletes nothing.

### Capture protocol v1

1. The worker deletes any previous `capture_dir` and starts `capture_command` with
   pipes for standard input and output, standard error to the journal, and no
   `RCLONE_*` variables.
2. The command copies the data directory's secrets and the Git exports into its
   output directory, takes the block-sweep lock, opens one read-only database
   snapshot, copies the content blocks that snapshot references, writes
   `capture.json`, prints one line, `snapshot SNAPSHOT-ID purge PURGE-ID` (`-`
   when there has been no purge), and waits.
3. The worker runs `dump_command` with `{snapshot}` in its arguments replaced by the
   snapshot ID (never through a shell), writes its standard output to `postgres.sql`
   (mode 0600) in its staging directory, and requires exit status 0.
4. The worker writes `done` and a newline. The command ends its snapshot, releases
   the lock, prints `ok` and exits 0.

Any other outcome fails the capture. The worker closes the command's standard
input, which aborts the capture: the command ends its snapshot and releases the
lock. After 30 seconds the worker ends the command's process group, then deletes
`capture_dir` and its staging directory, records the error and retries later.

## Restoring snapshots

A notebook snapshot is a complete Git bundle; check it against its manifest first:

```sh
rclone --config rclone.conf copyto dropbox-crypt:ForgetMeNote/notes/NAME.bundle notes.bundle
sha256sum notes.bundle
git clone notes.bundle restored-notes
git -C restored-notes log --oneline
```

To bring it back into ForgetMeNote, use **Import notebook** with a new ID
([notebook export and import](../../docs/backups.md#notebook-export-and-import)).

An instance snapshot decrypts on any machine through a `crypt` remote with the
same password and salt:

```sh
rclone --config rclone.conf copyto gdrive-crypt:ForgetMeNote/instance/NAME.tar.gz instance.tar.gz
sha256sum instance.tar.gz
mkdir restore && tar -xzf instance.tar.gz -C restore
```

Then restore it as [complete instance backup](../../docs/backups.md#complete-instance-backup)
describes: load `postgres.sql` into an empty database, put the data directory back
from `capture/`, and the `include/` paths at their own locations. Never unpack
over a live data directory.

## rclone

Every destination but a folder needs rclone, pinned at v1.74.3. It reaches Google
Drive, Dropbox and many other services, and encrypts with `crypt`, as one separate
executable, so the worker stays standard library Python.

The [worker image](#the-worker-image) includes it at `/usr/local/bin/rclone`, with
its MIT licence in `/usr/share/doc/rclone/COPYING`, and names it in
`FMN_BACKUP_RCLONE`; the worker uses it when its configuration sets no `rclone`. The
application image does not include it. On another host, install it from the
project's release archive and check the archive's SHA-256:

| Archive | SHA-256 |
| --- | --- |
| `rclone-v1.74.3-linux-amd64.zip` | `dbee7ccd7a5d617e4ed4cd4555c16669b511abfe8d31164f61be35ac9e999bd2` |
| `rclone-v1.74.3-linux-arm64.zip` | `8f8d47446e061f80c3256659fe8e21f56d72d96aaefe1275d088ea5eb6b42aa7` |

The worker uses `copyto`, `moveto`, `copy`, `cat`, `lsjson`, `hashsum`, `check`,
`deletefile`, `obscure` and `listremotes`. It runs rclone with `--config` (the
configured file or an empty temporary one) and `--ask-password=false`, with only
`PATH` and `HOME` (its state directory) in the environment apart from managed
remotes, bounds every call, and ends the process group on a timeout or a failed
lease check. CI runs the real-rclone tests on the pinned version: change the pin in
the `Dockerfile`, `.github/workflows/check.yml` and here together, after checking
rclone's changelog for those commands; the worker's tests check that the three
agree. Locally:

```sh
FMN_RCLONE=/usr/local/bin/rclone python3 -m unittest discover -s deploy/git-backup
```

These tests define local and `crypt` remotes in the environment and need no
network. Without `FMN_RCLONE` they are skipped, and with `FMN_REQUIRE_RCLONE=1` a
missing binary fails them.
