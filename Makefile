VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo 0.2.0-dev)
LDFLAGS := -s -w -X ivarabase/internal/core.Version=$(VERSION)

.PHONY: build test run ui ui-dev dev dev-reset linux compat selftest test-apple test-proxy docker docker-multiarch

build:
	go build -ldflags "$(LDFLAGS)" -o bin/ivarabase ./cmd/ivarabase

test:
	go vet ./... && go test ./...

run: build
	./bin/ivarabase serve

# cross-compile for a Linux server (pure Go, no cgo)
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/ivarabase-linux-amd64 ./cmd/ivarabase
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/ivarabase-linux-arm64 ./cmd/ivarabase

# rebuild the React UI into web/static (embedded by go build); the built files are committed
ui:
	cd web/ui && npm install && npm run build

# dev instance (./.dev-data, login in scripts/dev.env) + hot-reloading UI: open http://localhost:5173
dev:
	./scripts/dev.sh

# wipe the dev instance (it is recreated with the dev login on the next make dev)
dev-reset:
	-pkill -f "ivarabase serve --data $(CURDIR)/.dev-data"
	rm -rf .dev-data

# UI only, on :5173 - needs `ivarabase serve` already running on :8787
ui-dev:
	cd web/ui && npm run dev

# supabase-js compatibility suite against a running server (needs `ivarabase login` or IVARA_URL/IVARA_TOKEN)
compat: build
	./scripts/compat.sh

# Every end-to-end suite (compat, apple, proxy) on throwaway instances (DENO=, PG_BIN_DIR=, POSTGREST= test other versions)
selftest:
	./scripts/selftest.sh

# Sign in with Apple end to end, against a throwaway instance and a mock of Apple (no Apple account needed)
test-apple:
	SUITES=apple ./scripts/selftest.sh

# Behind a reverse proxy (tls.mode proxy) with an nginx stand-in and a wildcard certificate
test-proxy:
	SUITES=proxy ./scripts/selftest.sh

# Docker image (PostgreSQL major in the tag); docs/DOCKER.md
PG_MAJOR ?= 18
IMAGE ?= ghcr.io/ankittchaurasia/ivarabase
docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg PG_MAJOR=$(PG_MAJOR) -t $(IMAGE):$(VERSION)-pg$(PG_MAJOR) .

# amd64 + arm64, pushed to the registry (needs docker buildx and a login)
docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=$(VERSION) --build-arg PG_MAJOR=$(PG_MAJOR) \
		-t $(IMAGE):$(VERSION)-pg$(PG_MAJOR) --push .
