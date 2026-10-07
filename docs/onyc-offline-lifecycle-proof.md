# ONyc offline native lifecycle proof

This opt-in proof uses the current Go unsigned compilers and public program/account snapshots in LiteSVM. It never starts a validator, reads secrets, signs, submits a transaction, or touches a database. It is not worker admission, queue, restart, production rotation, or signature proof.

## Inputs and safety

- Source base: `e2384fb`.
- Native test exporter: `go/backyard-rwa-worker/internal/backyardrwa/onyc_offline_proof_test.go`.
- Connected bank: `crates/squads-test-harness/tests/onyc_offline_lifecycle.rs`.
- Public fixture/result directory for this run: `/home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof`.
- `fixture-current/snapshot.json` pins account data hashes, current ELF hashes and capture slots. Historical candidate snapshots are not execution inputs.
- All compiler/test commands must run inside the hard resource wrapper below: 3 GiB memory, no swap, 64 tasks, one CPU. Stop if disk free falls below 3 GiB. Run one command at a time. Never start validators or node services.

The first request validates installed policies 141–144/151 and normalized bridge policies 152–155 against the embedded manifest. The native compiler exports its exact account needs with `operation: discover`. All subsequent requests contain the actual evolving bank accounts. Missing/mismatched policy bytes fail closed.

The only synthetic setup is a flat $100,000 book: idle USDC and totalValue become 100,000,000,000 raw; current receipt NAV/custody become zero; locked profit becomes zero; the high-water mark is rebased against unchanged effective LP supply. Circulating LP mint supply, holders, fee accumulators and fee rates remain unchanged. This is not a migration of current AUTO assets. ONyc must already be flat. Initial account before/after bytes and hashes are recorded. No policy, pool, reserve, oracle, farm or obligation override is permitted. No account override is permitted after execution begins.

## Resource-bounded commands

From the proof worktree, compile the Go test exporter (no test or production main runs):

```sh
sudo -n systemd-run --wait --collect --pipe --unit=onyc-proof-go-build-unique \
  --property=User=exedev --property=MemoryMax=3G --property=MemorySwapMax=0 \
  --property=TasksMax=64 --property=CPUQuota=100% --property=RuntimeMaxSec=900s \
  --working-directory=/home/exedev/dev/loyal-yield-routing-ASK-2316-onyc-proof/go/backyard-rwa-worker \
  --setenv=GOMAXPROCS=2 -- /usr/local/bin/go test -p 1 -c \
  -o /home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof/overflow-fix/onyc-go-fixed.test ./internal/backyardrwa
```

The Rust test uses `ONYC_PROOF_DIR`, `ONYC_PROOF_OUTPUT`, `ONYC_PROOF_COMPILER`, and `ONYC_PROOF_LP_HOLDERS` (the public mint-filtered holder response). `ONYC_PROOF_CASE` selects `1x`, `1.75x`, or `fractional`. The fractional case explicitly requests $20k below the native borrow cap; it does not claim reserve exhaustion. `ONYC_PROOF_PARTIAL` defaults to `required`; only the explicit `skip` value runs a separately labelled full-exit-only control. Run `cargo test -p squads-test-harness --test onyc_offline_lifecycle --no-run` under the same wrapper with a 900-second build cap and these variables:

```text
CARGO_TARGET_DIR=/home/exedev/dev/loyal-yield-routing/target
CARGO_BUILD_JOBS=1
CARGO_INCREMENTAL=0
CARGO_PROFILE_DEV_DEBUG=0
CARGO_PROFILE_TEST_DEBUG=0
```

Execute the built test with `--ignored --nocapture` inside a fresh wrapper with `RuntimeMaxSec=120s`. Keep the same resource limits. Each test's Go child inherits its parent's cgroup. Record the unit before starting. To cancel, stop the unit with `sudo -n systemctl stop <unit>`; stopping only the `systemd-run` client can leave work running.

## Evidence and limits

