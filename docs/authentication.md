# Authentication and account ownership

ForgetMeNote defaults to local username/password login when no external authentication is configured. OIDC or a trusted proxy automatically replaces local password sign-in. The recovery administrator key remains available in every mode and is an emergency operator credential, not a personal account. Authentication never implicitly grants project permissions.

## Local accounts (default)

Follow [deployment and initial administrator setup](deployment.md#authentication). Setup requires an authenticated recovery session, creates the first local administrator atomically, and signs them in. It is disabled once any user record exists, and with OIDC or trusted-proxy authentication configured. Restarting, disabling or demoting an administrator does not reopen setup. There is no public registration or first-visitor takeover.

Admins invite additional users with single-use links and can issue one-hour local password-reset links. Invitations grant only their selected groups and notebook roles, never Admin authority. The existing direct local account/password API remains supported; direct account creation grants no notebooks or Admin authority. See [organization administration](instance-administration.md). Assign instance authority in **Administration** and project roles in **Notebook settings → People**. Users change their own password under **Your settings**, supplying the current password. For a lost administrator password, use recovery access and the Admin reset form. There is no email delivery service; Admins copy and deliver invitation/reset links. Resetting/changing a password revokes the target's browser and MCP sessions; disabling a user also revokes them and preserves the account rather than recreating it.

Usernames are case-insensitive, start with a letter, and accept letters, digits, dots, underscores and hyphens up to 64 characters. They map to random stable internal IDs; matching a proxy username or provider email never claims that person's account. Passwords are 12–256 UTF-8 bytes, stored only as independently salted PBKDF2-HMAC-SHA256 hashes (600,000 iterations, 16-byte salt, 32-byte result) in the private catalog. Only a Go test binary hashes new passwords at a lower cost, and a server accepts stored hashes only at 600,000. Go's standard-library implementation adds no dependency or runtime service. No plaintext passwords appear in API responses, logs or project Git history. Use HTTPS remotely.

Local credential endpoints enforce same-origin requests, generic login failures, at most four concurrent password operations per process, and fixed one-minute budgets (10 per submitted account key, 30 per TCP peer, 120 total). Recovery-key sign-in at `/login` shares these budgets as one account key in every authentication mode: at most 10 wrong recovery keys are accepted per minute, while a correct key returns its charge so repeated valid sign-ins never lock out the operator; the recovery bearer header is unaffected. Forwarded IP headers are not trusted. Budgets are kept in Postgres and shared by every gateway, so they survive restarts; a reverse proxy shares its peer budget, so large deployments should use their external provider/edge controls. This is a small-instance implementation, not a complete identity-management product or an audited authentication service.

## External authentication takes precedence

Configuring all OIDC settings or a trusted proxy disables local password login, creation and changes in both UI and API. GitHub authorship linking does not disable local login. A provider outage never falls back to passwords. Removing external-auth configuration explicitly re-enables any retained local credentials; switching modes does not erase accounts or rewrite Git history.

Existing authenticated browser/MCP sessions keep their normal expiry and remain subject to revocation and permissions. To immediately end them when changing modes, use the existing per-user or full-instance revocation procedure below. For local-to-OIDC migration, sign in locally **before** enabling OIDC, retain that session, and use **Link OIDC to this account** after enabling it. Link before the session expires and before signing in through OIDC as a new account. Local password changes and UI are disabled once OIDC is configured, but a still-valid session can perform explicit linking. No accounts are matched automatically.

## External OIDC

Register a confidential web client at your provider with:

- Redirect URI: `https://notes.example.com/auth/oidc/callback` (exactly).
- Authorization code grant, `openid` scope, PKCE S256, signed ID tokens.
- Client-secret authentication at the token endpoint. The OAuth library supports provider-discovered endpoints and standard basic/post client authentication.
- Provider policy requiring the authentication factors appropriate to your deployment. ForgetMeNote imposes no MFA requirement. The optional Authelia preset uses `one_factor` by default; operators may explicitly choose `two_factor`.

Configure the app:

```dotenv
PUBLIC_URL=https://notes.example.com
OIDC_ISSUER_URL=https://auth.example.com
OIDC_CLIENT_ID=forgetmenote
OIDC_CLIENT_SECRET_FILE=/run/secrets/oidc-client-secret
```

Mount a readable secret file into the container, or use a Docker Compose secret at that path. All three OIDC values are required together. `PUBLIC_URL` is the external origin without a path. The issuer must match discovery and ID tokens exactly, including any trailing slash or realm path. HTTPS is required except for loopback development. A private CA can be supplied through Go's `SSL_CERT_FILE`; do not disable certificate verification.

Login uses authorization code flow, random browser-bound state, nonce, and PKCE. `github.com/coreos/go-oidc/v3` v3.21.0 verifies signatures, issuer, audience and expiry; `golang.org/x/oauth2` v0.36.0 handles the code exchange. No provider access/refresh tokens are retained. Pending logins are stored in Postgres, so the callback may reach any gateway, and expire after ten minutes. Discovery is lazy so provider outages do not prevent recovery or existing sessions; new logins fail closed with an actionable error. Restart after provider endpoint changes.

Do not put proxy login redirects in front of OIDC callbacks or MCP protocol routes. ForgetMeNote authenticates those routes itself. Existing trusted-proxy deployment remains supported; see [SWAG configuration](trusted-proxy.md).

## Provisioning and linking

After a verified first login, ForgetMeNote creates an internal user with **no Admin authority and no project access**. `(issuer, subject)` is the identity key, not email, display name or username. Admins can assign display names; Owners can search enabled people by name or exact stable User ID in Notebook settings → People. Project Owners can grant roles only within projects they own. Adding a project member does not create an Authelia/provider account.

When an account has no display name, OIDC sign-in uses the email address from the verified ID token as its default, including on the next login for an existing unnamed account. Existing display names and the login's `openid` scope are preserved; the provider must include the email claim and `email_verified: true` in its ID token. A missing, malformed, unverified (including an absent `email_verified` claim), or overlong address leaves the default as **Contributor**. The email does not identify or link accounts, grant access, or change Git authorship.

Set `requestEmail: true` on `oidc` (legacy `OIDC_REQUEST_EMAIL=true`) to also request the `email` scope. Google, for example, leaves the address out of the ID token without it; the provider's client must allow that scope. Each sign-in keeps its verified address, and the Google Workspace domain from the `hd` claim, on that external identity, replacing the previous sign-in's values; an unverified sign-in clears them. Their one use is pairing chat members by email, which an administrator turns on separately (see [personal assistant](personal-assistant.md#pairing-by-email)). An identity has none until its next sign-in after this release.

For an existing local or trusted-proxy account, sign in using that existing method, open **Your settings → Sign-in and authorship details → Link OIDC to this account**, then authenticate with the provider. Both the existing session and verified provider identity are required. When starting from a trusted-proxy login, ForgetMeNote issues its normal browser session for that verified user so the public callback need not receive proxy identity headers. Permissions, stable Git authorship ID, and optional GitHub link remain on the original account. The recovery administrator cannot be linked as a personal identity.

An Admin can link an identity for someone instead: **Administration → People**, select the person, then **Link a sign-in identity** with the provider's issuer and subject (`bind_external_identity`). Use the subject the provider issues for that person, never an email match.

Link **before** first signing in through OIDC as a new account. An identity already attached to another account is rejected; there is deliberately no account merge or automatic transfer. For a proxy-to-OIDC transition, retain proxy login until each existing user has linked. Multiple verified identities may be linked to one account; changing provider issuer/subject never silently moves access. Cross-provider provisioning and a link-removal/merge administration UI are deferred.

Disabled records and identity mappings remain stored. Subsequent OIDC login refuses the disabled account instead of creating or reactivating it. Re-enable only through an explicit ForgetMeNote Admin action. `ADMIN_USERS` now seeds absent trusted-proxy users only. It never updates an existing record, including a disabled or demoted administrator. For an existing user, use the administration UI or the explicit offline `forgetmenote grant-admin USER_ID` command.

## Trusted-proxy user registration

A verified trusted-proxy request (for example, Authelia through SWAG) registers
the user in ForgetMeNote's people directory when they first access the app. Both
the trusted socket peer and the shared proxy secret must match; a `Remote-User`
header alone cannot register an account. Logging into Authelia without visiting
ForgetMeNote does not populate the app's directory.

New directory entries grant no Admin rights, notebook-creation permission, or
notebook membership. Owners and Admins can then find the user in **Notebook
settings → People** and explicitly assign a role. Existing directory entries,
including disabled accounts, are never reset by registration. Legacy authenticated
accounts missing a directory entry are repaired on their next app request while
preserving their stable identity, GitHub attribution, and session-revocation epoch.
Account and directory creation are persisted together; a storage failure returns
an error rather than reporting a successful login.

## Sessions, logout, and offboarding

| Credential | Lifetime / revocation |
|---|---|
| ForgetMeNote browser session | Absolute 12 hours; no sliding renewal. Opaque token stored hashed in the instance database. Sign out deletes the current session. |
| MCP access token | Up to one hour, bounded by the authorization lifetime; user and current project permissions checked on every call. |
| MCP refresh token | Thirty days from issue/rotation; each exchange renews it, bounded by 90 days from consent. Disable/revoke invalidates refresh too. |
| Authelia session in the bundle | One hour maximum, fifteen minutes inactive; no remember-me. In-memory sessions end when Authelia restarts. |
| Recovery key | No automatic expiry; keep private, rotate explicitly. |
| API credential (`fmn_`) | 1–365 days as requested; issued through an OAuth grant, at most that grant's remaining 90-day authorization and revoked with it. |

On an HTTPS `PUBLIC_URL` the browser session cookie is `__Host-forgetmenote` and the OIDC sign-in binding cookie is `__Host-gitnotes-oidc` (Secure, `Path=/`, no `Domain`), so a sibling subdomain cannot plant either for login CSRF. The retired `gitnotes` session cookie is neither issued nor accepted; logout still expires it. During the transition a session issued under the earlier `forgetmenote` name is still accepted and moved to the `__Host-` name on its next request, so upgrading signs nobody out; a planted cookie that disagrees with the current session is rejected rather than chosen. Sign-ins started before an upgrade must be started again, as before. Plain-HTTP loopback development keeps the `forgetmenote` and `gitnotes-oidc` names.

OAuth endpoints have budgets independent of password sign-in, also shared by every gateway. Registration, consent and unauthenticated token requests share 240 per TCP peer and 1,200 in total per minute. A token request that presents a refresh token or authorization code ForgetMeNote issued is instead limited to 30 per account per minute, so unauthenticated traffic sharing a reverse-proxy peer cannot block refresh. Revocation keeps its own 240-per-peer budget. A client registration that never completes a code exchange is removed after one hour, and a full table of 256 registrations displaces the oldest such registration; registrations that have connected, and those stored before this rule, keep the 30-day rule. Each account holds at most 64 live refresh tokens: a new authorization beyond that ends the account's oldest other authorization, including its access tokens and API credentials minted through it, and that client must reconnect. OIDC sign-in starts are limited to 30 per peer and 120 in total per minute; when 256 sign-ins are pending, the oldest is dropped and must be started again.

An OAuth grant with `notes:manage` can mint API credentials (`create_api_credential`, and `issue_service_credential` for Admins) only with scopes that grant itself holds; requesting any other scope returns 403. The credential expires no later than the grant's absolute authorization end, and revoking that authorization (`/oauth/revoke`, CLI `auth logout`, or refresh-token replay detection) also revokes every credential minted through it. The instance database records the issuing authorization on the credential. Credentials issued from a browser session, the recovery key or the stopped-instance `forgetmenote admin credential` command, and credentials issued before this rule, keep their requested scopes and lifetime.

New OAuth consent lasts at most 90 days; existing authorizations retain their original end date. A just-used refresh token has a 30-second retry window, including when its own expiry falls inside that window, for the same client and resource, provided the authorization remains active. Reuse outside that window while the consumed token remains unexpired revokes that authorization and records `oauth.refresh_replay`, without recording tokens. The entry's `usedAgeSeconds` is how long ago the replayed token was first exchanged (absent when it was never exchanged), and `chainAlreadyRevoked` reports whether the authorization had been revoked before this replay. Refresh does not interrupt MCP sessions or resource subscriptions. Catalog storage failures return HTTP 503 without consuming a valid refresh token; a fenced catalog must be recovered by restarting before refresh can succeed.

**Your settings → Revoke your browser and AI sessions** invalidates all ForgetMeNote browser sessions, MCP access/refresh tokens and pending MCP consent for that user. Admins can select a user under **Administration** and use **Revoke browser and AI sessions** without changing project roles. Disabling a user there also invalidates sessions/tokens immediately, and re-enabling does not resurrect them. Removing Admin authority likewise revokes existing sessions/tokens and organization grants; restoring Admin does not revive them. The user can sign in again with their current permissions. Credentials, permissions, scope and instance AI policy are checked once per project request or tool call and at each persisted disclosure; an API credential is looked up again before its tool result is returned. Work still in flight, such as a live-note long poll or a chat stream, is rechecked every second. Each gateway caches sign-in state for at most 250 ms, so a revocation made on one gateway takes effect on the others within that time; the gateway that made it applies it at once.

ForgetMeNote logout/revocation does not end the identity-provider session. A user with a valid Authelia session can sign in again. Likewise, logging out of or disabling someone in Authelia does not retract already-issued ForgetMeNote browser/MCP credentials: OIDC login is not continuous account-state synchronization. To offboard, **disable in ForgetMeNote and in Authelia**, and restart bundled Authelia after account maintenance to end its in-memory sessions. For an external provider, use that provider's session-revocation controls. Trusted-proxy installations must revoke/disable upstream access as well.

MCP remains ForgetMeNote' own user-bound OAuth service: unauthenticated consent redirects to the login page, OIDC returns to the original consent request, and the client receives a ForgetMeNote token only after approval. Connecting GitHub for Git commit attribution is entirely separate and changes neither login nor authorization. Remote backup credentials belong to an optional external backup job, outside the app.

Recovery key rotation invalidates recovery browser sessions, but does not revoke personal browser sessions or MCP grants. To revoke all browser and MCP sessions, stop ForgetMeNote, preserve a database backup, then delete the stored sign-in state (`DELETE FROM documents WHERE kind = 'oauth'`) and restart; all clients must reconnect. For recovery grants specifically, an Admin can revoke `@local-admin` through `/account/sessions/revoke`. Recovery bearer access itself requires key rotation. Legacy signed recovery cookies continue until their existing expiry/key rotation (or recovery-user session revocation). OAuth grants must carry an authorization family and its absolute expiry; invalid stored OAuth state is rejected at startup, and requests then fail closed. Delete incompatible state only while stopped and after preserving a database backup; doing so clears browser sessions, registered clients and API credentials, which must be re-established. Valid family-bearing state needs no reset. No notebook content or Git history is changed.

## Storage and backup

The catalog, in the instance database, retains existing user/project keys and adds local credential mappings, issuer/subject mappings and revocation epochs. Existing `accounts` and GitHub links are preserved. The instance database holds hashed browser sessions, OAuth clients, tokens and API credentials as one versioned document that each gateway rereads at most every 250 ms, and at once for a token it does not hold, and changes under a row lock. While the database is unreachable, a gateway refuses every credential once its copy is older than 250 ms. Neither lives in project Git repositories. Back up all `/data` and the database, not just project repositories; for bundled Authelia also preserve its user file, SQLite database and encryption/signing secrets. See [complete restoration](standalone.md#backup-and-full-restoration).

Password references (accessed 2026-09-07): [Go PBKDF2](https://go.dev/pkg/crypto/pbkdf2/), [OWASP password storage work factors](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#pbkdf2).

Account creation, notebook-creation grants, and scoped membership controls are described in [Notebooks and people](workspaces.md).
