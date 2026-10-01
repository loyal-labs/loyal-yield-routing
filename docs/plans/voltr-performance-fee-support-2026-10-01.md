# Voltr 20% performance fee support

## Scope

Base: live worker `afbe2ae`. Approved code changes accept exactly admin performance
2000 bps and seven zero fee terms, make the 1% accumulated-fee ratio warning-only,
and report routine NAV hourly. Withdrawal, safety and post-transaction reporting
keep their existing priority. No fee/HWM reset, harvesting, new dependencies,
production writes, deployment, latch clearance or monitor rearming is included.

## Implementation and verification

- [x] Checked fee/effective-LP arithmetic and exact fee tuple; preserve gross NAV,
  stored withdrawal ceilings and book/custody/ticket/program guards.
- [x] Pinned unsigned program proof: 58 transactions, including repeated fees,
  withdrawal dilution and pilot-size minute/ten-minute/hourly report rounding.
- [x] Hourly routine reporting and matching report freshness; one-minute
  withdrawal/capital-mutation fallback and immediate required reports unchanged.
- [x] Candidate fee reserve uses net NAV rise to its peak, hourly reports,
  bounded transition recipes and rounding. KEEP uses a terminal-fee upper bound
  that excludes initial Q48 dust from guaranteed profit. Display APY remains an
  explicitly nominal continuous fee-paying estimate.
- [x] Scoped review passed after reproducing and correcting its Q48 counterexample.
- [x] Focused checks: 185 pass before the final additional precision regression;
  final race checks: 48 pass, zero skips. Vet and worker build pass.
- [x] Eight targeted database permission/persistence contracts pass on the existing
  disposable local test database. Fixtures now carry coherent fee evidence.
- [x] Final full suite: 1392 test/subtest passes, 95 skips, and the same seven
  top-level SDK-parity failures plus seven nested cases as baseline. No new failures.
- [x] Read-only live preflight at 2026-10-01 17:52:07 UTC: LTV 42.82%, no
  unfinished operation or withdrawal demand, approved fees and coherent HWM.
  Hypothetically removing only the durable latch selects REPORT_NAV. No latch was changed.

## Activation gates

- [ ] Approve deployment of the exact reviewed release SHA, latch clearance and monitoring restart.
- [ ] Build/publish that SHA through CI; confirm an idle operation window and deploy its pinned image.
- [ ] Verify lease handover while still latched, repeat fresh state checks, then clear only the resolved latch.
- [ ] Confirm a successful on-chain NAV report, reconciled fee accounting and no new latch; monitor failures as well as success.

Evidence: `/tmp/voltr/incident-20261001-fee/`, especially `hourly-review.md`,
`hourly-live-preflight.json`, `hourly-final-full.jsonl`, `hourly-final-race.log`,
`hourly-db-core.log`, `hourly-db-final.log`, `hourly-db-recheck.log`, and
`calibration/pilot-rounding-results.json`. Other skipped DB cases are not claimed verified.
