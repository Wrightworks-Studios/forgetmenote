# AWS instance recovery

The container is replaceable. Its whole `/data` volume and protected deployment
assets are not. Keep the image digest, mounts, public URL, resource limits and
provider configuration paths in deployment configuration. Keep credentials out
of the image and source repository. Administration settings, users, permissions
and notebook connections live in the catalog on `/data`; environment variables
should not become a second owner for those settings.

`deploy/production/durability.py` renders a CloudFormation stack and narrowly
scoped Kubernetes RBAC. It needs standard-library Python and the existing AWS
CLI. AWS Backup owns snapshots. A read-only Lambda exports deployment
configuration and checks backup health using AWS's bundled SDK and its own IAM
identity. It cannot scale workloads or start, stop or delete backups. Duplo
continues to own the running service.

## Protection and schedules

- Retain the dedicated encrypted EBS volume. Set the **PV** reclaim policy to
  `Retain` as well as StatefulSet PVC retention; these are separate controls.
- One daily snapshot at 09:00 UTC, retained 14 days in the same AWS region.
  There are no scheduled hourly, monthly, or cross-region copies.
- These online snapshots are **crash-consistent**, like recovering after sudden
  power loss. They include all files and Git history, but they do not promise a
  clean application checkpoint across files. Use a stopped backup before upgrades.
- Daily at 12:15 UTC, export the StatefulSet, PVC/PV, Service, configured Ingresses
  and all referenced Secrets/ConfigMaps to encrypted S3 in that region. Objects
  include the image digest and have 14-day governance retention. Run objects
  expire after 35 days; images and bootstrap assets have no automatic expiry.
- Vault Lock and S3 Object Lock use **governance** mode. Privileged administrators
  can remove/bypass these locks. A separate backup account is the stronger
  boundary against complete account compromise; this stack does not create one.
- Enable stack termination protection. Keys, buckets and vaults have retain
  policies. Do not schedule KMS key deletion while backups depend on them.

Configuration exports and scheduled snapshots are independent. For recovery,
choose configuration captured with the matching image and deployment, rather
than assuming an arbitrary export belongs to a particular snapshot.
Stopped upgrade backups should record their configuration key explicitly.

## Provisioning

Copy [the example](../deploy/production/durability.example.json) into a private
operator directory and fill in actual names. This deployment expects one EBS
CSI volume, one StatefulSet replica, and EKS `API` or `API_AND_CONFIG_MAP`
authentication. `name` names the stack's vault, key, bucket, roles and alarms;
`workload` names the StatefulSet to export and defaults to `name`. Never change
`name` on a deployed stack; set `workload` when the StatefulSet changes, as it
does after a [move onto Postgres](../deploy/production/postgres-cutover.md). RBAC must list every referenced secret/configmap. Newly added
references without access cause an export failure. Projected secret/configmap
references are included; Kubernetes regenerates projected service-account tokens
and downward API files from the saved pod specification. Other projected sources
and additional persistent volumes require explicit coverage. If replacing the PVC/volume,
update both the backup selection and worker configuration before relying on it.

```sh
python3 deploy/production/durability.py --config /secure/instance.json --output /secure/rendered
aws --profile ADMIN --region REGION cloudformation create-stack \
  --stack-name forgetmenote-durability \
  --template-body file:///secure/rendered/primary.json \
  --capabilities CAPABILITY_NAMED_IAM --enable-termination-protection
```

Apply `rbac.json` with an existing cluster administrator. Secret reads are
restricted by name; all Kubernetes access is read-only. AWS Backup uses separate
AWS-maintained backup/restore service roles.

The export schedule defaults to `ExportState=DISABLED`; the daily snapshot rule
starts immediately. Invoke `<instance>-backup` Lambda once with a unique event
`id`. Check its response, S3 objects, and AWS Backup results. A successful
invocation can report `healthy: false` when a backup is missing, stale, or a recent
job failed. Perform the restore drill, then set `ExportState=ENABLED`. Preserve
that parameter on later updates.

Protect the Duplo service export, templates, instance configuration, stack
outputs and recovery receipts under `bootstrap/` in the bucket. Refresh the
Duplo export whenever service configuration changes. Import/adapt the saved
configuration through Duplo during recovery; do not create a competing
controller over the production PVC.

Archive each deployed image under `images/` with its immutable registry digest,
source revision, Docker archive tag and SHA-256 receipt. Use `docker image save`
and KMS-encrypted uploads. Retain images as long as corresponding backups. A
verified archive can be loaded and pushed to another registry if the original registry is lost.

## Stopped backups before upgrades