Each native request and compiled zero-signature wire is retained. Each executed leg records input amount, wire SHA-256, packet/static-account/ALT/instruction counts, compute units, logs, and all observed account pre/post images/hashes. The source account capture and ELF deployment captures can have different slots; deployment identities must predate the account snapshot. Clock advances by one slot and one second after each executed transaction. Amounts are built and executed at the same observed clock. Oracle bytes are never changed. This exercises bounded debt accrual, not long-horizon market moves.

Baseline: `baseline-historical.log` fails at installed policy mismatch. The historical candidate policies and old report ticket cannot be silently used as current proof. The test-only new path does not change the historical harness's labels or behavior.

A compiled harness is not a passing lifecycle. The emitted result must explicitly state which connected invariants passed. An ignored/skipped test is not execution evidence.

## Current findings

- The current $100k native entry succeeds. No1000-raw replacement is used.
- Full exits pass for1x, a native capacity-sized1.75x ceiling, and an explicitly requested$20k under-cap loan. Both loans are fully swapped/redeposited before any repayment. Each repayment is funded only by released/sold ONyc. Every capital mutation is followed by a native NAV report.
- Clone-only quotes execute freshly constructed single-hop WhirlpoolV2 instructions through the Go compiler. Quote probes preserve the main bank. The exact main-bank wire enforces a 50bps minimum from the executed clone output. A separate impossible-minimum clone fails with capital/program state unchanged; only its fee payer may pay fees. These are offline execution quotes, not production Jupiter responses or worker admission.
- Pool state evolves through actual swaps. No pool poststate is assigned. Immutable ELF bytes are pinned at load rather than copied into each leg's input. Mutable account bytes/hashes are retained throughout.
- All 11 captured LP holders sum to 3,255,644 raw, equal to the captured mint supply. The holder capture is atslot 452734177; the main snapshot is 452734005. This separate-slot limitation is explicit. LP mint/holders and fee configuration remain unchanged. Native fee LP accrues normally through the intermediate NAV reports.
- The original $10k partial withdrawal exposed an `int64` overflow in `partialWithdrawalReleaseReceipts`. `control-04` retains the failure: the captured $100k case returned 0 instead of 8,770,549,397,934 receipts. Vlad approved the narrow local fix. Multiplication and division now use the existing `math/big` library with checked conversion back to `int64`; floor rounding and risk caps stay unchanged. The three connected partial-plus-full-exit cases pass with this fix. PR #266 deployed this arithmetic fix as `64743c6` on 2026-10-03; deployment health was verified separately from these offline proofs.

`fullExitPass` and `fullLifecyclePass` are separate. Skipping a partial withdrawal cannot produce `fullLifecyclePass:true`. Earlier diagnostic outputs are retained, including the closed-obligation assertion failure after a successful instruction chain; they are not final passing results.

No pending receipt execution, LP redemption, withdrawal queue, durable restart, selector admission, single-use loan authorization, spend-budget admission, fresh reserve-exhaustion trigger, deployed code change, or production signature is proved. Partial proceeds restored to Voltr idle are not a user withdrawal payment. Cost results report gross idle and fee-diluted incumbent LP wealth separately. Executor SOL/rent cost is excluded from USDC capital totals.

## Retained pre-fix full-exit artifacts (2026-10-02)

All values below are USDC. These are synthetic $100k entry/full-exit proofs, **not partial-withdrawal passes**.

| Case | Borrowed | Terminal gross idle | Fee-diluted incumbent capital cost | Result directory |
| --- | ---: | ---: | ---: | --- |
|1x|0|99,980.001186|46.330679|`control-full-02`|
|Native 1.75x ceiling|75,030.795019|99,964.993261|80.596175|`leverage-full-03`|
|Explicit under-cap loan|20,000.000000|99,976.001009|55.514539|`fractional-full-02`|

