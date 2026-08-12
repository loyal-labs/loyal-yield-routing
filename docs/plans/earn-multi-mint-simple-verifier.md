# Earn Multi-Mint Simple End-State Verifier

Status: frozen before implementation on 2026-08-11.

Amendment on 2026-08-11: V1's phrase "no policy creates" was corrected to
"a new policy creates." The former contradicted the stated requirement that
new users receive the six-mint-capable policy shape; no verification criterion
was relaxed.

Amendment on 2026-08-11: V7's Ultracite result is graded against the frozen
base for pre-existing files, because the same 34 files already produce 871
whole-file errors at `e26f2d18632825a86007789665f9abed38f1ce0d`. A changed-file
zero-error requirement would therefore grade old code as this implementation.
All newly added frontend files must be clean, and the identical pre-existing
file set must not increase its diagnostic count. This changes only the lint
measurement, not any product or safety criterion.

Run this document cold against these worktrees:

- `APPS_REPO=/private/tmp/loyal-apps-earn-multi-mint-simple`
- `ROUTING_REPO=/private/tmp/loyal-yield-routing-earn-multi-mint-simple`

The product supports exactly CASH, USDG, PYUSD, USDC, USDT, and USDS in one
Earn vault and one shared policy lifecycle. Deposits, withdrawals, idle
deposits, and reserve moves are same-mint. Autodeposit remains USDC-only.
Existing policies are never mutated automatically.

The verifier is adversarial: grade observable behavior, not implementation
summaries. A skipped required check is a failure. No on-chain write or mainnet
broadcast is part of verification.

## V0 — Scope and architecture

Required:

- Both worktrees start from their recorded current-main commits and contain no
  unrelated feature, deployment, schema, cross-mint, or autodeposit changes.
- There is one canonical asset registry mapping each supported mint to symbol,
  decimals, and token program. CASH/USDG/PYUSD are Token-2022;
  USDC/USDT/USDS are classic SPL Token.
- Public deposit intent is `{ mint, amountRaw }`. Token program, symbol,
  decimals, and reserve are derived by trusted code rather than supplied by the
  browser.
- Public withdrawal intent is `{ sourceId, amountRaw | "max" }`. It contains no
  caller-selected mint, reserve fallback, destination, full-exit source list,
  symbol, decimals, or token program.
- Current balance truth is one complete `EarnSnapshot`/holdings vector. There
  is no separate singular "primary position" used to decide money/no-money or
  portfolio totals.
- No worker capability is encoded by changing queue availability timestamps or
  by magic JSON fields. A rollout gate may control publication only.

## V1 — Deposit identity and policy capability

For all six product mints, prove a deposit intent reaches preparation with the
same mint and a same-mint Safe Kamino reserve. The trusted registry must supply
the correct token program, and reserve decoding must confirm reserve owner,
mint, and declared token program before transaction construction.

Required negative controls:

- unsupported mint fails before construction;
- wrong reserve mint/program fails before construction;
- a legacy policy accepts USDC/USDT/USDS but CASH/USDG/PYUSD returns HTTP 409
  `earn_policy_update_required` before any policy or deposit transaction is
  built;
- a new policy creates the six-mint-capable policy shape;
- no path updates an existing policy automatically.

Enumerate checked-in web/mobile/example callers. Each must send an explicit
mint, or remain an explicitly isolated USDC-only autodeposit caller.

## V2 — One complete holdings snapshot

One pure chain reader must produce a snapshot containing:

- `observedSlot` and completeness/freshness;
- every positive or zero policy-authorized Kamino reserve holding;
- one directly-derived idle ATA for each supported policy mint, using that
  mint's token program.

API reads, reconciliation, withdrawal verification, and final zero proof must
consume this reader or the same pure decoding primitive; duplicated independent
market/mint/ATA discovery algorithms fail V2.

Required scenarios:

- USDC reserve + idle PYUSD returns both sources;
- two reserves of one mint remain two holdings;
- RPC failure/incomplete coverage is unknown, never zero;
- product state is no policy / complete zero / any positive holding;
- raw balances of different mints are never assigned to one holding or summed
  into a token-denominated field. Nominal stablecoin-par totals, when needed,
  are explicitly named as such.

