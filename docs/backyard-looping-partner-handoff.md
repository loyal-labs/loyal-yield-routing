# Backyard Looping vault partner handoff

## Identities

- Voltr vault "Loyal Looping": `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`
- Asset: USDC `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`
- LP mint (loyLOOP, 9 decimals): `5MAGdiFcSQumjZX5BDh3LqHyFog5auvEBXXJezDXAArg`
- Voltr program: `vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8`
- Adaptor: Voltr Trustful `3pnpK9nrs1R65eMV1wqCXkDkhSgN18xb1G5pgYPwoZjJ`
- Strategy: `GUCvzfn1hqThWp5uQNF7CqoxC1XckCPTjsRGKVXHPxu1`
- Manager: Squads vault `6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY` (Settings `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`, vault index 2)
- Admin: `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`

## Terms

- Fees: all 0 (manager and admin performance and management fees, issuance, redemption).
- Withdrawal waiting period: 600 seconds.
- Max cap: 1,000,000 USDC.

## Integrating

A depositor talks only to the Voltr program, using `@voltr/vault-sdk`. The adaptor and the manager are never in a user's transaction.

- Deposit: `depositVault(amount)` mints loyLOOP at the current share price.
- Withdraw:
  1. `requestWithdrawVault(amount, isAmountInLp, isWithdrawAll)` escrows the LP. The escrow account must exist first: it is the loyLOOP ATA owned by the user's request-withdraw receipt PDA (`["request_withdraw_vault_receipt", vault, user]` under the Voltr program). Put an idempotent ATA create (payer = user) in the same transaction, or Voltr fails with `AccountNotInitialized` on `request_withdraw_lp_ata`. The amount must be non-zero: `amount = 0` with `isWithdrawAll = true` fails with `InvalidAmount` (6000), so withdraw everything by passing the LP balance with `isAmountInLp = true`.
  2. After 600 s, `withdrawVault()` pays USDC from the vault's idle account.
  3. `cancelRequestWithdrawVault()` returns the escrowed LP.
- Pending requests: `getAllPendingWithdrawalsForVault(vault)` lists them, and `getPendingWithdrawalForUser(vault, user)` returns one user's.
- Share price: the vault's total value divided by the LP supply, both read from the Vault account.

## What the manager does

Loyal's worker manages the vault through its own Squads account. Squads policies bind each act to these exact programs and accounts.

- **Put money to work:** deposited USDC moves from Voltr idle to the Squads account, then into Kamino looping lanes. The worker enters a lane only when its expected 30-day gain exceeds the full move cost, including the lane's one-time setup rent (about $2.50). On Oct 14 2026 that was about $490 idle for ONyc and $710 for Maple 1.5x. Below that, USDC stays in Voltr idle.
- **Report value:** the worker reports the Squads book through Voltr `deposit_strategy`: cash plus each lane's collateral minus debt, read from chain at one slot. It reports when the book moves more than 50 bps from the last reported value, and at least hourly.
- **Pay withdrawals:** a pending request makes the worker unwind enough of a lane. It then returns the USDC to Voltr idle in one transaction, before the 600 s wait ends.

The reported value is the manager's statement of the Squads book. It is not computed on chain by Voltr. Anyone can recompute it from the Squads vault's token accounts and Kamino obligations.

## Mainnet canary (Oct 14 2026)

| Step | Signature | Time |
|---|---|---|
| Flat vault: entry into Maple 1.5x with 39.99 USDC (allocate, swap, open, borrow, top-up) | `3maJzJEG…` … `5TqZLKN2…` | 90 s |
| 20 USDC request against the invested vault: delever, swap, return to idle | `5ve3Q8d3…`, `zB6CPqDS…`, `5VbQG4A5…` | funded 53 s after the request |
| Claim after 600 s | `5vY4aPSJ…` | paid 20.000000 USDC |
| Squads cash returned for a 39.99 USDC request | `67mypwNs…` | 10 s |
