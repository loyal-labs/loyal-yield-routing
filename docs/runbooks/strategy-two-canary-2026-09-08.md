# Strategy-two 1-USDC canary runbook — 2026-09-08

Status: operator handoff, not an execution approval. This is the P2.11
strategy-two lifecycle for the HXtk vault after the Phase 1 repair and the
strategy-two policy cutover. The canary amount is exactly `1,000,000` raw USDC
(1 USDC). Keep the Render worker suspended until every cutover gate below is
complete.

## Safety and scope

The target is vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`, using the
custom adaptor `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW`. The strategy-two
config is an operator-derived value; the delegated executor is the v2 executor
`62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` this week (owner decision, one
hot key). Both must be supplied as public addresses and never as key material. Do not use the generic
`runtime simulate-user-*` commands in `tools/backyard-voltr`: those commands
target the unrelated `AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK` route.

Every repository-controlled mainnet CLI send is operator-only, requires
`CONFIRM_MAINNET=1` inside the `op run --env-file=.env.1password` boundary, and
requires the appropriate operator signer. The Go worker has no
`CONFIRM_MAINNET` gate: deploying or resuming the pinned image on Render with
`POLICY_KEYPAIR` provisioned is the operator's send authorization for the
worker. The worker then sends autonomously within its policies, start gates
(legacy-retired policy gate, Settings anchor plus genesis check,
program-identity pins, and latch), and M1–M8 monitors. Keep the service
suspended until every precondition below is verified; a worker send before
that is an abort condition. No agent run, simulation, or returned signature is
authorization to send.

## Preconditions

1. Phase 1 is finalized and captured at
   `docs/evidence/backyard-rwa-strategy2/phase1-postconditions.json`: `tv ==
   idle`, strategy-one `receipt1 == 3,793,536` and unchanged, all fee
   accumulators are zero, strategy-one custody is zero, LP dead weight is
   `1,000`, and `D = idle + Σreceipts − tv == 3,793,536`. At the Phase 1
   post-state, `tv == idle == 3,793,417` and strategy-one `receipt1 ==
   3,793,536`, so `idle + receipt1 − tv == 3,793,536`; before repair,
   `3,793,417 + 2,793,417 − 2,793,298 == 3,793,536`. Never crank the old
   receipt below `3,793,536`.
2. The finalized degradation restore is complete with
   `LockedProfitDegradationDuration = 86,400` at least 24 h after the repair
   signature and before this canary or any tester deposit (operator checklist
   step 7; contract P1.7).
3. The fresh strategy-two config, report ticket, strategy receipt, and custody
   ATA are initialized and read back at finalized commitment with receipt and
   custody both zero. The four replacement policies are finalized at the
   Settings-derived seeds `145–148`; the basic policy set occupies `141–144`
   and its seed-`144` policy is the journal's anchor; the one-shot repair
   policy at `140` is finalized and removed; seeds `62–65` are absent.
4. The shared seed journal is identity-bound to the Settings address, mainnet
   genesis hash, strategy-two config, delegated signer, the seed-144 basic
   policy anchor `Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz`, finalized
   observation slot, and the anchor data hash pinned to the basic install
   readback (`43d09b3cbdd63f1c775f7a660ac75ac76f699b18bd1298ec1bf87d088e6d535a`). Run
   the keyless start gate and expect `PASS_LEGACY_RETIRED`:

   ```sh
   cd tools/backyard-voltr
   op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two --config <config2-addr> --delegated <executor2-addr> --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json --assert-legacy-retired'
   ```

5. The adaptor v3.3 upgrade has a finalized readback and M6 has been re-pinned
   to the deployed adaptor ProgramData slot and
   `836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0`. The
   Voltr executable pin remains
   `bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56`.
6. The strategy-two worker manifest/config is generated from the shared seed
   journal, the image is an immutable `backyard-rwa-worker:sha-<commit>` image,
   the Render service is configured for that image, and the worker startup
   Settings-anchor plus genesis check passes within its 60-second deadline.
7. The route-level latch is clear only because its cause was resolved. If an
   operator must clear it, use the existing command with a meaningful reason;
   it writes the `HOLD_CLEARED` journal row and does not send a chain
   transaction:

   ```sh
   cd go/backyard-rwa-worker
   op run --env-file=.env.1password -- sh -c 'go run ./cmd/backyard-rwa-worker clear-hold --route "$BACKYARD_RWA_ROUTE_KEY" --reason "<operator reason>"'
   ```

8. Before the canary, verify the zero-fee configuration, `600`-second waiting
   period, `1,000,000,000,000` raw NAV cap, and the complete M1–M8 monitor set.

## Canary lifecycle

Capture the finalized pre-canary snapshot, worker image revision, M6 pins,
policy identities, and latch state in
`docs/evidence/backyard-rwa-strategy2/phase2-canary.json` before step 1.

1. **Deposit 1 USDC.** In the deployed strategy-two wallet/client, call the
   Voltr SDK user deposit instruction for the HXtk vault with `amountRaw =
   1,000,000` and the user signer. The integrated repository intentionally has
   no HXtk strategy-two user-signer shell command; record the client
   transaction signature and finalized event instead of substituting the
   unrelated generic CLI.
2. **Allocate via the worker.** After the deposit is finalized, start the
   pinned strategy-two worker through the approved Render deployment. The
   worker chooses the allocation path; there is no standalone `allocate`
   subcommand. Capture the worker operation id, policy seed, transaction
   signature, finalized slot, and before/after account batch. The worker has no
   `CONFIRM_MAINNET` gate: the pinned Render deployment/resumption with
   `POLICY_KEYPAIR` provisioned is the operator's send authorization, and the
   worker sends autonomously within its policies and start gates. Any worker
   start or send before every precondition is verified is an abort.
3. **Refresh and restore.** Let the worker complete the report/refresh and
   restore path only when M1–M8 remain green. Record each finalized signature,
   ticket sequence, report NAV, Voltr `tv`, idle, strategy-two receipt, and
   strategy-two custody. At every resting boundary, custody must be zero; the
   transient staged amount must equal the journaled amount.
4. **Request withdrawal.** In the same deployed wallet/client, request
   withdrawal of the exact LP amount minted by the 1-USDC deposit. Record the
   finalized request signature, request receipt, LP escrow amount, event index,
   and `withdrawableFromTs`. The chain value must equal
   `requestTx.blockTime + 600` (with only the documented one-second Voltr
   rounding tolerance).
5. **Wait 600 seconds.** Wait from the finalized request transaction, then
   re-read the receipt at finalized commitment. Do not claim while the chain
   deadline is in the future; a wall-clock sleep alone is not proof.
6. **Claim.** In the deployed wallet/client, claim the finalized request after
   the deadline. Record the finalized claim signature, exact payout, exact LP
   burn, receipt closure, escrow drain, idle delta, and final `tv`. The LiteSVM
   reference is `1,000,000` raw in and `999,999` raw out; the live evidence
   must use the actual finalized quote and must not silently replace it with a
   fixture number.

## Monitors that must be green

These are the names and meanings implemented in
`go/backyard-rwa-worker/internal/backyardrwa`:

| Monitor | Green condition |
| --- | --- |
| M1 book identity | `VoltrTotalValueRaw == VoltrIdleRaw + PriorReportedNAVRaw + VoltrReceiptCustodyTrackedRaw`; receipt1 is excluded and remains `3,793,536`. |
| M2 receipt vs armed NAV | strategy-two receipt equals the latest armed NAV/return data; malformed, missing, or mismatched data holds. |
| M3 custody discipline | zero at rest; during a stage transient the known custody amount equals the journaled staged amount and Voltr-tracked custody stays zero. |
| M4 idle covers pending quotes | Voltr idle covers pending withdrawal demand; otherwise only safe unwind/restore work is admissible. |
| M5 reserve health | Kamino reserve/oracle freshness, status, lending-market, and emergency-mode gates pass. |
| M6 program identity | Voltr and adaptor ProgramData addresses, finalized deploy slots, and executable hashes match the current pins. |
| M7 fee accumulators | fee accumulators remain bounded and manager/admin performance fees remain zero. |
| M8 single reporter | ticket `last_consumed_sequence` equals the journal’s last reconciled sequence; an unknown nonzero sequence holds. |

## Evidence and aborts

The evidence file must contain the pre-canary snapshot, every lifecycle step,
signatures and finalized slots, decoded `tv`/idle/receipt2/custody2, LP and USDC
deltas, all M1–M8 verdicts, policy and ProgramData identities, worker image
revision, and the final conservation check. Finish only when custody2 and
receipt2 are zero and final `tv == pre-canary tv + 1,000,000 − payoutRaw`,
where `payoutRaw` is the actual finalized claim payout recorded in the
evidence (the LiteSVM reference is `999,999`), with no performance-fee LP
minted.

Abort immediately on any monitor hold, latch, missing/foreign account, policy or
identity drift, old policy presence, non-finalized or ambiguous send, unknown
worker image, NAV/book mismatch, custody residue, ticket sequence mismatch,
nonzero fee, future/mismatched deadline, incorrect payout or LP burn, duplicate
operation, or inability to prove the repository CLI send's
operator/`CONFIRM_MAINNET=1` gate or the worker's pinned-image/
`POLICY_KEYPAIR` authorization. Preserve all journals and use only the
relevant reconciliation path after an ambiguous send; never replay a one-shot
leg.
