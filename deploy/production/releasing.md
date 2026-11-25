# Releasing images

Build, verify and publish a committed image. Deployment is separate: a single host
follows [deploying ForgetMeNote](../../docs/deployment.md), and a hosted instance
deploys with its own deployment repository's tools.

## Local image release

Keep the source repository private. `release_image.py` publishes a private image
at `ghcr.io/OWNER/REPOSITORY` using your existing GitHub CLI login, or to a private
ECR repository with your AWS sign-in. It builds
`linux/amd64`, runs the [selected resumable gate](../../docs/verification.md)
with bounded concurrency, then pushes `main-COMMIT` and prints the verified digest to deploy.
It does not deploy a workload or change an existing service.

Requirements: a clean, committed `main` checkout, Go matching `go.mod`, Node 24,
Python 3.12+, Git, GitHub CLI, Make, OpenSSL, and a running Docker engine with
Buildx and Compose. Apple Silicon is supported through Docker's amd64 emulation;
the image build and smoke tests can be slower there. Leave localhost port 19443
available for the disposable authentication smoke tests. On Linux, install
Playwright's system dependencies as described in the [release gate](../../docs/production-readiness.md#release-gate).

One-time setup, from the repository root:

```sh
# The pinned operator Python in .venv/operator (deploy/requirements.txt).
make operator-venv

# Sign in with gh auth login if needed, then extend the existing login.
gh auth refresh --hostname github.com --scopes write:packages,read:packages
```

No separate personal access token or AWS credentials are needed for local
publishing. Package permissions are distinct from repository ownership.

```sh
# Inspect the plan without contacting GitHub, building or publishing.
.venv/operator/bin/python deploy/production/release_image.py --dry-run

# Verify a committed candidate without publishing.
.venv/operator/bin/python deploy/production/release_image.py --since DEPLOYED_REVISION --verify-only

# Publish, reusing successful checks and the exact verified image.
.venv/operator/bin/python deploy/production/release_image.py --since DEPLOYED_REVISION
```

Before creating a candidate, compare against the source revision in the **deployed**
`publication.json`:

```sh
.venv/operator/bin/python deploy/production/release_image.py --since DEPLOYED_REVISION --dry-run
```

`--since` skips the image build and publication for known documentation, test and
operator-only changes. Run the affected tests separately. It checks production
Go embedding declarations; embedded content and unknown paths still require a
release. When publishing, pass the same `--since DEPLOYED_REVISION` without
`--dry-run`. Do not use a merely built or published revision as the deployed baseline.

An interrupted command resumes from `~/.cache/forgetmenote/releases` (or
`$XDG_CACHE_HOME/forgetmenote/releases`). `--state-dir` selects another private
location. Keep that directory and the local Docker image until publication is
complete. Each attempt retains raw logs, source revision, input fingerprints,
toolchain versions, timings and the immutable image ID. `publication.json` adds
the verified registry digest. Credentials never enter this directory.

A failed check blocks resumption until investigated. `--retry-failed` explicitly
reruns failed/incomplete work and preserves the original evidence; unchanged
successful checks are reused. Changing source, tooling or image inputs invalidates
affected checks. `--jobs 1` serializes work on constrained machines; the default
uses at most six host slots, two browser workers and one Docker scenario at a time.
Do not run separate releases against the same Docker engine/HTTPS port using
different state directories. There are no automatic test retries.

Each check has a time limit: 10 minutes for the backup tests, 20 for a smoke and
an hour for anything else. A check past its limit is stopped, including its
containers, and fails like any other, so it needs `--retry-failed` after you find
out why. The backup check's package download has its own limit and three attempts,
because a stalled download is not a test result.

Race-test results are reused per Go package and its full test/build dependency
closure, including embedded assets and runtime fixtures. Shared application
changes still run all browser cases; test-only Go edits can reuse the browser
result. Host checks start longest-first using recorded durations, and measured
web shards keep their membership on resume. Routine releases run the core and
affected container scenarios. Automatic full releases run every browser case, the
backup tests and the quick
scenarios (core, headless, resource, chat, cluster). They add source, jev, oidc and
totp only when the change touches what those exercise, or the image and its
dependencies. An explicit `--mode full` runs all nine. The operator Python tests
run beside the matrix rather than before it. See [verification](../../docs/verification.md)
for invalidation rules, fixture ownership and measured bottlenecks.

