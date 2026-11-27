# Instance configuration

Operators can use one private, sectioned `instance.json` file. Set
`INSTANCE_CONFIG_FILE` to its absolute path and mount it read-only outside every
notebook repository. Credentials remain separate mounted files; never put their
contents into configuration, notebooks, exports or deployment logs.

```json
{
  "version": 1,
  "server": {"origin": "http://localhost:8080", "headless": false},
  "identity": {},
  "ai": {},
  "integrations": {},
  "modules": {},
  "notebooks": {}
}
```

Sections have typed, strict schemas; unknown fields, trailing JSON and unsupported
top-level versions fail before the application opens. Feature owners validate
their semantic configuration before recovery and request admission; failure
closes partial construction. `server` accepts `data`, `listen`, `origin`,
`headless`, `browseExcluded` and `outgoing`. Explicit command-line flags override
these server defaults. Omitted server settings retain the existing container
controls (`DATA_DIR`, `LISTEN_ADDR`, `PUBLIC_URL`, `HEADLESS` and
`BROWSE_EXCLUDE_PREFIXES`). A false `headless` value explicitly enables the UI.
`server.outgoing` is `"paused"` on a restored copy beside the live instance, so
it sends no Slack or email, runs no automations or background paid AI, grants no
backup lease and refreshes no personal token; see [outgoing
paused](postgres.md#outgoing-paused). Legacy installations set `OUTGOING`; like
`DATABASE_URL_FILE`, it conflicts with `INSTANCE_CONFIG_FILE` rather than being
ignored, and `migrate-instance-config` carries it into `server.outgoing`.
`server.database` (`urlFile`, optional `org`) names the optional Postgres control
plane; legacy installations use `DATABASE_URL_FILE`. The URL file is a private
mounted secret, read only at startup and never logged. Serving refuses a database
whose schema lacks this binary's migrations; run `forgetmenote migrate` first.
See [Postgres transition](postgres.md). `server.blobs` names where files kept
outside the database live: omitted, they stay in the data directory; `directory`
names another absolute path; `s3` (`bucket`, optional `prefix`, `region`,
optional `endpoint` and `allowHTTP`, optional `roleArn`) names a bucket the
gateways share. Legacy
installations put that object in the file `BLOBS_CONFIG_FILE` names.

`identity` accepts `admins`, `proxy` (`secretFile`, `trustedCidrs`), `oidc`
(`issuer`, `clientId`, `secretFile`, `defaultGroups`, `requestEmail`) and identity-only `github`
(`clientId`, `secretFile`). `ai` accepts `chat`, `decisions` and `embeddings`
(`url`, optional `secretFile`). Chat and decisions retain their documented
feature schemas, model capacities, privacy policy and mounted secret references.

`integrations` accepts inline `data`, `personal`, `automations`, `slack`,
`repository` and `backups` configurations using their existing strict schemas,
plus the optional `python` executable. Notebook owner processes are gone with
storage v2, so the `notebooks.owners` map and the legacy `OWNER_MAP_FILE` are
refused ([notebook storage](architecture.md#notebook-storage)).

`modules` names the feature modules an instance turns off, in its one field
`off`. Omitted or empty, the instance runs every module, as it always has. The
modules are `chat`, `automations`, `judgments`, `data-connections`, `sources`,
`repositories`, `tasks`, `messaging`, `personal`, `memory` and
`managed-backups`. Three need others: `tasks` needs `automations`; `messaging`
needs `automations`, `tasks`, `memory` and `chat`; and `personal` needs
`automations` and `messaging`. So this turns off task queues and the two
modules that depend on them:

```json
{"version": 1, "modules": {"off": ["tasks", "messaging", "personal"]}}
```

An unknown name fails before the application opens, and so does a module left
on while one it needs is off; that error names both modules.

Three modules can be turned off so far: `judgments`, `data-connections` and
`sources`. Each other module's wiring lands separately, and until it has,
startup refuses an `off` list that names the module, so that no module runs
while its operator believes it is off. An instance that turns a module off:

- offers none of its tools: they are absent from MCP and `/api/v1/actions`,
  and its routes answer 404;
- runs none of its workers and takes none of its leases. With `sources` off,
  nothing could resume a source job, so startup retires the jobs left from
  when the module ran and removes their files at once, however young, rather
  than keep uploads for the rest of their 24 hours. A job another gateway that
  still runs the module holds is left to it;
- reads none of its settings: with `judgments` off, `ai.decisions` and chat's
  judgment settings are ignored, and with `data-connections` off,
  `integrations.data`. Their credentials stay where the operator put them,
  unread;
- keeps its data. Migrations still run, its records and files stay, account
  deletion and purge still reach them, and turning the module on again finds
  them as they were. Archived sources stay readable notes with their originals;
  they are neither updated nor verified while `sources` is off.

What other features ask of an off module answers that it is not available:
chat installs no judge and search offers no judgment ranking, and a person's
saved data watches are skipped and kept, so their assistant goes on without
them and they cannot be changed until data connections are on again. The
browser leaves out the module's controls, and Administration's AI settings and
instance status say it is off.

The setting is read once at startup, before recovery; changing it means
restarting every gateway, and a configuration reload with a different set is
refused. It has no legacy environment variable. The startup log and
Administration's instance status list each module and whether it is on.

Configured providers do not enable AI processing or grant notebook access.
Instance policy, private Inbox consent, connection grants and notebook roles
remain authoritative. Startup logs and Administration expose the same
credential-free summary of installed integrations; they do not expose operator
configuration, endpoints, file paths or secret values.

## Migrate an existing installation

Preserve the deployment configuration and its mounted files. Run this offline
operator command with the installation's existing environment:

```sh
forgetmenote admin migrate-instance-config --data /data --output /private/instance.json
```

The destination directory must already exist outside notebooks. The command
creates a new mode-0600 file, synchronizes it and its directory, refuses an
existing destination and leaves all source files unchanged. It copies inline
configuration and credential references; it does not activate jobs or make
provider requests. Review the private result locally, mount it read-only and
replace the legacy integration variables with `INSTANCE_CONFIG_FILE`.

Mixed sectioned and legacy integration inputs are rejected by name, rather than
silently merged. The authoritative legacy list is
`internal/instanceconfig/config.go:LegacyEnvironment`. Existing installations
without `INSTANCE_CONFIG_FILE` still use those supported inputs through the
same typed installation methods. Supported feature-level file entrypoints also
remain for current consumers.

The Docker image's installed `/usr/local/bin/python3` interpreter is a runtime
default: an omitted `integrations.python` retains it, while an explicit section
takes priority. A different legacy `AUTOMATION_PYTHON` setting still conflicts
with the sectioned file. Compose preserves the image's default for existing
installations. Local operator commands also use sectioned data and notebook
visibility defaults; an explicit `--data` continues to override the file.
The recovery `token` command follows valid sectioned data too. If the file is
unreadable, use `forgetmenote -data /actual/instance/data token`; explicit recovery
does not require healthy provider configuration and never guesses another
instance's directory.

Rollback is restoring the previous deployment environment and mounts. This
configuration migration does not modify notebook or conversation storage.
Storage migrations have their separate stopped-instance procedure in
[storage migrations](storage-migrations.md). A configuration failure aborts
startup and closes partial construction; do not serve a partially configured
instance.
