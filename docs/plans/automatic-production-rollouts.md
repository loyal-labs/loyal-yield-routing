# Automatic production rollouts

Requested 2026-11-12. Work is tracked on the [Loyal Tuff board](https://usetuff.com/b/wkIxRA2p8i7S96lfYShOlX).

## Outcome

A merged main commit runs the worker/database/SVM checks, web and mobile checks,
browser journeys, monitoring contracts, and realtime image verification. Only a
successful verification graph may deploy that exact commit. The production job
uses the verified artifacts, runs preflight against the host, deploys each service
in sequence, and requires fresh healthy observations before advancing.

The service set is Backyard, Earn MAX, retail, observer, Telegram, web, admin,
stats, and realtime. GitHub orchestrates the existing Hetzner host; signing and
database credentials remain in its encrypted systemd credential store.

## Implementation

1. Make the existing verification workflows reusable and add a main production
   workflow. Preserve the PR checks and verification-artifact contract. Serialize
   production runs without cancelling an activation, and reject a superseded
   commit before deployment. No PR job receives production deployment secrets.
2. Upload the binaries and web assets produced by those checks. Extend the
   existing deploy command to consume these exact artifacts without rebuilding.
3. Add a host-side read-only rollout check: service identity, previous binary,
   vault selection, fresh observations, persisted targets and permissions,
   recovery state, and failed/held operations. Queries use bounded read-only
   transactions, never shared session settings. Check migration differences
   for the complete rollout before changing any service.
4. Preserve the existing host lock, checksum validation, scoped in-flight gates,
   deliberately stopped-service refusal and guarded rollback. Require a bounded
   post-deploy watch, including fresh observations from the new process, and
   stop the sequence on the first failed/held operation, exit or restart. Missing
   evidence fails the check. Ordinary no-op strategy decisions are not failed
   operations. A failure never clears a latch, changes targets or drops signed work.
5. Add realtime activation using the binary extracted from the verified image,
   preserving the environment launcher and its credential envelope. Check the
   installed checksum, readiness and restart count, with restoration on failure.
6. Exercise refusal, success, sequencing and recovery paths with bounded offline
   tests; lint workflows and shell. Run the new production preflight read-only
   where possible. Record activation prerequisites and evidence separately from
   local test results.

## Activation and boundaries

The production job requires a dedicated SSH deployment key, a pinned host key,
and the web application's allowlisted public build configuration. Database and
worker signing keys are not GitHub secrets. Deployment access to the host is
privileged and belongs only in the production environment.

This implementation does not automatically apply database migrations or restart
intentionally stopped services. Migration differences and unsafe recovery states
stop for review. New transaction shapes still require real-wire preparation and
persist/sign validation in the verification suite; CI cannot infer an omitted test.
Service/unit configuration migrations and credential rotation remain explicit
operational changes rather than silently following a binary release.

Publishing/merging these changes and the first production rollout are separate
from implementing and locally verifying them. No automatic release is claimed
until its GitHub run and production evidence have been verified.

The [activation and recovery runbook](../../deploy/hetzner/automatic-rollouts.md)
names the required GitHub configuration, read-only preflight command and failure
signals. Initial activation remains a separate task on the Tuff board.

## Acceptance

- Failed, skipped, cancelled or missing required verification cannot deploy.
- The deployed bytes come from verification of the same merged commit.
- Concurrent releases cannot interrupt an active service transition or deploy an
  obsolete commit after a newer release.
- Preflight refuses unknown identity, missing/stale health, migration changes,
  recovery latches and unsafe rollback prerequisites before any activation.
- A failure/hold/restart during observation stops the rollout and reports the
  failing service; signed work remains recoverable under the existing guard.
- A successful rollout records each service's release and fresh health evidence.
- Tests exercise negative paths, not merely workflow/source-string presence.

## Verification and remaining activation work

Local verification on 2026-11-12 covers pipeline sequencing/refusals, artifact
reuse, real activation scripts against disposable fake hosts, scoped vault
gates, failure observation and guarded restoration. Workflow actionlint,
shellcheck and shell syntax checks pass. Full worker/web builds are left to the
GitHub verification graph; no production deployment has been performed.

Read-only host checks passed for all nine service identities and health
endpoints, including fresh database observations for both vaults. The existing
Backyard process also passed the full 120-second observation window without a
deployment or restart. The source-baseline comparison deliberately refuses the
initial automatic rollout: seven older service releases have migration or
systemd changes since their running commit. Those baselines need explicit
reconciliation. GitHub production credentials/environment and public frontend
build variables must also be configured before activation.
