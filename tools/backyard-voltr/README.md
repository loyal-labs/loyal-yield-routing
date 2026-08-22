# Backyard Voltr partner validation

This is the maintained mainnet-first proof surface for:

```text
Backyard user -> Voltr USDC vault -> native Kamino Main / OnRe / Prime / Maple
                                      ^
                         Squads PDA manager, eight exact policies
```

It does not use Trustful, a Loyal custody token account, a directly
Squads-owned Kamino position, or a custom Solana program.

The only current configuration source is `src/domain/route-spec.ts`. The
withdrawal waiting period is exactly 600 seconds. Voltr instructions come from
the pinned `@voltr/vault-sdk`; conversion to legacy web3 transaction/RPC types
is isolated in `src/integrations/solana-compat.ts`.

The separate 86,400-second locked-profit degradation duration is not a
withdrawal lock. It controls Voltr profit smoothing and must not be used to
derive a user's receipt deadline.

## Read-only checks

Run secret-dependent commands through the repository's 1Password mount:

```sh
bun run check
bun run verify:structure
bun run verify:compatibility

# Reproduce the exact semantic envelope that needs operator approval. This
# command loads no signer and never creates or sends a transaction.
op run --env-file=../../.env.1password -- sh -c '
  bun src/cli.ts bootstrap approval-init-adaptor
'

op run --env-file=../../.env.1password -- sh -c '
  bun src/cli.ts verify squads
  bun src/cli.ts bootstrap simulate-init-adaptor
'
```

`verify:compatibility` is a confirmed-mainnet, no-broadcast fail-fast probe for
the exact Main, OnRe, Prime, and Maple USDC reserves. It live-decodes each
Kamino graph, rebuilds the pinned Voltr initialize/deposit/withdraw
instructions, measures the real Squads/ALT packets, simulates each missing
strategy's atomic initialization, rechecks deployment identities, and proves
why the runtime surface requires eight physical policies. A compatibility PASS
does not claim the strategies or policies are installed and never substitutes
for the final confirmed lifecycle verifier.

Policy compilation and independent recompilation:

```sh
op run --env-file=../../.env.1password -- sh -c '
  bun src/cli.ts policies compile --out /tmp/backyard-runtime-policies.json
'
bun src/cli.ts policies verify --artifact /tmp/backyard-runtime-policies.json
```

The compiled artifact records one intentional Squads limitation: its
`ProgramInteraction` data constraints pin the canonical 30-byte payload bytes,
but the policy format has no instruction-data length comparator and therefore
cannot itself reject appended trailing bytes. Exact length is a canonical
builder and pre-send verifier invariant; it is not claimed as on-chain Squads
coverage.

## Mutation boundary

Every `execute-*` mode requires `CONFIRM_MAINNET=1` plus exact identity/hash
confirmation flags. A sender rebuilds and simulates a fresh transaction,
reloads protected confirmed state, persists one signed wire, and submits that
byte-identical wire through a bounded recovery loop. Each RPC submission keeps
`maxRetries: 0`; the sender polls the expected signature between submissions,
never rebuilds or re-signs, and records the actual submission attempt count and
wire hash before confirmed readback. An unknown send status is recovered only
by the precomputed signature; never rerun with a new intent blindly.
The package is private. Its exported low-level preparation/sender helpers are
internal plumbing shared by the bootstrap, policy, manager, and user slices;
they are not an in-process sandbox or an authorization boundary against
malicious same-repository code. The supported CLI `execute-*` paths are the
operator boundary. The low-level sender still requires `CONFIRM_MAINNET=1`
plus an explicit authorization context slot at or after simulation. That slot
is passed to RPC preflight so a load-balanced endpoint cannot authorize against
an older bank.
The atomic vault/adaptor bootstrap additionally requires the exact
`--confirm-max-total-lamports 12900000` ceiling; a refreshed blockhash cannot
expand the approved SOL exposure. Strategy bootstrap and each policy install
likewise require their reported maximum-lamport confirmation, and policy
installation additionally confirms the exact policy-create data hash. The two
legacy partial-bootstrap senders are not exposed; vault initialization and
adaptor registration have one atomic mutation path.

For the current route, the stable strategy-bootstrap instruction-data hashes
are:

- set temporary setup manager:
  `cc6da1676cab30a17e0b18f2a4f03506d650070fbb4f151b2cc4eb403652ef67`;