Use the **Duplo service Stop/Start controls**, not `kubectl scale`: Duplo can
reconcile a direct Kubernetes pause back to its saved replica count. Do not
change organization-wide permissions or store a personal Duplo token in Lambda
just to automate a maintenance window.

Use the tested [Duplo deployment command](../deploy/production/deploying.md#deploy-through-duplo-with-one-stopped-checkpoint)
for application releases. If committed runtime inputs have not changed since
the deployed revision, run focused operator/test checks and leave the service,
image and snapshots alone.

1. While the service is online, verify the published candidate's exact revision
   and digest, one ready replica, the retained encrypted volume, a completed
   fallback backup created within 30 hours, no recent backup failures and no
   backup job for the volume still active. Warm-ups (step 2) count neither as
   the fallback nor as failures. Protect both the previous and candidate images,
   source and verification receipts. Invoke the configuration-export Lambda and
   save its configuration key together with the previous image and Duplo
   service settings.
2. For **migrations or unknown compatibility**, while the service is still
   online, start a **warm-up** AWS Backup job of the same volume in the same
   vault, with the same role, 7-day retention (the vault lock's minimum) and
   tags `Purpose=release-warmup` and `Consistency=crash`. Wait for it to finish.
   The warm-up is never a restore point. It only gives the stopped snapshot a
   recent base, so that snapshot copies only the blocks written after it. If
   the request is rejected or the job fails, continue without it. If it is
   still running, do not stop the service: AWS Backup runs one backup job per
   resource, so the stopped job would fail.
3. Stop the service through Duplo; wait for its pod to disappear and its specific
   EBS volume to be `available` with no attachments. Record the quiesced time.
4. Start an AWS Backup EBS job in the instance vault using the backup service
   role, 14-day retention, and `Consistency=unverified`. Record its job ID and
   configuration key. Prevent concurrent deployments during this operation.
5. Poll the job for a recovery-point ARN, then query that **actual EBS snapshot**.
   Its volume ID must match, it must be encrypted, and its start time must be
   after the quiesced time. Only `pending` or `completed` establishes that the
   immutable snapshot point exists. Job acceptance alone is insufficient.
6. For **reviewed backward-compatible** updates, update to the candidate image
   immediately after capture, then start that image once. AWS continues the
   transfer independently. For **migrations or unknown compatibility**, wait for
   backup completion before changing the image. Both modes always attempt Duplo
   Start on errors, retain the original failure, and verify the new application
   and sidecars plus HTTP health/readiness. Do not restart the old version merely
   to wait for transfer and then stop a second time for rollout.
7. Track backup completion separately from service readiness. A pending snapshot
   is not a completed/restorable backup: the compatible path retains the recent
   completed fallback during this interval. AWS Backup records the final state;
   the existing daily console-only monitor includes upgrade jobs. Use the
   deployment command's `--status` to save the final protected result. Only a completed job whose snapshot was
   observed during the stopped interval can be tagged `Consistency=application`
   using EC2 `create-tags` on the snapshot ID. AWS Backup `tag-resource` does not
   accept EBS snapshot ARNs. Protect the receipt in the bucket. A failed job is
   not a backup; retain its original diagnostics.

Recovery evidence lives under `upgrades/RUN_ID/` in the existing protected bucket.
The stopped capture points to the **pre-upgrade** configuration and previous image.
The deployment receipt also references the candidate source/archive and desired
Duplo service configuration. Use those associations when restoring; do not assume
the most recently uploaded configuration belongs to a snapshot. Ordinary errors
attempt to start the current configured image; they do not automatically restore
data or downgrade across a possible storage migration.

This flow depends on observing the actual point-in-time EBS snapshot while the
volume is detached. AWS permits using a volume while that snapshot is `pending`;
later writes do not alter its captured contents. The asynchronous transfer can
still fail, which is why completion and the older fallback remain explicit.
See [AWS snapshot guidance](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-creating-snapshot.html).

The warm-up relies on documented incremental behavior:

- **EBS:** after a volume's first snapshot, each later snapshot holds only the
  blocks changed since the previous one. A snapshot is full instead when the
  earlier snapshot came from another account or used a different KMS key
  ([how EBS snapshots work](https://docs.aws.amazon.com/ebs/latest/userguide/how_snapshots_work.html)).
- **AWS Backup:** it does not fully manage EBS. An EBS backup is therefore
  incremental whenever an earlier backup of the resource exists in the same
  Region, whatever vault holds it
  ([incremental backups](https://docs.aws.amazon.com/aws-backup/latest/devguide/creating-a-backup.html#incremental-backup-works)).
- **Encryption:** EBS backups keep the source volume's key
  ([backup encryption](https://docs.aws.amazon.com/aws-backup/latest/devguide/encryption.html)).

The warm-up therefore uses an AWS Backup job like the stopped backup, rather
than EC2 `create-snapshot`. For that path, both guides document the stopped
snapshot as incremental over the warm-up. The AWS Backup guide does not say
whether a snapshot created outside AWS Backup counts as an earlier backup. A
plain EC2 snapshot would also sit outside the vault's lifecycle and lock, would
need its own deletion, and would need EC2 snapshot permissions this tooling
does not otherwise use. Two other AWS Backup limits shape the warm-up. AWS
Backup allows one concurrent backup job per resource
([quotas](https://docs.aws.amazon.com/aws-backup/latest/devguide/aws-backup-limits.html)).
The vault lock fails jobs whose retention is shorter than its minimum
([Vault Lock](https://docs.aws.amazon.com/aws-backup/latest/devguide/vault-lock.html)).

## Independent restore drill

1. Choose a completed recovery point and suitable protected configuration. Use
   a stopped upgrade backup or a scheduled backup, never a
   `Purpose=release-warmup` warm-up. Keep
   downloaded Secret JSON private: its values are base64, not separately
   encrypted once downloaded. Never print or commit it.
2. Use `get-recovery-point-restore-metadata` and `start-restore-job` with the
   restore role, EBS type, target availability zone and encrypted gp3 settings.
   Record the **new** volume ID; never overwrite the production volume.
3. Mount the new volume read-only in an idle helper pod. Use the EBS CSI driver's
   `ReadWriteOnce` access mode with read-only mount flags; `ReadOnlyMany` is not
   supported. Copy all files, including dotfiles, to a disposable Docker volume.
   For large transfers, a checksummed, KMS-encrypted S3 tar archive avoids a long
   Kubernetes exec stream. Compare content hashes
   before starting the application and run Git integrity checks. Plain Markdown
   and Git history must remain readable without ForgetMeNote.
4. Boot the exact archived image against that writable copy using Docker
   `--network none`, no AI/data-provider credentials or external provider settings,
   and a local-only `PUBLIC_URL`. Network isolation prevents restored background
   jobs from contacting production integrations.
5. On the stopped copy, issue a one-day `notes:read` API credential for an existing
   account using `forgetmenote admin credential`. Verify health, the notebook
   catalog, editable-notebook reads, unauthenticated rejection and rejection of
   writes with that read-only credential through container loopback. Verify
   repository snapshots directly with Git. Repository API reads require live
   GitHub installation authorization, even with the GitHub configuration restored;
   network-isolated reads return unavailable. Record this limitation rather than
   bypassing permission checks. GitHub API and Google/OIDC need separate live
   integration checks after restoring their configuration and network access.
6. Remove only disposable restore containers, volumes, pods and claims after
   checking every ID against production. Retain the encrypted backups and proof.

On the Postgres stack the state is in three places: notebook repositories on the
owner's volume, the control plane in the database, and source job, export and
repository source files in the blob bucket. The same plan backs up all three, but
not in one instant. Restore the owner's volume from its recovery point, the
database to the same time with RDS point-in-time restore (or its AWS Backup
recovery point), and the bucket from its recovery point into a new bucket; then
boot an owner and one gateway against the copies, isolated as above, with their
own owner map and `PUBLIC_URL`, before trusting the restore.

Storage v2 moves the notebooks themselves into the database and the bucket and
has no owner process, so after that cutover the database and the bucket hold
every notebook (revisions and content blocks), and the volume holds the Git
exports and caches. Restore the database and the bucket to the same point: a
file version whose blocks are missing cannot be read. This stack's tools still
describe the owner layout; they are revised with its storage v2 cutover
([storage v2 cutover](storage-cutover.md)).

Recovery also needs the cluster, networking, IAM, certificates and DNS. This
stack does not duplicate the organization's infrastructure or protect against
loss of the entire AWS region. Recreate the protected secrets and startup
configuration, attach a replacement data volume
through Duplo, and verify integrations before changing DNS or enabling external
jobs. Restoring secrets does not reverse credential revocation.

## Monitoring and verification

Alarms are **AWS console only**: no SNS recipients or other actions. One alarm
covers export Lambda errors. The freshness alarm covers missing exports for
36 hours, no completed primary backup created within 30 hours, or
failed/expired/aborted jobs in the last 24 hours. The daily receipt records the
reason. A later success does not hide an earlier failure; inspect its original
job before rerunning.

```sh
python3 -m unittest discover -s deploy/production -p test_durability.py
```

Unit tests do not prove live AWS permissions or recovery. Keep completed backup
and restore job IDs, the image digest and a restore verification receipt.
Deployment-only changes do not require rebuilding an unchanged application image
or rerunning its already successful release gate.
