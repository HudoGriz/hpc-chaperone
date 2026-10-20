.PHONY: build test lint secrets
build:
	CGO_ENABLED=1 go build -o chaperone ./cmd/chaperone
test:
	go test -race -cover ./...
lint:
	go vet ./...
# Whole git history; fails on any credential. Also run by .githooks/pre-push.
secrets:
	docker run --rm -v "$(CURDIR):/repo" ghcr.io/gitleaks/gitleaks:v8.21.2 git /repo --redact
