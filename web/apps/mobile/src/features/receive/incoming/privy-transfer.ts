import type { BridgeCall, BridgeQuoteResponse, EvmDepositAddressResponse } from "@loyal-labs/api";
import {
  getAllUserEmbeddedEthereumWallets,
  isUnifiedWallet,
  rpc,
  type ConnectedEthereumWallet,
  type User,
  type usePrivyClient,
} from "@privy-io/expo";
import { evmChains as chains } from "@/features/receive/evm-chains";

const evmAddress = /^0x[0-9a-f]{40}$/i;
const txHash = /^0x[0-9a-f]{64}$/i;

export function isExternalOwner(user: User | null, wallet: string) {
  return (
    user?.linked_accounts.some(
      (account) =>
        account.type === "wallet" &&
        account.chain_type === "solana" &&
        account.address === wallet &&
        account.wallet_client !== "privy" &&
        account.wallet_client_type !== "privy" &&
        account.wallet_client_type !== "privy-v2" &&
        account.connector_type !== "embedded",
    ) ?? false
  );
}

export function linkedSender(user: User | null, route: EvmDepositAddressResponse) {
  if (route.kind !== "relay_signed" || !route.address || !evmAddress.test(route.address)) return null;
  const address = route.address.toLowerCase();
  const account = getAllUserEmbeddedEthereumWallets(user).find(
    (a) => a.address.toLowerCase() === address,
  );
  return account && isUnifiedWallet(account) && account.user_can_sign !== false
    ? { address: account.address, id: account.id }
    : null;
}

export function validateQuote(chain: string, quote: BridgeQuoteResponse) {
  const network = chains[chain as keyof typeof chains];
  if (
    !network ||
    quote.chainId !== network.id ||
    !quote.requestId ||
    !/^\d+$/.test(quote.amountOutRaw) ||
    !/^\d+$/.test(quote.amountOutMinRaw) ||
    BigInt(quote.amountOutMinRaw) <= BigInt(0) ||
    BigInt(quote.amountOutMinRaw) > BigInt(quote.amountOutRaw) ||
    !quote.calls?.length ||
    quote.calls.length > 2 ||
    quote.calls.some(
      (c) => !evmAddress.test(c.to) || !/^0x(?:[0-9a-f]{2})+$/i.test(c.data) || !/^0+$/.test(c.value),
    )
  ) {
    throw new Error("The quote could not be verified. Review a new quote.");
  }
}

type Client = ReturnType<typeof usePrivyClient>;

export async function receiptProvider(wallet: ConnectedEthereumWallet, chainId: number) {
  const provider = await wallet.getProvider();
  await provider.request({
    method: "wallet_switchEthereumChain",
    params: [{ chainId: `0x${chainId.toString(16)}` }],
  });
  const [chain, accounts] = await Promise.all([
    provider.request({ method: "eth_chainId" }),
    provider.request({ method: "eth_accounts" }),
  ]);
  if (
    Number(chain) !== chainId ||
    !Array.isArray(accounts) ||
    !accounts.some((a) => typeof a === "string" && a.toLowerCase() === wallet.address.toLowerCase())
  ) {
    throw new Error("Reconnect the EVM wallet shown in the transfer.");
  }
  return provider;
}

export async function sendSponsoredCall(
  client: Client,
  owner: { id: string; address: string },
  chainId: number,
  call: BridgeCall,
  assertActive: () => Promise<void>,
) {
  const caip2 = `eip155:${chainId}` as const;
  const payment = { to: call.to, data: call.data, value: `0x${BigInt(call.value).toString(16)}` };
  const transaction =
    chainId === chains.tempo.id
      ? { type: 118 as const, from: owner.address, chain_id: chainId, calls: [payment] }
      : { ...payment, from: owner.address, chain_id: chainId };
  let handedToTransport = false;
  try {
    // Expo's EIP-1193 send broadcasts self-paid raw bytes. Wallet RPC signs the sponsorship flag.
    const result = await rpc(
      {
        app: client.app,
        getCompiledPath: client.getCompiledPath.bind(client),
        fetchPrivyRoute: async (route, options) => {
          await assertActive();
          handedToTransport = true;
          return client.fetchPrivyRoute(route, options);
        },
      },
      async (input) => {
        await assertActive();
        return client.embeddedWallet.signWithUserSigner(input);
      },
      {
        wallet_id: owner.id,
        method: "eth_sendTransaction",
        caip2,
        sponsor: true,
        params: { transaction },
      },
    );
    if (
      result.method !== "eth_sendTransaction" ||
      result.data.caip2 !== caip2 ||
      !txHash.test(result.data.hash)
    ) {
      throw new Error("Waiting for the bridge to verify the transaction.");
    }
    return { hash: result.data.hash };
  } catch (error) {
    const e = error as { code?: unknown; eipCode?: unknown };
    if (
      !handedToTransport &&
      (e?.code === "exited_auth_flow" || e?.code === "mfa_canceled" || e?.eipCode === 4001)
    ) {
      throw Object.assign(new Error("Wallet approval canceled."), { code: 4001 });
    }
    if (!handedToTransport && e?.code !== 4001) {
      throw Object.assign(new Error("The wallet could not authorize this transfer."), {
        code: "move_not_broadcast",
      });
    }
    throw error;
  }
}
