# Storage v2 cutover of an owner and gateways

Audience: operators of a Duplo instance on the Postgres stack of
[moving onto Postgres](postgres-cutover.md): a **notebook owner** service that keeps
every notebook's Git repository on its own EBS volume, and load-balanced
**gateways** with no disk of their own. Storage v2 keeps every notebook in Postgres
and the blob store and has no owner process: the first storage v2 image stops
at `the owner command has been removed`, and its gateways refuse the owner map.
So that release cannot be deployed as an image update. `deploy_instance.py` and
`roll_gateways.py` refuse it, naming this runbook, and `storage_cutover.py` moves
the instance instead, once. It follows the single-host
[storage v2 cutover](../../docs/storage-cutover.md), whose commands, statuses and
refusals apply here unchanged.

## What changes

The owner's volume holds the repositories, and only a pod that mounts it can
import them. So the owner service becomes the **exporter**: a gateway of the new
release, with the gateways' settings, secrets and probes, on the owner's volume as
its data directory. It keeps the same service name, volume, claim and AWS Backup
plan, so the owner's deployment configuration describes it from then on. Like
every storage v2 gateway it keeps the notebooks' Git exports in
`DATA/exports/NOTEBOOK.git`, on that volume. It keeps the owner's load balancer
configuration, so the public origin still reaches only the gateways. The owner's backup sidecars run beside it on
the exports instead of the working trees: each sidecar that mounted
`projects/NOTEBOOK/workspace` mounts `DATA/exports` read-only at `/exports`
instead, and reads a storage v2 configuration of its own. The gateways drop the
owner map and otherwise stay as they are.

## Before the window

1. Publish the first storage v2 release as usual and keep its receipts
   (`deploy_release.py store`). A pipeline that deploys each release stops at its
   preflight with the refusal above, before anything is stopped: that is expected.
2. For each backup sidecar of the owner, add a key to the secret its
   configuration comes from, holding the same watcher configuration with
   `"repository": "/exports/NOTEBOOK.git"`; keep the old key until the cutover
   is done. Name each in the gateway configuration:
   ```json
   "storage_v2": {"sidecars": {"SIDECAR": {"key": "BACKUP_CONFIG_V2", "path": "backup-v2.json"}}}
   ```
   The export continues the same history, so its first push is up to date.
3. Run the preflight. It only reads, reports every problem together, and writes
   the derived services (`exporter-held.json`, `exporter-serving.json`,
   `gateway-v2.json`) beside its record for review:
   ```sh
   .venv/operator/bin/python deploy/production/deploy_release.py storage-cutover --preflight \
     --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REF
   ```
   It requires the owner to run `forgetmenote owner` and the gateways
   `forgetmenote migrate && exec forgetmenote serve` on the same release without
   `OUTGOING`, every gateway replica ready, each sidecar's new key in its secret
   and its last push synced, a recent completed backup of the owner's volume, the
   previous image archived in the recovery bucket, an encrypted private database
   with automated backups and deletion protection, a versioned blob bucket, and
   permission to run commands in pods. The candidate must be a storage v2 release
   whose image still matches its publication.
4. A rehearsal restores the owner volume's latest recovery point and a database
   snapshot into separate resources and runs the storage commands there. It copies
   personal content, so it needs its own approval; this tool does not run one.

## The window

```sh
.venv/operator/bin/python deploy/production/deploy_release.py storage-cutover \
  --owner-config /secure/owner.json --gateway-config /secure/gateways.json --image REF
```

The instance is down from the first step until the gateways serve again, mostly
waiting for the backups; on a small instance the storage commands take seconds.
Each phase records its outcome in `storage-cutover.json` before the next begins:

| Phase | What happens |
| --- | --- |
| `stopped` | The gateways stop, then the owner, and its volume detaches. |
| `captured` | An AWS Backup of the stopped volume (kept 90 days) completes and its snapshot is tagged application-consistent; a manual RDS snapshot `DATABASE-pre-storage-v2-ID` becomes available. |
| `held` | The exporter starts on the new image without a server or sidecars; `forgetmenote migrate` runs, and the notebook repositories on the volume are listed. |
| `imported`, `verified`, `exported` | `admin storage import`, `verify` and `export`: every listed notebook must succeed. |
| `checked` | `admin storage check` must pass, and `retire` without `-yes` must find every notebook's files removable, while the previous release can still return. |
| `serving` | The exporter and the gateways serve the new release with `OUTGOING=paused`; the public origin answers `healthz` and `readyz`, and each backup sidecar reports its export pushed. From here people can save. |
| `finished` | `migrate -finish-rollout` makes the previous release refuse the database. |
| `retired` | A second short stop: `admin storage retire -yes` removes the previous release's files, as the single-host runbook explains, and both services serve again, still paused. |
| `complete` | Outgoing actions resume. The record becomes the instance's live release for the next deployment. |

Each storage command's output is in the `command-*.json` record the failure
names; [the single-host runbook](../../docs/storage-cutover.md) explains every
status and refusal. After a failure the instance stays as it stopped: read the
record, fix the cause, then continue from the interrupted phase with the same
command and `--resume`. Never repeat a failed step by hand. A failed retire is the
exception: the new release serves again, still paused, and `--resume` stops it
once more to retire.

## Rollback

Until `-finish-rollout`, `--rollback` stops both services and restores their
recorded definitions on the previous image. The previous release serves its
untouched repositories and ignores what storage v2 stored. Once the new release
has served (`served_from` in the record), it may hold saves that a rollback
discards; pass `--discard-v2-writes` only after checking that nothing was saved.
The imported notebooks stay in the database. A later cutover verifies them again,
but refuses each notebook the previous release changed meanwhile: avoid serving
the previous release after a rollback unless you will restore the database first.

After `-finish-rollout` the previous release refuses the database. Returning to it
means restoring the RDS snapshot to the instance's database and the owner volume
from the cutover's recovery point, then restoring both service definitions from
`owner-before.json` and `gateway-before.json`. Anything saved on storage v2 is in
the exports of the volume you replace; keep that volume.

## Afterwards

Routine releases deploy as before: `deploy_instance.py` updates the exporter, with
its sidecars, and `roll_gateways.py` rolls the gateways. Then remove what storage
v2 ignores: the owner token secret and its mounts, the `OWNER_*` variables, the
owner map, and the cutover helper service; and, once each sidecar has pushed from
its export, its old configuration key. Keep the backup, the RDS snapshot and the
recovery bucket records for as long as you might restore them.