Use `--image ghcr.io/OWNER/NAME` to override the destination. The default derives
from the GitHub `origin` remote. `--image ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME`
publishes to ECR instead, signing in with `aws ecr get-login-password` and the host's
AWS sign-in; it needs no GitHub access. The repository must already exist: create it
with immutable tags. GitHub creates new container packages as private;
the publisher refuses an existing public package and checks visibility again
before and after pushing. It does not change package visibility or permissions.
The OCI source label associates the image with its source repository.

The build uses an isolated archive of committed files, excluding ignored local
configuration. The checks use that same snapshot. Linux-only backup tests run in
a disposable container with `inotify-tools`; the published image is not modified.
Python operator dependencies must already be installed in the selected Python
environment. Node dependencies and the suite's browsers, Chromium and WebKit,
are installed from the checked-in versions during verification; WebKit's system
libraries are not, so run `make setup-browser` (with root) once on a new gate
host. Authentication goes through stdin to a temporary
Docker configuration that is removed on completion or failure, preserving your
existing Docker login and context. An already-published commit returns its
existing private digest without rebuilding or replacing the image.

Deploy the printed `REPOSITORY@sha256:...` reference. Registry tags can be
changed outside this script; the digest identifies the exact checked image.
The script does not fetch, merge or push Git commits; update and commit `main`
before running it.

## GitHub Actions release

An instance's own deployment repository runs its release pipeline from a checkout of
this repository. Its tools read the instance's live revision and stop when no runtime
file changed since. If an earlier run already published an image for that runtime,
it deploys that image without verifying again. Otherwise it verifies with `--since`
that revision before any cloud sign-in, publishes reusing those checks and deploys.
A live revision this history lacks, such as one built from a repository this one
replaced, counts every runtime file as changed, so that release verifies completely.

[Notify deployments](../../.github/workflows/notify-deployments.yml) starts that
pipeline on each push to `main` that changes more than documents, once the
`DEPLOY_REPOSITORY` and `DEPLOY_DISPATCH_TOKEN` secrets of the `deployments`
environment are set. The target is a secret so that public logs mask it.

### Branches

Pull requests merge into `next`, the default branch, by squash, so several changes
can wait there and release together. A release is a pull request from `next` into
`main`, merged with a merge commit. `main` then holds each change once, `next`
stays an ancestor of `main`, and the push starts the pipeline above. Never squash
or rebase that pull request, and never delete `next`. A fix that cannot wait may
go straight to `main`; merge `main` back into `next` afterwards. Both branches
require the `fast` check.

## Choosing verification coverage

Pass `--since DEPLOYED_REVISION` to both verification and publication. Automatic
mode selects complete affected Go modules and importers, critical web callers plus
feature boundaries, critical browser journeys plus feature suites, and core plus
affected containers. Multiple known features select their combined coverage.
Shared infrastructure, security, storage, authority, dependency and unknown changes
escalate to full regression. `--mode routine` cannot override that decision.

Use `--mode full`, or a manual GitHub Check, for a complete regression on demand. No
baseline also selects full. Selection, toolchain, source and exact
image are recorded; full and compact results are distinct. See
[verification](../../docs/verification.md) for the coverage table and resumption.
An already verified runtime image can be deployed after operator-only changes
without rebuilding it; retain its original release directory and source tag.


Operator-only updates do not require a new application image. To finish a reviewed
runtime candidate after updating release selection, use
`release_image.py --revision VERIFIED_RUNTIME_COMMIT --since DEPLOYED_REVISION`.
The tool rejects nonancestor candidates, current runtime differences and newer
Go/browser/smoke regression inputs. It reuses
the original image identity and matching receipts, and records the operator
revision. Scoped retries retain every investigated failed browser case and its
original evidence; they cannot skip other failed checks.
