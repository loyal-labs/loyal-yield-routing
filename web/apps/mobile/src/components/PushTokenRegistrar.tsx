import { useEffect, useRef } from "react";

import { useWallet } from "@/lib/wallet/wallet-provider";
import {
  addOneSignalSubscriptionIdListener,
  getOneSignalSubscriptionId,
  loginOneSignal,
  requestOneSignalPermission,
} from "@/services/onesignal";
import { registerOneSignalSubscription } from "@/services/push-registration";

export function PushTokenRegistrar(): null {
  const { publicKey } = useWallet();
  const lastLoggedInPublicKey = useRef<string | null>(null);
  // "<wallet>:<subscriptionId>" last stored by the server.
  const lastRegistered = useRef<string | null>(null);

  useEffect(() => {
    if (!publicKey) return;
    let active = true;
    let unsubscribe: (() => void) | null = null;

    const register = async (subscriptionId: string) => {
      const key = `${publicKey}:${subscriptionId}`;
      if (!active || lastRegistered.current === key) return;
      if (await registerOneSignalSubscription(subscriptionId, publicKey)) {
        lastRegistered.current = key;
      }
    };

    void (async () => {
      if (lastLoggedInPublicKey.current !== publicKey) {
        lastLoggedInPublicKey.current = publicKey;
        await loginOneSignal(publicKey);
        const granted = await requestOneSignalPermission();
        console.log(
          "[push] Registrar completed for wallet",
          publicKey.slice(0, 8),
          granted ? "(permission granted)" : "(permission not granted)",
        );
      }
      // The server pushes only to subscriptions registered through the Earn
      // session, so register the ID now and again whenever it changes.
      unsubscribe = await addOneSignalSubscriptionIdListener((id) => {
        void register(id);
      });
      if (!active) {
        unsubscribe?.();
        return;
      }
      const subscriptionId = await getOneSignalSubscriptionId();
      if (subscriptionId) await register(subscriptionId);
    })();

    return () => {
      active = false;
      unsubscribe?.();
    };
  }, [publicKey]);

  return null;
}
