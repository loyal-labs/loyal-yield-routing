#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: scripts/referrals/local-stack.sh db|web|stop|env

A disposable local stack for the referral routes: Postgres 17 with the
databases app and yield, and loyal-web serving the Go API against them.

  db    init and start Postgres, drop and recreate app and yield, apply
        both schemas. Yield runs loyal-migrate in three steps: the chain
        through 0012, the historical app snapshots in
        web/apps/web/src/lib/yield-optimization/migrations (they create
        user_yield_positions, which later chain files alter), then the
        rest of the chain. 0071 is recorded in the ledger without running:
        it asserts a production Backyard row that an empty database lacks.
        App is drizzle-kit push of web/packages/db-core: the drizzle
        migration chain does not apply to an empty database.
  web   run loyal-web on LOYAL_WEB_ADDRESS (default 127.0.0.1:8787)
        against the local databases. Every credential lives in
        $REFERRALS_LOCAL_DIR/credentials, AUTH_JWT_SECRET included, a
        random value made once. Unserved /api paths proxy to a closed
        local port, never to production.
  stop  stop Postgres.
  env   print the local APP_DATABASE_URL and YIELD_DATABASE_URL, always
        127.0.0.1 on REFERRALS_PG_PORT. seed-local.sh and session-local.sh
        take their URLs from here and from nowhere else.

Then:
  scripts/referrals/seed-local.sh
  curl -H "Cookie: $(scripts/referrals/session-local.sh alice)" http://127.0.0.1:8787/api/referrals

Env: REFERRALS_LOCAL_DIR (default /tmp/referrals-local),
REFERRALS_PG_PORT (default 55491), LOYAL_WEB_ADDRESS.
EOF
}

root=$(cd "$(dirname "$0")/../.." && pwd)
dir=${REFERRALS_LOCAL_DIR:-/tmp/referrals-local}
port=${REFERRALS_PG_PORT:-55491}
case $port in *[!0-9]* | "") echo "REFERRALS_PG_PORT must be a port number" >&2; exit 2 ;; esac
pgbin=$(brew --prefix postgresql@17 2>/dev/null)/bin
url() { echo "postgres://postgres@127.0.0.1:$port/$1"; }
psql_on() { db=$1; shift; "$pgbin/psql" -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U postgres -d "$db" "$@"; }

start_postgres() {
  mkdir -p "$dir"
  [ -d "$dir/pg" ] || "$pgbin/initdb" -D "$dir/pg" -U postgres --auth=trust -E UTF8 >/dev/null
  "$pgbin/pg_ctl" -D "$dir/pg" status >/dev/null 2>&1 ||
    "$pgbin/pg_ctl" -D "$dir/pg" -o "-p $port -k $dir" -l "$dir/postgres.log" -w start >/dev/null
}

need_web_modules() {
  [ -d "$root/web/node_modules" ] || { echo "web/node_modules is missing: run bun install in web/" >&2; exit 1; }
}

build() {
  mkdir -p "$dir"
  (cd "$root/go/workers" && GOTOOLCHAIN=local /usr/bin/lockf /Users/user/xstock-scratch/build.lock nice -n 15 \
    env GOMAXPROCS=4 GOMEMLIMIT=3GiB GOFLAGS=-p=4 go build -o "$dir/$1" "./cmd/$1")
}

migrate() {
  build loyal-migrate
  NEON_DATABASE_URL=$(url yield) "$dir/loyal-migrate" -db yield "$@"
}

schemas() {
  psql_on postgres -c "DROP DATABASE IF EXISTS app WITH (FORCE)" -c "DROP DATABASE IF EXISTS yield WITH (FORCE)" \
    -c "CREATE DATABASE app" -c "CREATE DATABASE yield"
  migrate -to 12 up | tail -1
  for snapshot in "$root"/web/apps/web/src/lib/yield-optimization/migrations/0*.sql; do
    psql_on yield -f "$snapshot" >/dev/null 2>"$dir/snapshot.err" || { cat "$dir/snapshot.err" >&2; exit 1; }
  done
  migrate -to 70 up | tail -1
  skipped="$root/migrations/yield/0071_backyard_rwa_phase1_activation.sql"
  psql_on yield -c "INSERT INTO loyal_yield.schema_migrations (version, name, checksum)
    VALUES (71, 'backyard_rwa_phase1_activation', '$(shasum -a 256 "$skipped" | cut -d' ' -f1)')"
  migrate up | tail -1
  (cd "$root/web/apps/telegram" && DATABASE_URL=$(url app) bunx drizzle-kit push --force >"$dir/drizzle.log" 2>&1) ||
    { tail -20 "$dir/drizzle.log" >&2; exit 1; }
  echo "app and yield ready on 127.0.0.1:$port"
}

web() {
  creds="$dir/credentials"
  mkdir -p "$creds"
  chmod 700 "$creds"
  [ -s "$creds/AUTH_JWT_SECRET" ] || openssl rand -hex 32 >"$creds/AUTH_JWT_SECRET"
  url app >"$creds/WEB_APP_DATABASE_URL"
  url yield >"$creds/WEB_YIELD_DATABASE_URL"
  echo "http://127.0.0.1:8899" >"$creds/WEB_SOLANA_RPC_URL"
  build loyal-web
  CREDENTIALS_DIRECTORY=$creds LOYAL_WEB_ADDRESS=${LOYAL_WEB_ADDRESS:-127.0.0.1:8787} \
    LOYAL_WEB_UPSTREAM=http://127.0.0.1:9 exec "$dir/loyal-web"
}

case ${1:-} in
  db) need_web_modules && start_postgres && schemas ;;
  web) web ;;
  stop) "$pgbin/pg_ctl" -D "$dir/pg" stop ;;
  env) echo "export APP_DATABASE_URL=$(url app) YIELD_DATABASE_URL=$(url yield)" ;;
  *) usage; [ "${1:-}" = "--help" ] ;;
esac
