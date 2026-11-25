# Platform-independent outputs build natively; only the runtime stages emulate
# linux/amd64 on other hosts. Both pins are multi-platform indexes.
# Each stage copies an allowlist, and .dockerignore drops tests and fixtures from it,
# so only image inputs invalidate the build (deploy/production/verification_plan.py
# IMAGE_SOURCES; operator tests keep the three in agreement). Cache mounts hold
# downloads and compiler output between builds; they never reach an image layer.
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:8a32c8cf649a49915a66e81e7a609223c708d0adabf288ee3f4077d765542f85 AS frontend
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend ./frontend
COPY internal/web/ui ./internal/web/ui
RUN npm run build

# Go matches go.mod; 1.27.4 fixes GO-2026-6859 (net/http MaxBytesReader server crash).
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY plugins ./plugins
COPY --from=frontend /src/internal/web/ui/dist ./internal/web/ui/dist
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/forgetmenote ./cmd/forgetmenote

FROM python:3.12-slim-bookworm@sha256:e697c81897302a4c4333bade000d98038fbb21eb064f8ee1fd42d02ebcd9d337 AS iwork
COPY deploy/debian.sources /etc/apt/sources.list.d/debian.sources
ARG IWORK_VERSION=0.1.10-3+b1
RUN apt-get update \
 && apt-get install -y --no-install-recommends g++ pkg-config libetonyek-dev=${IWORK_VERSION} \
 && rm -rf /var/lib/apt/lists/*
COPY deploy/iwork/convert.cpp /src/convert.cpp
RUN mkdir -p /out \
 && g++ -O2 -std=c++11 /src/convert.cpp $(pkg-config --cflags --libs libetonyek-0.1 librevenge-generators-0.0 librevenge-stream-0.0) -o /out/forgetmenote-iwork

FROM python:3.12-slim-bookworm@sha256:e697c81897302a4c4333bade000d98038fbb21eb064f8ee1fd42d02ebcd9d337 AS runtime
COPY deploy/debian.sources /etc/apt/sources.list.d/debian.sources
ARG APP_UID=1000
ARG APP_GID=1000
ARG POPPLER_VERSION=22.12.0-2+deb12u3
ARG FFMPEG_VERSION=7:5.1.9-0+deb12u1
ARG TESSERACT_VERSION=5.3.0-2
ARG IWORK_VERSION=0.1.10-3+b1
ARG TESSDATA_REVISION=87416418657359cb625c412a48b6e1d6d41c29bd
ARG TESSDATA_SHA256=7d4322bd2a7749724879683fc3912cb542f19906c83bcc1a52132556427170b2
# Deterministic local parsers and specialized OCR; no general inference runtime.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client ca-certificates tzdata \
      poppler-utils=${POPPLER_VERSION} tesseract-ocr=${TESSERACT_VERSION} \
      ffmpeg=${FFMPEG_VERSION} libetonyek-0.1-1=${IWORK_VERSION} \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /usr/share/tesseract-ocr/5/tessdata /usr/share/forgetmenote \
 && python -c 'import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])' \
      https://raw.githubusercontent.com/tesseract-ocr/tessdata_fast/${TESSDATA_REVISION}/eng.traineddata \
      /usr/share/tesseract-ocr/5/tessdata/eng.traineddata \
 && echo "${TESSDATA_SHA256}  /usr/share/tesseract-ocr/5/tessdata/eng.traineddata" | sha256sum -c - \
 && printf 'Poppler %s; Tesseract %s eng tessdata_fast %s sha256 %s; FFmpeg %s; libetonyek %s' \
      "${POPPLER_VERSION}" "${TESSERACT_VERSION}" "${TESSDATA_REVISION}" "${TESSDATA_SHA256}" "${FFMPEG_VERSION}" "${IWORK_VERSION}" \
      > /usr/share/forgetmenote/source-versions \
 && groupadd -g ${APP_GID} forgetmenote && useradd -m -u ${APP_UID} -g forgetmenote forgetmenote \
 && mkdir /data && chown forgetmenote:forgetmenote /data
COPY --from=iwork /out/forgetmenote-iwork /usr/local/bin/forgetmenote-iwork
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
