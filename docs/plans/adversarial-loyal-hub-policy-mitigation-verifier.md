# Verifier: Adversarial Loyal Hub Policy Mitigation

Run this verifier from the repository root against an implementation of
`docs/plans/adversarial-loyal-hub-policy-mitigation.md`.

Do not grade from summaries or commit messages. Read the source, run the focused
LiteSVM tests, and mark each required condition PASS or FAIL. Overall verdict is
PASS only if every required condition passes.

## Required Conditions

### 1. Immutable Hook Program Exists

PASS only if the repo contains a small Solana SBF program, implemented with
Pinocchio, that is intended to be used as the guarded Loyal Hub route hook.

The hook program must:

- expose distinct pre-hook and post-hook instruction modes, tags, or handlers
- allocate no hook-owned state account
- store no route snapshots
- only read accounts plus hook instruction data / forwarded Squads route data
- fail closed on malformed input, missing accounts, bad token account layout, or
  unsupported token programs

FAIL if the mitigation is only documentation, only off-chain code, or only direct
Squads data constraints on the existing Loyal Hub policy.

### 2. Pre-Hook Enforces Zero Vault 1 Intermediates

PASS only if the pre-hook verifies both Vault 1 intermediate liquidity accounts
before the route executes:

- token A account exists
- token A account mint equals the expected token A mint
- token A account authority/owner equals Vault 1
- token A account amount is exactly `0`
- token B account exists
- token B account mint equals the expected token B mint
- token B account authority/owner equals Vault 1
- token B account amount is exactly `0`

FAIL if the pre-hook only checks account pubkeys, only checks mints, only checks
one intermediate account, or permits a nonzero token B balance that could
subsidize a dust Hub output.

### 3. Post-Hook Verifies Forwarded Smart-Account Route

PASS only if the post-hook consumes the forwarded Squads smart-account message
instructions and transaction accounts, not runtime CPI logs, and verifies the
route shape exactly.

It must reject missing, extra, or reordered top-level route instructions. The
accepted route must be exactly:

1. Kamino withdraw token A
2. Loyal Hub swap token A to token B
3. Kamino deposit token B

The post-hook must validate the withdraw instruction:

- program id is `KAMINO_LEND_PROGRAM_ID`
- data discriminator is Kamino withdraw reserve liquidity
- withdraw amount is `W`
- owner account is Vault 1
- source reserve/market accounts are the expected token A Kamino accounts
- liquidity mint is token A
- source collateral is the expected Vault 1 token A collateral account
- destination liquidity is Vault 1 token A account
- token program and instruction sysvar accounts match the expected accounts

The post-hook must validate the Hub swap instruction:

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

The post-hook must validate the deposit instruction:

- program id is `KAMINO_LEND_PROGRAM_ID`
- data discriminator is Kamino deposit reserve liquidity
- deposit amount is `D`
- owner account is Vault 1
- destination reserve/market accounts are the expected token B Kamino accounts
- liquidity mint is token B
- source liquidity is Vault 1 token B account
- destination collateral is the expected Vault 1 token B collateral account
- token program and instruction sysvar accounts match the expected accounts

FAIL if the post-hook only checks the Hub instruction, only checks the policy
account, trusts a client-supplied `W`/`D` without matching instruction data, or
does not require `pass_inner_instructions = true` on the post-hook policy.

### 4. Post-Hook Enforces Normalized Fee-Floor Math

PASS only if the post-hook enforces the cross-instruction amount relationship
with `u128` arithmetic:

```text
W == Hub amount_in
D == Hub amount_out
Hub min_out >= D
D_normalized * 10_000 >= W_normalized * (10_000 - allowed_fee_bps)
```

`D_normalized` and `W_normalized` must account for the token A and token B mint
decimals. The implementation may use a fixed stable-route fee floor or a stricter
quote-specific floor, but it must not accept a dust output/deposit for a full
withdrawal.

FAIL if the math is performed with lossy floating point, with overflow-prone
`u64` multiplication, without decimal normalization, or only inside the
adversarial mock rather than the hook.

### 5. Policy Builder Makes Hooks Mandatory For Hub Routes

PASS only if the Loyal Hub route policy builder creates or updates the User
Vault / Vault 1 ProgramInteraction policy with:

