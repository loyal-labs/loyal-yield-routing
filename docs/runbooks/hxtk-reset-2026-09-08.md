# HXtk reset runbook — 2026-09-08

Status: read-only rehearsal and operator handoff. This document records the
mainnet order; it is not authorization to execute it. No transaction was
signed or broadcast while preparing this runbook. Every command below that can
change mainnet state is guarded by `CONFIRM_MAINNET=1` and the appropriate
1Password-provided signer.

The script lives in the nested package. Start the runbook with:

```sh
cd tools/backyard-voltr
```

Run every command below from that directory. The guarded commands use the
package-local `.env.1password` path. Keep the Render worker suspended for the
whole reset. The CLI has no worker-suspend operation.

## Identities and current evidence

- Voltr: `vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a`
- vault HXtk: `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`
- admin and PolicyCreate signer: `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`
- Squads manager/vault: `ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`
- Squads settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- delegated ExecuteSync signer: `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`
- report ticket: `C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5`
- adaptor: `FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW`
- pending request receipt: `8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e`

The widened finalized read was four `getMultipleAccounts` batches of 64
policy PDAs covering seeds `0..255`. It found 111 occupied seeds. Occupied
ranges are:

```text
17-24, 29, 32-50, 57-139
```

The old `REPORT_NAV` policy is seed 63 and is not used for this repair. The
finalized Settings readback reports policy seed `139`, so Squads
`PolicyCreate` assigns the next sequential seed, **140**. The policy PDA
derived for that exact seed is
`7vqKymJ4RcP9TUR9jT6G2ruuRp3j6rVhTzoYJWYTe2dR`.

### Compiler build hygiene

Policy compilation uses the per-checkout target
`target/backyard-voltr-compilers`; it never uses the shared
`.phase3-recovery/target`. The shared directory remains reserved for
`bun run build:adaptor` (whose script verifies the artifact hash) and Rust
tests.

The deployed program does not accept an arbitrary policy seed from the action:
it derives `Settings.policySeed + 1`. The tool therefore records
`expectedSeed=140`, compiles and derives exactly that seed, re-reads the
finalized counter immediately before an operator send, and aborts if the
counter moved. It never falls back silently to another seed. Seeds are not
reused after `PolicyRemove`.

The unsigned PolicyCreate simulation for seed 140 is
`SIMULATION_PASS_UNSENT`: packet `1,151` bytes and `52,425` compute units in the
regenerated evidence below, under the 1,232-byte packet limit and with no
Squads `6024` error. The 0–255
occupancy scan remains context and is recorded in the evidence; it does not
override the Settings sequential-seed rule. Strategy-two policies should
therefore be the next four seeds after this repair policy, expected **141–144**.

Stable constraint evidence is in
[`repair-policy.simulated.json`](../evidence/hxtk-reset-2026-09-08/repair-policy.simulated.json).
Each constraint is represented as `offset`, `operator`, `kind`, and `value`
for data constraints, with the program's `pinnedPubkeys` recorded alongside
it. This is the JSON shape to diff against the LiteSVM seed-100 stand-in.

## Preconditions and abort gates

1. In the Render dashboard, suspend worker `srv-dabkt0ojo6nc7381o9fg`.
   There is no CLI suspend command. Confirm the worker is no longer able to
   crank before touching the vault.
2. Re-read all figures at finalized commitment immediately before step 1:
   `tv=2,793,298`, idle USDC `=3,793,417`, strategy-one receipt position
   `=2,793,417`, custody `=0`, LP supply `=99,941,522`, accumulated admin-fee
   LP `=89,601,150`, degradation `=86,400`, and waiting period `=600`.
   Recompute `idle + receipt1 - tv = 3,793,536`. Confirm the request receipt
   still belongs to admin and its escrow still holds `99,941,522` LP.
3. Abort on any figure, owner, manager, ticket, PDA, mint, or signer drift.
   Never crank the orphaned old strategy-one receipt below `3,793,536`.
   Never use live policy 63 for this repair.

The read-only preflight is:

```sh
bun run reset:hxtk verify --simulate
```

The repair command has a stricter frozen-state gate than the general preflight.
It reads the vault, idle ATA, LP mint, strategy receipt, custody ATA, report
ticket, pending request receipt, and request escrow in one finalized
`getMultipleAccounts` snapshot and records that RPC context slot as
`observationSlot`. It requires exactly `tv=2,793,298`, `idle=3,793,417`,
`receipt1=2,793,417`, `custody=0`, `lpSupply=99,941,522`, degradation `0`,
admin performance fee `0`, waiting period `600`, the admin-owned request
receipt/escrow, and report-ticket `last_consumed=444,157,930`. A later read
must not be relabeled as this observation slot. After repair the gate requires
`tv=idle=3,793,417`, `receipt1=3,793,536`, custody `0`, unchanged LP supply,
and unchanged degradation. A finalized, policy-linked repair journal together
with on-chain `tv == idle` is canonical consumed state; reruns stop with
`REPAIR_ALREADY_APPLIED` (the on-chain equality is also never bypassed by a
newer ticket slot).

