# Codex handoff — Backyard RWA Phase 3 all-market support

Implement v2 at docs/plans/backyard-rwa-phase3-family-activation-verifier.md.
That file alone defines acceptance; this handoff supplies implementation order.
The operator requested the revision to remove late cap/proof blockers and ship
all catalogued markets through one shared implementation.

## Runtime goal text

> Ship deployed support for the exact 11 catalogued Backyard RWA lanes on
> mainnet-beta under docs/plans/backyard-rwa-phase3-family-activation-verifier.md
> v2. Extend the existing serialized Go worker, preserve its journal/lease and
> installed policy authority, and enforce 1/20/60 USDC gross caps with reserved
> exits before broadcast. Verify all lanes in batches and run three new-family
> canaries ending LIVE_VALIDATED or proven CAPACITY_PENDING. Preserve Prime/Maple
> proof. Complete only when verify:rwa-phase3-family-activation proves R01-R08,
> deployment identity, truthful readiness and flat finalized canary custody.
> Follow the contract's standing authorization, expiry and exclusions; report
> precise external gates after independent in-scope work is exhausted.
> Documentation completion does not complete this implementation goal.

Use one runtime goal and worker queue. Verify actual Phase 2 goal closure and
unresolved work before live activation; PR #223 alone does not prove goal/lease
status. Replace any superseded runtime objective explicitly, rather than trying
to satisfy v1. Do not create another competing goal on the serialized worker.

## Authorization

The contract's standing envelope covers local implementation, tests and
signed-unsent simulation, three bounded canaries, all-11-lane runtime support,
forward-rollover repair, committed immutable deploys and admin squash-merges of
docs-only evidence PRs. Code changes follow normal validation/merge rules.
Proceed without another approval inside that envelope; stop at its explicit
identity, authority, cap, destructive-action or scope boundaries.

Caps remain 1 USDC per transaction including fees, 20 per family across all
attempts/cleanup, and 60 for the goal. Three times 20 leaves no extra margin.
Sibling support does not authorize additional funded canaries or indefinite
capital allocation after the finite goal expires.

## Implementation details and order

1. **Baseline and feasibility.** Start from current main, preserving unrelated
   changes. Capture measured baseline subclaims and implement the remaining
   verifier measurements alongside their runtime slices. Resolve identities, custody/exposure,
   credentials/accounts, simulation and deployment access. Prove a bounded
   controlled-state simulation sample before depending on it. Measure each
   full lifecycle under the new accounting; record finite stage deadlines and
   concrete external gates. Required setup/protocol closes must surface here.
2. **Governor and shared runtime.** Extend the existing package/journal. Persist
   spent amounts, unresolved reservations and conservative exit reserves with
   the lease; enforce admission before signing and authorization before sending.
   Carry explicit mint/program/extensions/decimals, debt custody, USDC valuation,
   repayment and conversion for USDC/PYUSD/USDG/USDS. Reuse RuntimeRoute and
   remove Maple-only/two-route assumptions where necessary; no worker clones
   or general plugin framework.
3. **Decisive proof.** Exercise cap/exit-reserve rejection without signing/sending,
   restart and ambiguous-submission reservations, and successful reserved-budget
   unwind. Batch proof for all 11 bindings and paths. Reuse unchanged structural
   evidence while explicitly covering token programs, decimals, farms and swap
   differences. Repair only observed policy defects through forward rollover.
4. **Deployment and queue.** Deploy verified immutable source and confirm R01
   before any live Phase 3 broadcast. Keep one worker lease while processing
   OnRe/ONyc/USDC, AUTO/AUTO/PYUSD, Ethena/USDe/PYUSD, subject to evidenced
   same-family substitution. Advance only when flat/reconciled or after an
   unfunded capacity HOLD with no exposure/unresolved submission. A partial
   entry must unwind. This fixed activation sequence is explicitly allowed;
   moving open positions between markets and yield selection remain excluded.
5. **Truthful readiness.** Funded canaries complete their full lifecycle.
   CAPACITY_PENDING requires a real deployed unfunded capacity HOLD, fresh
   evidence, no exposure/submission, and complete controlled-state execution
   through production builders and actual enforcement. It is not current-chain
   or future-capacity proof. Missing code, policies, accounts, funding, quotes
   or tooling cannot use this outcome. All 11 lanes need support; six siblings
   require batched proof rather than another live funding matrix.
