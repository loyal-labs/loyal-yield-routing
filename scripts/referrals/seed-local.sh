#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: scripts/referrals/seed-local.sh [--friends N]

Writes the referral fixtures into local App and Yield databases. A rerun
deletes every fixture row first, so each run starts from the same state.

  alice  inviter, signed up 40 days ago, code A11CE00
  bob    bound to alice, $60 deposited (principal_amount_raw 60000000)
  carol  bound to alice, never deposited
  dan    bound to alice, deposited $60 and withdrew all of it: principal 0,
         status still active, as the observer's withdrawal writes it
  erin   signed up 8 days ago, unbound
  frank  signed up today, unbound, code FRANK00
  gina   inviter, signed up 40 days ago, code G1NA000, with two bound friends
         that exercise the masked label:
  h      email h@referrals.test (one-character local part), $25 deposited
  ivan   no email (wallet-only), $5 deposited

Fixture emails are <name>@referrals.test, except ivan's, which is NULL.

  --friends N  also bind N extra friends to alice, each with a $10 deposit

The databases are the ones of scripts/referrals/local-stack.sh: 127.0.0.1 on
REFERRALS_PG_PORT (default 55491). No database URL is read from the
environment, so nothing here can reach another host.
EOF
}

friends=0
case ${1:-} in
  --friends) friends=${2:?--friends takes a count} ;;
  "") ;;
  *) usage; [ "$1" = "--help" ]; exit ;;
esac
case $friends in *[!0-9]* | "") echo "--friends takes a whole number" >&2; exit 2 ;; esac

unset PGHOST PGHOSTADDR PGSERVICE PGSERVICEFILE PGDATABASE PGPORT
eval "$("$(dirname "$0")/local-stack.sh" env)"
app=$APP_DATABASE_URL
yield=$YIELD_DATABASE_URL

run() { psql -X -q -v ON_ERROR_STOP=1 -v friends="$friends" "$1"; }

fixtures="('alice', 'ESPHGrHg6YM1asBqFRmvrwUjrEfeeU5XEGn4C1L8aPWg', 40, 'alice@referrals.test'),
  ('bob', 'DwLxEDVMiZHJabtRqsFTqFUHQYRx85bX5aEcrLogCZBN', 30, 'bob@referrals.test'),
  ('carol', '563GFSb2Kx5qTFNoDzo5oSe9gwpMxcaDxdfWRpebxMpe', 20, 'carol@referrals.test'),
  ('dan', 'Bba92cU9fmfct7YC2rx4xAbSrUUza6JDwc6VqNAry81U', 10, 'dan@referrals.test'),
  ('erin', '6gSw18yCRKfSdEozHgcbBaBvtTXNWjEQ7esNoUcLDieJ', 8, 'erin@referrals.test'),
  ('frank', '3vKkNzPRDQAiYee6WxXkxs88m9buaHpSPTPuYcbjT6XP', 0, 'frank@referrals.test'),
  ('gina', 'Eq7tYdBcXyUJLbB8waw8G2UzAF6K8xmLwUe5jap2g1pq', 40, 'gina@referrals.test'),
  ('h', '69wFnDXsK7u9oBV9YiMQbbFskrBdvqddwTN7UFRPy6Hq', 20, 'h@referrals.test'),
  ('ivan', '7h9Tu7eGNq2u3vqthQ8Af6mZ9V3fq9KM9eXus1DXPLmq', 15, NULL)"

run "$app" <<SQL
DELETE FROM app_users
WHERE email LIKE '%@referrals.test' OR subject_address IN (SELECT wallet FROM (VALUES $fixtures) f (name, wallet, age, email));
INSERT INTO app_users (id, provider, subject_address, email, created_at, updated_at)
SELECT md5('referrals-fixture-' || name)::uuid, 'solana', wallet, email,
       now() - make_interval(days => age), now()
FROM (VALUES $fixtures) f (name, wallet, age, email);
INSERT INTO app_users (id, provider, subject_address, email, created_at, updated_at)
SELECT md5('referrals-extra-' || i)::uuid, 'solana', 'RefExtra' || lpad(i::text, 6, '0'),
       'friend' || lpad(i::text, 6, '0') || '@referrals.test', now() - interval '20 days', now()
FROM generate_series(1, :friends) i;
SQL

run "$yield" <<SQL
CREATE TEMP TABLE fixture AS
SELECT name, wallet, md5('referrals-fixture-' || name)::uuid AS id, now() - make_interval(days => age) AS signed_up_at
FROM (VALUES $fixtures) f (name, wallet, age, email);
DELETE FROM loyal_yield.referral_bindings
WHERE referee_wallet LIKE 'RefExtra%'
   OR referee_app_user_id IN (SELECT id FROM fixture)
   OR referrer_app_user_id IN (SELECT id FROM fixture);
DELETE FROM loyal_yield.referral_codes WHERE app_user_id IN (SELECT id FROM fixture);
DELETE FROM loyal_yield.user_yield_positions
WHERE wallet_address LIKE 'RefExtra%' OR wallet_address IN (SELECT wallet FROM fixture);

INSERT INTO loyal_yield.referral_codes (app_user_id, code)
SELECT id, code FROM fixture JOIN (VALUES ('alice', 'A11CE00'), ('frank', 'FRANK00'), ('gina', 'G1NA000')) c (name, code) USING (name);

CREATE TEMP TABLE friend AS
SELECT wallet, id, signed_up_at, principal, inviter FROM fixture
JOIN (VALUES ('bob', 60000000, 'alice'), ('carol', NULL, 'alice'), ('dan', 0, 'alice'),
             ('h', 25000000, 'gina'), ('ivan', 5000000, 'gina')) d (name, principal, inviter) USING (name)
UNION ALL
SELECT 'RefExtra' || lpad(i::text, 6, '0'), md5('referrals-extra-' || i)::uuid, now() - interval '20 days', 10000000, 'alice'
FROM generate_series(1, :friends) i;

INSERT INTO loyal_yield.referral_bindings (referee_app_user_id, referee_wallet, referrer_app_user_id, source, referee_signed_up_at, bound_at)
SELECT f.id, f.wallet, a.id, 'link', f.signed_up_at, f.signed_up_at + interval '1 hour'
FROM friend f JOIN fixture a ON a.name = f.inviter;

INSERT INTO loyal_yield.user_yield_positions (
    wallet_address, smart_account_address, settings, vault_index, vault_pubkey, policy_id, policy_account,
    policy_seed, initial_reserve, initial_liquidity_mint, deposit_mint, principal_amount_raw,
    first_deposit_signature, last_deposit_signature, last_confirmed_slot, status, created_at, updated_at,
    current_reserve, current_liquidity_mint, current_amount_raw, current_observed_slot, current_observed_at)
SELECT wallet, 'vault-' || wallet, 'settings-' || wallet, 0, 'vault-' || wallet, 0, 'policy-' || wallet,
       0, 'reserve', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', principal,
       'deposit-' || wallet, 'deposit-' || wallet, 1, 'active', signed_up_at + interval '2 hours', now(),
       'reserve', 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', principal, 1, now()
FROM friend WHERE principal IS NOT NULL;
SQL

psql -X -A -F ' ' -t "$app" -c "SELECT f.name, u.id, coalesce(u.email, '-'), u.subject_address
  FROM (VALUES $fixtures) f (name, wallet, age, email) JOIN app_users u ON u.subject_address = f.wallet ORDER BY u.created_at"
echo "alice code A11CE00, frank code FRANK00, gina code G1NA000, $friends extra friends"
