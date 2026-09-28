# HXtk strategy-two bootstrap runbook (2026-09-08)

Repair path for Voltr vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA` and the
custom NAV adaptor `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW` after the
strategy-one receipt orphaning (see
`docs/plans/backyard-rwa-adaptor-strategy2-audit-2026-09-08.md`). Strategy two is
a fresh adaptor config keypair on the SAME vault/adaptor/Settings, a delegated
executor, and four policies at fresh seeds. Everything in this runbook is
unsigned-by-default: every rehearsal below loads no key material and never sets
`CONFIRM_MAINNET`.

> Cutover week note (owner decision, 2026-09-10): ONE hot key is in play, so the
> strategy-two delegated executor reuses the v2 executor
> `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`. Only the adaptor config must
> be fresh; key separation is required before third-party money.

### Compiler build hygiene

Policy compilation uses the per-checkout target
`target/backyard-voltr-compilers`; it never uses the shared
`.phase3-recovery/target`. The shared directory remains reserved for
`bun run build:adaptor` (whose script verifies the artifact hash) and Rust
tests.

## Identities and derivation

| Role | Value / derivation |
| --- | --- |
| Voltr vault | `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA` (unchanged) |
| Custom adaptor | `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW` (unchanged) |
| Squads Settings | unchanged; manager `ST999…` (vault), settings signer `BAqg…` |
| v2 strategy config (retired) | old adaptor config key (stays on chain until step 6) |
| strategy-two config | NEW keypair, operator-derived offline (below) |
| strategy-two delegated executor | the v2 executor `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` this week (owner decision, one hot key) |
| Derivation domain | `loyal-rwa-multiply-mainnet-v3` (`STRATEGY_TWO_DERIVATION_DOMAIN`) |

The real keypairs never enter the repo or chat. Only the strategy-two **config**
is derived this week: the operator derives it from the setup-admin seed via the
existing domain signer
(`tools/backyard-voltr/src/integrations/signer.ts`,
`deriveRwaMultiplyStrategySigningMaterial` over `STRATEGY_TWO_DERIVATION_DOMAIN`)
under `op run --env-file=tools/backyard-voltr/.env.1password -- …`, and supplies
only the resulting **address** to every command below (`--config`). The
delegated signer is NOT derived: it is the existing v2 executor
`62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` (owner decision, one hot key),
passed wherever `--delegated` / `--delegated-signer` appears. The bootstrap tool
derives the same config keypair from the admin seed and refuses `--config`
values that do not match that derivation.

Derived per config key (compute with
`src/domain/rwa-multiply-strategy2-route-spec.ts::deriveStrategyTwoVoltrAccounts`):

- `strategy_init_receipt2 = ["strategy_init_receipt", vault, config2]` (Voltr)
- `vault_strategy_auth2 = ["vault_strategy_auth", vault, config2]` (Voltr) — PDA
  signing authority, no account data on chain
- `custody2 = ATA(vault_strategy_auth2, USDC)` (permissionless create)
- `report_ticket2 = ["report_ticket", config2]` (adaptor program)

## Policy seeds and caps

- The first strategy-two installer invocation requires the finalized Squads
  Settings `policy_seed` counter to be exactly `144` and requires the finalized
  basic policy anchor at seed `144`
  (`Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz`, the withdraw policy of the
  installed basic set) to exist. Install **the next four seeds (expected
  145–148 after the basic set installed at 141–144)** (allocation /
  nav-refresh / stage-withdrawal / withdraw).
  The installer records this expectation in the shared `--seed-journal`; every
  later invocation reads that journal and refuses a different seed set.

  The first execute creates a journal whose finalized readback must remain:

  ```json
  {
    "policySeedBefore": "144",
    "expectedSeeds": ["145", "146", "147", "148"],
    "settingsAddress": "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6",
    "genesisHash": "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
    "strategyTwoConfig": "<config2-addr>",
    "delegatedSigner": "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
    "anchorPolicy": "Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz",
    "anchorPolicyDataSha256": "43d09b3cbdd63f1c775f7a660ac75ac76f699b18bd1298ec1bf87d088e6d535a",
    "observationSlot": "<finalized-observation-slot>"
  }
  ```

  The config and observation slot above illustrate the finalized readback
  shape; the installer writes the real operator-derived config and observed
  slot. The anchor hash is NOT dynamic: it is pinned to the finalized basic
  install readback — field `accountDataSha256` for seed `144` in
  `docs/evidence/backyard-rwa-basic/policy-install-readback-v1.json` — and the
  installer refuses any live anchor bytes that hash differently. The one-shot
  repair policy at seed `140` was consumed and removed, so it can no longer
  serve as an anchor.
  Every invocation revalidates the Settings counter, genesis, Settings
  identity, strategy-two identities, anchor policy ownership/presence, anchor
  account hash against that readback, and the finalized observation before it
  proceeds.

  The no-execute installer readback prints the same `expectedPolicySeeds` and
  the current finalized Settings counter; before the first create those values
  are `145–148` and `144`, respectively. Immediately before every send the
  installer re-reads finalized Settings and aborts if the operation's journaled
  counter no longer matches.
- Amount cap **100_000_000 raw (100 USDC)** per instruction; raising it is a seed
  rollover, never an edit.
- **Daily USDC spending limit on EVERY policy: 300_000_000 raw (3 × operational
  cap) per 1-day period.** Every 145–148 create attaches the identical
  USDC/Daily/300_000_000 limit. Squads charges a spending limit only against
  balance *decreases*, so the limit is inert on the allocation and nav-refresh
  lanes (the adaptor's deposit path moves custody INTO the Squads ATA there) and
  binding on every lane that can reduce the Squads ATA — the stage-withdrawal
  and withdraw policies. This is the binding cap for a compromised delegate: the
  100 USDC amount cap is per inner instruction, and Squads ProgramInteraction
  cannot express instruction cardinality, so without the spending limit one
  delegated execution could pack arbitrarily many legal stage transfers; with it,
  outflow per period is bounded no matter which policy executes or how many
  instructions one execution packs.
- **Packet fit with the limit embedded** (signed create packets, measured by
  `every_policy_carries_the_daily_usdc_spending_limit` in
  `crates/loyal-actions/src/autonomous_vaults/voltr_custom.rs`, bound 1_232):

  | Policy | Seed | Packet bytes |
  | --- | --- | --- |
  | allocation | 145 | 1_195 |
  | nav refresh | 146 | 1_159 |
  | stage withdrawal | 147 | 656 |
  | withdraw | 148 | 1_195 |
- **What remains unbounded:** repeated arm/capital pairs are NAV *reports*, not
  USDC outflow. They are bounded by the reported-NAV cap
  (1_000_000_000_000 raw = the vault maxCap, closing audit finding U3, pinned at
  arm offset 39 and capital offset 51) and by the adaptor's report-interval
  enforcement (max age 32 slots), not by a transfer budget. They cannot move
  funds: only the stage policy's spending-limited lane can.
- **Hash authority:** the policy data hashes recorded in
  `docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-bootstrap.simulated.json`
  are throwaway-identity rehearsal values (`authoritative: false`). The install
  hashes exist only after the operator derives the real config keypair and the
  target is recompiled. For the **retired v2 policies 62–65**, the authoritative
  frozen byte hashes live in
  `docs/evidence/backyard-rwa-go/policy-helius-bridge-lifecycle-v2.json` and
  `docs/evidence/backyard-rwa-go/policy-signed-unsent-v1.json`
  (`docs/evidence/backyard-rwa-go/policy-install-readback-v1.json` predates the
  bridge and no longer contains those seeds).

## Order of operations (cutover)

**Steps 1–5 are one planned bridge outage of the backyard bridge worker.** The old
worker must be fully stopped and verified absent before any v2 policy is removed,
because a still-running container holds the old delegated key and could otherwise
execute strategy-one/v2 policies that are still installed. Do not shorten the
order; do not run the new image until step 7.

1. **Rehearse (no keys):** `strategy-two-bootstrap.simulated.json` (wire A), the
   LiteSVM ordered proof `crates/squads-test-harness/tests/voltr_reset_sequence.rs`
   (R5a), and `policy-remove-62-65.simulated.json`.
2. **Bootstrap strategy two on the Voltr side** — three wires, in order (step 3
   below for commands). Privileges are transaction-wide, so `initialize_config`
   (config WRITABLE_SIGNER) can never share a transaction with
   `initialize_report_ticket` (config READONLY):
   - **Wire A** `initialize_config`: 13 accounts; args 17 bytes = vault index `0`,
     `max_report_nav_raw = 1e12`, `max_report_age_slots = 32`. Config keypair signs.
   - **Wire B** `initialize_report_ticket` (payer, config readonly, ticket, system).
   - **Wire C** atomic round-trip: `create AssociatedTokenIdempotent` (custody2) →
     `updateVaultConfig(Manager = BAqg)` → `initializeStrategy` (BAqg signs via
     Squads) → `updateVaultConfig(Manager = ST999)`. The round-trip must stay in
     one transaction so the manager is always restored; it must leave the vault
     state byte-identical (the execute path asserts this).
3. **Compile and install the next four policies signed by the delegated
   executor** — the existing v2 executor
   `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` this week, not a derived key
   (`--target strategy-two --config <config2> --delegated
   62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`), **one seed per invocation,
   in derived seed order, one
   transaction journal path each, and one shared seed-expectation journal**.
   Legacy policies 62–65 are expected to coexist during this install; their
   presence is not an installer failure. The installer reads the seed journal,
   compiles the next derived policy, and refuses to install seed N+1 until seed
   N is finalized and verified (`strategy-two seed … is not finalized and
   verified; refusing to install seed … out of order`). Immediately before each
   send it reads the finalized counter again and aborts before broadcast if it
   differs from the counter recorded in that operation's pending journal. After
   each send, re-run the same command **without** `--execute` with the shared
   seed journal and inspect the finalized rows: the installed row must be
   `pass:true`; after the fourth seed the verdict is `PASS_ALREADY_FINALIZED`.
   The daily USDC limit rides in every policy's create bytes (see the caps
   section).
4. **STOP the old strategy-one worker and verify zero running instances** on
   Render (`docs/render-worker-images.md` for the service IDs): suspend/scale the
   `backyard-rwa` bridge service to zero, confirm no in-flight deploy restarts it,
   and record the stopped state. From here until step 7 the bridge is
   intentionally dark — this is the planned bridge outage.
5. **Retire v2 policies 62–65**: `--preflight` first, then the signed
   `--execute` path, which refuses to run until
   `verifyInstalledCustomPolicies` passes for the strategy-two target at the
   derived seeds 145–148.
6. **Verify 62–65 are absent at finalized commitment**: re-run the retirement
   tool's preflight and expect every legacy row `present:false`
   (`PASS_ALREADY_RETIRED`), then the installer's keyless
   `--assert-legacy-retired` readback (command in step 7) expecting
   `{"verdict":"PASS_LEGACY_RETIRED","broadcast":false}`.
7. **Only now deploy and start the strategy-two worker image** (worker constants
   below, GHCR `sha-<commit>` tag). Before the service is allowed to start, run
   the policy-generation preflight; it is keyless and exits nonzero while any of
   seeds 62–65 still exists on chain:

   ```sh
   cd tools/backyard-voltr
   SOLANA_RPC_URL="$SOLANA_RPC_URL" bun run src/activation/rwa-multiply-custom-policies.ts \
     --target strategy-two --config <config2-addr> --delegated <executor2-addr> \
     --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
     --assert-legacy-retired
   ```

   Expect `{"verdict":"PASS_LEGACY_RETIRED","broadcast":false,…}`. This is
   a standalone worker-start readback. The installer intentionally does not use
   this absence gate: legacy policies coexist during step 3 and are retired only
   after the four replacement rows are finalized and verified.
   The worker enforces the same gate independently at startup: the
   `backyard-rwa-worker` binary derives the four seed 62–65 policy PDAs from
   `bridgeSettings`/`bridgeSquadsProgram` and exits non-zero with
   `legacy custom policies still installed at seeds 62-65; run the PolicyRemove
   step before starting this worker: [<addresses>]` while any of them exists at
   finalized commitment (any read failure also refuses startup). The gate
   compiles clean (`go build ./...` in `go/backyard-rwa-worker`); its derivation
   is pinned to the recorded 62–65 addresses in
   `legacy_policy_gate_test.go`.

## Step 4 — stop the old worker and prove zero instances (Render)

Dashboard (operator-only action — suspend is never scripted or run by an agent):
`https://dashboard.render.com` → workspace → service
`loyal-backyard-rwa-worker` (`srv-dabkt0ojo6nc7381o9fg`, background worker) →
**Suspend**.

