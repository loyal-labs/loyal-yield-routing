# Earn LaserStream gap reconciliation

Two tools repair different parts of an Earn stream gap:

1. `reconcile-earn-policy-projection-gap.sh` discovers missing users and policies from global Squads program history and repairs policy projections.
2. `reconcile-earn-laserstream-gap.sh` uses the repaired policy universe to find and enqueue missing deposit, withdrawal, and rebalance reconciliation jobs.

Run the policy repair first when the policy projection cursor stalled. Both tools default to read-only audit mode. Provide `NEON_DATABASE_URL`, `SOLANA_RPC_URL`, and `EARN_MAX_DELEGATE` through the operator environment rather than command arguments.

## Policy projection gaps

Audit an inclusive finalized slot range before making any production writes:

```sh
scripts/reconcile-earn-policy-projection-gap.sh \
  --environment mainnet \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-policy-gap-audit.json
```

The scanner walks finalized signatures for the Squads program, decodes successful policy transactions, reloads current Earn MAX policy state, and compares route/setup and Earn MAX projections with the database. Review every finding whose `coverage` is `missing`. The audit does not depend on an identity already existing in the yield database, so it discovers users first seen during the gap. Increase `--max-signatures` explicitly if the bounded range exceeds its default safety limit. For an old historical range, pass `--before-signature <squads-signature-after-to-slot>` so the RPC starts near the upper bound instead of paging backward from current history.

After the audit is reviewed and production mutation is explicitly approved, replay the relevant policy transactions idempotently. For each settings account with a missing projection, execution replays the complete detected create/remove sequence so superseded policies do not become active again:

```sh
scripts/reconcile-earn-policy-projection-gap.sh \
  --environment mainnet \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-policy-gap-execution.json \
  --execute
```

Execution does not advance the live projection cursor by default. Advance it only after the complete bounded replay is ready to become the new resume point. The command requires the previously observed cursor and aborts before replay if another process changed it:

```sh
scripts/reconcile-earn-policy-projection-gap.sh \
  --environment mainnet \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-policy-gap-execution.json \
  --execute \
  --advance-cursor \
  --expected-cursor <reviewed-current-cursor>
```

Stop the live policy projector while executing a cursor-advancing repair. Restart it only after verifying the report and database projections. Then run the cash-flow reconciliation below so newly discovered identities are included.

## Cash-flow reconciliation

The cash-flow gap tool audits finalized Solana history against durable Earn reconciliation jobs and chain mutations. Its default mode is read-only: it writes a JSON report and does not enqueue jobs or advance a cursor.

```sh
scripts/reconcile-earn-laserstream-gap.sh \
  --environment mainnet \
  --wallet <wallet> \
  --from-slot <inclusive-finalized-slot> \
  --to-slot <inclusive-finalized-slot> \
  --report-file earn-gap-audit.json
```

Review `candidates`, especially entries whose `status` is `missing`. `completed` means a durable chain mutation covers that signature/vault; `pending` means a job exists but has not completed. A completed no-op job does not count as durable cash-flow coverage.

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
