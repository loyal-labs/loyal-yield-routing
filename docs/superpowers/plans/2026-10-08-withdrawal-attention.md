# Earn MAX withdrawal attention implementation plan

**Goal:** Keep small/large legitimate withdrawals on safe partial paths; report operator-required delays honestly; remove false Backyard pages. No new automatic full-debt authority.
**Architecture:** Existing worker decisions and confirmations stay authoritative. Add a fenced display-only withdrawal-health projection, read server-side for the configured pooled vault, with stable metrics consumed by current monitoring. Existing UI layout is retained.
**Stack:** Go/Postgres/Prometheus/Alertmanager; Next.js/TypeScript/Bun.

## Task1 — worker safety and state (backend worker)
- Trace partial fallthrough; prefer supported validated debt-preserving operations without letting90%/$50/six-round heuristics grant full-exit authority. Unknown or unbuildable partial remains held; preserve explicitly confirmed unwind and verified emergency paths.
- Persist display-only withdrawalHealth under existing lease-fenced route JSON without decision generation changes. Scope by canonical vault/program/cluster/route. Fresh known user-withdrawal intervention blocker => operator_attention; ordinary work => waiting; no demand => none; missing evidence => unavailable. Preserve current attention during failed reads and onset across restarts. No false recovery from NAV-only activity. A15minute no-funding-progress timeout may escalate persistent shortfall, excluding covered cooldown waits.
- Publish stable family/route attention gauge plus observation freshness; reflect successful durable write, never every retry. Exact typed selector_finish_current_work_first becomes expected deferral, not failed; retain unknown selector_evaluate_unavailable failure and safe cause codes.
- Tests: small$5 and90% do not authorize full exit; feasible partial remains possible; unknown and cumulative/signed safeguards unchanged; status persistence/currentness; expected deferral vs genuine failure metrics. Use bounded native tests; no live writes.

## Task2 — rules and notification (monitoring worker)
- Add deduplicated withdrawal-attention alert from persisted-state metric and freshness coverage. Retain existing genuine failure math. Use actionable summaries without fractional operation claims.
- Escaped Telegram template: status, severity, impact/action/runbook; no inaccessible GeneratorURL or arbitrary labels. Validate promtool/rule fixtures and template escaping. Update operating README only; no host changes.

## Task3 — frontend (root)
- Read only scoped fresh worker health via existing server-only Yield Neon connection in Voltr summary. Never expose raw state/errors. Claimability stays chain-derived; canClaim overrides delay messaging. Missing, ambiguous, stale or wrong-scope health cannot become healthy/claimable.
- Disable header, RWA Loop and equivalent mobile/activity Withdraw entrypoints while pending; accessible hover/focus/touch explanation. Preserve Claim and Check status and existing backend duplicate-request rejection.
- Distinct attention/unavailable text with no ETA or unverified team-notified claim. Update Earn MAX README. Focused money/status verifier, scoped lint and configured typecheck; no local frontend build.

## Integration
- Two workers only; disjoint ownership, root handles commits. Cross-review safety/alerts, verify exact combined diff. Publish PRs; do not silently merge parent migrationPR271. No new debt clearing, limit/credential changes, remote services or live financial actions during implementation.
