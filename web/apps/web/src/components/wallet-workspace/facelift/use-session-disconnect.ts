"use client";

import { useWallet } from "@solana/wallet-adapter-react";

import { useAuthSession } from "@/contexts/auth-session-context";

export function useSessionDisconnect(): {
  canDisconnect: boolean;
  disconnectAll: () => void;
} {
  const { isAuthenticated, logout } = useAuthSession();
  const { connected: isWalletConnected, disconnect } = useWallet();
  return {
    canDisconnect: isAuthenticated || isWalletConnected,
    disconnectAll: () => {
      void Promise.allSettled([logout(), disconnect()]);
    },
  };
}