- initialize the native Kamino Main strategy:
  `057ae86a8c0baded43cc1e07c6818daaf5f30d1c2232cc30d932bbc630082d4f`;
- restore the Squads PDA manager:
  `439d763c251cf83afda10b88c3b045dddf19e55a9660d7d329699e7696ef6fd3`.

These are semantic instruction hashes, not reusable signed packets. A sender
still rebuilds with a fresh blockhash and refuses if any canonical instruction
byte changes.

The intended order is deliberately short:

1. initialize the fresh Voltr vault and add the native Kamino adaptor atomically;
2. initialize the exact Main strategy with the setup-only manager swap restored
   atomically to the Squads PDA;
3. create the exact strategy-owned USDC ATA with the setup admin as payer;
4. install runtime deposit and withdrawal policies sequentially;
5. execute one bounded tiny user/manager/withdrawal lifecycle (the current
   testing wallet supports a 500,000-raw-USDC proof without a funding step);
6. run the read-only lifecycle verifier against the confirmed evidence manifest.

No simulation, compiler output, or returned signature is final proof.

## Runtime commands

The user and guardian paths are deliberately separate:

```sh
# User signer; creates the LP ATA idempotently when needed.
bun src/cli.ts runtime simulate-user-deposit --amount-raw 500000

# User signer; prove that the deprecated direct instant-withdraw packet is
# rejected without broadcast. The proof requires exact Custom 6015,
# InstantWithdrawNotAllowed logs, wait=600, no event/state mutation, and the
# canonical one-instruction packet. Use the request/claim flow for withdrawals.
bun src/cli.ts runtime simulate-instant-withdraw-rejection --amount-lp <LP_RAW>

# Guardian signer; exact four-market catalog is mandatory. Each manager
# operation names only its closed strategy id; policy/account bytes come from
# the catalog and cannot be supplied on the command line.
bun src/cli.ts runtime simulate-manager --operation deposit \
  --strategy-id main --amount-raw 500000 \
  --artifact ../../docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v1.json \
  --authorization ../../docs/evidence/backyard-voltr-four-market/policy-catalog-authorization-v24.json

# Execute only with an explicit, new intent path. The exact pre-send packet,
# authorization/artifact hashes, expiry, and expected signature are persisted
# with wx/no-overwrite semantics before the single send; recovery reports the
# same signature and instructs operators not to resend.
bun src/cli.ts runtime execute-manager --operation deposit \
  --strategy-id main --amount-raw 500000 \
  --artifact ../../docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v1.json \
  --authorization ../../docs/evidence/backyard-voltr-four-market/policy-catalog-authorization-v24.json \
  --confirm-authorization-sha256 <AUTHORIZATION_FILE_SHA256> \
  --confirm-route-authorization-sha256 <EFFECTIVE_ROUTE_AUTH_SHA256> \
  --lifecycle-id <LIFECYCLE_SHA256> \
  --confirm-vault <VAULT> --confirm-amount-raw 500000 \
  --confirm-artifact-sha256 <CATALOG_FILE_SHA256> \
  --confirm-wrapper-data-sha256 <WRAPPER_DATA_SHA256> \
  --intent-path docs/evidence/backyard-voltr-four-market/intents/manager-main-deposit-<ID>.json

# Setup admin; creates the strategyAuth-owned USDC ATA required by the native
# Kamino adaptor. This is a separate, exact ATA-only transaction.
bun src/cli.ts bootstrap simulate-strategy-asset-ata

# After reviewing that fresh report, execute only with all exact approvals:
# --confirm-ata <reported strategyAuth USDC ATA>
# --confirm-instruction-data-sha256 <reported ATA instruction hash>
# --confirm-max-total-lamports 3000000

# After a confirmed request, before its exact 600-second deadline.
bun src/cli.ts runtime simulate-withdraw-claim-premature \
  --request-signature <CONFIRMED_REQUEST_SIGNATURE>
```

