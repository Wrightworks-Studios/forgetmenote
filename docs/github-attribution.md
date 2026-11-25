# Per-user Git authorship and optional GitHub attribution

ForgetMeNote saves locally without a GitHub account or a network connection. The authenticated user is the Git author; `ForgetMeNote <forgetmenote@localhost>` is the committer. Each user receives a random, stable ID in the instance catalog. The default author is `User <short ID>` with `<full ID>@users.gitnotes.invalid`, so neither an SSO login nor a private email address enters new commit headers or attribution trailers.

The account mapping is separate from project roles and GitHub connections. Changing roles, linking GitHub, changing a GitHub username, or disconnecting does not replace the internal ID. Back up the complete Docker data directory and the instance database, which holds the catalog, to preserve user identities and settings across server migrations. A project Git bundle contains its commits but does not contain the instance account mapping or permissions.

All mutation paths share attribution: note saves and restores, comments and replies, resolution, locks, properties, media uploads, moves/renames, and duplication. Existing commits are not rewritten. Comments and history display the participant's configured name while they retain access to the notebook; removed or disabled participants display “Former contributor.” Relative timestamps have the full timestamp on hover. Account settings describe the default as a private notebook identity instead of showing the synthetic address.

HTTP, API and MCP results retain the original comment author IDs and Git attribution fields. Their additional `people` and `authorDisplayName` fields are presentation data derived from current notebook permissions, not a global people directory or proof about an imported commit. Git account IDs and login subjects are separate namespaces. External author names remain readable when no local account mapping exists. Older and externally created commits with no source trailers display “Legacy / external”; ForgetMeNote does not infer a verified identity from an old commit message.

New commits include these trailers:

- `Gitnotes-User`: the stable opaque local account ID, also present when GitHub attribution is enabled.
- `Gitnotes-Source`: `browser` for the web API or `mcp` for the MCP endpoint. Internal workspace operations without an authenticated request use `system`.
- `Gitnotes-Client`: the registered OAuth client ID bound to an MCP access grant, when available. This identifies the authorized client registration, not a verified software brand. Self-reported MCP initialization names, User-Agent, tool arguments, and arbitrary identity headers are not used. The shared bootstrap admin bearer has no reliable client registration, so no client is recorded for it.

Git author headers and trailers describe the server's attribution; this does not add cryptographic commit signing. Imported Git metadata remains historical data.

## GitHub OAuth application setup

This optional integration currently supports GitHub.com. Use a dedicated **OAuth App** for this ForgetMeNote deployment. It requests **no OAuth scopes**, using only the authenticated public profile to verify account ownership. It does not request `repo`, `user`, `user:email`, organization access, or any repository permission.

1. In GitHub **Settings → Developer settings → OAuth Apps → New OAuth App**, register an application.
2. Set the homepage URL to your ForgetMeNote `PUBLIC_URL` (for example `https://notes.example.com`).
3. Set the authorization callback URL to exactly `https://notes.example.com/account/github/callback`, replacing the origin with your `PUBLIC_URL`. For a local test deployment, the callback can be `http://localhost:8080/account/github/callback`.
4. Copy the client ID and create a client secret. Store the secret in a file outside every project repository. Make it readable by the container's configured UID, and mount it read-only.
5. Set `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET_FILE`, then recreate the application container. Leave both unset to disable new GitHub connections. Existing local saves and disconnect controls continue to work.

Example Compose override:

```yaml
services:
  forgetmenote:
    environment:
      GITHUB_CLIENT_ID: "your-oauth-app-client-id"
      GITHUB_CLIENT_SECRET_FILE: /run/secrets/github-client-secret
    volumes:
      - ./secrets/github-client-secret:/run/secrets/github-client-secret:ro
```

Do not commit the secret or place it in a project workspace. Keep `PUBLIC_URL` accurate behind SWAG, and apply the same authentication routing to `/account` and `/account/github/*` as the rest of the app. The callback must return to the signed-in ForgetMeNote user. Do not expose an unauthenticated identity-header bypass for callbacks.

Use a dedicated identity-only app: GitHub can return previously authorized scopes when an OAuth app is reused. ForgetMeNote rejects tokens with extra scopes. If this happens, revoke the previous authorization in GitHub's application settings and reconnect.

## Connecting and choosing attribution

Open **Your settings → GitHub attribution → Connect GitHub**, authorize the account, and return to ForgetMeNote. Connecting verifies and records the account; local attribution remains the default until you opt in.

To opt in, select **Use GitHub attribution for future commits**, paste the `noreply` address shown in [GitHub email settings](https://github.com/settings/emails), and save. ForgetMeNote accepts only an address matching the numeric ID and login returned by the authenticated GitHub API. For accounts created before July 18, 2017, it also accepts the legacy username-only format belonging to that same account. Arbitrary author names, private addresses, and addresses belonging to another account are rejected.

The copy step is intentional: older accounts can have different `noreply` formats depending on their email privacy settings. Profile authorization verifies the account; it does not reveal which legacy email privacy format the user has enabled. Copy the actual address from GitHub, rather than guessing it. If needed, enable “Keep my email address private” in GitHub first. GitHub documents how to switch older accounts to an ID-based address.

Future commits use the verified GitHub login as author name and the selected account-associated `noreply` address. GitHub can associate those commits with that account when the project is pushed. If you change your GitHub username, disconnect and reconnect to refresh the login and opt in again. An ID-based `noreply` address preserves association through username changes better than the legacy format.

**Disconnect GitHub** immediately returns future requests to the same local identity and cancels any earlier authorization still in flight. Past commits retain their authors. A save already authorized and in flight keeps its captured request identity. Reauthorization is not login to ForgetMeNote and never grants project or Admin permissions.

Authorization uses a one-time expiring state, a separate HttpOnly browser binding cookie, PKCE, an exact callback URL, and a subject match. One GitHub account can be linked to only one local account in an instance. Tokens are used only during account verification, discarded afterwards, and never stored in ForgetMeNote or used for backups. Disconnect changes local settings; you can separately revoke the OAuth app authorization in GitHub's application settings.

## Backups stay separate

An optional external backup job can push all project commits, regardless of author, using the host operator’s credentials. ForgetMeNote does not push or store backup credentials; see [host-managed backups](backups.md). Connecting GitHub does not configure a remote, change SSH keys or credential helpers, grant ForgetMeNote permissions, push changes, or give ForgetMeNote personal repository credentials. Opening pull requests or other GitHub actions on behalf of users is not implemented.

## Verification and references

Automated tests use an isolated GitHub-shaped HTTP server to check the OAuth exchange, authenticated profile lookup, PKCE, state/session/user binding, expiry, replay, opt-in validation, disconnect, persistence, and offline commits. A real GitHub consent flow and profile association after pushing require the self-hoster's registered OAuth app and account; synthetic tests cannot verify those external settings.

Official references, checked September 7, 2026:

- [GitHub OAuth web application flow](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)
- [OAuth scopes and no-scope public profile access](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps)
- [GitHub noreply address formats](https://docs.github.com/en/account-and-profile/reference/email-addresses-reference)
