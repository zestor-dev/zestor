.PHONY: test test-all test-root test-codec integration-test test-sqlite

GO ?= go

test: test-root test-codec

test-all: test integration-test

test-root:
	$(GO) test ./...

test-codec:
	cd codec && $(GO) test ./...

integration-test: test-sqlite

test-sqlite:
	cd store/sqlite && $(GO) test ./...