6. **Close-out.** Reconcile deployed identity, exact lane matrix, policy bindings,
   budgets/reservations, lease, operations and finalized custody. Update existing
   admin/partner handoff, promote stable checks, run every R01-R08 condition,
   retain results and close only on current PASS.

The Phase 2 recorded 13.651629 excludes restoration/fees and does not normalize
all assets to USDC. Do not use it to certify these caps. Measure the real action
graph, reserve exits before entry, and shrink canary size if needed without
removing required lifecycle actions. Never bypass a cap to recover.

## Rules for keeping the goal moving

- Diagnose the first false condition with a discriminating check; preserve its
  evidence and fix the implementation. Do not repeat unchanged expensive suites,
  redeploy without a fix, or introduce acceptance gates at close-out.
- Retry capacity only after meaningful economic-state changes; a new slot is
  insufficient. No blind resends or unbounded command/retry loops.
- Finish independent local work and eligible families around family-specific
  gates, but never bypass shared lease/accounting/ambiguous-submission failures.
- Report LIVE_VALIDATED, SUPPORTED_READY and CAPACITY_PENDING separately.
  Controlled-state success is not live finalization evidence.
- Consumer webapp, Earn Max UI, optimizer, arbitrary routing, new authority/
  programs, destructive closes, second writers/journals and unrelated work
  remain excluded under the contract.

