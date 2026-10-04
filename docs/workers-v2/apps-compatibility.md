# Retained Apps and Earn bridge compatibility

This audit covers the authenticated floor writer and the expressly retained Rust Earn domain bridge. They remain existing producers; this branch does not import the complete newer Apps Earn MAX feature or replace the bridge with a second Go admission owner.

## Source boundary

| Source | Revision or file digest | Scope |
| --- | --- | --- |
| Routing behavior/schema baseline | `05338bbb70ce2e5287956f6e3659964e37bfa2f6` | Original bridge, journal, route CAS and snapshot protocol |
| Apps baseline | `81b5c0256317c46e4fa9fdac49d6c0b347530b03` | Existing authenticated floor/control interface |
| Isolated Apps contract branch | `49573b7e7393e25ad4804e39d85aa3b223b347be` | Audited floor writer and physical column mappings |
| Additional Apps source study | `45de590113312d54de7585a710620a8a80b504db` | Saved exact payout and one-transfer root Claim preparation, read through `git show`; no wholesale import |
| Pinned Apps floor repository | SHA256 `9caac874f3d919b8959fab616e311f48b7c09fb5c9486acdf36a32891e26af6d` | `apps/web/src/lib/yield-optimization/earn-autodeposit-repository.server.ts` |
| Pinned Apps migration0009 | SHA256 `210bfabba260950da59629ceee080f1426c67338a80866515a0f3aa0620c64ab` | Existing `floor_rebaseline` enum dependency |
| Retained external store | SHA256 `35c23b7a1154b61ceadc14880d9ff6cb82ca4767ff0127c615f4f8b016e7d4ce` | `crates/loyal-yield-store/src/multiply_state_store.rs`; admission SQL unchanged |
| Independently executed root Claim fixture | SHA256 `b05aef59a880bb23e6096fe6f42b435e426f6c33d601d757e3362767b1df1d6e` | `go/workers/internal/multiply/testdata/svm-root-wallet-claim.json` |

## Floor writer and native control

The web floor-confirm handler resolves the authenticated principal and supplies its settings and wallet to the repository. The body supplies the policy, delegation, vault index and proposed floor. The repository rejects negative floors and requires an active exact settings/wallet/policy/delegation/vault1 tuple. This source inspection does not claim a new HTTP authentication end-to-end test.

The Drizzle names `active` and `lifecycleStatus` map to existing physical columns `desired_active` and `chain_status`; no column adapter is needed. The floor statement locks the same target row as Go control, changes only `wallet_balance_floor_raw`, suppresses only open surplus lots, and preserves selected custody. It reads the current wallet projection, records a synthetic event, and coalesces one scheduled slot using the latest delegation start and the later due time. Missing projection or an amount at/below the floor produces no new floor surplus. Desired enablement, observed lifecycle, setup generation and bootstrap generation keep their existing owners.

Go control uses the source generation fence and reads the latest floor under the target lock. A completed bootstrap cannot be recreated by a later same-generation control pass. App floor events use the0069 bounded `[-1999999999999,-1000000000000]` range; activation events use the disjoint `[-2999999999999,-2000000000000]` range. The registered0069 allocator remains authoritative.

The first real-schema test failed with22P02 because the retained Apps migration0009 enum value was absent from the Rust registry fixture. Root added [migration0089](../../crates/loyal-yield-store/migrations/0089_autodeposit_floor_rebaseline_classification.sql), containing only the authentic `floor_rebaseline` enum addition; it does not replace the corrected bounded sequence. This is an actual compatibility dependency, not a guessed new classification. It must be present before the retained Apps floor writer is accepted against a freshly registered schema. Only the isolated local A fixture was altered during this audit.

[The pinned SQL compatibility test](../../go/workers/internal/autodeposit/apps_compatibility_test.go) renders the actual Apps CTE with Drizzle physical names and scalar placeholders. Registered PostgreSQL execution verifies Go bootstrap → two App floor revisions → same-generation Go reconciliation; actual concurrent App/Go target contention; unchanged desired/lifecycle/generations; disjoint event ranges; delegation start and scheduled-slot coalescing; and preservation of selected lot state. Selected-state preservation is not a claim of financial execution or a synthetic receipt. The retained handler still owns authentication and input validation.

## External deposits and root-owned Claims

`earn-domain-bridge` decodes confirmed transactions, projects Earn MAX memos, and reads the exact vault0 USDC account transfer at the LaserStream slot. The account reader requires successful confirmed transaction metadata, resolves legacy/v0 account keys, and requires one exact opposite USDC counterparty delta. Deposit admission remains the retained source path. Withdrawal memos establish the saved request; Claim payment is bound to that request's immutable amount and destination under the route lease, rather than inventing a new claim amount or request identifier from the transfer.