Read-only verification with the Render CLI (v2.22.0, authenticated
`render whoami`) immediately after the suspend:

```sh
render services -o json | python3 -c 'import json,sys; print([{"id": s["service"]["id"], "name": s["service"]["name"], "suspended": s["service"]["suspended"]} for s in json.load(sys.stdin) if s["service"]["id"] == "srv-dabkt0ojo6nc7381o9fg"])'
render services instances srv-dabkt0ojo6nc7381o9fg -o json
```

Pass criteria: the service record reads `"suspended": "suspended"` (pre-suspend
it reads `"not_suspended"`) and the instances list is empty (`[]` — today it
holds one instance, `srv-dabkt0ojo6nc7381o9fg-vgnh8`). Save both raw outputs to
`docs/evidence/backyard-rwa-strategy2/phase0-render-suspend.json` with the
fields:

```json
{
  "serviceId": "srv-dabkt0ojo6nc7381o9fg",
  "serviceName": "loyal-backyard-rwa-worker",
  "suspended": "<service.suspended value>",
  "instanceCount": 0,
  "instances": [],
  "cliCommands": ["render services -o json", "render services instances srv-dabkt0ojo6nc7381o9fg -o json"],
  "capturedAt": "<UTC ISO-8601>",
  "operator": "<who suspended and verified>"
}
```

