# EVM bridge: state, next step, open items

Status as of 2026-11-05. The release plan, wiring plans, decision trail and Figma exports live outside the repo in `~/loyal/artifacts/loyal-bridge/docs/` (`plan.md`, `plan-incoming-transfers.md`, `trail.md`, `incoming-decisions.tsv`, `deploy-checklist.md`, `support-matrix.md`). This file is the program's handoff.

## What is live

- EVM to Solana deposits in `loyal-web`. A user whose Solana wallet is a Privy embedded wallet gets the Privy auto-bridge deposit address. An external wallet (Phantom) gets a Privy embedded EVM wallet and signs a Relay bridge itself, gas sponsored by Privy, with no Privy policies. The route kinds are in `loyal_yield.evm_deposit_routes`.
- Bridge Out, Tron deposits and the Move flow ship in the same web release. Out quotes carry Loyal's 40 bps as a Relay app fee.
- Launched 2026-11-04 21:53 PST (#697). The web host drop-in `86-launch.conf` sets `LOYAL_TEST_WALLETS=*`, which opens Bridge, Other chains and referrals to every signed-in wallet. To roll back, remove the drop-in and restart `loyal-web`. The gate is `go/workers/internal/webapp/test_wallets.go`.
- Relay deposits are `depositErc20(depositor,token,amount,id)`, selector `e8017952`, and `depositNative(depositor,id)`, selector `49290c1c`. The `id` is not the Relay request id. `go/workers/internal/bridge/quote.go:92` pins the selector and `checkDeposit` (`quote.go:337`) refuses any other calldata.
- A Privy deposit account rejects a key-owned destination ("must be a 1-of-1 owned by the authenticated user"), so the auto-bridge can only be proved with a real signed-in user. This was observed on 2026-11-02 with a key-owned test wallet.

## Incoming transfers (shipped 2026-11-05)

A `relay_signed` user's deposit waits on their Privy embedded EVM wallet until they sign a Move. Two changes make that visible:

- The "Confirm incoming transfer" card and the sidebar badge on Assets (Figma node 6446:104953, #734) went live on `loyal-web` `10b74b704` at 23:25Z. Confirm runs the Move in `use-bridge-move.ts`. The Bridge screen Move row is gone. The server refuses a second in-flight incoming move for the same EVM sender, chain and asset (409 `bridge_in_flight`, `funding_bridge.go`). `POST /api/funding/bridge/transfers/{id}/release` stamps `released_at` (migration `0138`) when the deposit was never broadcast; the tracker still adopts released rows.
- The unmoved-funds alert (#733, `go/workers/internal/bridge/unmoved.go`) went live on retail `bdb8b71ab` at 23:18Z. It publishes `loyal_bridge_incoming_unmoved{address,chain,asset,under_min}` every 10 minutes, and Prometheus holds the 30-minute clock (`LoyalBridgeIncomingUnmoved`, `Stranded`, `ScanStale`, `ScanFailing`). It runs on the `privy-gas-spend.conf` credentials, which are the website's Privy app.

Deploy notes from this release:
- The host's rules record named a pre-squash branch sha, so `deploy retail` skipped the rules. They were installed by hand with `install-rules.sh`. Fixed on branch `fix/rules-by-content`: the deploy now places the host rules by file content, with no record.
- The host's `alertmanager.yml` had drifted from the repo since October and was reinstalled.
- After the 2026-11-05 15:46 PST DB switch, yield migrations go to Hetzner (10.92.1.1) only. Logical replication does not carry DDL, so ask the Hetzner migration coordinator before any yield or app DDL.

The release exists because a teammate's 25 USDT on Ethereum sat unseen on `relay_signed` address `0xbd03…8179` from 2026-11-05 18:29:59Z. It was still unmoved at ship time; they confirm it from the card. Decision trail and deploy evidence: `~/loyal/artifacts/loyal-bridge/docs/incoming-decisions.tsv` and `~/loyal/artifacts/loyal-bridge/evidence/deploy-incoming/`.

## Open

- The card with real funds is unproven on prod. The signed-in prod check (wallet `BAqg…`, zero balance) proved discovery and the empty state only. The first real Confirm, the teammate's or a $1 Base test, closes it.
- The card has no fee or receive-amount line before signing. The designer has not drawn one yet.

- The auto-bridge for embedded wallets is untested end to end. The owner waived the live proof on 2026-11-02.
- Email-first sign-up may make the same double Privy call that caused the 429 fixed in #687 (`privy-session-sync.tsx`). The owner skipped the test.
- Privy gas sponsorship runs on fee credits with no limits. There is no low-balance alert on the credits.
- The observer takes about 30 s to start while its seed takes 22 to 29 s.
- Findings from the 2026-11-02 audit that were not rechecked: `transfer.go` treats a Relay failure as terminal, a `relay_signed` row can be stored for an embedded wallet when the Privy lookup is empty, concurrent POSTs create several Privy accounts, and `journeys.spec.ts` has no bridge case.
- Tron M1 to M3 operator proofs and Bridge Out BO1 to BO5 money lanes were never run with real money.
