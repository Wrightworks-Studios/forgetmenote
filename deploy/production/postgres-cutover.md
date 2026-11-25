# Moving an instance onto Postgres

The Postgres stack replaces the single file-based service with three kinds of
Duplo services: a **notebook owner** that holds every notebook repository on its
own EBS volume, **gateways** that serve the web, API and MCP from Postgres and S3
with no disk of their own, and, for the move only, a **cutover helper**. The move
itself is `cutover.py`: one stopped window, after a rehearsal against a parallel
set of services that leaves production running. Nothing here is automatic; each
run, and any copy of production data into a rehearsal, needs its own approval.

**Storage v2:** storage v2 keeps every notebook in Postgres and the blob store and
has no notebook owner process, `owner` command or owner map. This runbook and
`cutover.py` describe the stack before it, a notebook owner and gateways. Its first
storage v2 release moves with [the storage v2 cutover of an owner and
gateways](storage-cutover.md), which turns the owner service into the gateway that
keeps the Git exports; `deploy_instance.py` and `roll_gateways.py` refuse to
deploy that release as an image update.

## What to create first

AWS, in the instance's region and VPC:

- **Postgres**: an RDS instance, encrypted, not publicly accessible, with
  automated backups and deletion protection (the cutover's preflight refuses
  anything else). It can be created in RDS directly: the tools find it by its
  identifier. Put it in the cluster's VPC with a security group of its own that
  admits port 5432 only from the cluster nodes' security group. Create one
  empty database for production and a separate one for each rehearsal; an
  import refuses a database that already holds an instance. Its URL, with
  password, is a Kubernetes secret (`url` key) and is never logged.
- **Blob bucket**: S3 with versioning, every public access block, default
  encryption and a lifecycle rule that expires noncurrent versions, 30 days for
  example ([postgres](../../docs/postgres.md)). Give production and each
  rehearsal their own prefix.
- **Blob credentials**: the gateways and helper sign S3 requests with the AWS
  default credential chain, which takes keys in the environment before a pod's
  web identity, unless `blobs.json` names `roleArn`. Grant `s3:GetObject`,
  `s3:PutObject` and `s3:DeleteObject` on `PREFIX/*`, and `s3:ListBucket` on
  the bucket with `s3:prefix` limited to `PREFIX/*`, to one of:
  - preferably, an IAM role in the cluster's account that trusts the pods'
    service account through the cluster's OIDC provider (EKS IAM roles for
    service accounts), named as `roleArn`. The store then assumes it with the
    pod's web identity token and ignores keys in the environment, such as
    release A's `AWS_ACCESS_KEY_ID` for its Redshift connector, which the
    gateways and helper carry over. The service account must carry an
    `eks.amazonaws.com/role-arn` annotation (any role) so EKS mounts the token.
  - otherwise, the IAM user whose keys release A already sets, which the chain
    then uses for blobs too.

  A bucket encrypted with its own KMS key also needs `kms:GenerateDataKey` and
  `kms:Decrypt` on that key; S3-managed encryption needs neither. With one role
  or set of keys, grant only the prefix in use: the rehearsal's while it runs,
  then production's before the cutover, so a rehearsal configuration can never
  write production's objects.
- **Durability**: render `durability.py` with the owner's StatefulSet as
  `workload`, its volume as `volume` and claim, `database_instance`,
  `blob_bucket` and the gateway service under `deployments`, then apply it.
  Keep the file-based stack's `name`: it names the locked vault and key. AWS Backup then keeps the owner's volume, the
  database and the bucket in the locked vault, and the worker reports the
  instance healthy only while all three have recent backups.

Kubernetes secrets and config maps in the tenant:

| Name (example) | Holds | Mounted by |
| --- | --- | --- |
| `forgetmenote-database` | `url`: the Postgres URL | helper, gateways |
| `forgetmenote-recovery` | `admin.token`: a new random recovery key of at least 32 characters | gateways, at `/data/admin.token` |
| `forgetmenote-owner` | `token`: the owner's shared secret, at least 32 random bytes | owner, helper, gateways |
| `forgetmenote-cluster` | `owners.json` (one entry, e.g. `pool`: the owner's in-cluster `url`, its `secretFile`, `allowHTTP` for plain HTTP, and `placeNew`) and `blobs.json` (`{"s3": {"bucket", "prefix", "region", "roleArn"}}`) | helper, gateways |

The file-based release's own configuration and provider secrets (chat, GitHub
App, Slack, OIDC or proxy, personal connection key and the rest) carry over
unchanged: mount them in the gateways exactly as release A mounts them, and in
the helper wherever the import needs them, such as the personal connection key
for `personal-connections/credentials.enc` ([postgres](../../docs/postgres.md)).

**The recovery key changes.** Release A's key stays on its volume, which this
tool never reads; the new stack uses the new key from `forgetmenote-recovery`.
After the cutover, recovery sign-in takes the new key and the old one no longer
works anywhere but on release A, so store the new one where the old one was kept.
Ordinary sessions, OAuth clients and grants move with the import.

## The three services

All three run the candidate image, published and verified as for any release
([releasing](releasing.md)), with release A's security settings: non-root,
read-only root filesystem, no added capabilities.

**Owner** (`services.owner`): one replica, `Recreate`, args `owner`, an empty
encrypted EBS claim at `/data` (ReadWriteOncePod), a writable `/tmp`, the owner
secret, `OWNER_SECRET_FILE`, `OWNER_ALLOW_HTTP=true` for in-cluster HTTP, and
`OWNER_GATEWAYS` set to the gateway replicas plus one for a rolling update.
Readiness `/readyz` and liveness `/healthz` on port 8080. Give it an in-cluster
service address and no load balancer. Only the gateways and helper may reach it:
where the cluster enforces NetworkPolicy, admit only them; where it does not,
any pod can reach the port and the owner secret alone admits a caller, so keep
that secret to the owner, helper and gateways (or serve TLS with
`OWNER_TLS_CERT_FILE` and `OWNER_TLS_KEY_FILE`, whose certificate the gateways
must trust). Start it before the cutover; it serves no notebook until the
cutover places them.

