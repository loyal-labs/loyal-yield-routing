# Cross-mint orchestration release verifier

This is the backend release boundary before `loyal-apps` adds cross-mint
enrollment and its user-facing toggle. Run it cold against the repository,
production Yield Neon, finalized mainnet RPC, GitHub Actions, and Render. Return
`PASS_READY_FOR_FRONTEND_WIRING` only when every required condition is true.

## Required end state

### Repository and image

- `main` contains the reviewed Rust SDK, generalized two-policy detector,
  one-row policy catalog, planner, three-leg worker/reconciler, finalized
  confirmer, recovery behavior, deployment wiring, and this verifier.
- The unrelated Multiply/RWA planning document remains outside the commit.
- The focused Rust, Squads, disposable PostgreSQL, live read-only Jupiter,
  lint, and production build gates pass.
- `worker-images.yml` succeeds for the runtime commit and publishes immutable
  `light-workers:sha-<commit>` and operator image tags. Render never uses
  `latest` or an unverified local image.

### Policy observation and existing-policy continuity

- `loyal-squads-policy-monitor` is packaged in `light-workers` and runs as its
  own production Render worker with `--cluster mainnet --commitment finalized`.
- The monitor writes the canonical orchestration cluster `mainnet-beta`, not
  the CLI alias `mainnet`, and has only `NEON_DATABASE_URL` and
  `HELIUS_API_KEY` as required secret inputs.
- Migrations 35 and 36 are applied. Existing active Earn policy rows are
  explicitly promoted to the production cluster only after a release-time
  continuity audit; deployment must not leave the 4,227-policy baseline
  silently ineligible.
- New generalized swap-policy rows remain empty unless an actual finalized
  create/update is observed. Unknown removals remain as tombstones.

### Worker topology and rollout posture

- The policy monitor plus planner, health projector, revalidator, executor,
  confirmer, reconciler, and same-mint monitor run the intended immutable
  light-worker image and retain their exact commands, plans, regions, registry
  credential, and shutdown settings.
- Planner, revalidator, and executor have
  `EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER=true`. Planner and workers share a
  50-bps maximum value-loss contract; workers also use a 50-bps slippage cap.
  Revalidator and executor have `JUPITER_API_KEY` without exposing its value.
- `cross_mint_movement_controls` explicitly contains `mainnet-beta` with
  `start_new_movements=false` and `continue_or_recover_existing=true`.
  Backend code may be armed, but no production withdrawal starts before the
  frontend enrollment path is released and an operator intentionally opens
  this final gate.

### Live verification

- Every target service's latest deploy is `live`, on the expected image tag
  and digest, and not suspended. Recent deploy/runtime logs contain no startup,
  migration, panic, database, RPC, Jupiter, or reconnect loop failures.
- Production schema readback reports migrations 35/36, the explicit safe gate,
  zero active cross-mint movements, and the audited active Earn-policy count.
- Planner logs report cross-mint discovery enabled while the database start
  gate remains closed. Executor/revalidator stay healthy with no cross-mint
  policy installed. The policy monitor remains connected at finalized
  commitment.
- The already-passed current 30-pair and 478-topology read-only matrices remain
  evidence; this release sends no user or test-wallet transaction.

## Verifier-first rollout order

1. Add the missing policy-monitor image/service wiring, canonical cluster name,
   Render verifier, and safe production continuity procedure.
2. Re-run local and disposable-database gates; review the staged scope; commit
   and push the runtime change.
3. Build immutable images for that exact runtime commit and require successful
   GitHub Actions completion.
4. Deploy the policy monitor first so migration 35/36 is applied, audit and
   explicitly promote the existing production Earn-policy baseline, and write
   the closed movement gate.
5. Configure cross-mint env names, then deploy the fleet services one at a time:
   planner, health projector, revalidator, executor, confirmer, reconciler, and
   same-mint monitor. Stop on the first failed deploy or startup check.
6. Run the live Render/database verifier, inspect recent error logs, and pin the
   verified runtime image and deploy evidence in the repository with a final
   commit and push.

## Frontend handoff boundary

After PASS, `loyal-apps` only needs to create/remove the two immutable swap
policies, wait for finalized strict readback, expose the separate cross-mint
toggle/risk inputs, and surface routing state. It must not duplicate policy
bytes, pair tables, Jupiter parsing, saga state, or recovery logic. Opening the
global `start_new_movements` gate is a separate, explicit release action after
that frontend path is live.

## Verdict

- `PASS_READY_FOR_FRONTEND_WIRING`
- `FAIL_REPOSITORY_OR_IMAGE`
- `FAIL_POLICY_OBSERVATION_OR_CONTINUITY`
- `FAIL_WORKER_CONFIGURATION_OR_DEPLOYMENT`
- `FAIL_LIVE_RUNTIME_OR_DATABASE`
