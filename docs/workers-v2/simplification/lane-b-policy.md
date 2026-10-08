# B — one mechanical Squads policy decoder

Read go/workers/AGENTS.md and docs/workers-v2/contracts.md. Allowed edits:
fleet/revalidator.go, closely related policy/ABI tests; multiply/policymatch.go,
closely related policy/ABI tests; new internal/squadspolicy Go files if needed.
Do not edit multiply/worker.go, runtime health files, family journals, cmd,
go.mod/go.sum, schemas or fixture provenance/hashes. Root owns other consumer edits.

Study both actual decoders before deciding what is reusable. Fleet intentionally
accepts a narrower threshold/delegate/index/constraint subset; Multiply checks
complete canonical policy payload, timing, spending limits/hooks and topology.
Share raw decoding only where layout/contracts really match. Never replace a
strict consumer validation with a broader parser's mere success. Preserve legacy
and compact layout handling, ambiguity behavior, limits and malformed rejection.

Prefer one concrete bounded decoded representation and explicit family validation.
Avoid reflection, generic codec frameworks, loss of account-data/owner constraints,
or public aliases that merely conceal two parsers. Remove the obsolete decoding
path, and retain family economic/authority decisions beside each family.
Keep fleet planning independent of signing/broadcast and Multiply.

Use existing independent SDK fixtures and malformed/authority tests. Add only
meaningful acceptance/rejection parity cases where consolidation changes the
boundary. Send the exact proposed shared API and rejection differences early.
Deliver actual deleted parser/representation ledger and scoped semantic/race
results. Root runs connected proof and handles other-package caller changes.
No connected database access, source-sharing provider or nested agents.
