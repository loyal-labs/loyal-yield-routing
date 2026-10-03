# Worker v2 contract

Objective: implement the approved worker families on isolated branches, verified
against offline and disposable-database behavior, ready for later migration-aligned
review. Main merge, deployments, production inspection/activation and changes to
migration resources are excluded from this goal.

Authoritative verifier: `scripts/verify-workers-v2.sh`. During implementation its
package gates can pass while the goal remains incomplete. Overall acceptance also
requires the following actual scenarios in the integrated binaries/stores; missing
coverage or an unwired responsibility prevents overall PASS.

1. Autodeposit completes without app reads; floor/control changes rebaseline lots,
   maintain the one-hour coalesced deadline, freeze destination before pull, and
   recover both transaction legs while retaining idle custody, even after pause.
2. Observer capture/job/cursor commits are atomic; application dedupe/effects/done
   commits are atomic. Overlap, reorder, interruption and newer coalesced demand
   converge without lost financial events or acknowledgement of unapplied work.
3. Fleet deterministic planning matches the rescored reference under identical
   constraints; improved shared-reserve priorities update correctly. Admission
   rechecks fresh policy, custody, writable conflicts and aggregate capacity.
4. Retail routes persist exact attempts, recover uncertainty without duplicate
   spending, reconcile exact effects and preserve cross-mint custody checkpoints.
   Lookup tables retain explicit preparation/usage/recovery ownership.
5. Backyard preserves fixed manifest routing, safety/withdrawal/NAV ordering,
   actual-effect caps and its signed/intent/submitted recovery semantics.
6. Multiply preserves its own policy recipes, custody, receipts and recovery;
   ordinary swaps or Backyard do not substitute for it.
7. Hourly telemetry preserves actual observation clocks and coverage. Apps retain
   HTTP/mobile compatibility, user wallet signing and separate withdrawal cleanup;
   each removed repair writer has a demonstrated replacement owner.
8. Old/new contention, stale fences, immutable attempts, coalesced revisions and
   capacity release pass against disposable databases using real schema contracts.
9. All three binaries compile, exercised concurrency is race-safe, cancellation
   joins owned work, and replay uses the actual runtime decision functions.

## Units and evidence

Collateral raw, liquidity raw and USD micros are distinct values, tagged with mint
and verified decimals. Convert collateral using observed exchange evidence and
record its slot/provenance. Validate SQL BIGINT limits; use checked arithmetic and
local big.Int intermediates. Missing/unknown evidence never becomes zero capacity
or permission. Rates may be finite floats; amounts/custody remain integer.

Complete exposure snapshots replace the set; partial observations patch it.
Actual lifecycle/chain evidence orders financial events; unrelated table sequence
IDs do not. Capture progress is separate from applied progress. Cross-database
delivery uses stable event identity and destination-atomic dedupe/checkpoints.

## Writers and authority

Observer owns confirmed financial projections and verified market/policy facts.
Retail engine owns Autodeposit, fleet and Multiply autonomous operations; Backyard
has a separately credentialed engine instance. Apps own authenticated desired
controls/revisions and external receipt verification, not autonomous progress.
User deposits/withdrawals remain SDK/user-wallet signed; retail vault index 1 and
legacy agent vault index 0 stay distinct. Observer/planner have no private key or
broadcast client. SSE stays unchanged initially.

Desired controls, observed chain state and effective eligibility are separate.
An unavailable position may block admission but must preserve desired enablement.
The current target CHECK accepts pending/active/closed/inconsistent, not a fabricated
paused_missing_position chain status.

Custody ownership must interoperate with current Rust claims, operations and
reservations. No Go-only lock/table can replace that. Adopt or drain exact legacy
attempts before new admission; a database fence cannot invalidate signed wire.
Release reserve capacity only after the required observation is strictly newer
than its movement slot, not merely because an operation becomes terminal.

## Durable transitions

Family business state and transaction attempt state remain distinct. Persist
immutable exact bytes/hash/identity before send, then durable broadcast intent.
Signed-but-unsent recovery and ambiguous intent/submitted recovery follow each
existing family's protocol. Never create a fresh spend from a balance delta or
blindly resend an uncertain attempt. Confirm expected exact receipt effects.

Autodeposit pull and deposit are separate transactions. Freeze amount/destination
before pull; idle funds stay claimed until top-up/accounting finishes. New lots
wait one hour; batch due=max(existing due,new lot due). External spending consumes
newest lots; sweeps consume oldest eligible lots. Recovery precedes fresh admission.

Preserve canonical realized-APY allocation/hour/expiry boundaries. Personal modeled,
public simulated and realized return products retain distinct meanings. Missing
historical allocation/coverage remains missing. Corrections to collateral units,
continuous funded APY coverage, target vocabulary and heap ranking have separate
reviewed expectations from mechanical parity.

Root integrates shared contracts and signs off actual evidence. Schema changes are
additive and tested with old binaries. No production schema application or branch
merge into main occurs under this implementation authorization.
