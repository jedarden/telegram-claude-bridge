.PHONY: all build proxy bridge dashboard clean test test-admin-allowlist test-v1-conformance vet docker validate-deployment-security test-deployment-security

VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
COMMITSHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.CommitSHA=$(COMMITSHA) -X main.BuildDate=$(BUILDDATE)

all: build

build: proxy bridge dashboard

dashboard:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/dashboard ./cmd/dashboard/

proxy:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/proxy ./cmd/proxy/

bridge:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/bridge ./cmd/bridge/

clean:
	rm -f bin/proxy bin/bridge bin/dashboard

test: test-admin-allowlist
	go test ./...

test-admin-allowlist:
	bash scripts/test-manage-admins.sh

# Run the bridge-facing v1 proxy contract suite explicitly. The normal test
# target already includes it; this target gives CI and focused local checks a
# stable command that names the conformance boundary.
test-v1-conformance:
	go test -buildvcs=false ./cmd/proxy -run '^TestProxyBridgeV1_' -count=1

vet:
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMITSHA) -t telegram-claude-bridge:$(VERSION) .

# Validate the bridge-side security contract checked into this repository.
# Full deployment validation takes explicit manifest/policy paths; see the
# deployment security guide and the fixture test below.
validate-deployment-security:
	bash scripts/validate-deployment-security.sh --static-only --bridge-unit deploy/telegram-claude-bridge.service

test-deployment-security:
	bash scripts/test-validate-deployment-security.sh
