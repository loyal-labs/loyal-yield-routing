# Exact 2000-bps Voltr calibration

`voltr_fee_2000_calibration` in `crates/squads-test-harness/tests/voltr_reset_sequence.rs` is a focused ignored LiteSVM proof. The historical reset workflow remains separate. The proof checks the pinned Voltr and custom-adaptor executable hashes before loading them.

Run from the repository root:

```sh
CARGO_BUILD_JOBS=2 CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=0 CARGO_PROFILE_TEST_DEBUG=0 cargo test -p squads-test-harness --test voltr_reset_sequence voltr_fee_2000_calibration -- --exact --ignored --nocapture
```

Optionally set `VOLTR_FEE_CALIBRATION_OUTPUT` to an existing directory's JSON output path. The file records actual transaction logs, clock, before/after account bytes, fee terms, HWM bits/timestamps and withdrawal quote checks. `assertionsPassed` is written only after all assertions pass.

The proof uses captured public **mainnet** binaries/accounts from `fixtures/voltr-repair/_manifest.json`. It then seeds synthetic healthy local capital, supply and HWM states. The old strategy receipt is explicitly seeded as version 2, matching the current worker contract. Captured fixtures and synthetic state supply all inputs. The test runs locally with signature verification disabled and default signatures. Transactions stay inside LiteSVM. In-process LiteSVM supplies the runtime. These state overrides are limited to local fixtures. Fee changes, calibration/reset of an existing production HWM, production harvesting, and worker latch clearance/rearming each require separate authorization.

The approved tuple is manager performance 0, admin performance 2000, and all management, issuance, redemption and protocol performance fees 0; profit degradation is 0. The matrix checks report gain/equality/loss/recovery/repetition and one-raw-unit rounding; 0→2000 ordering; pure capital allocation/return versus real gain on both entrypoints; deposits, escrow supply, stored/current claim caps, dilution cancellation/re-request; valid fee ownership above 1% after accrual and withdrawal; and unsigned local harvest supply parity.

The pinned executable accrues fee LP while leaving gross strategy NAV intact. A 100,000-raw gain from assets/effective LP of 1,000,000 accrues 18,519 admin LP and net HWM bits `303992831141806`. Fees use the existing HWM and positive report profit. The fee-setting timestamp leaves that HWM unchanged. A zero-fee report advances HWM; the fee config instruction preserves it. For synthetic unchanged/loss reports above a low existing HWM, fees remain zero and HWM advances. A positive one-raw-unit report can crystallise old above-HWM profit. Two 50,000-raw gain reports produce final effective LP `1018880` and price `1100000/1018880`, versus `1018519` and `1100000/1018519` for one 100,000-raw report. Repeated dilution and rounding put holder gain below nominal 80% of total gross gain. A flat 20% haircut is a nominal forecast proxy; a guaranteed lower bound needs explicit cadence, HWM and accrual-state assumptions.

This proof provides local executable evidence. Production activation and end-to-end worker verification remain separate work. Coverage excludes other nonzero fee terms, nonzero degradation, concurrent/report-interleaved user flows, every U80F48/u64 boundary or worker decision persistence. Live fixture refresh would be a separate explicit read-only mainnet operation; this test never fetches fixtures automatically.
