import { useEffect, useRef } from "react";

import { useWallet } from "@/lib/wallet/wallet-provider";
import {
  loginOneSignal,
  requestOneSignalPermission,
} from "@/services/onesignal";

export function PushTokenRegistrar(): null {
  const { publicKey } = useWallet();
  const lastRegisteredPublicKey = useRef<string | null>(null);

  useEffect(() => {
    if (!publicKey) return;
    if (lastRegisteredPublicKey.current === publicKey) return;

    lastRegisteredPublicKey.current = publicKey;
    void (async () => {
      await loginOneSignal(publicKey);
      const granted = await requestOneSignalPermission();
      console.log(
        "[push] Registrar completed for wallet",
        publicKey.slice(0, 8),
        granted ? "(permission granted)" : "(permission not granted)",
      );
    })();
  }, [publicKey]);

  return null;
}
