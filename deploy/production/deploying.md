# Deploying to Duplo

Deploy an already published, verified image to the pilot service. Images come from [releasing](releasing.md).

## Deploy through Duplo with one stopped checkpoint

`deploy_instance.py` deploys an already published, verified image. It uses the
existing AWS Backup vault, encrypted recovery bucket and configuration-export
Lambda from the [AWS recovery setup](../../docs/aws-recovery.md). It provisions
no infrastructure and does not change backup schedules, retention or alarm recipients.

Copy [deploy_instance.example.json](deploy_instance.example.json) to a private
operator directory and fill in existing resource IDs. Install no new keys: use
your existing AWS profile, GitHub CLI login, Duplo login and kubeconfig. `aws`,
`gh`, `duploctl`, `kubectl`, `docker` and `git` must be on `PATH` (including a
virtual environment's `bin` directory if that is where `duploctl` is installed).
Finish interactive logins before beginning a maintenance window. Use one operator
at a time; the local lock prevents simultaneous commands on this host, not another
administrator changing the service from another machine.

For a hostname change, prepare DNS, HTTPS and the identity provider's callback
first. Update affected integration callbacks as part of the same cutover. Pass
`--public-url https://new.example.com` to the deployment command, keeping the
operator configuration's `public_url` at its previous value for this receipt.
The stopped update changes only the explicit `PUBLIC_URL` environment value and
matching Host headers on application health probes. Other environment values,
secrets, ingress and integration settings are untouched; configuration drift
still aborts the update. Readiness is checked at the new URL.

The new URL is recorded for resumption. An origin-only change can reuse the
current image by supplying its verified directory to both `--previous-release`
and `--release-dir`; no image rebuild is needed. Preserve the original operator
configuration with this deployment's records, then use the new origin for future
deployments. Employees download the companion for the new origin and sign in
again; OAuth audiences are not migrated or weakened.

Paths default from the configuration. Deployment state lives under its optional
`state_root` (default: the directory holding the configuration file). A new
deployment uses `STATE_ROOT/deploy-YYYYMMDD-REVISION`; the previous release is the
newest `ready` deployment there whose release receipts match the image it deployed;
`--status` and `--resume` use the newest deployment. The plan prints the chosen
`state_dir` and `previous_release`. Pass `--state-dir`, `--previous-release` or
`--release-dir` to override any choice.

```sh
# Local plan only: no cloud calls or mutations. Plans inspect committed HEAD.
.venv/operator/bin/python deploy/production/deploy_instance.py --config /secure/deployment.json --plan

# A reviewed compatible runtime update; publishing must have completed first.
.venv/operator/bin/python deploy/production/deploy_instance.py --config /secure/deployment.json \
  --compatibility compatible \
  --compatibility-note 'Reviewed storage and startup changes; the previous image can read all newly written data.'

# Later: record completion or report failure without stopping or redeploying.
# --wait blocks (up to --timeout seconds) until the backup completes or fails.
.venv/operator/bin/python deploy/production/deploy_instance.py --config /secure/deployment.json --status --wait
```

Use real review evidence in the compatibility note. For migrations, destructive
changes or uncertain backward compatibility, omit `--compatibility` (default
`unknown`) or use `--compatibility migration`; both wait for the stopped backup
to **complete** before updating the image. A `pending` snapshot alone is not yet
a restorable backup.

To shorten that wait, these two modes take a **warm-up** backup of the running
volume just before the stop and wait for it to complete. EBS snapshots are
incremental, so the stopped snapshot then copies only the blocks written since
the warm-up. Before, it copied everything written since the previous snapshot,
often hours earlier: about 8 minutes of outage for `da1cf97` and 4 for `0a043d7`.
The warm-up is an AWS Backup job in the same vault, with the same role, tagged
`Purpose=release-warmup` and `Consistency=crash`. It is kept for 7 days, the
vault lock's minimum; the lock fails jobs with shorter retention. It is
crash-consistent and **never a restore point**. Receipts name only the stopped
backup as `backup_job`, and `--status` reports only that backup. Preflight never
chooses a warm-up as the fallback, and only the stopped snapshot can be tagged
`Consistency=application`. Restore from the stopped upgrade backup or a scheduled
backup, never from a `Purpose=release-warmup` point. `deployment.json` records
the warm-up's job and outcome under `warmup`.

If AWS rejects the warm-up request, or its job fails, the deployment continues
with the unchanged stopped sequence, as without a warm-up. A failed warm-up does
not block later deployments, although the daily console monitor still lists it
as a failed job. AWS Backup runs only one job per resource at a time. So if the
warm-up has not finished within `--timeout`, or its state cannot be read, the
deployment stops *before* stopping the service: a stopped backup started then
would fail after the outage had begun. Once the job finishes, start a fresh
deployment; its own warm-up then has little to copy. For the same reason,
preflight refuses while any backup job for the volume is active, such as the
09:00 UTC scheduled backup or an earlier warm-up. Compatible deployments take no
warm-up: they never wait for completion, and creating the snapshot point takes
seconds.

The command verifies the live previous image, private registry digests, complete
verification receipts, one healthy replica, the retained encrypted EBS claim and
a recent completed fallback backup. Before stopping, it protects the old and new
images, source, release receipts, configuration export and Duplo settings. Existing
image archives are reused and their retention extended, rather than uploaded again.

A failed backup job blocks deployments for 24 hours, even after a later success,
so a real fault is not hidden by one good run. To deploy past a job you have
reviewed, name it with `--accept-failed-backup JOB` and explain why with
`--accepted-failure-note`. Each named job must be a recent failure for this volume
with a later completed backup; every other failure still blocks, warm-ups never
count as that later backup, and `deployment.json` records the job and the note.
For migrations and unknown compatibility it then completes the warm-up described above.
It then stops once, verifies the actual snapshot, updates the application and
same-image sidecars, starts once and checks readiness. Readiness waits, within
the operator timeout, until one ready pod and public `/healthz` and `/readyz`
all succeed: the load balancer answers HTTP 503 itself until it marks the new
target healthy. At the deadline it reports the last status, keeping each
endpoint's last response. A compatible deployment
returns while AWS continues transferring the snapshot; AWS Backup records its
outcome and the existing daily console-only health monitor checks failed jobs.
The result prints service readiness and backup state separately.

If deployment tooling changes after an image passes verification, keep using that
image with its original `--release-dir`. The command accepts it only when the
committed runtime inputs still match, and records both the image's source revision
and the operator revision. Generated load-balancer DNS and cloud names, and omitted
empty optional service fields, do not count as configuration drift. Changes to
routing, certificates, mounts or environment still stop the deployment. After a
failed attempt resumes application writes, use
a fresh state directory and stopped backup rather than reusing the old capture.

Local deployment logs can contain service configuration and secrets; keep the
directory private. To diagnose a deployment, read `deployment.json` (the receipt and
its phase) first, then `failure-*.json` (the first error) and the `command-NNNN.json`
it names. Repeated identical successful responses are stored as small
`{"same_as": "command-NNNN.json"}` references; failures always keep full output. They retain the first command/HTTP failure, response body,
pod, events and application logs. Do not retry failures blindly. `--resume` can
finish startup/readiness after a successful image update without another stop.
Interrupted preparation, capture or uncertain image updates require inspection
of the actual service image and a fresh deployment attempt with fresh preflight
and capture; an earlier snapshot cannot cover writes after a restart. Do not
hand-edit receipts or roll back to an older image without checking data-format
compatibility. SIGINT/SIGTERM and ordinary errors attempt Duplo Start; power loss
or SIGKILL still requires an operator to inspect and recover the service.

## Deploy one release with one command

`deploy_release.py` runs the steps above for an instance with an owner and gateways,
from your machine or a CI runner. Storage v2 has no owner process: the first
storage v2 release moves with `deploy_release.py storage-cutover`
([storage v2 cutover of an owner and gateways](storage-cutover.md)), and an
ordinary deployment refuses it. Afterwards the owner service is the gateway that
keeps the Git exports, and releases deploy it and the gateways as before. It keeps release receipts and deployment records
in the recovery bucket, so the host carries nothing between runs.

```sh
# After release_image.py publishes: keep its receipts where any host can deploy them.
.venv/operator/bin/python deploy/production/deploy_release.py store \
  --owner-config /secure/owner.json --release-dir ~/.cache/forgetmenote/releases/REVISION

# Read-only: every check for both roles, with all problems reported together.
.venv/operator/bin/python deploy/production/deploy_release.py preflight \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REFERENCE \
  --compatibility compatible --compatibility-note 'Reviewed rollback assumptions.'

# Preflight, owner, gateways, then wait for the upgrade backup.
.venv/operator/bin/python deploy/production/deploy_release.py deploy \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REFERENCE \
  --compatibility compatible --compatibility-note 'Reviewed rollback assumptions.'
```

Two read-only queries serve a pipeline, each printing its answer alone on stdout.
`deployed --owner-config CONFIG` prints the live release and the runtime files the
checkout changed since it, as JSON. `prepared --owner-config CONFIG --repository REPOSITORY`
prints the pinned image published for the checkout's runtime, from HEAD or the newest
earlier commit with the same runtime files.

`REFERENCE` is the `Deploy this image` line `release_image.py` printed. Both configurations
must share one `state_root`. Each run first copies `deployments/` from the bucket into
it, then returns its records afterwards, even when a step fails. Image and source
archives stay out of those records; they already live in `images/` and `upgrades/`.
Run preflight before the release gate when a deployment is planned: it finds backup,
image and sign-in problems in seconds, and `--plan` contacts no service at all.

A run that stops does not retry. Read the records first, then choose:

```sh
# Finish this release from where it stopped: the owner, the gateways, the upgrade backup.
.venv/operator/bin/python deploy/production/deploy_release.py resume \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REFERENCE

# Start over, when resume refuses because the stopped attempt needs a new capture.
.venv/operator/bin/python deploy/production/deploy_release.py deploy --fresh-attempt \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REFERENCE \
  --compatibility compatible --compatibility-note 'Reviewed rollback assumptions.'

# Return this release's gateways to the image they ran before it.
.venv/operator/bin/python deploy/production/deploy_release.py rollback-gateways \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REFERENCE
```

`resume` keeps every refusal above: it continues an owner only after a successful image
update, and it stops at the first role that cannot continue. `--fresh-attempt` runs preflight
and takes a new capture, so check the actual service image first. `rollback-gateways` acts
only on the newest rollout and leaves the owner on its image. There is no owner rollback
command, because an older owner may not read newer data: check data-format compatibility
first, and treat going back as a new reviewed deployment.

A receipt accepts only the configuration that wrote it, so continue a release with the
configuration files that started it, whichever host you continue from.

### Sign-ins on a runner

Leave `aws_profile` and `kubeconfig` out of the configuration and the tools use the
host's sign-in: AWS credentials in the environment and the default kubeconfig, which
`aws eks update-kubeconfig` writes. With `DUPLO_TOKEN` set the tools call `duploctl` without
`--interactive`, which would discard the token and wait for a browser; without it they use
the browser login. A runner pulls the image to archive it, so sign Docker in to its
registry first: `aws ecr get-login-password` for ECR, or `docker login ghcr.io`. The tools
look ECR images up with the AWS sign-in. GHCR lookups use `gh`, which needs `GH_TOKEN`
with `read:packages`, also when the release being replaced is a GHCR image.
The checkout needs full history, since release comparison reads earlier commits.
None of this is recorded: tokens stay in the environment, out of every command record.

The lock that stops two deployments of one role is local to each host, so run one
deployment at a time across hosts. A workflow needs a `concurrency` group.

## Pull the private image from DuploCloud

An image in an ECR repository of the cluster's own account needs no pull credential:
the nodes' role reads it. A GHCR image needs a dedicated Docker registry credential in
the workload's tenant:

- Registry: `ghcr.io`
- Username: the GitHub account that owns the pull credential
- Password: a GitHub credential with `read:packages` and access to the private image
- Kubernetes secret name: `forgetmenote-ghcr`

Use a dedicated pull credential rather than copying your broader publishing
login into the cluster. GitHub documents a classic PAT with `read:packages` for
unattended registry access; an authorized GitHub CLI OAuth credential also works.
Keep it in DuploCloud/Kubernetes secrets, outside this repository and the image.

The secret must have type `kubernetes.io/dockerconfigjson` in
the workload’s namespace. Reference it from the ForgetMeNote service's pod template
using `imagePullSecrets: [{name: forgetmenote-ghcr}]`; `pilot.yaml` includes that
reference. Leave existing services' registry credentials intact. Paste the
publisher's digest reference into the new ForgetMeNote service's image field.
No GitHub source checkout or AWS registry keys are needed on the pod.

See [DuploCloud registry credentials](https://docs.duplocloud.com/docs/automation-platform/overview/aws-services/containers/docker-registry-credentials)
and [Kubernetes private image pulls](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/).

## Workload and runtime configuration

Use `pilot.yaml` as the workload contract. If Duplo manages the service, enter
those settings in Duplo instead of also applying a second Deployment controller.
Use one replica, Recreate updates, a dedicated encrypted EBS CSI volume supporting
ReadWriteOncePod, and the application's filesystem ownership locks. Do not attach
another application’s data volume or share a writable ForgetMeNote volume between instances.
The example requests 1 CPU / 2 GiB and limits 4 CPU / 8 GiB; measure repository
indexing and native OCR imports before setting final capacity.

Replace namespace, image, storage class and public host placeholders. `/readyz`
needs a Host header matching `PUBLIC_URL`; `/healthz` is the liveness check.
Create the operator-owned ConfigMaps and runtime Secret before starting:

- `forgetmenote-pilot-config`: selected JSON files from `deploy/integrations` plus
  existing chat/GitHub configuration. Start `connections.json` with only approved
  endpoints, or `{"version":1,"connections":[]}` for initial bootstrap.
- `forgetmenote-pilot-env`: optional environment variable names documented in
  `deploy/integrations/README.md`. Enable scheduled jobs only after service accounts
  and grants exist. Do not put secret values in this ConfigMap.
- `forgetmenote-pilot-runtime`: GitHub App key, Slack signing/bot tokens, model keys,
  OIDC/proxy secrets and external connection tokens as required. Mount files under
  `/run/secrets`; keep them outside `/data` notebook repositories.
- A least-privilege pod IAM role for Redshift. The AWS SDK uses runtime credentials
  and refreshes them; no static AWS keys are needed in the image or notebooks.

Bootstrap the organization's administrator with the existing offline
`forgetmenote admin bootstrap --username NAME --password-stdin` command against
the new volume **before** the server owns it. Use the existing organization UI or
management CLI/API to create users, groups, service accounts and credentials.
Configure existing OIDC or trusted Authelia proxy authentication for the web/API.
Employee MCP clients use `PUBLIC_URL/mcp` and ForgetMeNote OAuth. Remove any old
`MCP_ACCESS_CONFIG_FILE` setting before upgrading; the Cloudflare assertion adapter
and legacy aliases are no longer supported. Generic browser OIDC remains supported.
Expose only the intended private pilot hosts through the authenticated gateway;
the ClusterIP origin must not be reachable by untrusted peers. Provision the
appropriate ingress/network policy using the cluster's existing Duplo conventions.

Back up the **whole instance volume**, including catalog, OAuth/client identity,
chat and automation state, plus separately managed operator config/secrets. A
notebook Git bundle alone cannot restore the service. Take a quiesced encrypted
snapshot, restore it to a new isolated volume, and boot that restore with outgoing
jobs/Slack disabled. Retain the previous image digest and a compatible full snapshot
for rollback; never run an older catalog reader against newly upgraded state.
Background jobs write automation run format 5; interactive chat writes format 6.
The reader recovers formats 1–6. Adopting Administration-managed chat clients also
upgrades the catalog to format 7. Restore the compatible pre-upgrade snapshot when
rolling back to an older reader. On HTTPS, sessions survive a rollback only to a
release that reads `__Host-forgetmenote`. Releases at `2adf4d0` or earlier read
only `forgetmenote` and the retired `gitnotes` cookie, so rolling back to one of
them signs everyone out once.

## AWS durability

Use the [AWS recovery runbook](../../docs/aws-recovery.md) for versioned deployment
assets, protected secrets, daily EBS backups and an independent restore
drill. Notebook Git backups do not replace whole-instance recovery. On the
Postgres stack, also name `database_instance`, `blob_bucket` and the gateway
`deployments` in the recovery configuration, so the database and blob bucket are
backed up and checked with the owner's volume ([moving onto
Postgres](postgres-cutover.md)).

### Add credentials to the existing provider mount

When an authorized feature requires new keys in the existing instance secret,
put their names in the private deployment configuration's `provider_secret_keys`
list. For Cloudflare website capture:

```json
{"provider_secret_keys": ["CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_BROWSER_RUN_TOKEN"]}
```

Preflight verifies nonempty key presence without requesting credential values in
command logs. The stopped image update adds each key at the same-named path in
the existing `providers` mount, preserving its other explicit keys. Conflicting
paths or a different secret owner are rejected. No separate live rollout is
needed to mount the new files. The operator still owns the token/account values
and the `CHAT_CONFIG` credential-file references. Preserve the previous config
for rollback: older binaries reject the new optional `websiteCapture` field.

To change an instance's existing per-source import byte limit, pass
`--source-bytes BYTES`. The value must be a positive bounded integer. This updates
only `FMN_LIMIT_SOURCE_BYTES` and uses the normal backup, stopped update and
readiness sequence. When runtime code is unchanged, pass the current verified
release as `--release-dir`; rebuilding the image is unnecessary. Connector response
limits remain separate operator configuration. Repeated requests for the current
limit do not restart the service; resuming a deployment retains its recorded limit.
