# Optional Authelia integration preset

**This is not the default ForgetMeNote deployment.** The default is one application container with local username/password login; see [authentication](authentication.md). Use this extra provider/proxy preset only if you explicitly want to run Authelia or test OIDC.

Use this preset on an ordinary Docker host/VPS with durable local filesystem storage. It renders a three-service Compose bundle: **ForgetMeNote + Authelia + Caddy**. ForgetMeNote still runs one Go process; its notebooks are in the bundled Postgres database and the blob store under persistent `/data`. Authelia uses its supported file user backend, SQLite storage, and default in-memory sessions; Redis is unnecessary for this single instance. Caddy handles HTTPS. If you already run SWAG/Authelia or another OIDC provider, use [existing infrastructure](deployment.md) instead.

## Setup and first administrator

Install Docker with Compose, Python 3.9+, OpenSSL, and Git. Obtain this repository, then create an operator virtual environment outside the deployment directory:

```sh
python3 -m venv ~/.venvs/forgetmenote-operator
~/.venvs/forgetmenote-operator/bin/pip install -r deploy/requirements.txt
~/.venvs/forgetmenote-operator/bin/python -I deploy/standalone/operator.py --directory ~/forgetmenote-instance setup
```

The operator collects separate ForgetMeNote/Authelia DNS hostnames, the initial administrator's Authelia username, an optional email, and a notification mode. **The default is operator-managed enrollment, with no email delivery or SMTP dependency.** Choosing `smtp` also collects a required email, authenticated TLS SMTP settings and a private SMTP password. The initial-user password is always prompted privately. There are no default passwords; credentials are not printed or placed in process arguments. The operator dependencies (PyYAML and argon2-cffi) are used only for supported Authelia YAML and Argon2id hashes; they are not in the ForgetMeNote container. The running application never mounts or edits the Authelia user file.

The generated directory must be **outside the source repository**. It contains `compose.yaml`, `Caddyfile`, `settings.json`, `secrets/`, `authelia/`, `data/`, and Caddy certificate storage. Keep it private. Configuration includes secrets and is written with mode 0600; directories use 0700. The container UID/GID defaults to the invoking operator (1000 when invoked as root). Setup records those values. Use the same operator for later maintenance.

Setup is safe to rerun: existing config, passwords, keys, data, and user records are not replaced. It resumes missing initialization steps but does not recreate a missing individual user when the user file already exists. After initial provisioning, a missing user database requires restoration; a marker prevents silently creating replacement identities. Generated configuration is operator-owned: rerunning does not overwrite custom changes. If moving the source checkout, update the Compose build context explicitly. The generated Authelia image is pinned to 4.39.22 and Caddy to 2.10.2-alpine; update deliberately after reviewing upstream releases.

1. Create DNS A records (and AAAA only with working IPv6) for both hostnames pointing to the VPS. Open inbound TCP 80/443 for Caddy. Do not publish ForgetMeNote 8080 or Authelia 9091. Allow outbound HTTPS, DNS, time synchronization, and SMTP only when configured; correct host time matters for TOTP. Disable any proxy cache on private routes.
2. Start from the generated directory:
   ```sh
   cd ~/forgetmenote-instance
   docker compose up -d --build
   docker compose ps
   ```
   Caddy obtains certificates after DNS resolves. An initial OIDC attempt may need retrying while certificates/services initialize. In SMTP mode, Authelia checks its SMTP connection on startup; verify actual delivery separately. In operator mode no email is sent.
3. Open ForgetMeNote, select **Sign in with your identity provider**, and sign in using the initial Authelia username and password. The optional preset uses `one_factor` for its ForgetMeNote OIDC client, so no authenticator enrollment is required. ForgetMeNote creates the user with no project access. Authelia is the external provider in this preset, so ForgetMeNote' own local password form is disabled.
4. Copy **Your settings → Sign-in and authorship details → User ID** from this authenticated session. Explicitly grant the initial administrator:
   ```sh
   docker compose stop forgetmenote
   ~/.venvs/forgetmenote-operator/bin/python -I /path/to/forgetmenote/deploy/standalone/operator.py --directory ~/forgetmenote-instance grant-admin OIDC_USER_ID
   docker compose start forgetmenote
   ```
   Replace `OIDC_USER_ID` with the exact ID you just verified. This is an offline operator action protected by the catalog's single-writer lock. It refuses absent or disabled users. Refresh the app; it now shows Admin. It does not set `ADMIN_USERS`, and later restarts/setup reruns cannot restore demoted or disabled authority.