Run the sole verifier through the mounted secret environment without reading
or copying it:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-phase3-family-activation
```

Do not claim the command exists or passes until implemented and executed.

## Implementation checkpoint — 2026-09-04

The implementation goal is active in Codex. Baseline and fresh token/capacity
reads are retained under `docs/evidence/backyard-rwa-go/phase3/`. The sole
verifier exists but remains a fail-closed measurement scaffold: R01-R08 are
NOT_IMPLEMENTED, not satisfied by the presence of new source or tests.

Implemented locally: fixed 1/20/60 budget accounting with exit reservations;
lease-locked PostgreSQL reservation and wire binding; guards before production
bridge/Kamino/Jupiter signer loading and at broadcast intent; decimal-aware
reserve valuation and target borrowing. The race-enabled Go suite passed with
the disposable PostgreSQL test enabled, covering concurrent duplicate admission,
restart retention, unbound-wire rejection and stale-writer fencing. This is
not yet the complete R01 witness, migration validation or live proof.

Do not deploy this intermediate implementation: reservation producers, fresh
cost/fee/exit estimation and typed durable HOLD handling still need integration.
Finalized settlement and expired-unsent release are now integrated with the
existing journal: settlement rechecks the exact receipt/effects under the lease;
expiry requires finalized block-height expiry and a subsequent signature-absence
read. Release restores the captured pre-transaction exit reserve. The database
test drives these paths and rejects confirmed-only or unrelated receipts. These
controlled tests do not establish live program or deployment proof.
The production compilers now expose their exact unsigned messages for fee
measurement before signer loading; the existing signing paths reuse those same
compilers. Fee reads bind `getFeeForMessage` to message hash and minimum slot,
rejecting null/zero/stale results. The executable-debit gate shares the actual
builders and custody graph, rejects partial-sweep restoration contracts, and
charges underlying withdrawal liquidity rather than receipt units. Tests cover
message/signature equivalence and fee-inclusive transaction-cap rejection.
These primitives still need fresh valuation and complete exit-graph estimates
wired into durable admission; their unit success does not satisfy R01.
USDC-normalized valuation now checks mint/program/decimals, message-bound fees,
overflow and a maximum 32-slot validity interval. The route-token observer reads
both reserves/mints and chain Clock coherently, rejects prices older than 60
seconds, and can obtain refreshed prices through an unsigned permissionless
reserve-refresh simulation using the existing production prefix. Captured
simulation state is explicitly labeled, not represented as a landed refresh.
Non-USDC valuations apply a 1% upward token-price and downward USDC-price margin;
USDC against itself remains exact. These are conservative estimates, not a
guarantee against arbitrary subsequent price changes.
The read-only price preflight passed for the two existing runtime lanes at
slots 444380781-444380783 and observed a 5,000-lamport unsigned NAV-message fee.
This is not all-eleven-lane, native-SOL valuation, full lifecycle or R01 proof.
Native-fee price sourcing subsequently passed at slot 444382714 using the
unique wrapped-SOL reserve in Kamino Main as a read-only price reference, not
an executable lane. Its unsigned refresh uses ABI order Pyth, Switchboard
price/twap, Scope (different from reserve storage order). The exact unsigned
NAV message cost 5,000 lamports, conservatively valued at 520 micro-USDC.
No setup-cost coverage is implied by that fee-only observation. Complete
exit-cost estimates and admission wiring remain open; this is not R01 PASS.
The production worker now journals typed build-time budget HOLDs before
restart recovery can overwrite the cause. Its lease-fenced transition releases
only never-submitted reservations, restores the prior exit reserve and keeps
spent counters unchanged. The real disposable PostgreSQL slice verifies this
and rejects the same release for signed operations. This preserves diagnostics;
it does not yet implement family queue scheduling or complete admission.
The new gates intentionally
refuse transactions without an initialized durable goal budget and admission.
No production budget was initialized; no Phase 3 transaction was signed or sent.

Next: finish the bounded simulation/setup/cost preflight; complete R01 admission
and recovery end-to-end; extend the shared runtime and exact bindings to all 11
lanes; implement the flat sequential queue; replace every verifier scaffold
with authoritative behavioral measurements before deployment/canary closeout.
Fresh state confirms ONyc/USDe use 9 decimals. PYUSD/USDG include Token-2022
extensions, currently disabled transfer hooks and zero transfer-fee schedules;
decode and recheck these settings in production rather than assuming plain SPL
accounts. Non-USDC debt custody/NAV/conversion is still unfinished.
The shared custody decoder now accepts the Token-2022 account ABI for
ImmutableOwner, zero TransferFeeAmount and inactive TransferHookAccount, while
rejecting unknown/duplicate/truncated extensions, wrong account types, withheld
fees and active hook state. This is an account-side compatibility change only:
fresh mint-level fee schedules/hooks, exact lane binding and non-USDC NAV are
still required before those lanes can execute or advertise readiness.
Budget-price observation now validates current mint transfer semantics: both
older and scheduled Token-2022 transfer-fee rates must be zero, the hook program
must be disabled, and unsupported/malformed/duplicate extensions fail closed.
Confidential mint configuration does not authorize confidential custody or
operations. This validation is wired into the price observer, not yet complete
production admission; lane-pinned authority identities and send-time freshness
still need end-to-end proof. Local race tests passed, not live all-lane proof.
Kamino's four mutation account vectors now share typed route inputs for market,
custody, underlying token programs and optional farms. The SDK receipt-token
program stays classic SPL independently of liquidity token programs. Existing
Prime byte fingerprints remain unchanged; unknown lanes and caller token-program
mutations reject. This is the shared layout groundwork, not installation of the
other nine routes: fresh exact bindings, policy bytes, oracle refresh graph and
full program execution proof remain required before they can execute.
The read-only resolver now exposes reserve decimals, farms, oracle configuration
and individual observation slots. All 11 reserve/custody identity graphs resolved
at confirmed slot 444385567 (Settings policy seed 139). This does not prove farm
user initialization, current policy admission, capacity, or executable readiness.
The attempted addition of nine runtime bindings was rejected by safety review
as an unapproved destination expansion and was not applied. Resolve that exact
authorization boundary before retrying. Phase 3 verifier TypeScript errors were
repaired without changing its FAIL semantics; the offline diagnostic still
reports eight incomplete conditions. Package typecheck remains red on six
pre-existing possibly-undefined errors in generate-rwa-phase2-r03-plan.ts at
that checkpoint. The subsequent repair replaces its unchecked array assertion
with exact RPC cardinality and explicit required-account checks. Package
`bun run check` and all four `test:phase2-runtime` cases now pass. No runtime
destination bindings were added while the safety-review approval is outstanding.

## Resumption checkpoint — 2026-09-04, after the skill/workflow review

The previous paragraphs are chronological evidence, not current completeness
claims. Codex reported no goal on resumption; a fresh Phase 3 goal is now active
with the same v2 scope and 1/20/60 envelope. Work remains in the existing
`/private/tmp/loyal-backyard-phase3.3EQhbn` worktree. Do not start another worker.

The verifier now computes per-subclaim verdicts, reports missing behavioral
proof explicitly and invokes the actual worker's read-only `--inspect-phase3`
route resolver. It no longer assigns every row a blanket NOT_IMPLEMENTED.
Compiled route resolution is still only Prime/Maple; it is not full capability
or deployment proof. `--output <new-path>` retains an immutable full snapshot.
Source fingerprints distinguish the working tree from deployed source and
reject source changes during a verification run.

Fresh read-only snapshot `docs/evidence/backyard-rwa-go/phase3/resumption-2026-09-04.json`
(binding slot 444397851, before the subsequent NAV fix) establishes:

- The old deployed image still holds the sole lease; no nonterminal operation,
  Phase 3 operation or initialized Phase 3 budget was observed.
- All 44 Kamino operation account vectors were compared to retained installed
  bytes plus known Maple rollovers using fresh finalized account identities.
  Ten vectors differ only at debt-farm positions: borrow/repay on all three
  OnRe lanes and Maple/USDG and Maple/PYUSD. This is measured repair work, not
  authority to invent new semantics or claim that the current policies execute.
- Farm users for OnRe/USDC, OnRe/USDG, Maple/USDC and Maple/PYUSD exist with
  the expected vault owner, obligation delegatee and farm. OnRe/USDS and
  Maple/USDG farm users are absent. Resolve setup and its cost before live use.
- AUTO and Ethena vectors match their current installed policy bytes and their
  obligations/custodies are present. This narrows the earlier rejected expansion;
  it does not bypass that review or establish Jupiter/full-lifecycle readiness.
- Prime/USDC and Maple/USDC obligations are currently absent after prior flat
  closure. Do not treat absence alone as failed retained lifecycle proof; prove
  any required reinitialization path separately before a new entry.

Implemented next, locally: route NAV now converts collateral, debt liability
and non-USDC idle debt custody into USDC using a same-batch USDC reference;
it no longer mixes debt raw units with USDC or omits idle debt. Tests exercise
depeg, unequal decimals, conservative rounding, changed poststate and missing/
mismatched input rejection. Existing USDC-route fingerprint shape is preserved.
This does not generalize the on-chain NAV adaptor or advertise new route support.

Validation: Go full race suite and 12 targeted Bun tests pass; package typecheck
passes. PostgreSQL integration is environment-gated and was not enabled in this
resumption's full Go suite. No policy installs, runtime-binding activation,
deployment, mainnet budget initialization, signing or sending occurred.

Next discriminating work: demonstrate sequential execution with real programs,
then
finish complete cost/exit admission through production. Continue exact binding
review and narrow farm-policy repairs without waiting on unrelated work. The
platform's rejected expansion must be resolved by its permitted review with
this exact evidence; new authority still requires the operator. Goal remains
active and R01-R08 remain incomplete, not capacity-pending or completed.

Local mechanism probe subsequently passed on `solana-test-validator` 4.2.0:
`/private/tmp/backyard-phase3-svm.VAO9c4/sequential-probe.ts` executed two dependent
native-program transfers at local slots 245/246, reconciling the intermediate
recipient balance from 1,000,000 to 985,000 lamports after a 10,000-lamport return
and 5,000-lamport fee. Genesis was
`CZc1XFVecEauFk2AcHZCRxaLrKaALq5uV4M2uKzvqGga`, explicitly not mainnet. Only
ephemeral in-process keys and fake local funds were used. The sandbox required
permitted local-port execution; its review approved the bounded probe. The
validator was stopped and port 18899 verified closed. This proves local state
persistence, not Squads/Kamino/Jupiter/Voltr execution, production signer proof,
controlled capacity or R04 acceptance. The next sample must load exact real
program/account state and execute the required lifecycle without bypassing
policy or authority checks. Do not repeat the native-program probe.

### Production budget identity and setup feasibility checkpoint

The durable reservation now binds its family to the operation's journal lane
at admission and again at later authorization/reconciliation reads. Missing
lanes, another family's lane and post-admission lane changes reject with
`reservation_family_does_not_match_journal_lane`. The real disposable-PostgreSQL
integration test and full Go race suite passed with that test enabled. This
does not wire the missing production admission producer or shared setup bucket.
Shared policy setup must not be disguised as an extra funded historical canary.

The sole verifier now includes the read-only compiled worker's setup-rent
inspection and retains it in
`docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json`.
All R01-R08 remain incomplete. At slot 444404813, borrow-policy rent alone was
1.006738 USDC under the governor's conservative native-SOL valuation, above
the existing $1 transaction cap; repay-policy rent was 0.907909 USDC.

A discriminating local test showed the repository Squads fixture is not the
deployed binary (fixture SHA256
`83234e417a0819006abdd0b4267e1f2fac697ef6584410f5bacb0c3f03c1dada`):
it allocated 860 bytes for the borrow policy shape, not the installed 1,400.
Do not use it to declare the deployed repair affordable. A read-only mainnet
program snapshot at finalized slot 444405300 fetched program data
`2g3u9qgz4adKQVN1TUoh7bbBKqaSsjXtz1yX2ptagW5T`, deployment slot 443245754,
data SHA256 `9dae95130a6f0a36b8ea4a4b16d3d10a526ee830fe30f90f13c47264eafa800b`.
The padded ELF SHA256 is
`1c95bd7be140589d2aec38a85d7ecfe70ec69639277f622c898f821ab1d636fa`.
The explicit deployed-program test passed: borrow/repay allocations are
1,400/1,250 bytes and the payer pays rent plus a separate fee. Test subjects
used ephemeral keys and local fake funds only. No mainnet signature or send.

Reproduction (bounded run; the initial clean dependency build took 3m43s,
subsequent execution takes seconds):

```sh
SQUADS_SMART_ACCOUNT_PROGRAM_SO=/private/tmp/backyard-phase3-squads-rent.2VG8nM/deployed-squads.so \
  cargo test -p squads-test-harness --test rwa_policy_creation_rent -- --ignored --nocapture
