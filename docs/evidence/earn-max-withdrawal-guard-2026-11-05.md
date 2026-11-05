# Ordinary withdrawal slippage guard — November 5, 2026

This report records pre-release validation. New ordinary Earn MAX withdrawal
sales request and enforce a maximum **20 bps (0.2%) deterioration from
the approved quote**. This is not a maximum total investment loss.

The decision carries its limit into standalone and atomic withdrawal swaps,
Jupiter's quote, the executable instruction validator and the durable JSON
decision record. The compiler checks the encoded slippage and retained output
minimum. Liquidation protection, term repayment and operator exits retain the
existing 50 bps ceiling. Already signed transactions recover their recorded
bytes; a restart does not recompile them under the new limit.

A refusal produces a `price_limit` hold and `withdrawal_price_limit` withdrawal
health reason. The worker reobserves before trying another quote. Waiting beyond
the existing withdrawal deadline does not widen the limit. Available vault cash
continues to cover withdrawals first.

## Verification

| Check | Result |
| --- | --- |
| Existing Backyard baseline | Passed |
| Worker verifier: formatting, vet, all package tests and worker command builds | Passed, with a fresh disposable local PostgreSQL cluster |
| Ordinary/partial withdrawal, cash coverage, residual conversions, term repayment, hard LTV, operator exit and legacy recovery decisions | Passed |
| Reject wider API quotes, wider executable instructions and unenforceable retained minimums | Passed |
| Real standalone and atomic transaction compilation, ephemeral signing and persisted-envelope validation for PRIME, Maple and AUTO | Passed |
| Durable decision reload, older signed 50-bps recovery, hold reporting, later cash coverage and reobserve/retry | Passed |
| PRIME position funded by a $100,000 simulated purchase; exit at 20 bps | Passed: lending position cleared and exit cash restored to vault idle |
| Maple position funded by a $100,000 simulated purchase; exit at 20 bps | Passed: lending position cleared and exit cash restored to vault idle |
| PRIME exit after a preceding sale quoted at about $4.96 million | Rejected by Jupiter error 6001; all preceding transactions succeeded and all **55 tracked principal accounts were unchanged** by the failed withdrawal |

The funded cycles invoke production `assembleStep`, quote preparation and
transaction compilers. Each normal cycle validates six financial transactions
through ephemeral signing and the persisted-envelope check. RPC bundle
simulation uses unsigned wires with signature verification disabled; no live
policy key is loaded and no transaction is broadcast.

The first Maple attempt failed in the diagnostic signing check because the
harness paired a newly quoted transaction with an earlier blockhash. The harness
now passes each transaction's own blockhash and validity height. The corrected
Maple cycle passed. This was a test setup correction, not a production change.

The latest fresh live-vault observation returned **HOLD / at_target**, without
proposing a sale, leverage reset or rotation. Earlier standalone captures had
returned `receipt_nav_mismatch`; the live service remained active, reported no
manual hold and had zero restarts since startup. Repeat this continuity check
immediately before any actual rollout.

## Boundaries

Funding above $1,000 is an unsigned hypothetical transfer inside the simulated
bank. Only guaranteed buy output is deposited; buy surplus remains outside the
lending position. These are funded position-exit tests, not proof that every
cent of a $100,000 user withdrawal was liquidated. The candidate positions have
no debt. Maple uses the experimental Whirlpool filter; its production Manifest
filter is unchanged.

The stressed experiment proves atomic rejection and rollback. Later acceptable
quotes and legacy recovery are covered by worker tests; it does not prove that
stressed market liquidity will recover within a particular time. The limit can
make withdrawals wait longer. Pacing, an overall economic cost ceiling and
Maple/PRIME diversification remain separate work.

Sanitized amounts, slots, source/capture hashes and checks are in
[the evidence JSON](earn-max-withdrawal-guard-2026-11-05.json). Private raw captures
and verification logs are under `/tmp/earn-max-withdrawal-guard-20261105`.
Remote diagnostic directories were removed and the disposable local PostgreSQL
cluster was stopped.
