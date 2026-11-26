import type {
  EvmDepositAddressResponse,
  EvmDepositNetwork,
  TronDepositAddressResponse,
} from "@loyal-labs/api";

const evmAddress = /^0x[\da-f]{40}$/i;
export const tronAddress = /^T[1-9A-HJ-NP-Za-km-z]{33}$/;
// Same renewal window as bridge.DepositAddressLead on the Go server.
const tronRenewalWindow = 30 * 24 * 60 * 60 * 1000;
export const renewAt = (address: TronDepositAddressResponse) =>
  Date.parse(address.expiresAt) - tronRenewalWindow;

export function validatedDeposit(value: EvmDepositAddressResponse) {
  if (
    !Array.isArray(value.networks) ||
    !["privy_autobridge", "relay_signed"].includes(value.kind) ||
    (value.address !== null && !evmAddress.test(value.address))
  ) {
    throw new Error("Could not read the deposit address. Please try again.");
  }
  return value;
}

export function receiveNetworks(
  deposit: EvmDepositAddressResponse | null,
  tron: TronDepositAddressResponse | null
): EvmDepositNetwork[] {
  return (deposit?.networks ?? []).flatMap((network) => {
    if (network.route === "evm") {
      // Every supported EVM network must agree with this wallet's shared 0x address.
      const address =
        network.address &&
        network.address.toLowerCase() === deposit?.address?.toLowerCase()
          ? network.address
          : null;
      return [{ ...network, address }];
    }
    if (network.route !== "relay_deposit_address" || network.chain !== "tron")
      return [];
    // GET includes a cached TRON address without its expiry. The POST checks
    // expiry and returns the address and current fee for the selected route.
    const address = tron && renewAt(tron) > Date.now() ? tron.address : null;
    return [
      {
        ...network,
        address,
        minUsd: tron?.minUsd ?? network.minUsd,
        tokens:
          network.tokens?.map((token) => ({
            ...token,
            feeUsd: tron?.feeUsd ?? token.feeUsd,
            receiveUsdc: tron?.receiveUsdc ?? token.receiveUsdc,
          })) ?? null,
      },
    ];
  });
}
