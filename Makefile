.PHONY: build test fmt vet

build:
	go build -o cancanneed ./cmd/cancanneed

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...