## Mainnet order

### 1. Disable degradation and launch fees

Simulate first:

```sh
bun run reset:hxtk config --simulate
```

Expected simulation: degradation `86,400 -> 0`, admin performance fee `500 ->
0`, books unchanged, packet `354` bytes. The reason for doing this first is
that the repair jump must not be smeared as locked profit, and the frozen
withdraw request must be re-quoted after the repaired NAV. Fees remain zero at
launch.

The guarded journaled operator command is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk config --execute --journal /absolute/path/hxtk-config.json
```

The shared flow writes `/absolute/path/hxtk-config.json.pending` with the
signed wire, wire/message hashes, message base64, and expected pre/post state
before its single send. It then finalizes and renames the pending wire; use the
read-only reconcile form only for an ambiguous send:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk config --reconcile --journal /absolute/path/hxtk-config.json
```

After finalization, re-read the vault and require degradation `0`, admin fee
`0`, unchanged tv/idle/receipt/custody/LP, and the two expected config events.

### 2. Compile and create the one-shot repair policy

Re-run the widened scan and compiler simulation:

```sh
bun run reset:hxtk repair-policy --expect-seed 140 --simulate
```

Use the Settings' next seed (expected `140`; abort if different). The Settings
readback must report `policySeed=139` and `expectedSeed=140`. The policy PDA must be
`7vqKymJ4RcP9TUR9jT6G2ruuRp3j6rVhTzoYJWYTe2dR`. The policy must be the fresh
nav-refresh artifact from the shared custom policy compiler, with exactly
arm-report and deposit-strategy constraints, no spending limit, unconstrained
report digest, and exact `Equals U64Le(3,793,536)` NAV constraints at arm-report
offset `39` and capital-report offset `51`. An absent NAV constraint is a
failure. Require packet `<=1,232` bytes and the admin as both fee payer and
Settings signer. A changed
Settings counter is an abort; do not choose a different seed.

