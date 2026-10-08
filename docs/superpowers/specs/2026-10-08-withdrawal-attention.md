# Earn MAX withdrawal consent, status, and alerting — design draft

## Approved product rule
Valid user withdrawal requests may authorize the debt repayment necessary to return their funds. Automatic full repayment requires proof of necessity; an optimizer preference, failed planner, percentage, buffer, iteration count, or tiny-withdrawal fallback is not proof. Preserve ordinary full-exit confirmation and verified liquidation-risk handling.

## Source findings that constrain implementation
- Current partial planner declines cases at 90% demand, less than $50 remaining, six rounds, target1x and other heuristics. These are not protocol necessity constraints. Withdrawal fallthrough must never authorize flattening the position.
- Existing withdrawal demand sums rounded-up receipt upper bounds. Full-exit authority cannot use these as exact payable liabilities; actual payout depends on current redemption value and fees.
- Existing partial sizing/quotes show a candidate can work. They do not establish an upper bound on all safe debt-preserving withdrawals. No general full-repayment necessity proof currently exists in the supported cross-mint flow.
- Worker pre-send evidence cannot guarantee receipt conditions still hold at chain execution. Atomic enforcement requires transaction/program support, separately reviewed for existing policy and transaction limits.

## Conservative delivery boundary
Implement safe partial-path selection and explicit operator-attention fallback without broadening full-debt authority. Do not ship a speculative proof-success branch. Enable automatic necessary full repayment only after a narrow proof model and its enforcement boundary are approved and implemented. Unknown remains pending, not permission.

## Frontend
Keep existing visual layout. Disable top, RWA Loop and equivalent responsive Withdraw entrypoints for pending withdrawal; hover/focus/touch explanation: “Finish your current withdrawal before requesting another.” Keep Claim/Check status usable. Guard navigation/prepare races as well as buttons.
Fresh operator-attention evidence gives distinct delayed status with no ETA or unsupported “team notified” claim. Normal cooldown/liquidity waits remain normal. Stale status is unavailable, never recovered/paid. Chain receipt/liquidity remains sole claimability authority.

## Worker health and alerts
Persist minimal display-only withdrawal health using existing lease-fenced route-state projection; never change money authority or duplicate recovery latches. Known intervention blockers escalate; persistent unfunded/no-funding-progress proposes15m threshold. Persist onset across restart; NAV reports alone do not reset funding progress. Clear only on fresh evidence. Map the actual canonical Backyard route/cluster before exposing allowlisted status via authenticated existing summary.
Classify exact typed selector_finish_current_work_first as expected selector deferral before sanitization, not failure. Keep unknown selector_evaluate_unavailable failures alertable; retain safe reviewed cause/stage codes. Keep genuine terminal/post-send and stale-work alerts.
Use stable route/family attention gauges and freshness coverage; deduplicated actionable attention alert. Improve Telegram template (escaped summaries, impact, next action and approved runbook link), omit inaccessible GeneratorURL. Fractional increase is Prometheus extrapolation; improve wording, not threshold math.

## Validation
Regression: $5 cannot acquire full-exit authority; 90% alone cannot; partial failure/stale or rounded-up demand cannot. Check aggregate requests, claims/cancellations, fee/NAV changes, funding reuse, cumulative repayment and signed recovery. Check busy selector does not increment failed counter while unknown/real failures still do. Alert-rule firing/resolution/freshness and template checks. Scoped frontend lint/typecheck, actual disabled-button interactions and claim/status paths; Vercel build, no local frontend build.

## Approved staged delivery
User approved: ship the independent UI/attention/noise fixes and safe partial safeguards first, keeping automatic full clearing disabled until necessity proof and pre-send-versus-atomic boundary are reviewed. This is a staged delivery, not a claim that automatic necessary full repayment is implemented.
