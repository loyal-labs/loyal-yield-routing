import type { EvmDepositAddressResponse } from "@loyal-labs/api";
import { depositAuthorization } from "../deposit-authorization";

function preview(): EvmDepositAddressResponse {
  const expiry = Date.now() + 600_000;
  return {
    address: null,
    assets: [],
    chains: [],
    kind: "privy_autobridge",
    minimums: {},
    networks: [],
    requestExpiry: expiry,
    toSign: {
      version: 1,
      method: "POST",
      url: "https://api.privy.io/v1/wallets/wallet-id/deposit_accounts/crypto",
      headers: {
        "privy-app-id": "app",
        "privy-request-expiry": String(expiry),
      },
      body: {
        type: "inline_route",
        source: { mode: "include", values: [{ asset: "usdc", chain: "base" }] },
        destination: { asset: "usdc", chain: "solana" },
      },
    },
  };
}

it("signs the exact server preview for the active embedded wallet", () => {
  const route = preview();
  expect(depositAuthorization(route, "app", "wallet-id")).toEqual(route.toSign);
});

it("refuses an unbound wallet, mismatched wallet, app, expiry or funding operation", () => {
  expect(() => depositAuthorization(preview(), "app", undefined)).toThrow();
  expect(() =>
    depositAuthorization(preview(), "app", "another-wallet")
  ).toThrow();
  expect(() =>
    depositAuthorization(preview(), "another-app", "wallet-id")
  ).toThrow();
  for (const change of [
    (route: EvmDepositAddressResponse) => {
      route.toSign!.url += "/send";
    },
    (route: EvmDepositAddressResponse) => {
      route.toSign!.body.destination.chain = "ethereum";
    },
    (route: EvmDepositAddressResponse) => {
      route.requestExpiry = Date.now() - 1;
    },
    (route: EvmDepositAddressResponse) => {
      route.toSign!.headers["privy-request-expiry"] = "0";
    },
  ]) {
    const route = preview();
    change(route);
    expect(() => depositAuthorization(route, "app", "wallet-id")).toThrow();
  }
});
