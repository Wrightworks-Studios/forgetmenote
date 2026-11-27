> Default deployment: one ForgetMeNote container with local username/password accounts. OIDC and trusted-proxy authentication are optional and disable local password sign-in when configured. Authelia is not bundled by default. See [authentication and first account setup](authentication.md).

# Deploy ForgetMeNote

Use the existing app-only `compose.yaml` with persistent `/data`. Choose [external OIDC](authentication.md) or the trusted-proxy integration below. Local accounts need no identity service. The [Authelia bundle](standalone.md) is an optional provider example and integration fixture. [Railway guidance](railway.md) uses the app plus a volume and external provider.

The app serves HTTP internally. Your reverse proxy, such as SWAG, terminates HTTPS and forwards the original host. `PUBLIC_URL` must exactly match the external HTTPS origin, without a subpath. The app rejects unexpected hosts and cross-origin mutations. Do not expose port 8080 directly to the Internet.

## Container and storage

Copy the repository to your server, copy `.env.example` to `.env`, and create `data/`. Set `PUID`/`PGID` to the owner of that directory, then build with Compose. Build arguments create a matching Unix user. Changing those IDs requires a rebuild and matching data ownership. Linux IDs must not conflict with users already present in the base image; the defaults are 1000:1000.

To skip the build, each release also publishes `ghcr.io/wrightworks-studios/forgetmenote`, built with the default IDs and every optional integration, with [signed provenance and a verification attestation](verification.md#published-images). Keep `data/` owned by 1000:1000, set `image: ghcr.io/wrightworks-studios/forgetmenote@sha256:DIGEST` for the `forgetmenote` service in `compose.override.yaml`, then run `docker compose pull forgetmenote` and `docker compose up -d --no-build`.

The default port binding is loopback-only. If SWAG runs in Docker, attach ForgetMeNote to the same dedicated proxy network. For example, save `compose.override.yaml` with your existing network name:

```yaml
services:
  forgetmenote:
    networks: [proxy]
networks:
  proxy:
    external: true
    name: swag_proxy
```

Configure SWAG to reach `http://forgetmenote:8080`. Keep the dedicated network restricted to trusted infrastructure. The loopback port can remain for local diagnosis; the app still authenticates requests. Use a valid certificate and `PUBLIC_URL=https://notes.example.com`.

## Authentication

Local username/password login works without additional services. For first setup, retrieve the recovery key privately:

```sh
docker compose exec forgetmenote forgetmenote token
```

Sign in through **Recovery administrator**, then complete **Create your administrator account**. This single authenticated action creates the first account with Admin authority and signs you in. It is unavailable once any user exists, even if that user is later disabled or demoted. On an existing instance, use **Administration** to manage users instead. Keep the recovery key outside project notes.

For external login, choose one recipe:

- [OIDC](authentication.md): your provider owns credentials; ForgetMeNote owns project permissions.
- [SWAG/Authelia trusted proxy](trusted-proxy.md): preserve your existing proxy sign-in.

External authentication disables local password login and the first-local-administrator flow. Follow the provider recipe for explicit initial Admin provisioning. A new OIDC user receives no project access automatically. See [account maintenance and session revocation](authentication.md) for ongoing administration.

## Authelia: one sign-in, app-managed project roles

The full [trusted-proxy recipe](trusted-proxy.md) covers header verification, the proxy secret, and MCP route exceptions. Do not add a second authentication service if you already have one.

## Startup and upgrades

```sh
docker compose up -d --build
docker compose logs --tail=50 forgetmenote
```

Before an upgrade, make a [complete instance backup](backups.md#complete-instance-backup). Update the application source to the chosen release, rebuild, and recreate the service with the same `/data` mount and authentication configuration. Run only one instance against a data directory. Verify login, a note, and its history afterwards. Never use `down -v` as an upgrade step.

The first release with storage v2 moved every notebook from its Git repository into the database and blob store once, in a stopped window. This release no longer has that import: an instance still keeping notebooks in Git upgrades through an earlier release that has `forgetmenote admin storage import`, following that release's `docs/storage-cutover.md`, first.

Git-provider backup credentials and schedules stay outside the application. Follow the [backup recipe](backups.md) for optional remote pushes and full restoration.

Optional [repository-connected notebooks](repository-notebooks.md) use a separate read-only GitHub App configured by `REPOSITORY_APP_CONFIG_FILE`. Mount its configuration, private key, client secret, and webhook secret outside notebook content. This reads upstream source and is independent of backup pushing and GitHub authorship linking. Include `/data/repository-sources` in full backups, including retained evidence and connection state. Current source keyword indexing is local; embeddings reuse the existing operator endpoint and require source-specific consent. See the source-search memory/disk bounds before enabling large repositories.

## Verify your deployment

- Sign in through your selected login method and check the account identity.
- A user with no grants sees no projects; Read cannot edit or comment; Comment can discuss but cannot edit.
- `/mcp` without a bearer token returns 401 with resource metadata, not an HTML login page.
- Direct requests with spoofed identity headers cannot authenticate.
- OAuth consent displays the expected AI client and permissions; revoke a user's project grant and verify the existing connection loses access.
- Restart the container and restore a backup on another directory before importing valuable content.

The optional [Authelia preset](standalone.md) and [Railway guide](railway.md) cover those deployment-specific steps. Backups and authentication each have their own linked recipe; the application still runs as one container with durable local storage.

## Several gateways

Notebook content and the control plane are in Postgres and the blob store, and
each notebook change is one database transaction, so several gateway processes
of the same binary can serve one instance; there are no notebook owner processes
([notebook storage](architecture.md#notebook-storage)). Gateways that share an
instance need:

- the same Postgres database and org (`server.database`);
- an S3 blob store (`server.blobs`; [blob storage](postgres.md#blob-storage)),
  because each gateway has only its own disk: content blocks, source jobs,
  repository sources and export bundles begun on one gateway continue on
  another;
- the same recovery key in `/data/admin.token`, as a mounted secret: it binds
  recovery sessions and signs OAuth consent forms, so a consent shown by one
  gateway and submitted to another fails unless they share it;
- one process per Git export directory: an export directory (`data/exports`) is
  held by one process at a time, so run the backup worker beside the data
  directory that holds the exports.

Their data directories otherwise hold only derived caches. Apply migrations once
per rollout with `forgetmenote migrate` before the new binary serves, and after
every gateway runs a release that raises the reader version, run
`forgetmenote migrate -finish-rollout` once ([migrations](postgres.md#migrations)).


## Operator resource budgets

Set these positive integer environment variables on the **application container**.
Values are bytes except object/run counts. Omitted values use the defaults below;
invalid or unknown `FMN_LIMIT_` variables stop startup. Restart after a change.
Budgets protect finite host resources; increase them with available memory/disk.
They do not replace authorization, parsing protections, model context, or upstream
format/protocol constraints.

| Variable | Default | Owner |
| --- | ---: | --- |
| `FMN_LIMIT_FILE_BYTES` | 33554432 | Individual notebook files and indexed repository blobs |
| `FMN_LIMIT_SOURCE_BYTES` | 67108864 | Source uploads, archived originals, and aggregate chat source input |
| `FMN_LIMIT_SOURCE_TEXT_BYTES` | 4194304 | Extracted text, explicit chat attachments and logical table expansion |
| `FMN_LIMIT_BUNDLE_BYTES` | 33554432 | Uploaded notebook Git bundles |
| `FMN_LIMIT_IMPORT_BYTES` | 268435456 | Expanded import objects and document ZIP contents |
| `FMN_LIMIT_IMPORT_OBJECTS` | 100000 | Import objects and archive entries |
| `FMN_LIMIT_INDEX_BYTES` | 536870912 | Derived indexes, snapshots, chunks and vector memory estimates |
| `FMN_LIMIT_HISTORY_BYTES` | 67108864 | Historical note text and derived scan budgets |
| `FMN_LIMIT_STATE_BYTES` | 67108864 | Automation checkpoints and retained chat results |
| `FMN_LIMIT_ARTIFACT_BYTES` | 33554432 | Rendered exports/charts per reply |
| `FMN_LIMIT_AUTOMATION_RUNS` | 1000 | Retained automation runs; terminal runs are cleaned before admission |
| `FMN_LIMIT_AUTOMATION_AI_WORKERS` | 8 | Concurrent independent AI automation jobs; model admission remains separate |
| `FMN_LIMIT_AUTOMATION_WORKERS` | 4 | Concurrent independent deterministic automation jobs |
| `FMN_LIMIT_READ_REQUESTS` | 2 × effective CPUs, minimum 2, default ceiling 32 | Concurrent read-only operations and their HTTP responses |
| `FMN_LIMIT_WRITE_REQUESTS` | Effective CPUs, minimum 2, default ceiling 16 | Concurrent mutating or unclassified operations and their HTTP responses |
| `FMN_LIMIT_SOURCE_REQUESTS` | Half effective CPUs, minimum 1, default ceiling 4 | Shared source import, research and automation I/O |
| `FMN_LIMIT_DECODE_REQUESTS` | Effective CPUs, minimum 2, default ceiling 8 | Concurrent API/MCP request-envelope decoding |

Action admission follows the registered tool's reviewed read-only metadata and
is shared by API, MCP, browser actions and AI callers. Reads and writes have
separate pools, so a read burst cannot consume save admission. Unknown action
classifications use the write pool. CPU defaults follow the process's effective
`GOMAXPROCS`, including container CPU allocation; explicit positive operator
values override the defaults. Set these in the container's environment (for
Compose, an environment override), rather than only in a host shell.
Body decoding retains a separate bounded permit and releases it once a typed
action has its read/write permit. A slow read therefore retains its read permit
without occupying the pool that admits incoming save envelopes. HTTP admission
remains held until both the operation and response finish, including after a
legacy MCP client disconnects. MCP listen and legacy GET notification streams
share a separate 128-stream bound, including list-change streams without a
notebook resource subscription.

All encodings and metadata count against their owning storage budget. In particular,
a base64 attachment is larger in an automation checkpoint than its original bytes.
Raising the source or artifact budget may require raising file/state budgets too.
Table expansion accounts for empty cells and sparse grids; ZIP expansion remains
bounded even for compressed inputs. XLSX’s intrinsic row/column limits remain.

Chat JSON separately configures `maxRecordBytes`, `maxEvents`, `maxConversations`,
`runSeconds`, `maxRounds`, `maxTools` and concurrency. Zero `maxConversations` means
no product quota. Connection-level execution/storage restrictions still compose
with the instance’s configuration. Data connections own `maxBytes`, `maxRows`,
`timeoutSeconds` and concurrency; repository-source JSON owns cache/evidence/file/
refresh budgets. These can be raised without the previous hidden upper ceilings,
subject to finite machine arithmetic.

Model requests use the declared model context and output capacity, with wire
encoding headroom; there is no separate 8 MiB request/stream ceiling, 128 KiB
assistant tool-result ceiling, or 12,000-character browser result cutoff. Full
results that fit stay intact. When needed, model context omits old dialogue or
known evidence with an explicit omission; original retained results and receipts
remain available for a smaller query or an export. Mandatory inputs that still
cannot fit produce an explicit context error.

Data connection discovery and calls forward upstream independently by default,
without sharing notebook read/write admission or background worker queues. An
omitted or zero `concurrent` setting adds no connector queue. A positive value
explicitly opts a connection into a shared per-connection cap; remove such legacy
caps when operating an MCP data gateway that should pass client calls through.
Authentication, operation grants, schema checks, cancellation, and per-call time
and response-size settings still apply.

Each call checks its operation's contract identity (`schemaHash`), any configured
pin and its input schema before reaching the upstream. A configured connection's
contract is its configuration, checked once at startup. An MCP or broker
connection asks its upstream: `list_data_operations` always does, and calls
reuse that answer for up to a minute under the same credential. A call whose
`schemaHash` differs from the reused answer asks the upstream again and is
refused as changed only if the upstream disagrees too; a failed call discards
the answer, so the next call asks again.

Every data call records a pending activity receipt before it runs and its outcome
after. If the outcome cannot be recorded, a failed call still returns its own
error, and a completed read fails closed: its result is withheld, and repeating
it is harmless. A write that completed is still returned,
with its receipt and a `warning` saying the outcome was not recorded, because
refusing it would invite the caller to repeat an external effect that already
happened. The activity log then holds only that call's pending receipt, and the
gateway logs `data call outcome not recorded` with the connection, operation and
receipt ID; check the upstream before anyone repeats the write.
