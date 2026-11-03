# Bridge UI handoff

This branch contains the Figma Bridge In and Bridge Out interfaces for backend integration. The bridge previews run in development, use fixtures, and never provision addresses, request quotes, sign, or submit transfers.

Production retains the existing backend-connected `ReceiveSheet` and its `loyal.bridge` browser opt-in. The sidebar Bridge action stays disabled in production. `shell.tsx` selects `BridgePreviewReceiveSheet` in development so this UI work does not replace the newer deposit integration on main.

## Entry points

All component paths below are relative to `src/components/wallet-workspace/facelift/`.

| Flow | Entry | Owning files |
| --- | --- | --- |
| Bridge In | Receive → Other chains → deposit asset → source chain; separate asset-to-receive picker | `bridge-preview-receive-sheet.tsx`, `bridge-preview-data.ts` |
| Bridge Out | Sidebar Bridge → send asset → receive asset → destination chain → recipient | `bridge-workspace.tsx`, `bridge-recipient-pane.tsx`, `bridge-preview-data.ts` |
| Navigation and motion | Shared Assets action stack, retained entry/exit, selector sheets, token/text changes | `crypto-page.tsx`, `shell.tsx`, `sidebar.tsx`, `token-icon-swap.tsx` |

Bridge Out shares the existing `MiddlePaneSlide`, `PaneReveal`, `SheetReveal`, and `TextSwap` behaviors with Send/Swap. Token icon transitions were extracted unchanged from Swap for reuse. Search remains keyboard accessible and has no red focus ring.

## Backend integration boundaries

- Replace the preview token/network catalog with authoritative supported routes. The fixture lists do not establish that a token/chain combination is supported.
- Bridge In: integrate the redesigned asset-first flow with the existing `ReceiveSheet` / `useEvmDepositAddress` behavior, retaining its auth, address provisioning, loading/error, and signing rules. The preview QR encodes `loyal:bridge-design-preview`, and its copy action deliberately does not copy the example address. Receive's Solana tab continues to use the authenticated wallet address.
- Bridge Out: replace the Figma balances, prices, amounts, fees, and recent recipients with real data. The displayed quote numbers are design fixtures, not transaction calculations. `Send now` currently shows a preview notice; add the authoritative quote/check/submit lifecycle and processing, success, and failure states before enabling transfers.
- Recipient validation currently checks address format only. Replace it with the supported destination-chain validation. Keep recipient confirmation scoped to the selected chain: changing Ethereum to Arbitrum clears the address, quote visibility, and send-ready state even though both accept the same address format; switching back must not restore the old confirmation. Draft/error state resets with the chain too.
- After integrating and validating the backend, consolidate the preview and live Receive implementations and remove the development-only selection in `shell.tsx`. Treat production enablement as a separate release decision.

## Design references

- Bridge In: [main](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72958), [deposit asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72826), [source chain](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-72884), [receive asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6349-75617).
- Bridge Out: [main](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71417), [send asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72228), [receive asset](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6360-75201), [destination chain](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6360-75254).
- Recipients: [empty](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71153), [pasted](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72165), [typing](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72198), [invalid](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-72118), [ready](https://www.figma.com/design/YTJOPpIYC7FEctch7b43Jz?node-id=6358-71737).

## Preview and verification

Use the real Vite entrypoint, `web/apps/loyal`, with `NODE_ENV=development` and the normal local environment. The current review preview is [loyal-dev.arbatov.dev/app](https://loyal-dev.arbatov.dev/app).

The browser smoke checks cover both entry paths, asset-first selectors, search/back navigation, address typing/paste/invalid/exact-match states, Ethereum ↔ Arbitrum reset, preview-only submission, desktop/mobile layout, keyboard focus, shared motion, reduced motion, and rapid close/reopen. Checks block transaction requests and use a fixture session; real wallet signing and transfers are not exercised. Local validation uses targeted lint and scoped TypeScript checks; no production build is required for this UI handoff.