Every runtime sender rechecks deployment identities, protected confirmed
prestate, token economics, and SOL fee/rent accounting immediately before its
single send. It persists the exact ordered 42-account prestate bytes plus a
fixed-signer Ed25519 pre-send attestation, then emits the exact poststate bytes
and a linked settlement attestation after confirmation. The verifier
recomputes every row/data/state hash and verifies both signatures; legacy
hash-only protected-state outputs fail closed. Every user sender also requires
an explicit `--confirm-user` identity before loading the signer. For withdrawal
request and claim, the receipt PDA, LP amount, claim deadline, and originating
request signature are validated against that identity. The claim path compares
the complete confirmed request packet to the SDK-built ATA-create plus Voltr
request instructions.
User deposit execution additionally requires
`--confirm-max-total-lamports 3000000`; withdrawal-request execution requires
`--confirm-max-total-lamports 5000000`. These are machine confirmation values,
and each preflight/readback computes spend as the quoted fee plus exact newly
created account rent.

The direct instant-withdraw path has no execution command. Its rejection proof
uses confirmed commitment/context slots, never broadcasts, and binds the exact
packet/error/log/state evidence to the four-market route. User deposit,
withdrawal request, and withdrawal claim evidence/intents are bound to the
four-market route id and hash, while the canonical Voltr vault builder remains
the exact vault-level SDK surface.

The four-market restoration worker begins with the read-only confirmed receipt
scan:

```sh
bun src/cli.ts runtime scan-withdrawals \
  --request-signature <CONFIRMED_REQUEST_SIGNATURE> \
  --request-event-index <REQUEST_EVENT_INDEX> \
  --request-receipt <EXACT_RECEIPT_PDA> \
  --out ../../docs/evidence/backyard-voltr-four-market/withdrawal-demand-scan-confirmed-v1.json
```

This command loads no signer and never broadcasts. It filters the exact Voltr
receipt discriminator and partner vault, applies the strict deployed receipt
decoder plus canonical PDA check, and reads the exact idle USDC ATA. It reports
an aligned confirmed observation slot, the exact raw RPC query/config hashes,
raw account bytes, and a request-signature/event-index/receipt-generation
fingerprint when those origin flags are supplied. The slot is not a durable
first-observation marker; restart dedupe and execution ownership live in the
existing orchestration outbox. `idleShortfallRaw` is a
conservative bigint sum: each fixed-point receipt amount is rounded up to raw
USDC units before pending demand is compared with the configured idle floor.
For a restoration scan, all three request-origin flags are required together
and must come from the same confirmed request artifact. The CLI does not
default the event index or infer a receipt from a multi-receipt scan. Omit all
three flags only for an unbound inventory scan.

Turn the confirmed scan into a restoration plan with exact four-market position
evidence. The default loads positions from confirmed RPC at or after the scan
slot; `--positions` accepts a previously produced, route-bound position
artifact:

```sh
op run --env-file=../../.env.1password -- sh -c '
  bun src/cli.ts runtime plan-withdrawal-restoration \
    --scan ../../docs/evidence/backyard-voltr-four-market/withdrawal-demand-scan-confirmed-v1.json \
    --generation <POSITIVE_GENERATION> \
    --lifecycle-id <LIFECYCLE_SHA256> \
    --route-authorization-sha256 <EFFECTIVE_ROUTE_AUTH_SHA256> \
    --protected-address-set-sha256 <PROTECTED_ADDRESS_SET_SHA256> \
    --protected-state-sha256 <REQUEST_POSTSTATE_SHA256> \
    --protected-context-slot <REQUEST_POSTSTATE_SLOT> \
    --outbox-input-out ../../docs/evidence/backyard-voltr-four-market/restoration-outbox-input-v1.json
'
```

The planner is read-only and refuses stale or mixed slots, non-positive or
over-cap legs, missing request-origin bindings, and a plan that does not restore
the exact shortfall. The default position reader loads all four exact route
positions from confirmed RPC; `--positions <POSITION_EVIDENCE_JSON>` is allowed
only for a separately produced route-bound position artifact. Submit the emitted outbox JSON through the existing Rust
Earn boundary; do not add a second TypeScript scheduler:

```sh
cargo run -p loyal-yield-orchestrator --bin fleet-opportunity-planner -- \
  --enqueue-voltr-restoration-json \
  ../../docs/evidence/backyard-voltr-four-market/restoration-outbox-input-v1.json
```

The bridge worker then executes only the exact Main restoration withdrawal legs
authorized by that durable outbox movement. Supply the full restoration bridge
binding to `runtime execute-manager --strategy-id main --operation withdraw`:
`--restoration-origin-id`, generation, leg id, owner, protected address/state
hashes, protected context slot, and evidence directory. After confirmation,
persist the manager intent/readback and run the maintained readback command
shown below. The readback must be combined with the exact manager transaction
artifact before claim; it does not manufacture chain evidence.

