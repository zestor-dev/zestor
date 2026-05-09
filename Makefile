.PHONY: test test-all test-root test-codec integration-test test-sqlite test-postgres

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
	cd store/postgres && $(GO) test ./...
