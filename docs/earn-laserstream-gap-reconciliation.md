# Earn LaserStream gap reconciliation

The gap tool audits finalized Solana history against durable Earn reconciliation jobs and chain mutations. Its default mode is read-only: it writes a JSON report and does not enqueue jobs or advance a cursor. Provide `NEON_DATABASE_URL` and `SOLANA_RPC_URL` through the operator environment rather than command arguments.

```sh
scripts/reconcile-earn-laserstream-gap.sh \
  --environment mainnet \
  --wallet <wallet> \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-gap-audit.json
```

Review `candidates`, especially entries whose `status` is `missing` or `refund_only`. `completed` means a durable non-refund chain mutation covers that signature/vault; `pending` means a job exists but has not completed. A completed no-op job does not count as durable cash-flow coverage. `refund_only` means rent was recorded but there is no separate cleanup or cash-flow mutation; it is an audit candidate, not proof that the position should be closed.

Re-run the same bounded command with `--execute` only after the report has been reviewed and production mutation has been explicitly approved:

```sh
scripts/reconcile-earn-laserstream-gap.sh \
  --environment mainnet \
  --wallet <wallet> \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-gap-execution.json \
  --execute
```

Execution enqueues only candidates classified as `missing`; existing completed chain mutations and pending jobs are not re-enqueued. When one transaction appears in several watched account histories, execution prefers a non-policy account frame so policy discovery cannot consume a cash-flow repair as a no-op. The report is saved before any enqueue and rewritten with execution counts afterward. The deployed reconciliation consumer processes the resulting jobs.

Use `--live-targets-only` to audit exactly the identities currently returned to the monitor. The default also includes historical identities so closed or retired vaults can be recovered.

## Live policy deletion ordering

Policy deletions retain distinct durable job identities from wallet/settings policy-discovery updates in the same transaction. Otherwise a wallet update arriving first can consume the only job, leaving catalog state inactive while the position stays active. For legacy Earn, successful policy-removal reconciliation must still run the existing zero-balance/closed-policy cleanup proof; updating the policy catalog alone does not settle positions. Earn Max keeps its existing policy-monitor ownership.

This change does not modify financial projection rules or replay safeguards. Deploy the monitor before replaying affected history; previously completed jobs are not reopened automatically. Verify policy-close processing, withdrawal and cleanup markers, position state, and worker/autodeposit/rebalance processing freshness after deployment. The app symptom was “This Earn policy is no longer active. Refresh Earn before withdrawing.” despite a completed on-chain full exit.

## Previously recorded refunds without cleanup

Deploy the updated monitor and gap tool before requesting a repair. Audit each wallet and bounded slot range, verify finalized withdrawal/cleanup signatures, zero holdings and closed policies, and inspect later deposits and holding-event history. Recover missing withdrawals first, then audit refund-only cleanup separately; never resend the on-chain transaction or delete refund history.

```sh
scripts/reconcile-earn-laserstream-gap.sh \
  --environment mainnet \
  --wallet <wallet> \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --repair-refund-cleanups \
  --report-file earn-refund-cleanup-audit.json
```

Only after separate approval, repeat with `--execute`. This mode requires explicit `--wallet` and `--to-slot`, selects only refund-only candidates, and uses a separate deterministic repair job identity. Pending or completed repairs are not duplicated. The consumer must prove an anchored wallet refund, zero balances and closed policies; unavailable proof remains a visible retry error rather than a completed no-op. A wallet that has since deposited again may not satisfy this live proof: do not enqueue its cleanup without further historical investigation.

Refund accounting and cleanup have separate transactional completion markers. An existing refund cannot suppress a missing cleanup, and repeating a successful repair changes neither refund history nor balances. Cleanup does not clear policies or balances whose slots show later activity. Withdrawal replay uses deposit history for the policy at the withdrawal slot, never reactivates an old policy, and fails if no owning position history exists. A later zero-balance holding event settles old principal even when a new deposit has reactivated the same position row; replay before that boundary records immutable withdrawal history without debiting current funds. Later or same-slot deposits without a proven settlement boundary block replay; missing or inconsistent history requires manual review, not a guessed position assignment.

After execution, inspect pending jobs and errors, rerun the read-only audit, verify one withdrawal/refund/cleanup record per expected signature, and compare current principal, holdings and active policies with later deposits and finalized chain state. A successful enqueue is not a completed repair. No migration or production write is needed to run the audit.

Focused verification (disposable local PostgreSQL, no production credentials):

```sh
bash scripts/verify-earn-replay-repair.sh ../loyal-app
bash scripts/verify-earn-laserstream-gap-reconciliation.sh
```