The guarded journaled command is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk repair-policy --expect-seed 140 --execute --journal /absolute/path/hxtk-repair-policy.json
```

`--execute` performs the finalized transaction lookup and reconciliation itself.
Use the standalone `--reconcile` form only if the raw send was attempted but
the execute process returned with an ambiguous `.pending` journal; it is not a
second normal step:

```sh
bun run reset:hxtk repair-policy --expect-seed 140 --reconcile --journal /absolute/path/hxtk-repair-policy.json
```

Require a finalized policy readback with seed `140`, the expected PDA, settings,
delegated executor, constraints, no hooks, and no spending limit. The
operator path re-reads Settings immediately before the send and aborts if its
counter changed from 139. After creation, it must verify the finalized
PolicyCreate signature and message hash, then decode the live policy account
and compare its program IDs, pinned pubkeys, both exact NAV constraints,
threshold, timelock, delegated signer, and vault index with the compiled
artifact. The live policy bytes are compared to the hash recorded in the
finalized PolicyCreate journal as a dynamic continuity pin, alongside decoded
semantic checks. This is not a static raw-account hash pin.
Abort on an occupied PDA, unexpected owner, semantic mismatch, or packet
growth. The following strategy-two policies must use the next four
sequential seeds, expected `141–144`; they must not be selected or created
early as a workaround.

### 3. Finalized policy readback and target binding

The PolicyCreate reconcile is the finalized policy readback. Run the repair
simulation only after that reconcile has succeeded, binding it to the
finalized creation journal:

```sh
bun run reset:hxtk repair --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --simulate
```

Until the exact policy exists and is finalized, the required output is
`PENDING_REPAIR_POLICY`, naming `expectedSeed=140` and its PDA. Do not
substitute policy 63, use a different seed, or infer success from a compiled
artifact.

### 4. Execute the repair through the fresh policy

The repair is one Squads `ExecuteSync` instruction whose inner wire is the
proof's arm-report plus capital-report/deposit-strategy sequence. The observed
report slot is the ticket sequence; NAV is `3,793,536`; the report digest is
the fixed per-run digest emitted by the compiler. The transaction fee payer
and delegated signer are `62JL...` (`POLICY_KEYPAIR`), not the admin.

The adaptor rejects a report unless `report.sequence == report.observed_slot`,
`observed_slot <= current_slot`, and `current_slot - observed_slot <= 32`.
The tool keeps an 8-slot safety margin, so it requires
`current confirmed slot - observed slot + 8 <= 32`. It takes the report
sequence from a fresh confirmed `getSlot` immediately before building the arm
and capital instructions, after the consistent NAV snapshot, policy readbacks,
and compilation work. The NAV inputs remain those snapshot values; in
particular the phantom receipt value remains `3,793,536`.

The same age check runs immediately before simulation and as the final
pre-send gate, before the attempted mark. The pending send-age status is
rewritten and fsynced before that final check; on a healthy RPC, expect the
raw send to follow within the margin, typically **1–4 slots** after the last
check. A stale check aborts with `REPORT_SLOT_STALE_PRE_SEND` before the
attempted mark and sole raw send. The transaction is prepared and simulated
at `commitment: "confirmed"` with `minimumContextSlot` set to the snapshot
context slot and the prepared blockhash; the prepared simulation does not use
`replaceRecentBlockhash`. A finalized simulation can lag the confirmed report
by about 31 slots and therefore cannot satisfy this 32-slot age rule. The
repair leg settles with `sendPreparedConfirmedOnce`, then polls the typed
finalized signature read every **2 seconds**, for at most **30 polls**. Three
consecutive read errors leave the leg attempted for read-only reconcile; two
consecutive absent reads prove expiry only after finalized block height is
greater than `lastValidBlockHeight + 64`. A proven expiry is recorded as
`aborted-pre-send` with `abortReason: attempted-expired-proven`, the same
attempt token, both heights, and both absent reads; it is safely re-runnable
only with a **new** journal, while the original journal remains history. On
finalized, the existing finalized journal message check, reconciliation,
finalized journal, and auto policy removal continue unchanged. Every snapshot
fingerprint, report-age, policy, simulation, attempted-mark, and finalized
reconciliation gate remains in force. When the RPC returns a simulation
context slot, that slot is used for `reportSlotAgeAtSimulate`. The JSON output
and journal record `reportSlotAgeAtSimulate`, `reportSlotAgeAtSend`, the
observed/current slots, the 8-slot margin, and the pinned 32-slot maximum.

Simulate the confirmed repair transaction immediately before any operator action:

```sh
bun run reset:hxtk repair --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --simulate
```

Require all checks to pass:

```text
tv              3,793,417
idle            3,793,417
receipt1        3,793,536
custody         0
LP supply       99,941,522 (unchanged)
degradation     0 (unchanged from step 1)
ticket          consumed at the observed sequence and idle afterward
```

The guarded journaled operator path is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk repair --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --execute --journal /absolute/path/hxtk-repair.json
```

`--execute` settles the repair at confirmed, performs the existing finalized
repair reconciliation, and then immediately runs
`repair-policy-remove` with its own derived journal,
`/absolute/path/hxtk-repair.policy-remove.json`, in the same invocation. The
normal path therefore has no separate removal command; if removal fails, the
repair process exits non-zero with `REPAIR_FINALIZED_POLICY_STILL_PRESENT` and
all later legs remain blocked until the standalone recovery command in step 5
successfully closes seed 140.

Abort on any failed simulation, report-ticket sequence/hash mismatch, policy
drift, LP change, custody change, or post-state other than the exact values
above. A stale confirmed slot aborts with `REPORT_SLOT_STALE_PRE_SEND` before
the attempted mark and before the sole raw send. The elected
`aborted-pre-send` artifact is safely re-runnable with the same journal (or a
new journal) after a plain rerun of the same repair leg; no on-chain mark or
send occurred. The current evidence is pending because the policy is absent;
it does not contain a fabricated post-state.

### 5. Recover one-shot policy removal

The normal repair `--execute` path retires the policy immediately. Keep this
standalone command for recovery when an ambiguous send or a failed automatic
removal leaves a finalized repair journal but the seed-140 policy still
exists. Simulate removal against the same finalized creation journal:

```sh
bun run reset:hxtk repair-policy-remove --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --repair-journal /absolute/path/hxtk-repair.json --simulate
```

Require the exact seed-140 one-shot policy to close, Settings bytes to remain
unchanged, and the admin account to remain present. `PolicyRemove` does not
reuse or decrement the Settings counter: seed 140 is retired permanently, and
the next policy creation remains sequential at the then-current counter. The
command has a simulate-before-send, pending-wire journal, and finalized
reconcile path:

```sh
bun run reset:hxtk repair-policy-remove --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --repair-journal /absolute/path/hxtk-repair.json --reconcile --journal /absolute/path/hxtk-repair.policy-remove.json
```

Use `--reconcile` whenever automatic removal reached its attempted or
finalized mark. Use `--execute` with the same complete flags only when the
automatic removal never started and no removal pending journal exists. If the
repair send itself is ambiguous, reconcile it first with:

```sh
bun run reset:hxtk repair --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --reconcile --journal /absolute/path/hxtk-repair.json
```

