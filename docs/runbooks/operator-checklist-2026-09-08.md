# Backyard RWA operator checklist — 2026-09-08

One ordered handoff for the HXtk reset, strategy-two cutover, adaptor v3.3, and
the 1-USDC canary. This is not execution authorization. Never print secrets,
never set `CONFIRM_MAINNET` in an agent session, and never send without the
operator signer and the guarded command shown in the linked runbook.

0. **Suspend Render first.** In the Render dashboard suspend background worker
   `srv-dabkt0ojo6nc7381o9fg` (`loyal-backyard-rwa-worker`). Record the dashboard
   screenshot/event in `docs/evidence/backyard-rwa-strategy2/phase0-render-suspend.json`,
   then run the read-only checks:

   ```sh
   render services -o json | python3 -c 'import json,sys; print([{"id": s["service"]["id"], "name": s["service"]["name"], "suspended": s["service"]["suspended"]} for s in json.load(sys.stdin) if s["service"]["id"] == "srv-dabkt0ojo6nc7381o9fg"])'
   render services instances srv-dabkt0ojo6nc7381o9fg -o json
   ```

1. **Run Phase 1.** From `tools/backyard-voltr`, use the per-checkout
`target/backyard-voltr-compilers` for policy compilation, then follow every ordered step in
   [`hxtk-reset-2026-09-08.md`](hxtk-reset-2026-09-08.md), beginning with
   `bun run reset:hxtk verify --simulate`. Read and follow the "Operational
   rules for `--execute`" section of that runbook before the first `--execute`:
   one process per leg, never re-arm after a crash without a chain readback
   and a reconcile of the same journal, and never force a wedged claim. Do
   not skip the canonical machine-
   and user-local state root, per-leg replay fence, journal barrier, finalized
   request-time check, exact-payout claim fence, or the automatic seed-140
   one-shot policy retirement chained by `repair --execute`. Save the finalized
   post-state as `phase1-postconditions.json`; do not crank receipt1 below
   `3,793,536`.

2. **Upgrade to adaptor v3.3.** From the repository root:

   ```sh
   CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target bun run build:adaptor
   op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target cargo run -p loyal-voltr-rwa-nav-adaptor-deployer -- --spec v3'
   op run --env-file=.env.1password -- sh -c 'SOLANA_RPC_URL="$SOLANA_RPC_URL" CONFIRM_MAINNET=1 CARGO_TARGET_DIR=/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target cargo run -p loyal-voltr-rwa-nav-adaptor-deployer -- --spec v3 --execute --barrier-dir /absolute/path/adaptor-v3-barriers'
   python3 scripts/voltr_deploy_check.py --expect-voltr-sha bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56 --expect-adaptor-sha 836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0
   ```

   The deployer dry-run is the review gate; the upgrade is operator-signed.
   Update the worker M6 adaptor slot/hash and manifest only after the finalized
   readback, then rebuild and re-prove the worker image.

3. **Bootstrap strategy two and install policies.** Compile policies only in the
per-checkout `target/backyard-voltr-compilers`, then follow
   [`hxtk-strategy2-bootstrap-2026-09-08.md`](hxtk-strategy2-bootstrap-2026-09-08.md)
   in order: rehearsal, bootstrap phases A/B/C, then one policy per transaction
   at the Settings-derived seeds `145, 146, 147, 148`. The strategy-two
   delegated executor reuses the v2 executor this week (owner decision, one
   hot key); the adaptor config must still be fresh. Generate the worker
   binding from the shared seed journal:

   ```sh
   cd tools/backyard-voltr
   bun run generate:rwa-multiply-strategy2-worker-config -- --seed-journal ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-policy-seeds.journal.json --config <config2-addr> --delegated-signer <executor2-addr> --output ../../docs/evidence/hxtk-strategy2-2026-09-08/strategy-two-worker-config.json
   ```

4. **Cut over and retire 62–65.** Keep old policies installed while seeds
   `145–148` are individually finalized and verified. Stop the old worker,
   run the retirement preflight, execute the four-policy retirement only after
   replacement readback passes, and finish with the keyless
   `--assert-legacy-retired` gate. Do not reuse seeds 141–144 or run the new
   worker before 62–65 are absent at finalized commitment.

5. **Build and deploy the worker image.** The Go image is published by the
   integrated tree’s `.github/workflows/backyard-rwa-worker-image.yml` on a
   trusted `main` change; `.github/workflows/worker-images.yml` publishes the
   Rust/light image families and is not the Go-image publisher. Observe the
   trusted run without exposing secrets:

   ```sh
   gh run list --workflow backyard-rwa-worker-image.yml --branch main --limit 1 --json databaseId,status,conclusion,headSha
   ```

   After that run publishes
   `ghcr.io/loyal-labs/loyal-yield-routing/backyard-rwa-worker:sha-<commit>`,
   update the existing Render service with the immutable image and start
   command, then deploy and read back the instance:

   ```sh
   render services update srv-dabkt0ojo6nc7381o9fg --runtime image --image ghcr.io/loyal-labs/loyal-yield-routing/backyard-rwa-worker:sha-<commit> --registry-credential loyal-ghcr --start-command /usr/local/bin/backyard-rwa-worker --output json --confirm
   render deploys create srv-dabkt0ojo6nc7381o9fg --output json --confirm
   render services instances srv-dabkt0ojo6nc7381o9fg -o json
   ```

   Confirm the revision label, `LOYAL_IMAGE_VERSION`, command, policy-generation
   start gate, Settings anchor/genesis check, and 60-second startup deadline.
   The Go worker has no `CONFIRM_MAINNET` gate: deploying or resuming this
   pinned image with `POLICY_KEYPAIR` provisioned is the operator's send
   authorization, after which it sends autonomously within its policies and
   start gates. Keep the service suspended until every canary precondition is
   verified.

6. **Run the canary only after step 7.** Use [`strategy-two-canary-2026-09-08.md`](strategy-two-canary-2026-09-08.md): after the finalized degradation restore at least 24 h after the repair signature, run the 1 USDC deposit, worker allocation, refresh/restore, user withdrawal request, finalized 600-second wait, then claim. Capture
   `docs/evidence/backyard-rwa-strategy2/phase2-canary.json`. The existing
   generic user CLI is for `AdwK…`; use the deployed HXtk strategy-two wallet
   flow and record its exact finalized signatures. Do not treat
   `CONFIRM_MAINNET=1` as a worker gate; its authorization is the pinned
   Render deployment/resumption with `POLICY_KEYPAIR` provisioned after all
   preconditions pass. A worker send before then is an abort condition.

7. **Restore degradation only after the repair, before step 6.** Do not restore before the
   finalized repair transaction plus `86,400` seconds and a finalized closed
   claim/request state. Use the guarded command from the reset runbook:

   ```sh
   cd tools/backyard-voltr
   op run --env-file=.env.1password -- env CONFIRM_MAINNET=1 bun run reset:hxtk restore-degradation --repair-journal /absolute/path/hxtk-repair.json --claim-journal /absolute/path/hxtk-claim.json --execute --journal /absolute/path/hxtk-restore-degradation.json
   ```

8. **Operator decisions still open.** Keep the settings-graph relaxation (P3.1
   (g)) as `OPERATOR CONFIRMATION REQUIRED`; keep the single hot key for this
   week, so the strategy-two delegated executor reuses the v2 executor. Key
   separation is required before third-party money.
