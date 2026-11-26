import type { EvmDepositAddressResponse } from "@loyal-labs/api";

/** Only authorize the provider's receive-address operation for this wallet. */
export function depositAuthorization(
  preview: EvmDepositAddressResponse,
  appId: string,
  walletId: string | null | undefined
) {
  const request = preview.toSign;
  const expiry = preview.requestExpiry;
  if (
    preview.kind !== "privy_autobridge" ||
    !request ||
    !walletId ||
    !/^[A-Za-z0-9_-]+$/.test(walletId) ||
    request.url !==
      `https://api.privy.io/v1/wallets/${walletId}/deposit_accounts/crypto` ||
    request.version !== 1 ||
    request.method !== "POST" ||
    !expiry ||
    !Number.isSafeInteger(expiry) ||
    expiry <= Date.now() ||
    request.headers?.["privy-app-id"] !== appId ||
    request.headers?.["privy-request-expiry"] !== String(expiry) ||
    request.body?.type !== "inline_route" ||
    request.body.destination?.chain !== "solana" ||
    request.body.destination?.asset !== "usdc" ||
    request.body.source?.mode !== "include" ||
    !Array.isArray(request.body.source.values) ||
    !request.body.source.values.length
  ) {
    throw new Error(
      "Could not verify the deposit address request. Please try again."
    );
  }
  return { ...request, version: 1 as const, method: "POST" as const };
}
