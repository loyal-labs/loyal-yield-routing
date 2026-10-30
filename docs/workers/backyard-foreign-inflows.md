# Backyard unsolicited USDC transfers

The RWA worker explains unsolicited transfers to the Voltr idle and strategy
custody accounts from transaction evidence. There is no amount pin or rounding
allowance. The Trustful route keeps its existing accounting.

The ledger starts at the finalized NAV report immediately before the October 30
incident: signature `4xAkkN1U82HKEaEFeS1N5YvtoyC831vdL67EudTybpEWeLmPN9pfZNgRKwGbj8DuoPtbrwAzb2jzw5nUPorbsWq8`,
slot `463246083`. Its idle balance was `3633480291` raw and its active strategy
custody was empty. This is an audited history boundary, not permission to infer
an opening credit from a current balance. Saved public transaction fixtures
include this report and the subsequent 1-USDC gift.

For each account, the worker replays signatures in chain order from a durable
checkpoint, checking balance continuity, token identity, instruction ancestry,
and every transfer against transaction pre/post balances. Transfers inside Voltr
are protocol movements; external gifts remain distinguishable even in a
transaction that also invokes Voltr. Owned Squads staging never becomes a gift.
Unused account metas, zero transfers and classic-token read/no-op instructions
cannot create a new hold vector. Temporary sender accounts are identified from
their initialization instructions.

Only finalized checkpoints and evidence enter
`loyal_yield.backyard_foreign_inflow_cursors` and
`loyal_yield.backyard_foreign_inflows`. Evidence and cursor updates commit in one
transaction. Newer confirmed transfers are replayed in memory for the current
observation, then discarded; a rolled-back gift does not survive a restart.
The selector replays through a read-only store.

Idle credits explain the exact difference between Voltr's running total and
idle plus reported NAV plus tracked custody. Custody must equal staged principal
plus proven gifts. A Voltr restore retires the custody credits when it sweeps
them into idle and books them into its total. These gifts do not create a second
idle credit. Donations to custody can therefore benefit vault holders; idle
donations remain outside the running total.

A gift arriving after restore signing is accepted only when replay to the
transaction's exact prestate explains the entire extra debit and matching idle
credit. Staged principal and the other accounts must still reconcile exactly.
The instruction's existing per-execution amount cap stays in place. Since Voltr
sweeps the whole account, its actual sweep can exceed that amount solely by
proven foreign credits; owned principal cannot exceed the existing cap.

Deficits, unexplained token movements and custody faults still require manual
recovery. Missing receipts, incomplete history, absent CPI ancestry, RPC errors
or account/history reads that have not converged defer execution without
inventing credit. Replay scans at most 4,000 signatures and advances at most
64 receipts per observation. A larger backlog or unavailable archival history
requires investigation; do not reset the checkpoint to current balances.

## Activation and verification

Apply `migrations/yield/0125_backyard_foreign_inflows.sql` with `loyal-migrate`
before activating the new binary. The tables are additive and the prior recovery
binary ignores them. Drain existing operations through the normal Backyard
deploy procedure; operations signed by the old binary do not contain the new
sweep proof fields. Deploy the merged release with
`bun run deploy backyard <sha> --migrations-applied`.

At first start, replay must catch up from the audited report before the worker
can resume decisions. Verify the idle checkpoint has the known 1,000,000-raw
credit (plus any later evidenced gifts), both checkpoints advance, NAV reports
resume, and the service has no hold or restart. A healthy process alone does not
prove catch-up or protection has resumed. Stop rollout observation on the first
held/failed operation or restart. Existing manual holds are never auto-cleared.

Offline coverage includes the real incident receipt, mixed protocol/gift
transactions, transient sender accounts, rollback of unfinalized gifts, missing
history, exact custody checks, donation during signed restore, restart replay,
and real wire signing/persist validation. The SQL test uses a fresh local
Postgres database via `BACKYARD_FOREIGN_TEST_DATABASE_URL`, restricted to
`127.0.0.1` and a database named `backyard_foreign_test*`. Live sweep behavior and
production activation must be verified during the authorized rollout.

Never send goodwill payments directly to either custody ATA. Pay the user in
plain USDC or transfer LP tokens.
