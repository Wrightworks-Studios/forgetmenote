# Managed private backups

A notebook Owner can send the notebook's saved Git history to a private GitHub
repository, a Google Drive folder, a Dropbox folder, or any of them at once (one
destination of each kind per notebook). The application owns destination
consent, authority checks, short worker leases, and verified receipts.
`deploy/git-backup/watcher.py` remains the only upload implementation. Saves
never wait for a destination. Existing SSH worker configurations continue to
work. GitHub is described first; [Google Drive and Dropbox](#google-drive-and-dropbox)
follow. Each works without the other.

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

A protected Inbox or personal assistant notebook gets a backup destination only
from its own person's account: an Admin's authority over another person's Inbox
does not connect, control or lease one, for GitHub or a drive.

## Google Drive and Dropbox

The worker uploads snapshots of the notebook's saved Git history into one folder
of the Owner's Google Drive or Dropbox account. The application never uploads.
It keeps the destination's OAuth refresh token, sealed, and for each upload
leases the worker a fresh short-lived access token. The worker's snapshot
format, retention and restore steps are described with the worker
([`deploy/git-backup`](../deploy/git-backup/README.md)).

### Register your own OAuth clients

Every instance registers its own clients; there is no shared ForgetMeNote
client. Use separate clients for development and production.

**Google Drive.** In a Google Cloud project, enable the **Google Drive API**.
Configure the OAuth consent screen and add the one scope
`https://www.googleapis.com/auth/drive.file`, which lets the client see only
the files it creates, never the rest of the account's Drive. Create an **OAuth
client ID** of type **Web application** with the authorized redirect URI
`PUBLIC_URL/backup/google/callback` (for example
`https://notes.example.com/backup/google/callback`). Save the client secret in a
private file. While an external consent screen is in Testing, Google expires its
refresh tokens after seven days; publish it before relying on backups.

**Dropbox.** In the Dropbox App Console, create an app with **Scoped access**
and **App folder** access. On its Permissions tab enable `account_info.read`,
`files.metadata.read`, `files.metadata.write`, `files.content.read` and
`files.content.write`. On its Settings tab add the redirect URI
`PUBLIC_URL/backup/dropbox/callback`. Save the app secret in a private file. A
destination folder lives inside the app's folder, `Apps/<app name>/`. Dropbox
limits apps in development status to a small number of linked accounts; apply
for production status for more.

### Configuration

Add `drives` to the same `BACKUP_APP_CONFIG_FILE`. `app` and `allowCreate` may be
left out when there is no GitHub App, and `drives` when there are no drive
destinations; one of `app` or `drives` is required, and so is `workers`. Each
provider is optional.

```json
{
  "workers": {
    "local": {"owner": "local", "tokenFile": "/run/secrets/backups/worker-token"}
  },
  "drives": {
    "sealKeyFile": "/run/secrets/backups/drive-seal-key",
    "google": {
      "clientId": "123456789012-example.apps.googleusercontent.com",
      "clientSecretFile": "/run/secrets/backups/google-client-secret"
    },
    "dropbox": {
      "appKey": "exampleappkey",
      "appSecretFile": "/run/secrets/backups/dropbox-app-secret"
    }
  }
}
```

The seal key file holds at least 32 bytes of key material, raw or as hex or
base64 text; a shorter key is refused at startup. Create one with
`openssl rand -hex 32 > drive-seal-key` and mode 0600. Every gateway of an
instance needs the same key. It seals each destination's refresh token and
encryption password in the shared state; neither the database nor an export
holds the key, so a database copy alone reveals neither. Keep it with the
instance's recovery assets: without it, every drive destination must be
reconnected, and an encrypted destination's password is recoverable only from
the Owner's own copy.

The OAuth endpoints are the providers' own and cannot be configured; tests reach
fake providers through the HTTP client.

### Owner setup

Open **Notebook settings → History** as the notebook's Owner. Each configured
provider has a row. **Connect** goes to the provider, which asks the Owner to
allow access (PKCE, an offline grant). Back in ForgetMeNote, the setup screen
names the account and the folder, `ForgetMeNote/<notebook>-<six random letters
and digits>`, fixed for the destination. The Owner must confirm that anyone with
access to that folder can read every snapshot, including earlier revisions,
unless the snapshots are encrypted.

**Encrypt snapshots** is off by default. When chosen, ForgetMeNote makes a
random password (32 random bytes, base64url) and shows it once, in a copyable
field, with the warning that losing it makes the snapshots unreadable. It is not
shown again: restoring an encrypted snapshot needs the Owner's copy. The
application keeps it sealed and gives it to the worker in each lease.

Each row shows the account, folder, encryption, the last verified snapshot's
time and commit, how many snapshots the folder keeps, pending saved changes and
any error, with **Back up now**, **Pause**/**Resume** and **Disconnect**.
Reconnecting the same account continues the destination: the same folder,
encryption password and receipts. Connecting another account starts a new
destination in a new folder and revokes the replaced grant. **Cancel** on the
setup screen revokes the grant it obtained. A setup left unfinished is forgotten
after ten minutes, but its grant stays listed among the account's connected apps
until the account owner removes it.

**Disconnect** ends active leases, forgets the refresh token and password, and
revokes the grant at the provider. Snapshots already uploaded stay in the folder.
Revoking a Google token ends the OAuth client's whole grant for that Google
account, so while another notebook's destination uses the same Google account
the grant is kept; it is revoked with the last one. Dropbox revokes only the
destination's own token. If the provider does not confirm the revocation, the
row says so; remove the app from the account's connected apps. An account owner
who removes access at the provider stops the destination until the Owner
reconnects.

The same status and controls are available to MCP and API clients:
`managed_backup_status` lists a notebook's drive destinations, and
`control_managed_backup` takes `kind` (`github`, the default, `gdrive` or
`dropbox`). Connecting, the folder consent and the encryption choice happen
only in the browser.

### Worker API

The worker authenticates as for GitHub (`X-Backup-Worker` and its bearer token)
under `/internal/backups/v1/`. A request without `kind`, or with
`"kind": "github"`, has the GitHub meaning and shapes, unchanged.

- `GET bindings` lists drive destinations first, then GitHub bindings. A drive
  entry is `{"kind","project","generation","notebookGeneration","path",
  "encrypted","enabled","blocked","request","handoff"?}` plus `lastRevision` and
  `lastSnapshot` once one is verified. It never holds a token.
- `POST lease {"project","kind","generation","revision"}` refreshes an access
  token at the provider and answers `{"id","project","kind","generation",
  "notebookGeneration","revision","path","token","tokenExpires","expires",
  "encryption"}`. `encryption` is `{"password"}` or `null`. `tokenExpires`
  (RFC 3339) is at least ten minutes away; the lease `expires` in three
  minutes. One lease per destination is active at a time. Leases are refused
  while the destination is paused, blocked, disconnected or in a handoff, for
  another generation or storage owner, when its Owner's authority changed, and
  on a restored copy whose outgoing actions are paused.
- `POST check {"lease"}` answers `{"valid":true}` while the lease holds.
- `POST receipt {"lease","project","kind","generation","revision","error"?,
  "diverged"?,"snapshot":{"name","sha256","size","kept","deleted":[...]}}`
  ends the lease. A successful receipt names the verified snapshot; `kept` is
  the number of snapshots the folder keeps after retention deleted `deleted`.
  The worker's error text is not stored. `diverged` stops the destination until
  the Owner resumes it.
- `POST drain {"project","kind","generation","handoff"}` acknowledges that the
  worker stopped uploading for an authority handoff.

Access tokens are not revoked after each upload; they expire at the provider.

### Handoffs, restored copies and exports

An authority handoff fences drive destinations as it fences
the GitHub binding: their generation rises, their leases end, and the handoff
waits for the worker's drain of each. A committed handoff disconnects them and
revokes their grants; an aborted one leaves them paused for an explicit resume.

A copy restored beside the live instance, with outgoing actions paused, grants
no lease and calls no provider: the grants are the live instance's.

`export-files` writes drive destinations to `managed-backups/drives.json` beside
`bindings.json`, with their refresh tokens and passwords still sealed, and
`import-files` reads them back; the file-based release ignores `drives.json`.
They work again only on gateways that hold the same seal key.

Once a notebook has a drive destination, the shared backup state is version 3,
which gateways from before drive destinations refuse rather than drop it.
Upgrade every gateway, and the worker, before connecting the first drive
destination.

## Administration → Backups

Admins see one row per notebook with its managed destinations and when each last
verified a snapshot. Notebooks without a managed destination come first. A
personal notebook's row omits its provider account. Backups an operator runs on
the server's host (operator SSH mode, full-instance backups) are not visible to
the application and are not listed. MCP and API clients read the same list with
`managed_backup_overview`. The page follows the `managed-backups` module switch.

Sources: [App manifests](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest),
[installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation),
[repository creation](https://docs.github.com/en/rest/repos/repos#create-a-repository-for-the-authenticated-user),
[Google OAuth for web server apps](https://developers.google.com/identity/protocols/oauth2/web-server),
[Dropbox OAuth guide](https://developers.dropbox.com/oauth-guide).
