# Observe deposits after closing Earn

A full exit closes the current policy pair and managed vault. The yield database
has no app-account tables in production, so discovery must retain the historical
wallet/settings/vault identity to observe the next setup. Previously, filtering
all sources to active positions, active vaults, or unfinished onboarding dropped
that identity after cleanup. A later finalized deposit then remained absent from
policy and position projections, and withdrawal preparation returned
`earn_policy_inactive` despite funded on-chain holdings.

Managed vault discovery now retains closed identities when their recorded cluster
matches the environment. Existing active discovery, including legacy records with
unknown cluster, remains unchanged. Closed policy accounts and their replay floor are omitted. This does
not activate a policy, change balances, or bypass finalized transaction proof.
The existing stream consumer observes new wallet/settings updates and projects
the new generation.

Already missed transactions require a bounded `earn-laserstream-gap-reconcile`
audit and enqueue, scoped to the affected wallet and finalized slot interval.
Deploy the immutable worker image to the Earn ATA monitor before testing another
close, deposit, and withdrawal cycle. Confirm both finalized chain results and
durable policy, deposit, and withdrawal rows.

Verification uses the existing disposable PostgreSQL replay scenario in
`scripts/verify-earn-replay-repair.sh`, including watch retention after cleanup,
cluster isolation, no policy reactivation, and new-deposit accounting.

The current full migration runner stops at migration 71 on an empty database
because that migration requires a production Backyard route. For this change,
the replay test passed on the disposable schema through migration 70; that is
the complete Earn schema used by this invariant.
