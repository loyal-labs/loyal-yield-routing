# ONyc zero-start flash-funded exit proof

This offline candidate extends the reviewed prefunded proof in PR #269. Production
permissions, limits, deployed programs and workers remain unchanged. Deployment
is paused pending authorized SSH access and separate production readiness.

## Result and funding boundary

The executed financing custody starts at **zero USDC**. A real top-level KLend
flash loan provides 20,000.000275 USDC. The adapter spends exactly that amount to
clear the debt, existing Squads policies release and sell ONyc, and guarded
settlement returns that same amount to the financing custody. The final real
KLend instruction repays the flash loan. The custody ends at **zero USDC**.

The twelve-instruction transaction is 806 bytes, uses 45 accounts and consumes
780,773 CU in the recorded run. The captured reserve's flash fee is zero. Both the
requested fee and the authenticated reserve fee must be zero; any positive fee
remains unsupported by this candidate.

Vault proceeds are 99,976.001193 USDC. Native NAV reporting, stage/restore and
terminal Voltr accounting pass. The captured LP mint and all 11 holder accounts
remain unchanged. Fee-normalized incumbent LP wealth is 99,944.485645 USDC. These
figures describe a synthetic $100k bank and a $20k under-cap debt position, not a
live investment return or a higher-leverage allocation.

The debt reserve is also the flash lender. Its supply finishes higher by the
actual debt payment; the flash principal itself nets to zero. Its fee receiver
balance stays unchanged. The receipt is consumed once, records begin index 4,
and stores the post-borrow financing balance. Rent and transaction fees remain
separate executor SOL costs.

## Honest planning

Two banks are established before entry: an execution bank with zero financing
USDC and a planning twin with an explicitly declared $100k financing balance.
Both execute the same actual allocation, swaps, deposit, borrow and redeposit.
Before the exit, the test compares every tracked account and Clock; only the
declared financing token amount may differ.

The planning twin runs a real flash/raw-debt-repay/flash-repay sandwich to quote
the exact debt debit under flash refresh ordering. It then executes withdrawal
and sale planning against its evolving state. Only instruction data and quotes
cross into the execution bank. No planner poststate is copied; there is no
post-setup funding, debt/reserve/pool/oracle assignment or fee override.

Both twins use a locally instruction-created lookup table and locally created
Squads settlement policy. The administrator's unsigned authority and synthetic
SOL setup remain explicit assumptions. The vault PDA signs only inside Squads.

## Guard changes

The prefunded ten-instruction grammar remains accepted. Flash mode has exactly
twelve instructions: borrow at 0, begin at 4, settlement at 10 and repayment at 11.
The repayment references **borrow index 0**. Both flash calls remain top-level,
with identical, fully pinned 12-account vectors and exact data shapes. Extra or
reordered calls, different custody/programs and mismatched amount/index reject.

Flash begin authenticates the existing debt reserve's owner, exact size,
discriminator, market, mint, stored supply/fee vaults and token program. The
u64LE flash-fee field at account bytes 4904..4912 must be zero. The actual debt
payment must equal flash principal; a requested upper bound is never reimbursed.
The initial financing balance at begin must equal that principal, excluding a
prefunded-plus-flash path. Existing receipt, closure, canonical Squads envelope,
recipient, retained-vault-cash and one-use settlement guards remain enforced.

## Rejection evidence

All 29 cases check exact errors/stages and complete tracked-account rollback,
except the executor's transaction fee. Removing both flash instructions from the
zero-balance bank fails at the actual SPL debt debit, proving that this exit
requires the financing. It covers missing/changed flash repayment,
wrong index/custody/program/discriminator, duplicate flash calls, reordered or
extra instructions, under/overfunded principal, nonzero starting financing cash,
zero/over-cap principal, nonzero requested fee, partial repayment, wrong or missing
settlement, wrong begin accounts, changed swap topology, impossible swap minimum
and retained-proceeds floor failure.

Pairing failures generally reject at flash borrow before debt repayment. The
retained-floor case fails after actual debt repayment, withdrawal/closure and
sale. These stages are recorded separately. The suite does not claim an executed
failure inside the final flash repayment instruction.

