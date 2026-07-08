# Idle Vault Stale Source Reconcile Verifier

Goal: idle-vault deposits must trust live RPC over stale DB idle balances. If DB says idle USDC is available but the live vault ATA has less, the worker must reconcile the DB source state and avoid submitting or recording a failed movement.

Required checks:

1. In `crates/loyal-yield-orchestrator/src/bin/same-mint-reserve-swap.rs`, `run_idle_vault_deposit_flow` must classify source-state mismatches before the generic preflight failure path using typed blockers, not string-count inference. At minimum these source-sync reasons are handled: live vault idle USDC is below planned amount, DB idle amount differs from planned amount, DB idle amount is above live vault ATA balance, and expected idle observed slot/time no longer match the DB row.

2. For the normal source-sync path, verify the output JSON status is exactly `idle_vault_deposit_stale_source_reconciled`, with `sendsTransactions: false`, `writesDecision: false`, and `writesCurrentIdleBalance: true`.

3. The same source-sync path must call a helper that writes `loyal_yield.vault_idle_token_balances_current` through `record_current_idle_token_balance`, using the live chain preview amount, the derived vault USDC ATA, the vault pubkey as owner, the preview observed slot, and `source_commitment = "confirmed"`. It must only claim reconciled after the returned current row matches the live amount, mint, owner, ATA, confirmed source, and at least the preview slot.

4. If the write is attempted but the returned current row still does not match live RPC, the worker must emit a non-executed status such as `idle_vault_deposit_stale_source_reconcile_conflict`, return success to the parent monitor, and still send no transaction or decision write. The normal source-sync path must also return success after writing the live idle balance.

5. Non-source blockers remain fail-closed in the existing generic blocked path. Examples: wrong target liquidity mint, wrong derived vault ATA, missing vault idle ATA, missing/invalid setup or policy constraints, and policy deposit simulation blockers.

6. In `crates/loyal-yield-orchestrator/src/bin/same-mint-yield-monitor.rs`, the idle-deposit execution branch must inspect the child JSON status. A successful child process with status other than `idle_vault_deposit_executed` must be surfaced with that child status and must not be labeled `idle_vault_deposit_executed`.

Verification commands:

```sh
git diff --check
cargo fmt --manifest-path crates/loyal-yield-orchestrator/Cargo.toml -- --check
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap
```

Verdict: PASS only if all required checks hold and all verification commands pass. FAIL if stale DB-vs-RPC idle source mismatch can still create repeated failed `rebalance_decisions`, send a transaction, or be mislabeled as an executed deposit.
