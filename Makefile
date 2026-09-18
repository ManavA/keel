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

# Replay every migrations directory in the module against a throwaway Postgres:
# applied to an empty schema, applied again for idempotence, then each file down
# and up. A down that cannot run — one missing CASCADE, typically — fails here
# rather than the first time somebody rolls back.
.PHONY: migrations-check
migrations-check:
	KEEL_REQUIRE_DB=1 go test -count=1 -v \
		-run 'TestRepositoryMigrations|TestDiscoverMigrationDirs' ./pg/migrate/

# -o /dev/null because `go build ./...` writes each main package's binary into
# the working directory. This target only needs to know whether it compiles.
.PHONY: build
build:
	go build -o /dev/null ./...

# Run examples/minimal against a throwaway Postgres. There is no run-firebase
# target yet: it would be identical to this one until the auth package lands,
# and a target that does nothing different is worse than a missing one.
EXAMPLE_DB_CONTAINER ?= keel-example-db
EXAMPLE_DB_PORT      ?= 55432
EXAMPLE_DB_URL       ?= postgres://keel:keel@127.0.0.1:$(EXAMPLE_DB_PORT)/keel?sslmode=disable

.PHONY: run-local
run-local:
	@docker rm -f $(EXAMPLE_DB_CONTAINER) >/dev/null 2>&1 || true
	docker run -d --rm --name $(EXAMPLE_DB_CONTAINER) \
		-e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel \
		-p 127.0.0.1:$(EXAMPLE_DB_PORT):5432 postgres:16-alpine >/dev/null
	@printf 'waiting for postgres'
	@until docker exec $(EXAMPLE_DB_CONTAINER) pg_isready -U keel >/dev/null 2>&1; do \
		printf '.'; sleep 1; \
	done; echo ' ready'
	@echo 'serving on :8080 — ctrl-c to stop, the database is removed on exit'
	@trap 'docker rm -f $(EXAMPLE_DB_CONTAINER) >/dev/null 2>&1 || true' EXIT INT TERM; \
		DATABASE_URL='$(EXAMPLE_DB_URL)' go run ./examples/minimal
