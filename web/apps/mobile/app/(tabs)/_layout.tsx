import { EarnMaxProvider } from "@/features/earn-max/EarnMaxProvider";
import { TabBar } from "@/components/TabBar";
import { ActivityProvider } from "@/features/activity/model/ActivityProvider";
import { router, Tabs } from "expo-router";
import { useEffect, useRef } from "react";
import { AppState } from "react-native";

const RESET_TO_HOME_AFTER_MS = 20 * 60 * 1000;

export default function TabsLayout() {
  const backgroundedAt = useRef<number | null>(null);

  useEffect(() => {
    const subscription = AppState.addEventListener("change", (next) => {
      if (next === "background" || next === "inactive") {
        if (backgroundedAt.current === null) {
          backgroundedAt.current = Date.now();
        }
        return;
      }

      if (next !== "active" || backgroundedAt.current === null) return;

      const elapsed = Date.now() - backgroundedAt.current;
      backgroundedAt.current = null;
      if (elapsed < RESET_TO_HOME_AFTER_MS) return;

      try {
        router.dismissAll();
      } catch {
      }
      router.navigate("/(tabs)");
    });
    return () => subscription.remove();
  }, []);

  return (
    <EarnMaxProvider>
      <ActivityProvider>
        <Tabs
          tabBar={(props) => <TabBar {...props} />}
          screenOptions={{
            headerShown: false,
            animation: "shift",
          }}
        >
          <Tabs.Screen name="index" />
          <Tabs.Screen name="wallet" options={{ href: null }} />
          <Tabs.Screen name="earn" options={{ href: null }} />
          <Tabs.Screen name="earn-max" options={{ href: null }} />
          <Tabs.Screen name="quests" />
          <Tabs.Screen name="activity" />
          <Tabs.Screen name="browser" />
          <Tabs.Screen name="library" />
          <Tabs.Screen name="profile" />
          <Tabs.Screen name="summaries" options={{ href: null }} />
        </Tabs>
      </ActivityProvider>
    </EarnMaxProvider>
  );
}
