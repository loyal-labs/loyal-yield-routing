# Fleet LiteSVM-first verifier

Run this verifier before starting the stateful validator E2E. Try to falsify
every condition. The validator stage is gated on an overall LiteSVM PASS.

## Required 1: exact captured fixture closure

- The input fixture first passes the offline finalized-Mainnet verifier.
- LiteSVM receives every manifest account at the exact pubkey with matching
  owner, executable bit, lamports, data length, and SHA-256 digest.
- Loaded LiteSVM accounts are read back and compared byte-for-byte.
- The fixture roots equal the production Squads, Kamino, Main/Prime market and
  reserve, and USDC constants used by Loyal Actions.
- Missing, duplicate, path-escaping, hash-mismatched, or root-mismatched
  accounts fail closed.

## Required 2: transaction-level Main-to-Prime proof

- The real committed Squads SBF fixture and Loyal Actions builders execute in
  LiteSVM through an ephemeral delegated signer.
- The route uses the exact Main/Prime market and reserve pubkeys from the
  captured fixture.
- Policy setup, Main deposit, route simulation, and route execution succeed.
- Main collateral decreases from a positive amount to zero and Prime
  collateral increases from zero to the same positive amount.
- The v0 message/ALT verifier proves exact account-universe coverage, a packet
  below the Solana packet limit, nonzero compute, and successful execution for
  the ordinary same-mint route.

## Required 3: truthful boundary and isolation

- Evidence says `LiteSVM`, `real Squads SBF`, and `deterministic Kamino mock`.
- It never claims real Kamino SBF, JSON-RPC, WebSocket, database, confirmer, or
  reconciler coverage.
- The command unsets inherited production RPC, DB, telemetry, and signer envs.
- No network listener, production database, production transaction, or
  production secret is used.
- No private-key array or production endpoint is written to evidence.

## Required 4: automated adversarial checker

`bun run verify:fleet-litesvm-e2e` must pass a positive fixture and reject:

1. a missing or unread-back-mismatched fixture account;
2. a Main/Prime root mismatch;
3. terminal-looking evidence without a balance delta;
4. evidence that falsely labels the Kamino mock as real Mainnet execution;
5. route evidence without simulation, execution, or exact ALT coverage; and
6. evidence containing a production endpoint or secret claim.

## Required commands

```sh
bash -n scripts/fleet-local-chain-e2e/run-litesvm.sh
bun run verify:fleet-litesvm-e2e
bun run fleet:litesvm-e2e -- --fixture fixtures/<capture>/manifest.json
git diff --check
```

## Verdict format

```text
Exact captured fixture closure: PASS | FAIL
Transaction-level Main-to-Prime proof: PASS | FAIL
Truthful boundary and isolation: PASS | FAIL
Automated adversarial checker: PASS | FAIL
LITESVM_E2E: PASS | FAIL
VALIDATOR_NODE_E2E: BLOCKED | READY
```
