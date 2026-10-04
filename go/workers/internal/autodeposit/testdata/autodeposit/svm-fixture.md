The connected financial acceptance fixture is produced by
`crates/squads-test-harness/tests/workers_v2_autodeposit.rs`, with its small
`workers_v2_autodeposit/mock_topup.rs` module. It is generated into a disposable
local path, not checked in as random wallet state.

The producer executes the exact canonical Apps policy creator through the
checked-in Squads SBF. Fresh Settings requires sequential policy seed 1; the
test first proves that jumping directly to seed 9 fails with Custom(6024).
It creates the authority/delegation using the real Subscriptions SBF and SPL
Approve. The retained Subscriptions fixture requires the low-rent initialization
profile used by its existing harness. Actual wallet SOL transfers make all
exported artifacts rent exempt, and default Rent is restored before any Go
financial execution. This does not certify deployed initialization rent behavior.

The JSON records actual program hashes, program owners, account bytes, and the
successful signed canonical creator receipt. Go verifies those hashes and the
creator/account proofs before using this state. The mock protocol's seed state
uses the reviewed full Reserve/Obligation layouts and the official KLend v2
builder to create its real Squads interaction policy at sequential seed 2.

To enable the suite, produce the JSON with `cargo test --locked -p
squads-test-harness --test workers_v2_autodeposit` using these public artifact
paths:

- `WORKERS_V2_AUTODEPOSIT_SVM_FIXTURE_OUTPUT`: disposable output JSON path.
- `MOCK_YIELD_PROTOCOLS_PROGRAM_SO`: built fixed-price mock SBF path.
- Optional `SQUADS_SMART_ACCOUNT_PROGRAM_SO` and `SUBSCRIPTIONS_PROGRAM_SO`:
  otherwise the existing checked-in SBF fixtures are used.

Build the existing `fleet-local-svm` example and `loyal-klend-proxy` binary.
Run `go test -race ./internal/autodeposit -run '^TestSVM' -count=1 -v` from
`go/workers` with:

- `AUTODEPOSIT_TEST_SVM_FIXTURE_PATH`: generated JSON path.
- `AUTODEPOSIT_TEST_SVM_PATH`: built `fleet-local-svm` executable.
- `KAMINO_TEST_KLEND_PROXY_PATH`: built official proxy executable.
- The same program artifact path environment variables used by the producer.
- `AUTODEPOSIT_TEST_DATABASE_URL`: the existing allowlisted, disposable
  `workers_v2_autodeposit` fixture URL for the current-floor publication test.

Explicit acceptance runs must check that both top-level tests ran without skips.
Without SVM environment variables, ordinary offline tests skip this separate gate.
The suite uses no provider, wallet secret, or production connection.

The Go-built pull executes real Squads, Subscriptions, and SPL programs. It
proves exact receipt deltas, actual remaining budget, frozen nonce identity
rejection, funded unauthorized signer rejection by Squads, and over-budget
rejection by Subscriptions CPI. Nonce identity belongs to the Go guard: the
canonical wallet-authorized policy intentionally does not pin a recurring nonce.
The current-floor test uses an actual signed pull and actual confirmed source
balance: a changed floor refuses durable publication before broadcast, while
the exact inclusive boundary admits the same packet with broadcast count zero.
It tests this store boundary, not full controller/bootstrap admission.

The official Go top-up packet executes real Squads/SPL plus the explicitly
fixed-price KLend mock SBF. Actual SPL transfer, collateral mint, obligation
update, exact production receipt validation, coherent position conversion,
total USDC supply conservation, and duplicate-packet recovery are checked.
No KLend interest, oracle, deployed program, or mainnet correctness is claimed.
The HTTP adapter only converts the helper's actual base64 transaction into JSON
static account keys for the production receipt parser; SBF-produced balances,
errors, slots, and logs are preserved.
