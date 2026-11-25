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
