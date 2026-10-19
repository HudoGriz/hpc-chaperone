.PHONY: build test lint
build:
	CGO_ENABLED=1 go build -o chaperone .
test:
	go test -race -cover ./...
lint:
	go vet ./...
