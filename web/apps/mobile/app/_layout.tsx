import "@/global.css";

import { BottomSheetModalProvider } from "@gorhom/bottom-sheet";

import { DatadogInit } from "@/components/DatadogInit";
import { ObservabilityInit } from "@/components/ObservabilityInit";
import { OtaUpdateBanner } from "@/components/OtaUpdateBanner";
import { PushTokenRegistrar } from "@/components/PushTokenRegistrar";
import { PrivyProviderRoot } from "@/components/wallet/PrivyProviderRoot";
import { SplashAnimation } from "@/components/SplashAnimation";
import { WalletAuthGate } from "@/components/wallet/WalletAuthGate";
import { initAnalytics, track } from "@/lib/analytics/analytics";
import { APP_EVENTS } from "@/lib/analytics/app-events";
import { initAttribution } from "@/lib/analytics/attribution";
import { AppReadyProvider } from "@/lib/app-ready";
import { SignApprovalProvider } from "@/lib/wallet/sign-approval";
import { WalletProvider } from "@/lib/wallet/wallet-provider";
import {
  setupNotificationHandler,
} from "@/services/notifications";
import { initOneSignal } from "@/services/onesignal";
import { useFonts } from "expo-font";
import { Stack } from "expo-router";
import * as SplashScreen from "expo-splash-screen";
import { StatusBar } from "expo-status-bar";
import { useCallback, useEffect, useState } from "react";
import { AppState } from "react-native";
import { GestureHandlerRootView } from "react-native-gesture-handler";

SplashScreen.preventAutoHideAsync();

export default function RootLayout() {
  const [showSplash, setShowSplash] = useState(true);

  const [fontsLoaded] = useFonts({
    Geist_400Regular: require("@expo-google-fonts/geist/400Regular/Geist_400Regular.ttf"),
    Geist_500Medium: require("@expo-google-fonts/geist/500Medium/Geist_500Medium.ttf"),
    Geist_600SemiBold: require("@expo-google-fonts/geist/600SemiBold/Geist_600SemiBold.ttf"),
    Geist_700Bold: require("@expo-google-fonts/geist/700Bold/Geist_700Bold.ttf"),
    Geist_900Black: require("@expo-google-fonts/geist/900Black/Geist_900Black.ttf"),
  });

  useEffect(() => {
    if (fontsLoaded) {
      SplashScreen.hideAsync();
    }
  }, [fontsLoaded]);

  useEffect(() => {
    void setupNotificationHandler();
    void initOneSignal();
  }, []);

  useEffect(() => {
    void initAnalytics();
    track(APP_EVENTS.opened);
    const removeAttributionListener = initAttribution();
    const subscription = AppState.addEventListener("change", (next) => {
      if (next === "active") {
        track(APP_EVENTS.opened);
      }
    });
    return () => {
      subscription.remove();
      removeAttributionListener();
    };
  }, []);

  const handleSplashFinish = useCallback(() => {
    setShowSplash(false);
  }, []);

  if (!fontsLoaded) {
    return null;
  }

  return (
    <GestureHandlerRootView style={{ flex: 1 }}>
      <BottomSheetModalProvider>
        <PrivyProviderRoot>
          <WalletProvider>
            <SignApprovalProvider>
              <DatadogInit />
              <ObservabilityInit />
              <PushTokenRegistrar />
              <StatusBar style="auto" />
              <WalletAuthGate />
              <AppReadyProvider splashDone={!showSplash}>
                <Stack
                  screenOptions={{
                    headerBackButtonDisplayMode: "minimal",
                  }}
                >
                  <Stack.Screen
                    name="(tabs)"
                    options={{ headerShown: false }}
                  />
                  <Stack.Screen
                    name="token/[mint]"
                    options={{ headerShown: false }}
                  />
                  {/* Category pages morph out of the tapped wallet card. Use
                      `containedTransparentModal` (NOT `transparentModal`): it stays
                      transparent so the wallet shows behind, but is contained in the
                      navigator rather than presented as a native modal — so there's
                      no native present/dismiss slide that `animation: "none"` can't
                      suppress (that slide was the jump at the end of close).
                      CardExpandTransition drives the entire motion. */}
                  <Stack.Screen
                    name="wallet/stablecoins"
                    options={{
                      headerShown: false,
                      presentation: "containedTransparentModal",
                      animation: "none",
                      gestureEnabled: false,
                    }}
                  />
                  <Stack.Screen
                    name="wallet/crypto"
                    options={{
                      headerShown: false,
                      presentation: "containedTransparentModal",
                      animation: "none",
                      gestureEnabled: false,
                    }}
                  />
                  <Stack.Screen
                    name="browser/site"
                    options={{ headerShown: false }}
                  />
                </Stack>
              </AppReadyProvider>
              <OtaUpdateBanner />
            </SignApprovalProvider>
          </WalletProvider>
        </PrivyProviderRoot>
        {showSplash && <SplashAnimation onFinish={handleSplashFinish} />}
      </BottomSheetModalProvider>
    </GestureHandlerRootView>
  );
}
