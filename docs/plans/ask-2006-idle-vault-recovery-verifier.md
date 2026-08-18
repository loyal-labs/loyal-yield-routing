# ASK-2006 idle-vault handoff verifier

Run this verifier cold from the repository root. Do not trust implementation
comments, commit messages, deployment state, or a passing unit test in isolation.
Return `PASS_AUTODEPOSIT_IDLE_VAULT_HANDOFF` only when every required condition
below is demonstrated; otherwise return `FAIL_AUTODEPOSIT_IDLE_VAULT_HANDOFF`
with the failing condition and concrete evidence.

## Required end state

1. Run the repository command `bun run verify:autodeposit-idle-vault-handoff`.
   It must exercise a confirmed autodeposit pull followed by the vault-to-Earn
   handoff and exit zero.
2. In that scenario the wallet pull is accepted exactly once. A retry, process
   restart, or concurrent claimant must not prepare, sign, or broadcast another
   pull for the same claim or scheduled slot.
3. After pull confirmation, a context-fenced chain observation at or after the
   pull slot is durably projected to
   `loyal_yield.vault_idle_token_balances_current` before the pull claim is
   considered recoverable. Replaying the handoff is idempotent and an older
   observation cannot overwrite a newer one.
4. The autodeposit executor does not prepare, sign, broadcast, or confirm a
   Kamino top-up. Its successful terminal result identifies the confirmed pull
   and the durable idle-vault handoff.
5. The existing fleet `idle_vault_usdc` source selects the positive projected
   balance and remains the sole owner of vault-to-Earn preparation, execution,
   retry, and confirmation. No ASK-2006-specific top-up queue or scheduler is
   introduced.
6. A recoverable fleet route/RPC failure leaves the positive idle balance
   eligible for a later fleet cycle and does not emit `kamino_top_up_failed` or
   both child and parent fatal operational alerts.
7. An operational alert is emitted exactly once when either:
   - a confirmed pull cannot be durably published as idle-vault work; or
   - the same positive idle balance remains unrecovered beyond the configured
     recovery SLA.
   The first ordinary route/RPC failure is observable but is not an operational
   alert.
8. User failure notification is at most once per scheduled slot and is not sent
   for the first recoverable route/RPC failure. A success notification may be
   sent after the fleet confirms the deposit.
9. A read-only historical recovery mode identifies unresolved
   `kamino_top_up_failed` executions, reconciles their vault token accounts at
   finalized commitment, ignores zero/already-recovered balances, and feeds
   positive balances through the same idle-vault projection path without
   broadcasting transactions.
10. `cargo fmt --all -- --check`, the focused Rust checks for every changed
    crate, the focused Bun tests for every changed TypeScript module, and
    `git diff --check` all pass.

## Adversarial scenarios the command must cover

- crash immediately after pull confirmation but before idle projection;
- replay of the same confirmed pull handoff;
- stale balance observation racing a newer observation;
- two workers attempting the same handoff;
- transient fleet RPC failure followed by success;
- idle balance crossing the recovery SLA;
- failure to persist the idle balance after a confirmed pull;
- historical failed execution whose vault is zero;
- historical failed execution whose vault still has a positive finalized balance.

## Nice to have

- A live-gated read-only mode that reports the current ASK-2006 recovery backlog
  from Neon plus finalized Solana RPC without changing either system.

## Verdict

Print one JSON object containing per-condition evidence, followed by exactly one
of:

```text
PASS_AUTODEPOSIT_IDLE_VAULT_HANDOFF
FAIL_AUTODEPOSIT_IDLE_VAULT_HANDOFF
```

The overall verdict is PASS only when all Required conditions hold.
