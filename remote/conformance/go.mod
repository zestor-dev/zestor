// Test-only module, built through go.work: it runs the shared suite through
// the remote client against the SQLite and Postgres backends, without making
// the remote module depend on either driver.
module github.com/zestor-dev/zestor/remote/conformance

go 1.25
