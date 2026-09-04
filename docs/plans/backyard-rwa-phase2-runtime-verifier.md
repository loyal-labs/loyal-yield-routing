# Backyard RWA Phase 2 runtime activation: verifier-first contract

Status: approved implementation contract v1.

This path is the single definition of done for activating the first Backyard
RWA route beyond the fixed Phase 1 `PRIME/USDC` runtime. It does not reopen the
completed Phase 1 or Phase 2 policy-catalog close-out. Identity-valid evidence
from that work is retained in Appendix A and is not a stateful replay gate.

## Outcome and sole verifier

Preserve the proven fixed `PRIME/USDC` lifecycle and activate exactly one
additional safe RWA market-family representative in the deployed serialized Go
worker. The Go worker must originate and reconcile one complete bounded
lifecycle for that representative:

1. observe current route and withdrawal state;
2. prove the route is eligible and its exit is buildable;
3. enter through the installed Squads policy catalog;
4. persist an active position or a typed capacity/risk `HOLD`;
5. give any withdrawal demand priority over increasing risk;
6. unwind completely, swap back to USDC when required, and return exact USDC
   through Squads and the Loyal adaptor to Voltr idle;
7. report NAV through the existing atomic one-use ticket; and
8. independently reconcile terminal custody and durable operation history.

