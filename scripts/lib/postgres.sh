#!/usr/bin/env bash
# Sourced by the checks that need a throwaway PostgreSQL. start_postgres runs one in
# the image the devnet's ledger runs (infra/bridge/compose.yaml) as container $1, on a
# random local port, with a random admin password that reaches the container through a
# file readable by the owner only and is never printed, and waits until it takes
# connections over TCP. It sets PG_PORT and PG_PASSWORD; the caller removes the
# container.

start_postgres() {
  local name="$1" envfile image
  image="$(sed -n 's/^ *image: *\(postgres:[^ ]*@sha256:[0-9a-f]\{64\}\) *$/\1/p' infra/bridge/compose.yaml)"
  [ "$(printf '%s\n' "$image" | grep -c .)" = 1 ] || { echo "postgres: no single pinned PostgreSQL image in infra/bridge/compose.yaml"; return 1; }
  # shellcheck disable=SC2034 # PG_PASSWORD and PG_PORT are for the caller
  PG_PASSWORD="$(openssl rand -hex 24)"
  envfile="$(mktemp)"
  chmod 600 "$envfile"
  printf 'POSTGRES_USER=admin\nPOSTGRES_DB=ledger\nPOSTGRES_PASSWORD=%s\n' "$PG_PASSWORD" > "$envfile"
  if ! docker run -d --name "$name" --env-file "$envfile" -p 127.0.0.1::5432 "$image" >/dev/null; then
    rm -f "$envfile"
    echo "postgres: the container did not start"
    return 1
  fi
  rm -f "$envfile"
  # shellcheck disable=SC2034
  PG_PORT="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
  # The image initializes the database with a server that listens on no TCP port, then
  # restarts it: ready over TCP is ready for use.
  for _ in $(seq 1 60); do
    if docker exec "$name" pg_isready -q -h 127.0.0.1 -U admin 2>/dev/null; then return 0; fi
    sleep 1
  done
  echo "postgres: not ready after 60 s"
  docker logs "$name" 2>&1 | tail -20
  return 1
}
