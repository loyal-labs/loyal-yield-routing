# Web workflow handoff

Status: prepared locally on Oct 22, 2026; not activated. Production source, DNS
and mobile OTA channels are unchanged. Related issue: ASK-2346.

## Ownership switches

All switches below default off when unset. They are GitHub Actions repository
variables, not secrets. `true` is the exact activation value.

| Root workflow           | Switch                                                                 | Required secrets / variables                                       |
| ----------------------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------ |
| Extension CI            | `LOYAL_EXTENSION_RELEASES_ENABLED` gates releases; PR builds still run | `VITE_COINGECKO_API_KEY`, `VITE_MIXPANEL_TOKEN` for release builds |
| Release Packages        | `LOYAL_PACKAGE_RELEASES_ENABLED`                                       | `NPM_TOKEN`, `RELEASE_GITHUB_TOKEN`                                |
| Sync Mirror Repos       | `LOYAL_MIRROR_SYNC_ENABLED`                                            | `MIRROR_SYNC_TOKEN`                                                |
| Preview Domain          | `LOYAL_VERCEL_PREVIEWS_ENABLED`                                        | `VERCEL_TOKEN`                                                     |
| Trusted Vercel Previews | `LOYAL_VERCEL_PREVIEWS_ENABLED`                                        | `TRUSTED_VERCEL_PREVIEW_USERS` JSON array                          |

The trusted preview policy is preserved, including stale-head checks and the
trusted-author/sender boundary. Neither preview workflow should be enabled
until the retained Vercel project builds this repo from `web/apps/web` (the next
migration step). Claude and Pullfrog remain inactive under `web/.github`; their
credentials are not transferred. Commit-style and browser journeys already run
from root. The old nested copies of the five newly moved workflows are removed.

## Credential transfer

The source repo currently has the six required secrets; the destination has no
repository-level secrets. Organization-secret access could not be verified
with this login. GitHub's secret listing cannot recover values.

A separate source-repo change adds `transfer-workflow-credentials.yml`. After
review and merge, dispatch it on trusted `main` in `loyal-labs/loyal-app`.
It uses the existing release token to access the fixed destination and sends
secret values to `gh secret set` through stdin. The CLI encrypts them with the
destination public key; values are not printed or written to local files.
It also copies the trusted-user variable. It does not enable publishers,
change Vercel, or disable source workflows. The bot credentials are excluded.
If the token lacks destination Actions-secret/variable write permission, the
job fails and source publishing stays available. An authorized maintainer must
provide that permission or set the six destination secrets through Settings;
never print or export their plaintext values into logs or chat.

Check the job result and the six destination secret names before activation.
A partial transfer is safe to retry while destination switches remain off.
Disable the one-time transfer workflow after successful handoff.

## Activation sequence

1. Merge the destination workflow PR with switches unset. Require the relevant
   CI checks, including extension artifacts and mirror selection, to pass.
2. Run the reviewed source credential-transfer workflow; verify all six names
   and the trusted-user variable at the destination. Its successful transfer
   does not prove external npm/mirror token validity.
3. Inspect outstanding source release PRs and runs. Pause old Release Packages,
   Sync Mirror Repos and Extension CI; wait for any active publisher to finish.
   Do not have two repositories publishing the same package/version/tag.
4. From `web/`, build shared packages and run `bun scripts/release-packages.ts
--dry-run`; review the versions it would publish. The release set must include
   `@loyal-labs/actions` and all workspace dependencies, with repository metadata
   matching this repo. Do not treat a dry run as a publication.
5. Enable `LOYAL_PACKAGE_RELEASES_ENABLED=true`, dispatch Release Packages on
   main and verify its Changesets PR or npm publication. Preserve the source
   release history; no old tags/releases are deleted or recreated here.
6. Enable `LOYAL_MIRROR_SYNC_ENABLED=true` after packages are available. Dispatch
   Sync Mirror Repos on main for the first complete sync. Later successful
   releases select mirrors from changed `web/` paths. Verify generated source
   commit records and target repo heads. Package-unavailable skips do not prove
   sync success.
7. Enable extension releases after the Chrome/Firefox artifacts and credential
   checks pass. Existing `ext-v*` tags in loyal-app remain historical; future
   releases are owned by loyal-yield-routing. Before the first release, verify
   the intended version and consumers of the old download location.
8. Leave both preview workflows off until the Vercel source-switch step is
   completed. Then stop the old preview workflows and enable the new ones.

Rollback before publication: unset destination switches and re-enable the old
workflows after checking no destination job is running. Published npm versions
are immutable; never try to overwrite them. Preserve the old repository until
both publishing and deployment ownership are fully verified.

## Unpublished work reconciliation

The source checkout remains untouched. Published main through `3c545490` is
reconciled: the first-deposit fix is mirrored by destination PR #555; original
mobile Earn MAX commit `5fe09749` was ported by PR #554.

- Mobile owner confirms its shipped OTA note already matches `web/apps/mobile/ota-notes.txt`.
- The context-provisioning documentation and auth/backend verifiers are already
  present. Apparent dirty backend files in the older local checkout are not new
  unpublished first-deposit work.
- Local Voltr program/plans re-exports are superseded by the consolidated SDK
  imports. The remaining claim-liquidity verifier mock is adapted to the SDK's
  context-bearing RPC response; its offline contract passes.
- Local shared-type re-exports are an optional cleanup; current web types and
  imported mobile shared types remain intact. Do not overwrite newer web types.
- Quests UI/assets and sidebar/tab/shell edits belong to the existing
  [Quests PR #841](https://github.com/loyal-labs/loyal-app/pull/841), authored by
  Vlad (`ask-2335-add-quests-webapp-ui-and-responsive-navigation`). They remain
  in the source checkout, outside this workflow change. Reconcile that PR and
  local AGENTS.md changes before archiving loyal-app.
- Root `app.json`, local `.context`, `docs/handoffs` and the local post-checkout
  hook are not silently promoted into product source.

## Verification

Oct 22 local checks passed: workflow actionlint and shell parsing, secret-handoff
stdin/no-output/reject-before-write self-check, mirror selection, all six mirror
generations, claim-liquidity offline contract and scoped lint. Read-only npm
queries found all 15 release-set versions already published, including Actions
0.1.1 and smart-account-vaults 0.1.2. No package was published. External token
write permission and remote CI remain unverified until the reviewed PRs run.

Run `actionlint` on the five migrated workflows and the source transfer workflow.
From `web/`, run `bun scripts/mirror-repos/verify-selection.ts` and
`bun scripts/mirror-repos/sync.ts --dry-run --skip-repo-check`. The latter generates
all six mirrors locally and runs their safety checks; it never pushes. Run the
offline claim-liquidity verifier after reconciling its RPC mock. No frontend
production build or live wallet transaction is required for this change.
