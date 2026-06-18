# Adversarial Loyal Hub Policy Mitigation Plan

## Summary

Assume the Loyal Hub swap program can be adversarial or can be upgraded into
adversarial behavior. Under that assumption, a Squads ProgramInteraction policy
that only checks the Hub program id, accounts, mints, authorizer, swap tag, and
fee field is not enough to keep user vault funds safe.

The primary development goal is a static guarded route policy for Vault 1:

```text
pre-hook:  Vault 1 token A balance == 0 and Vault 1 token B balance == 0
route:     Kamino withdraw A -> Loyal Hub swap A to B -> Kamino deposit B
post-hook: verify the forwarded top-level route instructions and final accounts
```

This scheme does not allocate hook state accounts and does not require snapshots.
The hook only reads accounts and verifies the static smart-account instruction
bundle forwarded by Squads when `pass_inner_instructions = true`.

## Current Risk

The adversarial harness scenario demonstrates the core boundary:

- the route policy allows a Loyal Hub-shaped swap instruction
- the adversarial Hub program takes the full input amount from the vault-owned
  input token account
- it returns only dust output
- the surrounding route can still deposit that dust and leave the real route
  value in Hub inventory

That is not specific to all-in-one routing. All-in-one is the clearest
demonstration because withdraw, swap, and deposit happen in one policy execution.
The same trust boundary applies to any route topology that includes a direct
Loyal Hub swap lane. Routes that do not invoke Loyal Hub are outside this
specific adversarial-Hub risk.

## Squads Hook Surface

In the Squads `policies` branch, a ProgramInteraction policy can invoke a
pre-hook before the smart-account message and a post-hook after it. When
`pass_inner_instructions = true`, Squads appends the serialized
`SmartAccountCompiledInstruction` list and the transaction accounts to the hook
call.

Those are the smart-account message instructions. They are not Solana runtime
CPI logs from inside Loyal Hub. For this mitigation, that is enough: the hook
does not need Hub CPI transfer logs because the route value is statically tied
to the Kamino withdraw amount, Hub swap arguments, and Kamino deposit amount.

## Static Route Invariant

The guarded route is:

```text
Vault 1 Kamino collateral A
  -> withdraw W of token A into Vault 1 token A account
  -> Hub swap W token A for O token B into Vault 1 token B account
  -> deposit D token B into expected Kamino B reserve/collateral destination
```

The hook must enforce:

```text
W == Hub amount_in
D == Hub amount_out
Hub min_out >= D
D_normalized * 10_000 >= W_normalized * (10_000 - allowed_fee_bps)
```

For a 0.1% fee cap:

```text
D_normalized * 1000 >= W_normalized * 999
```

Use `u128` arithmetic for cross-multiplication and normalize by mint decimals.

## Pre-Hook

The pre-hook is intentionally simple and stateless. It checks only the relevant
Vault 1 intermediate liquidity accounts:

- Vault 1 token A account exists
- Vault 1 token A account has mint A
- Vault 1 token A account owner/authority is Vault 1
- Vault 1 token A amount is `0`
- Vault 1 token B account exists
- Vault 1 token B account has mint B
- Vault 1 token B account owner/authority is Vault 1
- Vault 1 token B amount is `0`

No hook-owned state account is allocated. The zero-balance precondition prevents
preexisting token B from subsidizing a later Kamino deposit if Hub returns dust.

## Post-Hook

The post-hook validates the forwarded smart-account instructions and final
intermediate accounts. Require the route instruction list to contain exactly the
expected sequence for this guarded Hub route:

### 1. Kamino Withdraw A

Verify:

- program id is `KAMINO_LEND_PROGRAM_ID`
- data discriminator is Kamino withdraw reserve liquidity
- withdraw amount is `W`
- owner account is Vault 1
- reserve/market accounts are the expected source Kamino A reserve
- liquidity mint is token A
- source collateral is the expected Vault 1 Kamino A collateral account
- destination liquidity is Vault 1 token A account
- token programs and instruction sysvar accounts match the expected values

### 2. Loyal Hub Swap A To B

Verify:

- program id is Loyal Hub
- data tag is `swap_exact_in`
- user vault is Vault 1
- user input is Vault 1 token A account
- user output is Vault 1 token B account
- input mint is token A
- output mint is token B
- Hub input inventory is the expected token A lane inventory
- Hub output inventory is the expected token B lane inventory
- Hub authority is the expected lane authority
- Hub authorizer is the expected signer
- token programs match the mints
- `amount_in == W`
- `amount_out == O`
- `min_out >= O`
- `max_fee_bps <= allowed_fee_bps`
- `lane_id == expected_lane`

### 3. Kamino Deposit B

Verify:

- program id is `KAMINO_LEND_PROGRAM_ID`
- data discriminator is Kamino deposit reserve liquidity
- deposit amount is `D`
- owner account is Vault 1
- reserve/market accounts are the expected destination Kamino B reserve
- liquidity mint is token B
- source liquidity is Vault 1 token B account
- destination collateral is the expected Vault 1 Kamino B collateral account
- token programs and instruction sysvar accounts match the expected values

### 4. Amount Relationship

Verify:

```text
W == Hub amount_in
D == Hub amount_out
Hub min_out >= D
D_normalized * 10_000 >= W_normalized * (10_000 - allowed_fee_bps)
```

### 5. Final Intermediate Account State

