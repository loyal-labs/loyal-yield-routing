import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { getSolanaEndpoints, type SolanaEnv } from "@loyal-labs/solana-rpc";
import { Connection, Keypair } from "@solana/web3.js";
import type { WalletSigner } from "@loyal-labs/wallet-core/types";
import { createKeypairSigner } from "~/src/lib/keypair-signer";
import {
  changePassword as reEncryptKeypair,
  clearStoredKeypair,
  generateKeypair,
  getStoredPublicKey,
  hasStoredKeypair,
  importKeypair,
  loadKeypair,
} from "~/src/lib/keypair-storage";
import { track } from "~/src/lib/analytics";
import {
  WALLET_SETUP_EVENTS,
  LOCK_METHODS,
} from "~/src/components/wallet/wallet-setup-analytics";
import {
  confettiShown,
  isBalanceHidden as isBalanceHiddenStorage,
  isWalletUnlocked as isWalletUnlockedStorage,
  lastActivityAt as lastActivityAtStorage,
  networkSelection,
} from "~/src/lib/storage";

type Network = Extract<SolanaEnv, "mainnet" | "devnet">;

function getRpcUrl(network: Network): string {
  return getSolanaEndpoints(network).rpcEndpoint;
}

export type WalletState = "loading" | "noWallet" | "locked" | "unlocked";

interface WalletContextValue {
  state: WalletState;
  /** Active signer — only available when state === "unlocked" */
  signer: WalletSigner | null;
  connection: Connection;
  network: Network;
  balanceHidden: boolean;
  publicKey: string | null;

  createWallet: (pin: string) => Promise<void>;
  importWallet: (secretKey: Uint8Array, pin: string) => Promise<void>;
  unlock: (pin: string) => Promise<void>;
  lock: () => void;
  resetWallet: (initialMode?: "create" | "import") => Promise<void>;
  resetMode: "create" | "import";
  setNetwork: (network: Network) => Promise<void>;
  toggleBalanceHidden: () => Promise<void>;
  /** Re-encrypt the keypair with a new password. Only available when unlocked. */
  changePassword: (newPassword: string) => Promise<void>;
  /** Returns the secret key as a byte array. Only available when unlocked. */
  getSecretKey: () => Uint8Array | null;
  finalizeSigner: (keypair: Keypair) => void;
}

const WalletContext = createContext<WalletContextValue | null>(null);

export function useWalletContext(): WalletContextValue {
  const ctx = useContext(WalletContext);
  if (!ctx) {
    throw new Error("useWalletContext must be used within a WalletProvider");
  }
  return ctx;
}

