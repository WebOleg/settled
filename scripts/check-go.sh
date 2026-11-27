#!/usr/bin/env bash
# gofmt, go vet and go test on every Go package. The ledger's tests need PostgreSQL: a
# throwaway server in the pinned image (POSTGRES_IMAGE, from the Makefile), removed
# when the tests end (scripts/lib/postgres.sh).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/postgres.sh
. scripts/lib/postgres.sh

files="$(gofmt -l cmd internal)"
if [ -n "$files" ]; then
  echo "check-go: not formatted with gofmt:"
  echo "$files"
  exit 1
fi
go vet ./...

name="settled-check-go-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
start_postgres "$name"
SETTLED_TEST_POSTGRES="postgres://admin:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/postgres?sslmode=disable" go test -count=1 ./...
echo "check-go: ok"
