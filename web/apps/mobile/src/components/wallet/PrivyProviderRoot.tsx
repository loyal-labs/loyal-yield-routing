import { DEFAULT_SUPPORTED_CHAINS, mainnet } from "@privy-io/chains";
import { PrivyProvider } from "@privy-io/expo";
import type { ComponentProps, ReactNode } from "react";

import { env } from "@/config/env";
import { evmChains } from "@/features/receive/evm-chains";
import { ReceiveAuthorizationProvider } from "@/features/receive/ReceiveAuthorization";

const supportedChains: ComponentProps<typeof PrivyProvider>["supportedChains"] =
  [
  mainnet,
    ...DEFAULT_SUPPORTED_CHAINS.filter(
      (chain) => chain.id !== evmChains.ethereum.id
    ),
    ...Object.values(evmChains).filter(
      (chain) =>
        !DEFAULT_SUPPORTED_CHAINS.some(
          (defaultChain) => defaultChain.id === chain.id
        )
    ),
  ];

export function isPrivyConfigured(): boolean {
  return env.privyAppId.length > 0;
}

// Mounts Privy only when configured. Every Privy hook consumer must either
// guard with `isPrivyConfigured()` or render below this provider only when it
// is configured, since the hooks throw without a provider.
export function PrivyProviderRoot({ children }: { children: ReactNode }) {
  if (!isPrivyConfigured()) return <>{children}</>;
  return (
    <PrivyProvider
      appId={env.privyAppId}
      clientId={env.privyClientId || undefined}
      supportedChains={supportedChains}
      config={{ embedded: { solana: { createOnLogin: "off" } } }}
    >
      <ReceiveAuthorizationProvider>{children}</ReceiveAuthorizationProvider>
    </PrivyProvider>
  );
}
