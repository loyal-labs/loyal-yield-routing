import * as Haptics from "expo-haptics";
import { Platform } from "react-native";

export function withTapHaptic(onPress: () => void) {
  return () => {
    if (Platform.OS !== "web") {
      const feedback = Platform.OS === "android"
        ? Haptics.performAndroidHapticsAsync(Haptics.AndroidHaptics.Virtual_Key)
        : Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
      void feedback.catch(() => {});
    }
    onPress();
  };
}
