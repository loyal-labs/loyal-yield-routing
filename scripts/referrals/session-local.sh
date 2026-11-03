#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: scripts/referrals/session-local.sh <fixture>

Prints a Cookie header value, loyal_wallet_session=<jwt>, that signs the
fixture in to the loyal-web of scripts/referrals/local-stack.sh. <fixture>
is a seed-local.sh name (alice, bob, carol, dan, erin, frank) or any email
in the local app_users.

The token is the web wallet session the Next sign-in issues, minted by the
repo's own issueAuthSessionToken (web/apps/web/src/features/identity/server/
session-token.ts) with the local stack's random AUTH_JWT_SECRET. loyal-web
verifies it as any session. Privy sign-in cannot reach a fixture: Privy
creates its own wallet, and its dev app needs secrets a lane never reads.

  curl -H "Cookie: $(scripts/referrals/session-local.sh alice)" http://127.0.0.1:8787/api/referrals

In a browser on 127.0.0.1:8787, set the same cookie for that origin.

Env: REFERRALS_LOCAL_DIR (default /tmp/referrals-local), APP_DATABASE_URL.
EOF
}

case ${1:-} in
  "" | --help) usage; [ "${1:-}" = "--help" ]; exit ;;
  *@*) email=$1 ;;
  *) email=$1@referrals.test ;;
esac

root=$(cd "$(dirname "$0")/../.." && pwd)
dir=${REFERRALS_LOCAL_DIR:-/tmp/referrals-local}
app=${APP_DATABASE_URL:-postgres://postgres@127.0.0.1:${REFERRALS_PG_PORT:-55491}/app}
secret_file=$dir/credentials/AUTH_JWT_SECRET
[ -s "$secret_file" ] || { echo "no $secret_file: run scripts/referrals/local-stack.sh web first" >&2; exit 1; }

wallet=$(psql -X -A -t -v ON_ERROR_STOP=1 -v email="$email" "$app" <<'SQL'
SELECT subject_address FROM app_users WHERE email = :'email';
SQL
)
[ -n "$wallet" ] || { echo "no app user with email $email" >&2; exit 1; }

cd "$root/web"
[ -f packages/auth-core/dist/index.js ] || (cd packages/auth-core && bun run build >/dev/null)
SESSION_WALLET=$wallet SESSION_SECRET_FILE=$secret_file exec bun --conditions=react-server -e '
const { issueAuthSessionToken } = await import("./apps/web/src/features/identity/server/session-token.ts");
const wallet = process.env.SESSION_WALLET;
const secret = (await Bun.file(process.env.SESSION_SECRET_FILE).text()).trim();
const token = await issueAuthSessionToken(
  { authMethod: "wallet", provider: "solana", subjectAddress: wallet, displayAddress: wallet, walletAddress: wallet },
  secret,
  24 * 60 * 60,
);
console.log(`loyal_wallet_session=${token}`);
'