The sole verifier is:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-phase2-runtime
```

The command is read-only. It emits one structured `PASS`, `FAIL`, or `BLOCKED`
and separates source/byte, local composition, signed-unsent, deployment,
submission, confirmation, finalization, reconciliation, and live-behavior
evidence. Run it once for the baseline and once at completion; use affected
fast checks between those runs.

## Standing authorization envelope

- Cluster: `mainnet-beta`.
- Signer/payer: the existing Backyard operational signer already used by the
  proven lifecycle.
- Approved programs and destinations: the deployed Backyard Voltr vault,
  existing Loyal NAV adaptor, bound Squads smart account and currently installed
  policy catalog, existing Kamino and Jupiter programs on catalogued routes, the
  deployed Go worker, existing database, and existing Render services.
- Allowed without another approval: read-only inspection, local tests,
  signed-unsent simulations, forward-only policy rollover within the approved
  installed catalog design, deploys of worker/adaptor code committed in this
  repository, and lifecycle transactions up to `1 USDC` per transaction and
  `10 USDC` cumulative.
- One retrospective incident is authorized for transparent close-out reporting:
  finalized operation
  `fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe`
  requested `1,000,000` raw USDC but Voltr swept `3,793,417` raw USDC from its
  strategy custody to its idle custody. Exact finalized deltas conserved capital
  and did not change destination. This does not relabel the durable
  `manual_recovery` operation, count it as a successful lifecycle operation,
  authorize another exception, or increase either ordinary value cap.
- Expiry: when this goal closes.
- Stop and ask only for a new signer, cluster, program, destination, or route
  outside the installed catalog; an authority change; a cap increase; a
  destructive close or rent reclaim; or a verifier-contract weakening.

Log every consequential in-envelope action with non-secret identities, amount,
signature or deploy identity, commitment, and reconciled result.

## Required conditions

### R01 — current-state selection and frozen route

At confirmed commitment, revalidate the canonical-trace invalidation keys:
program deployments, SDK/source versions, instruction and account graphs,
signer/payer identities, current Settings and policy bytes, obligation state,
reserve state, lookup tables, and executable entry/exit quotes.

Choose exactly one non-`PRIME/USDC` representative from the installed 11-lane
catalog. Prefer a same-USDC-debt lane when it is currently safe and executable,
but do not select from stale APY, capacity, or saved simulation evidence. The
selected lane must use no new program, signer, destination, or authority and
must have a complete buildable exit. Freeze it in the canonical manifest before
money-moving implementation. Selection is an implementation decision, not a
runtime optimizer.

### R02 — exact two-route Go capability

The deployed Go runtime supports exactly the existing `PRIME/USDC` route and the
selected Phase 2 representative. It has typed route identities, account
decoding, current-state observation, installed-policy lookup, deterministic
entry/exit builders, capacity and health checks, withdrawal preemption, durable
idempotency, one nonterminal operation, lease fencing, NAV reporting, and
independent reconciliation for both.

Every catalogued but runtime-disabled lane fails closed. No caller-selected
route, arbitrary manifest entry, automatic switching, APY scoring, optimizer,
second writer, alternate journal, Rust/TypeScript money-moving runtime, registry,
or pre/post hook is reachable.

### R03 — selected-lane verification ladder

For the selected lane, retain one canonical trace and prove:

- official builder bytes, discriminators, accounts, signer roles, ordering,
  amounts, lookup tables, packet size, compute/heap needs, and deployed layouts;
- the valid graph is admitted by the exact installed policy intersection;
- mutations of program, signer, vault, market, reserve, obligation, mint,
  destination, amount, operation, instruction order, or extra instruction are
  rejected by the named enforcement owner;
- local Squads composition where it represents the deployed behavior; and
- fresh current-chain signed-unsent sequences for entry, typed `HOLD`, complete
  unwind, swap-back/return, and NAV report with final identities.

After one representative in a market family has live proof, sibling lanes in
that equivalence class terminate at batched signed-unsent positive and negative
simulation. No per-lane live matrix is required.

### R04 — immutable deploy and single writer

Build, publish, and deploy one immutable Go worker image from the committed
source. Read back the exact image digest and deployment identity. The new worker
must acquire the existing fenced route lease only after the prior writer can no
longer write. At most one writer and one live lease exist, and no operation is
stuck beyond the bounded reconciliation window.

### R05 — one Go-originated bounded live lifecycle

Using no more than the standing value caps, let the deployed Go worker originate
one live lifecycle for the selected representative. Persist signed wire or
ambiguous-broadcast state before optional evidence output; send each frozen wire
at most once; never blind-resend. Use confirmed commitment for progress.

The retained evidence must bind each operation to the deployed image, route
lease, observation, decision, exact installed policies, signature, confirmed
poststate, and subsequent operation. A typed current-capacity `HOLD` is valid
only when it is durably recorded and sends no risk-increasing transaction. The
route reaches full proof only after complete exit, USDC restoration, NAV report,
and finalized terminal custody reconciliation with no unintended residue.

The authorized incident must be retained separately from the successful
operation set. Its artifact must match the exact operation, signature, finalized
slot, requested and actual amounts, conserved account deltas, durable
`manual_recovery` status, explicit operator authorization, and deployed forward
fixes. R05 still requires its successful operation set to be reconciled and
within the ordinary caps; the incident cannot satisfy a missing successful
restore or any other lifecycle action.

### R06 — Phase 1 and withdrawal safety remain intact

Current readback and normal regression coverage prove that fixed
`PRIME/USDC`, bridge, NAV-ticket, withdrawal-preemption, manual-recovery, and
single-writer behavior remain valid. Do not replay the historical Phase 1
lifecycle. A user withdrawal must remain serviceable from either enabled route,
and restoration/recovery must outrank new allocation.

### R07 — truthful handoff and standing coverage

The manifest, partner handoff, admin read model, and operational audit identify
the selected runtime lane, deployed image, exact two-route capability, disabled
catalog lanes, recovery path, and explicit absence of route switching and an
optimizer.

Promote stable route-schema, byte, packet, policy-intersection, fail-closed,
withdrawal-priority, and single-writer checks into normal tests or CI. Live
RPC/database/Render/custody reconciliation remains outside unit tests. At close,
record promoted checks, retired goal-only checks, and retained evidence pointers.

## Verdict and stopping rules

- `PASS`: R01–R07 all hold against current authoritative state and Appendix A
  evidence remains identity-valid.
- `FAIL`: name the first false required condition and its authoritative evidence.
- `BLOCKED`: name the unavailable external dependency and exact resume condition.
- Current chain, database, and deployment state outrank manifests, saved JSON,
  SDK decoders, and summaries.
- Confirmed commitment is the progress gate. Finalized commitment is reserved
  for current Settings/seed when required and terminal custody reconciliation.
- Never replay an expired historical wire, reinstall a matching policy, or cycle
  policies for testing. Any mutation uses forward rollover.
- Stop only on current `PASS` or a genuine external gate after all safe in-scope
  work is exhausted.

## Hard exclusions

- No consumer webapp, wallet-connect flow, or Earn Max UI.
- No optimizer, APY scoring, arbitrary route selection, or automatic multi-route
  switching.
- No registry, pre-hook, post-hook, new adaptor architecture, or new onchain
  program.
- No account refund/closure campaign or rent reclaim.
- No per-lane live matrix, historical stateful replay gate, second writer,
  second journal, saga, or Rust/TypeScript money-moving runtime.
- No unrelated infrastructure or product work.

## Appendix A — retained proof, not replay gates

- Real confirmed Phase 1 deposit/allocation/`PRIME/USDC`/utilization-`HOLD`/
  unwind/restoration/claim/NAV lifecycle and conservation evidence.
- Deployed Loyal adaptor v2 byte proof and one-use NAV-ticket mutation evidence.
- Finalized Phase 2 catalog: 70 physical policies at seeds 67–136, representing
  11 lanes, 44 Kamino operations, and 52 directed Jupiter edges.
- Phase 2 resolver/compiler, packet measurement, grouped signed-unsent positives,
  and dangerous negative mutations.
- Existing immutable Go worker deployment, database migrations, lease and
  recovery model, fixed `PRIME/USDC` tests, and read-only admin macroview.
- Guard, registry, and hook retirement proof, with the NAV adaptor intentionally
  retained.

An Appendix A item returns to the active set only when a canonical-trace
invalidation key changed or cannot be matched to current authoritative state.
