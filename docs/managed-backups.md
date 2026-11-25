# Managed private GitHub backups

The application owns destination consent, authority checks, short worker leases,
and verified receipts. `deploy/git-backup/watcher.py` remains the only remote-push
implementation. Saves never wait for GitHub. Existing SSH worker configurations
continue to work.

## Register separate Apps

Sources uses Contents **read** and Metadata **read**. Backups uses Contents
**write** and Metadata **read**. Do not reuse an App ID. Optional repository
creation also needs Administration **write** on the Backups App and
`allowCreate: true` in the server configuration. This is an installation-wide
permission upgrade; existing installations must approve it. Routine installation
tokens still request only Contents write and Metadata read for one repository.

Print either manifest without contacting GitHub:

```sh
python3 scripts/github_app_manifest.py sources --origin https://notes.example --name 'Notes Sources'
python3 scripts/github_app_manifest.py backups --origin https://notes.example --name 'Notes Backups'
```

For an operator-controlled registration, add `--register --secrets-dir /absolute/new/private/directory`.
The script serves a review form on loopback. Open its printed URL, review the
permissions, and create the App on GitHub. It exchanges the registration code
server-side and writes private files with mode 0600; no credentials are printed.
Use `--org ORGANIZATION` for an organization registration, `--public` for a hosted
App that customers can install, and `--allow-create` only for the Backups App when
the broader permission is intended. Use separate names, origins, and secret
directories for development and production. Registration is a deliberate external
operator action; starting ForgetMeNote never registers an App.

Manual provisioning remains supported, including read-only mounted secrets. Set
the OAuth callback to `/repository/github/callback` for Sources or
`/backup/github/callback` for Backups on `PUBLIC_URL`. ForgetMeNote supplies PKCE
for each authorization; keep expiring user tokens enabled. Hosted Sources
subscribes to push/repository events at
`/repository/github/webhook`; installation access is also rechecked during refresh.
Backups checks the repository’s current App installation, suspension state,
Contents write permission, and privacy for each lease and while uploading; it
does not need webhooks. Repository metadata alone is insufficient because GitHub
can still serve it after suspending an App. After installing or changing selected repositories,
return to ForgetMeNote and connect again. Organization approval can leave the
choice list empty until approval is granted.

The registration helper writes host paths. When mounting those files into a
container, change the config paths to their mounted locations. Never place keys,
worker tokens, or generated configuration in notebook Git or this source checkout.

## Server configuration

Set `BACKUP_APP_CONFIG_FILE=/run/secrets/backups/app.json`:

```json
{
  "app": {
    "appId": "12345",
    "slug": "notes-backups",
    "clientId": "Iv1.example",
    "clientSecretFile": "/run/secrets/backups/client-secret",
    "privateKeyFile": "/run/secrets/backups/private-key.pem",
    "webhookSecretFile": "/run/secrets/backups/webhook-secret"
  },
  "allowCreate": false,
  "workers": {
    "local": {"owner": "local", "tokenFile": "/run/secrets/backups/worker-token"}
  }
}
```

The manifest omits webhooks for Backups and for localhost Sources pilots, which
refresh by polling. Hosted Sources Apps register push/repository webhooks.
The unused webhook secret is kept in the common App configuration. The
registration helper generates it when GitHub does not; manual provisioning can
use a random secret of at least 32 characters. Worker tokens also need at least
32 random characters.

Every notebook is stored by the instance itself, so its binding names the owner
`local` and a worker configured with `owner: "local"` may back it up; there are no
remote storage owners to configure workers for. Changes to these operator
credentials/configuration take effect on restart.

## Worker configuration

Use the existing systemd unit with a managed configuration file:

```json
{
  "mode": "managed",
  "gateway": "https://notes.example",
  "worker": "local",
  "token_file": "/private/secrets/worker-token",
  "state_dir": "/private/backup-state",
  "notebooks": {
    "personal": "/srv/forgetmenote/exports/personal.git",
    "team": "/srv/forgetmenote/exports/team.git"
  }
}
```

Each path is the notebook's Git export, `data/exports/NOTEBOOK.git`
([Git export repositories](backups.md#git-export-repositories)). The paths are a
fixed operator allowlist, never browser inputs. Every notebook must have a unique
local root; state and credentials must be outside those roots.
Do not run an independent SSH worker against the same destination branch. Run
`python3 deploy/git-backup/watcher.py --config /private/managed.json --once` for
one reconciliation, or omit `--once` for continuous operation. Only run this after
the intended real destination has been explicitly selected.

Managed mode polls committed HEAD and binding state every two seconds, debounces
for ten seconds, attempts within sixty seconds of continuous saves, and reconciles
every five minutes. Pushes are serial and bounded to 120 seconds. Credentials use
a private temporary helper that answers only for the exact GitHub host/path. They
never enter command arguments, URLs, Git configuration, logs, or status receipts.
The helper is removed after each attempt. Lease checks cancel the process group
when revocation is observed. A three-minute app lease does not change GitHub's
token lifetime: installation tokens normally expire after one hour. ForgetMeNote
revokes them after completion, cancellation, or lease expiry. Bindings and active
leases live in the instance's Postgres database, so a worker may lease from one
gateway and report to another, and a lease survives a gateway restart. A leased
token is stored only sealed with AES-GCM under a key derived from the Backups
App private key, which the database does not hold; a database copy alone reveals
no token. Any running gateway revokes a lease once it lapses or loses its
authority; if no gateway runs, the token stays valid until its GitHub
expiration. While the database is unreachable no lease is granted.

## User setup and recovery

Open **Notebook settings → History → GitHub backup** as Owner/Admin. Authorize
the Backups App, choose an existing **empty private repository**, and explicitly
confirm sharing all committed history with that repository's GitHub readers.
The branch must match the local notebook branch (normally `main`). A destination
branch belongs to only one notebook. When repository creation is enabled, the
same screen offers creation under a personal account or organization using the
authorizing user's App token. Select/install access to the created repository
before enabling it. A durable random creation marker reconciles uncertain
responses; retries never adopt an unrelated same-named repository or delete a
remote repository.

Controls show pending saved changes, verified commit/time, errors, Back up now,
Pause, Resume, and Disconnect. Reconnect after authorization changes. A private
destination becoming public stops new uploads. Pause/disconnect invalidates active
leases and cancels uploads once observed; already uploaded data cannot be retracted.
Disconnect leaves the remote repository intact.

Uploads are fast-forward only. The worker never merges, resets, invokes local
hooks, force-pushes, or deletes remote refs. Remote divergence stops retries until
the destination is deliberately resolved and backups resumed. For operator SSH
mode, use `--resume` after resolving it. Transient failures back off automatically.
The receipt records the exact pinned commit verified at GitHub, even if another
save happens during upload.

Clone the backup into an independent directory to recover ordinary Markdown,
attachments, and history without ForgetMeNote. To import into a fresh notebook,
create a bundle from that clone with `git bundle create notebook.bundle --all`
and use **New notebook → Import bundle**. Confirm the recovered Markdown,
attachment hashes, and history before using it. Never pull a backup into a live
workspace or replace a newer draft automatically.

Only saved/checkpointed content is covered. Browser drafts and uncheckpointed live
edits, chats, permissions, accounts, and receipts require the appropriate
[full-instance backup](backups.md). Git LFS support and GitHub file-size/storage
limits are unchanged; failures are visible and content/history is never truncated.

Sources: [App manifests](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest),
[installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation),
[repository creation](https://docs.github.com/en/rest/repos/repos#create-a-repository-for-the-authenticated-user).