If the canonical `repair-policy-remove` state is already `finalized`, no
removal command is needed; the recovery mapping emits:

```text
repair-policy-remove already finalized; verify with bun run reset:hxtk verify
```

Reconcile must read the seed-140 PDA as closed (absent or zero-lamport
system-owned empty account), with Settings unchanged. Never remove an
unrelated policy or reuse seed 140. If either finalized provenance journal is
missing, the command returns `PENDING_FINALIZED_POLICY_AND_REPAIR`; after both
journals exist, an absent policy returns `PENDING_REPAIR_POLICY` naming
`expectedSeed=140` and the derived PDA.

### 6. Harvest accumulated admin fee LP

Once policy removal is finalized, simulate:

```sh
bun run reset:hxtk harvest --simulate
```

Create manager/admin/treasury LP ATAs idempotently as part of the simulated
wire. Require the admin LP balance delta `+89,601,150` (not an absolute
balance), manager/treasury `0`, fee accumulators zero, LP supply increased only
by that fee, and books unchanged. The guarded existing command is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk harvest --execute --journal /absolute/path/hxtk-harvest.json
```

Re-read finalized balances and the vault before continuing.

### 7. Cancel the frozen request

The live request is receipt
`8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e`; its escrow should hold the
entire `99,941,522` LP. Simulate and verify that exact receipt/escrow identity:

```sh
bun run reset:hxtk cancel --simulate
```

The simulation is authoritative and must be re-read at this step. The cancel
journal is pinned to the original frozen receipt PDA above; a replacement
receipt is never an acceptable cancel target. Require
escrow drain, a 1:1 refund of the actual escrow amount as the admin LP balance
delta, zero LP burn, and a cleared receipt. Abort if the receipt
owner/authority or escrow amount differs. The guarded command is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk cancel --execute --journal /absolute/path/hxtk-cancel.json
```

### 8. Re-request all LP at the repaired NAV

Cancel and request are separate mainnet transactions. First confirm that the
finalized cancel journal has reconciled the admin LP balance delta against the
escrow refund. The request simulation is request-only and binds to that
finalized cancel journal:

```sh
bun run reset:hxtk request --cancel-journal /absolute/path/hxtk-cancel.json --simulate
```

Require all LP in escrow, admin LP ATA empty, and supply unchanged. The
simulation-only projection checks `withdrawableFromTs >= local now + 600`
seconds; finalized reconciliation instead requires
`withdrawableFromTs == request transaction blockTime + 600`, with at most a
one-second Voltr rounding tolerance. Then use:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk request --cancel-journal /absolute/path/hxtk-cancel.json --execute --journal /absolute/path/hxtk-request.json
```

Re-read the finalized receipt and escrow. Never combine the cancel and request
transactions on mainnet.

### 9. Wait for the withdrawal period

Wait at least `600` seconds from the finalized request transaction. Re-read the
receipt at finalized commitment and continue only when
`withdrawableFromTs <= latest finalized slot blockTime`. Abort on a missing
receipt, authority mismatch, LP amount drift, or a future deadline.

### 10. Claim

Simulate the claim against the finalized request journal and bind the claim to
that journal's new request receipt:

```sh
bun run reset:hxtk claim --request-journal /absolute/path/hxtk-request.json --simulate
```

The pre-send simulation records `expectedPayoutRaw` and `expectedLpBurnRaw` in
the pending journal. Finalized reconciliation requires payout `>= 1`, payout
exactly equal to `expectedPayoutRaw`, and LP burned exactly equal to both
`expectedLpBurnRaw` and the request LP amount; any deviation is
`RECONCILE_MISMATCH`. It also requires payout to remain no greater than the
receipt quote, idle to decrease by exactly the payout, and escrow to drain.
The guarded command is:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk claim --request-journal /absolute/path/hxtk-request.json --execute --journal /absolute/path/hxtk-claim.json
```

Reconcile the finalized receipt closure, user USDC payout, idle, tv, LP supply,
and ticket state before calling the reset complete.

### 11. Restore locked-profit degradation

Only after the finalized claim and no earlier than 24 hours after the finalized
repair transaction, run the new restore leg. It reads the repair journal's
finalized transaction `blockTime` and the latest finalized slot `blockTime`,
hard-gating `latest finalized blockTime - repair blockTime >= 86,400`; the
finalized restore transaction is checked against the same elapsed-time rule.
It also requires the finalized claim journal and rechecks that the request
receipt is closed and its escrow is drained before building the wire.

Simulate from the package directory:

```sh
bun run reset:hxtk restore-degradation --repair-journal /absolute/path/hxtk-repair.json --claim-journal /absolute/path/hxtk-claim.json --simulate
```

