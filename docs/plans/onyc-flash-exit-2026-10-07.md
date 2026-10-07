# ONyc zero-start flash exit implementation plan

User approved continued offline work on 2026-10-07, with deployment deferred until
working SSH access and production readiness. No production operation is part of
this plan. Baseline: reviewed prefunded candidate PR269, commit1b82ef918c91cf2c68288f52b1cf854f7e11ef57.

## Design and constraints

Retain the ten-instruction prefunded grammar. Add only the twelve-instruction
variant: top-level KLend flash borrow at0, shifted existing ten instructions,
and matching top-level KLend flash repay at11 referencing borrow index0.
Begin/settle become4/10 in this mode. Keep the same receipt64/PDA/operation1,
policy157, actual-debit reimbursement, canonical Squads envelopes, fixed custody,
closed obligation and retained-cash checks. No flash CPI or transfer allowance.

Authenticate the exact12-account flash vector and16/17-byte instruction data.
Require zero requested fee and authenticated reserve flash fee u64LE at
account bytes4904..4912 equal0. Require flash principal P>0, within the fixed cap,
X at begin=P, and actual measured debt payment=P. The main execution account X
starts at0, receives P, spends P to repay debt, receives P from guarded settlement,
and finishes at0 after the final real flash repayment. Record every boundary.

A separately declared initially-prefunded planning twin may prepare the exit.
Create both twins before capital setup and execute the same actual entry/borrow/
redeposit in each. Compare pre-exit financial state and clock, allowing only the
explicit initial X balance difference. Never copy planner poststate into main,
edit reserve fees/oracles/debt/pools, or add funding after setup. A planning-only
flash/raw-repay/flash-repay sandwich may quote actual debit with real protocol
refresh ordering. Its declared capital must never enter the zero-start proof.

Use in-process LiteSVM only. Every build/check/test runs under User=exedev,
MemoryMax=3G, MemorySwapMax=0, TasksMax=64, CPUQuota=100%, one Cargo job and bounded
900-second builds /120-second executions. Stop below3GiB free disk. Parent owns
and serializes every heavy command; implementers may edit/read only. No validators,
SSH, RPC, secrets, production DB/Render, policy/capital/risk changes or broadcasts.

Design references: artifact `onyc-flash-exit/flash-authority-design.md` and
`migration-parity-audit.md` under `/home/exedev/dev/voltr-handoff-20261002`.

## Tasks

1. Rebuild the migrated native Go exporter from exact source
   c3cb6bb602ea301a8f23b4bf3828b3f8b4bed54f, package go/workers/internal/backyard.
   Run the eight scoped native regressions in the parity audit, compare retained
   requests/wires, and rerun all three reviewed prefunded/current-policy filters
   with the new exporter. Keep the old baseline binary/results unchanged.
2. Add `onyc_guarded_flash_exit` in the existing settlement harness. Establish
   its red run against the prefunded-only SBF. Preserve existing filters.
   Implement only the reviewed grammar/fee/principal extension in
   crates/onyc-repay-settlement-probe/src/{lib.rs,contract.rs}.
3. Prove zero-start positive, exact lender/payer/vault balances, full packet/account/
   CU bounds, receipt consumption, and native terminal Voltr NAV/LP accounting.
   Exercise missing/changed flash repayment, wrong index/custody/amount, overfunded
   principal and nonzero starting X, nonzero requested fee, wrong settlement and
   failure after real debt repayment. Verify complete rollback except transaction
   fees and exact rejection stages; pairing failures at borrow are early failures.
4. Rerun prefunded and legacy checks. Independently review exact sources, SBF,
   both twin provenance, all negative snapshots and terminal ledger. Record claims
   separately: zero-fee flash proof vs positive-fee proof vs signed/admitted/live.
   Publish a focused stacked PR only after passing checks/review. Keep deployment
   paused. Higher-leverage construction follows only after this proof is complete.

## Deferred

Positive reserve fees require an honest additional fixture; never manufacture
one by changing fee bytes in a successful bank. A unit rejection of fee bytes is
not a protocol-level positive-fee proof. Additional malformed/nested coverage and
production admission/cutover are separate from the fixed offline candidate.


## Verified flash milestone — 2026-10-07

Migrated compiler built with Go1.26.5; eight native regressions pass. Three frozen
baseline Rust scenarios pass with identical economic results and81 identical
native outputs. Five extra partial/debt-bearing requests also match. Coverage
is limited to those86 inputs; initialize/discover and production runtime remain
outside the comparison.

The final zero-start flash run passes:806 bytes,45 accounts,780,773 CU,
20,000.000275USDC borrowed/paid/returned; main financing USDC starts/ends0.
All29 negatives roll back except transaction fees. The no-financing control
fails the real SPL debit. Native terminal NAV/LP reconciliation and all three
prefunded/current-policy regressions pass. Two isolated parser checks pass.
Independent source/binary/provenance/evidence review approved this fixed
zero-fee milestone. Source/binary hashes and review live in the artifact root.
No production permissions, limits, deployment or live transactions changed.

Next: separately reviewed instruction-constructed2.94x counterfactual. Preserve
the candidate principal cap and production admission limits. The current flash
proof does not establish positive fees or production readiness.
