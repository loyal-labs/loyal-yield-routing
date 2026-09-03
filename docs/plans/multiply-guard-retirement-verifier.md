# Multiply Guard Retirement Close-out Contract

Status: approved close-out contract v2

This document is the sole completion contract for retiring the obsolete Multiply guard and registry execution layer. It supersedes `docs/plans/guard-registry-decommission-plan.md` and v1 of this contract.

## User-approved scope change

On 2026-09-03 the user explicitly removed refunding and closing every residual guard-owned account from the required outcome. The exact non-runnable loader-v3 shell and four pinned registry/staging accounts are accepted inert residue. Their rent remains stranded but no user assets are at risk.

This scope change removes the loader-v4 activation gate from completion. It does not reinterpret the prior result: v1 was correctly blocked while full recovery was mandatory.

## Objective

Retire the obsolete guard/registry/hook execution surface while preserving the live NAV adaptor, generic Squads hook ABI, current hookless PRIME/USDC Phase 1 route, and installed Phase 2 policy catalog. Prove the accepted residue is still exactly frozen and cannot execute because its ProgramData account is absent.

## Scope and authority

This close-out is read-only on mainnet-beta. It authorizes no transaction, deployment, policy mutation, account closure, refund, authority change, user-asset movement, or Phase 2 runtime change.

The live NAV adaptor is required by Phase 1 and remains deployed and unchanged. Generic Squads `pre_hook` and `post_hook` fields remain because they are part of the upstream wire ABI; the active route continues to encode both as `None`.

## Hard constraints

- RPC must identify Solana mainnet-beta.
- Guard ProgramData `Hke6Nd6i5PkAEpGZGbjLf7sEc1TM48NGGjTjRQhnqX1G` remains absent.
- Retired hook policies `633UHSciFmPCr2dysjEEHq1pG1kx1E3Kk6W9d9JQSL5g` and `GUGvmxsqAvneNJoxx1FJJPpou9hckGhkiwSQse7ijqzx` remain absent.
- The accepted program residue is exactly program id `8moAa3vXstMPop9FtEnhTDRmcyo9HPn1CsywGMZ9K9n8`, owned by loader-v3, executable-flagged but non-runnable without ProgramData, 36 bytes, `1141440` lamports, data SHA-256 `5cbbdaf8e06df9ad669fc80fa2da8f31e1aa61ad0df620a94725cb042b2b5c85`.
- The four accepted guard-owned accounts retain their pinned owners, lengths, lamports, and SHA-256 data hashes recorded in `docs/evidence/multiply-guard-retirement/blocked-loader-v4-baseline-v1.json`. Any unexplained mutation or additional guard-owned account is outside this acceptance and fails closed.
- The repository contains no guard program, registry/recovery runtime, retirement sender, or guard deployment target.
- Preserve the NAV-adaptor deployer and generic Squads hook ABI.
- Do not alter Squads settings, membership, threshold, timelock, policy seed, installed Phase 1/Phase 2 policies, vault state, NAV adaptor code/config/report ticket, worker deployment, or database state.
- Do not install, recreate, close, or renumber policies.
- Do not implement Phase 2 route selection, switching, or optimization in this goal.
- Retained lifecycle and simulation artifacts are evidence, not replay gates.

## Completion conditions

The sole verifier returns `PASS` only when all conditions hold:

| ID | Condition |
| --- | --- |
| G01 | Mainnet readback exactly matches the accepted inert residue: pinned shell and four accounts present and unchanged; ProgramData and retired hook policies absent. |
| G02 | The repository has no guard/registry/recovery execution surface or guard deploy target; the NAV-adaptor deployer and generic Squads hook ABI remain intact. |
| G03 | The current v12 RWA verifier passes against live state, proving the deployed NAV adaptor, hookless Phase 1 path, exact 70-policy Phase 2 catalog, PRIME/USDC worker, and identity-valid retained lifecycle evidence. |
| G04 | The v2 contract and sanitized residue baseline are present and exact; the verifier is read-only and reports no broadcast. |

The sole command from the repository root is:

```sh
op run --env-file=.env.1password -- bun run verify:multiply-guard-retirement
```

Verdict semantics are `PASS`, `FAIL` with the first false condition and resume action, or `BLOCKED` only when required read-only evidence is unavailable.

## Proven evidence appendix

- The v2 sole verifier passed at confirmed slot `443879573` with `broadcast: false`, exact four-account owner enumeration, and no blocker.
- The v1 verifier baseline at slot `443879084` returned `BLOCKED` only because total recovery was then mandatory; G03 and G04 passed.
- PR #178 merged at `cc19ca4d23556423a539cbd6d9595d9ef0c5936e`, removing the guard deploy target and retaining an adaptor-only deployer.
- Phase 1 completed one finalized and reconciled PRIME/USDC lifecycle with the live NAV adaptor.
- Phase 2 has 70 exact policies at seeds 67-136, covering 11 market lanes, 44 Kamino operations, and 52 Jupiter edges.
- No recovery transaction was broadcast and no user asset moved.

## Post-PASS next milestone

The next independent Phase 2 goal is one deterministic Go runtime transition from PRIME/USDC to one safe representative alternative, including entry, HOLD, unwind, and return. Route selection and optimization remain later work.
