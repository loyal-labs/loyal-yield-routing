# Voltr / Earn MAX protection release

Prepared against integrated code commit `9448780329e18868e1a32c1d61553d6bcf5ec88d`, including current main `aa1ba4267`. Local verification and independent accounting review passed. Production rollout still requires the operational gates below; monitoring verification alone does not approve application deployment.

## Behavior and limits

- Earn MAX uses RWA vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`. Looping vault `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1` is the distinct Trustful route. Its ordinary withdrawal cost verifier is unsupported and holds; mandatory risk, term-maturity and operator reductions retain priority.
- NAV keeps the existing independent full-holding conversion-cost definition: marked assets and custody, less accrued debt and exit liabilities. The change requires fresh, exact full-size, scoped quote evidence for certification; missing coverage holds report-bearing and optional work while retaining actual holdings, debt and LTV for risk decisions. Quotes expire after 30 seconds and the existing slot window. A changed book cannot reuse a certificate. Financed or joint liquidation remains unqualified; this release does not redefine NAV to claim it.
- Ordinary withdrawal funding requires immutable owner-signed consent for the actual request generation, approved USDC minimum and bounded user-paid SOL fees. Missing, expired, conflicting or unverifiable consent holds new ordinary work. Widening requires a new signature. Consent expiry gates admission only: already reserved exact signed wires remain budgeted and may land until their blockhash expires. Cancellation must reconcile the on-chain receipt; a replacement request has a new generation.
- The updated official web/mobile claim atomically fences the generation/policy seed, verifies receipt economics, removes old authority USDC from the payout calculation, closes/recreates its ATA, claims and transfers the approved minimum through a temporary owner-only policy. Under-floor execution rolls back. Remaining proceeds stay user-owned and have a separate recovery action. Protected batch submission is rejected. This protects updated official flows; owners and old clients can still use public protocol entrypoints outside that flow. Pooled idle is not reserved for one user, and full estimated payout liquidity is still required.
- Cost reservations are accounting/admission records, not segregated user cash or a restored global spending cap. Pool economic expense is allocated across participating escrow LP and other holders; incremental cost excludes allowances already in NAV. Delegate-paid SOL network fees are protocol expense, audited separately in lamports and, when known, USDC. They do not consume the user's claim-fee allowance. User-wallet claim fees have their own strict generation-wide budget, exact signed-wire attempts and finalized failed-transaction charges. Unknown conversion is explicit, never silently zero.
- Emergency reductions preserve risk priority. Their pool impact does not widen ordinary consent or imply the user accepted a lower claim. A request can remain price-blocked afterward. No ETA, unlimited retries, all-fees guarantee or notification-delivery guarantee is established.

See [Earn MAX flow details](../../web/apps/web/src/features/earn-max/README.md), [worker ownership/recovery facts](facts.md), and [implementation plan](../plans/vault-integrity-and-net-return.md).

## Required release gates

Root's final integrated checks are pending. The merged-tree worker verifier passed format, vet, all offline package tests and four binary builds. Race detection passed for Backyard, Earn MAX and the web API. Independent PostgreSQL tests passed all 14 withdrawal-cost cases plus consent, claim-attempt and status tests with no skips, including the actual lifecycle constraint. The 31 wallet/wire/cost-display contract tests, SDK typecheck, focused mobile typecheck and scoped web/mobile lint passed. The focused web source comparison has four unchanged diagnostics versus current main and zero added diagnostics; it is not an all-green app typecheck. No local frontend app build or browser/device smoke was performed. Captured deployed-program tests passed the unchanged 1192/1209-byte claim wires and atomic rollback cases. Independent NAV, protected-claim and accounting reviews cover their stated scopes; retain the exact source manifests with the release evidence.

Before activation, confirm current deployed program/ALT compatibility, production quote coverage and deadline behavior, database grants under the actual service identities, and the operator's approved release SHA. Record practical limits of the offline program evidence and any browser/device smoke still missing. Complete financed lifecycle receipts are required before claiming financed deposit-to-claim or liquidation qualification.

## Operator deployment sequence

Commands below are for the authorized release operator after these gates pass. Use the existing secret-injection/credential process; do not put secrets in arguments or reports. Follow [Hetzner release procedures](../../deploy/hetzner/README.md#release-backyard-retail-and-observer) and [the deploy script](../../scripts/deploy-workers.sh).

1. Record previous family SHAs, target merged SHA, migration ledger status and outstanding signed worker/claim attempts. Preserve a durable database backup and exact-wire rows. Check the worker verification artifact with `bun run deploy backyard <sha> --check-ci`. The real deploy requires the commit on `origin/main` and green Workers v2 verification.
2. Apply additive Yield migrations [0139](../../migrations/yield/0139_earn_max_withdrawal_consent.sql) then [0140](../../migrations/yield/0140_earn_max_withdrawal_costs.sql), using `loyal-migrate -db yield status|up` through the existing `bun run yield:migrate:check` / `bun run yield:migrate` entrypoints. Inspect all pending versions before `up`; use the runner's `-to` boundary when later migrations must remain pending. Confirm applied checksums with `status`. Never rewrite applied files. These migrations create consent, cost-operation, allocation and claim-attempt tables; they contain no role grants.
3. Verify privileges under the configured service credentials; this is a capability check, not an instruction to create roles or issue GRANTs. The [existing owner policy](../../deploy/hetzner/README.md#session-auth-opt-in-owners-go) uses the Yield Neon owner credential for `WEB_YIELD_DATABASE_URL`. Approval needs SELECT/INSERT on `earn_max_withdrawal_consents`; claim accounting needs SELECT/INSERT/UPDATE on `earn_max_withdrawal_claim_attempts`. Display-only withdrawal cost status separately needs SELECT on both `backyard_withdrawal_cost_operations` and `backyard_withdrawal_cost_reservations`, plus SELECT on claim attempts. Startup gates these three capabilities independently; claim accounting and status also require the consent store. Status visibility does not grant approval or claim authority. Backyard needs consent reads and SELECT/INSERT/UPDATE on both withdrawal-cost tables, in addition to its existing operation privileges. Verify the applicable capabilities before exposing updated clients. Missing capability must remain unavailable, never fall back to an unprotected official claim.
4. Deploy the compatible Backyard runtime with `bun run deploy backyard <sha> --migrations-applied`. It builds the archived SHA, verifies the release checksum/stamp, gates on zero resumable in-flight operations before and after stopping, and checks the new worker start. Let the gate wait or fail; do not delete operations to force it through. The current `in-flight` count includes `decided` through `reconciling` operation states, including disclosed wires still in `decided`; it does not independently count terminal operations' unresolved cost rows or user claim attempts. Inventory those journals separately and retain compatible recovery. For intentionally stopped units use the documented `--stopped` procedure only after compatibility review.
5. Deploy the web API and bundled web client together using `bun run deploy web <sha>` under the documented build environment. This family has no migration check; steps 2–3 must already be complete. Publish the updated mobile client through its normal separately authorized release process only after API capability checks. Deploy observer/retail only if the final integrated release changes their owning paths; use the same migration-aware family procedure.
6. Read back actual running SHAs, clean startup journals, consent/attempt capability and immutable generation/version behavior. Inspect cost recovery and unresolved attempts without replacing their wires. Verify fresh full-size quote certification and explicit missing-coverage holds, withdrawal pending/claimable reasons, and both vaults' monitoring. Use separately approved canary actions for any transaction validation; an active unit alone proves no payout or cost bound.

Worker deployment installs Prometheus rules after engine activation, with validation and reload/readback. Repeating the same active SHA retries its rules step. It does not install Alertmanager configuration, and it does not apply ClickStack objects. Follow the separate [RWA](../../deploy/hetzner/monitoring/clickstack-vault-alerts.md) and [Looping](../../deploy/hetzner/monitoring/clickstack-looping-alerts.md) guides. Saved monitoring verification is in the operator report bundle.

## Read-only unresolved journal inventory

After migrations are present, run these queries in an already authorized Yield database connection using the documented credentials. They expose identities/status only, never raw signed wire. Record the result before deployment or rollback. Keep terminal/manual-recovery journal inventory separate from the generic in-flight gate so compatible fix-forward recovery is not blocked by that count.

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = '15s';

SELECT c.operation_id, o.route_key, o.status AS parent_status,
       c.state AS cost_state, c.transaction_signature,
       c.signed_wire_sha256 IS NOT NULL AS wire_bound,
       c.last_valid_block_height, c.unknown_reason, c.created_at,
       (SELECT count(*)
        FROM loyal_yield.backyard_withdrawal_cost_reservations r
        WHERE r.operation_id = c.operation_id) AS reservations
FROM loyal_yield.backyard_withdrawal_cost_operations c
JOIN loyal_yield.multiply_operations o USING (operation_id)
WHERE c.state IN ('reserved', 'unknown')
ORDER BY c.created_at, c.operation_id;

SELECT a.attempt_id, a.consent_id, c.vault, a.authority,
       a.request_signature, a.request_instruction,
       a.transaction_signature, a.last_valid_block_height,
       a.reserved_fee_lamports, a.created_at
FROM loyal_yield.earn_max_withdrawal_claim_attempts a
JOIN loyal_yield.earn_max_withdrawal_consents c USING (consent_id)
WHERE a.state = 'reserved'
ORDER BY a.created_at, a.attempt_id;

ROLLBACK;
```

