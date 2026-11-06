# Complete vault protection and qualify deployment

Prepared November 5, 2026, Pacific time. This is an implementation and release handoff. Creating this plan does not deploy code, apply migrations, move funds, or authorize those actions.

Start from `fix/vault-protection-20261105` at `90b5ca6ea90c54bcd39b424370e465e92069620b`, the head of [draft PR #750](https://github.com/loyal-labs/loyal-yield-routing/pull/750). Its integrated main is `57c84f77130addff60dad1426baaef562c0d9973`. The PR remained open, draft and mergeable when checked for this handoff. All eight returned workflow runs passed at that head. This evidence does not cover the future changes below.

Execute this handoff with Poteto's operator-landed stack workflow. The lead owns contracts, schema, integration, branch topology and the release manifest. Each implementation owner owns one unit. A reviewer who did not author the unit verifies it. The release operator owns migrations and activation. The risk owner approves funded scope and expansion. Stop at reviewed, tested PRs until the applicable landing and deployment authority exists.

## Preserve the approved behavior

- Keep the current NAV model. Require fresh quotes for the full holding size, and hold when coverage is missing. Financed liquidation remains a separate qualification.
- Keep ordinary withdrawals pending until their user-approved maximum total cost can be met. Show the reason and require a new signature to accept more cost. Emergency risk reduction retains priority.
- Keep exact signed-wire recovery, immutable consent versions and generation-wide accounting. Expiry, cancellation, a new consent or a stopped service cannot revoke a signed transaction.
- Do not restore the removed global spending cap. Release admission controls below govern which new actions may start. They do not replace user cost limits or spend budgets.

Earn MAX is RWA vault `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`. Looping is Trustful vault `GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1`. Their manager account is not the depositor's withdrawal authority.

## Define the release claim before enabling it

Complete the work only when supported Looping owners can approve costs and use a qualified claim path, deposits enforce the certified pricing basis atomically, and the exact release passes the applicable canary matrix. Preserve visible holds for unsupported owners, incomplete evidence and prices outside consent.

Record these three distinct completion states in the release manifest:

| State | Required evidence |
| --- | --- |
| Ready for operator review | Implementation, meaningful regressions, independent review, exact-head CI, recovery procedure and a fully specified canary packet |
| Official flows qualified | Current programs execute the exact client wires; supported owners complete the approved funded scenarios; balances, fees and journals reconcile |
| Public vault entrypoints protected | Voltr or another unavoidable on-chain boundary rejects deposits and claims that bypass the required protections, with direct-call negative tests |

Do not claim the third state from the second. A direct stale-NAV deposit can dilute other holders even when Loyal's client refuses it. An optional wrapper, a UI flag or a signed database consent does not close that public boundary. If protocol-level enforcement is unavailable, leave the full-vault protection claim blocked. Any limited official-client release needs an explicit statement of the remaining pooled exposure.

## Establish the starting evidence

The existing implementation protects manager NAV certification and updated official Earn MAX claims. Looping ordinary cost-sensitive funding holds at `withdrawal_generation_verifier_unavailable`. Its public SDK accepts direct wallet authorities, while Earn MAX consent expects a verified threshold-one Squads owner. The official deposit instruction has an amount argument but no minimum LP, expiry or certified-price argument.

Use these owning sources before changing contracts:

- [Looping partner integration](../backyard-looping-partner-handoff.md), [vault identities](../../go/workers/internal/backyard/vault.go), [cost participant loading](../../go/workers/internal/backyard/withdrawal_cost.go), [consent validation](../../go/workers/internal/earnmax/withdrawal_consent.go), and [generation validation](../../go/workers/internal/earnmax/withdrawal_generation.go).
- [Deposit ABI](../../web/packages/smart-account-vaults/src/voltr/program.ts), [official plans](../../web/packages/smart-account-vaults/src/voltr/plans.ts), [Squads preparation](../../web/packages/smart-account-vaults/src/voltr/prepare.ts), [private NAV certificate](../../go/workers/internal/backyard/nav_certificate.go), and [Voltr fee and supply decoding](../../go/workers/internal/programs/voltr/voltr.go).
- [Worker admission and recovery ordering](../../go/workers/internal/backyard/worker.go), [engine startup](../../go/workers/internal/backyard/engine.go), [API admission](../../go/workers/internal/webapp/earnmax_routes.go), and [deployment procedure](../workers/vault-protection-release.md).

The local evidence bundle is `/Users/user/loyal/reports/vault-protection-2026-11-05/`. Its `handoff/` directory contains independent Looping, deposit and canary discovery reports. Existing captured-program evidence proves the 1192-byte and 1209-byte claim wires. It does not prove deposit guards or direct-wallet Looping claims. No new runtime tests or live probes were run to write this plan.

## Assign units and preserve one writer per shared file

The branch names below are proposed, not created. Recheck the actual head and parent before starting. If PR #750 has landed, the lead starts from its actual main merge result and records the new base. Do not reset the separate dirty `loyal-yield-routing` checkout.

| Unit | Owner and exclusive write boundary | Parent and reviewable result |
| --- | --- | --- |
| 0. Resolve release prerequisites | Lead; contract and inventory documents only | Current PR head; supported owner inventory, public-boundary decision and approved verification contract |
| 1. Control new admission | Worker owner; `backyard/worker.go`, `engine.go`, engine configuration, web API admission, root-owned migration if needed | `fix/vault-protection-20261105` → proposed `fix/vault-release-admission`; durable, readable controls with recovery and risk regressions |
| 2. Scope Looping consent and accounting | Consent owner; `internal/earnmax` consent/readers, `backyard/withdrawal_cost*`, root-owned SQL and API bindings | Unit 1 → proposed `feat/looping-consent-accounting`; verified owner adapters and complete pool attribution, still held until unit 3 |
| 3. Qualify Looping settlement | Transaction owner; Voltr SDK, API claim qualification, web/mobile integration and Squads test harness | Unit 2 → proposed `feat/looping-protected-claim`; exact supported request/cancel/claim wires and visible consent flow |
| 4. Guard deposit pricing | Transaction owner, with lead-owned certificate/publication bridge; same shared SDK and harness files after unit 3 | Unit 3 → proposed `feat/voltr-deposit-pricing`; certified-price continuity and an atomically enforced official deposit path |
| 5. Prepare release and canary tooling | Release owner; scripts, CI, monitoring, release documents and scenario artifacts | Unit 4 → proposed `test/vault-production-qualification`; reproducible qualification packet and deployment gates |

Read-only discovery, deposit feasibility experiments and canary design may overlap. Use isolated worktrees for experiments that touch the shared harness or SDK. Integrate them in the table's order. Do not run competing writers over `plans.ts`, `prepare.ts`, the API schema or migrations. The lead assigns new migration numbers from current main; never edit applied 0139/0140 or copy another migration number blindly.

### Unit 0: settle only the decisions that change implementation

Inventory current Looping receipt owners, pending requests, fees, wait periods and installed programs with read-only tools. Do not infer current terms from the October partner canary. Classify authorities as direct ed25519 wallets, supported Squads owners, or unsupported PDA/multisig authorities. Record the official Looping client and its release owner. A mobile label reading "Looping" currently uses Earn MAX data and does not prove a GrHZ client exists.

Recommend direct ed25519 wallets plus the already supported Squads owner class as separate adapters. Keep unsupported authorities pending. The lead must approve the authority proof and claim design before broadening the loader. The risk owner must decide whether the release requires unavoidable public protection before any activation or permits a clearly bounded official-client stage. The default full-vault completion condition requires unavoidable protection.

For deposits, identify the authority allowed to certify a publication and the Voltr owner who can enforce the public boundary. Reuse existing manager trust where possible. Adding a new attester key or program authority requires a reviewed trust and recovery contract. Do not let the API acquire that authority implicitly.

### Unit 1: add controls that hold admission without stopping recovery

Implement narrow, authenticated, versioned release controls for each vault and new action class. Separate new optional additions, new ordinary funding, new official deposit preparation and new claim-attempt admission. Permit status reads, consent inspection and cancellation according to their existing contracts. Preserve previously reserved exact-wire handling, settlement and the existing emergency decision path. Define one durable admission cutoff that checks the control version in the same transaction as admission. Specify which previously admitted operations may finish, and expose their identities in the hold readback. A plain flag read followed later by signing is insufficient. Never treat a wire disclosed before the cutoff as revocable.

The lead owns the durable representation and compare-and-set update contract. Require restart persistence, default-deny for unconfigured newly introduced paths, operator identity, reason, scope, version and readback. Avoid a general feature-flag framework. Existing manual-recovery safety latches keep their meaning; the new rollout hold must not override them.

Do not use these existing controls as substitutes: invite GET currently returns `redeemed:true`; `admit()` checks cluster; the RWA selector flag only disables selector sampling; pilot-entry settings bound a pooled strategy action. The manual-recovery latch returns before operation recovery. Stopping Backyard stops both compiled vault workers and settlement.

Prove hold changes racing preparation, signing and reservation; restart persistence; unknown configuration; recovery of already disclosed wires; continued independent settlement; and mandatory reductions under the new admission hold. A canary allowlist can limit official callers, but it cannot isolate the pooled strategy or stop public Voltr calls. Return an explicit effective capability view to clients. Remove any new unprotected fallback or misleading use of invites as a release gate.

### Unit 2: extend consent without reinterpreting RWA signatures

Keep the current `loyal:earn-max:withdrawal-consent:v1` bytes, endpoints and persisted recovery identities. Add a distinct Looping contract and owner adapter. A direct wallet signs for its own receipt authority and explicit destination. A Squads adapter verifies current Settings ownership, threshold, timelock and permissions. Do not fabricate Settings for direct wallets.

Bind the new contract to cluster, program, vault, receipt, owner class, owner, authority, destination, successful request signature and invocation ordinal. Include LP amount, reference value and slot, fee basis, claim floor, wallet fee budget, immutable version, nonce and expiry. Scope readers, locks, budgets and uniqueness consistently. Audit existing keys that omit vault before migrating; preserve v1 retries and unresolved reservations during coexistence. Additive storage is preferable to rewriting signed payloads or renaming the existing feature wholesale.

Qualify both public LP-amount and asset-amount request modes. The existing verifier accepts LP mode. Require every affected nonzero pending receipt when allocating shared ordinary funding costs. Unsupported owners or unverifiable history must hold the operation, not disappear into the other-holder allocation. Preserve effective supply, current fees, NAV-covered versus incremental cost, separate delegate SOL expense, participant reservations and exact-wire settlement.

For legacy requests, derive the latest successful lifecycle from receipt and transaction evidence. Never invent consent, copy an RWA signature, or reset spent cost. Show missing-consent and unsupported-owner reasons. A replacement request is a new generation. Cancellation completes only with chain evidence of escrow return; it does not clear an ambiguous older wire.

Regressions must cover one authority using both vaults in one transaction, cross-domain replay, mixed owner classes, inner-instruction ordinals, concurrent widening, legacy requests, current fee changes, conservation across participants, partial funding and restart. Include expired consent with an unresolved wire. Only remove the blanket non-RWA hold after unit 3 passes for the supported cohort; retain explicit unsupported cases.

### Unit 3: prove Looping claim enforcement before enabling funding

Design the direct-wallet claim as a separate bounded experiment. The Earn MAX fence depends on a distinct Squads authority, policy seed and dust isolation. Reusing that code with a different vault constant does not prove a direct-wallet claim floor or request-generation fence.

Require same-transaction generation/economics validation, payout isolation from pre-existing USDC, the approved minimum, and the correct owner destination. Prove cancellation and replacement cannot redirect an older signed wire to another generation. If the deployed ABI cannot provide that property, keep ordinary funding held and select an enforceable program boundary with the protocol owner. An off-chain history lookup alone is not an atomic generation fence.

After the experiment passes, wire the exact qualified transaction into the Looping SDK/API and the selected official client. Require explicit cost approval, visible waiting-price and unknown states, immutable widening, bounded wallet fees, durable attempt reservation before disclosure, and residual recovery. Reject unsupported batch senders and old-client protected-operation fallbacks. Preserve public bypass limitations in the integration guide.

Execute the final serialized client wire against current captured Voltr, Squads where applicable, and real ALT contents. Verify signatures, wrong-generation rejection, wrong-owner rejection, under-floor atomic rollback, donated-dust isolation, failed-transaction fees, ambiguity across restart and replacement refusal. Record packet size, compute units and heap. Add Looping cases beside `voltr_withdrawal_protection.rs`; do not label existing RWA cases as Looping coverage.

### Unit 4: prove certified deposit pricing, including dilution protection

Run this feasibility sequence before choosing production code:

1. Establish an authenticated link from the private NAV certificate to an actual published report. Bind publication identity, report hash, manifest, full quote amounts, quote times and slots, holdings/debt/book digest, NAV and confirmed publication result. A freshly signed response containing an old published NAV is insufficient. If a price-affecting observation changes, recertify or hold. Do not turn a confirmation timestamp into a new quote timestamp.
2. Prefer a disposable existing-Squads-policy experiment for the official Squads path. Test a precondition, deposit and postcondition in one final signed transaction. Owner SyncV2 bypasses policy predicates, so a predicate that never executes cannot protect the deposit. Require canonical Clock checks at execution, not only before wallet signing. Preserve the 30-second quote TTL and the stricter configured slot lag, at most 32 slots.
3. Bind canonical programs, vault, protocol, mints, token owners, source amount, destination, owner/authority and the certified publication. Derive real deposit arithmetic from the deployed binary or authoritative source. Include supply, locked profit, clock-dependent unlock, issuance/management/performance fees, fee accumulators and dead weight. Account for elapsed debt and any book change in certificate validity. A field not represented in the guard needs a proven invariant, not an omitted check.
4. Verify both recipient LP delta and the total supply/fee changes allowed by the certified pricing basis. A minimum-LP floor alone permits overmint and dilution of existing holders. Isolate the baseline from donated LP and old balances. Derive integer rounding bounds from the actual instruction rather than adding a discretionary tolerance.
5. Execute the exact production TS wire with signature verification, current binaries and ALT. Require packet size at most 1232 bytes and compute/heap within the configured limits. Test the actual signing delay and expiry semantics. Every rejected deposit must restore token, supply, vault and policy state, apart from the network fee.

The experiment must reject stale or future quotes, missing full-size coverage, a changed book, a different publication, vault/mint/owner substitution, insufficient LP, excess issuance and old-LP dust. Interleave another deposit, NAV report, fee harvest or clock-only fee/unlock change. Include bootstrap supply, overflow and rounding boundaries. Preserve the NAV definition throughout.

If the existing policy cannot enforce provenance, complete state predicates, expiry or packet limits, stop that candidate. A wrapper may enforce an attested report and LP delta around CPI, but remains optional unless Voltr gates the bare entrypoint. Atomic manager publish-plus-deposit is another candidate only if both real authorities sign the exact same expiring transaction and its packet fits. The user cannot manufacture the manager's signer or ticket. Separate report-then-deposit transactions are not an atomic guard.

Do not broaden manager authority or deploy a new program to make the experiment pass without its reviewed contract. The protocol owner must supply the unavoidable deposit/claim gate, upgrade or pause mechanism for public protection. Test bare SDK calls directly after that change. If they still bypass the requirement, report the public protection gate as failed.

### Unit 5: make qualification reproducible

Add the new regression cases to the existing worker and web contract workflows. Database financial cases must produce explicit PASS results with no skips. Capture the exact head, parent, program hashes, upgrade slots, ALT contents, schema checksums and serialized wire hashes. Use the existing independent NAV, accounting and claim reviews as scoped predecessor evidence; changed contracts need new review.

Add observable counters/reasons for certification success and holds, quote age and full-size coverage, release-control scope/version, pending consent classes, price-blocked requests, unresolved attempts, oldest unresolved age and settlement progress. Keep network fees in lamports when conversion is unknown. Do not label unknown values zero or equate an hourly NAV report with a fresh deposit certificate. Use bounded metric labels; keep transaction identities in the existing evidence records.

Produce `release-manifest.json`, `scenario-results.jsonl`, `balance-reconciliation.csv`, `program-compatibility.json`, `admission-readback.json`, `quote-deadlines.json` and `rollback-rehearsal.md` in a new timestamped release evidence directory. These are future artifacts, not files this handoff claims already exist. Each scenario row needs its code/program/config revision, input amounts, signatures or fixture IDs, before/after slots, expected outcome, actual outcome, reconciliation and reviewer verdict.

## Run the checks at their owning boundary

These commands exist at the starting SHA. Run them from the repository root unless a working directory is specified. Install locked dependencies and the pinned Go/Rust/SBF toolchain first. No local frontend application build is permitted by the web instructions; use focused type/lint checks and authorized CI-built browser/device artifacts.

| Check | Command or recipe | Required result |
| --- | --- | --- |
| Worker behavior and binaries | `bash scripts/verify-workers-v2.sh` | Format, vet, offline tests and four binary builds pass |
| Race-sensitive packages | From `go/workers`: `GOTOOLCHAIN=local go test -race -p 1 ./internal/backyard ./internal/earnmax ./internal/webapp` | Pass on the changed contract |
| Financial database cases | Reproduce the workflow's complete PostgreSQL 17 service, seven database variables and `Create the disposable test databases` step. Run `bash scripts/prepare-vault-withdrawal-test-db.sh`, then the workflow's explicit database-test list plus new cases | Fresh test DBs; actual lifecycle constraint; explicit PASS for every case, zero SKIP |
| Wallet and UI value contracts | From `web`: `bun test packages/smart-account-vaults/src/wallet.test.ts packages/smart-account-vaults/src/voltr/program.test.ts packages/smart-account-vaults/src/voltr/plans.test.ts packages/smart-account-vaults/src/voltr/prepare.test.ts packages/shared/src/earn-max-withdrawal-costs.test.ts` | Existing cases and new Looping/deposit cases pass; no protected bypass |
| Existing program tests | `cargo test --locked -p squads-test-harness --test voltr_withdrawal_protection` | Existing four local cases pass; two capture opt-ins remain separately accounted for |
| Monitoring contracts | `python3 scripts/clickstack-vault-alerts_test.py` and `python3 scripts/clickstack-looping-alerts_test.py` | Existing tests plus new hold/absence cases pass |
| Integrated delivery | Current-head Workers v2, loyal-web, Web journey contracts, Extension and Commit Style workflows, plus any new program proof | All applicable checks and independent reviews pass on the actual final head/base |

The full database recipe is in [.github/workflows/workers-v2-verify.yml](../../.github/workflows/workers-v2-verify.yml). Set `BRIDGE_TEST_DATABASE_URL`, `EARNMAX_TEST_DATABASE_URL`, `BACKYARD_FOREIGN_TEST_DATABASE_URL` and `EVM_DEPOSIT_TEST_DATABASE_URL` and create their four disposable databases. Also set `VAULT_CONSENT_TEST_ADMIN_DSN`, `BACKYARD_WITHDRAWAL_TEST_DSN` and `EARNMAX_WITHDRAWAL_TEST_DSN` for the withdrawal bootstrap. Setting only those last three leaves unrelated tests in the workflow's explicit list skipped. Use loopback PostgreSQL and a fresh `vault_cost_test_*` database. The bootstrap refuses an existing database; never point it at production. Do not infer a clean historical schema bootstrap from this focused fixture. Extend the CI workflow's pinned fixture setup for new program tests rather than relying on an ignored local test.

For each new guard, run the failing scenario on the parent and the corrected scenario on the head where meaningful. If the parent lacks the feature, record that limitation and prove the new contract directly. Re-run current program captures after any program, account layout, policy or ALT change.

Performance qualification matters for quote validity and recovery. Capture the baseline first, then use the same holdings/amounts and provider responses for interleaved repeated parent/head probes. Record request counts, complete-coverage rate and quote age, not only latency. Preserve the existing three-second ladder-start budget and eight-second collection deadline. Live observations must separately prove that evidence remains within its 30-second/slot limits at execution. Do not relax those limits to improve success rates. Measure oldest unresolved age under a backlog; the settlement loop's ten-second ticker is not a ten-second completion guarantee. A full timeout batch can take roughly 192 seconds before scan overhead and backlog.

## Stage production canaries without hiding pooled exposure

Before funded work, the release operator and risk owner must approve a concrete packet naming the exact SHA, vault, wallet, source of funds, each amount, total capital at risk, maximum incremental cost, wallet SOL fees/rent, LTV bounds, test window, stop conditions and compatible recovery owner. No amount or financial loss threshold is authorized by this document. Unknown required fields block that packet.

Use an isolated vault with the same relevant configuration when available. Current Backyard compiles both vault identities; supporting a separate vault requires reviewed configuration work. Isolation reduces financial exposure but does not prove production vault account compatibility. Recheck the real target independently. If a canary uses an existing pooled vault, enumerate all affected holders and balances in the risk packet. A small test wallet does not bound other holders' exposure.

Proceed through these gates in order:

| Gate | Actions and evidence | Expansion condition |
| --- | --- | --- |
| A. Offline and disposable staging | Captured programs, DB crash/restart/concurrency cases, public-bypass tests, admission hold and rollback rehearsal; a provided isolated API environment | All invariants pass. No production staging service is assumed to exist |
| B. Browser and device | CI-built web and a physical mobile device; consent display, sign rejection, reconnect, delayed signing, interrupted submission, old client, status/remainder recovery; fixture transactions without funded broadcast | Correct wire and UI evidence; unsupported clients remain held |
| C. Read-only production preflight | Current programs/ALTs/policies, vault terms and holders, quote sizes/age/deadlines, grants, migration ledger, journals, monitoring and current/previous release SHAs | No unexplained incompatibility or unresolved recovery contract; sealed manifest |
| D. Activate under hold | Backup and journal inventory; additive migrations; capability checks; compatible worker then API/client activation with verified admission held | Actual running SHAs and controls read back; recovery/monitoring healthy; old clients cannot silently enter protected official flows |
| E. Funded cash lifecycle | One approved scenario at a time: protected deposit, request, consent, ordinary cash funding, claim, residual recovery; repeat for supported Looping owners | Finalized receipts and token-unit reconciliation, approved floors/fees respected, no new ambiguity |
| F. Financed lifecycle | Deposit, entry, borrow, conversion, collateral deposit, accrued interest/fees, partial withdrawal, repayment, ordinary exit and claim at separately approved sizes | Complete terminal wealth and pool accounting. Cash success does not qualify financing |
| G. Expand | Increase one dimension at a time: owner class, vault, route, amount or concurrency | Reviewer and risk owner accept the prior scenario and revised exposure packet |

Exercise stale quotes, missing coverage, worsening price, mixed concurrent users, consent widening/cancel/replacement, failed fees, restart, provider outage and unknown-wire recovery in offline/staging first. Do not deliberately cause a real insolvency or liquidation to test emergency priority. Financed liquidation requires separate stress evidence and approval. Successful elapsed time alone is not sufficient; require the relevant fee, NAV refresh, request waiting-period and reconciliation events.

Reconcile wallet USDC and SOL, minted/escrowed/burned LP, fee recipients, pool idle, strategy custody, lender-direct receipts, debt and accrued interest. Separate pool expense already included in NAV from incremental expense, and user fees from delegate fees. Count pending escrow and residual user-owned USDC once. Terminal scenarios end in claimed USDC. Apply exact token arithmetic with only the program-derived rounding bounds. Require zero unexplained differences and zero unapproved admissions; an unknown price conversion remains explicitly unresolved.

## Deploy and recover from the exact reviewed release

Follow [the existing release procedure](../workers/vault-protection-release.md) after the gates and approvals above. The worker deploy script checks main ancestry and its input-tree CI artifact. Web deployment has no equivalent CI gate. The operator must independently require exact-head qualification for all changed components.

1. Record current family SHAs, approved target and previous protection-compatible SHA. Inventory both resumable operations and separate terminal/manual-recovery cost rows and client attempts. Back up the database and preserve signed bytes securely; export hashes and identities, not raw wires.
2. Run `bun run deploy backyard <approved-sha> --check-ci`. Inspect `bun run yield:migrate:check`. Apply only the reviewed migration range with the migration runner's `-to` boundary. At the starting revision, additive 0139 then 0140 are required; later units must record their own ordered versions and checksums.
3. Verify capabilities under actual service identities. Follow the existing credential policy; do not invent new roles or broad grants. Deploy the compatible worker with `bun run deploy backyard <approved-sha> --migrations-applied`, then web with `bun run deploy web <approved-sha>`. Any required new on-chain program/gate must have its own approved procedure before client enablement. Mobile publication remains separately controlled.
4. Read back actual binaries, service state, schema, controls, policies, program/ALT identities and journals. A nonzero post-activation Prometheus-rule step can leave the engine already active; inspect actual state before retrying. ClickStack objects and notification configuration are separate from that rule installation.
5. Verify both vaults' monitoring over fresh evaluation cycles. The previous six OK evaluations at `2026-11-06T00:32:50Z` proved evaluation only. Arrange a separately authorized notification-delivery test before relying on paging. Record delivery and acknowledgement; do not silently send test messages.

On any invariant breach, unexpected public bypass, code/program/config drift, missing evidence, unexplained reconciliation, quote deadline failure or approved financial bound breach, close new admission and stop expansion. Keep compatible exact-wire recovery, settlement and the existing risk path available. Never delete state or bypass the deployment in-flight gate to proceed. Blocked-but-correct withdrawals are an availability outcome, not permission to widen user cost.

Roll back only to a verified protection-compatible SHA that understands unresolved journals. Keep additive migrations, consents, attempts and reservations. If no compatible rollback exists, retain admission hold and fix forward. Finalized effects must reconcile, or finalized nonlanding plus blockhash invalidity must be established, before releasing reservations or preparing replacements. A newer consent or elapsed wall-clock time is insufficient.

Resume only after the cause is corrected, affected checks pass on the current revisions, outstanding exposure is reconciled, and the operator/risk owner accepts the updated packet. Record the hold reason, current heads, last completed scenario, unresolved identities and next permitted action in `resume.md`. Do not create a recurring monitor without a separate request.

## Close the handoff and manage drift

Before handing a unit to the next owner, record its branch, actual parent, head, file ownership, test receipts, independent verdict, remaining holds and retry count. Account for every worker. Keep one append-only decision trail in the release evidence directory.

Land only the independently verified contiguous stack from the bottom. The first unverified prerequisite is the landing ceiling. The lead alone retargets/rebases after a parent lands; freeze other writes during landing. Recheck evidence when the base, program, configuration or behavior changes. An unchanged patch alone does not establish a safe new base. New heads require their applicable CI.

The final reviewer must challenge three claims independently: Looping owner/generation coverage, deposit certificate-to-execution fairness, and canary isolation/recovery. Deliver the exact reviewed manifest and remaining limitations. Plan completion means this handoff is feasible and reviewed. Production qualification remains the work described above.