For recovery, the existing key remains in `data/admin.token`. An authorized operator may retrieve it with `docker compose exec forgetmenote forgetmenote token` and use **Recovery administrator** on the login page. Setup does not print it. Keep recovery access private; it does not bypass TLS or automatically link provider identities.

## Accounts and two-factor maintenance

Authelia's file backend has no invitation UI. The minimal workflow is **create the Authelia account → user signs in through OIDC → assign project access in ForgetMeNote**. No second manual identity creation is needed. Existing-account linking is described in [authentication](authentication.md).

Stop Authelia before using the operator, so password-reset writes and operator changes cannot race. Account edits use an exclusive operator lock and atomic file replacement; restart reloads the backend and ends its in-memory sessions. This temporarily signs out other Authelia sessions; it does not end their ForgetMeNote sessions.

```sh
cd ~/forgetmenote-instance
docker compose stop authelia
~/.venvs/forgetmenote-operator/bin/python -I /path/to/forgetmenote/deploy/standalone/operator.py --directory ~/forgetmenote-instance add bob
docker compose start authelia
```

The same command supports `password bob`, `email bob`, `disable bob`, `enable bob`, `enroll-totp bob`, and `reset-totp bob`. Passwords are entered through private prompts; email-based password recovery is enabled only in SMTP mode. In operator mode use `password USER`; the browser password-reset feature is disabled. Usernames are stable and are not renamed/recycled: Authelia's subject mappings depend on its stored identity state. `disable` does not delete users or grant/revoke ForgetMeNote roles. Offboarding requires disabling the ForgetMeNote user too.

The generated OIDC client uses `authorization_policy: one_factor`. TOTP is available but not required. To require it, explicitly set the client policy to `two_factor`, then arrange enrollment as follows. In operator mode, create each account, run `enroll-totp USER` while Authelia is stopped, and deliver its QR privately to that user. There is no email or second manual ForgetMeNote account creation. Store authenticator recovery material securely. If a user loses their authenticator, verify their identity out of band, stop Authelia, run `reset-totp USER`, then `enroll-totp USER` and restart. Reset uses Authelia's supported storage CLI and deletes the old generated QR if present. In SMTP mode the user can instead re-enroll through browser email verification. It does not remove other second factors such as WebAuthn. Use Authelia's own documented CLI for those when enabled. No generic invitations or identity-management UI is included.

Operators can explicitly edit `authelia/configuration.yml` to change allowed methods/session durations/client policy. Choosing `two_factor` requires enrollment before the user can access ForgetMeNote; ForgetMeNote does not independently validate a provider-specific MFA claim. Validate config before restarting:

```sh
docker compose run --rm --no-deps authelia authelia config validate --config /config/configuration.yml
```

Never paste configuration validation output into public reports without reviewing it; upstream validation errors can quote invalid configuration values. The setup command captures such output instead of printing it.

## Optional email delivery

**Email functionality requires SMTP configuration.** Without it, the portal explicitly says email is disabled, browser password recovery is disabled, and device enrollment/recovery belongs to the server operator. Authelia still requires a notifier, so this mode uses its filesystem notifier. If a user tries an email-dependent settings action, the dialog says **No email was sent** and directs them to the operator; any verification artifact stays in private `authelia/notification.txt`. Do not expose that file as a web mailbox. This is an operator-managed deployment, not self-service email enrollment.

The bundle uses Authelia's supported locale overrides to replace email promises in the portal and settings. The notices are in English across all locales shipped in pinned Authelia 4.39.22; other UI text remains localized. Review these upstream keys and language lists on provider upgrades. No custom authentication code or weakened MFA policy is involved.

To explicitly switch an existing instance:

```sh
docker compose stop authelia
~/.venvs/forgetmenote-operator/bin/python -I /path/to/forgetmenote/deploy/standalone/operator.py --directory ~/forgetmenote-instance notifications smtp
docker compose run --rm --no-deps authelia authelia config validate --config /config/configuration.yml
docker compose start authelia
```

The command prompts for TLS SMTP address, username, sender and a private password. It enables password recovery, selects SMTP instead of the filesystem notifier, and removes the no-email text overrides. Configure real recipient addresses with `email USER` and verify delivery before relying on recovery. To return to operator mode, use `notifications operator` with Authelia stopped. Switching never changes user records, TOTP registrations, OIDC keys, ForgetMeNote permissions or content. Existing bundles without a recorded mode retain their SMTP behavior until explicitly changed. Setup reruns preserve existing settings.

