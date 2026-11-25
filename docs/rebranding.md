# Upgrade from Gitnotes to ForgetMeNote

The product is **ForgetMeNote** (no spaces). The repository, Docker image, CLI command, Go module and companion plugin use lowercase **forgetmenote**. No domain or image registry publication is implied by this rename; build the Docker image from this repository as before.

Stop the old app using its existing Compose configuration. Back up the full instance, then run the new image against the **same** `/data` volume with the same UID/GID and environment settings. The source Compose service is now `forgetmenote`; an existing operator-owned deployment may retain its `gitnotes` service name and update only its image to `forgetmenote:local`. Never run both against one data directory. This changes neither project repositories nor user permissions.

Keep `PUBLIC_URL`, your provider's issuer, OIDC client ID/secret, and GitHub callback registrations unchanged when upgrading an existing instance. New optional-provider configurations use `forgetmenote` as the client ID; do not recreate an existing provider client merely to rename it. Its display name can be changed independently. The standalone operator recognizes both old and new application service names and preserves existing generated configurations.

The app deliberately retains compatibility identifiers used by saved data and integrations:

- `.gitnotes-locks.json` and `Gitnotes-User` / `Gitnotes-Source` / `Gitnotes-Client` commit trailers.
- `@users.gitnotes.invalid` local attribution addresses, so authors keep their stable Git identities.
- Browser link cookies, draft keys and saved preferences. The `gitnotes` session cookie and the `X-Gitnotes-Proxy-Secret` header have since been retired: people who upgrade straight from a Gitnotes image sign in again, and trusted proxies must send `X-ForgetMeNote-Proxy-Secret`.
- `/home/gitnotes` for existing SSH mounts and a `gitnotes` executable alias inside the new image for existing operator commands.

New system commits use `ForgetMeNote <forgetmenote@localhost>`. Old commits are never rewritten. The UI, new CLI/image names, documentation, plugin and MCP server display the new brand. Existing installed plugins still reach the same `/mcp` endpoint; use `scripts/configure_plugin.py` for a newly branded copy.

To verify an upgrade with disposable data, retain the old image and run:

```sh
OLD_IMAGE=gitnotes:local NEW_IMAGE=forgetmenote:local python3 scripts/rebrand_smoke.py
```

This checks an actual old-image account and its sign-in again after the retired session cookie, Git attribution, content/history and lock enforcement after switching images. It never touches your running instance or its data.
