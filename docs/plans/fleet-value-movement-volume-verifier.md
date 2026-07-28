# Fleet Value Movement and Volume Verifier

This is the fixed verifier for the production fleet routing/rebalance rate. It
checks outcomes, not whether workers are merely running. Overall PASS requires
the isolated database proof, new finalized production movement, updated volume,
and value-weighted coverage of the largest eligible vaults. Keep running the
plan, implementation, and verifier loop until those outcomes are true.

"Routing rate" means the rate at which economically eligible capital progresses
from a durable opportunity to a reconciled on-chain movement. "Volume" means
the sum of `principal_usd_micros` and `amount_raw` over unique reconciled signed
submissions. Planned, signed, expired, failed, or duplicated rows contribute
zero volume.

Do not weaken this verifier to match an implementation. If no economically
eligible production route exists, report `BLOCKED_NO_ELIGIBLE_ROUTE`; do not
report PASS from old movements or dry-run evidence.

## Required 1: ALT mutation exclusion

In the isolated verifier, force both orderings of the July 16 race on one
physical lookup table:

1. An active route-resolution or prepared-transaction lease prevents an
   `extend`, `rollover`, `deactivate`, or `close` operation from being signed or
   granted a broadcast permit.
2. A nonterminal mutating operation prevents a new route-resolution or
   prepared-transaction lease from being created.
3. `verify` remains nonmutating and may coexist with usage leases.

PASS only if no test ordering can produce a signed route whose recorded ALT
mutation epoch changes before broadcast.

## Required 2: poison-row isolation and safe recovery

Seed two signed submissions: one with stale or expired ALT protection and one
with valid protection. PASS only if:

- the valid submission is claimable and can advance independently;
- the invalid submission is never returned to a broadcast-capable lane;
- an expired, never-broadcast submission can be claimed by a recovery-only lane
  without requiring currently selectable ALT protection;
- recovery requires an expired blockhash and absent signature history, marks
  the row `expired`, and releases its conflict, capacity, ALT, decision, and
  opportunity locks;
- an attempted broadcast still requires finalized route-effect absence proof;
- one invalid row cannot roll back attempts or claims for another row.

Renewing expired ALT leases or broadcasting old signed bytes is an automatic
FAIL.

## Required 3: exact volume accounting

The isolated verifier must take a volume snapshot, reconcile one fixture, and
take another snapshot. PASS only if:

- reconciled movement count increases by exactly one;
- reconciled `principal_usd_micros` increases by exactly the fixture principal;
- reconciled `amount_raw` increases by exactly the fixture amount;
- replaying reconciliation does not increment any total again;
- signed, submitted, expired, failed, and effect-ambiguous fixtures add zero;
- every counted row has one unique submission, decision, opportunity, and
  transaction signature.

`amount_raw` comes from the executed decision. The opportunity retains the
immutable published amount and principal used for discovery and ranking. For a
same-mint route, ordinary positive reserve accrual may increase the final
decision amount by at most the existing 100 ppm queue bound. The verifier must
prove the planner plan exactly matches the published amount, the signed decision
plan exactly matches the executed amount, and the delta stays within that bound.

The production evidence JSON must expose the current totals and baseline delta,
including count, raw amount, principal USD micros, and newest reconciliation
time.

Fresh market APYs and target-capacity telemetry must revalidate admission before
signing, but they must not replace the planner-published APYs and edge in the
durable decision identity. The isolated verifier must prove that refreshed
economics cannot bind as the published decision and that the exact published
economics can still bind after successful fresh revalidation.

The live market-plane sample may retry for at most six one-second attempts so
the one-second confirmed refresher can catch an event-driven pointer advance.
PASS still requires one attempt with exact active-reserve coverage and every
identity, commitment, source, freshness, and observation-floor invariant true.

## Required 4: live movement and routing rate

Capture a source-bound baseline before rollout. After rollout, wait for an
economically eligible route or use one explicitly approved bounded canary. Run
the production evidence collector with the rollout cutover and baseline.

PASS only if at least one route created after cutover:

- advances `signed -> submitted -> confirmed -> reconciliation_pending -> reconciled`;
- has `broadcast_count > 0` and a successful finalized Solana signature;
- has ordered lifecycle timestamps and slots;
- has source and target snapshots tied to the same vault and route;
- proves source value decreased and target value increased by the routed amount;
- is reflected exactly once in the reconciled volume delta;
- submits within 2 minutes of signing and reconciles within 15 minutes.

The decisive latency evidence is computed over the same bounded, post-cutover
reconciled submissions used for movement and volume proof. Latest-epoch queue
latencies remain diagnostic only: a fresh complete epoch may correctly contain
zero opportunities after the eligible fleet is optimized, so its null latency
percentiles cannot invalidate already bounded movement evidence.

The queue must have no zero-broadcast signed row older than 5 minutes, no
confirmation batch invariant loop, and no effect-ambiguous route. Worker
heartbeats without this movement evidence are FAIL.

## Required 5: largest-account optimization

At one fresh, non-expired optimizer epoch, rank active policy-backed vaults by
routeable USD principal from `vault_reserve_positions_current`. The evidence
must record the top ten, or every eligible vault when fewer than ten exist, and
must distinguish:

- already optimal: current reserve is the best eligible positive-value reserve;
- moved: a post-cutover reconciled route placed the vault in that reserve;
- no positive edge: no eligible target has a positive net edge;
- blocked: any other state, including stale data, ALT coverage, capacity,
  signing, confirmation, or reconciliation failure.

PASS only if every top-three vault and at least 90% of principal across the
ranked cohort is either already optimal, moved, or has no positive edge. A
vault with a positive safe route may not be excluded from the denominator. Any
`blocked` top-three vault is FAIL and must drive another plan/do/verify cycle.

For every `moved` vault, its unique reconciled submission must be included in
the post-cutover volume delta. This section cannot pass from historical
movements predating the rollout.

## Required 6: commands and artifact

Run:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run fleet:verify -- --isolated-database'
```

```sh
cargo check -p loyal-yield-orchestrator \
  --bin fleet-route-confirmer \
  --bin route-lookup-table-provisioner \
  --bin fleet-orchestration-verifier \
  --bin fleet-orchestration-production-evidence
```

Capture a pre-rollout artifact:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run fleet:production-evidence -- --output /tmp/fleet-volume-baseline.json'
```

Then capture post-rollout evidence using the actual UTC cutover:

```sh
op run --env-file=.env.1password -- sh -c \
  'bun run fleet:production-evidence -- \
    --cutover-at <RFC3339> \
    --baseline /tmp/fleet-volume-baseline.json \
    --output /tmp/fleet-volume-after.json'
```

The final artifact must give PASS/FAIL for every required section and an overall
PASS only when all six sections pass. It must include identifiers and measured
totals but no URLs, secrets, signer material, or signed transaction bytes.