Prefer checking that the Vault 1 token A and token B accounts are still valid
token accounts with amount `0` after route execution. This catches leftover
intermediate liquidity and some token-account authority/close tricks without
state allocation.

This final zero-balance check depends on the actual allowed policy instruction
set. Before making it mandatory, verify who is allowed to create, initialize,
close, or reassign Vault-owned token accounts/ATAs in the same policy execution.
If Vault ATA lifecycle instructions are allowed elsewhere, the hook must either
reject those instructions in this route bundle or explicitly model the allowed
ATA lifecycle.

## Policy Shape

The delegated policy should still allow the direct top-level route instructions,
but only with the mandatory pre-hook and post-hook attached:

```text
ProgramInteractionPolicy {
  account_index = 1
  instructions_constraints = [
    Kamino withdraw A constraint,
    Loyal Hub swap A->B constraint,
    Kamino deposit B constraint,
  ]
  pre_hook = Some(Hook {
    program_id = guarded_route_static_hook
    instruction_data = pre_check_zero_intermediates(...)
    pass_inner_instructions = false or true
  })
  post_hook = Some(Hook {
    program_id = guarded_route_static_hook
    instruction_data = post_check_static_route(...)
    pass_inner_instructions = true
  })
  spending_limits = optional extra loss caps
}
```

The pre-hook does not need forwarded instructions if it only checks the two
intermediate token accounts. The post-hook must use
`pass_inner_instructions = true`.

Do not keep a second hookless Hub route policy for the same delegated signer. If
hookless direct Hub invocation remains allowed, the delegate can bypass the
static route postcondition.

## Why This Blocks The Adversarial Hub Drain

If Hub keeps token A and returns dust, then the required Kamino B deposit cannot
succeed because:

- Vault 1 token B started at zero
- the post-hook requires the route to deposit `D` token B
- `D` must equal Hub `amount_out`
- `D` must satisfy the normalized fee floor versus withdrawn `W`

The adversarial route that sets Hub output and Kamino deposit to dust fails the
post-hook amount relationship. A route that claims a large deposit but receives
dust from Hub fails at Kamino deposit execution because Vault 1 token B has
insufficient balance.

## Replay And Budget Controls

A per-call static route guard limits one execution, but reusable delegation can
still repeat valid small routes. Add one of:

- quote-specific policy creation/removal
- deadline encoded in hook instruction data and enforced by the hook
- Squads spending limits as a second line of defense
- active-lane/rebalance preflight in the off-chain executor

For live funds, prefer quote-specific or short-lived policies first. Reusable
guarded policies should come after spending limits and ATA lifecycle assumptions
are verified.

## Interim Mitigation

If hooks are not available immediately, tighten the direct Hub policy by adding
data constraints on Hub and Kamino instruction data:

- Kamino withdraw amount equals expected `W`
- Hub `amount_in == W`
- Hub `amount_out >= min_authorized_out`
- Hub `min_out >= min_authorized_out`
- Hub `max_fee_bps <= allowed_fee_bps`
- Kamino deposit amount equals expected `D`
- `D` satisfies the normalized fee floor versus `W`
- exact lane, mint, vault, reserve, and token-account constraints

This is weaker than a post-hook because ProgramInteraction constraints do not
compute cross-instruction arithmetic. Treat it as a temporary blast-radius limit
unless the policy is quote-specific with exact values.

## Test Plan

Add focused LiteSVM coverage around the existing adversarial Hub fixture:

- guarded policy is initialized on Vault 1, not Vault 0
- pre-hook rejects nonzero Vault 1 token A or token B intermediate balances
- post-hook receives the forwarded smart-account route instructions
- post-hook accepts the honest withdraw A -> Hub swap -> deposit B route
- post-hook rejects dust Hub output and dust Kamino deposit
- post-hook rejects `W`, Hub `amount_in`, Hub `amount_out`, and `D` mismatches
- post-hook rejects wrong Kamino reserve, collateral, or liquidity accounts
- post-hook rejects wrong Hub inventory, lane, mint, or token program accounts
- final zero-balance check is validated against the actual allowed policy
  instruction set, including any Vault ATA create/close paths
- hookless direct Hub policy remains available only behind an explicit
  trusted-Hub mode, if still needed

Relevant local commands:

```sh
cargo build-sbf -- -p mock-yield-protocols-program -p loyal-hub-swap-program
cargo test -p squads-test-harness --test yield_route_policy_adversarial -- --nocapture
bun run test:squads
```

## Rollout Plan

1. Add the immutable guarded static-route hook program.
2. Add hook payload builders in `loyal-actions` for Vault 1 zero-balance
   pre-checks and static route post-checks.
3. Extend policy builders to attach mandatory hooks for Loyal Hub route policies.
4. Verify actual allowed policy instructions around Vault ATA creation/closure.
5. Add adversarial-Hub regression tests.
6. Validate with the focused Squads harness and then the full `bun run
   test:squads` gate.
7. Deploy the hook with immutable or governance-locked upgrade authority before
   using it for live delegated policies.

## Open Questions

- Whether final zero-balance validation should be mandatory for all guarded
  routes depends on the actual policy allowance for Vault ATA lifecycle
  instructions.
- The 0.1% threshold assumes stable-value route mints. If non-stable mints are
  later allowed, the hook must use an approved quote/oracle input instead of a
  fixed normalized amount comparison.
- Token-2022 fee-bearing mints need credited-output checks based on actual token
  account effects and allowed mint configuration, not just nominal transfer
  instruction amounts.
