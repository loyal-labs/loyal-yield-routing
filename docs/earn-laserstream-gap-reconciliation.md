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
