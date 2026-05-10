.PHONY: test test-all test-root test-codec integration-test test-sqlite test-postgres \
	postgres-up postgres-down postgres-logs

GO ?= go

test: test-root test-codec

test-all: test integration-test

test-root:
	$(GO) test ./...

test-codec:
	cd codec && $(GO) test ./...

integration-test: test-sqlite test-postgres

test-sqlite:
	cd store/sqlite && $(GO) test ./...

test-postgres:
	cd store/postgres && POSTGRES_REQUIRE=1 $(GO) test ./...

postgres-up:
	docker compose up -d postgres

postgres-down:
	docker compose down

postgres-logs:
	docker compose logs -f postgres
