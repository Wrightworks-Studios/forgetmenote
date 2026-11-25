# Trusted-proxy authentication with SWAG and Authelia

Use this recipe with the [app deployment](deployment.md). It is an alternative to [OIDC](authentication.md), for an existing authenticated proxy.

Use Authelia's NGINX `auth_request` integration. Authelia authenticates the user and supplies `Remote-User`; ForgetMeNote owns Read/Comment/Edit grants. No separate ForgetMeNote account password is required. Authelia controls new browser sign-ins. Existing AI tokens are issued by ForgetMeNote: when offboarding someone, disable them in ForgetMeNote as well as Authelia to immediately revoke their AI access. Group or account-state synchronization from Authelia is not implemented.

ForgetMeNote accepts identity headers only when **both** checks pass:

1. The direct TCP peer belongs to `TRUSTED_PROXY_CIDRS` (the proxy address, not an untrusted forwarded header).
2. `X-ForgetMeNote-Proxy-Secret` matches the server's secret file. The retired `X-Gitnotes-Proxy-Secret` alias is ignored; update any proxy that still sends only that header.

Generate a secret locally, outside the source repository:

```sh
openssl rand -hex 32 > data/proxy.secret
chmod 600 data/proxy.secret
```

Ensure the container's UID can read it. In `.env`:

```dotenv
PUBLIC_URL=https://notes.example.com
PROXY_SECRET_FILE=/data/proxy.secret
TRUSTED_PROXY_CIDRS=172.30.0.2/32
ADMIN_USERS=alice
```

Replace the example address with SWAG's stable direct-peer address. Prefer an exact address; never trust a network containing untrusted containers. `ADMIN_USERS` creates only absent proxy users as Admins. Existing records, including later demotions and disable decisions, are never overwritten on restart. Manage existing users explicitly in ForgetMeNote.

In the protected SWAG location, use your installed Authelia integration and forward its **verified response**, overwriting any client-supplied values. The relevant directives are:

```nginx
# Your existing SWAG/Authelia auth_request configuration goes here.
# These variables must come from the auth subrequest response.
auth_request_set $forgetmenote_user $upstream_http_remote_user;
proxy_set_header Remote-User $forgetmenote_user;
proxy_set_header X-ForgetMeNote-Proxy-Secret "REPLACE_WITH_CONTENTS_OF_PROXY_SECRET";
proxy_set_header Host $http_host;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_pass http://forgetmenote:8080;
proxy_buffering off;
proxy_read_timeout 120s;
client_max_body_size 33m;
```

This is a location fragment, not a replacement for SWAG's full TLS or Authelia configuration. Store the secret in the protected SWAG configuration; never return it as a response header. Overwrite `Remote-User` on every protected request. Do not derive identity from a request's existing `Remote-User` or `X-Forwarded-User`.

In a trusted-proxy-only deployment, protect the web UI and account/project routes through Authelia. For a transition to OIDC, keep `/account/oidc/link` protected by the existing proxy identity but let `/auth/oidc/callback` reach ForgetMeNote without an upstream login redirect. Starting an explicit link carries that verified proxy identity in a ForgetMeNote browser session for the callback. The recovery admin key remains available; do not distribute it to ordinary users.

## MCP routes need bearer authentication, not a proxy login redirect

AI clients cannot complete an Authelia HTML login on each MCP request. Add an unprotected-at-the-proxy location for exactly these paths:

```nginx
location ~ ^/(mcp|oauth/(register|token|revoke)|\.well-known/oauth-authorization-server|\.well-known/oauth-protected-resource(/mcp)?)$ {
    proxy_set_header Host $http_host;
    proxy_set_header Remote-User "";
    proxy_set_header X-ForgetMeNote-Proxy-Secret "";
    # Releases before the alias retirement still accept this header; keep clearing it while one is a rollback target.
    proxy_set_header X-Gitnotes-Proxy-Secret "";
    proxy_set_header Authorization $http_authorization;
    proxy_pass http://forgetmenote:8080;
    proxy_buffering off;
    proxy_read_timeout 120s;
    client_max_body_size 5m;
}
```

ForgetMeNote authenticates `/mcp` itself. Registration and discovery are public; token issuance requires PKCE authorization or a valid refresh token. **For this trusted-proxy-only preset, keep `/oauth/authorize` protected upstream** so it authenticates the consenting user. With OIDC configured, ForgetMeNote can gate this route itself through its browser session/login flow. Keep any existing `^~` locations from accidentally taking precedence over this example regex. Adapt the full NGINX configuration to your SWAG version, run `nginx -t`, and test each route after installation.


References: [Authelia trusted-header SSO](https://www.authelia.com/integration/trusted-header-sso/introduction/) and [NGINX integration](https://www.authelia.com/integration/proxies/nginx/).

Header aliases must agree when both are present. Keep the existing header while a protected rollback image still requires it; see [notebook compatibility](notebook-compatibility.md).
