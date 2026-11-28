# Platform-independent outputs build natively; only the runtime stages emulate
# linux/amd64 on other hosts. Both pins are multi-platform indexes.
# Each stage copies an allowlist, and .dockerignore drops tests and fixtures from it,
# so only image inputs invalidate the build (deploy/production/verification_plan.py
# IMAGE_SOURCES; operator tests keep the three in agreement). Cache mounts hold
# downloads and compiler output between builds; they never reach an image layer.
#
# FMN_OMIT names optional integrations a self-hosted build leaves out, separated
# by spaces: postgres redshift outlook slack (docs/integrations.md). Each sets
# the Go build tag fmn_omit_NAME. The default, empty, builds every integration,
# and the release image uses it. iwork and ocr name integrations that were
# removed, so they leave out nothing.
ARG FMN_OMIT=
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:8a32c8cf649a49915a66e81e7a609223c708d0adabf288ee3f4077d765542f85 AS frontend
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend ./frontend
COPY internal/web/ui ./internal/web/ui
RUN npm run build

# Go matches go.mod; 1.27.4 fixes GO-2026-6859 (net/http MaxBytesReader server crash).
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
ARG TARGETOS TARGETARCH FMN_OMIT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY plugins ./plugins
COPY --from=frontend /src/internal/web/ui/dist ./internal/web/ui/dist
RUN --mount=type=cache,target=/root/.cache/go-build \
    tags=; for name in $FMN_OMIT; do \
      case $name in postgres|redshift|outlook|slack) tags="$tags,fmn_omit_$name" ;; \
      iwork|ocr) echo "FMN_OMIT: $name was removed; nothing to leave out" >&2 ;; \
      *) echo "FMN_OMIT: unknown integration $name" >&2; exit 1 ;; esac; \
    done \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags "${tags#,}" -ldflags="-s -w" -o /out/forgetmenote ./cmd/forgetmenote

# The backup worker image's downloads, fetched for the target and checked against
# pinned SHA-256s; nothing runs here. rclone uploads snapshots to drives and other
# remotes (deploy/git-backup/README.md#rclone), pinned at the release CI tests the
# worker with, and comes with its MIT licence; change the pin in
# .github/workflows/check.yml and the worker's README with it (test_watcher.py
# checks that they agree). The key is PostgreSQL's apt signing key, fingerprint
# B97B 0AFC AA1A 47F0 44F2 44A0 7FCC 7D46 ACCC 4CF8.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS worker-downloads
ARG TARGETARCH
ARG RCLONE_VERSION=1.74.3
ARG RCLONE_SHA256_AMD64=dbee7ccd7a5d617e4ed4cd4555c16669b511abfe8d31164f61be35ac9e999bd2
ARG RCLONE_SHA256_ARM64=8f8d47446e061f80c3256659fe8e21f56d72d96aaefe1275d088ea5eb6b42aa7
ARG RCLONE_COPYING_SHA256=8cd2e9e750b90a04b7d82dbbca3930c696ae0309d7c10464f90a44f45754cd04
ARG POSTGRESQL_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
RUN case $TARGETARCH in amd64) sum=$RCLONE_SHA256_AMD64 ;; arm64) sum=$RCLONE_SHA256_ARM64 ;; \
      *) echo "rclone: no pinned release for $TARGETARCH" >&2; exit 1 ;; esac \
 && mkdir -p /out/usr/local/bin /out/usr/share/doc/rclone /keys \
 && name=rclone-v$RCLONE_VERSION-linux-$TARGETARCH \
 && wget -q -O /tmp/rclone.zip https://downloads.rclone.org/v$RCLONE_VERSION/$name.zip \
 && echo "$sum  /tmp/rclone.zip" | sha256sum -c - \
 && unzip -p /tmp/rclone.zip $name/rclone > /out/usr/local/bin/rclone \
 && chmod 755 /out/usr/local/bin/rclone \
 && wget -q -O /out/usr/share/doc/rclone/COPYING https://raw.githubusercontent.com/rclone/rclone/v$RCLONE_VERSION/COPYING \
 && echo "$RCLONE_COPYING_SHA256  /out/usr/share/doc/rclone/COPYING" | sha256sum -c - \
 && chmod 644 /out/usr/share/doc/rclone/COPYING \
 && wget -q -O /keys/postgresql.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
 && echo "$POSTGRESQL_KEY_SHA256  /keys/postgresql.asc" | sha256sum -c - \
 && chmod 644 /keys/postgresql.asc \
 && rm /tmp/rclone.zip

