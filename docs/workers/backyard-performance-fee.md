# Backyard Looping worker performance fee

The Looping worker accrues 10% on gains above its previous per-share high at hourly NAV reports. Published NAV reserves the unpaid fee. Extra reports carry that balance, and paying it releases the same reservation. The first report after activation establishes the baseline using the higher of current economic value per share and Voltr’s existing high-water mark, with historical fee LP included in supply. Earn Max keeps its 15% Voltr admin fee.

## Prepare

1. Apply `migrations/yield/0150_backyard_looping_performance_fee.sql` through the normal yield migration runner. The Looping database role needs `SELECT` and `UPDATE (applied_operation_id)` on `loyal_yield.backyard_looping_fee_capital_resolutions`. The operator recording an external receipt also needs `INSERT` on that table. Deploy the fee-aware worker and stats dashboard on `loyal-main`. A Looping route with its 15% Voltr fee and no worker checkpoint continues using the existing terms.
2. The approved weekly transfer ceiling is 10,000 USDC, or `10000000000` micro-USDC. It applies to the exact treasury. The worker calculates its payable balance independently and carries excess forward.
3. Rehearse the policy with its authorized Settings signer and the vault fee change with the existing Voltr utility. The policy command simulates by default. The Voltr rehearsal uses the public admin key and verifies the simulated post-state.

```sh
weekly_cap_raw=10000000000
loyal-engine policy apply backyard \
  --settings 5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6 \
  --delegate 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 \
  --vault looping --literal PAY_PERFORMANCE_FEE --weekly-fee-cap-raw "$weekly_cap_raw"

# From tools/backyard-voltr with SOLANA_RPC_URL set
bun src/activation/voltr-admin-performance-fee.ts --vault looping --bps 0
```

The treasury owner is `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`. Its Solana USDC account is `8SoPTUZ4tcSXHJ3M4WMV6sMCAiAb26S2H51h34Pb24Ce`. The policy binds the Looping custody account, manager, mint, treasury, transfer size and weekly allowance.

## Activate

1. Install the approved policy using the rehearsed command with `--send`. Confirm finalization.
2. Use the Looping unit’s database credential to check `loyal-engine backyard in-flight --vault looping`. Once it returns zero, stop `loyal-backyard.service` on `loyal-main`, then check the count again. If an operation appeared during the stop, resume the current worker, let it reconcile, and repeat the drain before changing terms.
3. Apply the rehearsed Voltr update under the vault admin using `CONFIRM_MAINNET=1` and `--send`. Wait for finalization. Old accrued Voltr fee shares remain claimable. The fee change targets the Looping vault `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`.
4. Run the command below with the Looping unit’s RPC credential. It verifies the finalized zero-fee vault, treasury, effective supply and exact installed policy. Repeat with `--apply` and the unit’s direct database credential to create the checkpoint. An identical retry retains the original anchor and balance.

```sh
loyal-engine backyard activate-performance-fee --vault looping --weekly-fee-cap-raw "$weekly_cap_raw"
```

5. Start `loyal-backyard.service`. Verify its first finalized report commits checkpoint version 1 with zero assessed. Check the next eligible hourly report against the inherited high. Verify that Earn Max still reports 1,500 bps through `bun src/activation/voltr-admin-performance-fee.ts --vault rwa --bps 1500` in simulation mode.

## Verify and recover

Monday at 00:00 UTC makes the prior week’s accrued USDC payable. A healthy worker settles on an eligible tick within the policy allowance and available cash after depositor withdrawals. Follow the journal through the finalized treasury credit. On a failed or held operation or a service restart, inspect the exact operation and its receipt before allowing another payout.

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = 10000;
SELECT state->'performanceFee' AS checkpoint
FROM loyal_yield.multiply_route_states
WHERE route_key = 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY';
SELECT action, status, confirmed_slot, transaction_signature,
       expected_effects #> '{expectedEffects,performanceFee}' AS fee_entry
FROM loyal_yield.multiply_operations
WHERE route_key = 'backyard:6HTfVjwk5FfW4GH67hmEen7VHeURdB5EJj6E429qTrUY'
  AND expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL
ORDER BY created_at DESC LIMIT 10;
COMMIT;
```

`outstanding` is all unpaid fees. `payable` is the matured part and `lastAssessment` is the last hourly assessment time in Unix seconds. Preserve the checkpoint, signed operation journal and fee-aware worker while any fees remain outstanding. Reverting to a gross-NAV binary can give unpaid fees back to holders and make a later payout reduce their share price again.

### Outside capital

The finalized cash ledger distinguishes a direct USDC transfer into Looping’s Squads custody from an investment return. The worker raises its per-share high by the outside principal. USDC sent straight to Voltr idle stays outside the fee base until Voltr books it. A collateral gift or an outside Kamino principal change can require a USDC valuation. In that case the checkpoint records the finalized transaction signature in `capitalHold`. Withdrawals, net NAV publication and payment of previously accrued fees continue. A temporary `history_unavailable` retries on the next hourly assessment.

For a signature hold, verify the external amount in native USDC and the effective LP supply at the event slot. Include outstanding fee LP and dead-weight LP. Record the calculation and its evidence. Use the Looping RPC and the operator’s database credential to rehearse the following command, then repeat with `--apply` to record the reviewed basis. The worker may stay online.

```sh
loyal-engine backyard resolve-performance-fee-capital --vault looping \
  --signature "$held_signature" --principal-usdc-raw "$outside_principal_raw" \
  --effective-lp-raw "$effective_supply_at_event" --by "$operator" --evidence "$basis_reference"
```

The next eligible hourly publication waives the unresolved interval and retains the higher of current net price and the previous high plus the reviewed principal per share. Finalization consumes that receipt’s approval once and records the operation in `loyal_yield.backyard_looping_fee_capital_resolutions.applied_operation_id`. A second external event has its own receipt and review. When `capitalHold` clears, future fresh gains accrue normally.
