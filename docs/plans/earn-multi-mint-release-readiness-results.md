# Earn Multi-Mint Release Readiness Results

Verifier: `docs/plans/earn-multi-mint-release-readiness-verifier.md`

Executed locally on 2026-08-11 against:

- app base `e26f2d18632825a86007789665f9abed38f1ce0d`
- routing base `5125f7f464d15b7ee5624642e5a3fe81cba026fc`
- isolated branch `codex/earn-multi-mint-simple`

No deployment, rollout-control change, signer load, transaction submission, or
on-chain policy mutation was performed.

## Verdict

| Requirement | Result | Evidence |
| --- | --- | --- |
| R0 Scope and reviewability | PASS | Both diffs were reviewed; no migration, ABI, cross-mint, worker-topology, policy-mutation, or non-USDC autodeposit change exists. `git diff --check` passes. The largest automated-formatting diff was mechanically reduced from about 1,100 changed lines to about 260. |
| R1 Fail-closed rollout allowlist | PASS | One app parser defaults missing/blank to USDC, rejects invalid/duplicate values, drives the selector and all three manual-deposit prepare paths, and is not used by holdings/withdrawals. Focused tests cover default, subsets, all six, invalid/duplicate, and disabled-mint rejection. Invalid runtime configuration exits nonzero. |
| R2 User-flow wiring | PASS | Deposit intent remains exact mint plus raw amount; withdrawal intent is exact source ID plus raw amount or Max. Holdings retain distinct idle/reserve sources. Focused UI/domain tests and the withdrawal route suite pass. |
| R3 Policy and transaction safety | PASS | New policy bytes accept both token programs; legacy policy capability is detected without mutation and Token-2022 returns typed update-required. Reserve owner, market, mint, and token program are validated before Kamino instruction construction. |
| R4 Holdings, earnings, and APY | PASS | Complete RPC holdings, all-source zero proof, mint-keyed principal, concurrent source accounting, idle-zero weighted APY, and missing-coverage behavior are covered by focused tests. |
| R5 Routing and publication | PASS | The router has the exact six-mint/token-program universe, preserves mint/program through idle observation and same-mint execution, separates complete publication from bounded patching, and has no capability timestamp/JSON fence. Rust format, check, and tests pass. |
| R6 Build, lint, and regression evidence | PASS | Smart-account typecheck passes; the 1Password-backed production build passes. Eight new TypeScript files pass Ultracite. On the identical 40 modified legacy files, head has 691 Ultracite errors versus 900 at base. Frontend `tsc` has the same single unrelated observability-test error at base and head. The full smart-account client suite is identical at base/head: 31 pass, 9 fail. Focused suites pass. |
| R7 Read-only readiness and handoff | PASS | `verify:earn-multi-mint-readiness` emits bounded per-product JSON, is signerless/read-only, fails invalid configuration, and reports missing local RPC/Timescale evidence as unknown. The rollout runbook defines dark deployment, per-mint enablement, rollback, and independent evidence states. |
| R8 External production evidence | NOT RUN | No deployment or value-moving canary was authorized. `deployed`, `canaryFinalized`, and `userReady` remain false. |

`CODE_RELEASE_CANDIDATE`: **PASS**

`USER_READY`: **false** (`R8 NOT RUN`)

## Commands and counts

- Focused money/wire/snapshot/accounting suite: 54 passed, 0 failed.
- Exact-source withdrawal route suite: 8 passed, 0 failed.
- Smart-account package typecheck: passed.
- Full smart-account client suite: head 31 passed/9 failed; base 31 passed/9 failed with the same test names.
- Frontend production build: passed with existing warnings.
- Frontend TypeScript: head and base each report only
  `src/features/observability/lifecycle-error-detail.test.ts:105`.
- New-file Ultracite: 8 files, 0 errors.
- Legacy modified-file Ultracite: head 691 errors; base 900 errors over the identical 40 files.
- Routing: `cargo fmt --check`, `cargo check -p loyal-yield-orchestrator`, and
  `cargo test -p loyal-yield-orchestrator` passed; the library suite reports
  69 passed and all binary/doc test targets completed without failure.

## Readiness artifact result

With no local read credentials, the report produced all six product rows,
enabled only USDC by default, and returned `dataReady: "unknown"`,
`deployed: false`, `canaryFinalized: false`, and `userReady: false`. An explicit
all-six allowlist marked all six rows enabled without upgrading readiness.
`USDC,BOGUS` exited nonzero with a bounded error.
