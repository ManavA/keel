GOLANGCI_LINT_VERSION ?= v2.6.0

.PHONY: all
all: fmt test lint vuln

.PHONY: test
test:
	go test ./...

# Database-backed tests, with a missing Docker made a failure rather than a
# skip. pg/testdb needs Docker; nothing else does.
.PHONY: test-db
test-db:
	KEEL_REQUIRE_DB=1 go test -count=1 ./pg/...

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run ./...

.PHONY: vuln
vuln:
	@command -v govulncheck >/dev/null 2>&1 || \
		go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

.PHONY: fmt
fmt:
	gofmt -l -w .
	go mod tidy

# The secret scan CI also runs. Run it yourself before pushing: by the time the
# CI job fails, the secret is already in the remote's history.
.PHONY: secrets
secrets:
	gitleaks detect --redact --verbose

.PHONY: build
build:
	go build ./...
