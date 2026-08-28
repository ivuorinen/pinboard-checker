# Project automation for pinboard-checker
set shell := ["bash", "-cu"]

BINARY := "pinboard-checker"

# Default: show help
default:
    @just --list

# Format Go code
format:
    gofmt -s -w .

# Lint Go code
lint:
    golangci-lint run

# Run all tests. -race is not optional: the whole program is a worker pool
# sharing one mutable Statistics, so a race introduced there must fail.
test:
    go test -race ./...

# Run tests with coverage; writes coverage.out and the HTML report
coverage:
    go test -race -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1
    go tool cover -html=coverage.out -o coverage.html

# Build the binary
build:
    go build -o {{ BINARY }} .

# Run GoReleaser (dry-run by default)
release:
    goreleaser release --clean --skip=publish --snapshot

# Run GoReleaser for an actual release (requires GITHUB_TOKEN)
release-publish:
    goreleaser release --clean

# Regenerate THIRD_PARTY_NOTICES.md (required by the BSD-3 deps)
notices:
    ./scripts/gen-notices.sh

# Update Go modules
tidy:
    go mod tidy

# Clean build artifacts
clean:
    rm -rf {{ BINARY }} dist/ coverage*