## V3 — Exact withdrawal and cleanup

The backend must re-read a fresh snapshot and locate exactly one holding by
`sourceId`. Missing, duplicate, stale, or changed IDs fail; it must never guess
by mint, amount, reserve fallback, or array position.

Required scenarios:

- reserve A=100 and reserve B=50; withdraw 20 from A -> A=80, B=50;
- idle PYUSD remains selectable while a USDC reserve exists;
- Max drains only the selected source;
- draining PYUSD while USDC remains keeps the policy active;
- idle withdrawal transfers that mint directly to its wallet ATA and does not
  require a synthetic Kamino target;
- cleanup is offered only after a fresh exact-zero snapshot across all
  authorized reserve holdings and idle ATAs;
- cleanup closes each zero account with its actual token program. There is no
  cross-mint dust aggregate treated as USDC.

## V4 — Frontend wiring

Vlad's existing selector, position rows, and withdrawal navigation are reused.

Required:

- the deposit selector shows the registry products supported by the active
  cluster, wallet public balances, and the shared stablecoin icon resolver;
- changing the selection changes the mint sent to prefetch and submit and
  invalidates stale preparation;
- active rows render every holding with mint and market identity;
- clicking a row opens Withdraw with that exact `sourceId` selected;
- destination/CTA copy is derived from the selected holding's asset;
- `earn_policy_update_required` is a blocking update prompt, not a generic
  failure or silent transaction;
- autodeposit UI and execution remain USDC-only.

## V5 — Earnings and APY

Principal is confirmed external deposits minus confirmed external withdrawals,
keyed by mint. Live holdings are separate current exposure.

Required:

- deposit 100, live balance 101 -> principal stays 100;
- concurrent reserve exposure is preserved by source ID, not collapsed by mint;
- current portfolio APY is nominal-value weighted by reserve holding; idle
  contributes zero;
- earned history aggregates per-source/per-mint intervals into one chart series;
- missing or stale APY coverage is marked unavailable/stale rather than filled
  from an unrelated primary position.

## V6 — Routing

For each of the six assets, prove idle observation -> planning -> revalidation
-> transaction construction -> confirmation reconciliation preserves one mint
and one token program. Source mint must equal target mint. ATA derivation uses
the observed asset's token program.

Required:

- one `EarnUniverse`/asset registry defines observation completeness;
- idle ATAs are derived directly from vault + asset, not discovered through
  reserve rows;
- complete publication writes reserve positions and the full idle set under one
  observation generation, while bounded patch publication cannot delete unseen
  holdings;
- APIs are explicitly named `publish_complete_vault` and
  `apply_observed_patch` (or equivalently unambiguous names); no generic method
  silently changes between replacement and patch semantics;
- rollout order is upgraded readers/executor first, then enable non-USDC
  publication. No timestamp/JSON capability fence exists;
- historical `idle_vault_usdc` and `idle_vault_deposit` wire values remain only
  at serialization/persistence boundaries.

## V7 — Executable verification

Keep tests only for money movement, policy/wire compatibility, source identity,
snapshot completeness, accounting, and atomic publication.

From `APPS_REPO`, run focused tests for V1-V5, then:

```sh
bun run --cwd packages/smart-account-vaults typecheck
bun run --cwd frontend ultracite --changed
```

If the checked-in script does not accept `--changed`, run Ultracite only on the
touched files. For pre-existing files, compare the identical file set with the
frozen base as described above; report both counts. Do not run a local frontend
production build.

From `ROUTING_REPO`, run:

```sh
cargo fmt --check --manifest-path crates/loyal-yield-orchestrator/Cargo.toml
cargo check -p loyal-yield-orchestrator
cargo test -p loyal-yield-orchestrator
```

Also inspect `git diff --check`, `git status --short`, and the complete scoped
diff in both repositories.

## Verdict

Report V0-V7 individually as PASS or FAIL with command output and exact file
evidence. Overall PASS requires every required condition. Do not modify this
verifier to accommodate the implementation unless it demonstrably misstates
the product contract; document any such amendment before further work.