After each canonical TypeScript manager restoration leg is confirmed and its
signed intent/confirmation is persisted through the existing Neon outbox
boundary, reload the database-owned rows with the maintained one-shot
readback command:

```sh
cargo run -p loyal-yield-orchestrator --bin backyard-voltr-restoration-readback -- \
  --input ../../docs/evidence/backyard-voltr-four-market/restoration-readback-input.json
```

The input contains only the route cluster, immutable restoration `originId`,
generation, and expected leg count. `NEON_DATABASE_URL` is required; no
signer, Solana packet, or broadcast is loaded. The command refuses pending,
partial, duplicate, cross-generation, or unacknowledged rows and derives the
verifier's `durableOutbox` rows from PostgreSQL payloads, including the exact
manager signature, fence, confirmed slot, and transaction-anchored readback
context. The operator then combines this readback with the canonical manager
transaction artifacts and independently verified shortfall recomputations;
the readback command does not manufacture chain evidence.

The shared Earn adapter is a replay artifact, not another planner. Produce it
from the exact Rust observation/planner replay and bind it to the same lifecycle
and request-protected context:

```sh
bun src/cli.ts verify earn-adapter \
  --input ../../docs/evidence/backyard-voltr-four-market/earn-adapter-producer-input-v1.json \
  --artifact-out ../../docs/evidence/backyard-voltr-four-market/earn-adapter-confirmed-v1.json
```

The producer input must use the maintained `loyal-yield-orchestrator` and
`loyal-yield-store` outputs. Hand-editing a replay or claiming that normal Earn
optimization restored a withdrawal is rejected by the final verifier.

## Lifecycle evidence

`verify lifecycle` accepts one strict schema-version-1 JSON manifest. It binds
the route identities and raw asset/LP amounts; the exact runtime policy file;
an artifact-hashed premature-claim proof; and unique, ordered confirmed
transactions. Each transaction entry contains only `signature` and the
SHA-256 of its serialized confirmed message. Unknown or missing fields fail.

The verifier then independently checks mainnet genesis, signers, ordered
top-level programs, route accounts, token and lamport deltas, request/claim
events, the exact 600-second deadline, policy origins, current deployments,
and closed receipt/escrow state. A manifest cannot pass merely by naming a
successful signature.

The stricter four-market confirmed verifier uses a deterministic path-only
manifest input. It derives all identities, amounts, signatures, slots, intent
hashes, and message hashes from the maintained command artifacts; the input
cannot provide expected signers, programs, accounts, or deltas:

```sh
bun src/cli.ts verify four-market-manifest \
  --inputs ../../docs/evidence/backyard-voltr-four-market/manifest-inputs-v1.json \
  --out ../../docs/evidence/backyard-voltr-four-market/confirmed-lifecycle-v1.json

bun src/cli.ts verify four-market \
  --commitment confirmed \
  --evidence ../../docs/evidence/backyard-voltr-four-market/confirmed-lifecycle-v1.json
```

`manifest-inputs-v1.json` is intentionally a small, exact path manifest. It
lists the 13 confirmed transaction artifact paths in lifecycle order, the
seven exact proof artifacts, and the policy catalog/authorization.
Its transaction keys are exactly `userDeposit`, the eight ordered
`manager{Main,Onre,Prime,Maple}{Deposit,Withdraw}` legs,
`managerMainFallbackDeposit`, `withdrawRequest`,
`managerMainRestorationWithdraw`, and `withdrawClaim`; its artifact keys are
`instantWithdrawRejection`, `prematureClaim`, `withdrawalScanner`,
`restoration`, `earnAdapter`, `negativeMutations`, and `finalReconciliation`.
Create it only after every artifact exists; do not paste transaction fields or
invent signatures into it. The verifier derives those values from each named
artifact and fails closed on unknown keys, duplicate signatures, route/hash
drift, or a gap between request poststate, restoration, and claim.

The final command emits `BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_PASS` only after
all four strategy graphs, eight policies, byte-exact confirmed packets,
withdrawal restoration, shared Earn adapter, mutation matrix, and final current
conservation gates pass. The coherent manifest has thirteen confirmed
transactions; the named Main restoration withdrawal sits between request and
claim so the
protected-state chain contains no invisible mutation.