Nonempty results require compatible reconciliation or explicit manual recovery review. A `failed`/`reconciled` parent, consent expiry or elapsed wall-clock time alone does not establish that its signed wire cannot land. These read-only queries do not release reservations, settle fees or authorize replacement transactions.

## Rollback without discarding signed state

Disable new official claim/funding admission through existing operator controls before reverting protection-capable code; retain the runtime/API needed to reconcile already reserved attempts. Do not invent a new environment toggle. If no suitable admission control exists, stop release expansion and have root choose a compatible rollback procedure.

Keep migrations 0139/0140, immutable consents, cost reservations, claim attempts and exact signed bytes. Database fencing, consent expiry, a new consent or a stopped worker cannot revoke signed chain transactions. Reconcile finalized exact-wire effects, or prove finalized nonlanding after blockhash invalidity, before releasing exposure or admitting replacement work. Never clear ambiguity by deleting rows or marking a disclosed wire unsigned.

`bun run deploy <family> <previous-compatible-sha>` restores an eligible prior binary; web uses `bun run deploy web <previous-compatible-sha>`. The script retains newer Prometheus rules. An old binary that ignores the new cost journal or protected attempts is not a safe recovery target while those records are unresolved. Prefer a reviewed protection-capable fix-forward release; otherwise hold admission and retain compatible recovery until durable signed exposure is settled. A stopped-mode failure leaves the previous drop-in stopped. Preserve deploy logs and backups, and confirm schema/runtime compatibility before any manual restart.
