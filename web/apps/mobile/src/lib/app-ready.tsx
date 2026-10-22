import { createContext, type ReactNode, useContext } from "react";

import { isWalletUnlocked, useWallet } from "@/lib/wallet/wallet-provider";

const AppReadyContext = createContext(false);

export function AppReadyProvider({
  splashDone,
  children,
}: {
  splashDone: boolean;
  children: ReactNode;
}) {
  const { state, onboardingReplayActive } = useWallet();
  const ready = splashDone && isWalletUnlocked(state) && !onboardingReplayActive;

  return (
    <AppReadyContext.Provider value={ready}>
      {children}
    </AppReadyContext.Provider>
  );
}

export function useAppReady(): boolean {
  return useContext(AppReadyContext);
}