Two isolated parser checks cover nonzero/disabled fee fields, reserve identity
and layout, and exact flash pair bytes/keys/privileges. These are parser-only results; execution against a real nonzero-fee reserve
remains unproved. The test increases log capture
to 64 KiB before initializing the twins because the default 10 KiB truncates the
last instruction's logs. Compute, packet and account limits remain unchanged.

## Migrated compiler compatibility

The proof uses the migrated unsigned native Go exporter from
`codex/workers-v2-golive` at `c3cb6bb602ea301a8f23b4bf3828b3f8b4bed54f`, package
`go/workers/internal/backyard`. Its protocol builders/manifests and NAV/LP math
preserve the old source behavior; runtime credentials, identity, locks,
telemetry and concurrent pricing have separate changes.

Before extending the adapter, the three retained prefunded/current-policy
scenarios passed with this compiler. All 81 native input/output pairs matched
byte-for-byte, including 61 serialized wires. Five additional retained requests
matched for 1x/fractional/1.75x partial sizing, debt-bearing withdrawal and partial
repayment. Eight scoped native regressions also passed. This establishes parity
for those inputs and captured banks; initialize/discover requests, production
admission, signer use, concurrency and service cutover remain outside that proof.

Keep both source identities: the Rust proof tree and the exact migrated Go
compiler tree. The migration branch does not yet contain these Rust proof files.
A future integration must carry the reviewed files and local crate lock entry;
do not replace its entire lockfile or add the candidate to deployment wiring.

## Reproduction

Follow the resource wrapper and fixture instructions in
`onyc-repay-settlement-proof.md`. Use the same captured programs/accounts,
LP-holder capture and Solana platform-tools compiler. Build the candidate SBF
and host harness separately. Every build/test remains bounded to 3 GiB memory,
zero swap, one CPU, 64 tasks, one Cargo job; 900 seconds per build and 120 seconds
per execution. Run one heavy command at a time; stop below 3 GiB free disk.
No validator, node service, RPC, production database or signing is needed.

The migrated exporter is compiled from its pinned module with:

```sh
go test -p 1 -c -o /home/exedev/dev/voltr-handoff-20261002/onyc-flash-exit/onyc-go-c3cb6bb.test ./internal/backyard
```

Run that command inside the bounded wrapper from the exported `go/workers` tree,
with `GOTOOLCHAIN=local`, `GOENV=off`, `GOPROXY=off`, `GOSUMDB=off`, `GOMAXPROCS=2`
and `CGO_ENABLED=0`. The recorded compiler version is Go 1.26.5; this is an offline
test binary, not the deployed engine binary.

Set `ONYC_PROOF_COMPILER` to that binary and `ONYC_SETTLEMENT_PROGRAM_SO` to the
new SBF. Keep the other three fixture variables from the prefunded proof and
choose a new output directory. Run the printed Rust test executable with:

```text
--ignored --nocapture --exact repay_first::settlement::onyc_guarded_flash_exit
```

Require exactly one passing test, plus reruns of both prefunded filters and the
existing-policy denial filter. Compile the two isolated adapter tests with
`cargo test --locked --offline -p onyc-repay-settlement-probe --lib --features no-entrypoint --no-run`,
then execute the printed binary separately under the 120-second wrapper.

Commands, cgroup readbacks, binary/source hashes, migration parity review,
planning provenance and runtime evidence are retained under
`/home/exedev/dev/voltr-handoff-20261002/onyc-flash-exit` and the corresponding
`flash-*` run directories under `onyc-repay-settlement`.

## Remaining boundaries

The candidate is fixed to one operation, route, zero-fee reserve and local policy.
Unsigned simulation does not prove private-key possession or production authority.
Positive fees, runtime fee-configuration rejection against an honest additional
reserve fixture, nested invocation, flash-specific receipt replay and nonzero
existing-vault-cash flash scenarios remain separate coverage. Prefunded replay
and cash-preservation regressions still apply to that mode only.

The 2.94x counterfactual remains a next step requiring real protocol construction.
Current production risk/capital limits stay unchanged. Working SSH alone does
not make this one-shot candidate production-ready.
