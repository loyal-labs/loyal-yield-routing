# Collecting Earn Max referral fee evidence

The collector reads finalized Solana mainnet RWA vault and LP mint accounts and successful transactions. It stores raw Earn Max LP shares. Annual rewards in USDC are computed by the separate capital collector.

Once the opening checkpoint exists, the [Earn MAX harvest job](earn-max-referral-harvest.md) can collect eligible fees into Loyal’s treasury. It runs under a separate fee-paying wallet.

## Starting a period

1. Apply migrations 0146–0148 and configure the already approved program launch in `referral_earn_max_program`. The collector stays idle until a launch exists.
2. Run `loyal-web cron earn-max-referral-fees --anchor --dry-run`. Check the finalized slot, historical signature marker and raw admin accumulator. The code pins the deployed Voltr program and LP mint authority, Loyal admin, 15% admin performance fee, 0.4% issuance and zero management, redemption, manager and protocol rates.
3. After approval, run `loyal-web cron earn-max-referral-fees --anchor` once. Eligible opening fee inventory is zero. Any admin LP already accrued is reserved as ineligible. A later harvest must account for that entire reserve as well as newly observed assessments. A zero post-harvest balance is also a valid starting point.
4. Enable the optional `loyal-web-cron-earn-max-referral-fees.timer` with the release and credential drop-ins used for the web cron instances. It runs every five minutes. Inspect `journalctl -u 'loyal-web-cron@earn-max-referral-fees.service'` for the verified slot, actual harvest count and any held evidence. The binary also accepts `--dry-run` to check a batch without writing.

The job consumes the normal web cron App DB, Yield DB and mainnet RPC credential files. It never needs a signer. Use SELECT on the referral launch, attribution, verified capital and 0148 relations. Grant INSERT on the new ledger relations, sequence USAGE for lot IDs, and UPDATE on the cursor, assessment harvest association, participant coverage and fractional remainders. Settlement reads the immutable lot and harvest relations.

## Collection and recovery

Each interval must reconnect to the previous finalized vault signature. The reader orders transactions by finalized block membership, matches the actual Voltr invocation to the event, reconciles actual token mint/burn effects, and checks the resulting fee accumulator and LP supply against the closing finalized accounts. Supported user deposits, withdrawal requests, cancellations and claims account for their token movements. An isolated zero-capital strategy report can classify new fee LP only when effective supply, high-water mark and vault value rise under the pinned fee configuration.

Each eligible friend receives a fraction of the assessed fee equal to their proved free eligible LP immediately before the transaction divided by total effective supply. Withdrawal escrow, unreferred holders and unproved ownership do not increase another friend’s allocation. Fractional raw LP stays with the original friend across confirmed harvests. A finalized harvest must mint the exact reconciled admin fee amount into the admin’s derived LP account, with matching token balance change. An assessment alone creates no spendable lot.

`referral_earn_max_fee_weights.hold_reason` identifies individual missing ownership histories. Each run retries up to 16 oldest held entries against the original finalized block and assessment. A later proof can release that friend’s share from an earlier harvest exactly once. An unrecoverable historical ownership gap stays attached to that friend.

A vault-wide hold leaves `referral_earn_max_fee_cursor.verified_slot` at the last reconciled point. Examples include a missing archive marker, a changed fee configuration, a multi-invocation vault transaction, a report with inseparable fee sources, or a mismatched treasury mint. The bounded scanner reads up to four 64-signature history pages per pass. Restore the original evidence or extend the supported replay range/instruction validation before retrying. Replacing the cursor with a current balance would discard historical attribution. Independent transfers out of the treasury are the settlement worker’s custody and solvency check.