The normalized incumbent capital measure is `terminalIdle * initialEffectiveLPSupply / terminalEffectiveLPSupply`. Cost is$100k minus that measure. All quantities use integer floors. Actual circulating-holder equity is also reported separately. Initial effective LP supply is 3,256,900; circulating supply is 3,255,644; existing accrued admin fee LP is 256; dead weight is 1,000. Terminal admin fee LP is 1,114 / 1,742 / 1,283 respectively. The three executions advance 12 / 36 / 24 seconds. The 1.75x run uses one partial repayment cycle before final payoff; this is not a partial customer withdrawal.

`final-source-sha256.json` pins the tested Rust/Go harness sources, production builder/partial-sizing sources and compiled Go binary. `final-results-summary.json` contains the table's raw integers. Every final positive result has `fullExitPass:true`, `fullLifecyclePass:false`, and `partialWithdrawalProved:false`.

The pre-fix required partial case remains in `partial-blocked-final.log` and its input/output directory. The original red regression remains in `go-final-checks.log`. The regression now runs by default as `TestONycPilotSizedPartialWithdrawalDoesNotOverflow`, with captured 1x, 1.75x and fractional cases. Only the overflow calculation in the production helper changed; the earlier files and failure evidence remain intact.

## Approved local fix and complete offline reruns

New artifacts live under `overflow-fix/`. All three results have `fullExitPass:true`, `fullLifecyclePass:true`, and `partialWithdrawalProved:true`. The requested partial shortfall is $10,000; the sizing includes the existing 1% buffer.

| Case | USDC restored by partial withdrawal | Final total gross idle | Fee-diluted incumbent capital cost | Artifact |
| --- | ---: | ---: | ---: | --- |
|1x|10,084.875181|99,980.001186|46.330679|`control-partial-01`|
|Native 1.75x ceiling|10,071.740811|99,964.992076|80.597360|`leverage-partial-01`|
|Explicit $20k under-cap loan|10,076.732552|99,976.000680|55.514868|`fractional-partial-01`|

Final gross idle includes the partial proceeds; they were not paid to a user in this test. It must not be added to the partial-restoration column. The 18 / 44 / 32-second executions preserve the same timing, program, pool, policy, fee, signature and worker-admission limitations documented above. Partial cash stays in Voltr idle while the remaining position funds its own debt repayment.

Focused Go checks: 13 top-level passes, zero failures, one isolated-database test skipped. The regression's three subcases pass. Each connected Rust case passes with the required partial path enabled. `overflow-fix/independent-review.md` records the scoped static review. Source/binary hashes, result summaries and runtime logs are retained in that directory. No live transaction, validator, deployment or risk-limit change occurred.

### Exact Rust execution example

After the bounded compilation above, run one case at a time. Change the unit/output name for each run. Use `ONYC_PROOF_CASE=1.75x` or `fractional` for the other complete offline cases. The required partial path is enabled below. An explicit `ONYC_PROOF_PARTIAL=skip` still runs a separately labeled full-exit-only control.

```sh
sudo -n systemd-run --wait --collect --pipe --unit=onyc-proof-control-unique \
  --property=User=exedev --property=MemoryMax=3G --property=MemorySwapMax=0 \
  --property=TasksMax=64 --property=CPUQuota=100% --property=RuntimeMaxSec=120s \
  --working-directory=/home/exedev/dev/loyal-yield-routing-ASK-2316-onyc-proof \
  --setenv=CARGO_TARGET_DIR=/home/exedev/dev/loyal-yield-routing/target \
  --setenv=CARGO_BUILD_JOBS=1 --setenv=CARGO_INCREMENTAL=0 \
  --setenv=CARGO_PROFILE_DEV_DEBUG=0 --setenv=CARGO_PROFILE_TEST_DEBUG=0 \
  --setenv=ONYC_PROOF_DIR=/home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof/fixture-current \
  --setenv=ONYC_PROOF_LP_HOLDERS=/home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof/lp-filtered-accounts.json \
  --setenv=ONYC_PROOF_OUTPUT=/home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof/control-unique \
  --setenv=ONYC_PROOF_COMPILER=/home/exedev/dev/voltr-handoff-20261002/onyc-offline-proof/overflow-fix/onyc-go-fixed.test \
  --setenv=ONYC_PROOF_CASE=1x --setenv=ONYC_PROOF_PARTIAL=required \
  -- /home/exedev/.cargo/bin/cargo test -p squads-test-harness \
  --test onyc_offline_lifecycle -- --ignored --nocapture
```