export function WalletProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = useState<WalletState>("loading");
  const [signer, setSigner] = useState<WalletSigner | null>(null);
  const [network, setNetworkState] = useState<Network>("mainnet");
  const [balanceHidden, setBalanceHidden] = useState(false);
  const [publicKey, setPublicKey] = useState<string | null>(null);
  const [activeKeypair, setActiveKeypair] =
    useState<Awaited<ReturnType<typeof loadKeypair>>>(null);

  const connection = useMemo(
    () => new Connection(getRpcUrl(network), "confirmed"),
    [network]
  );

  useEffect(() => {
    if (activeKeypair && state === "unlocked") {
      setSigner(createKeypairSigner(activeKeypair, connection));
    }
  }, [connection, activeKeypair, state]);

  useEffect(() => {
    let cancelled = false;

    async function init() {
      const [hasKeypair, storedPk, net, hidden, sessionResponse] =
        await Promise.all([
          hasStoredKeypair(),
          getStoredPublicKey(),
          networkSelection.getValue(),
          isBalanceHiddenStorage.getValue(),
          browser.runtime.sendMessage({ type: "GET_SESSION_KEYPAIR" }),
        ]);

      if (cancelled) return;

      setPublicKey(storedPk);
      setNetworkState(net);
      setBalanceHidden(hidden);

      if (sessionResponse?.secretKey) {
        try {
          const keypair = Keypair.fromSecretKey(
            new Uint8Array(JSON.parse(sessionResponse.secretKey))
          );
          buildSigner(keypair);
          return;
        } catch {
        }
      }

      setState(hasKeypair ? "locked" : "noWallet");
    }

    init();
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    const unwatchNetwork = networkSelection.watch((value) => {
      setNetworkState(value);
    });
    const unwatchBalance = isBalanceHiddenStorage.watch((value) => {
      setBalanceHidden(value);
    });
    const unwatchUnlocked = isWalletUnlockedStorage.watch((value) => {
      if (!value && state === "unlocked") {
        track(WALLET_SETUP_EVENTS.walletLocked, {
          method: LOCK_METHODS.autoTimeout,
        });
        setSigner(null);
        setActiveKeypair(null);
        setState("locked");
      }
    });

    return () => {
      unwatchNetwork();
      unwatchBalance();
      unwatchUnlocked();
    };
  }, [state]);

  useEffect(() => {
    if (state !== "unlocked") return;
    let lastSent = Date.now();
    const onActivity = () => {
      const now = Date.now();
      if (now - lastSent < 30_000) return; // throttle to once per 30s
      lastSent = now;
      void lastActivityAtStorage.setValue(now);
    };
    const events = ["click", "keydown", "scroll", "mousemove"] as const;
    for (const e of events)
      document.addEventListener(e, onActivity, { passive: true });
    return () => {
      for (const e of events) document.removeEventListener(e, onActivity);
    };
  }, [state]);

  const buildSigner = useCallback(
    (keypair: Awaited<ReturnType<typeof loadKeypair>>) => {
      if (!keypair) return;
      const walletSigner = createKeypairSigner(keypair, connection);
      setSigner(walletSigner);
      setActiveKeypair(keypair);
      setPublicKey(keypair.publicKey.toBase58());
      setState("unlocked");
      void isWalletUnlockedStorage.setValue(true);
      void lastActivityAtStorage.setValue(Date.now());
      void browser.runtime.sendMessage({
        type: "STORE_SESSION_KEYPAIR",
        secretKey: JSON.stringify(Array.from(keypair.secretKey)),
      });
    },
    [connection]
  );

  const createWallet = useCallback(
    async (pin: string) => {
      const keypair = await generateKeypair(pin);
      buildSigner(keypair);
    },
    [buildSigner]
  );

  const importWallet = useCallback(
    async (secretKey: Uint8Array, pin: string) => {
      const keypair = await importKeypair(secretKey, pin);
      buildSigner(keypair);
    },
    [buildSigner]
  );

  const unlock = useCallback(
    async (pin: string) => {
      const keypair = await loadKeypair(pin);
      if (!keypair) {
        throw new Error("Invalid PIN or no stored keypair");
      }
      buildSigner(keypair);
    },
    [buildSigner]
  );

  const lock = useCallback(() => {
    track(WALLET_SETUP_EVENTS.walletLocked, { method: LOCK_METHODS.manual });
    setSigner(null);
    setActiveKeypair(null);
    setState("locked");
    void isWalletUnlockedStorage.setValue(false);
    void browser.runtime.sendMessage({ type: "CLEAR_SESSION_KEYPAIR" });
  }, []);

  const changePassword = useCallback(
    async (newPassword: string) => {
      if (!activeKeypair) throw new Error("Wallet must be unlocked.");
      await reEncryptKeypair(activeKeypair, newPassword);
    },
    [activeKeypair]
  );

  const [resetMode, setResetMode] = useState<"create" | "import">("create");

  const resetWallet = useCallback(
    async (initialMode: "create" | "import" = "create") => {
      setSigner(null);
      setActiveKeypair(null);
      setPublicKey(null);
      setResetMode(initialMode);
      await clearStoredKeypair();
      await isWalletUnlockedStorage.setValue(false);
      await confettiShown.setValue(false);
      void browser.runtime.sendMessage({ type: "CLEAR_SESSION_KEYPAIR" });
      setState("noWallet");
    },
    []
  );

  const setNetwork = useCallback(async (net: Network) => {
    await networkSelection.setValue(net);
    setNetworkState(net);
  }, []);

  const toggleBalanceHidden = useCallback(() => {
    setBalanceHidden((prev) => {
      const next = !prev;
      void isBalanceHiddenStorage.setValue(next);
      return next;
    });
  }, []);

  const getSecretKey = useCallback((): Uint8Array | null => {
    if (!activeKeypair) return null;
    return activeKeypair.secretKey;
  }, [activeKeypair]);

  const finalizeSigner = useCallback(
    (keypair: Keypair) => {
      buildSigner(keypair);
    },
    [buildSigner]
  );

  const value = useMemo<WalletContextValue>(
    () => ({
      state,
      signer,
      connection,
      network,
      balanceHidden,
      publicKey,
      createWallet,
      importWallet,
      unlock,
      lock,
      changePassword,
      resetWallet,
      resetMode,
      setNetwork,
      toggleBalanceHidden,
      getSecretKey,
      finalizeSigner,
    }),
    [
      state,
      signer,
      connection,
      network,
      balanceHidden,
      publicKey,
      createWallet,
      importWallet,
      unlock,
      lock,
      changePassword,
      resetWallet,
      resetMode,
      setNetwork,
      toggleBalanceHidden,
      getSecretKey,
      finalizeSigner,
    ]
  );

  return (
    <WalletContext.Provider value={value}>{children}</WalletContext.Provider>
  );
}
