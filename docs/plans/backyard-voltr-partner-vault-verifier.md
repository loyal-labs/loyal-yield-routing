# Backyard Voltr partner-vault verifier

Status: completed mainnet partner proof (600-second route). The maintained
implementation, artifact checks, refusal guards, nine finalized lifecycle
transactions, independent lifecycle verifier, and current-state reconciliation
all pass. The authoritative manifest is
`docs/evidence/backyard-voltr-partner-vault/lifecycle-600s-finalized-v1.json`;
the independent result is
`lifecycle-verifier-finalized-600s-v2.json` with
`PARTNER_LIFECYCLE_PASS` and zero failed gates.

All earlier 3,600-second and 86,400-second withdrawal-lifecycle evidence is
superseded. It may be retained as historical evidence, but it cannot satisfy
or authorize any step of this verifier.

This replaces the 24-hour POC as the definition of done for Backyard partner
validation. It checks the observable vault lifecycle, not whether an
implementation checklist was followed. Do not weaken it to match partial work.

## Required end state

### 1. Vault and authority

- A finalized mainnet Voltr vault for native USDC exists and decodes with the
  pinned deployed layout and approved Voltr program identity.
- `withdrawalWaitingPeriod` is exactly `600` seconds.
- Its manager is the Loyal Squads vault PDA derived from the approved Settings
  account and vault index. It is not an EOA.
- The native Kamino adaptor receipt and the strategy receipt for Loyal's
  cataloged Main USDC reserve exist and decode against approved program
  identities.
- Runtime custody is Voltr idle/strategy custody. No Trustful account, Loyal
  custody ATA, or directly Squads-owned Kamino position appears in the route.

### 2. Runtime policy

- The delegated guardian is not a vault owner or unrestricted manager.
- Exactly the intended permanent Squads `ProgramInteraction` policies authorize
  native-Kamino Main deposit and withdrawal.
- Each policy binds the vault, Squads manager PDA, Voltr/native adaptor,
  reserve/market/farm graph, USDC mint, Token Program, exposed critical account
  roles, instruction discriminator/tail, `additionalArgs = None`, and
  `0 < amount <= configured maximum`.
- Setup-only manager authority and setup-only policies are absent from the
  runtime boundary.
- Substituting another vault, manager, guardian, reserve, mint, program,
  discriminator, account role, zero amount, or over-limit amount fails before
  signing or fails the deployed policy simulation.

### 3. Finalized partner lifecycle

One coherent evidence manifest names the exact route and finalized signatures.
Independent verification proves all of these boundaries:

1. User USDC decreases by the deposit amount; Voltr idle increases by that
   amount; exactly one canonical deposit event binds the user, vault, asset,
   raw asset amount, raw LP amount, total-value delta, dead-weight delta, and
   zero-fee LP-supply relationship.
2. Guardian/Squads moves the bounded amount from Voltr idle into the Main
   Kamino strategy; Voltr idle decreases, the approved Main reserve liquidity
   supply increases by the exact asset amount, and its canonical
   obligation/collateral/farm position increases.
3. Guardian/Squads returns the requested liquidity to Voltr idle before the
   user's withdrawal is processed; the same Main reserve liquidity supply
   decreases by the exact asset amount, collateral/farm exposure decreases,
   the transient strategy USDC ATA remains zero, and idle increases by the
   reconciled amount. Accrued residual yield may remain strategy-owned.
4. The user's withdrawal request escrows the intended LP and creates the
   canonical receipt. The finalized event binds the exact receipt, asset,
   requested LP amount, `isAmountInLp = true`, `isWithdrawAll = true`, quoted
   asset amount, and `withdrawableFromTs - requestedTs == 600` exactly.
5. A pre-deadline claim simulation is rejected without mutation. A claim at or
   after the deadline finalizes, pays exactly the finalized request's
   fixed-point event quote, burns the exact escrowed LP, leaves dead weight and
   accumulated fees unchanged, decreases vault total value and LP supply by
   the event-bound quantities, closes the receipt, retains the canonical escrow
   ATA empty and rent-exempt for reuse, and produces the expected fee/rent and
   final vault balances.

Compilation, local tests, simulation, submission, or an RPC-returned signature
alone cannot satisfy a finalized boundary.

### 4. Partner implementation shape

`tools/backyard-voltr/` is the maintained partner-validation implementation.
It has one source for route identities and limits and exposes these boundaries:

