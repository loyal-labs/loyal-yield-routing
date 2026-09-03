# Backyard Voltr + Squads RWA worker: close-out contract

Status: approved close-out contract v12.

This path is the single definition of done for closing Backyard Phase 1 and the
Phase 2 policy-capability installation. It supersedes the historical v11 launch
contract retained below the `Historical v11 contract` marker. Conditions already
proven by immutable lifecycle, deployment, or installation evidence are Appendix
A inputs, not stateful replay gates. Only the conditions in this v12 section are
active.

## V12 outcome and sole verifier

Close the implementation with one read-only verifier that proves:

1. deployed identities still match the proven Backyard system;
2. current Squads policies are the set-exact expansion of the approved 11 lanes,
   44 Kamino operations, and 52 swap edges;
3. no historical signed transaction is replayed as a current simulation;
4. the deployed Go worker remains the sole fixed PRIME/USDC writer with no stuck
   operation; and
5. contract, manifest, evidence index, and partner handoff accurately describe
   the deployed system and its exclusions.

The sole verifier remains:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-multiply-custom-lifecycle
```

It is read-only and emits one structured `PASS`, `FAIL`, or `BLOCKED`. Run the
full verifier once at baseline and once at completion; use affected fast checks
between them.

## V12 standing authorization envelope

Cluster: mainnet-beta. Signer/payer: the existing Backyard operational signer already used in this thread.

Approved surface: the deployed Voltr vault + Loyal adaptor, the bound Squads smart account and its installed policy catalog (all currently installed seeds), the deployed Go worker, Kamino and Jupiter programs on the already-proven routes, and the existing Render services.

Allowed action classes without asking: all read-only and simulation-only operations; signed-unsent simulation batches; policy installs/retires that stay within the approved catalog design by forward rollover; lifecycle transactions (deposit, swap, open/close/unwind, NAV report, withdrawal return) up to 10 USDC per transaction and 100 USDC cumulative; program upgrades of the adaptor and worker to code committed in this repo; deploys via the batched confirmed-commitment uploader.

Expiry: when this goal closes.

Stop and ask ONLY for: a new signer, cluster, program, or destination outside this surface; any authority or upgrade-authority change; raising these caps; a destructive close or rent reclaim of a live account; or any change that weakens the verifier contract. Everything else: act, log the transaction summary, continue.

The envelope authorizes implementation; it does not require a transaction.
Prefer current readback and retained valid evidence.

## V12 remaining required conditions

### C01 — identity and single-writer reconciliation

At confirmed commitment, independently read the Voltr vault, strategy/adaptor
binding, adaptor config and report ticket, Squads Settings, and vault PDA. Match
the canonical manifest and programs. Verify the immutable Go worker image is
live, owns the active lease, has no competing writer, and has no operation stuck
beyond the bounded reconciliation window.

### C02 — set-exact current policy catalog

Read finalized Squads Settings once to freeze the seed boundary, then read and
decode every installed catalog policy. Expand physical constraints into semantic
entries and deterministically emit expected and actual policy counts/accounts,
11 lanes, 44 Kamino operations, 52 swap edges, missing/unexpected/duplicate
entries, unexpected authority reachable by the runtime delegate, and temporary
or diagnostic accounts. Actual authority must be bijective with the approved
catalog. The installed generation is 70 policies at forward seeds 67–136 unless
current authoritative readback proves a later approved rollover. Do not reinstall
a matching policy merely to prove it.

### C03 — truthful evidence semantics

Historical signed-unsent artifacts are archival evidence. Verify pinned hashes,
Ed25519 signatures, `broadcast:false`, recorded creation-time results, signature
absence, and current deployed identities. Never submit an expired historical wire
to `simulateTransaction` or call `BlockhashNotFound` execution proof.

If an identity changed, build one fresh current-state wire for the invalidated
equivalence class. At most one representative lane per market family may run
live; after one member has existing live proof, the remaining class terminates at
batched signed-unsent simulation with signature verification. No per-lane live
matrix is a close-out gate.

### C04 — deployed contract and handoff agree

The closed action vocabulary includes deployed bridge, NAV, PRIME/USDC
entry/exit, and swap-step actions, including `SWAP_USDC_TO_PRIME_STEP` and
`SWAP_PRIME_TO_USDC_STEP`. The handoff must state that manifest `ready` means
the fixed Phase 1 executor is enabled; unresolved/empty Phase 2 manifest fields
must not be presented as evidence of Phase 2 authority. The evidence index must
not cite obsolete commands or seeds.

One checked-in handoff states the vault, adaptor, strategy, Squads, worker, and
deployment identities; deposit/allocation/HOLD/withdrawal/600-second claim/NAV
behavior; fixed PRIME/USDC runtime scope; Phase 2 policy-only capability;
monitoring and recovery; and explicit exclusions for optimization, route
switching, consumer Earn Max, and onchain NAV computation.

### C05 — standing regression coverage and retirement

Promote stable source/byte, action-schema, manifest consistency, catalog-set,
packet-limit, forbidden-runtime, and fail-closed checks into normal CI or repo
tests. Live RPC/database/Render/lifecycle reconciliation stays out of unit tests.
At close, record promoted checks, retired historical replay checks, and retained
evidence pointers. The close-out wrapper may remain only as an explicit
operational audit, never a hidden permanent CI requirement.

## V12 rules and verdict

- Current authoritative state wins over manifests, saved JSON, SDK decoders, and
  summaries.
- Confirmed is the progress gate; finalized is reserved for current Settings/seed
  and terminal custody reconciliation.
- Reuse an unchanged canonical trace when program/deployment identity, SDK/source,
  instruction graph, identities, policy bytes, Settings/seed, and relevant state
  still match.
- Any required policy mutation uses forward rollover; never recreate in place or
  cycle a policy merely for testing.
- No historical stateful replay, per-lane live matrix, optimizer, new route
  executor, consumer Earn Max, UI expansion, adaptor redesign, or unrelated infra.
- Log each consequential in-envelope action with non-secret identity, amount,
  signature/deploy identity, commitment, and reconciled result.
- `PASS`: C01–C05 all hold, Appendix A evidence remains identity-valid, and no
  forbidden work was added.
- `FAIL`: name the first false condition and authoritative evidence.
- `BLOCKED`: name the unavailable dependency and exact resume condition.

## Appendix A — already proven; pointers, not replay gates

- Real confirmed Phase 1 lifecycle, conservation, utilization HOLD, and NAV:
  `docs/evidence/backyard-rwa-go/lifecycle-v1.json`.
- Adaptor v2 byte proof and creation-time mutation simulations:
  `docs/evidence/backyard-rwa-go/adaptor-v2-ticket-simulation-v5.json`.
- Phase 2 resolver/compiler, packet measurements, four grouped positives, seven
  negatives, and finalized installation: `docs/evidence/backyard-rwa-go/phase2/`.
- Installed generation: 70 policies, seeds 67–136, semantically 44 Kamino
  operations and 52 directed swap edges.
- Go worker image `sha-bdae0957e394727dcdaf449775659bd8e92d3727`, digest
  `sha256:b93a5e260fa31116d71e487d5f06e72989614a8accd03b018da6a57f34293a99`.
- Retained read-only Backyard admin macroview V07 evidence.

An Appendix A condition returns to the active set only if an invalidation key in
the v12 rules changes or cannot be matched to current state.

---

## Historical v11 contract — non-normative record

Status: approved Phase 1 implementation contract v11.

This file is the sole definition of done for the first Backyard Finance RWA
vault. It replaces the earlier Rust fleet/four-market orchestration plan.

The current implementation must report FAIL against the Phase 1 contract until
the custom adaptor authenticates a Squads-PDA-signed NAV report, the exact
bridge and PRIME/USDC policies are live, one Go worker owns the route, and one
real end-to-end Backyard lifecycle is independently reconciled. The complete
eleven-lane policy catalog is a separately reported Phase 2 expansion. Its
absence must not be hidden, but it does not block the first fixed-route release.

## 1. Outcome

At confirmed commitment on Solana mainnet-beta, one Backyard Voltr USDC vault
must:

1. receive a user deposit into Voltr idle;
2. allocate it through the vault's bound Loyal adaptor into one exact Squads
   smart-account USDC account;
3. let one serialized Go worker attempt the fixed PRIME/USDC Kamino Multiply
   entry, reach the collateral-only state, and persist a utilization HOLD
   before borrowing when the confirmed reserve ceiling blocks that leg;
4. observe a withdrawal request immediately, stop increasing risk, unwind the
   required amount, return it through Squads and the adaptor to Voltr idle
   before the 600-second waiting period ends;
5. let the user claim after the waiting period;
6. submit NAV reports through the adaptor only through one atomic Squads payload
   whose first instruction arms the exact one-use report ticket and whose second
   instruction invokes Voltr to consume it; and
7. leave one durable, independently reconcilable decision and transaction trail
   in the existing database.

Phase 2 gives the smart account a compact, exact policy catalog capable of all
eleven requested RWA Multiply lanes and the required swaps. The Phase 1 worker
uses only the bridge/NAV policies and fixed PRIME/USDC lifecycle policies.

Phase 1 PASS means this fixed PRIME/USDC outcome has happened once with real
internal Backyard capital, one deployed Go writer owns it, and all current
deployed identities still agree. Source, simulation, or deployment alone is
never PASS. Phase 2 has its own PASS/FAIL/BLOCKED result for the 11-lane,
44-operation, 52-edge catalog.

## 2. Non-goals and accepted tradeoffs

- No consumer Earn Max behavior in this milestone.
- No APY optimizer or route switching. The only entry route is PRIME/USDC.
- No Rust or TypeScript money-moving worker or runtime sidecar.
- No multi-leg precomputed saga, cancellation graph, event bus, or new queue.
- No SVM or LiteSVM readiness gate.
- No separate low-value canary and no live test of every lane. The live test is
  one real internal Backyard lifecycle; policy simulations are batched.
- Confirmed commitment is sufficient. Finalized is not a readiness gate.
- Reuse the existing Voltr-approved vault and preserve allowAnyAdaptor enabled.
  Voltr InitializeStrategy still requires the exact initialized adaptor receipt,
  so Phase 1 creates that minimal receipt without a governance mutation.
- Go computes and reports NAV for the MVP. Fully onchain economic NAV is a
  later hardening project, not hidden inside this milestone.
- No complicated admin console. The first page is a read-only operating view.

These tradeoffs reduce delivery time. They do not relax signer, account,
reserve, signed-wire uniqueness, conservation, or reconciliation checks.

## 3. One verifier and one verdict

Reuse and upgrade the existing command:

~~~sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-multiply-custom-lifecycle
~~~

The command is read-only and has broadcast=false in every RPC path. It must not
load a signing key, construct an approval transaction, mutate the database, or
accept caller-authored evidence as truth.

The output schema is loyal-backyard-rwa-go-lifecycle/v3. The top-level verdict
and process exit code are the Phase 1 release verdict; Phase 2 and the admin
macroview remain explicit independent results:

~~~json
{
  "schema": "loyal-backyard-rwa-go-lifecycle/v3",
  "verdict": "PASS | FAIL | BLOCKED",
  "releasePhase": "phase1",
  "commitment": "confirmed",
  "sourceCommit": "...",
  "deployedImageDigest": "...",
  "manifestSha256": "...",
  "policyCatalogSha256": "...",
  "phase1": { "verdict": "PASS | FAIL | BLOCKED", "checkIds": [] },
  "phase2": { "verdict": "PASS | FAIL | BLOCKED", "checkIds": [] },
  "supplemental": { "adminMacroview": "PASS | FAIL | BLOCKED" },
  "checks": [
    {
      "id": "...",
      "verdict": "PASS | FAIL | BLOCKED",
      "condition": "...",
      "evidence": {},
      "resumeCondition": null
    }
  ],
  "firstFailure": null,
  "blocker": null
}
~~~

- Exit 0 is PASS.
- Exit 1 is FAIL.
- Exit 2 is BLOCKED.
- Any FAIL wins over BLOCKED.
- BLOCKED is reserved for a genuinely unavailable external prerequisite and
  must state the exact resume condition. Missing code, a mismatched deployment,
  or a false invariant is FAIL.
- Output separates static, simulation, submission, confirmation, deployment,
  reconciliation, and live-lifecycle evidence.
- RPC URLs, database URLs, secret names, key material, and environment values
  are never emitted.

The verifier itself may remain a small TypeScript read-only wrapper. The rule
that runtime services are Go applies to observation, decision-making,
transaction construction, submission, confirmation, and reconciliation.

## 4. Frozen deployment manifest

Create one checked-in, generated manifest consumed independently by the Go
worker, policy compiler, admin read model, and verifier. The manifest contains
no secret and binds:

- cluster and confirmed commitment;
- Voltr program, vault, LP mint, idle USDC account, adaptor program, strategy,
  strategy authority, vault cap, fees, and 600-second wait;
- Squads program, Settings account, vault index, derived vault PDA, delegated
  executor, exact USDC ATA, and policy account set;
- USDC and every supported collateral/debt mint and token program;
- all Kamino market, reserve, liquidity-supply, collateral-supply, fee/farm,
  obligation, and authority accounts used by the eleven lanes;
- every swap program and exact allowed mint edge;
- adaptor report schema, config address, and derived report-ticket PDA;
- policy catalog hash and worker decision-schema version;
- fixed MVP route and risk parameters; and
- source commit and immutable deployed image digest.

Discovery-time addresses are provisional until the manifest generator derives
and independently reads them from confirmed mainnet:

| Identity | Discovery value |
| --- | --- |
| Voltr vault | HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA |
| Loyal adaptor program | FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW |
| Active strategy config | 9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj |
| Derived report ticket PDA | C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5 |
| Squads Settings | 5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6 |
| Squads vault index | 0 |
| Squads vault PDA | ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh |
| Delegated executor | 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 |
| Squads USDC ATA | EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe |
| Voltr program | vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8 |

The generator must resolve Maple/USDG and every other current reserve address
from current mainnet state. A stale or guessed address is FAIL. If the existing
v1 adaptor config cannot be upgraded with a one-use, fully authenticated
migration, create a fresh v2 strategy config under the same adaptor program and
prove that the old strategy has zero receipt supply and is unreachable. Do not
add a generic mutable rebind instruction.

## 5. Minimal adaptor v2

### 5.1 What “signed by the Squads smart account” means

A Squads vault is a PDA and has no private key. The only accepted proof is that
the Squads program granted signer privilege to the exact derived vault PDA with
invoke_signed and that privilege reached the adaptor through the authorized CPI
chain.

Voltr does not forward Squads signer privilege through its adaptor CPI. The
approved Phase 1 topology therefore proves Squads origin through one reusable
adaptor-owned report ticket. Every deposit, withdrawal, and NAV-report path must
require all of:

1. squads_vault.key equals the immutable configured vault PDA;
2. squads_vault.is_signer is true at the adaptor's direct ArmReport instruction;
3. the PDA re-derives from the configured Squads program, Settings account, and
   vault index;
4. the Settings account is owned by the configured Squads program and decodes
   to the expected authority graph;
5. voltr_strategy_authority.key equals the exact PDA derived from the configured
   Voltr program, vault, and strategy;
6. voltr_strategy_authority.is_signer is true;
7. every vault, mint, token program, ATA, config, and writable account equals
   the immutable config; and
8. the outer transaction executor is the exact delegated signer authorized by
   the exact Squads ProgramInteraction policy; and
9. the canonical runtime emits one Squads sync payload containing exactly two
   ordered inner instructions: direct adaptor ArmReport, then the exact Voltr
   capital call whose adaptor CPI consumes that ticket.

Checks 1-7 belong in the adaptor. Checks 8-9 belong in the exact Squads policy
and canonical builder and are independently decoded by the verifier. An address
match without signer privilege at ArmReport is FAIL. Direct address-only Voltr
authorization is forbidden.

The ticket PDA is derived under the adaptor program from
`[b"report_ticket", strategy_config]`. It is initialized once, is exactly 96
bytes, and has this frozen v1 layout:

~~~text
discriminator[8] | version:u8 | bump:u8 | armed:u8 | reserved[5]
strategy_config:Pubkey | last_consumed_sequence:u64
active_sequence:u64 | active_wire_sha256:[u8;32]
~~~

The discriminator is `f568b6c53ae774ed`; version is 1; reserved bytes are
zero. InitializeReportTicket has discriminator `7c29df0da5f6463e`, no data,
and exact accounts payer signer/writable, config read-only, ticket writable,
system program read-only.

ArmReport has discriminator `a4aff629b28c2303`, exactly 79 bytes, and exact
data `discriminator | operation:u8 | VoltrTail[70]`. Operation is 0 for deposit
or 1 for withdraw. VoltrTail is the exact amount u64 plus the Some tag, 57-byte
length, and ReportV1. Its accounts are config read-only, ticket writable,
Settings read-only, exact Squads vault signer/read-only, and exact executable
Squads program read-only.

ArmReport requires sequence=observed_slot, sequence>last_consumed_sequence, a
nonfuture/fresh observed slot, the exact configured Squads signer/Settings
graph, and an exact hash of the complete Voltr tail. A fresh active ticket
rejects overwrite. An active ticket whose active_sequence is older than the
configured max report age may be replaced by a newer otherwise-valid report;
this is the bounded recovery path for an ArmReport transaction that landed
without its consume. ArmReport writes armed=true, active_sequence, and
active_wire_sha256. The following Voltr instruction forwards the same ticket as
remaining account index 17; the adaptor receives it as account index 8,
writable. The capital path requires the exact Voltr strategy-authority signer
and exact config/accounts, matches the complete wire hash and sequence,
performs the transfer/return-data work, then consumes the ticket and advances
last_consumed_sequence. Config is read-only throughout. Successful consume
retains only last_consumed_sequence; it clears armed, active_sequence, and
active_wire_sha256 to zero. The exact current PDA is
C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5 with bump 254.

The canonical runtime always emits exactly the two-instruction ArmReport then
Voltr order. Deployed Squads ProgramInteraction validates each supplied
constraint but does not enforce that every policy constraint is covered, so an
Arm-only payload is a bounded expected-success case, not a negative-matrix
rejection. Its signed-unsent proof must show only the ticket simulation image
changes, with exact canonical active_sequence/wire hash and unchanged
last_consumed_sequence; independent confirmed readback remains unchanged and
the signed wire has no onchain status. This subset behavior is never used by the
runtime.

The frozen ProgramInteraction constraints use indexes `[0,1]` for ArmReport
and Voltr. ArmReport pins adaptor program/data plus accounts 0=config and
1=ticket; the adaptor revalidates Settings, vault signer, and Squads program.
Allocation/NAV pins Voltr account indexes `[0,2,3,8,11,12,13,14,15,16,17]`;
withdraw pins `[0,2,5,6,9,12,13,14,15,16,17]`. The unrelated policy-64 staging
transfer remains a single instruction with constraint index `[0]` and cannot
arm a ticket.

### 5.2 Immutable config and compact report

The config is initialized once and remains read-only on every capital and NAV
path. It freezes:

- schema version;
- Voltr program, vault, strategy, strategy config, and strategy authority;
- Squads program, Settings, vault index, and derived vault PDA;
- asset mint, token program, and exact Squads asset ATA;
- maximum reportable NAV and maximum report age.

The report payload is fixed-length and rejects trailing data:

~~~text
ReportV1 {
  version: u8,
  sequence: u64,
  observed_slot: u64,
  nav_after_raw: u64,
  snapshot_digest: [u8; 32]
}
~~~

Acceptance rules:

- sequence is nonzero and exactly equals observed_slot;
- observed_slot is not from the future and is not stale;
- nav_after_raw is at or below the configured cap; zero is valid;
- the digest is nonzero and binds the complete Go NAV component snapshot,
  receipt fingerprint, and policy catalog hash;
- a sequence/slot mismatch, stale slot, wrong signer, wrong strategy,
  duplicate mutable account, or extra bytes fail before state changes; and
- the config is never mutated and the exact NAV is returned to Voltr through
  Solana return data.

The strategy config remains immutable and read-only. The report ticket is the
only writable authentication/replay cell. Its monotonic last-consumed sequence,
fresh-active overwrite rejection, stale-active recovery, and consumed-state
transition are the onchain one-use boundary; the pinned delegate, serialized Go
lease, persist-before-send journal, and signed-wire uniqueness remain defense
in depth. Reconciliation must complete before a new report wire is built.

This proves report origin and freshness, not the economic truth of the number.
The sole verifier independently recomputes the same NAV from RPC and fails on a
mismatch. That is the deliberate MVP trust boundary.

### 5.3 Capital paths

- Deposit with amount greater than zero transfers exactly that USDC amount from
  the exact Voltr strategy token account to the exact Squads USDC ATA, accepts
  the report, and returns the reported NAV.
- Deposit with amount zero moves no tokens but may accept a fresh report.
- Before withdrawal, the Go worker stages the exact requested USDC amount from
  Squads into the exact Voltr strategy token account through an approved Squads
  payload. The adaptor validates the already-staged balance, accepts the
  post-withdraw report, and returns NAV. Voltr then restores its own idle
  balance.
- The adaptor does not pull through a delegate, choose a protocol, calculate
  APY, enumerate Kamino accounts, maintain an offchain queue, or contain a route
  optimizer.

The onchain program remains a narrow authenticated bridge and NAV-report
boundary.

## 6. Exact Squads policy catalog

### 6.1 Authorized lanes

The semantic catalog contains exactly these eleven tuples:

| Market | Collateral | Allowed debt |
| --- | --- | --- |
| OnRe | ONyc | USDC, USDG, USDS |
| Prime | PRIME | USDC, PYUSD, USDS |
| Maple | syrupUSDC | USDC, USDG, PYUSD |
| AUTO | AUTO | PYUSD |
| Ethena | USDe | PYUSD |

Each tuple authorizes exactly Deposit, Withdraw, Borrow, and Repay against its
own current reserve graph: 44 semantic Kamino permissions. Every permission
pins the exact market, collateral reserve, debt reserve, liquidity/collateral
supplies, obligation identity, authority graph, programs, account order, roles,
instruction tag, and data constraints.

The existing global debt-market/custody cartesian form is not acceptable. For
each lane, a wrong reserve with the same mint and a cross-lane obligation must
be rejected by Squads before K-Lend executes.

### 6.2 Swap graph

The catalog permits:

- each of four stablecoins to each of five RWA tokens: 20 directed edges;
- each RWA token back to each stablecoin: 20 directed edges; and
- each stablecoin to every other stablecoin: 12 directed edges.

That is exactly 52 directed edges. It permits no RWA-to-RWA edge and no self
edge. Program, authority, source/destination mint, source/destination custody,
token program, slippage-bound fields, and account count are constrained by
Squads. ProgramInteraction policy bytes do not encode signer/writable roles;
the canonical Go builder therefore owns the exact role vector and rejects any
role drift before signing, while signed simulation proves that the accepted
wire produces only the expected downstream mutation. Token-2022 and classic
SPL identities are explicit and never inferred from a token symbol.

### 6.3 Packing ladder

Optimize rent by market without weakening an invariant. Build complete signed
policy-create/update transactions and enforce the 1,232-byte packet limit.
Select the first safe rung that fits:

1. five market policies, one swap policy, and two Voltr bridge policies;
2. split only an overflowing market into risk-increasing and risk-reducing
   operations;
3. split an overflowing market by collateral/debt lifecycle;
4. use exact per-lane policies only for a group that still cannot fit; and
5. split swap inbound/outbound/stable or bridge stage/return only when its own
   packet overflows.

Best case is eight policies, but eight is not a correctness requirement until
the real packets are measured. The chosen first-fitting layout, packet sizes,
policy PDAs, semantic expansion, total rent, and catalog hash are frozen before
installation. Never drop a reserve, signer identity, program, account-role
check, or data constraint to save rent. Account roles remain a mandatory
builder invariant even though they are not representable in Squads policy
account bytes.

The two bridge policy families cover:

- authenticated Voltr allocation and NAV reporting; and
- exact Squads-to-Voltr staging and withdrawal restoration.

The verifier must prove allowAnyAdaptor is enabled on this exact vault and the
strategy receipt resolves to the exact deployed adaptor program. It must also
scan every policy usable by the runtime delegate. Any stale, temporary,
duplicate, fallback, or broader policy that could move this vault's Voltr,
Kamino, SPL, or Token-2022 capital is FAIL. Unrelated policies for a different
delegate are reported but do not fail this route.

## 7. One Go worker

### 7.1 Shape

Implement one module and one process:

~~~text
go/backyard-rwa-worker/
  go.mod
  cmd/backyard-rwa-worker/main.go
  internal/backyardrwa/
    config.go
    state.go
    observe.go
    decide.go
    build.go
    execute.go
    reconcile.go
    store.go
    nav.go
~~~

Keep packages concrete. Do not introduce interface layers for a single RPC,
database, signer, or route. The process performs a five-second confirmed
RPC/database loop. A future LaserStream wake-up may reduce latency but is not a
dependency or a second execution path.

All production observation, decision, transaction construction, simulation,
submission, confirmation, reconciliation, and NAV calculation for this vault
run in Go. Rust remains valid for the onchain adaptor and policy/action
libraries; TypeScript remains valid for build-time catalog generation and the
read-only verifier.

### 7.2 Serialized lifecycle

Every iteration does:

~~~text
recover ambiguous work
-> read one coherent confirmed snapshot
-> choose zero or one literal action
-> persist the decision
-> build and simulate
-> persist signed bytes and signature
-> persist broadcast intent
-> send once
-> confirm
-> reconcile exact effects
-> re-observe before choosing again
~~~

Never send first and journal later. Never rebuild or resend an ambiguous
signature. Recover it from chain, then reconcile or hold for manual review.
There is at most one nonterminal operation for this vault.

### 7.3 Closed action set and decision order

The MVP action enum is:

- HOLD
- RECOVER_TRANSACTION
- VOLTR_ALLOCATE_TO_SQUADS
- OPEN_PRIME_USDC_STEP
- DELEVER_PRIME_USDC_STEP
- STAGE_SQUADS_TO_VOLTR
- VOLTR_RESTORE_IDLE
- REPORT_NAV

The decision function is pure over a persisted snapshot and applies this exact
precedence:

1. recover or reconcile a nonterminal transaction;
2. HOLD_MANUAL_RECOVERY on identity, policy, custody, snapshot, conservation,
   unknown-token, or multiple-obligation ambiguity;
3. deleverage immediately when the hard LTV bound is reached;
4. when any active Voltr withdrawal receipt exists, prohibit new risk and
   advance exactly one unwind, stage, restore, or NAV step;
5. if Squads idle already covers aggregate receipt demand, stage the exact
   conservatively rounded shortfall to Voltr;
6. report NAV after each confirmed capital mutation or when the last accepted
   report is older than 60 seconds;
7. with no receipt, allocate eligible Voltr idle to Squads;
8. advance one PRIME/USDC entry step toward the fixed target; otherwise
9. HOLD with an explicit reason.

After every confirmed action, discard the remainder of the old plan and
recompute from a fresh slot. There is no durable multi-transaction plan.

### 7.4 Fixed initial route and risk rule

Use PRIME collateral with USDC debt because it keeps the base asset in USDC,
uses classic SPL debt, and avoids a Token-2022 or cross-stable dependency in the
first live path.

- target LTV: 5,000 bps;
- hard LTV: min(6,000 bps, current liquidation-threshold bps minus 1,500 bps);
- if hard LTV is not strictly above target LTV, do not enter;
- do not enter unless observed net APY is positive after current borrow cost
  and explicit protocol fees;
- require fresh reserve state, sufficient capacity, a complete policy hash,
  and a buildable exit packet; and
- if any gate is false, HOLD. Never silently choose a second route.

Opening size is the minimum of eligible idle capital, remaining policy limit,
reserve capacity, and the amount that keeps post-action LTV at or below target.
Withdrawal sizing is the exact aggregate receipt shortfall plus conservative
rounding needed to leave the requested raw USDC in Voltr idle before the
deadline. Safety deleverage and withdrawal restoration outrank earning yield.

The policy catalog makes the other ten routes possible later; changing the
worker's fixed route or target requires a reviewed manifest version, not a
hidden environment flag.

## 8. Database reuse and decision trail

Reuse multiply_route_states, multiply_operations, and
multiply_position_snapshots/current projections. Add only a narrow migration
for route kind backyard_rwa_v1, the closed action enum, and JSON/schema
constraints. Do not add a second decision table, outbox, receipt table, or
signed-submission table.

One multiply_operations row is both the durable decision and execution journal.
It contains or binds:

- route ID, operation ID, generation, action, and reason code;
- amount raw, source, destination, and receipt-demand raw;
- observation slot, snapshot hash, policy catalog hash, and manifest hash;
- precondition accounts and raw balances;
- unsigned message hash, signed transaction bytes/signature, and simulation
  result;
- broadcast intent, confirmation slot/status, exact reconciled deltas, and
  terminal state; and
- timestamps and a sanitized error/recovery reason.

The database may project state, but chain receipts and token balances remain
truth. The route state stores the sorted active-receipt fingerprint, aggregate
amount, and earliest deadline. Duplicate observations produce the same
decision identity and cannot create a second active operation.

Allowed transitions are:

~~~text
decided -> built -> simulated -> signed -> broadcast_intent
        -> submitted -> confirmed -> reconciled

any pre-broadcast state -> failed
ambiguous submitted state -> reconciling -> reconciled | manual_recovery
~~~

No transition from failed or ambiguous to a fresh send is allowed without a
new observation and new operation identity.

## 9. NAV calculation and report

From one coherent confirmed minContextSlot snapshot, Go computes:

~~~text
Voltr strategy USDC
+ Squads idle USDC and approved idle stablecoins
+ current value of approved RWA collateral and active Kamino deposits
- current value of active Kamino borrows
= reported NAV raw
~~~

Implementation rules:

- decode classic SPL and Token-2022 accounts with explicit owners;
- value each custody once even when aliases appear in multiple instruction
  graphs;
- permit exactly zero or one recognized active Kamino obligation in the MVP;
- reject unknown token balances, multiple nonzero obligations, stale reserve or
  oracle state, negative arithmetic, overflow, and snapshot-slot mixing;
- use conservative deterministic rounding and persist every component;
- bind sorted component account hashes, raw values, slot, receipt fingerprint,
  manifest hash, and policy catalog hash into snapshot_digest; and
- derive sequence=observed_slot from the one coherent confirmed snapshot, and
  build no later report wire until the previous report outcome is reconciled by
  the serialized delegate/database journal.

The verifier recomputes NAV through independent RPC decoding. It must not call
the Go worker's NAV function or trust the stored aggregate. Equality is in raw
USDC units; an unexplained one-unit difference is FAIL.

## 10. Thin Vault integrations page

Add one read-only page under Vault integrations in the existing Loyal Apps
admin surface. It uses the existing database projections and direct read-only
RPC where already available. It shows:

- current AUM/NAV, report slot/time, and freshness;
- current APY or an explicit unavailable state;
- Voltr idle, Squads idle, PRIME collateral, USDC debt, LTV, and route status;
- vault/adaptor/Squads identities, cap, fees, admins, and withdrawal wait;
- recent decisions and money movements with signature and terminal status; and
- deposit, withdrawal-request, Voltr-restoration, and claim history.

No charts, optimizer controls, mutation buttons, new analytics store, or custom
indexer are required. A wrong or stale number must be labeled; it must not be
silently replaced with zero.

## 11. Required verifier checks

Run these in fail-fast order.

### V01 contract_and_forbidden_surface

PASS when the v11 Phase 1 contract and v3 output schema are exact, the fixed route is
PRIME/USDC, and repository/deployment search proves no reachable Rust or
TypeScript money-moving worker for this vault. Fail on a second writer,
optimizer, saga, caller-selected manifest, or broadcast-capable verifier.

Evidence: source AST/build graph, service commands, environment-key names
without values, and deployed image metadata.

### V02 adaptor_identity_and_signer

PASS when independent decoding proves immutable config bindings, the one exact
adaptor-owned report-ticket PDA/layout, exact Squads-vault PDA signer at
ArmReport, and exact Voltr strategy-authority PDA signer at ticket consumption.

The canonical signed-unsent transaction must simulate successfully as one
Squads sync payload with exactly two ordered inner instructions:
`ArmReport -> Voltr capital/adaptor consume`. The config remains read-only; the
ticket is writable at Voltr remaining account index 17 and adaptor account index
8; the final ticket is inactive with a monotonic last-consumed sequence; and
exact adaptor NAV return bytes are independently read from transaction metadata
or, when Squads CPI wrapping leaves metadata returnData null, the exact immutable
`Program return:` runtime log.

The exact v11 matrix contains 38 rejections plus one bounded expected-success
Arm-only proof. Rejections cover: direct Voltr without a ticket,
consume-before-arm, reversed order, extra/third instruction, different second
instruction, second consume, same/lower sequence re-arm, fresh arm while
active, nonsigner/wrong Squads vault, wrong Settings owner/index/address
lookalike, wrong delegated executor/policy, nonsigner/wrong Voltr authority,
wrong ticket PDA/owner/config/index/writability, wrong operation/amount/wire
hash, zero or mismatched sequence/observed-slot, stale/future slot, oversized
amount/NAV, trailing bytes, wrong vault/strategy/mint/token program/ATA, and
duplicate/aliased writable accounts. The Arm-only proof must show the exact
ticket-only armed overlay described above. A valid ArmReport followed by a
deliberately failing Voltr leg must roll the ticket, config, and capital fully
back.

Evidence: program ELF hash and authority, immutable config bytes, ticket bytes
before/after, PDA derivations, exact Squads policy and two-instruction decoding,
signed simulation logs, pre/post account diffs, rollback proof, exact account
indexes/roles, and signer metas observed separately at ArmReport and consume.
Every signed-unsent row also proves its signature absent on chain and an
independent confirmed readback at or after the simulation context.

### P2 catalog_semantics_and_packing

Phase 2 PASS when the frozen policy catalog expands to exactly 44 Kamino permissions
and 52 directed swap edges, every lane pins its exact debt reserve, the chosen
packing rung is the first one whose full signed create/update transactions fit
1,232 bytes, and total rent is reported.

Batch signed-unsent simulations by structural group: three-lane market
policies, singleton market policies, swap graph, and bridge lifecycle. Do not
send one live transaction per lane. Negative cases must prove same-mint
wrong-reserve, cross-lane obligation, unapproved edge, extra instruction,
amount-cap breach, and signer substitution fail in Squads before the downstream
protocol. Writable-role substitution must be rejected by the canonical Go
builder before signing; an independently constructed mutated wire must also
simulate without downstream state mutation.

Evidence: canonical semantic JSON, packet bytes/sizes, catalog hash, policy
account bytes/owners/activation, simulation logs, and runtime-delegate policy
scan. This result is always emitted, but it is not a Phase 1 release gate.

### V04 go_state_machine_and_store

PASS when the Go module builds and focused deterministic tests prove:

- decision precedence and zero-or-one action;
- withdrawal preemption at every open-loop state;
- exact KLend utilization decoding/math, durable utilization HOLD/no-send,
  hard-LTV protection, and withdrawal precedence;
- duplicate observation idempotency;
- persist-before-send ordering;
- exact signed-byte recovery without blind resend;
- re-observation after every confirmed mutation;
- transition constraints and one nonterminal operation; and
- independent NAV fixtures for classic SPL, Token-2022, rounding, unknown
  custody, and mixed-slot rejection.

These are small Go/domain and database-contract tests. They are not an SVM
suite.

Evidence: Go version, test results, migration checksum, schema introspection,
and fixture hashes.

### V05 deployed_single_writer

PASS when exactly one active Go service owns this route, its immutable GHCR
image digest maps to the source commit and manifest, its command invokes the Go
binary directly, and no active Rust/TypeScript worker can claim the vault's
route ID, delegated signer, or advisory lock.

The production service has exactly one instance. Its active route lease owner
is exactly `render:<service-id>:sha-<40-hex-source-commit>` and its expiry is in
the future, binding database authority to the independently read live service
and immutable image rather than to an arbitrary nonempty owner string.

Evidence: deployment/service metadata, image digest, startup identity log,
database lease owner, and absence of competing recent writes.

### V06 live_internal_lifecycle

The lifecycle evidence schema is `loyal-backyard-rwa-live-lifecycle/v3`; older
evidence predating the utilization-HOLD scope decision is declaration-only and
cannot pass.
PASS only after one real internal Backyard lifecycle at an explicitly approved
operational amount proves:

1. user deposit confirmed into Voltr idle;
2. adaptor allocation confirmed into the exact Squads ATA;
3. worker decision persisted before send;
4. the worker encounters the confirmed Kamino utilization ceiling, persists a
   durable HOLD, and emits no later risk-increasing action;
5. authenticated sequence=observed_slot NAV returned by the adaptor and
   independently recomputed, with the exact atomic ArmReport -> Voltr consume
   ticket transition;
6. withdrawal receipt confirmed and no later risk-increasing action emitted;
7. enough position unwound and exact requested raw USDC restored to Voltr idle
   before the 600-second deadline;
8. post-deadline user claim confirmed; and
9. final Voltr, Squads, Kamino, adaptor NAV, receipt, database, and user
   balance deltas conserve value within explicit protocol fees.

This is the first live use, not a separate canary. Confirmed signatures and
post-transaction account reconciliation are sufficient; finalized polling is
not required.

Evidence: signatures, slots/block times, independently decoded instructions
and receipts, raw pre/post balances, operation rows, report sequence and digest,
ticket PDA bytes/sequence/wire hash for every bridge/NAV transaction, immutable
config hash, and the observed dust-cycle conservation equation. Across the live
proof, the verifier derives NAV identity from database observations, signed wire,
adaptor return data, and current account state; it accepts no operator-entered NAV.

Target-LTV/open-position proof and a positive-yield attestation are not Phase 1
gates because the confirmed utilization ceiling prevented borrowing and no
position snapshot exists. Positive-yield monitoring, a fresh leveraged open,
and deliberate pre-deadline claim rejection are explicit fast follows; their
absence must be reported and must never be represented as passing evidence.
Across the lifecycle, every bridge/NAV signature must contain one ArmReport and
the matching report embedded in the following Voltr consume; ticket sequences
must increase strictly, no
ticket may remain armed, and the database journal must bind the same signed wire,
ticket sequence, and wire hash before send.

### V07 admin_macroview_truth

PASS when the deployed Vault integrations page names the same vault/manifest
and its AUM, component balances, route/LTV, settings, receipt history, movement
history, and signature statuses equal independent RPC/database reads at the
displayed freshness boundary.

Visual polish is not a readiness gate. A missing deployment or materially
incorrect/stale unlabeled value is FAIL.

## 12. Delivery order

### Phase 0: freeze truth before implementation

1. Upgrade the sole verifier to emit the v3 phase-separated schema and a truthful baseline.
2. Generate the current-mainnet manifest and resolve every lane, especially
   Maple/USDG.
3. Freeze the adaptor v2 ABI, Go action enum, database transition diagram, and
   policy semantic JSON.
4. Build the canonical signed-unsent Squads -> Voltr -> adaptor packet first.

Exit: V01 has a truthful result and the exact two-instruction ticket-forwarding
topology simulates. No downstream work may compensate for failed signer or
writable-ticket propagation.

### Phase 1: fixed PRIME/USDC release

Adaptor track:

- implement immutable read-only config/report v2, the one reusable ticket PDA,
  Squads-signed ArmReport, Voltr-signed consume, stale-active recovery, exact
  two-instruction runtime topology, exact transfers/return data, and independent
  ABI fixtures;
- deploy the smallest upgrade and freeze its ELF/program-data authority hash.

Fixed-policy track:

- correct exact debt-reserve binding;
- install and pin the four bridge/NAV policies and the complete PRIME/USDC
  deposit, borrow, repay, and withdraw policy/account vectors.

Go track:

- add the one worker module, focused decision/NAV/build/reconcile logic, and the
  narrow database migration;
- support only the closed action set and fixed PRIME/USDC route.

Admin track:

- define the read model against the frozen fields and build the thin page after
  the migration shape is fixed.

Release sequence:

1. Bind/upgrade the exact Voltr strategy config and adaptor config, including
   the required minimal adaptor receipt while preserving allowAnyAdaptor.
2. Publish one immutable Go image and deploy exactly one active writer.
3. Prove V05 from live Render metadata, startup identity logs, and the active
   database lease; checked-in JSON is not deployment truth.
4. Run one real internal deposit -> allocate -> attempt PRIME/USDC entry ->
   collateral-only state -> utilization HOLD -> request -> unwind -> restore ->
   claim lifecycle and independently reconcile V06.
5. Evaluate V07 against loyal-apps origin/main, the production Vercel commit,
   an authenticated page response, and the same read-only database snapshot.

Each state-changing transaction requires explicit operational approval,
simulation, confirmed signature, and immediate reconciliation. This plan does
not grant that authority.

Exit: V01, V02, V04, V05, and V06 pass and the command exits 0. V07 is reported
independently as supplemental operating-surface truth and must not be
misrepresented as deployed when credentials or the production commit are
unavailable.

### Phase 2: expand the policy catalog

1. Resolve all current market/reserve/custody identities for the eleven lanes.
2. Add all 44 Kamino permissions and 52 swap edges.
3. Measure the packing ladder, install the first-fitting policy groups in
   batches, close superseded policies, and re-scan the runtime delegate.
4. Run the grouped signed-unsent and negative simulations; do not live-test
   every lane one by one.

Exit: P2 catalog_semantics_and_packing passes independently. A Phase 2 failure
is visible in the sole verifier but does not retroactively falsify a healthy
fixed PRIME/USDC Phase 1 lifecycle.

## 13. What may be batched and what may not

Batch for speed:

- policy creation/removal by measured packet fit;
- signed-unsent simulations by structural policy group;
- read-only account fetches with one minContextSlot;
- Go domain cases in table-driven tests; and
- admin-history reads.

Never batch across a correctness boundary:

- do not combine ambiguous and new transaction submission;
- do not open the next leverage step before confirming/reobserving the prior
  one;
- do not build a later NAV signed wire before the previous report reconciles in
  the serialized delegate/database journal;
- do not install a policy packet whose fully signed bytes were not measured;
- do not enable the Go writer while any old writer can claim the route; and
- do not treat gross token outflow as withdrawal restoration or user claim
  proof.

## 14. Completion ledger

The final handoff records each layer separately:

| Layer | Required terminal evidence |
| --- | --- |
| Static | source/ABI/catalog/manifest hashes and forbidden-writer scan |
| Simulation | grouped canonical packets and required negative failures |
| Submission | explicitly approved signatures, one generation each |
| Confirmation | confirmed slots and decoded instructions |
| Deployment | immutable Go image and sole active writer |
| Reconciliation | exact account/database effects and conservation |
| Live | one complete internal Backyard deposit-to-claim lifecycle |
| Observation | deployed macroview equals independent reads |

Implemented, merged, deployed, confirmed, reconciled, and PASS are different
states. Only the final row-complete ledger permits PASS.

## 15. Simplicity audit

Before merge, delete or reject any component that introduces:

- a second decision/execution journal;
- a Rust/TypeScript runtime bridge;
- a generic adaptor router;
- mutable route selection;
- a multi-transaction plan object;
- a second transaction sender;
- a second NAV aggregate trusted by the verifier;
- a per-lane live-test harness; or
- an abstraction with only one implementation and no external contract.

The intended system is one thin onchain adaptor, one exact policy catalog, one
serialized Go process, the existing database, one small admin page, and one
verifier. Anything else needs a concrete invariant that these pieces cannot
protect.