**Gateways** (`services.gateway`): the configured replicas (two by default),
rolling updates with surge 1 and no unavailability, command
`/bin/sh -c 'forgetmenote migrate && exec forgetmenote'` so each new image
migrates before it serves, `/data` an `emptyDir` (it holds caches only) with the
recovery key mounted at `/data/admin.token`, and `PUBLIC_URL`,
`DATABASE_URL_FILE`, `BLOBS_CONFIG_FILE` and `OWNER_MAP_FILE` beside release A's
settings. Probe `/healthz` for readiness and liveness: `/readyz` checks the Host
header against the origin. Create them stopped; the cutover starts them.

**Helper** (`services.helper`): one replica, entrypoint `sleep infinity`, with
the claim it imports mounted **read-only** at `/source`, the gateways' database,
blob (with their AWS keys, if the blobs use them), owner map and secret
settings, and a writable `/tmp` large enough for an
export of the control plane and blob files (it checks the import by exporting it
there and deletes the export). Create it stopped. For production its claim is
release A's, which can attach only once release A stops; for a rehearsal it is a
volume restored from one of release A's recovery points.

**Git backup worker**: release A's backup sidecar runs beside its volume; after
the cutover the notebooks live on the owner's, so the worker becomes a sidecar
of the owner service, with the owner's claim where its configuration expects
the notebooks. For managed GitHub backups, add a worker whose `owner` is the
owner map entry's `url`, with a token of its own, to the backups App
configuration: placement moves every binding to that owner, and the old `local`
worker can no longer lease ([managed backups](../../docs/managed-backups.md)).
Never run an operator-token worker against a rehearsal: it pushes without
asking a gateway, so `OUTGOING=paused` cannot stop it, and the restored copy
would reach the live destination.

The operator's kubeconfig needs `pods/exec` in the tenant namespace; preflight
checks it before stopping anything.

## Rehearse

A rehearsal runs every step against a parallel set of services and leaves
release A serving:

1. Restore a recent release A recovery point to a new volume and claim (this
   copies personal content; it needs its own approval), and give the rehearsal
   its own database, bucket prefix, owner map pointing at its own owner, owner
   secret, recovery key, origin and Duplo service names. A rehearsal whose owner
   map named the production owner would write production notebooks.
2. Set `OUTGOING=paused` on the rehearsal gateways, so the copy sends no Slack
   messages, runs no automations or reviews, refreshes no personal grants and
   leases no backups as the live instance ([outgoing paused](../../docs/postgres.md#outgoing-paused)),
   and `PUBLIC_URL` to the rehearsal origin. Preflight refuses either missing.
3. Copy `cutover.example.json` to a private path, fill it in with
   `"rehearsal": true`, and run:

```sh
.venv/operator/bin/python deploy/production/cutover.py --config CONFIG --release-dir RELEASE --plan
.venv/operator/bin/python deploy/production/cutover.py --config CONFIG --release-dir RELEASE
```

4. Sign in at the rehearsal origin, check notebooks, permissions, history,
   search and sources, and time the run: the production window is about as long.
5. Stop and delete the rehearsal services, database, prefix and restored volume.

`make smoke-cutover` runs the same tool against containers on one host (release
A, Postgres, an S3 stand-in, an owner and two gateways), including the rollback.

## Cut over

With `"rehearsal": false` and the production service names:

```sh
.venv/operator/bin/python deploy/production/cutover.py --config CONFIG --release-dir RELEASE --plan
.venv/operator/bin/python deploy/production/cutover.py --config CONFIG --release-dir RELEASE
```

It stops release A, takes and completes a stopped AWS Backup of its volume and
records the database's restore point, then the helper migrates, imports, checks
an export of the import against it, and streams the notebook repositories to the
owner, whose copy must hash the same. Every notebook is then placed on the owner,
the helper stops and the gateways start. Each phase is recorded in
`cutover.json` before the next; on a failure the instance stays stopped: read
`cutover.json`, `failure-*.json` and the command record they name, then
`--resume` or `--rollback`.

When it reports serving, move the public host's DNS (or Duplo load balancer)
from release A's service to the gateways and check the public origin.

**Rollback before any write**: `cutover.py --config CONFIG --rollback` stops the
gateways and helper and restarts release A on its untouched volume. After the
gateways served, add `--before-writes` only once you have confirmed no write was
accepted; once writes resumed, roll back with `export-files`
([postgres](../../docs/postgres.md)). Move DNS back either way.

## Deploy afterwards

Upgrade owners before gateways: a gateway may call owner methods an older owner
lacks.

- **Owner**: `deploy_instance.py` with a configuration whose `name`, `claim` and
  `volume` are the owner's ([deploying](deploying.md)). The owner stops for its
  backup and image update as release A did; gateways answer notebook requests with
  an error meanwhile.
- **Gateways**: `roll_gateways.py --config CONFIG --release-dir RELEASE`. It
  records the database's restore point, protects the image and receipt, updates
  the image and waits until every replica runs it and the origin answers;
  `--rollback` returns them to the previous image. After every gateway runs a
  release that raises the reader version, run `forgetmenote migrate
  -finish-rollout` once ([migrations](../../docs/postgres.md#migrations)).
