# Loyal referrals v1: state, next step, open items

Status as of 2026-11-05. The design, plan and build trail live outside the repo in `~/loyal/artifacts/loyal-referrals/docs/` (`design.md`, `plan.md`, `trail.md`). This file is the program's handoff.

## What shipped

Referral codes, invite links, binding, the Invite page and the Earn MAX cash-flow ledger. All of it is on main (R1 #666 API, R2 #673 web capture, R3 #674 web Invite, R4 #671 mobile, R5 #672 cash-flow ledger, R6 #682 hardening, #698 gate, #699 invite-code capture). loyal-web 420826429 carried it to production on 2026-11-04.

- The reward is a later yield-share program for the inviter only. v1 has no money in it (`design.md`).
- Every referral route answers 403 `not_open` unless the signed-in wallet is in `LOYAL_TEST_WALLETS`. The gate is `go/workers/internal/webapp/test_wallets.go`. `LOYAL_TEST_WALLETS=*` opens it to every wallet; that is the launch lever and it is the setting on the web host since the 2026-11-04 launch (`deploy/hetzner/README.md`, `LOYAL_TEST_WALLETS` row).
- A pending invite code survives a sign-in hand-off in the `loyal_ref` cookie (`Domain=askloyal.com`, 30 days). `/i/CODE` redirects to `/app?ref=CODE`.
- The Earn MAX cash-flow ledger is forward-only, with no backfill. Its writer is the observer.
- Grants for the referral tables are in `deploy/hetzner/README.md` under "Earn family grants".

## Next step

Nothing is blocked. The referral UI is open to all wallets, so the next work is the open items below and the yield-share program, which the owner has not scheduled.

## Open

- Boot-time `?ref=` capture without a reload.
- Mobile boot never retries a lost `privy/complete` (the trunk issue, not referral code).
- iOS and Play install referrer are untested.
- The mobile native referral UI still sits behind a manual MMKV flag (`loyal.referrals` = `1`) with no test-wallet auto-unlock.
- The non-test 403 is proven by a router test only, not on production, and is moot while `LOYAL_TEST_WALLETS=*`.
- The first `/api/referrals` call after a restart once returned 503 `app_users does not exist`. The cause is unknown; it did not repeat.
- The yield-share program must key bindings to the Privy user, not the wallet (`design.md`, "Later", item 5).