FROM python:3.12-slim-bookworm@sha256:e697c81897302a4c4333bade000d98038fbb21eb064f8ee1fd42d02ebcd9d337 AS runtime
COPY deploy/debian.sources /etc/apt/sources.list.d/debian.sources
ARG APP_UID=1000
ARG APP_GID=1000
ARG POPPLER_VERSION=22.12.0-2+deb12u3
# The server's one document tool is Poppler's pdftotext, for PDF text layers;
# connected agents read scans, images, media and Apple documents themselves.
# source-versions records it for provenance.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client ca-certificates tzdata poppler-utils=${POPPLER_VERSION} \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /usr/share/forgetmenote \
 && printf '%s' "Poppler ${POPPLER_VERSION}" > /usr/share/forgetmenote/source-versions \
 && groupadd -g ${APP_GID} forgetmenote && useradd -m -u ${APP_UID} -g forgetmenote forgetmenote \
 && mkdir /data && chown forgetmenote:forgetmenote /data
COPY --chmod=755 deploy/git-backup/watcher.py /usr/local/bin/forgetmenote-backup
COPY --from=build /out/forgetmenote /usr/local/bin/forgetmenote
USER forgetmenote:forgetmenote
WORKDIR /data
ENV DATA_DIR=/data HOME=/home/forgetmenote AUTOMATION_PYTHON=/usr/local/bin/python3 \
    OMP_NUM_THREADS=1 OMP_THREAD_LIMIT=1 PYTHONDONTWRITEBYTECODE=1
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["forgetmenote", "healthz"]
ENTRYPOINT ["forgetmenote"]

# The backup worker image (deploy/git-backup/README.md#the-worker-image): the
# application image plus the pinned rclone and pg_dump, so one container runs every
# worker mode, instance snapshots included, with nothing on the host. It shares
# every layer of the application image. pg_dump comes from PostgreSQL's archive,
# which keeps every release, at the newest 17.x minor: it dumps the Postgres 17
# that compose.yaml runs and every older server.
FROM runtime AS worker
ARG POSTGRESQL_CLIENT_VERSION=17.12-1.pgdg12+1
ARG POSTGRESQL_COMMON_VERSION=297.pgdg12+1
USER root
COPY --from=worker-downloads /keys/postgresql.asc /usr/share/keyrings/postgresql.asc
RUN printf '%s\n' 'Types: deb' 'URIs: https://apt-archive.postgresql.org/pub/repos/apt' \
      'Suites: bookworm-pgdg-archive' 'Components: main' 'Signed-By: /usr/share/keyrings/postgresql.asc' \
      > /etc/apt/sources.list.d/postgresql.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends postgresql-client-17=${POSTGRESQL_CLIENT_VERSION} \
      libpq5=${POSTGRESQL_CLIENT_VERSION} postgresql-client-common=${POSTGRESQL_COMMON_VERSION} \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /usr/lib/postgresql/17/bin/pg_dump /usr/local/bin/pg_dump
COPY --from=worker-downloads /out/ /
USER forgetmenote:forgetmenote
ENV FMN_BACKUP_RCLONE=/usr/local/bin/rclone
HEALTHCHECK NONE
ENTRYPOINT ["forgetmenote-backup"]

# The application image, which a build without --target produces.
FROM runtime AS app