The bridge stays dark from this step until step 7. Do not restart, resume, or
deploy the service for any reason short of the documented rollback.

## Worker configuration generated from the seed journal (step 7)

Generate the strategy-two worker binding file from the shared seed journal
after the four policy readbacks. The generator derives policy addresses and
strategy PDAs from the journal and the two operator-supplied public identities;
the worker handoff therefore has no source-code seed literals:

```sh
cd tools/backyard-voltr
bun run generate:rwa-multiply-strategy2-worker-config -- \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --config <config2-addr> --delegated-signer <executor2-addr> \
  --output ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-worker-config.json
```

The generated readback must report `policySeeds` equal to the journal's
`expectedSeeds` (`145–148` for the basic-set boundary at counter `144`), and the file is
the reviewed input for the strategy-two worker manifest/config update. The
worker's independent startup gate remains hard-coded to the legacy retirement
set 62–65 and must stay unchanged.

## Worker constants to change (step 7)

- `go/backyard-rwa-worker/internal/backyardrwa/build.go:96-133`: use the
  generated worker config for `bridgeStrategy` → config2,
  `bridgeStrategyAuth` → vault_strategy_auth2, `bridgeStrategyReceipt` →
  receipt2, `bridgeStrategyATA` → custody2, `bridgeDelegate` → the new
  delegated executor, and `bridgePolicy*` → the four derived policy addresses.