The guarded execute and ambiguous-send recovery commands are:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk restore-degradation --repair-journal /absolute/path/hxtk-repair.json --claim-journal /absolute/path/hxtk-claim.json --execute --journal /absolute/path/hxtk-restore-degradation.json
```

If that send is ambiguous, reconcile the same journal without a new send:

```sh
bun run reset:hxtk restore-degradation --repair-journal /absolute/path/hxtk-repair.json --claim-journal /absolute/path/hxtk-claim.json --reconcile --journal /absolute/path/hxtk-restore-degradation.json
```

Require degradation `86,400`, admin/manager performance fees `0`, and waiting
period `600` after finalized reconciliation. Record the NAV-sniper
haircut/degradation result (audit T10.7), and abort if the 24-hour delay,
finalized repair timestamp, post-claim book, or finalized journal wire cannot
be proved.

## Operational rules for `--execute` (2026-09-08 audit)

The final adversarial audit of this tool (fleet/integration `2fe25fa`) confirmed
that two clean concurrent invocations cannot both reach the raw send, that the
evidence decodes to the pinned 837-byte PolicyCreate, and that every canonical
state write is generation-elected. The recovery-path hardening in commit
`23d62a8` adds the same guarantees to crash recovery. The following rules stay
mandatory for every `--execute` and `--reconcile` invocation:

1. **One process per leg, one operator, one machine.** Never start a second
   `reset:hxtk` invocation for this vault while another is running or while the
   outcome of the previous one is unknown. Do not run any leg from two
   terminals, two checkouts, or two hosts.
2. **After a crash, timeout, or any ambiguous outcome during `--execute`, do
   not re-arm and do not start a fresh journal.** First run
   `bun run reset:hxtk verify --simulate` and read the on-chain state for the
   leg you were executing (receipt values, pending request receipt, the policy
   at seed 140, config values). Then run `--reconcile` with the SAME journal
   path. Continue only when the reconcile result agrees with the chain. An
   attempted record is never downgraded by an abort artifact; reconciliation
   can finalize it from the expected signature or re-arm it only after the
   recorded blockhash validity height is proven passed.
3. **Treat abort artifacts as attempt-bound evidence.** Their name is
   `<journal>.aborted-<unix-ms>-<attemptToken>.json`; the content repeats the
   16-byte attempt token, canonical generation, journal binding hash, pending
   binding hash, signed wire, and expected signature. Recovery applies one only
   when all bindings match the current `pending` record. Foreign or legacy
   artifacts are stale; a foreign artifact blocks reuse unless the current
   state is already `aborted-pre-send`.
4. **Never delete, rename, or edit files under the canonical state root**
   (`~/.loyal/hxtk-reset/<vault>/`). Claim-held readers roll a pointer forward
   from the highest contiguous `.gen-N`; read-only/simulate paths report
   `STATE_POINTER_BEHIND` with the generation numbers. Claim breaks use an
   elected breaking marker and inode-checked unlink, and `--break-claim` is only
   for a claim whose recorded pid and start time are proven dead on this host.
5. **Do not rely on the tool alone for the second-send guarantee.** Each leg
   also has on-chain guards (the one-shot policy at seed 140, the adaptor
   ticket-sequence check, the exact-payout claim fence, receipt state), but the
   simulate immediately before each send is what catches a repeat. Keep fees at
   zero and preserve the Option A operating rules until the external gates are
   deliberately changed.

The attempted-expiry re-read and RPC-error window is fixed after commit
`1dfd1c4`; no known gaps remain in this audit scope, and the rules stay
mandatory. After confirmed repair settlement, the tool polls the typed
finalized signature read every 2 seconds for at most 30 polls. Three
consecutive finalized-read errors leave the leg attempted for reconcile. A
null finalized status is absence; expiry requires two consecutive absent reads
and finalized block height strictly greater than
`lastValidBlockHeight + 64`. A proven expiry is re-armable only with a new
journal; the original journal and attempt token remain history.

## Recovery matrix

`attemptToken` is a random 16-byte lowercase hex value created with the
canonical `pending` record and carried into the `attempted` record. It is also
stored in every abort artifact, whose exact name is
`<journal>.aborted-<unix-ms>-<attemptToken>.json`. A canonical `attempted`
record is changed only by signature reconciliation or by the blockhash-expiry
reconciliation path after the two-read rule above. Expiry uses
`broadcast: "attempted"`; the canonical abort election happens before artifact
publication. Legacy `abortReason: "attempted-expired"` records remain bound to
their original journal. New proven expiry records use
`abortReason: "attempted-expired-proven"`, record the same attempt token,
both heights, and both absent reads, and may be re-armed only by executing a
new journal; the original journal remains history. If a process crashes after
the proven-expiry election but before the artifact or marker is complete, a
same-journal `--reconcile` completes that publication and returns 0, while a
same-journal `--execute` completes it and then refuses nonzero with the
new-journal re-arm instruction; it never sends or enters policy removal.

| Canonical state | Artifacts present | Fresh `--execute` | Fresh `--reconcile` | Fresh `--simulate` | Fresh `--break-claim` |
| --- | --- | --- | --- | --- | --- |
| `none` | none | Builds and elects a new `pending` generation; only that sender may mark `attempted`. | Refuses: there is no attempted canonical binding to reconcile. | Reads and reports the pre-reset gate; writes no state. | Repairs only a proven-dead claim; otherwise leaves the leg untouched. |
| `none` | stranded `.gen-N` | Claim-held read rolls the pointer to the highest contiguous generation, then applies the normal journal barrier. | Same roll-forward while the claim is held; no new attempted record. | Reports `STATE_POINTER_BEHIND` and generation numbers; writes no pointer. | Resumes an elected break marker if present, then applies the dead-claim test. |
| `pending` | `.pending` journal | Validates the token/binding; a pre-send failure becomes an attempt-bound `aborted-pre-send`; it never sends from recovery. | Applies only the matching pre-send abort and leaves the leg re-armable; never writes `attempted`. | Reports the pending binding and writes no transition. | Handles only the independent claim; it cannot alter the canonical leg. |
| `pending` | journal only, no `.pending` | Publishes an attempt-bound synthetic abort through the generation election; the leg may be re-run. | Same self-sufficient pre-send recovery; no send. | Reports the recoverable pending condition; writes no abort or pointer. | Handles only the independent claim. |
| `pending` | bound abort artifact | Refuses reuse of the same pending attempt; the next send must re-arm from `aborted-pre-send`. | Applies the artifact only when token, generation, journal binding, and pending binding all match. | Lists the artifact as matching evidence; writes nothing. | Handles only the independent claim. |
| `pending` | foreign abort artifact | Refuses with `JOURNAL_HAS_FOREIGN_ABORT_ARTIFACT`. | Ignores it for state transition and lists it in `staleAbortArtifacts`. | Lists it as stale; writes nothing. | Handles only the independent claim. |
| `attempted` | `.pending` journal | Refuses a fresh send and refuses artifact-driven downgrade. | Checks the expected signature; finalizes on chain proof, or elects expiry only after two typed `absent` reads and the 64-block margin. It never re-arms this attempted record. | Reports attempted and its validity height; writes nothing. | Handles only the independent claim. |
| `attempted` | `.pending`, no abort artifact, signature landed | Does not send; recovers the canonical wire and finalizes. | Re-reads the signature and finalizes; no artifact is created. | Reports attempted/landed; writes nothing. | Handles only the independent claim. |
| `attempted` | `.pending`, no abort artifact, signature expired | Does not send; canonical-first expiry recovery creates the bound abort artifact. | Elects `attempted-expired-proven`, records both absent reads and both heights, then publishes the artifact idempotently; no refusal. | Reports attempted and validity height; writes nothing. | Handles only the independent claim. |
| `attempted` | `.pending`, abort artifact present, signature landed | Does not send; chain proof finalizes and leaves the artifact stale evidence. | Re-reads the signature and finalizes; no abort downgrade. | Lists the artifact; writes nothing. | Handles only the independent claim. |
| `attempted` | `.pending`, abort artifact present, signature expired | Does not send; completes the canonical-first expiry sequence only after the two-read rule. | Completes the proven expiry abort with `rearmable:true`; RPC errors leave `attempted`. | Lists the bound artifact; writes nothing. | Handles only the independent claim. |
| `attempted` | no `.pending`, no abort artifact, signature landed | Does not send; reconstructs the final journal from canonical pending data and finalizes. | Re-reads the canonical expected signature and finalizes. | Reports missing journal barrier and attempted state; writes nothing. | Handles only the independent claim. |
| `attempted` | no `.pending`, no abort artifact, signature expired | Does not send; re-checks height and completes canonical-first expiry recovery. | Re-reads the canonical expected signature and height, then elects the abort and publishes its artifact. | Reports attempted and validity height; writes nothing. | Handles only the independent claim. |
| `attempted` | no `.pending`, abort artifact present, signature landed | Does not send; finalizes from canonical wire and chain proof. | Re-reads the canonical expected signature and finalizes; artifact remains stale evidence. | Lists the artifact; writes nothing. | Handles only the independent claim. |
| `attempted` | no `.pending`, abort artifact present, signature expired | Does not send; re-reads the signature and reports `rearmable:false` while unreadable. | Only a null result followed by the 64-block margin and a second successful null result completes the abort marker; RPC errors leave `attempted`. | Lists the artifact; writes nothing. | Handles only the independent claim. |
| `aborted-pre-send` (`attempted-expired`) | abort artifact present, signature landed | Refuses a new journal and prints the original-journal finalize instruction. | Elects `finalized` for the same attempt token and original journal; never re-arms. | Reports the landed signature; writes nothing. | Handles only the independent claim. |
| `aborted-pre-send` (`attempted-expired`) | abort artifact present, signature still unreadable | Emits `JOURNAL_MISMATCH_ATTEMPTED_EXPIRED` with `rearmable:false` and the original-journal reconcile instruction; writes and claims nothing. | A landed signature finalizes the original attempt; an RPC error makes no state transition. | Reports stale evidence; writes nothing. | Handles only the independent claim. |
| `aborted-pre-send` (`attempted-expired-proven`) | abort artifact present, original journal | Completes an interrupted artifact/marker publication idempotently, then refuses nonzero with the new-journal re-arm instruction; it never sends or enters policy removal. | Completes an interrupted artifact/marker publication idempotently and returns 0; otherwise reports the proven state. | Reports both heights and both absent reads; writes nothing. | Handles only the independent claim. |
| `aborted-pre-send` (`attempted-expired-proven`) | new journal | Re-arms with a new attempt token and preserves the original journal in history; never reuses the original journal. | Reports the proven expiry and new-journal instruction; sends nothing. | Reports re-armable state; writes nothing. | Handles only the independent claim. |
| `attempted` | final journal | Refuses filename-only promotion and a second send. | Requires the chain signature/message proof before `finalized`; otherwise keeps `attempted`. | Reports the final journal as non-authoritative without chain proof. | Handles only the independent claim. |
| `attempted` | foreign abort artifact | Refuses; an abort artifact can never move `attempted`. | Lists it as stale and reconciles the attempted record independently. | Lists it as stale; writes nothing. | Handles only the independent claim. |
| `aborted-pre-send` | abort artifact(s) for another pre-send reason (not `attempted-expired` or `attempted-expired-proven`) | May re-arm with a new attempt token when the journal/leg policy permits; stale artifacts remain visible. `REPORT_SLOT_STALE_PRE_SEND` is safe to rerun with the same journal or a new journal because it is elected before the attempted mark and sends nothing; rerun after a plain confirmed-slot refresh. | Does not send; reports the abort and its bindings. | Reports re-armable state and stale artifacts; writes nothing. | Handles only the independent claim. |
| `finalized` | final journal | Emits `already finalized`; no new send. | Emits `already finalized` with the verification command. | Emits the same recovery command; writes nothing. | Handles only the independent claim. |
| any state | dead claim | Takes over only through the token/inode election and dead-process proof. | Same claim takeover rules; it does not change a leg state by itself. | Reports the claim/deadness decision; writes no takeover. | Completes a resumable breaking marker and unlinks only on inode match. |
| any state | breaking marker or renamed token | Resumes the marker before proceeding; a live replacement claim is never removed. | Resumes the marker without creating an attempted record. | Reports the marker and identity mismatch if present; writes nothing. | Completes the marker idempotently, then removes only the recorded dead inode. |

## Canonical state root and replay recovery

The canonical fence is machine- and user-local, not checkout-local. The execute
and reconcile paths always resolve it from `os.userInfo().homedir`; environment
variables cannot select another namespace. By default it is:

```text
/Users/<operator>/.loyal/hxtk-reset/HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA/
```

The home directory must be a real directory and not group/world-writable. Each
component `.loyal`, `hxtk-reset`, and the vault directory is walked with
`lstat`, must not be a symlink, must be owned by the current uid, and must have
no group/other permission bits. Missing components are created with mode
`0700`; an insecure or foreign-owned component names itself in the error.
`HXTK_RESET_STATE_ROOT` and `HOME` are ignored by execute/reconcile and are not
recovery controls.

Run every leg as the same user on the same machine. The canonical state file,
not the requested journal directory, is the replay authority. Each state file
records both `checkoutRoot` and `stateRoot`, binds the signed message/wire and
pending metadata, and records the exact SHA-256 of the finalized journal.

The normal path never uses `--allow-repeat`. If a repeatable state-idempotent
leg (`config`, `harvest`, or `restore-degradation`) is intentionally retried
after finalization, use a new journal path that does not exist and pass
`--allow-repeat`; the old journal and signature are retained in the canonical
state history. A one-shot leg, or any leg still `pending` or `attempted`, may
not use this recovery path.

If the pre-send snapshot gate fails, the tool aborts before the attempted mark
and before the sole raw-send call. It moves the self-sufficient signed wire to
`<journal>.aborted-<unix-ms>-<attemptToken>.json`, records
`status: "aborted-pre-send"`, and leaves no `.pending` file to reconcile.
Recheck finalized state, then rerun the same leg with the same or a new journal
and no `--allow-repeat`:

```sh
op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk repair --expect-seed 140 --policy-journal /absolute/path/hxtk-repair-policy.json --execute --journal /absolute/path/hxtk-repair.json
```

`--reconcile` never sends from an aborted journal. Any exception after the
attempted mark keeps the leg `attempted`; use the same journal's reconcile path
after checking whether the expected signature finalized. A pre-send crash with
canonical `pending` but no published journal produces the same bound
`aborted-pre-send` recovery record, because the raw send is unreachable before
the attempted mark. An attempted-expiry abort instead records
`abortReason: "attempted-expired-proven"`; its canonical state is elected
first, then the `.pending` rename/artifact and final marker are idempotently
completed. Re-arm only with a new journal after the proof is complete.

## Idempotency and journal policy

| Leg | Operational property |
| --- | --- |
| `config` | State-idempotent; a repeat requires a new journal and explicit `--allow-repeat`. |
| `repair-policy` | One-shot; never replay a finalized PolicyCreate or reuse seed 140. |
| `repair` | One-shot; a finalized policy-linked repair plus `tv == idle` is consumed state. |
| `repair-policy-remove` | One-shot; PolicyRemove closes the policy and the seed is never reused. |
| `harvest` | Not durably idempotent if fees accrue between attempts; a repeat requires explicit `--allow-repeat` and a new journal. |
| `cancel` | One-shot for the frozen request receipt. |
| `request` | One-shot for the post-cancel request receipt. |
| `claim` | One-shot for the post-request receipt. |
| `restore-degradation` | State-idempotent config restore, but only after the finalized repair/claim gates and 24-hour delay; a repeat requires explicit `--allow-repeat`. |

`SOLANA_TESTING_PK` is used only by the admin/config/policy/remove/harvest and
user-authority legs; `POLICY_KEYPAIR` is used by the delegated repair
ExecuteSync leg. Resolve those through the mounted 1Password environment and
never print key material. Every `--execute` call requires
`CONFIRM_MAINNET=1`, a journal path, simulation immediately
before submission, and finalized reconciliation in the same process. Before
any raw send, the tool consults the canonical vault-scoped state root described
above, regardless of the requested journal directory. The state records
`pending`, `attempted`, `aborted-pre-send`, or `finalized`; one-shot legs cannot
be replayed, and `config`, `harvest`, and `restore-degradation` require an
explicit `--allow-repeat` plus a new journal only when intentionally repeated.
Before the read/check/write fence, every send or reconcile path creates a
token-named `<leg>.claim.<token>` with `pid`, `startedAtUnixMs`,
`processStartTime`, `hostname`, `journal`, and a 16-byte hex `token`, then
publishes the fixed `<leg>.claim` pointer as a hard link. Claims are advisory;
the generation election below is the send gate. The claim stays held through
build, simulation, send, finalization, reconciliation, and every canonical
state write. Release compares the pointer inode with this process's token-file
inode before unlinking either name. A live claim blocks the leg with the
owning pid; a reused pid is dead when its recorded process start time differs.
A crash leaves the claim in place; `--break-claim` is allowed only for a claim
on the local hostname whose pid and process start time are proven dead. It
renames only that exact dead token file, then unlinks the fixed pointer only if
its inode is unchanged. A competing breaker or a replacement live claim is
never removed. The break lease remains the serialization point for breakers.
Each canonical state write writes a `0600` same-directory temporary, fsyncs it,
and atomically elects `.gen-N` with a hard link before renaming the reader
pointer. Readers require the pointer bytes to equal the elected generation
inode; a mismatch is `STATE_GENERATION_CONFLICT` and prevents a send. The raw
send is reachable only after this process wins the `attempted` generation; no
claim logic is relied upon for that guarantee.
The pre-send journal is marked `broadcast:"attempted"` with the expected
signature before raw submission, so a crash in the ambiguous-send window
remains fenced. Post-pre-mark status is volatile and nested as:
`sendStatus: { verdict, sendError, submission, attemptedAtUnixMs, signature }`.
The repair leg also re-runs its single finalized multi-account snapshot
immediately before send.
This rehearsal set
`CONFIRM_MAINNET` nowhere and sent nothing.

If an operation fails after a broadcast, stop, preserve the journal, and use
only that command's `--reconcile` path after finalized status is visible. Never
replay a journal, remove an unrelated policy, overwrite an occupied seed, or
resume the worker until the corresponding finalized readback and all abort
gates pass. Reconciliation is ambiguous-send recovery only: a successful
`--execute` already performs finalized reconciliation and renames
`<journal>.pending` to `<journal>.sent-wire`; do not run `--reconcile` as a
second normal execution.
