# ONyc conditional settlement implementation plan

> Execute inline in the existing isolated ASK-2316 worktree. User approved the offline-only paired adapter on 2026-10-07. The earlier authority review is the design source: `/home/exedev/dev/voltr-handoff-20261002/onyc-repay-first-proof/takeover-authority-review.md`.

**Goal:** prove a complete, sale-funded exit that returns only the actual external debt payment to its payer.

**Architecture:** a test-only SBF adapter records a real KLend V2 repayment and permits one Squads-authorized settlement in that same transaction. Existing collateral release and Jupiter sale policies keep their current destinations. Begin must reject a transaction without its matching settlement; settlement cannot spend without the matching successful begin.

**Tech stack:** existing Rust/Solana 2.3, LiteSVM, captured program ELFs, native Go instruction compiler and existing Squads policy builders. No new external dependency.

## Constraints and acceptance

- Offline only. No production policy/program/manifest, worker admission, DB, Render or risk-limit changes; no live signatures/broadcasts.
- Use in-process LiteSVM only. Every build/test runs under User=exedev, MemoryMax=3G, MemorySwapMax=0, TasksMax=64, CPUQuota=100%; one Cargo job, incremental off, dev/test debug off. Build timeout 900s; test timeout 120s. Serialize heavy work; stop below 3 GiB free disk.
- Reuse the declared $100k flat-vault and $20k under-cap loan setup from PR #267. No reserve/oracle/pool/debt rewrites after initial setup. Retain the original current-permission test unchanged in meaning.
- Initial guarded candidate uses explicit external prefunding and zero financing fee. A zero fee is the first hard fee ceiling, not proof of positive-fee flash financing. Reject nonzero fee requests in that candidate.
- Pin the vault, obligation, market, debt reserve, USDC/classic-token program and payer custody. Reject token delegates/close authorities and account aliases. Actual debit comes from before/after the adapter's real repayment CPI, not the requested upper bound. Repayment must extinguish nonzero debt on the pinned obligation.
- Receipt is adapter-owned, PDA-derived from the pinned obligation/payer/operation identity, and transitions once from absent to active to consumed. A consumed record remains a replay tombstone. Authenticate account owner, seeds, exact data length, version and status. No reset/close instruction.
- Begin is top-level; settlement is the single inner call in the pinned Squads policy envelope. Bind both through the actual instructions sysvar, exact receipt/operation identity and order. Same-slot equality alone is insufficient. No nested begin, standalone return, duplicate settlement, extra unapproved instruction, or successful begin without settlement.
- Settlement requires the vault signer, exact recorded recipient, flat/closed obligation and cleared ONyc custody. Reimburse actual paid principal only in the first candidate. Post-settlement vault USDC must preserve its pre-begin USDC balance; all remaining sale proceeds stay in the vault.
- Never add a raw SPL transfer allowance or executor-owned Jupiter output. New policy is created through actual Squads instructions in the local bank; any initial admin funding and unsigned governance assumption are explicit evidence limitations.
- Enforce 1,232-byte packet fit, actual real-EOA signer topology, account and compute limits before claiming composition feasibility. Candidate ALT/policy creation must have separate recorded provenance.

## Task 1 — complete native exit composition feasibility

Files: `crates/squads-test-harness/tests/onyc_offline_lifecycle/repay_first.rs` (child module declaration); `crates/squads-test-harness/tests/onyc_offline_lifecycle/settlement.rs` (opt-in scenario).

- [x] Create `onyc_atomic_exit_prefunding_feasibility`: execute the existing native $20k loan setup, then plan actual external repay, policy-bound release and sale using an evolving clone. Decompile native wires with the captured ALT; recompile the entire instruction list with the actual executor as the only top-level signer.
- [x] Record packet/static/loaded account/instruction counts before trying execution. Assert the full prefix fits 1,232 bytes. An oversized prefix is a real design blocker, not permission to disable the limit.
- [x] Run the complete prefix against the original bank, confirm zero remaining debt/collateral, compare final token balances with planning execution, and retain exact wire/CU/logs/account snapshots. `prefix-alt-01`: 605 bytes, 40 accounts, 527,538 CU, one test passed. Financier reimbursement and full financed exit remain false.
- [x] If the existing ALT is insufficient, measure missing keys and evaluate a separately provisioned candidate ALT through actual local table instructions before building the adapter. Do not falsify captured ALT provenance.

## Task 2 — guarded prefunded reimbursement

Files: test-only SBF crate under `crates/onyc-repay-settlement-probe/`; `settlement.rs`; workspace lock only for the local crate; no production build/deployment wiring.

- [x] Add one positive scenario asserting executor final USDC equals its initial prefunding and vault USDC equals initial cash plus actual sale proceeds minus measured debt debit. Establish its expected failure before implementing reimbursement.
- [x] Implement paired begin/settle instructions with the invariants above, narrow route-specific constants and existing SPL/System/KLend interfaces. Reuse existing Squads wire/policy builders in the harness; any small on-chain Squads envelope decoder must have SDK wire parity checked by execution.
- [x] Run the candidate as SBF, not a permissive host mock. Create its candidate settlement policy via the captured Squads program. Keep the original current-policy denial tests passing.
- [ ] Exercise missing/duplicate settlement, standalone return, replay/stale receipt, wrong payer/recipient/vault/obligation/reserve, amount clamping, excessive fee, nested invocation, unexpected instruction and insufficient sale proceeds. Assert exact errors, rejection ordering and complete tracked-account rollback except transaction fees.

## Task 3 — terminal accounting and flash extension

- [x] After guarded prefunded success, run native Voltr restore and NAV/LP accounting. Reconcile external funds, vault proceeds, fee dilution, residuals and rent without counting reimbursement as profit.
- [ ] Add top-level flash borrow/repay only after binding exact payer/custody/reserve/amount/index and actual fee computation. Reject unnecessary principal and over-reimbursement. Missing/altered final flash repayment must roll back all capital state.
- [ ] Verify full packet/account/CU fit anew. Keep positive flash-fee proof false unless an honest positive-fee fixture executes it; never edit reserve fees to manufacture passing evidence.
- [x] Retain source/binary hashes, commands, logs and limitations. Review the final adapter boundary before any PR/publication claim. Only then consider a separately instruction-constructed 2.94x bank; live leverage remains unchanged.


## Verified prefunded checkpoint — 2026-10-07

The fixed prefunded candidate and its nonzero-existing-cash scenario each pass:
742 bytes, 44 accounts, 707,702 CU; 24 transaction negatives and two historical
receipt-reuse negatives; native terminal NAV/LP reconciliation. The old
existing-permission probe also passes. `prefunded-final-review.md` independently
approves only this bounded milestone and verifies source/SBF hashes plus all 52
negative snapshots across the two runs.

Runtime coverage still excludes explicit nested calls, malformed state images
and actual sale proceeds below repaid principal. The retained-floor and impossible
DEX-minimum cases prove rollback, not those omitted scenarios. Flash funding,
positive financing fees and 2.94x remain pending. See
`docs/onyc-repay-settlement-proof.md` for commands and exact claim boundaries.
