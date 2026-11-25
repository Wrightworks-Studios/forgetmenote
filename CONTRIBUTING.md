# Contributing

This guide takes you from a fresh clone to a pull request. Coding agents start
from [AGENTS.md](AGENTS.md) instead; it covers the same ground in more detail.

## Prerequisites

- Go 1.27.4 (`go.mod`), Git, and Node.js 24 with npm, as CI and the Docker build use.
- PostgreSQL 14 or later, with `initdb`, `pg_ctl` and `postgres` on your `PATH`.
  Go and browser tests start throwaway clusters with them. Debian and Ubuntu keep
  them in `/usr/lib/postgresql/VERSION/bin`; add that directory to `PATH`.
- Python 3.12 for `make check-fast`. On Linux it also needs `openssl` and
  `inotify-tools`, and browser tests need a C compiler.
- Docker with Compose, only for container smoke tests and the [README quick start](README.md#run).

## Build

```sh
git clone https://github.com/Wrightworks-Studios/forgetmenote.git
cd forgetmenote
npm ci
npm run build
go build -o bin/forgetmenote ./cmd/forgetmenote
```

`npm run build` bundles the browser assets into `internal/web/ui/dist/`, which the
binary embeds; rerun it after changing `frontend/` or `internal/web/ui/`, before a
Go build or browser test. `make build` runs both steps.

## Run a local instance with sample notes

Everything below lives in `data/`, which Git ignores; delete it to start over.
This Postgres trusts every local connection, so use it only for development.

```sh
mkdir -p data/app
initdb -D data/pg -U forgetmenote -A trust -E UTF8 --no-locale
pg_ctl -D data/pg -l data/pg.log -o "-p 5433 -c listen_addresses=127.0.0.1 -k ''" start
createdb -h 127.0.0.1 -p 5433 -U forgetmenote forgetmenote
echo 'postgres://forgetmenote@127.0.0.1:5433/forgetmenote?sslmode=disable' > data/database-url
export DATABASE_URL_FILE=$PWD/data/database-url
./bin/forgetmenote migrate
account=$(echo 'local-dev-password' | ./bin/forgetmenote admin bootstrap \
  --data data/app --username dev --password-stdin | sed -E 's/.*"user":"([^"]+)".*/\1/')
./bin/forgetmenote admin credential --data data/app --user "$account" --name dev \
  --projects default --scopes notes:read,notes:write --audiences api --days 7 \
  --output data/dev.token
./bin/forgetmenote -data data/app -url http://localhost:8080
```

The server never migrates itself: rerun `migrate` after pulling new migrations. In
a second terminal, load the repository's own documentation as sample notes:

```sh
export FORGETMENOTE_SERVER=http://localhost:8080 FORGETMENOTE_TOKEN="$(cat data/dev.token)"
for f in docs/*.md; do ./bin/forgetmenote note write default "$f" --file "$f" --create >/dev/null; done
```

Open <http://localhost:8080> and sign in as `dev` with `local-dev-password`; the
account is an Admin. Stop with Ctrl-C and `pg_ctl -D data/pg stop`. To start
again, run the `pg_ctl … start` and `export` lines, then the server.

## Checks before a pull request

Start a behavior change with the smallest test that fails without it, through the
caller's boundary: `httptest` for HTTP, the MCP SDK client for MCP, `pgtest`
databases for storage and real temporary Git repositories where Git is involved.
Use synthetic data; never real notes, credentials or paid AI calls.

```sh
go test -race ./internal/PACKAGE -run '^TestYourBehavior$' -count=1
npm test
make setup-browser   # once: Playwright's Chromium and WebKit
make browser-summary ARGS='scripts/browser/FEATURE.spec.mjs'
```

Pull-request CI runs `make check-fast`: formatting, `go vet`, the frontend, fixture
and Python tests, the build without [optional integrations](docs/integrations.md)
(`make check-core`), and race tests for the Go packages changed since `BASE`
(every package without it). Run it before you push:

```sh
python3.12 -m venv .venv
. .venv/bin/activate
make setup-deps
make check-fast BASE=origin/next
```

CI runs no browser tests, so run the specs for the screens you changed. The suite
targets Linux; on macOS a few editor keyboard cases fail because macOS uses
different editing shortcuts.

## Formatting browser tests

Browser tests in `scripts/browser/` use pinned Prettier, one statement per line.
After editing them, run `npx --no-install prettier --write scripts/browser`;
`make format-browser` checks them and is part of `make check-fast`.

## Pull requests

- Branch from `next` and open the pull request against `next`, the default
  branch. Pull requests are squash-merged.
- Keep each one to a single coherent change, with its tests, and leave unrelated
  code alone. Describe the problem, the resulting behavior and the checks you ran.
- Keep personal and organization details out of files, commit messages, branch
  names and pull request text: no names of employers, customers, instances or
  people, hostnames, internal URLs, account IDs, local paths or personal email
  addresses. Use `example.com` and synthetic fixtures, and commit with your GitHub
  noreply address.
- Report security issues privately, as [SECURITY.md](SECURITY.md) describes.

## Learn the codebase

[Architecture](docs/architecture.md) explains notebook storage, authority,
authentication and ownership; read it before changing those boundaries. The
[engineering rules](docs/engineering-rules.md) are the full requirements, and the
[documentation index](docs/README.md) lists the user and operator guides.

## Licence

ForgetMeNote is licensed under the [Functional Source License 1.1, ALv2 Future License](LICENSE.md).
