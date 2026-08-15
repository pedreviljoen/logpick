# logpick development tasks.

_default:
    @just --list

# Build the binary.
build:
    go build ./cmd/logpick

# Run the unit test suite.
test:
    go test ./...

# Run the suite under the race detector. Not optional, see DESIGN.md 13.2.
race:
    go test -race ./...

# Lint.
lint:
    golangci-lint run

# Drive the whole UI against the fixture corpus, no network.
run-mock:
    go run ./cmd/logpick --mock ./internal/transport/testdata/fixtures

# Format.
fmt:
    gofmt -s -w .
    goimports -w .
