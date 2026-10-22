import { Redirect, useFocusEffect } from "expo-router";
import { StatusBar } from "expo-status-bar";
import { useCallback, useEffect, useState } from "react";
import { useWindowDimensions } from "react-native";
import Animated, {
  cancelAnimation,
  Easing,
  useAnimatedStyle,
  useSharedValue,
  withDelay,
  withTiming,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { ARTBOARD_BASE_WIDTH } from "@/components/earn/AutodepositHelpSheet";
import { EarnDog } from "@/components/earn/EarnDog";
import { QUESTS_ENABLED } from "@/lib/feature-flags";
import { Text, View } from "@/tw";

import CoinIcon from "../../assets/images/quests/coin_58.svg";

const DOG_HEAD_RATIO = 506 / ARTBOARD_BASE_WIDTH;
const REVEAL_START_DELAY_MS = 500;
const DOG_RISE_MS = 800;
const ENTER_EASING = Easing.bezier(0.22, 1, 0.36, 1);

const COIN_DROP_DELAY_MS = REVEAL_START_DELAY_MS + 200;
const COIN_DROP_MS = 650;
const COIN_EASING = Easing.bezier(0.34, 1.56, 0.64, 1);

function TeaserCard() {
  return (
    <View
      style={{
        borderRadius: 24,
        backgroundColor: "#f7f7f7",
        padding: 20,
        alignItems: "center",
      }}
    >
      <CoinIcon width={29} height={49} />
      <Text
        style={{
          fontFamily: "Geist_500Medium",
          fontSize: 20,
          lineHeight: 24,
          color: "#000",
          marginTop: 12,
        }}
      >
        New quests are coming!
      </Text>
    </View>
  );
}

export default function QuestsScreen() {
  if (!QUESTS_ENABLED) {
    return <Redirect href="/(tabs)" />;
  }
  return <QuestsScreenContent />;
}

function QuestsScreenContent() {
  const insets = useSafeAreaInsets();
  const { width } = useWindowDimensions();
  const [runId, setRunId] = useState(0);

  const scale = width / ARTBOARD_BASE_WIDTH;
  const dogAreaHeight = Math.round(340 * scale);
  const dogWidth = Math.round(300 * scale);
  const dogHeight = Math.round(dogWidth * DOG_HEAD_RATIO);
  const dogTop = Math.round(32 * scale);
  const coinWidth = Math.round(57.457 * scale);
  const coinHeight = Math.round(98.312 * scale);
  const coinTop = Math.round(10 * scale);
  const sink = Math.round(dogAreaHeight * 0.5);
  const riseY = useSharedValue(sink);
  const coinSlide = insets.top + coinTop + coinHeight + 20;
  const coinY = useSharedValue(-coinSlide);

  useFocusEffect(
    useCallback(() => {
      setRunId((id) => id + 1);
      return () => {
        riseY.value = sink;
        coinY.value = -coinSlide;
      };
    }, [riseY, sink, coinY, coinSlide]),
  );

  useEffect(() => {
    if (runId === 0) return;
    cancelAnimation(riseY);
    cancelAnimation(coinY);
    riseY.value = sink;
    coinY.value = -coinSlide;
    riseY.value = withDelay(
      REVEAL_START_DELAY_MS,
      withTiming(0, { duration: DOG_RISE_MS, easing: ENTER_EASING }),
    );
    coinY.value = withDelay(
      COIN_DROP_DELAY_MS,
      withTiming(0, { duration: COIN_DROP_MS, easing: COIN_EASING }),
    );
    return () => {
      cancelAnimation(riseY);
      cancelAnimation(coinY);
    };
  }, [runId, riseY, sink, coinY, coinSlide]);

  const riseStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: riseY.value }],
  }));
  const coinStyle = useAnimatedStyle(() => ({
    transform: [{ translateY: coinY.value }],
  }));

  return (
    <View className="flex-1" style={{ backgroundColor: "#000" }}>
      {runId > 0 && <StatusBar style="light" />}
      <View
        style={{
          height: insets.top + dogAreaHeight,
          backgroundColor: "#000",
          overflow: "hidden",
        }}
      >
        <Animated.View
          pointerEvents="none"
          style={[
            {
              position: "absolute",
              left: 0,
              right: 0,
              top: insets.top + dogTop,
              alignItems: "center",
            },
            riseStyle,
          ]}
        >
          <EarnDog
            runId={runId}
            startDelay={REVEAL_START_DELAY_MS}
            width={dogWidth}
            height={dogHeight}
            eyeGlint
          />
        </Animated.View>
        <Animated.View
          pointerEvents="none"
          style={[
            {
              position: "absolute",
              left: 0,
              right: 0,
              top: insets.top + coinTop,
              alignItems: "center",
            },
            coinStyle,
          ]}
        >
          <CoinIcon width={coinWidth} height={coinHeight} />
        </Animated.View>
      </View>

      <View className="flex-1" style={{ backgroundColor: "#fff" }}>
        <View style={{ paddingTop: 32, paddingHorizontal: 16 }}>
          <Text
            style={{
              fontFamily: "Geist_600SemiBold",
              fontSize: 36,
              lineHeight: 40,
              letterSpacing: -0.72,
              color: "#000",
            }}
          >
            {"Seeker Summer\nRound 1 completed"}
          </Text>
        </View>

        <View style={{ flex: 1 }} />

        <View
          style={{ paddingHorizontal: 16, paddingBottom: insets.bottom + 84 }}
        >
          <TeaserCard />
        </View>
      </View>
    </View>
  );
}
