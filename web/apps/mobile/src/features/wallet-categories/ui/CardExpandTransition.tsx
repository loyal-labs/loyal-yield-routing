import { useNavigation, useRouter } from "expo-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { StyleSheet, useWindowDimensions, View } from "react-native";
import { Gesture, GestureDetector } from "react-native-gesture-handler";
import type { GestureType } from "react-native-gesture-handler";
import Animated, {
  Easing,
  Extrapolation,
  interpolate,
  interpolateColor,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
  type SharedValue,
} from "react-native-reanimated";

import type { CardRect } from "../routes";

const CARD_RADIUS = 24;
const OPEN_MS = 440;
const CLOSE_MS = 260;
let cachedOrigin: { x: number; y: number } | null = null;
const FACE_FADE: [number, number] = [0, 0.22];
const CONTENT_FADE: [number, number] = [0.5, 0.85];
const DRAG_FACTOR = 0.45;
const DISMISS_DISTANCE = 130;
const DISMISS_VELOCITY = 900;

type Children = ReactNode | ((api: { close: () => void }) => ReactNode);

export function CardExpandTransition({
  sourceRect,
  cardColor,
  face,
  scrollOffset,
  scrollGesture,
  children,
}: {
  sourceRect?: CardRect;
  cardColor: string;
  face?: ReactNode;
  scrollOffset?: SharedValue<number>;
  scrollGesture?: GestureType;
  children: Children;
}) {
  const router = useRouter();
  const navigation = useNavigation();
  const { width: W, height: H } = useWindowDimensions();

  const progress = useSharedValue(sourceRect ? 0 : 1);
  const closingRef = useRef(false);
  const openedRef = useRef(false);

  // The source rect is in window coordinates (measureInWindow), but the box is
  // positioned inside this screen's root — which may not sit at the window origin
  // (status bar / safe-area insets). Measuring the root the SAME way lets us
  // subtract that offset so the box lands exactly on the real card. The morph
  // waits until this is known so it never animates from a wrong position.
  const rootRef = useRef<View>(null);
  const [origin, setOrigin] = useState<{ x: number; y: number } | null>(
    sourceRect ? cachedOrigin : { x: 0, y: 0 },
  );
  const handleRootLayout = useCallback(() => {
    rootRef.current?.measureInWindow((x, y) => {
      cachedOrigin = { x, y };
      setOrigin((prev) => prev ?? { x, y });
    });
  }, []);

  const sx = sourceRect?.x ?? 0;
  const sy = sourceRect?.y ?? 0;
  const sw = sourceRect?.width ?? W;
  const sh = sourceRect?.height ?? H;

  const finishClose = useCallback(() => {
    router.back();
  }, [router]);

  const close = useCallback(() => {
    if (closingRef.current) return;
    closingRef.current = true;
    if (!sourceRect) {
      router.back();
      return;
    }
    progress.value = withTiming(
      0,
      { duration: CLOSE_MS, easing: Easing.in(Easing.cubic) },
      (finished) => {
        if (finished) runOnJS(finishClose)();
      },
    );
  }, [sourceRect, router, finishClose, progress]);

  useEffect(() => {
    if (sourceRect && origin && !openedRef.current) {
      openedRef.current = true;
      progress.value = withTiming(1, {
        duration: OPEN_MS,
        easing: Easing.inOut(Easing.cubic),
      });
    }
  }, [sourceRect, origin, progress]);

  useEffect(() => {
    const unsubscribe = navigation.addListener("beforeRemove", (e) => {
      if (closingRef.current) return;
      e.preventDefault();
      close();
    });
    return unsubscribe;
  }, [navigation, close]);

  const pan = useMemo(() => {
    let gesture = Gesture.Pan()
      .enabled(!!sourceRect)
      .activeOffsetY(14)
      .failOffsetY(-14)
      .onUpdate((e) => {
        if (scrollOffset && scrollOffset.value > 0) return;
        if (e.translationY <= 0) return;
        progress.value = Math.min(
          1,
          Math.max(0, 1 - e.translationY / (H * DRAG_FACTOR)),
        );
      })
      .onEnd((e) => {
        if (scrollOffset && scrollOffset.value > 0) return;
        if (e.translationY > DISMISS_DISTANCE || e.velocityY > DISMISS_VELOCITY) {
          runOnJS(close)();
        } else {
          progress.value = withTiming(1, { duration: 180 });
        }
      });
    if (scrollGesture) {
      gesture = gesture.simultaneousWithExternalGesture(scrollGesture);
    }
    return gesture;
  }, [sourceRect, H, scrollOffset, scrollGesture, close, progress]);

  const boxStyle = useAnimatedStyle(() => {
    const p = progress.value;
    const ox = origin?.x ?? 0;
    const oy = origin?.y ?? 0;
    return {
      left: interpolate(p, [0, 1], [sx - ox, 0]),
      top: interpolate(p, [0, 1], [sy - oy, 0]),
      width: interpolate(p, [0, 1], [sw, W]),
      height: interpolate(p, [0, 1], [sh, H]),
      borderRadius: interpolate(p, [0, 1], [CARD_RADIUS, 0]),
      backgroundColor: interpolateColor(p, [0, 1], [cardColor, "#FFFFFF"]),
    };
  });

  const contentStyle = useAnimatedStyle(() => ({
    opacity: interpolate(progress.value, CONTENT_FADE, [0, 1], Extrapolation.CLAMP),
  }));

  const faceStyle = useAnimatedStyle(() => ({
    opacity: interpolate(progress.value, FACE_FADE, [1, 0], Extrapolation.CLAMP),
  }));

  const rendered =
    typeof children === "function" ? children({ close }) : children;

  return (
    <GestureDetector gesture={pan}>
      <View style={styles.root}>
        <View
          ref={rootRef}
          onLayout={handleRootLayout}
          pointerEvents="none"
          style={StyleSheet.absoluteFill}
        />
        {origin ? (
          <Animated.View style={[styles.box, boxStyle]}>
            <Animated.View
              style={[styles.content, { width: W, height: H }, contentStyle]}
            >
              {rendered}
            </Animated.View>
            {sourceRect && face ? (
              <Animated.View
                pointerEvents="none"
                style={[styles.face, { width: sw, height: sh }, faceStyle]}
              >
                {face}
              </Animated.View>
            ) : null}
          </Animated.View>
        ) : null}
      </View>
    </GestureDetector>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  box: { position: "absolute", overflow: "hidden" },
  content: { position: "absolute", left: 0, top: 0 },
  face: {
    position: "absolute",
    left: 0,
    top: 0,
    padding: 16,
    justifyContent: "space-between",
  },
});