Keep the deployment configuration and SMTP secret private. Both contain credentials; never commit them. Provider errors are captured rather than printed. If a mode switch fails, correct it and validate before restarting; do not assume delivery works merely because credentials were entered.

## Upgrades

Back up the complete stopped stack first. Update the source checkout, deliberately update pinned provider/proxy image versions when wanted, then `docker compose build forgetmenote`, `docker compose pull authelia caddy`, and `docker compose up -d`. Check service health, sign-in, project access and a synthetic save. Authelia may migrate its SQLite schema; roll back using the **complete pre-upgrade snapshot**, not merely an older container over a newer database. Never run two ForgetMeNote writers against one `/data` directory, including during a rolling upgrade.

The upgrade that introduces Postgres moves the instance's accounts, grants and other state out of `/data` once. After the backup and build, run `docker compose up -d postgres`, then `docker compose run --rm forgetmenote import-files` while ForgetMeNote is stopped (its entrypoint migrates the schema first), then `docker compose up -d`. The import prints each area's file count and digest; keep that output with the backup. It refuses a database that already holds the instance's state, so it never runs twice or after the instance served from Postgres. See [moving file state](postgres.md#moving-file-state).

The upgrade that introduces storage v2 moves every notebook from its Git repository into the database and blob store once, with `forgetmenote admin storage import` in a stopped window. This release no longer has that import: an instance still keeping notebooks in Git upgrades through an earlier release that has it, following that release's `docs/storage-cutover.md`, first.

## Backup and full restoration

Host-managed [Git remotes](backups.md) are optional content backups outside the application. They do not contain identities, project grants, GitHub links, browser/MCP credentials, Authelia passwords/TOTP registrations, encryption keys or certificates.

For a consistent full snapshot, stop **all three services**, archive the whole generated instance directory (including hidden files), and restart. Example from the directory's parent:

```sh
docker compose -f forgetmenote-instance/compose.yaml down
tar -czf forgetmenote-instance-backup.tar.gz forgetmenote-instance
docker compose -f forgetmenote-instance/compose.yaml up -d
```

Encrypt and copy backups off-host. Preserve the source revision/image versions and any external backup job configuration and credentials. The archive contains credentials; limit access. Unsaved browser drafts remain device-local.

Restore into a new empty directory with all services stopped. Restore **all** configuration, secrets, `/data`, Authelia user file/SQLite database, and Caddy data together; preserve file ownership and permissions. Correct the Compose build context if the source moved. Point DNS at the restored host (keep the same issuer hostname to preserve OIDC mappings), start exactly one stack, and verify sign-in/TOTP, user IDs/roles, recent files, assets, Git history, GitHub attribution settings and MCP. Restored ForgetMeNote sessions/tokens may still be valid until expiry; revoke them if restoring after a compromise. Authelia's in-memory sessions are intentionally not restored.

If an issuer or Authelia subject changes, use explicit account linking while existing authentication is still available; never solve this by matching email addresses or rewriting catalog identities. A project Git clone alone cannot restore access-control state.

## References and verification

Configuration was checked against Authelia 4.39.22. Official references (accessed 2026-09-07): [session providers](https://www.authelia.com/configuration/session/introduction/), [OIDC client policy](https://www.authelia.com/configuration/identity-providers/openid-connect/clients/), [file backend](https://www.authelia.com/configuration/first-factor/file/), [SMTP](https://www.authelia.com/configuration/notifications/smtp/), [TOTP operator commands](https://www.authelia.com/reference/cli/authelia/authelia_storage_user/).

Run `/path/to/operator-venv/bin/python -I scripts/oidc_smoke.py` after building `forgetmenote:oidc-test`. It uses disposable identities/data, an internal Caddy CA and loopback port 19443; it never edits host DNS. It checks password-only provider login, OIDC provisioning, MCP consent, explicit Admin, and independent full restoration. Run again with `REQUIRE_TOTP=1` to test explicit mandatory MFA. The test exercises the default no-email configuration and the actual enrollment operator, including private QR output, repeat protection, explicit portal notices and an account without an email address. Thus public certificate issuance, real SMTP delivery, and interactive email/QR enrollment remain separate operator acceptance checks.
