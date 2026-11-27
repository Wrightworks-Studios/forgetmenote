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
