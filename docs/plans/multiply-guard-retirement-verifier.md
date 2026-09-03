# Multiply Guard Retirement Close-out Contract

Status: active authoritative contract

This document is the sole completion contract for retiring the obsolete Multiply guard and registry layer. It supersedes `docs/plans/guard-registry-decommission-plan.md`, which predates the completed Phase 2 policy installation and must not be used as an execution plan.

## Objective

Retire the exact obsolete guard program shell and its four residual registry/staging accounts on mainnet-beta, reconcile every refunded lamport to the existing authority, remove the guard-specific deployment surface from the repository, and revalidate the already-live PRIME/USDC Phase 1 path without replaying identity-valid historical evidence.

The live NAV adaptor is required by Phase 1 and remains deployed and unchanged. Generic Squads `pre_hook` and `post_hook` wire fields also remain because they are part of the upstream ABI; only the retired guard-specific consumers and tooling leave scope.

## Standing authorization envelope

This goal authorizes only the following mainnet-beta state changes:

1. At program id `8moAa3vXstMPop9FtEnhTDRmcyo9HPn1CsywGMZ9K9n8`, migrate the remaining legacy loader-v3 shell to loader-v4, fund/resize it only as required by the loader, deploy the fixed close-only recovery artifact, retract it, and close the program shell.
2. Through that fixed recovery artifact, close only these exact guard-owned accounts:
   - `3GZpBrXGjCKELRwoK5VERYZeyKPJn7WiJAoUvkTFibU4`
   - `J1VEo6YTmMNfRRrZGjpkU8ZF8z2t3x5xHQySqYa2kMN2`
   - `4bSQzxkXKmezQTUyvNkMMgthsF4Wdc1J7eZr64QbxjAp`
   - `GdMMMAQCGihyN6tTbJiXjt8zZVmbmhNeS4yCRSXhJsnT`
3. Send every reclaimed lamport only to `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`, which must also match the injected `SOLANA_TESTING_PK` signer.

Expected gross recovery is exactly `136743120` lamports: `135601680` from the four data accounts plus `1141440` from the program shell. Network fees and temporary loader funding must be itemized separately and reconciled from finalized balances. The envelope expires when this goal reaches PASS or is closed.

No user asset, token, deposit, withdrawal, swap, borrow, repay, or other value-bearing lifecycle transaction is authorized by this envelope. The Phase 1 check therefore uses the known-route fast path: current live identities and readbacks, retained finalized lifecycle evidence whose invalidation keys remain unchanged, and the current close-out verifier. A future live replay requires a separately declared per-transaction and cumulative value cap.

## Hard constraints

- Network is mainnet-beta, proven by genesis hash before any send.
- The four targets must match their pinned owners, lengths, lamports, and SHA-256 data hashes before recovery.
- The already-closed programdata account and hook policies at seeds 15 and 16 must remain absent.
- No account outside the exact five-address closure set may be closed.
- Do not alter Squads settings, membership, threshold, timelock, policy seed, installed Phase 1/Phase 2 policies, vault state, NAV adaptor code/config/report ticket, worker deployment, or database state.
- Do not install, recreate, or renumber policies.
- Do not implement Phase 2 route selection, switching, or optimization in this goal.
- Preserve generic Squads hook fields and their `None` values where required by its ABI.
- Every broadcast stage requires simulation or loader preflight, a durable before-send barrier, one send, confirmed/finalized signature reconciliation, and an after-send barrier. Never blindly resend an ambiguous stage.
- Never persist a private key or signed wire payload in evidence, logs, or Git.

## Completion conditions

The sole verifier must report `PASS` only when all conditions hold:

| ID | Remaining condition |
| --- | --- |
| G01 | The guard program id, its programdata account, the four residual guard-owned accounts, and the two retired hook policies are absent at confirmed commitment. |
| G02 | Finalized evidence proves the exact four-account refund, program-shell refund, all stage signatures/slots/fees, the authority balance equation, and no closure outside the allowlist. |
| G03 | The repository contains no live guard/registry/recovery runtime or guard-specific deploy target; the NAV-adaptor deploy path and generic Squads hook ABI remain intact. |
| G04 | The current v12 RWA verifier passes against live state, proving the deployed NAV adaptor, exact hookless Phase 1/Phase 2 policy catalog, PRIME/USDC worker, and retained identity-valid Phase 1 lifecycle evidence. |
| G05 | Scoped checks pass and the branch contains only the intended retirement/evidence changes. |

The sole command, run from the repository root with the existing 1Password FIFO mounted, is:

```sh
op run --env-file=.env.1password -- bun run verify:multiply-guard-retirement
```

The verifier returns one structured `PASS`, `FAIL`, or `BLOCKED` result. Any failed condition includes a concrete resume condition.

## Current external gate

The authoritative baseline found the exact frozen prestate, but loader-v4 feature account `2aQJYqER2aKyb3cZw22v4SL2xMX7vwXBRWfvS4pTrtED` is absent on mainnet-beta. A signed-unsent migration simulation failed at the loader with `InstructionError(0, InvalidInstructionData)`. No transaction was broadcast.

The sanitized baseline is retained at `docs/evidence/multiply-guard-retirement/blocked-loader-v4-baseline-v1.json`.

Loader-v3 cannot safely recover this state: its close instruction requires the missing ProgramData account, while deploy requires an uninitialized Program account. The remaining shell is already initialized. Therefore G01 and its dependent G02 are externally blocked until loader-v4 activates; the exact account prestate must be revalidated before resuming.

## Proven evidence appendix

These facts are retained evidence and are not replay gates:

- PR #177 merged at commit `3375d954d76d4214456894494dadb52ec75c60ef` with the v12 RWA close-out verifier passing.
- Phase 1 completed one finalized and reconciled PRIME/USDC lifecycle with the live NAV adaptor.
- Phase 2 policy installation completed with 70 exact policies at seeds 67-136, covering 11 market lanes, 44 Kamino operations, and 52 Jupiter edges.
- The guard programdata account and hook policies at seeds 15 and 16 were already closed before this contract began.

Those proofs remain valid only while their current verifier invalidation keys remain unchanged.

## Post-PASS next milestone

After PASS and close-out, the next independent Phase 2 goal is one deterministic Go runtime transition from PRIME/USDC to one safe representative alternative, including entry, HOLD, unwind, and return. Route selection and optimization remain later work.
