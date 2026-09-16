# Pilot budget validation

## Implementation

Explicit pilot authority augments the existing Phase 3 budget. Historical `SpentMicros` and per-operation `BookedSpentMicros` remain cumulative gross history. Separate bounded execution-cost counters permit principal to be reused after exact finalized settlement. Admission still reserves the current full gross debit plus the complete remaining gross exit. Ambiguous submissions retain reservations; proven unspent expiry restores the previous exit reserve.

Chosen pilot limits are 100 USDC total vault deposits, 10 USDC working equity, 20 USDC gross debit per transaction, 100 USDC committed authorization per family, 150 USDC across the pilot. The larger gross envelope covers repeated movement of the same principal within a complete exit. New entry work stops after 5 USDC of cumulative execution costs; already reserved recovery remains possible. No limit change is permitted while exit reservations remain.

Activation is an explicit database operation under the existing route lease/row lock. It neither loads a signer nor enables deposits. It requires finalized mainnet flat observations across all eight configured historical/current lanes, zero user LP supply and fees, current receipt NAV/custody zero, disarmed ticket, returned strategy/Squads cash, and an internally consistent idle vault book. Missing legacy budget is accepted only when no historical Phase 3 authorization exists, and is recorded as an absent prior snapshot. Existing budgets must be open with no outstanding operation or exit reservations. Physical and journal-derived stops both block activation. Repeated activation is inspection only; archived prior budget hashes use canonical JSON after PostgreSQL jsonb serialization, and inherited gross history cannot decrease.

## Local verification

- 100 rotations / 300 finalized reducer settlements reuse principal, preserving 4507 USDC in test gross history and accumulating 3 USDC of execution costs. Restart keeps ambiguous reservations and immutable identities.
- PostgreSQL activation tests cover missing/existing budgets, preserved 17-USDC historical spend, finalized RPC commitment, restart idempotency, canonical JSON, derived stops and corrupted markers/counters.
- Exact native initializer reconciliation runs in both legacy and pilot modes. Wrong wire, wrong rent and unfinalized effects preserve the reservation. Correct finality books gross and execution cost atomically in the budget and operation journal.
- Full worker suite and targeted regression cases pass. These are local tests, not live rotation proof.

## Current live blocker and evidence

Read-only finalized evidence at slot 447431070 fails the strict flat check because historical PYUSD custody `J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn` still contains 214898 raw units (0.214898 PYUSD). Pilot collateral and obligations are flat; the current vault has 23 raw USDC idle and zero strategy/Squads USDC. The same PYUSD residue is visible in retained September 4 captures; it was not created by this implementation.

`pilot-flat-state.json.gz` retains the full public read and HOLD result. Uncompressed SHA256: `649175a261ddc2c53c0a49f39c0a6acac5c7f8e9256746e4c7b5251df43e26f8`.

`pyusd-return-quote.json` is an unsigned, read-only Jupiter quote at slot 447432006 for all 214898 raw PYUSD into USDC: quoted 214919 raw, minimum 214275 raw at 30 bps. It proves quote availability at that observation only; it is not an admitted or simulated execution and must be refreshed before use.

## Still disabled / incomplete

No activation command or worker call enables the pilot mode yet. 10-USDC tranche wiring, actual native exit-fee liquidity, residual conversion/return and full release lifecycle remain required. Production database, policies, balances and deployment were not changed in this work.

## Measured execution-cost producer

Pilot admission now derives its execution-cost bound from the compiled message, exact measured debit, observed fee, and retained token/native price evidence. A swap values its enforced minimum output with lower token/upper USDC prices; missing, stale or inverted credit intervals reject admission. Borrow costs include origination fees; deposit and full repayment costs include their independently bounded rounding windows. Withdrawal books one raw liquidity unit for the pinned KLend floor conversion. Native initializer rent remains a recoverable asset under its exact finalized native-balance reconciler. All expense counters book admitted conservative upper bounds, not claimed realized P&L.

Admission records this bound in the existing reservation and operation authorization. Generic numeric pilot reservations are refused. Retries, pre-signing builds and the final locked send fence recompute the classification and reject an increased expense even when the gross debit still fits. Signed repricing leaves the original wire unchanged; a validated fresh-cost HOLD retains it and its reservation for the existing proven-absence recovery path.

Local regression coverage includes understated/corrupted costs, missing or stale minimum-credit valuation, inverted intervals, borrow fees, receipt/debt rounding, recoverable native rent, wire-preserving repricing, and real PostgreSQL admission/build/send rejection. Fable independently reviewed the arithmetic and checked retained same-ELF withdrawal execution: 92653355 actually burned receipt units release 99999999 raw liquidity, exactly the floor formula. This establishes the rounding assumption; it is not a new live transaction.

Cost observation now measures without choosing a deployment allowance. Under the route lock, admission checks the current cost, every complete-exit step and its summed reservation against the persisted budget limits. Build/send still require the fresh cost to fit the exact reservation. The early signed-cost rejection preserves legacy and pilot maximum caps without granting authority. Local PostgreSQL verification accepts a 10-USDC measured allocation under pilot limits, refuses a narrowed one-USDC durable limit, rejects an oversized exit step and an understated sum, then exercises the real build/send guards. Full worker/database tests and Go vet pass; Fable found no cap bypass in the reviewed production callers.

The producer and current-limit gates are connected, but decision tranche wiring, pilot activation and rollout remain disabled. No production mutation was made.
