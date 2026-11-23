.PHONY: test test-all test-root test-codec test-remote test-remote-backends integration-test test-sqlite test-postgres \
	test-postgres-docker postgres-up postgres-down postgres-logs proto

GO ?= go
BIN := $(CURDIR)/.bin

test: test-root test-codec test-remote
	@echo "note: this does not run the SQLite or Postgres modules."
	@echo "      make test-sqlite"
	@echo "      make test-postgres          # Postgres already listening on :5432"
	@echo "      make test-postgres-docker   # start a temporary Postgres, test, then remove it"

test-all: test integration-test

test-root:
	$(GO) test ./...

test-codec:
	cd codec && $(GO) test ./...

test-remote:
	cd remote && $(GO) test ./...

# The remote client against SQLite, and against Postgres when POSTGRES_CONN is set.
test-remote-backends:
	cd remote/conformance && $(GO) test ./...

# Regenerates remote/gen from proto/. The generators are pinned in tools/go.mod
# and installed into .bin, so every machine produces the same code.
proto:
	cd tools && GOWORK=off GOBIN=$(BIN) $(GO) install tool
	PATH="$(BIN):$$PATH" $(BIN)/buf lint
	PATH="$(BIN):$$PATH" $(BIN)/buf generate

integration-test: test-sqlite test-postgres test-remote-backends

test-sqlite:
	cd store/sqlite && $(GO) test ./...

test-postgres:
	cd store/postgres && POSTGRES_REQUIRE=1 $(GO) test ./...

# A disposable Postgres on :55432, so a database already on :5432 is left alone.
test-postgres-docker:
	@docker rm -f zestor-test-pg >/dev/null 2>&1 || true
	@docker run -d --rm --name zestor-test-pg -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:16-alpine
	@bash -c 'trap "docker stop zestor-test-pg >/dev/null" EXIT; \
		for i in $$(seq 1 30); do docker exec zestor-test-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; \
		cd store/postgres && POSTGRES_CONN="postgresql://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable" POSTGRES_REQUIRE=1 $(GO) test ./... && \
		cd ../../remote/conformance && POSTGRES_CONN="postgresql://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable" POSTGRES_REQUIRE=1 $(GO) test ./...'

postgres-up:
	docker compose up -d postgres

postgres-down:
	docker compose down

postgres-logs:
	docker compose logs -f postgres
