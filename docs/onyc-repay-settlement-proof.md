# ONyc guarded prefunded exit proof

## Scope

This is an offline candidate for ASK-2316. Existing deployed permissions, worker
admission, risk limits and capital limits remain unchanged. The new SBF program
is isolated from production deployment wiring. Its fixed route, operation identity `1`, candidate
policy seed `157`, zero financing fee and captured one-hop swap topology are
intentional limits of this proof.

The existing policy-only probes in PR #267 still reject reimbursement. This
candidate adds one locally created Squads policy that can call only the guarded
settlement entrypoint. Raw SPL transfer authority and alternate Jupiter output
custodies remain absent.

## What executes

1. Create a candidate lookup table through actual local ALT instructions. Warm it
   by one slot before entry; preserve oracle bytes and the current timestamp.
2. Load the candidate SBF and create policy `157` through the captured Squads
   program. Record the unsigned administrator assumption, synthetic SOL funding,
   policy wire, program hash and lookup-table provenance.
3. Build the existing native $100k vault position and $20k under-cap loan through
   real allocation, swaps, deposit, borrow and redeposit instructions.
4. Execute ten instructions atomically: reserve/obligation refreshes; adapter
   repayment; fresh reserve/obligation refreshes; existing-policy collateral
   withdrawal and sale; guarded Squads settlement.
5. Run the native NAV report, stage proceeds, restore Voltr idle and report again.
   Compare final NAV, circulating LP supply, mint/holder bytes and fee settings.

The adapter authenticates the full instruction sequence before repayment. It
measures payer USDC immediately around the actual KLend V2 CPI, including its farm
update. Settlement returns only that measured debit. The receipt PDA transitions
from absent to active to consumed; the consumed account remains as a replay
record. Receipt rent and transaction fees are separate SOL costs.

The bound withdrawal closes the obligation. Settlement accepts only its exact
System-owned, empty, zero-lamport closed state after the receipt authenticated a
live obligation at begin. It also requires empty ONyc custody and preserves all
USDC that was in the vault before begin. Other live-flat obligation shapes are
unsupported by this first candidate.

## Results

The original captured lookup table produced a 1,566-byte repay/release/sale
prefix, above Solana's 1,232-byte limit. A genuinely created local candidate table
reduced the prefix to 605 bytes; it executed in 527,538 CU. The guarded version
fits in 742 bytes with 44 accounts and uses 707,702 CU in the final run.
The final matrix has 24 transaction negatives plus two receipt-reuse cases.

The prefunded payer starts with $100k of declared synthetic USDC. Its actual
repayment is 20,000.000275 USDC, below the requested 20,000.005350 USDC. Settlement
returns exactly 20,000.000275 USDC. The payer's net USDC spend is zero. Vault
proceeds of 99,976.001193 USDC are restored to Voltr idle; the native fee-adjusted
incumbent LP wealth is 99,944.485645 USDC. Circulating LP supply and all captured
holder bytes remain unchanged. These are synthetic-bank execution results, not
live investment returns.

A second scenario transfers 100 USDC from the declared external wallet to the
vault through a real SPL instruction before begin. After repayment/settlement,
the payer returns to its post-contribution balance and the vault retains
100,076.001193 USDC. The contribution is recorded separately from financing.

Negative cases require exact errors and compare all tracked account snapshots;
only the executor's transaction fee may remain charged. They cover missing,
duplicate, standalone and malformed settlement; wrong policy, recipient,
operation and input accounts; nonzero financing fee; zero/partial/over-cap
payment; changed swap topology; impossible swap minimum; retained-proceeds floor;
and consumed/stale-active receipt reuse. The failed final settlement executes
repayment, obligation closure and sale first, then rolls all of them back through
Squads. Historical receipt images are adversarial fixtures only; no financial,
reserve, oracle or pool state is replaced in those cases.

The retained-proceeds negative raises the required retained balance above the
available sale surplus. Actual sale output below principal, explicit nested calls and malformed-state
runtime cases remain unproved. Stack/account checks are present in source.

## Reproduction

Use the public snapshot, native Go exporter and LP-holder capture documented in
`onyc-offline-lifecycle-proof.md`. The test-only crate is
`crates/onyc-repay-settlement-probe`; its source must be built as SBF, not registered
as a host mock. Use the installed Solana platform-tools compiler. The verified
machine uses `1.89.0-sbpf-solana-v1.52`; stock Rust cannot build the SBF target.

Every command must run under the existing resource wrapper: `User=exedev`,
`MemoryMax=3G`, `MemorySwapMax=0`, `TasksMax=64`, `CPUQuota=100%`, one Cargo job,
incremental off, dev/test debug off. Run only one heavy command at a time and
stop below 3 GiB free disk. Build limit is 900 seconds; each test limit is 120
seconds. No validators or local node services.

Build the SBF with the following command inside that wrapper, setting `PATH` to
include Cargo and Solana and `RUSTC` to the platform-tools compiler:

```sh
cargo-build-sbf --manifest-path crates/onyc-repay-settlement-probe/Cargo.toml   --sbf-out-dir /home/exedev/dev/voltr-handoff-20261002/onyc-repay-settlement/sbf-final   --offline --jobs 1 --no-rustup-override -- --locked
```

Compile the host harness separately with the 900-second wrapper:

```sh
cargo test --locked --offline -p squads-test-harness --test onyc_offline_lifecycle --no-run
```

Run the printed test executable under a fresh 120-second wrapper for each filter.
Use `--ignored --nocapture` and require exactly one passing test per command:

```text
repay_first::settlement::onyc_guarded_prefunded_exit
repay_first::settlement::onyc_guard_preserves_existing_cash
repay_first::onyc_existing_boundary_repay_first
```

Set `ONYC_SETTLEMENT_PROGRAM_SO` to the SBF `.so` in the output directory above.
Keep `ONYC_PROOF_DIR`, `ONYC_PROOF_COMPILER` and `ONYC_PROOF_LP_HOLDERS` from the
existing proof. Give each run a distinct `ONYC_PROOF_OUTPUT`. Exact executed
commands, cgroup readbacks, source/binary hashes and results are retained under
`/home/exedev/dev/voltr-handoff-20261002/onyc-repay-settlement`.

## Remaining boundaries

The proof uses unsigned simulation with actual on-curve signer identities. Private-key possession, production policy approval, worker admission/recovery,
queue redemption and live withdrawal are outside this proof. Squads supplies
the vault PDA signature for inner calls only.

Flash-funded exit, positive financing fees and the instruction-constructed 2.94x
scenario remain unproved. The adapter deliberately rejects flash instructions
and nonzero fees. Extending that grammar and accounting requires fresh tests and
review. Existing production borrowing limits remain unchanged.
