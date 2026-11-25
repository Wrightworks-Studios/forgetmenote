# Verification and release receipts

Use focused tests while changing code, then one selected release gate per committed
runtime candidate. Documentation, test and operator-only changes require focused
checks but no new application image or production backup/restart. Compare with
the actually deployed revision using `release_image.py --since REVISION --dry-run`;
unknown paths and production embedded content still require a runtime candidate.
This classification never marks a changed test as passed.
Do not restart already successful checks for unchanged inputs because
an agent session or terminal was interrupted.

## Development loop

The browser suite includes a theme and accessibility sweep using pinned
`@axe-core/playwright` 4.13.0 and its locked axe-core dependency. This is
development tooling, excluded from production bundles. It scans library, note,
search options, and settings in light, dark, and parchment at phone and desktop
widths using WCAG A/AA rules, retains the reports and screenshots, and separately
checks focus, history, system-theme changes, and horizontal overflow. Passing
automated checks does not replace a screen-reader walkthrough. See the
[upstream Playwright integration](https://github.com/dequelabs/axe-core-npm/blob/develop/packages/playwright/README.md).

Browser tests are formatted with pinned Prettier 3.5.3 (development tooling,
excluded from production bundles) so each statement sits on its own line and
searches, diffs and edits stay small. `make format-browser` checks them in
`check-fast` and before the browser suite; run `npx --no-install prettier --write
scripts/browser` after editing tests.

The suite runs in Playwright's Chromium, and `editor-caret-webkit.spec.mjs` runs
the caret cases again in its WebKit, Safari's engine. `make setup-browser`
installs both browsers with their system libraries (through the system's package
manager, so with root). The release gate installs both browsers but not their
libraries, so run `make setup-browser` once on a new gate host.

On Linux the browser suite needs a C compiler (`cc`; preinstalled on GitHub's
Ubuntu runners). Setup compiles `scripts/browser/no-netlink.c` and preloads it
into Chromium only. Chromium otherwise treats every host interface change as a
network change and fails pending requests with `net::ERR_NETWORK_CHANGED`;
release verification starts Docker containers beside the suite, and their
interfaces made fixture pages lose module scripts and never initialise.

Parallel browser invocations must use distinct `FMN_BROWSER_RESULTS` directories
and `FMN_BROWSER_REPORT` paths. Playwright clears its output directory at startup;
sharing it can remove another invocation's live trace. Cases within an invocation
retain the fixture isolation and concurrency rules below.

Read the affected callers/tests, reproduce a defect with the smallest behavioral
test, change the owner, then run related regression cases. Examples:

```sh
# Build frontend assets only when their inputs changed.
npm run build

go test -race -count=1 ./internal/web -run '^TestRelevantBehavior$'
npx --no-install playwright test scripts/browser/editor-drafts.spec.mjs --grep 'historical rollback'
node --test scripts/browser-fixture.test.mjs
python -m unittest discover -s deploy/production
```

Keep the selected work under five minutes where practical. Focused development checks do not substitute for the selected release gate.
Never use automatic retries or relax timeouts/assertions to turn a failure green.

Before optimizing, retain Go JSON (`go test -race -json -count=1 ...`) and the
Playwright JSON report. Use CPU/block profiles on the slow package when wall time
alone does not explain the bottleneck. Browser reports include each case's
elapsed time and setup/teardown in total duration. Compare with the same host,
cache state and worker limits, and report competing workload.

## Routine releases and full regression

```sh
# Use the source revision actually deployed, not an arbitrary recent commit.
python deploy/production/release_image.py --since DEPLOYED_REVISION --dry-run
python deploy/production/release_image.py --since DEPLOYED_REVISION --verify-only
# When publication is authorized, reuse the same baseline, state and checked image:
python deploy/production/release_image.py --since DEPLOYED_REVISION

# Complete regression: before the first release each week, or explicitly anytime.
python deploy/production/release_image.py --mode full --verify-only
```

Automatic mode selects coverage from the changed behavior against the deployed
revision. A routine release runs the complete affected Go package suites and
importers; the large web package uses critical authorization/save boundaries,
reviewed feature callers and every compiled case from changed web test files.
It runs critical browser journeys plus affected feature suites, core container
startup and affected container boundaries. Multiple known features select the
union of their coverage. `verification.json` records selected and omitted web
cases; omitted cases are never reported as passed.

Missing/nonancestor baselines, unknown paths, shared fixtures, security, storage,
authority and dependency changes select full regression. Full mode retains all
Go race tests, every browser case, all nine container boundaries (the ninth, `smoke-cluster`, runs two gateways on Postgres and an S3 stand-in, with no owner), the source and
decision matrices, and backup-worker tests. It runs only on explicit request, locally
or as a manual GitHub Check; publishing a routine feature change does not force full
regression.
Format, vet, frontend/fixture and operator tests retain their exact input receipts.
Unchanged successful checks are reused without rerunning them.

An investigated full-browser failure may be retried with `--mode auto
--retry-failed` only if every original failed browser case remains selected in the
scoped suite. The runner records those retained failures and keeps original logs,
reports and traces. Other failed checks, missing reports and infrastructure errors
still block narrowing. A subsequent scoped pass does not claim a complete-suite
pass or establish that an intermittent failure is fixed.

The compact checks are subsets of the existing scripts with unchanged assertions.
`SMOKE_PROFILE=full` remains their standalone default. The full suites are retained;
a routine receipt never claims the omitted cases passed. New tests or changed
fixtures invalidate their corresponding receipts. Shared browser fixtures force
full selection. Update reviewed feature mappings with new feature boundaries;
unknown paths fail closed. The operator regression suite verifies that the
critical browser titles still exist; Playwright's zero-test detection stays on.

Formatting, vet, frontend/operator tests and the MCP route/tool inventory run
before the lengthy matrix. This catches broken routes and release tooling without
waiting behind hundreds of browser cases. The image build is a required check
that starts in the Docker lane beside them, from a separate copy of the archived
source; container checks are defined from its verified image ID and queue behind
it. A preflight failure still stops every other check from starting. The
Dockerfile compiles the frontend and the `CGO_ENABLED=0` Go binary natively for
the target platform; only the runtime stages emulate linux/amd64 on other hosts. All other Go coverage still runs under
`-race`, including examples and fuzz seeds. The web package runs in separate
process batches: measured suites target roughly 90 seconds per batch, bounded to
4–16 batches. This is a scheduling estimate, never a timeout or a skipped-test
budget. Membership freezes for each candidate. Existing candidate membership is
preserved on resume. Tests modifying process-wide transports/environment never
run concurrently inside one process. Each other Go package has its own receipt;
a package whose last measured run exceeded 150 seconds runs as 2–8 frozen
process batches the same way, one receipt per batch. Whether a package runs
batched also freezes for each candidate, so the candidate's own measured runs
never replan the checks its publication reuses.

`--jobs` bounds host slots (1–12, default the host CPUs minus four, capped at
ten). Browser verification uses one slot per worker, a third of the slots and at
most four workers. Host checks start longest first by their last measured run;
without one, as on a fresh GitHub runner, the browser suite starts first. Docker checks stay
in one lane because they share memory and OIDC's HTTPS port. No extra runners or
paid infrastructure are required. Browser suites continue to reset only stopped,
disposable fixture data; real restart/history cases retain their written data.

## Evidence and resumption

The runner archives committed source and checks image revision, repository and
linux/amd64 architecture. State defaults to `~/.cache/forgetmenote/releases`
(or `$XDG_CACHE_HOME/forgetmenote/releases`); `--state-dir` selects another private,
locked directory. It retains per-attempt raw logs, browser traces, immutable image
identity and `REVISION/verification.json`. `publication.json` adds the private
registry digest. A successful check retains its original source attribution when
reused. Never rebuild an unchanged verified image merely because a session ended.

Go receipts bind production/test import closures from `go list -race -deps -test
-json`, embedded files, package testdata, explicit external fixtures, command,
environment and relevant toolchain. Register external runtime fixtures in
`go_package_inputs`. Missing inventory, untracked local replacements and Go
`-overlay`/`-modfile` fail closed. Opt-in live service evaluations always rerun their
package; connection settings are hashed, never saved as plaintext.

Browser receipts conservatively hash all application/embedded assets and browser
fixtures, plus the selected grep and worker count. Test-only Go changes can reuse
browser results. Container receipts bind the exact image ID and selected profile;
compact results cannot satisfy full checks. Changes to test-command execution invalidate
checks; selection-policy changes affect the plan and actual selected commands,
not unrelated race-test proofs. Make commands also bind their Makefile. The operator
test receipt binds what those tests read: release tooling and smoke scripts, browser
titles, web test files, configuration templates, the Dockerfile, `.dockerignore`,
workflows, source fixtures and the list of committed paths. Vet and format receipts
include the Go test tools under `scripts/`. No receipt
is edited or promoted by hand. Toolchain fingerprints are scoped to each check.

Operator-only edits need their focused tests but no image, snapshot or restart.
When they follow an already verified application candidate, publish that original
candidate with its original archived check policy and state. New attempt receipts
record the current operator revision; reused receipts retain their original attribution. Use `--revision VERIFIED_RUNTIME_COMMIT` from the current operator checkout only
when only documentation/operator inputs changed and its runtime inputs are identical
to that ancestor commit. Newer Go, browser and smoke regression inputs cannot be
skipped through this option. Then deploy with the current
operator checkout and the original `--release-dir`: deployment verifies that all
runtime inputs still match, recording both revisions. Do not manufacture a new
application candidate solely for a verification script or documentation edit.

One table classifies every path for PR feedback, releases and deployment:
`path_class` in `deploy/production/verification_plan.py` names image inputs
(`IMAGE_SOURCES`, which the Dockerfile copies as an allowlist), tests and fixtures
(which `.dockerignore` drops from the image), documents (exactly
`notify-deployments.yml`'s `paths-ignore`) and release tooling. Committed Go
embedding adds runtime inputs per commit. A path no table names, or Go
implementation outside the image sources, counts as a runtime input until it is
reviewed. Operator tests check the table against those three files and require
every committed path to have a reviewed class.

A normal resume reuses successes, runs interrupted work and stops on a previous
failure. Investigate its original HTTP status/body, application logs and retained
trace before an explicit `--retry-failed`. There are no automatic retries or
weakened assertions. A later pass alone does not establish that an intermittent
bug is fixed. Keep synthetic credentials/data and browser traces private.

## Performance targets and known bottlenecks

Targets are focused verification under five minutes and a complete warm-cache
release gate within 10–15 minutes. Cold compiler, dependency, browser, OCR model
or base-image downloads must be reported separately. Do not remove race,
persistence, restoration, authorization or concurrency checks to meet a deadline.
Record actual wall time, resource limits, original failures and bottlenecks from
the candidate's private receipts. Host concurrency is bounded with `--jobs`;
Docker scenarios remain sequential. Fixed production waits are covered once at
their owner: Go tests drive the organizer's two-minute quiet period with explicit
times, and browser cases that only need an idle live poll ask for a short hold.
Resumption evidence is distinct from a fresh uninterrupted
run.

## Companion onboarding verification

The companion generator is the `forgetmenote/plugins` Go package, including its
explicit embedded assets. Its Go tests run the Python standard-library installer
in disposable homes, so installer/version/allowlist changes invalidate the package
receipt and importers. Browser/fixture/vet/format fingerprints also include the new
`plugins/` runtime owner; a CLI regression proves a skill edit invalidates those
receipts. Formatting includes `plugins/`. Browser coverage lives in
`scripts/browser/connect-ai.spec.mjs`; it uses non-admin synthetic accounts and
independent headless data, never production notebooks or connections.

Focused commands for this feature:

```sh
go test ./plugins -count=1
go test ./internal/catalog -run 'TestProvisionWorkNotebook|TestOIDCEnrollment' -count=1
go test ./internal/web -run 'TestConnectAI|TestEmployeeCompanionOAuth|TestOIDCDefaultGroup|TestOAuthManagementConsent|TestMCPDiscoverySchemasAreObjects' -count=1
npx --no-install playwright test scripts/browser/connect-ai.spec.mjs --workers=1
```

For client startup changes, exercise the installed client's discovery and at
least one tool call; package validation and OAuth login are insufficient. Keep
original failures and native-client wire evidence private. A release failure is
not overwritten when a later candidate passes.

## PR feedback and complete regression

`make check-fast BASE=<commit> JOBS=2` runs formatting, vet, frontend/fixture and
Python checks plus race tests for affected Go packages and their production or
race-test importers. Selection includes committed, staged, unstaged and untracked
inputs; deleted fixtures/embeds retain their owning packages, dependency changes
and runtime inputs no package owns select all packages. Test, document and
operator inputs that no Go package reads, such as browser harness files, Make
targets and release tooling, select none; this tier still formats browser files
and runs the fixture, frontend and operator tests. Bundle sources (`frontend/`, `internal/web/ui/`, `package.json`,
`package-lock.json`) select `internal/web`, which embeds the built bundle, and its
importers; this tier still runs the frontend tests and builds the binary. Routine
releases share this selection (`verification_plan.py` serves both tiers), and
`frontend/` or package manifest changes still make a release run the full gate.
Every selected package runs its complete suite; no test depends on `-short`.

With a verified baseline, the web package runs the existing critical authentication,
permission, conflict, MCP/API, sanitizer, live-edit and connector-revocation checks
listed in `verification_plan.py` (`CRITICAL_WEB`), the reviewed callers of each
affected feature (`FEATURE_WEB_TESTS`), and every compiled current
Test/Example/Fuzz case in changed web test files: the same membership a routine
release of that change selects. Operator tests require each feature glob to select
a current case. Four isolated process batches run these unchanged tests.
The selection report names every selected web case and counts omitted cases.
Missing baseline/input provenance or a
missing critical case selects the complete web inventory in 16 isolated batches.

This PR tier produces no release receipt. Routine release receipts use their own
reviewed affected coverage; explicit full gates retain every test. No
baseline means all race tests, including the entire web package.

GitHub Check uses this tier for pull requests. Its manual full
runs execute `release_image.py --verify-only --mode full` on a standard runner,
with npm downloads, Playwright browsers and image layers (`FMN_IMAGE_CACHE=gha`)
in the Actions cache. Pushes to `main` and `next` and a weekly schedule run only
the advisory scan. Publication
uses `--since DEPLOYED_REVISION --mode auto` by default. An automatic full plan keeps
every Go, web and browser test. Of the slow container scenarios (source, jev, oidc,
totp) it runs only those whose inputs changed. Image or dependency changes keep
every container, and so does an explicit `--mode full`. Actions are pinned to verified upstream commit IDs. The fast
tier aims for ten minutes, but shared changes can select broad coverage; runner
wall time and cold setup must be reported rather than hidden behind that target.
Fast and full runs have distinct [concurrency groups](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency), so a pull request cannot cancel a complete regression.
Account billing failures that prevent a job from starting are external blockers,
not passing checks, and are separate from local verification.

The Linux backup watcher tests require inotify-tools and fail if it is missing.
On other hosts only those live watcher cases skip; portable once, managed push,
revocation and independent restore tests still run. The complete release gate
retains its Linux container backup checks.

Source tests drive the real Poppler, Tesseract and FFmpeg on the host. Hosted image
descriptions in `internal/web` fail without Tesseract; other cases skip when a tool
is missing. GitHub's full job installs Ubuntu's packages, and `smoke-source` repeats
the whole matrix with the image's pinned versions.

Docker smokes must not rely on Docker Desktop hiding differences from a Linux host.
The gate runs under umask 077 and containers run as another user, so a bind mount
needs modes that user can read, and with `--init` or `--cap-drop ALL` no capability
covers for them. Native amd64 containers also start sooner than emulated ones.

Capacity scales with CPUs, so a large host can miss what a small one reaches. Source
imports get one slot on two cores and four from eight, and `smoke-resource` only meets
the upload refused at capacity on the smaller host. To reproduce that locally, run the
smoke in a Docker-in-Docker container limited with `docker update --cpuset-cpus=0,1`.

`forgetmenote healthz` performs a bounded unauthenticated request to the running
process at `http://127.0.0.1:8080/healthz` and opens no application state. An operator
may select a different endpoint with `-url`. Redirects, oversized responses and
non-`ok` bodies fail without disclosing upstream diagnostics. Docker uses this
probe instead of an additional wget package. Existing parser package versions,
base-image digests and OCR content hashes remain pinned.

## Reproducible runtime dependencies

Docker base images are digest pinned. Debian package sources also use the signed
snapshot timestamp in `deploy/debian.sources`; a base digest alone does not pin
repository contents. Snapshot validity-age checks are disabled because the
snapshot is intentionally historical; package signatures remain required.
A clean host, such as a GitHub runner, downloads every package from that archive,
so a `snapshot.debian.org` outage fails the image build. That is an external
blocker, not a passing check; wait for the archive and start a new run.
See [Debian snapshots](https://snapshot.debian.org/) and
[apt source configuration](https://manpages.debian.org/testing/apt/sources.list.5.en.html).

For an update, choose a reviewed snapshot timestamp, verify all explicit parser
versions are available in both the iWork and runtime stages, update the snapshot
and package pins together, and run the full image gate. Never fall back silently
to a moving mirror or disable signature checks. OCR data retains its separate
revision and SHA-256 pin. Python operator requirements have one owner at
`deploy/requirements.txt`; the runtime uses standard-library helpers.

Optional integration adapters remain in the single Go executable. They do not
start another server or load credentials when disabled. A current native symbol inventory totals about 3.03 MB for adapters/AWS/PostgreSQL
in a 55.3 MB unstripped binary; this excludes shared dependencies and is not a
counterfactual stripped-image saving. Splitting those small Go adapters would add packaging paths without removing the image's dominant local
parser/runtime dependencies; measure the checked image and binary before adding
a second runtime variant. Fonts and assets remain embedded, with license notices
computed from the actual production bundle. Historical brand-study font copies
reuse those assets; the used Young Serif study font remains.
