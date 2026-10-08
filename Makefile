.PHONY: build clean test fmt release

REVISION := $(shell git rev-parse HEAD)
VERSION ?= $(shell awk -F '"' '/^var Version = / {print $$2}' internal/version/version.go)
LDFLAGS := -s -w -X github.com/antonismor/Titanus-Core/internal/version.Version=$(VERSION) -X github.com/antonismor/Titanus-Core/internal/version.Revision=$(REVISION)

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/titanus ./cmd/titanus
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/titanusd ./cmd/titanusd
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/titanus-agent ./cmd/titanus-agent
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/titanus-init ./cmd/titanus-init

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

clean:
	rm -rf bin

release: build
	bash scripts/package-release.sh
