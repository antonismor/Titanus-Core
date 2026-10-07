.PHONY: build clean test fmt

build:
	mkdir -p bin
	go build -o bin/titanus ./cmd/titanus
	go build -o bin/titanusd ./cmd/titanusd
	go build -o bin/titanus-agent ./cmd/titanus-agent
	CGO_ENABLED=0 go build -o bin/titanus-init ./cmd/titanus-init

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

clean:
	rm -rf bin
