# ASK-2173 Earn LaserStream Reconciliation Plan

Tracking: [ASK-2173](https://linear.app/askloyal/issue/ASK-2173/add-earn-accounts-to-laserstream)

Pull requests:

- routing: [loyal-yield-routing#64](https://github.com/loyal-labs/loyal-yield-routing/pull/64)
- cron removal: [loyal-app#668](https://github.com/loyal-labs/loyal-app/pull/668)

## Goal

Make the existing LaserStream session the one source that wakes Earn
reconciliation. The two Loyal App cron scans are replaced without adding a
worker, another polling loop, a transaction subscription, or an event queue.

```text
confirmed LaserStream account update
  -> find every affected Earn vault
  -> fetch the exact transaction and slot-pinned account proof
  -> apply canonical Earn writes and the replay cursor in one Neon transaction
```

The same process continues to monitor balance-sweep wallet ATAs. Earn updates
never enter the balance-sweep observation/projector path, because that path
turns balance deltas into autodeposit lots.

## Subscription shape

One `SubscribeRequest` contains independent, named account filters:

- `balance_sweep_wallet_atas`;
- `earn_policy_accounts`;
- `earn_vault_accounts`;
- `earn_idle_token_accounts`;
- `earn_obligations`.

There is no transaction filter. An account notification already carries the
transaction signature, which is enough for a targeted confirmed transaction
lookup. There is no reserve fan-out: reserve state is read only when an
affected vault needs proof.

The watch set is rebuilt from durable application identity, onboarding,
position, and policy rows. Address lists are sorted and deduplicated. Any Earn
watch-set change rebuilds the physical LaserStream session from the durable
subscription request. The rebuild starts at the previous watch-set refresh
boundary, then advances that checkpoint after the refreshed set is installed.
That backfills events which landed between refreshes without replaying from the
process-start slot on every later addition. We intentionally do not use the
SDK's live write path because it clears `from_slot` and cannot backfill an event
that landed before the new filter was installed.

## Direct reconciliation boundary

`balance-sweep-ata-monitor::earn_reconciliation` owns the state machine. Its
single `EarnChainReader` interface has two implementations:

- `RpcEarnChainReader` supplies production transaction/account evidence;
- `FixtureEarnChainReader` supplies deterministic evidence to the isolated E2E.

Both readers return the same typed mutation and call the same store method.
One account can affect several vaults, so every affected vault is resolved
before the batch is written. The store applies the batch and advances the
LaserStream replay cursor inside the same SQL transaction. A proof error, RPC
lag, invariant failure, or database failure leaves both canonical state and
cursor unchanged.

No receipt table is needed. Replaying after a lost acknowledgement is safe
because canonical identities are already idempotent: policy account, deposit
signature, position identity, and active vault identity.

## Recovery cases

### Policy-only onboarding

Candidate: onboarding is `route_policy_confirmed`, route/setup accounts are
watched, and no deposit is recorded.

Proof: both accounts exist at or after the update slot, are Squads-owned, and
the update transaction cites the setup policy and wallet signer. The journal's
settings, vault, seeds/accounts, delegated signer, market, and mint are reused.

Write: upsert route/setup policies and the managed vault, then advance
onboarding to `setup_policy_confirmed`.

### Invisible deposit

Candidate: a watched vault, idle, or obligation update carries a confirmed
signature with no canonical deposit row.

Proof: fetch that transaction rather than scanning history, require the target
reserve in its accounts, calculate the wallet/vault raw token debit, validate
the recorded policy/market/mint, and read current idle accounts at or after the
update slot.

Write: select the active onboarding attempt before completed history, insert
the deposit by signature, resolve the active aggregate position by wallet and
vault identity rather than its original reserve, append one holding event,
publish reserve/idle state, and complete only that active attempt. A top-up
after reserve A was rebalanced to reserve B therefore updates the same
aggregate row when it targets B. Replay adds neither principal nor a duplicate
event.

### Full-withdraw cleanup

Candidate: an active position has a recorded full withdrawal at or after its
last confirmed position slot.

Proof is anchored at `minContextSlot = withdrawal_confirmed_slot`:

- watched Kamino obligations have no deposit or borrow;
- watched idle accounts are zero or below the product dust allowance;
- all SPL Token and Token-2022 accounts owned by the vault are enumerated;
- any positive unknown token account blocks cleanup;
- policy accounts are read in the same slot-pinned proof.

`confirm_missed` uses the account-update transaction after policy accounts are
absent. `cleanup_pending` uses the recorded withdrawal while policies remain;
the existing refund path may later reclaim their rent. Both deactivate policies
and the vault, zero reserve/idle rows, and close/zero the active position.

A positive balance is a proven no-op and may advance the cursor because its
later change emits another watched update. RPC below the required slot is an
error and does not advance the cursor.

## Store contract

`loyal-yield-store::apply_direct_earn_reconciliation` applies an ordered list
of `PolicyOnly`, `Deposit`, `Cleanup`, or `Noop` mutations. The replay cursor is
the final statement in that transaction and records only the highest fully
reconciled slot. There are no reconciliation jobs, receipts, leases, or fleet
worker handoffs.

## Verification

The frozen verifier at `verification/smart-account-laserstream/verify.sh`
starts disposable PostgreSQL, applies production routing and Loyal
App-compatible Earn schemas, and sends simulated evidence through the
production reconciliation function.

It proves subscription shape, deterministic active-onboarding policy selection,
policy-only and deposit convergence, cross-reserve aggregate reuse,
positive-balance no-op, failure rollback, minimum-context lag, both cleanup
classes, replay idempotency, advancing watch-set replay checkpoints, absence of
old handoff tables, and zero balance-sweep side effects. It finishes with
focused format, test, compile, and whitespace checks.

```sh
bash verification/smart-account-laserstream/verify.sh \
  --routing-root /private/tmp/loyal-yield-routing-laserstream-implementation \
  --app-root /private/tmp/loyal-app-laserstream-implementation
```

## Rollout

Pre-merge:

- rebase on current routing `main` and preserve its migrations 40 through 45;
- register the LaserStream replay cursor as migration 46 in both registries;
- keep the routing verifier and worker-image checks green;
- treat the rollout as urgent: loyal-app#668 is already merged and its
  production Vercel deployments completed, so the cron fallback is no longer
  available.

Post-merge:

1. publish the routing `laserstream-workers` image;
2. apply routing migration 46;
3. redeploy `loyal-balance-sweep-ata-monitor-staging` with the immutable image;
4. verify all five filters, watch-set rebuild replay, immediate proof-failure
   restarts, replay progress, and canonical reconciliation;
5. redeploy `loyal-balance-sweep-ata-monitor` and repeat those checks.

Services to redeploy are `loyal-balance-sweep-ata-monitor-staging` and
`loyal-balance-sweep-ata-monitor` on Render. The loyal-app cron removal is
already deployed; no additional Loyal App, fleet worker, projector, or Render
service is part of this rollout.