- a pre-hook pointing at the guarded route hook program
- a post-hook pointing at the guarded route hook program
- `post_hook.pass_inner_instructions == true`
- the route policy account index set for Vault 1, not Vault 0
- no hookless direct Loyal Hub policy for the same delegated signer, unless it is
  behind an explicit trusted-Hub mode that tests show is not used by the
  adversarial-guarded route path

FAIL if a delegated signer can still execute the same Vault 1
withdraw -> direct Loyal Hub swap -> deposit route through a hookless policy.

### 6. Final Intermediate Account Handling Is Safe

PASS only if tests and/or source prove one of these is true:

- the post-hook requires Vault 1 token A and token B intermediate accounts to
  still be valid token accounts with amount `0` after route execution, and the
  route policy rejects create/close/reassign instructions that could evade this;
  or
- the implementation explicitly models every allowed Vault ATA lifecycle
  instruction in the guarded route policy and proves it cannot bypass the
  zero-balance invariant.

FAIL if final zero-balance checks are omitted while the same policy can create,
close, reassign, or replace Vault-owned token accounts.

### 7. LiteSVM Tests Prove Honest And Adversarial Behavior

PASS only if focused LiteSVM tests cover all of the following:

- guarded policy is initialized for Vault 1, not Vault 0
- pre-hook rejects nonzero Vault 1 token A balance
- pre-hook rejects nonzero Vault 1 token B balance
- post-hook receives forwarded smart-account route instructions
- honest withdraw A -> Loyal Hub swap A to B -> deposit B route succeeds
- adversarial Loyal Hub dust-output route fails and leaves user vault funds safe
- post-hook rejects mismatched `W`, Hub `amount_in`, Hub `amount_out`, and `D`
- post-hook rejects wrong Kamino reserve, collateral, or liquidity accounts
- post-hook rejects wrong Hub inventory, lane, mint, authorizer, authority, or
  token program accounts
- hookless direct Hub execution is unavailable for the guarded delegated signer,
  or only available in an explicit trusted-Hub mode not used by the guarded route

FAIL if tests only call hook handlers directly without executing through the
Squads LiteSVM ProgramInteraction policy, or if they do not use
`MockProgram::AdversarialLoyalHubSwap` or an equivalent adversarial Hub program.

### 8. Required Commands Pass

Identify the hook program crate name from `Cargo.toml` / workspace metadata, then
run the relevant SBF build and focused Squads harness tests. At minimum, run:

```sh
cargo build-sbf -- -p mock-yield-protocols-program -p loyal-hub-swap-program -p <hook-program-crate>
cargo test -p squads-test-harness --test yield_route_policy_adversarial -- --nocapture
bun run test:squads
```

If the guarded-hook tests live in a different Squads harness test file, run that
test file too and include the exact command in the verdict.

FAIL if any command fails, if the adversarial guarded-route tests are skipped, or
if the tests require plaintext secrets or network access.

## Nice-To-Have Checks

- The hook uses generated ABI/schema constants for Loyal Hub and stable local
  constants for Kamino account/data offsets instead of unexplained magic numbers.
- Hook payload builders live in `crates/loyal-actions` and expose a narrow public
  API rather than leaking broad internal Squads wire types.
- The plan doc is updated with the final crate names, test names, and any
  remaining assumptions about ATA lifecycle instructions.

## Verdict Format

Return:

```text
Overall: PASS | FAIL

Required:
1. Immutable Hook Program Exists: PASS | FAIL - evidence
2. Pre-Hook Enforces Zero Vault 1 Intermediates: PASS | FAIL - evidence
3. Post-Hook Verifies Forwarded Smart-Account Route: PASS | FAIL - evidence
4. Post-Hook Enforces Normalized Fee-Floor Math: PASS | FAIL - evidence
5. Policy Builder Makes Hooks Mandatory For Hub Routes: PASS | FAIL - evidence
6. Final Intermediate Account Handling Is Safe: PASS | FAIL - evidence
7. LiteSVM Tests Prove Honest And Adversarial Behavior: PASS | FAIL - evidence
8. Required Commands Pass: PASS | FAIL - evidence

Nice-to-have:
- ...

Blocking failures:
- ...
```
