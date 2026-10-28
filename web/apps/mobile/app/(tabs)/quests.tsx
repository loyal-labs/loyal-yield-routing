import { Redirect, useLocalSearchParams } from "expo-router";

import { QuestsScreen } from "@/features/quests/QuestsScreen";
import { QUESTS_ENABLED } from "@/lib/feature-flags";

export default function QuestsRoute() {
  const { state } = useLocalSearchParams<{ state?: string }>();
  if (!QUESTS_ENABLED) return <Redirect href="/(tabs)" />;
  return <QuestsScreen inProgress={state !== "default"} />;
}
