# Bridge UI handoff

Bridge In is live: `ReceiveSheet` renders the Figma V2 design on server data in every environment, behind the `loyal.bridge` browser opt-in. Bridge Out runs on server data: chains and tokens from `/api/funding/bridge/destinations`, recently used recipients from `/api/funding/bridge/out/recipients`, and Relay quotes from `/api/funding/bridge/out/quote`. Send now simulates the deposit, confirms, prompts the wallet, submits the signature, and polls `/api/funding/bridge/transfers?direction=out`. The sidebar Bridge action is enabled in development or with the `loyal.bridge` browser opt-in, and disabled otherwise.

## Entry points

All component paths below are relative to `src/components/wallet-workspace/facelift/`.

| Flow | Entry | Owning files |
| --- | --- | --- |
| Bridge In | Receive → Other chains → deposit asset → source chain; Asset to receive is a fixed USDC on Solana cell with a tooltip | `receive-sheet.tsx`, `use-evm-deposit-address.ts` |
| Bridge Out | Sidebar Bridge → send asset → receive asset → destination chain → recipient | `bridge-workspace.tsx`, `bridge-recipient-pane.tsx`, `use-bridge-out.ts` |
| Navigation and motion | Shared Assets action stack, retained entry/exit, selector sheets, token/text changes | `crypto-page.tsx`, `shell.tsx`, `sidebar.tsx`, `token-icon-swap.tsx` |

Bridge Out shares the existing `MiddlePaneSlide`, `PaneReveal`, `SheetReveal`, and `TextSwap` behaviors with Send/Swap. Token icon transitions were extracted unchanged from Swap for reuse. Search remains keyboard accessible and has no red focus ring.

## Backend integration boundaries

- Bridge In takes its assets, chains, addresses, minimums and fees from `GET /api/funding/evm-deposit-address`, and a row whose amount the server omits stays hidden. There is no receive picker because every route lands as USDC on Solana, and no deposit time because deposit responses carry none.
- Recipient validation currently checks address format only. Replace it with the supported destination-chain validation. Keep recipient confirmation scoped to the selected chain: changing Ethereum to Arbitrum clears the address, quote visibility, and send-ready state even though both accept the same address format; switching back must not restore the old confirmation. Draft/error state resets with the chain too.
- Treat production enablement as a separate release decision.

## Design references

- Bridge In: [main](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72958), [deposit asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72826), [source chain](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72884), [address](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-75617).
- Bridge Out: [main](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71417), [send asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72228), [receive asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6360-75201), [destination chain](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6360-75254).
- Recipients: [empty](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71153), [pasted](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72165), [typing](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72198), [invalid](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72118), [ready](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71737).

## Preview and verification

Use the real Vite entrypoint, `web/apps/loyal`, with `NODE_ENV=development` and the normal local environment. The current review preview is [loyal-dev.arbatov.dev/app](https://loyal-dev.arbatov.dev/app).

The browser smoke checks for Bridge Out cover the entry path, asset-first selectors, search/back navigation, address typing/paste/invalid/exact-match states, Ethereum ↔ Arbitrum reset, desktop/mobile layout, keyboard focus, shared motion, reduced motion, and rapid close/reopen. Checks block transaction requests and use a fixture session; real wallet signing and transfers are not exercised. Local validation uses targeted lint and scoped TypeScript checks; no production build is required for this UI handoff.
