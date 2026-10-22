import { useEffect, useState } from "react";
import { Modal, StyleSheet, useWindowDimensions } from "react-native";
import Animated, {
  Easing,
  useAnimatedStyle,
  useSharedValue,
  withDelay,
  withSpring,
  withTiming,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { DogNose, EarnDog } from "@/components/earn/EarnDog";
import { Pressable, Text, View } from "@/tw";

import PawPrint from "../../../assets/images/quests/paw_print.svg";
import SheetCurl from "../../../assets/images/quests/sheet_curl.svg";
import Snout from "../../../assets/images/quests/snout.svg";

const DOG_HEAD_RATIO = 506 / 400;
const DOG_OVERLAP = 0.2;
const REVEAL_DELAY_MS = 120;

// White panel anatomy (drives both the panel min-height and where the dog
// snout meets it). pt 72 + title 80 + gap 16 + subtitle 48 + gap 48 + button 50.
const BUTTON_HEIGHT = 50;
const PANEL_PT = 72;
const PANEL_CONTENT = PANEL_PT + 80 + 16 + 48 + 48 + BUTTON_HEIGHT;
const PANEL_PB = 40;

// The paw is stamped onto the card after the slide-in spring has settled (~120ms
// delay + ~540ms spring), so it reads as a distinct beat — the sheet lands, then
// the paw presses down.
const SHEET_SPRING = { damping: 26, stiffness: 170, mass: 1 } as const;
const SPRING_SETTLE_AMPLITUDE = 0.0009;
const SHEET_SPRING_SETTLE_MS = Math.ceil(
  ((2 * SHEET_SPRING.mass * 1000) / SHEET_SPRING.damping) *
    Math.log(1 / SPRING_SETTLE_AMPLITUDE),
);
const PAW_STAMP_DELAY_MS = REVEAL_DELAY_MS + SHEET_SPRING_SETTLE_MS;

export type QuestCompleteVariant = "single" | "all";

const COPY: Record<
  QuestCompleteVariant,
  { title: string; body: string; cta: string }
> = {
  single: {
    title: "Task complete",
    body: "You completed a Seeker Summer task",
    cta: "Next",
  },
  all: {
    title: "Quest complete!",
    body: "You completed all Seeker Summer tasks",
    cta: "Great",
  },
};

export function QuestCompleteNotification({
  visible,
  variant,
  onClose,
}: {
  visible: boolean;
  variant: QuestCompleteVariant;
  onClose: () => void;
}) {
  const insets = useSafeAreaInsets();
  const { width } = useWindowDimensions();
  const [runId, setRunId] = useState(0);

  const dogWidth = width;
  const dogHeight = Math.round(dogWidth * DOG_HEAD_RATIO);
  const dogTuck = Math.round(dogHeight * DOG_OVERLAP);
  const panelMinHeight = PANEL_CONTENT + PANEL_PB + insets.bottom;
  const sheetTravel = panelMinHeight + dogHeight - dogTuck;

  const backdrop = useSharedValue(0);
  const sheetY = useSharedValue(sheetTravel);
  const pawScale = useSharedValue(1.5);
  const pawOpacity = useSharedValue(0);

  useEffect(() => {
    if (!visible) {
      backdrop.value = 0;
      sheetY.value = sheetTravel;
      pawScale.value = 1.5;
      pawOpacity.value = 0;
      return;
    }
    setRunId((id) => id + 1);
    backdrop.value = 0;
    sheetY.value = sheetTravel;
    pawScale.value = 1.5;
    pawOpacity.value = 0;

    backdrop.value = withTiming(1, { duration: 240 });
    sheetY.value = withDelay(REVEAL_DELAY_MS, withSpring(0, SHEET_SPRING));
    pawOpacity.value = withDelay(
      PAW_STAMP_DELAY_MS,
      withTiming(1, { duration: 80, easing: Easing.out(Easing.quad) }),
    );
    pawScale.value = withDelay(
      PAW_STAMP_DELAY_MS,
      withSpring(1, { damping: 13, stiffness: 300, mass: 0.7 }),
    );
  }, [visible, sheetTravel, backdrop, sheetY, pawScale, pawOpacity]);

  const backdropStyle = useAnimatedStyle(() => ({ opacity: backdrop.value }));
  const sheetStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: sheetY.value }],
  }));
  const pawStyle = useAnimatedStyle(() => ({
    opacity: pawOpacity.value,
    transform: [{ scale: pawScale.value }],
  }));

  const copy = COPY[variant];

  const scale = width / 400;
  const curlW = Math.round(78 * scale);
  const curlH = Math.round(100 * scale);
  const snoutW = Math.round(255 * scale);
  const snoutH = Math.round(140 * scale);
  const snoutTop = Math.round(298 * scale);
  const snoutLeft = Math.round((dogWidth - snoutW) / 2);
  const pawBottom = insets.bottom + PANEL_PB + BUTTON_HEIGHT + 24;

  return (
    <Modal
      visible={visible}
      transparent
      statusBarTranslucent
      animationType="none"
      onRequestClose={onClose}
    >
      <View style={{ flex: 1 }}>
        <Animated.View
          style={[
            StyleSheet.absoluteFill,
            { backgroundColor: "rgba(0,0,0,0.72)" },
            backdropStyle,
          ]}
        />

        <Animated.View
          pointerEvents="box-none"
          style={[StyleSheet.absoluteFill, sheetStyle]}
        >
          <View
            pointerEvents="none"
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              bottom: panelMinHeight - dogTuck,
              alignItems: "center",
            }}
          >
            <EarnDog
              runId={runId}
              startDelay={REVEAL_DELAY_MS}
              width={dogWidth}
              height={dogHeight}
              eyeGlint
              blinkOnly
            />
          </View>

          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              bottom: 0,
              minHeight: panelMinHeight,
              backgroundColor: "#fff",
              paddingTop: PANEL_PT,
              paddingHorizontal: 20,
              paddingBottom: insets.bottom + PANEL_PB,
              overflow: "hidden",
            }}
          >
            <View
              pointerEvents="none"
              style={{ position: "absolute", top: 0, right: 0 }}
            >
              <SheetCurl width={curlW} height={curlH} />
            </View>
            <Animated.View
              pointerEvents="none"
              style={[
                {
                  position: "absolute",
                  right: 24,
                  bottom: pawBottom,
                  width: 108,
                  height: 105,
                },
                pawStyle,
              ]}
            >
              <PawPrint width={108} height={105} />
            </Animated.View>

            <Text
              style={{
                fontFamily: "Geist_700Bold",
                fontSize: 36,
                lineHeight: 40,
                letterSpacing: -0.72,
                textTransform: "uppercase",
                color: "#000",
                width: 254,
              }}
            >
              {copy.title}
            </Text>
            <Text
              style={{
                marginTop: 16,
                fontFamily: "Geist_400Regular",
                fontSize: 17,
                lineHeight: 22,
                color: "rgba(60,60,67,0.6)",
                width: 210,
              }}
            >
              {copy.body}
            </Text>

            <Pressable
              onPress={onClose}
              accessibilityRole="button"
              accessibilityLabel={copy.cta}
              style={{
                marginTop: 48,
                height: BUTTON_HEIGHT,
                borderRadius: 78,
                backgroundColor: "#000",
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <Text
                style={{
                  fontFamily: "Geist_500Medium",
                  fontSize: 17,
                  lineHeight: 22,
                  color: "#fff",
                }}
              >
                {copy.cta}
              </Text>
            </Pressable>
          </View>

          <View
            pointerEvents="none"
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              bottom: panelMinHeight - dogTuck,
              height: dogHeight,
            }}
          >
            <View
              style={{
                position: "absolute",
                top: snoutTop,
                left: snoutLeft,
                width: snoutW,
                height: snoutH,
              }}
            >
              <Snout width={snoutW} height={snoutH} />
            </View>
            <DogNose width={dogWidth} height={dogHeight} />
          </View>
        </Animated.View>
      </View>
    </Modal>
  );
}