The retained store accepts external operations only when already `reconciled`, action is `deposit_claim_asset` or `claim`, cycle/route match, next generation is fenced, and no current worker operation is attached. It atomically inserts the sparse external journal, CAS-updates the route and, for Claims, writes the confirmed terminal position snapshot. A stale route CAS or snapshot failure rolls the journal back; duplicate receipt identities do not advance the route again. External records deliberately have no worker signed bytes, wire hash, blockhash or last-valid height. The Go decoder accepts this terminal form; recovery does not attempt to synthesize or broadcast a worker transaction. Runtime census excludes reconciled external journals while retaining independent route policy, snapshot and unresolved-work gates.

[The source-statement SQL test](../../go/workers/internal/multiply/apps_compatibility_test.go) extracts and executes the retained Rust insertion, CAS and Claim snapshot statements against the registered G fixture. It uses the actual SVM Claim receipt identity and effects to verify atomic rollback, idempotency, saved payout preservation, bare-journal Go decoding, one terminal snapshot and absence of autonomous rebroadcast. This proves SQL producer/reader compatibility. Its test-local admission guard is explicitly not a new production API or a full bridge/RPC integration test. Deposit acceptance is established by the actual Rust admission action guard and shared sparse insert/decoder; no independently executed deposit receipt was invented for this audit.

## Approved intentional Claim corrections

The original bridge compared the observed amount with `min(saved_amount, source_pre)`. A partial payout could therefore mark a larger saved request claimed. The retained bridge now requires exactly the saved amount, adequate source prebalance, exact request and destination, and unchanged saved amount. Partial9/request10 is refused before withdrawal/accounting mutation; full10 is accepted by the pure guard.

Matching memo/token changes or an arbitrary successful Policy CPI are insufficient authority evidence. The account reader now retains the private decoded signed transaction and loaded keys until Claim validation. Before accounting changes, the bridge requires a successful confirmed receipt's signature to match the SDK-verified transaction; an exact signed outer Squads ExecuteSyncV2 **Transaction** payload at vault0; exactly one inner SPL TransferChecked for the saved source, USDC mint, destination, vault authority, amount and six decimals; exact ordered SDK envelope bytes with the same proven account-table order; exact receipt deltas; and no other top-level instruction except ComputeBudget. Missing loaded-key or actual pre/post token-balance evidence, extra custody/accounts/transfers, Policy mode, invalid signatures and receipt drift fail closed. Current operation ownership, goal and observation slot are checked before Claim mutation.

The successful actual Squads Transaction-mode instruction supplies root-permission execution authorization at its execution slot. The saved request does not contain an App subject-wallet identity, and Earn MAX watches intentionally omit that wallet. This implementation therefore proves the Squads root-permission signer, not an invented application subject identity. Adding that independent identity would require a separately approved immutable request-authority admission contract.

The Rust validator's positive test imports the checked-in signed wire and effects produced by actual Squads SBF plus SPL Token execution in LiteSVM. It does not compare a current reconstruction against itself. Validly signed SDK Policy/wrong-settings/wrong-vault/extra-transfer envelopes and signature, loaded-key and receipt corruption are negative parser tests, not fabricated successful financial proofs. The pre-existing SVM producer separately proves that the exact root payout executes and an unauthorized delegate fails. No mature KLend execution or external provider proof is asserted.

The altered retained source is [earn_reconciliation.rs](../../crates/balance-sweep-ata-monitor/src/earn_reconciliation.rs), SHA256 `757914954f63bcec2e6ffdf128e9c81c33ddbcd8ebfca0f2bb711686c6074d72` at this audit checkpoint. These two corrections intentionally diverge from the pinned original bridge; the external store SQL remains unchanged. This bridge must remain part of the runtime until a native replacement proves equivalent receipt authorization, sparse terminal admission, CAS/idempotency, snapshot atomicity and recovery behavior.

## Local acceptance

- Registered task-owned PostgreSQL17 A floor/control compatibility race test: PASS1.508s, no skip.
- Registered task-owned PostgreSQL17 G retained Claim sparse journal/atomic reader compatibility race test: PASS1.817s, no skip.
- Rust `cargo test --offline --locked -p balance-sweep-ata-monitor --lib earn_max_claim`: both exact-payout and retained root-envelope tests PASS.

These checks use disposable loopback fixtures and checked-in source/receipts. They do not authorize production writes, deploy the observer or establish live database/provider migration readiness. No source application branch was rebased or wholesale imported.
