.DEFAULT_GOAL := frontend
PYTHON ?= python3
BASE ?=
JOBS ?= 2
export BASE
SMOKE_IMAGE ?= forgetmenote:poc
BACKUP_TEST_COMMAND ?= $(PYTHON) -m unittest discover -s deploy/git-backup
export SMOKE_IMAGE
# Leaves out every optional integration (docs/integrations.md).
CORE_TAGS := fmn_omit_postgres,fmn_omit_redshift,fmn_omit_outlook,fmn_omit_iwork,fmn_omit_ocr
# The web cases that reach those integrations' owners.
CORE_WEB_TESTS := Source|Hosted|Format|DataConnection|Integration|MCPContract

.PHONY: setup-deps setup-browser operator-venv browser-summary ui-capture format-browser frontend test test-go test-frontend test-fixture test-python check check-fast check-core check-release check-browser build build-core build-clients smoke smoke-core smoke-headless smoke-source smoke-resource smoke-chat smoke-jev smoke-oidc smoke-totp smoke-cluster smoke-full test-python-host vet format

# Install explicitly; focused checks reuse the existing toolchain and caches.
setup-deps:
	npm ci
	go mod download
	$(PYTHON) -m pip install -r deploy/requirements.txt

# The suite runs in Chromium, and the caret spec also in WebKit, Safari's engine.
setup-browser:
	npx --no-install playwright install --with-deps chromium webkit

# Pinned operator Python for release tooling: .venv/operator/bin/python deploy/production/release_image.py ...
operator-venv:
	$(PYTHON) -m venv .venv/operator
	.venv/operator/bin/python -m pip install --quiet -r deploy/requirements.txt

# Counts, failures and artifact paths only; ARGS selects files or flags, e.g. ARGS='scripts/browser/editor-*.spec.mjs'.
browser-summary: frontend
	node scripts/browser-summary.mjs $(ARGS)

# Screenshots of the main surfaces from a disposable instance; OUT selects the directory.
ui-capture: frontend
	node scripts/ui-capture.mjs $(or $(OUT),ui-capture)

frontend:
	npm run build

test-go: frontend
	go test -race ./... -timeout 45m

test-frontend: frontend
	npm test

# A cold build can outlast a test's timeout, so build before the tests start.
test-fixture: build
	FMN_BROWSER_BINARY=$(CURDIR)/bin/forgetmenote node --test scripts/browser-fixture.test.mjs

test-python-host:
	$(PYTHON) -m unittest discover -s deploy/standalone
	$(PYTHON) -m unittest discover -s deploy/production
	$(PYTHON) -m unittest discover -s scripts -p test_github_app_manifest.py

test-python: test-python-host
	$(BACKUP_TEST_COMMAND)

test: test-go test-frontend test-fixture test-python

vet: frontend
	go vet ./...

format:
	@test -z "$$(gofmt -l cmd internal plugins scripts)"

# Browser tests keep one statement per line (pinned Prettier, scripts/browser/.prettierrc.json).
format-browser:
	npx --no-install prettier --check scripts/browser

# The build without optional integrations: every package and test compiles,
# and the integrations' owners and their callers pass.
check-core: frontend
	go vet -tags $(CORE_TAGS) ./...
	go test -race -count=1 -tags $(CORE_TAGS) ./internal/integrations ./internal/connectors ./internal/sources ./cmd/forgetmenote
	go test -race -count=1 -tags $(CORE_TAGS) ./internal/web -run '$(CORE_WEB_TESTS)'

# This is PR feedback, never a production release receipt.
check-fast: test-frontend test-fixture test-python vet format format-browser check-core
	$(PYTHON) deploy/production/fast_check.py --jobs $(JOBS)

check-release:
	$(PYTHON) deploy/production/release_image.py --verify-only --mode full --jobs $(JOBS)

check: check-fast

check-browser: frontend format-browser
	npm run test:browser

build: frontend
	go build -trimpath -o bin/forgetmenote ./cmd/forgetmenote

build-core: frontend
	go build -trimpath -tags $(CORE_TAGS) -o bin/forgetmenote-core ./cmd/forgetmenote

build-clients: frontend
	@for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		GOOS=$${platform%/*} GOARCH=$${platform#*/} CGO_ENABLED=0 go build -trimpath -o bin/forgetmenote-$${platform%/*}-$${platform#*/} ./cmd/forgetmenote || exit; \
	done

smoke: smoke-core

smoke-core:
	$(PYTHON) scripts/docker_smoke.py

smoke-headless:
	$(PYTHON) scripts/headless_smoke.py

smoke-source:
	$(PYTHON) scripts/source_smoke.py

smoke-resource:
	$(PYTHON) scripts/resource_smoke.py

smoke-chat:
	$(PYTHON) scripts/chat_smoke.py

smoke-jev:
	$(PYTHON) scripts/jev_smoke.py

smoke-oidc:
	REQUIRE_TOTP=0 $(PYTHON) scripts/oidc_smoke.py

smoke-totp:
	REQUIRE_TOTP=1 $(PYTHON) scripts/oidc_smoke.py

smoke-cluster:
	$(PYTHON) scripts/cluster_smoke.py

# Preserve sequential execution even if the caller uses make -j.
smoke-full:
	@for target in core headless source resource chat jev oidc totp cluster; do \
		$(MAKE) smoke-$$target || exit; \
	done
