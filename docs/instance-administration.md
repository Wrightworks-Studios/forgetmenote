# One organization per instance

An installation belongs to one person or organization. Notebooks remain independent
Git repositories and permission boundaries. Personal use needs no groups. There
is no organization switcher, public registration, billing service or new database.
Instance Admins retain access to every notebook; the host operator remains trusted.

## Instance branding

In **Administration → Instance**, set the organization name, choose a PNG/JPEG
logo and select **Save branding**. **Use default logo** restores the flower when
saved. The header, browser icons, sign-in, invitation and AI connection pages use
the same settings. MCP responses identify the instance by its current name and
icon. Existing clients may cache their display until they reconnect.

The logo is decoded and re-encoded as PNG; SVG and other active formats are not
accepted. Uploads are bounded to 1 MiB and 2048 × 2048 pixels. Name, image and
audit receipt are saved atomically in the catalog and included in instance
backups. They are public branding, not secret configuration or notebook content.

[Installed apps](editor.md#install-as-an-app) use the same name and logo. The
logo is padded to a square, extending its edge color when the edge is opaque, and
scaled to 192 and 512 pixels; the maskable icon keeps it within the central 60%.
A logo smaller than 512 × 512 looks soft when enlarged.

New plugin downloads bundle that image in `assets/icon.png`. Codex manifests use
`composerIcon` and `logo`; updating an existing local installation requires a fresh
download and the normal plugin upgrade. Package identity is tied to the public
origin, so changing a display name or logo does not create another plugin. Offline
CLI package generation uses the supplied name and the default flower icon.

Changing the hostname is an operator action: prepare DNS, HTTPS and the identity
provider's new `/auth/oidc/callback` first, then change `PUBLIC_URL`. Update GitHub
App callbacks/webhooks, Slack events and backup-worker host settings that still
refer to the old origin. Download a new companion for the new origin and sign in
again; existing OAuth credentials are audience-bound and are not transferable
by an HTTP redirect. Keep the old endpoint working until those dependencies are
ready. Branding alone does not alter authentication, data access or URLs.

## People and groups

Administration has People, Groups, Automation, Activity and Instance categories.
Admins manage flat groups. Notebook Owners can grant a group Reader, Commenter or
Editor access in Notebook settings → People. Ownership stays with individual
people. Effective access is the highest direct or group grant, limited by the
current credential's scope and notebook authority. Group access never makes a
GitHub-authoritative notebook writable. Protected personal Inboxes cannot be shared.

The notebook member view shows inherited access. Removing direct membership does
not remove a group grant. Removing a person from a group or lowering the group's
role fences work authorized under the previous grant. Re-adding the membership
does not revive already-invalidated work. Group edits require the current revision;
stale edits conflict rather than overwriting another administrator's update.
Deleting a group also revokes its pending invitations. Recreating the same group
ID cannot revive an old invitation or stale editor's revision.

People → Invite a person creates a copyable, single-use link, valid for seven days.
It can grant groups and Reader/Commenter/Editor notebook roles, never Admin status.
Local invitees choose their own credentials. Existing local or externally
verified accounts can sign in and accept. No accounts are merged by name or email.
An invitation cannot override a disabled account or downgrade existing access.
External sign-in provisions accounts with no access by default. An operator may
set `OIDC_DEFAULT_GROUPS=employees` (comma-separated existing group IDs) to enroll
new OIDC accounts in those groups. Restrict eligible identities at the identity
provider first: every newly provisioned OIDC account receives this membership.
Missing groups fail sign-in without creating a partially authorized account.
Later sign-ins do not undo membership removal, and linking an existing local
account does not change its groups. Existing accounts need explicit membership.
Groups never grant instance administration.

Password-reset links last one hour and work only with local authentication. A new
reset link revokes previous pending reset links for that person. Consuming a reset
link revokes their existing sessions and credentials. Invitation secrets are
hashed in storage, returned once, and carried in URL fragments rather than HTTP
request URLs. The acceptance page retains a token in tab session storage during
sign-in, then removes it on success. Admins deliver links through their own channel;
ForgetMeNote does not require SMTP or send messages.

## Employee onboarding

This is an optional deployment policy, not a built-in employee role:

1. Create an `employees` group in Administration → Groups.
2. In each selected organization knowledge, repository and profiles notebook,
   grant that group **Reader** through Notebook settings → People. Inspect direct
   and effective access: a Reader grant does not cancel an existing Editor/Owner
   grant, and instance Admins always retain authority.
3. Configure approved data connections on the server. In the existing connection
   access controls, grant the group only the selected read/query operations. A
   notebook grant never grants a data operation. Connection credentials stay in
   their existing server-side secret owners.
4. Invite people with that group selected, or explicitly set
   `OIDC_DEFAULT_GROUPS=employees` after restricting the eligible population at
   the identity provider. Without that setting, OIDC creates no group membership.
   Existing accounts need explicit membership. Repeated logins never restore
   revoked membership. A missing group or failed catalog write fails enrollment
   without leaving partial permissions; correct the setup and repeat sign-in.
5. After the person exists, select them in Administration → People → **Provision
   a work notebook**. Enter a name and a unique notebook ID. This creates one
   ordinary privately owned notebook, with no instance privileges. Repeating the
   same person/ID/name is safe. A conflicting/shared notebook is not repurposed;
   use its existing membership controls if you intentionally want to share it.
6. Give the employee the instance’s `/account/connect` address. They install or
   configure their own client and consent to `notes:read notes:write data:read`.
   Verify as a non-admin: read shared knowledge, write their chosen work notebook,
   and discover only approved operations.

An Admin can perform step 5 using the existing action interface after management
OAuth login:

```sh
forgetmenote action provision_work_notebook --file work-notebook.json
```

```json
{"user":"EXISTING_ACCOUNT_ID","id":"employee-work","name":"My work"}
```

Do not enable notebook creation or Admin access merely to provision this notebook.
Ordinary personal notebooks are isolated from other employees by membership;
instance administrators retain access. They are not the protected private Inbox.
Profile-maintenance automation keeps its own explicit Editor grants and data
operation grants; employees do not inherit its authority.

### Example: company deployment

This is configuration guidance only; no production permissions are changed by
shipping this feature. Use its public URL, such as `https://notes.example.com`, as the
origin and the instance name configured in Administration. An operator can give
an employee group Reader access to selected repository/knowledge notebooks and
`analytics-profiles`, explicitly grant its approved read/query operations, and
provision one ordinary work notebook per employee. Keep profile-maintenance
writing under the existing automation identity. Verify a non-admin account before
sharing the setup URL. The package contains no company-specific hostname, notebook ID,
schema or credentials; those values come from configuration and authorized data.

## Automation

Automation accounts belong to the instance and have a human steward. Admins grant
them explicit notebook access, up to Editor, and issue expiring API/MCP credentials
for an explicit notebook list. A credential cannot exceed the account's current
notebook permissions. Accounts cannot log in interactively, own notebooks, create
notebooks, become Admin, or receive a personal Inbox or private chat history.
Their content commits are attributed as Automation with a stable account ID.

Issue a replacement credential before revoking the old one to rotate without
interrupting a job. Secrets are returned once; the credential inventory contains
only metadata. Disabling an automation account invalidates its credentials, even
if the account is later enabled again. Disabling its human steward does not stop
independently authorized organization automation; assign a new steward explicitly.

## Offboarding

Select a person to inspect their groups, effective notebook access, shared notebook
ownership, credentials, automation stewardship and connected repository/backup
authorizations. Disable access first, then explicitly transfer shared notebooks
and reassign automation stewardship. Disabled account IDs and Git authorship remain
stable. Ownership transfer excludes personal Inboxes and never transfers personal
conversations or credentials.

The ordinary Admin interface refuses disabling/demoting the last active human
Admin. Recovery administrator access remains available for emergency intervention.
Notebook ownership does not otherwise prevent immediate account disablement.

User disablement revokes browser, OAuth and API credentials through the account
epoch. Existing provider calls cannot be recalled, but subsequent authorized work
is denied. Repository jobs are paused through the existing control boundary;
re-enabling the account also checks that its repository jobs are paused first.
Backup jobs retain their existing epoch/generation fencing and require explicit
reauthorization. Offboarding lists a person's Google Drive and Dropbox backup
destinations with their GitHub ones. **Administration → Backups** lists every
notebook's managed backup destinations, those without one first; see
[managed backups](managed-backups.md#administration--backups). Source
resume/reconnection is an explicit Owner/Admin operation.
If pausing fails, the account remains disabled and the API reports the error.

With OIDC or a trusted proxy, disable the person in ForgetMeNote and the identity
provider. This release does not implement SCIM or continuous provider-state sync.

## API, CLI and MCP

The browser and `/api/v1/actions/NAME` use the same typed actions as MCP. Management
requires `notes:manage` consent plus current Owner/Admin authority. Instance actions
are excluded from embedded chat and from ordinary API credentials; use a human
Admin OAuth session or operator recovery authentication for instance management.
Scoped credentials can use group-sharing actions only within an owned, allowed
notebook. Service accounts cannot own notebooks or administer the instance.

```sh
forgetmenote auth login --server https://notes.example.com --manage
forgetmenote organization status --json
forgetmenote organization group --file group.json
forgetmenote organization invite --file invitation.json
forgetmenote organization service --file service.json
forgetmenote organization offboarding ACCOUNT_ID --json
forgetmenote organization disable ACCOUNT_ID
forgetmenote action reassign_notebook_ownership --file transfer.json
```

Example group input:

```json
{"id":"engineering","name":"Engineering","members":["ACCOUNT_ID"],"revision":0}
```

Example invitation input:

```json
{"name":"New colleague","groups":["engineering"],"projects":{"handbook":"read"}}
```

`action list` lists schemas for all available actions. Additional actions include
`project_groups`, `set_project_group`, `delete_group`, `revoke_invitation`,
`create_password_reset`, `set_service_steward`, `issue_service_credential`,
`revoke_user_credential`, `set_instance_name`, and `set_instance_branding`. The
branding action accepts `name` and an optional base64 `logo`; omitting the logo
preserves it, while an empty string restores the default. Invitation acceptance and login
pages remain available with `HEADLESS=true`.

Existing personal API credentials and their notebook-management contracts remain
supported. API credentials cannot mint additional credentials or invoke the new
instance-management actions. They remain bounded by current account permissions.

## Activity and restoration

Account, permission, group and invitation changes and their activity receipts are
persisted atomically in the private catalog. Operator Admin grants and new external
identities also leave receipts. Credential and external integration changes record intent before crossing
into another state owner, followed by an outcome; an interrupted operation may
retain a pending receipt and should be checked before retrying. Activity records
contain identity IDs, operation metadata and outcomes, never passwords, invitation
secrets, API keys, note bodies or chat prompts. This is an administrative record,
not tamper-proof storage against the host operator.

The latest 5,000 receipts are retained. Activity → Export JSON exports the retained
window; preserve exports externally if longer retention is needed. The Instance
screen shows configured authentication and integration status; provider credentials
and processing consent continue to use their existing owners and limits.

The first audited mutation upgrades the private catalog to version 4 and preserves
`catalog.pre-organization.json`. Older binaries reject this format. User IDs,
notebook paths, memberships and content Git history are preserved. Take a complete
stopped-instance backup before upgrading. The migration snapshot alone is not a
complete backup or a safe rollback after new writes.

Back up all `/data` to retain groups, invitations, accounts, credentials, audit
records and application state. Notebook Git exports contain content and history,
not organization permissions. A restored backup reflects revocations as of its
snapshot; after security recovery, revoke sessions/credentials and pending
invitations as appropriate before exposing the restored service.