`fixture-current` also contains `entry-swap-request.json` and `exit-swap-request.json`, constructed from the captured current Jupiter response and captured ALT. The recorded HTTP quote is used for the first $100k entry. Subsequent swaps build new structured instruction data and use clone-executed quotes; they never resize a serialized wire.


## Repay-first existing-permission probes (2026-10-02)

The opt-in `repay_first::onyc_existing_boundary_repay_first` test adds offline
protocol probes in the crate-relative `tests/onyc_offline_lifecycle/repay_first.rs`.
Use the resource-bounded command above, replacing its working directory with
`--working-directory=/home/exedev/dev/loyal-yield-routing-ASK-2316-repay-first`
and choosing a new `ONYC_PROOF_OUTPUT` directory. Use this filter after `--`:
`--ignored --nocapture repay_first::onyc_existing_boundary_repay_first`.
Require exactly one passed test; zero matched tests is not verification.
The Go compiler, public snapshot and LP-holder capture remain the same.

Final evidence: `/home/exedev/dev/voltr-handoff-20261002/onyc-repay-first-proof/mechanics-final`.
The final test passed. Its wrapper verified 3 GiB memory, no swap, 64 tasks,
one CPU and UID 1000 from inside the running cgroup. No validator or live
transaction was used.

Proved against the captured programs and accounts:

- A $100 top-level flash borrow/repay pair succeeds. Missing repayment and wrong
  amount/index/custody reject with exact KLend errors 6032/6033 and roll back.
  The captured flash fee is zero; positive-fee repayment remains unproved.
- A native $100k vault entry borrows $20k, swaps it and redeposits it within
  existing risk limits. A separate executor-owned account repays the obligation
  directly. Requested payoff is 20,000,005,350 raw USDC; actual payer debit is
  20,000,000,275 raw. Reimbursement must use actual debit, not the requested amount.
- Current Squads policies reject executor reimbursement and a substituted Jupiter
  output account with error 6069 (`ProgramInteractionAccountConstraintViolated`).
  The allowed stage destination succeeds as a positive control.
- Prefunded repay/denied-return and flash-borrow/repay/denied-return prefixes
  roll back all tracked accounts, except the executor's 5,000-lamport transaction
  fee. These prefixes fit in 883 and 978 bytes. Full exit bundle fit is unproved.

The separate executor account starts with explicitly synthetic $100k. The flash-only
bank has a separate $1 synthetic fee reserve, unused by this zero-fee capture.
The synthetic setup records initial funding before execution. Subsequent reserve,
pool, oracle, policy and obligation state comes from protocol execution. Native
lifecycle steps advance one slot and one second. Dependency probes use a fixed
clock, as recorded in `probe-scope.json`. Only the on-curve executor is a top-level signer.
Zero-signature offline execution does not prove signatures or production admission.

After external repayment and the unchanged withdrawal/sale, the vault holds
119,976,001,468 raw USDC and the executor holds 79,999,999,725 raw. The executor
has received no reimbursement. Accordingly, `fullExitProved` and
`selfFinancingExitProved` remain false. No higher-leverage position was constructed.

The next proposed change is a separately reviewed conditional reimbursement
instruction that binds the actual debt payment, recipient, fee and same-transaction
settlement. A raw transfer allowance is insufficient. No new permission, adaptor,
production builder, deployment or risk-limit change is included in these probes.
