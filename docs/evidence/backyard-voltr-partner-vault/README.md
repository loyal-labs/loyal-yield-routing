# Backyard Voltr partner-vault evidence

Only artifacts whose `routeSpecSha256` is
`d73fb99b00c57153923f33560a7db13df02aba8b4c2a0b0fd181a893c8735f88`
belong to the current partner route.

The current route uses:

- 600-second withdrawal waiting period;
- 86,400-second locked-profit degradation, which is a distinct profit-smoothing
  parameter and does not affect withdrawal receipt deadlines;
- pre-created Squads Settings `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`;
- smart-account index 1 manager `DMPn3d7G2rcVVhvRbpSyEeq3cBW7bygiGjSgrLci5FYK`;
- fresh Voltr vault `AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK`;
- Main USDC reserve `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59`;
- runtime policy seeds 17 and 18.

Files with older route hashes are retained only as falsification/history. They
must never be accepted by a sender or cited as proof of the current lifecycle.
In particular, the abandoned dedicated-Settings candidates were consumed by
unrelated mainnet creators before approval; their failed preflights demonstrate
why the stable pre-created Settings route was selected.

## Finalized partner proof

`lifecycle-600s-finalized-v1.json` is the strict manifest for the completed
mainnet proof. `lifecycle-verifier-finalized-600s-v2.json` independently reloads
all nine finalized transactions, recompiles their canonical instruction graphs,
rechecks signer/account/data/ALT roles, decodes events and policies, and
reconciles current state. Its verdict is `PARTNER_LIFECYCLE_PASS` with zero
failed gates.

The bounded proof moved `500,000` raw USDC through this exact sequence:

1. user deposit into Voltr idle custody;
2. guardian/Squads manager deposit into the Main Kamino reserve;
3. guardian/Squads manager withdrawal back to Voltr idle;
4. withdrawal request for all `499,999,000` raw LP;
5. exact `Custom 6012 / WithdrawalNotYetAvailable` rejection before the
   600-second deadline; and
6. successful finalized claim after the deadline.

The claim signature is
`54f4fY1H6SeJH7ZQghi7hCMWKskC5t5G7xj7gxN9KggV9MYWJfY8XtgX8DX36LvK8dWabd3ywpqTn3UJNK1GGHRD`
at slot `440584274`. `current-finalized-600s-v1.json` then returns
`PARTNER_CURRENT_PASS`: the user LP supply is zero, the strategy position is
zero, and the vault retains only the expected one-raw-USDC fixed-point residual
(`totalValueRaw = idleRaw = 1`). Both runtime policies remain exact and pinned
deployments are unchanged.

Two deployed-program behaviors are intentional and now verified explicitly:

- fully withdrawing from Kamino closes the empty obligation and refunds its
  rent to the strategy authority; a missing obligation is accepted only while
  the decoded Voltr strategy position is exactly zero, and the deposit graph
  retains the rent/system accounts needed for lazy recreation;
- claiming closes the withdrawal receipt but retains its canonical LP escrow
  ATA at exact rent with a zero token balance for reuse. Only receipt rent is
  refunded to the user.

The live path also established three packet requirements that must remain in
the partner implementation: the strategy-owned USDC ATA must exist, manager
packets need a fixed 500,000 compute-unit limit, and Squads execution needs a
256 KiB heap frame. The saved finalized artifacts bind those details.

## Historical and preflight artifacts

These artifacts are simulation evidence only; none is a finalized transaction:

- `init-adaptor-approval-summary-600s-v1.json` is the public-safe semantic
  approval envelope reproduced without loading a signer (file SHA-256
  `96ca8d814f35a545d3fe2f747cec3cdb6c3d3988cc9f53273978d80f010f9265`).
  It binds the exact vault, manager, setup admin, 600-second configuration,
  zero-USDC movement, instruction-data hashes, and 12,900,000-lamport ceiling.
  It is a request for operator approval, not approval by itself.
- `init-adaptor-precreated-600s-v3.json` proves the SDK-built initialize packet
  encodes `600`, assigns manager `DMPn3...`, adds only the approved native
  Kamino adaptor, moves zero USDC, and passes current mainnet simulation.
  Stable semantic approval binds initialize data SHA-256
  `95908a35b501bcec3dddfbd1e1161e26c32bd300b20f715c2593967fae96848e`,
  add-adaptor data SHA-256
  `0cf74e227a64133ef1146badf51e5f5e5932b196ce5bec63e04520d9e5bae415`,
  and a maximum total SOL debit of `12,900,000` lamports. The saved blockhash,
  message hash, and signature are expired simulation details and are not an
  authorization to send.
- `runtime-policies-precreated-600s-v1.json` is the exact two-policy compiler
  artifact for seeds 17 and 18. Its file SHA-256 is
  `bb01e3acde558bf40458962f13ff2f9749e32346cd0f3406940f322bb8ee38ea`.
- `deposit-policy-17-preflight-v3.json` proves seed 17 remains free and the
  exact artifact-bound create packet passes simulation after rechecking that
  the pre-existing index-0 policies cannot act as this route's index-1 manager.
- `policy-semantic-stability-600s-v1.json` records two fresh mainnet policy
  simulations whose raw account hashes differ because Squads initializes a
  timestamp, while all 54 exact semantic policy gates pass in both runs. The
  sender therefore verifies decoded authority and constraints after finality,
  rather than comparing mutable whole-account bytes.
- `manager-deposit-runtime-simulation-600s-v3.json` is an intentional
  fail-closed probe before deployment. The exact guardian/Squads wrapper,
  fee-payer role elevation, ALT, Main route, and deployment gates pass; the
  operation stops because the new vault, strategy, and policies do not yet
  exist. It is not evidence of a successful manager deposit.
- `current-preinitialize-600s-2026-08-20-v2.json` is the latest public-RPC,
  read-only baseline at context slot `440560462` (file SHA-256
  `2e7f99e20e2d42539cfc23916e3bba2da2afa34aaadacfd921186957b1a89aee`).
  All pinned deployment identities pass, while the eight expected fresh vault,
  adaptor, and strategy accounts are absent. Its expected
  `PARTNER_CURRENT_FAIL` verdict proves prestate only, not route success.
  `current-preinitialize-600s-2026-08-20.json` is the immediately preceding
  baseline and remains historical evidence.
- `user-deposit-runtime-simulation-600s-v2.json` is a 500,000-raw-USDC
  fail-closed probe. Its SDK instruction, signer balance, route, and deployment
  checks run, but the transaction stops at the absent fresh LP mint/vault. The
  testing wallet has enough USDC for the intended 500,000-raw proof, but not
  the RouteSpec's 1,000,000-raw maximum template amount.

Blockhash-dependent message hashes and expected signatures expire. A sender
must rebuild, resimulate, and present a fresh transaction-specific summary; it
must not broadcast one of these saved packets.
