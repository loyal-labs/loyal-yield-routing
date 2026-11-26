import type {
  EvmDepositAddressResponse,
  EvmDepositNetwork,
} from "@loyal-labs/api";
import { receiveNetworks, validatedDeposit } from "../deposit-routes";

const evm = "0x" + "ab".repeat(20);
const trx = "TJRabPrwbZy45sbavfcjinPJC18kjpRTv8";
const network: EvmDepositNetwork = {
  chain: "base",
  name: "Base Mainnet",
  shortName: "Base",
  route: "evm",
  address: evm,
  minUsd: 0,
  assets: [],
  tokens: [],
  qrIcon: null,
  icon: null,
};
const preview: EvmDepositAddressResponse = {
  address: evm,
  assets: [],
  chains: [],
  minimums: {},
  kind: "relay_signed",
  networks: [
    network,
    { ...network, chain: "ethereum", address: "0x" + "ef".repeat(20) },
    { ...network, chain: "tron", route: "relay_deposit_address", address: trx },
  ],
};

it("displays the wallet's matching EVM address and requires a renewable TRON lease", () => {
  const route = validatedDeposit(preview);
  const addresses = (lease: Parameters<typeof receiveNetworks>[1]) =>
    receiveNetworks(route, lease).map((n) => n.address);
  expect(addresses(null)).toEqual([evm, null, null]);
  const lease = {
    address: trx,
    minUsd: 10,
    feeUsd: null,
    receiveUsdc: null,
    expiresAt: new Date(Date.now() + 31 * 86400000).toISOString(),
  };
  expect(addresses(lease)).toEqual([evm, null, trx]);
  expect(
    addresses({
      ...lease,
      expiresAt: new Date(Date.now() + 29 * 86400000).toISOString(),
    })
  ).toEqual([evm, null, null]);
  expect(() => validatedDeposit({ ...preview, address: trx })).toThrow();
});
