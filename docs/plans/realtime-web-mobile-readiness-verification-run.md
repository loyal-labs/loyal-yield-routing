# Realtime Web And Mobile Readiness Verification Run

Verified against production on 2026-07-12 using the fixed criteria in
`realtime-web-mobile-readiness-verifier.md`. No secret values, bearer tokens,
authorization headers, database credentials, or authenticated URLs are
recorded here.

| Check | Verdict | Evidence |
| --- | --- | --- |
| 1. Source reconciliation | PASS | Prior live commits `2712fed` and `34a991c` are ancestors of `main`; migrations 0013/0014 are unchanged; final immutable image is `light-workers:sha-4ead255ae260e7ddf31234e632c021d26e91b290`. |
| 2. Bearer token contract | PASS | Focused Rust tests and live smoke passed header-only auth, strict claims/signature/lifetime validation, previous-secret support, open-stream expiry closure, and simultaneous web/mobile streams. Query tokens are rejected. |
| 3. CORS/native access | PASS | Live preflight echoed `https://askloyal.com` with `Vary: Origin`, approved methods/headers, no credentials or wildcard; unknown origin received no permission; no-Origin mobile request connected. |
| 4. Mobile replay/resync | PASS | Replay predicates execute in SQL before `limit + 1`; full live smoke passed retained replay, 501 unrelated rows, matching overflow resync/close, stale cursor resync, cursor conflict, and string IDs. Seven-day batched retention is configured. |
| 5. Event truth/correlation | PASS | Production execution `5575`, target `5751`, slot `22228` emitted `scheduled > requested > selected > pull_confirmed > completed`; deposit `11761` and position `5903` were linked before atomic completion event `215677`. No duplicate selected transition was emitted. |
| 6. Identity/cluster isolation | PASS | Production assertions returned zero deliverable private rows with incomplete identity and zero deliverable legacy autodeposit rows. Live smoke passed cross-user and cross-cluster non-delivery. |
| 7. Immediate orchestrator wakeup | PASS | Migration 0016 emits only committed requested-slot hints on `loyal_yield_autodeposit_wakeup`. Final worker ignored the broad SSE channel, debounced three distinct hints as `wakeup_count=3`, re-read durable state, created zero executions for nonexistent hints, and retained periodic fallback scans. |
| 8. Runtime protections | PASS | Bounded queue, overflow disconnect, 15-second heartbeat, graceful shutdown, health/readiness, retention, reconnecting DB listener, and privacy-safe metrics are live. Final readiness was `listener=true broadcast_lag=0`; reconnects and queue overflows were zero. |
| 9. Adversarial verification | PASS | Rust checks/tests, 17 executor tests, targeted TypeScript/ESLint, isolated Neon migration/correlation canaries, exact-image Render verifier, and final live SSE smoke all passed. |
| 10. Deployment record | PASS | Production migrations 0015/0016 pass the real ledger/schema check. Realtime deploy `dep-d99fptok1i2s73e3vo2g` and worker deploy `dep-d99fqeks728c73d6jtlg` are live on digest `sha256:06d0399e1d0c457d73e85b9fce3d176d2ba6352cffde2cec529aa0e12d26f192`; `render.yaml` and the deployment record match. |

Overall verdict: **PASS**.

The final production full smoke result was:

```text
sse=PASS cors=true bearerAuth=true expiryClosure=true concurrentClients=true identityIsolation=true replay=true replayFlood=true matchingOverflow=true staleCursor=true
```

The signing key remains concealed. Its handoff reference is:

```text
1Password Environment: loyal-noncritical-env
Variable: REALTIME_AUTH_SECRET
```

Render Blueprint validation continues to report the repository's documented
private-GHCR visibility caveat. Live API readback independently verified both
services' exact image, digest, registry credential, direct Neon host, and
non-secret configuration.
