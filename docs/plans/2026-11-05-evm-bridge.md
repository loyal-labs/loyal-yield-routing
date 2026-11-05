# EVM bridge: state, next step, open items

Status as of 2026-11-05. The release plan, wiring plans, decision trail and Figma exports live outside the repo in `~/loyal/artifacts/loyal-bridge/docs/` (`plan.md`, `plan-incoming-transfers.md`, `trail.md`, `incoming-decisions.tsv`, `deploy-checklist.md`, `support-matrix.md`). This file is the program's handoff.

## What is live

- EVM to Solana deposits in `loyal-web`. A user whose Solana wallet is a Privy embedded wallet gets the Privy auto-bridge deposit address. An external wallet (Phantom) gets a Privy embedded EVM wallet and signs a Relay bridge itself, gas sponsored by Privy, with no Privy policies. The route kinds are in `loyal_yield.evm_deposit_routes`.
- Bridge Out, Tron deposits and the Move flow ship in the same web release. Out quotes carry Loyal's 40 bps as a Relay app fee.
- Launched 2026-11-04 21:53 PST (#697). The web host drop-in `86-launch.conf` sets `LOYAL_TEST_WALLETS=*`, which opens Bridge, Other chains and referrals to every signed-in wallet. To roll back, remove the drop-in and restart `loyal-web`. The gate is `go/workers/internal/webapp/test_wallets.go`.
- Relay deposits are `depositErc20(depositor,token,amount,id)`, selector `e8017952`, and `depositNative(depositor,id)`, selector `49290c1c`. The `id` is not the Relay request id. `go/workers/internal/bridge/quote.go:92` pins the selector and `checkDeposit` (`quote.go:337`) refuses any other calldata.
- A Privy deposit account rejects a key-owned destination ("must be a 1-of-1 owned by the authenticated user"), so the auto-bridge can only be proved with a real signed-in user. This was observed on 2026-11-02 with a key-owned test wallet.

## Next step

PR #734 (`feat/web-incoming-transfer-card`, IN2) is merge-ready and waits for the owner's review. It adds the "Confirm incoming transfer" card and the sidebar badge from Figma node 6446:104953 and runs the Move that already exists. It carries migration `0138` (`released_at`). Apply `0138` before deploying web, because the deploy script does not gate web on migrations. IN1 (the unmoved-funds alert) is merged as #733 (`go/workers/internal/bridge/unmoved.go`). Its deploy needs `privy-gas-spend.conf` on the same Privy app and SELECT on `evm_deposit_routes`; Prometheus holds the 30 min clock on a per-address gauge. As of 2026-11-05 evening nothing from #733 or #734 is deployed and the 25 USDT is still unmoved. Decision trail: `~/loyal/artifacts/loyal-bridge/docs/incoming-decisions.tsv`.

The card exists because a teammate's 25 USDT on Ethereum has sat on a `relay_signed` address since 2026-11-05 18:29:59Z. Move is user-pressed and only on the Bridge screen, and no EVM watcher tells the owner.

## Open

- The auto-bridge for embedded wallets is untested end to end. The owner waived the live proof on 2026-11-02.
- Email-first sign-up may make the same double Privy call that caused the 429 fixed in #687 (`privy-session-sync.tsx`). The owner skipped the test.
- Privy gas sponsorship runs on fee credits with no limits. There is no low-balance alert on the credits.
- The observer takes about 30 s to start while its seed takes 22 to 29 s.
- Findings from the 2026-11-02 audit that were not rechecked: `transfer.go` treats a Relay failure as terminal, a `relay_signed` row can be stored for an embedded wallet when the Privy lookup is empty, concurrent POSTs create several Privy accounts, and `journeys.spec.ts` has no bridge case.
- Tron M1 to M3 operator proofs and Bridge Out BO1 to BO5 money lanes were never run with real money.
