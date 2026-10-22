"use client";

import { useSignal, viewport } from "@telegram-apps/sdk-react";
import type { Signal } from "@telegram-apps/signals";
import { useCallback, useEffect, useState } from "react";

interface SafeAreaInsets {
  top: number;
  bottom: number;
}

const getWebApp = () => {
  return (
    window as {
      Telegram?: {
        WebApp?: {
          isVersionAtLeast?: (version: string) => boolean;
          contentSafeAreaInset?: { top: number; bottom: number };
          safeAreaInset?: { top: number; bottom: number };
          onEvent?: (eventType: string, callback: () => void) => void;
          offEvent?: (eventType: string, callback: () => void) => void;
        };
      };
    }
  ).Telegram?.WebApp;
};

const getDeviceSafeAreaTopValue = (): number => {
  if (typeof window === "undefined") return 0;
  return getWebApp()?.safeAreaInset?.top ?? 0;
};

const getSafeArea = (): SafeAreaInsets => {
  const webApp = getWebApp();
  if (!webApp) return { top: 0, bottom: 0 };

  const hasContentSafe = webApp.isVersionAtLeast?.("8.0") ?? false;
  const contentInsets = hasContentSafe
    ? webApp.contentSafeAreaInset
    : undefined;
  const deviceInsets = webApp.safeAreaInset;

  return {
    top: (contentInsets?.top ?? 0) + (deviceInsets?.top ?? 0),
    bottom: (contentInsets?.bottom ?? 0) + (deviceInsets?.bottom ?? 0),
  };
};

export function useDeviceSafeAreaTop(): number {
  const sdkTop = useSignal(viewport.safeAreaInsetTop as Signal<number>);

  const [nativeTop, setNativeTop] = useState(getDeviceSafeAreaTopValue);

  useEffect(() => {
    const update = () => setNativeTop(getDeviceSafeAreaTopValue());
    update();
    const webApp = getWebApp();
    webApp?.onEvent?.("safeAreaChanged", update);
    webApp?.onEvent?.("viewportChanged", update);
    return () => {
      webApp?.offEvent?.("safeAreaChanged", update);
      webApp?.offEvent?.("viewportChanged", update);
    };
  }, []);

  return sdkTop || nativeTop;
}

export function useTelegramSafeArea(): SafeAreaInsets {
  const [insets, setInsets] = useState<SafeAreaInsets>(getSafeArea);

  const updateInsets = useCallback(() => {
    setInsets(getSafeArea());
  }, []);

  useEffect(() => {
    updateInsets();

    const webApp = getWebApp();
    webApp?.onEvent?.("contentSafeAreaChanged", updateInsets);
    webApp?.onEvent?.("safeAreaChanged", updateInsets);
    webApp?.onEvent?.("viewportChanged", updateInsets);

    return () => {
      webApp?.offEvent?.("contentSafeAreaChanged", updateInsets);
      webApp?.offEvent?.("safeAreaChanged", updateInsets);
      webApp?.offEvent?.("viewportChanged", updateInsets);
    };
  }, [updateInsets]);

  return insets;
}

export function useModalSnapPoint(): number {
  return 0.92;
}
