# Workers v2 go-live contract

This is the shared contract for the three go-live lanes. The owner-approved plan behind it is `~/.claude/plans/ok-so-right-now-harmonic-grove.md` on the coordinator's machine. Read this whole file before writing code.

## The rule

Fix the wrong fact where it is born. Delete everything that existed only to compensate for it.

- **No new layers to compensate.** Don't add a guard, cache, fallback, retry, sweep, clamp, debounce, second table or "for legacy" twin so that one layer can tolerate a wrong layer underneath it.
- **When a word on the trigger list appears in your design, stop.** The list: cache, memo, fallback, retry, guard, clamp, priority, sweep, debounce, "stays consistent with". Name the duplicate fact underneath, and delete one of the two copies.
- **Every lane deletes more than it adds**, excluding tests. Report net lines.

## Phase 1 boundary: the row contract is frozen

- Rust stays a stopped, restartable fallback until every family runs on Go. Go must therefore read and write **the existing tables with the same row states Rust reads and writes**.
- No new tables. No triggers on legacy tables. No column changes.
- v2's branch-only migrations 0087, 0088, 0090 and 0091 are reverted in phase 1. Their facts go onto the existing operation rows.
  - 0089 is a no-op, because Apps 0009 already adds the enum value. Keep it only if CI needs it.
- Deleting schema is phase 2, after Rust is retired.

## Single writer per family

- One process per family. It calls `engine.HoldFamily(ctx, directDSN, family)` at startup and stops writing when the returned channel closes.
  - Families are defined in `internal/engine/family.go`: observer, autodeposit, fleet, multiply, lookup and backyard. Fleet includes cross-mint and Voltr.
  - Use the direct Neon DSN, not the pooler.
- The family lock replaces Go-side leases between Go processes. Go writes the legacy lease and fencing columns only to the extent that a restarted Rust worker needs them to read the rows correctly.
- No adoption code. Drain-and-swap guarantees no in-flight foreign rows at the moment of the swap.
  - Remove the "foreign nonterminal row closes readiness" logic.
  - Remove the "retain legacy ALT op forever" logic.

## One send path: `land()`

- **Write-ahead.** Store the signed bytes, recent blockhash and `last_valid_block_height` on the operation row **before** the first send. A row without bytes was never sent.
- **Landing.** `land()` resends **the same bytes** until the signature is confirmed or the block height passes `last_valid_block_height`. Solana drops forwarded transactions, and this is the Rust confirmer's rebroadcast, kept as one function rather than as a worker.
- **Restart.** The family reads its rows in the signed or sent state and calls `land()` on each.
- **Outcomes.** Expired means not landed, so the row goes terminal and the planner replans. There is no separate confirmer stage and no separate signed-attempts table.

## Rust behavior to keep, fixed at its root cause

| Rust behavior | Root cause | Go fix |
|---|---|---|
| Rebroadcast until expiry | Dropped forwards | `land()` as described above |
| Sharded fee payers (`YIELD_ROUTE_FEE_PAYER_KEYPAIRS`) | Writable-account lock contention per block | The payer is a pure function of (vault, fixed payer list). Fee-only payers are allowed |
| Claim `min(saved, source_pre)` | The payout was saved before the source balance was final | Compute the payout from the source balance when the transaction is built. No `min` |
| Daily public model | Product cadence | Daily |
| Voltr fleet routes | Real production traffic | Port planning, execution and reconciliation |

For every row you touch:
1. Confirm the root cause with `git log -S` or blame on the Rust code and `docs/plans/*`.
2. Write it in the commit message.
3. Add a regression test that reproduces the original failure.

## Facts, not views

- Health is `internal/engine/facts.go` on `/metrics`, and nothing else:
  - `loyal_family_landed_total{family}`
  - `loyal_family_failed_total{family,code}`
  - `loyal_family_inflight{family}`
  - `loyal_family_last_progress_timestamp_seconds{family}`
- Alert rules live in Alertmanager (lane 3). Processes write logs as structured slog JSON on stdout; a collector ships journald to ClickStack.
- No in-process alert routing. No OTel log exporter in application code.
- `code` values are stable snake_case and reuse the Rust `OperationalError` codes where the meaning is the same.

## One language

Delete the Go → Rust child processes by porting what they do:
- `internal/fleet/route_build.go` → `loyal-klend-proxy`
- `internal/observer/earn/bridge.go` → `earn-domain-bridge`

## Lanes

Branch each lane from `codex/workers-v2-golive`. Push the lane branch, and run an adversarial "is this a compensating layer?" review before asking to merge.

### Lane 1, money path
- `land()` and write-ahead.
- Fold the confirmer into `land()`.
- Payer selection.
- Claim payout computed at build time.
- Port Voltr.
- `HoldFamily` in the retail families.
- Revert 0087, 0088 and 0090, putting their facts on the operation rows.

### Lane 2, one language plus Backyard
- Port the KLend builder and Earn/Squads application to Go, and delete both Rust children.
- Re-port `internal/backyard` from the production Backyard line, `origin/feat/voltr-rwa-selector`. `f821a78f6a` is the deployed image commit. Include its migrations 0074–0083 and 0085 in CI fixtures.
- Delete `go/kamino-fleet-planner` and `go/backyard-rwa-worker` once their code lives only in `internal/`.

### Lane 3, platform
- Static binaries plus systemd units with `LoadCredentialEncrypted=` and `Restart=always`, under `deploy/hetzner/`.
- Wire `/metrics` facts into all three commands.
- Prometheus and Alertmanager config, with the Telegram receiver.
- journald → ClickStack collector config.
- Revert 0091.
- On the Apps branch `codex/workers-v2-app-contract`, drop the 0091 dependency and fix its typecheck errors.

## Done for phase 1
- `go build ./... && go vet ./... && go test ./...` passes, plus the DB and SVM suites with fixtures.
- No child processes.
- No new tables.
- Each family runs for one hour against a refreshed DB copy with no money keys, and its decisions match production.
