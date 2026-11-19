# Earn Max referral capital

Apply the referral identity migration 0146 before capital migration 0147. The collector reads the identity conflict view under its shared transaction lock. Deploy the matching loyal-web binary with app database, yield database and archival mainnet RPC access.

An empty `loyal_yield.referral_earn_max_program` keeps the program inactive. At the approved go-live, record one finalized mainnet slot and its matching chain time in `launch_slot` and `launched_at`. The row is immutable. The collector starts at the later of go-live and binding. It processes the bound wallet’s verified Earn Max smart account. A newly eligible deposit requires the bound wallet’s signature on the finalized deposit transaction. Delegated-only deposits without historical ownership proof hold coverage for review.

Run `loyal-web cron earn-max-referral-facts <referee-app-user-id> --dry-run` for a read-only preview. Run the same command without `--dry-run` to record verified movements and a checkpoint. A successful invocation can report incomplete coverage. Inspect its JSON for the reason, the finalized slot and the checkpoint time.

Use the earnings and verification time together from `loyal_yield.referral_earn_max_capital_status`. Missing rows or null earnings mean no proven result. On a new evidence gap the previous proven amount and time remain visible. The separate attribution conflict view must also be checked when displaying or allocating earnings. A checkpoint records fee-net deposit accrual in micro-USDC. Settlement requires independently verified performance-fee shares and payment receipts.


For recurring collection, install the optional `deploy/hetzner/systemd/loyal-web-cron-earn-max-referral-facts.timer` and run `systemctl daemon-reload`. After the approved launch, enable it with `systemctl enable --now loyal-web-cron-earn-max-referral-facts.timer`. The existing web cron template uses the verified web release and its encrypted credentials. Every five minutes it selects up to four bindings in round-robin order. A failed collection advances the batch cursor and is retried on the next cycle. The failed unit and its JSON counters are visible with `journalctl -u loyal-web-cron@earn-max-referral-facts.service`. An incomplete result includes a reason and preserves the last proven checkpoint.

Run `loyal-web cron earn-max-referral-facts --dry-run` to preview the next batch with its existing cursor. Pause this optional timer before a database migration or cutover that requires quiescing writers, and wait until its active oneshot finishes. Stop recurring collection with `systemctl disable --now loyal-web-cron-earn-max-referral-facts.timer`.