```text
src/domain/route-spec.ts
src/domain/execution-intent.ts
src/integrations/solana-compat.ts
src/integrations/voltr.ts
src/bootstrap/commands.ts
src/runtime/commands.ts
src/verify/finalized.ts
src/verify/current.ts
src/cli.ts
```

- Voltr SDK builders are the canonical instruction source; no hand-encoded
  Voltr instruction may be used.
- `@solana/kit` is the internal representation. Legacy web3.js conversion is
  contained in `solana-compat.ts`.
- `RouteSpec` is the sole maintained source of vault, manager, guardian,
  reserve, program, mint, limit, and waiting-period identities.
- A typed `ExecutionIntent` binds route, operation, amount, canonical message
  hash, prestate slot, expiry, and nonce. No database or monitoring system is
  required for this partner-validation phase.
- Bootstrap/configuration commands are separate from normal user and manager
  runtime commands.
- Finalized-transaction proof is separate from current-state reconciliation;
  a later valid state transition must not rewrite historical truth.
- The historical `tools/backyard-voltr-poc/` may remain as an evidence archive
  but is not imported by or required to run the maintained partner tool.

### 5. Execution safety

- Every write verifies mainnet genesis, exact signer identity, owners,
  discriminators, layouts, token program, account roles, expected SOL/token
  deltas, approved deployment identities, and an unexpired blockhash.
- Every write has a public-safe unsigned summary and a fresh passing mainnet
  simulation before signer use. It sends at most once with transport retries
  disabled and recovers only by its precomputed signature.
- Every user write pre-authorizes the exact user identity. Deposit and request
  also bind fixed maximum fee-plus-new-rent exposure before signer loading.
- The low-level sender requires `CONFIRM_MAINNET=1` and an authorization context
  slot at or after simulation; RPC preflight cannot use an older bank.
- A manager operation is rebuilt and re-simulated after the final protected
  state, deployment, and semantic-policy reads. Finalized reserve-liquidity and
  collateral deltas come from the exact transaction metadata, not a later
  shared-market snapshot.
- Manager packets bind a fixed 500,000 compute-unit limit and 256 KiB heap
  frame. The strategy-owned USDC ATA is created and verified separately before
  manager execution.
- Secrets are loaded only through the mounted 1Password environment and never
  appear in artifacts, logs, source, commands, or chat.

The trusted-code boundary is explicit: this private CLI protects operators from
wrong inputs, stale state, RPC ambiguity, and accidental resend. It does not
pretend to sandbox malicious code already running inside the repository; such
code could invoke web3.js directly, so an in-process "unforgeable" wrapper would
be ceremony rather than a real security boundary.

## Literal verifier

Run from the repository root:

```sh
cd tools/backyard-voltr && bun run check
cd tools/backyard-voltr && bun run verify:structure
op run --env-file=.env.1password -- sh -c '
  cd tools/backyard-voltr &&
  bun src/cli.ts verify current --route main-usdc &&
  bun src/cli.ts verify lifecycle --route main-usdc \
    --evidence ../../docs/evidence/backyard-voltr-partner-vault/lifecycle-600s-finalized-v1.json
'
```

The two live commands must be read-only and must not load a signing key. Each
condition above receives a named PASS/FAIL gate. Overall verdict is `PASS` only
when every required gate passes against authoritative finalized evidence and
current state. On FAIL, record the smallest next mainnet experiment.

## Fail-fast execution order

1. Prove immutable route/deployment identities, exact 600-second configuration,
   deterministic SDK instruction bytes, and semantic policy constraints without
   a signer.
2. Simulate the atomic vault-plus-adaptor transaction against current mainnet
   state. Stop if its exact packet, SOL bound, deployment identity, or protected
   prestate changes.
3. Only after transaction-specific approval, finalize vault/adaptor, then the
   atomic setup-manager swap/Main-strategy initialization/manager restoration,
   then the two sequential runtime policies.
4. Use one bounded 500,000-raw-USDC lifecycle. Fail immediately on the first
   finalized readback mismatch; never continue merely because a signature was
   returned.
5. Prove the premature 600-second rejection, wait until the finalized receipt
   deadline, claim once, and run the independent lifecycle verifier before
   packaging partner evidence.

## Explicit non-goals

- Backyard frontend or wallet UX;
- database, scheduler, monitoring, alerting, or fleet orchestration;
- additional reserves beyond Main;
- a new Solana program;
- production multi-operator intent persistence.