- `go/backyard-rwa-worker/internal/backyardrwa/report_ticket.go`: `reportTicketPDA`
  → report_ticket2 (+ bump re-derivation), keep `sameKey(Data[16:48], bridgeStrategy)`.
- `go/backyard-rwa-worker/internal/backyardrwa/manifest/backyard-rwa-v1.json`:
  `identities.v2StrategyConfig`, `reportTicket`, `reportTicketBump`,
  `delegatedExecutor`, and `runtimeBindings.bridgePolicies` (account + `dataSha256`
  per policy — the authoritative hashes from step 3's install readback).
- `go/backyard-rwa-worker/internal/backyardrwa/execution_observe.go`: pinned config
  (length 472, version byte @8) and the 12-key bindings array must match config2's
  layout; `bridgeMaxNAV` @400 becomes 1e12, age 32 stays @408.
- Build/push via the `worker-images` GHCR workflow (`sha-<commit>` tags); update the
  Render services to the new immutable tag. Do not switch to `runtime: docker`.

## Commands (rehearsal, no keys)

```sh
cd tools/backyard-voltr
export CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target
# policy-removal rehearsal (unsigned, sigVerify:false)
bun run src/activation/rwa-multiply-retire-legacy-policies.ts --preflight \
  --evidence ../../docs/evidence/hxtk-strategy2-2026-09-08/policy-remove-62-65.simulated.json
# bootstrap rehearsal (unsigned; wire A simulated, B/C packet-fit + LiteSVM-proofed)
bun run src/activation/rwa-multiply-strategy2-bootstrap.ts \
  --evidence ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-bootstrap.simulated.json
```

Operator send variants (only after the rehearsal evidence passes; every command
loads secrets through 1Password and none of them print values; every send is
simulated before broadcast and journaled, and each journal path is used exactly
once):

```sh
cd tools/backyard-voltr
# Step 2 — bootstrap wires (run in order; --reconcile replays a sent wire's gates)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-strategy2-bootstrap.ts --phase A --execute \
  --config <config2-addr> --delegated-signer <executor2-addr> \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/bootstrap-wire-a.journal.json'
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-strategy2-bootstrap.ts --phase B --execute \
  --config <config2-addr> --delegated-signer <executor2-addr> \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/bootstrap-wire-b.journal.json'
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-strategy2-bootstrap.ts --phase C --execute \
  --config <config2-addr> --delegated-signer <executor2-addr> \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/bootstrap-wire-c.journal.json'
# Step 3 — install the next four seeds (expected 145–148 after the basic set
# at 141–144) under the delegated executor, reused from v2 this week by owner
# decision: one seed per invocation, in
# this order, one transaction journal path each, and one shared seed journal.
# The installer re-reads finalized Settings immediately before each send and
# aborts if its counter differs from the pending journal expectation.
# 3a. allocation (seed 145)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> --execute \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-install-145-allocation.journal.json'
# 3a readback (no --execute; inspect finalized rows and seed expectation)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json'
# 3b. nav refresh (seed 146)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> --execute \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-install-146-nav-refresh.journal.json'
# 3b readback (no --execute; inspect finalized rows and seed expectation)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json'
# 3c. stage withdrawal (seed 147 — the spending-limited outflow lane)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> --execute \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-install-147-stage-withdrawal.journal.json'
# 3c readback (no --execute; inspect finalized rows and seed expectation)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json'
# 3d. withdraw (seed 148)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> --execute \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-install-148-withdraw.journal.json'
# 3d readback (no --execute; expect PASS_ALREADY_FINALIZED with all four rows pass:true)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json'
# Step 5 — retire v2 policies 62-65 (preflight first, then --execute)
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-retire-legacy-policies.ts --preflight \
  --evidence ../../docs/evidence/hxtk-strategy2-2026-09-08/policy-remove-62-65.simulated.json \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --config <config2-addr> --delegated-signer <executor2-addr>'
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 \
  bun run src/activation/rwa-multiply-retire-legacy-policies.ts --execute --config <config2-addr> \
  --delegated-signer <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/policy-remove-62-65.journal.json'
# Step 6 — finalized 62-65-absent readback (keyless; expect PASS_LEGACY_RETIRED,
# broadcast:false). The worker start gate below fails closed until this passes.
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-custom-policies.ts --target strategy-two \
  --config <config2-addr> --delegated <executor2-addr> \
  --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json \
  --assert-legacy-retired'
```

Reconcile form (used when a send is interrupted after broadcast; verifies the
pending signature finalized and re-derives the wire's poststate gates before
any fresh account-absence precondition):

```sh
op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" \
  bun run src/activation/rwa-multiply-strategy2-bootstrap.ts --phase A --reconcile \
  --config <config2-addr> --delegated-signer <executor2-addr> \
  --journal ../../docs/evidence/hxtk-strategy2-2026-09-08/bootstrap-wire-a.journal.json'
```

## Evidence index (`docs/evidence/hxtk-strategy2-2026-09-08/`)

- `policy-remove-62-65.simulated.json` — keyless PolicyRemove rehearsal,
  `sent:false`, `signed:false`, `UNSIGNED_SIMULATION_PASS`.
- `strategy-two-bootstrap.simulated.json` — wire A simulation, wires B/C packet
  fit, throwaway-identity policy preview (`authoritative: false`).
- `strategy-two-worker-config.json` — operator-generated strategy-two worker
  bindings, derived from the shared seed journal after finalized policy
  readback (not generated during unsigned rehearsal).
- For the retired 62–65 hashes:
  `docs/evidence/backyard-rwa-go/policy-helius-bridge-lifecycle-v2.json` and
  `docs/evidence/backyard-rwa-go/policy-signed-unsent-v1.json`.

## Open questions

1. **Wire A on mainnet vs LiteSVM**: the live-mainnet simulation passes for
   `initialize_config` and the composite six-instruction wire surfaced
   `InvalidAccount(0x2)` for `initialize_report_ticket` — explained by
   transaction-wide privileges (config must be writable in wire A and readonly in
   wire B), matching the LiteSVM proof's split-transaction sequence. The execute
   path above sends three separate wires exactly as listed.
2. **Spending-limit behavioural proof**: the LiteSVM proof now exists at
   `crates/squads-test-harness/tests/program_interaction_daily_limit.rs`. It
   exercises the production daily-spending-limit seam and proves that excess
   packed outflow is rejected with Squads
   `ProgramInteractionInsufficientTokenAllowance` (`6073`). Amendment v1.4 of
   `docs/plans/backyard-rwa-strategy2-activation-verifier.md` records this proof
   and the production binding; this is no longer a follow-up.
3. Locked-profit degradation is re-enabled ≥24 h after the repair report; this
   runbook does not schedule it.
4. Fees stay 0 at launch; `initialize_config`/vault fees are unchanged by
   strategy two except the config-level NAV ceiling (1e12).
