# Earnings notification recovery and configuration audit

The November 12 automatic rollout activated the merged OneSignal migration
before its host configuration and app registration cutover were ready. The
previous working earnings binary, `6668c70d6178096b5d2b278a1b8bcf7373cd6fef`,
used Expo without a OneSignal API key. This was a premature provider change,
not deletion of an existing OneSignal credential.

A transaction-scoped read-only inventory found 516 Expo registrations and zero
OneSignal registrations. The last old-binary job completed at 22:15 UTC. The
new job failed from 22:26 UTC onward. Main web readiness remained healthy, which
the original deployment gate incorrectly accepted as sufficient.

## Applied recovery

Restored the existing earnings job to the retained binary with SHA-256
`2a129ae389edbd565b77f1c17250f09ba048f99962178bee8e56086896fc6cbe` using an
instance-specific override:

`/etc/systemd/system/loyal-web-cron@earn-yield-joy.service.d/60-notification-recovery.conf`

The loaded command was verified before starting the resumable job. Its first
recovery execution completed successfully: 55 wallets processed, 52 quiet,
3 unavailable, zero errors and zero eligible pushes. This verifies recovery of
processing, not delivery to a phone. Existing cursor and claim records were
preserved. The web process and money-moving workers were not restarted.

The normal 23:56 UTC timer then completed at 00:00:28 UTC: 60 wallets processed,
59 quiet, 1 unavailable, zero errors and zero eligible pushes. The timer stayed
enabled, and the restored job exited successfully without error events.

The cron's newly required `MIXPANEL_PROJECT_TOKEN` was copied from the existing
production analytics configuration through the configured Vercel connector,
directly into host encryption. Its reference is in
`loyal-web-cron@.service.d/60-analytics.conf`. No new analytics timer was enabled.
No credential values were printed, placed in source, or stored in plaintext.

## Credential audit

All installed encrypted credential references for Backyard, Earn MAX, retail,
observer, Telegram, web, admin, stats and realtime decrypted successfully and
were nonempty. Realtime's envelope contains its database, authentication and
allowed-origin configuration. This verifies installed material, not every
provider's authorization or delivery behavior.

Comparing production command credential reads with the previous working web
release identified three new names: `ONESIGNAL_APP_ID`,
`ONESIGNAL_REST_API_KEY`, and `MIXPANEL_PROJECT_TOKEN`. The OneSignal migration
also introduced the earnings and alerts Android channel settings. The existing
OneSignal design points to `loyal-noncritical-env` as the key's source; that
environment is not mounted in this checkout and the VM's vault helper is not
enrolled. The API key was not recovered during this audit.

The other absent settings predate this rollout: RS256 keys (HS256 remains
configured), invite-code hashes and notification webhook/debug secrets (legacy
backend fallback), library upload token and stats Slack webhook (optional
features). Autodeposit push remains explicitly off. These were not silently
enabled or assigned replacement secrets.

## Activation hold

Keep the recovery override until the OneSignal key, app ID, both Android
channels, authenticated subscription registrations and phone delivery are
verified. Provisioning a key alone would still leave the current Expo audience
unreachable. The OneSignal migration's existing device/cutover checklist remains
necessary. Remove the instance override only as part of that verified cutover.

The updated CI preflight was exercised read-only against the host and refused
activation with `missing credentials: ONESIGNAL_APP_ID, ONESIGNAL_REST_API_KEY`.
It also rejects mismatched cron releases, failed/stale/disabled scheduled jobs,
and requires successful scheduled execution after a web activation. The
recovery override therefore cannot be silently treated as a completed OneSignal
rollout. Unrelated services remain independently deployable through service
selection.

## Completing the CI transition

The OneSignal credentials were subsequently recovered, authenticated with a
read-only provider request, and installed as encrypted web/cron credentials.
Both Android channels are configured. The notification-only Android OTA was
published to Seeker and Play Store on November 13. The owned Seeker verified
authenticated subscription registration, Earnings and Account alerts delivery,
the Earn tap, and opening Earn MAX from a closed app.

A later inventory found 348 Expo wallets, 2 OneSignal-registered wallets and
347 Expo wallets without a OneSignal registration. The transition therefore
retains the Expo transport for wallets without a OneSignal registration and
prefers OneSignal once registered. It does not switch providers after an
ambiguous failure or send through both providers for one wallet. Existing
OneSignal credentials and the database role suffice; both web and cron were
verified to have SELECT, INSERT, UPDATE and DELETE on `push_tokens`.

Reconcile the exceptional recovery pin once, using the reviewed compatibility
release and its exact main-branch CI artifacts:

1. Require the PR checks and the selected web verification receipt. Verify the
   artifact checksums, production configuration and existing release identity.
2. Pause the earnings timer and allow any running job to finish. Preserve the
   recovery override and the existing release configuration for recovery.
3. Run the candidate's `cron earn-yield-joy --dry-run` under the existing
   encrypted credentials. It must not send or change progress/claims. A saved
   cursor or a truncated scan is partial evidence; credentials are checked
   separately.
4. Activate the exact verified release with the existing guarded web deployment
   command. Remove the reviewed instance override only after web readiness
   identifies that release, then restore the enabled timer. The web service and
   cron template must resolve to the same release.
5. Stop on the first service failure, restart, failed-wallet or push failure,
   analytics failure, or nonzero summary error/undelivered count. The rollout
   watch rejects these even when the cron exits successfully or other devices
   accepted the push. Require successful scheduled runs and the scoped CI
   preflight/watch before treating the transition as complete.

If a new earnings run has already written the new rule state, do not restore the
old Expo binary automatically: pause the timer and preserve the state for a
reviewed recovery. Rolling past the rule migration can repeat old milestones.
No worker rollout, optional timer activation or credential export is part of
this reconciliation.
