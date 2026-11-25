# Production deployment

These tools deploy a ForgetMeNote instance as DuploCloud services with AWS Backup
checkpoints. Each instance keeps its settings, records and pipeline in its own
deployment repository, outside this one.

- [Releasing images](releasing.md): local and GitHub Actions image release, choosing verification coverage.
- [Deploying to Duplo](deploying.md): the stopped-checkpoint deployment command, pulling the private image, workload configuration and AWS durability.
- [Moving onto Postgres](postgres-cutover.md): the owner, gateway and helper services, rehearsing and running the cutover, and deploying the new stack afterwards. Storage v2 removes the owner process.
- [Storage v2 cutover of an owner and gateways](storage-cutover.md): the one-time move of that stack onto storage v2, whose owner service becomes the gateway that keeps the Git exports.
