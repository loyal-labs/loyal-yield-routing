import { expect, test } from "bun:test";

import { publicEnv } from "./public-env";

test("a NEXT_PUBLIC_ variable the client does not read never reaches the bundle", () => {
  const shipped = publicEnv({
    NEXT_PUBLIC_PRIVY_APP_ID: "privy-app",
    NEXT_PUBLIC_SOLANA_RPC_ENDPOINT: "https://mainnet.helius-rpc.com/?api-key=not-a-real-key",
    HELIUS_API_KEY: "not-a-real-key",
  });

  expect(shipped).toEqual({ NEXT_PUBLIC_PRIVY_APP_ID: "privy-app" });
});
