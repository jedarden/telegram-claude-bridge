.PHONY: all build proxy bridge dashboard clean test vet docker

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

test:
	go test ./...

vet:
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMITSHA) -t telegram-claude-bridge:$(VERSION) .
