#!/usr/bin/env bash
# gofmt, go vet and go test on every Go package. The ledger's tests need PostgreSQL: a
# throwaway server in the pinned image (POSTGRES_IMAGE, from the Makefile), on a random
# local port, with a random password that is never printed, removed when the tests end.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
: "${POSTGRES_IMAGE:?POSTGRES_IMAGE is not set; run make check-go}"

files="$(gofmt -l cmd internal)"
if [ -n "$files" ]; then
  echo "check-go: not formatted with gofmt:"
  echo "$files"
  exit 1
fi
go vet ./...

name="settled-check-go-$$"
envfile="$(mktemp)"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -f "$envfile"
}
trap cleanup EXIT
password="$(openssl rand -hex 24)"
# The password goes to the container through a file readable by the owner only, not
# on a command line where ps would show it.
printf 'POSTGRES_USER=admin\nPOSTGRES_PASSWORD=%s\n' "$password" > "$envfile"
chmod 600 "$envfile"
docker run -d --name "$name" --env-file "$envfile" -p 127.0.0.1::5432 "$POSTGRES_IMAGE" >/dev/null
port="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
# The image initializes the database with a server that listens on no TCP port, then
# restarts it: ready over TCP is ready for the tests.
for _ in $(seq 1 60); do
  if docker exec "$name" pg_isready -q -h 127.0.0.1 -U admin 2>/dev/null; then break; fi
  sleep 1
done
docker exec "$name" pg_isready -q -h 127.0.0.1 -U admin || { echo "check-go: PostgreSQL did not start"; docker logs "$name" 2>&1 | tail -20; exit 1; }

SETTLED_TEST_POSTGRES="postgres://admin:${password}@127.0.0.1:${port}/postgres?sslmode=disable" go test -count=1 ./...
echo "check-go: ok"