```

The test pins that deployed ELF hash and is explicitly ignored in the default
suite; it must not silently fall back to the different repository fixture.
The public-program fetch script and identity JSON are in the same temporary
directory. Local rent is not current mainnet rent: combine allocation proof
with the verifier's fresh RPC rent/valuation, never the local rent amount.

The observed borrow-repair cost conflict needs either newly measured in-cap
feasibility or a specific operator-approved envelope revision before that
setup broadcast. Do not keep polling prices, weaken policies or silently raise
the cap. Continue unaffected work. The next critical implementation remains
complete production cost/exit admission (including separately accounted shared
setup), deployed-program stateful full-cycle proof, and exact runtime bindings
through the permitted review path. Nothing here resolves the earlier rejected
route expansion. No deployment, policy installation or live canary occurred.

### Fresh cost checks on production build paths

The subsequent local slice connects the real bridge, Kamino and Jupiter
builders to exact unsigned-message fee and fresh principal/native-SOL valuation
before signer access. Principal plus fees above the unchanged transaction cap
produce a typed HOLD with measured amounts, slot and message/valuation hashes.
The durable admission check also rejects a fresh known cost above its existing
reservation. Missing/stale valuation does not inherit old authorization.

These are rejection/revalidation checks, not a completed admission producer.
They deliberately do not certify zero setup cost, derive an exit graph, create
reservations, initialize a budget or revalue at the final send boundary. Those
remain required work. All production builders use the priced gate; the old
unpriced authorization wrapper exists only in storage tests, not production.

The verifier runs three named, bounded Go behavioral tests with controlled RPC
inputs through these actual production paths; it requires each test and package
to pass, rejecting skipped, absent, duplicated or failing results. It records
their local proof level explicitly. The offline snapshot
`docs/evidence/backyard-rwa-go/phase3/production-build-cost-2026-09-04.json`
records this R01 subclaim passing while every full R-condition remains
incomplete. External state was not refreshed in that offline snapshot; retain
the prior live snapshot with its own times and dependencies. The snapshot
precedes the addition of non-secret measured details to the same HOLD errors.

Validation: full Go race suite passed; the separate real disposable-PostgreSQL
admission/send-fence test passed, including fresh-cost-over-reservation rejection.
Nine targeted TypeScript tests and package typecheck passed. No signer access,
production database changes, route activation, deployment or live send occurred.
The policy-creation cap revision request remains unanswered and is not assumed
approved. Continue complete production setup/exit admission and remaining
runtime/protocol proof without repeating the unchanged rent probe.
