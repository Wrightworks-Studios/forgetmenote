# Optional integrations

Some integrations serve few instances or bring their own dependencies: an AWS
SDK client, a native document converter, an OCR engine, a chat workspace app.
Each one is built in by default, and the release image includes them all. A
contributor or self-hoster can leave any of them out of a build; the rest of
ForgetMeNote behaves the same.

| Name | What it adds | Leaving it out also drops |
| --- | --- | --- |
| `postgres` | PostgreSQL [data connections](../deploy/integrations/README.md) (`kind: postgres`) | Nothing: the instance database still uses pgx |
| `redshift` | Amazon Redshift data connections (`kind: redshift`) | The Redshift Data API client |
| `outlook` | Outlook `.msg` [sources](sources.md) | The compound-file and legacy charset readers |
| `iwork` | Pages and Keynote sources | libetonyek and the `forgetmenote-iwork` converter in the image |
| `ocr` | Local Tesseract OCR of image sources and of scanned regions in PDF pages | Tesseract and its English data in the image |
| `slack` | The [Slack app](../deploy/integrations/README.md) (`SLACK_CONFIG_FILE`) and personal Slack connections | The Slack Web API client |

## Build without them

Each name is a Go build tag, `fmn_omit_NAME`, which removes that integration's
code:

```sh
go build -tags fmn_omit_redshift,fmn_omit_ocr ./cmd/forgetmenote
make build-core   # every integration left out: bin/forgetmenote-core
```

The Dockerfile's `FMN_OMIT` build argument takes the names, separated by
spaces, sets the tags and leaves out the system packages of `iwork` and `ocr`.
Compose passes `FMN_OMIT` from `.env`:

```sh
docker build --build-arg FMN_OMIT="redshift iwork ocr" -t forgetmenote:local .
```

An unknown name fails the image build. `forgetmenote integrations` prints what a
binary includes; the server also logs it at startup, and **Administration**
lists what the build leaves out.

## What a build without one does

Configuration that names a left-out integration stops startup with an error
that names it and its tag: a `kind: redshift` data connection, a Slack app
configuration, or a `slack` application in the personal connections
configuration. A left-out source format is not offered for upload, in the
browser or by the `source_formats` tool, and importing one is refused with that
error. Stored originals of every format remain readable and downloadable, and
their notes keep their text. Personal Slack grants that a build with Slack
stored stay in the database, refused rather than used, until a build with
Slack runs again.

Without `ocr`, image sources are not offered, even where instance policy allows
hosted OCR, because their text starts with local OCR. A PDF keeps its native
text and physical page anchors. A page with images is marked incomplete and
says that this build leaves out local OCR; hosted OCR, where instance policy
allows it, still reads it.

## Where the code is

`internal/integrations` lists the integrations and reports which ones a build
includes. Each integration's code is in files of its owner that carry the build
constraint `!fmn_omit_NAME`; a file beside them with `fmn_omit_NAME` replaces it
with refusals:

| Name | Code | Replacement |
| --- | --- | --- |
| `postgres` | `internal/connectors/postgres.go` | `postgres_omitted.go` |
| `redshift` | `internal/connectors/redshift.go` | `redshift_omitted.go` |
| `outlook` | `internal/sources/outlook.go`, `rtf.go` | `outlook_omitted.go` |
| `iwork` | `internal/sources/iwork.go` | `iwork_omitted.go` |
| `ocr` | `internal/sources/ocr.go` | `ocr_omitted.go` |
| `slack` | `internal/messaging/slack.go`, `internal/web/slack_transport.go` | `slack_omitted.go`, `slack_transport_omitted.go` |

`git grep fmn_omit_NAME` finds every file of one, its tests included. For
Slack the tag removes the app's Web API transport, and `integrations.Check`
refuses a personal Slack application where personal connections open and a
stored Slack grant where one is read (`internal/connectors/personal_oauth.go`).
The web layer's Slack handlers, task forms and reminders stay in every build,
unreachable without a configured Slack app, as on an instance that configures
none. Shared contracts stay in
every build: the message and card types (`internal/messaging/post.go`), the SQL
operation contract and guard
(`internal/connectors/database.go`, `redshift_policy.go`), the source format
catalog (`internal/workspace/source_formats.go`), and the single gate that maps
a source format to its integration (`internal/sources/formats.go`).

## Tests

Tests run in both builds. A test of one integration carries its tag; a test that
covers several formats skips the ones the build leaves out, and caller-visible
tests check that each build offers and refuses the right formats and
connections. `make check-fast` vets and tests the build without any of them
(`make check-core`); the release gate verifies the default build, which the
release image uses, and its source smoke checks the image's OCR data, converter
and integration list.
