# Backyard Voltr tools

What remains here is the Backyard RWA Multiply operator scripts (the
`activate:*`, `resolve:*`, `probe:*`, `evidence:*` and `verify:rwa-*` entries
in `package.json`) and the pinned SDK dependencies that the Go Backyard tests
load as an independent oracle (`go/workers/internal/backyard/testdata/*.mjs`
resolves `@kamino-finance/*` from this package's `node_modules`).

```sh
bun install --frozen-lockfile
bun run check
```

The four-market Voltr router CLI (`src/cli.ts`), its policy catalog
authorization and its evidence directory were removed: the Backyard Voltr
vault is empty and its fleet route was deleted (#321), and Backyard executes
through the Go policy literals (`go/workers/internal/backyard/policy.go`),
which each build finds among the policies installed on chain.

### Audit selected Earn Max fee receipts

With a mainnet archive endpoint in `SOLANA_RPC_URL`, run the following from this directory.

```sh
bun run audit:earn-max-fee-receipts <signature> [<signature> ...]
```

This read-only command verifies up to 32 individual finalized transactions and their order in finalized blocks. It reports Voltr strategy deposits and withdrawals, user deposits, fee harvests and vault configuration field updates for the Earn Max vault. It rejects unsupported Earn Max instructions, missing logs, nested program failures and incomplete invocation traces. Raw LP and USDC amounts are integer strings. The command provides receipt-level evidence. A referral funding balance additionally requires continuous assessment, ownership and treasury evidence. [Verified mainnet examples and accounting requirements](../../docs/evidence/earn-max-referral-fees-2026-11-19.md)
