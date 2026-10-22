import { BlurView } from "expo-blur";
import * as Haptics from "expo-haptics";
import {
  ArrowDown,
  ArrowLeftRight,
  ArrowUp,
  ShieldOff,
  X,
} from "lucide-react-native";
import {
  type ComponentRef,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  BackHandler,
  StyleSheet,
  useWindowDimensions,
  View,
} from "react-native";
import Animated, {
  Easing,
  runOnJS,
  type SharedValue,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from "react-native-reanimated";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { Pressable, Text } from "@/tw";

export type MoreActionsAnchor = {
  x: number;
  y: number;
  width: number;
  height: number;
};

const ROW_TRAVEL = 18;
const STAGGER = 0.09;
const WINDOW = 0.6;
const FILL_FRACTION = 0.6;
const ICON_OPACITY = 0.6;

type QuestAction = {
  key: string;
  label: string;
  icon: ReactNode;
  run: () => void;
};

function AnimatedActionRow({
  action,
  rank,
  progress,
  onRun,
}: {
  action: QuestAction;
  rank: number;
  progress: SharedValue<number>;
  onRun: (run: () => void) => void;
}) {
  const [pressed, setPressed] = useState(false);

  const animatedStyle = useAnimatedStyle(() => {
    const start = rank * STAGGER;
    const raw = (progress.value - start) / WINDOW;
    const t = raw < 0 ? 0 : raw > 1 ? 1 : raw;
    return {
      opacity: t,
      transform: [{ translateY: (1 - t) * ROW_TRAVEL }],
    };
  });

  return (
    <Animated.View style={animatedStyle}>
      <Pressable
        onPress={() => onRun(action.run)}
        onPressIn={() => setPressed(true)}
        onPressOut={() => setPressed(false)}
        accessibilityRole="button"
        accessibilityLabel={action.label}
        className="h-[50px] flex-row items-center justify-end gap-3 pl-2 pr-2"
        style={{ transform: [{ scale: pressed ? 0.96 : 1 }] }}
      >
        <Text
          className="text-[20px] font-medium text-black"
          style={{ lineHeight: 24 }}
        >
          {action.label}
        </Text>
        {action.icon}
      </Pressable>
    </Animated.View>
  );
}

export function MoreActionsSheet({
  open,
  onClose,
  anchor,
  onSend,
  onReceive,
  onSwap,
  onUnshield,
}: {
  open: boolean;
  onClose: () => void;
  anchor?: MoreActionsAnchor | null;
  onSend?: () => void;
  onReceive?: () => void;
  onSwap?: () => void;
  onUnshield?: () => void;
}) {
  const insets = useSafeAreaInsets();
  const { width: screenW, height: screenH } = useWindowDimensions();
  const progress = useSharedValue(0);
  const [mounted, setMounted] = useState(false);
  const rootRef = useRef<ComponentRef<typeof View>>(null);
  const [origin, setOrigin] = useState({ x: 0, y: 0 });

  useEffect(() => {
    if (open) {
      setMounted(true);
    }
  }, [open]);

  useEffect(() => {
    if (!mounted) {
      return;
    }
    if (open) {
      void Haptics.selectionAsync();
      progress.value = withTiming(1, {
        duration: 560,
        easing: Easing.out(Easing.cubic),
      });
    } else {
      progress.value = withTiming(
        0,
        { duration: 380, easing: Easing.in(Easing.cubic) },
        (finished) => {
          if (finished) {
            runOnJS(setMounted)(false);
          }
        },
      );
    }
  }, [open, mounted, progress]);

  useEffect(() => {
    if (!mounted || !open) {
      return;
    }
    const sub = BackHandler.addEventListener("hardwareBackPress", () => {
      onClose();
      return true;
    });
    return () => sub.remove();
  }, [mounted, open, onClose]);

  const handleRootLayout = () => {
    rootRef.current?.measureInWindow((x, y) => setOrigin({ x, y }));
  };

  const actions: QuestAction[] = [];
  if (onUnshield) {
    actions.push({
      key: "unshield",
      label: "Unshield",
      icon: (
        <ShieldOff
          size={28}
          color="#000"
          strokeWidth={2}
          opacity={ICON_OPACITY}
        />
      ),
      run: onUnshield,
    });
  }
  if (onSwap) {
    actions.push({
      key: "swap",
      label: "Swap",
      icon: (
        <ArrowLeftRight
          size={28}
          color="#000"
          strokeWidth={2}
          opacity={ICON_OPACITY}
        />
      ),
      run: onSwap,
    });
  }
  if (onReceive) {
    actions.push({
      key: "receive",
      label: "Receive",
      icon: (
        <ArrowDown
          size={28}
          color="#000"
          strokeWidth={2}
          opacity={ICON_OPACITY}
        />
      ),
      run: onReceive,
    });
  }
  if (onSend) {
    actions.push({
      key: "send",
      label: "Send",
      icon: (
        <ArrowUp
          size={28}
          color="#000"
          strokeWidth={2}
          opacity={ICON_OPACITY}
        />
      ),
      run: onSend,
    });
  }

  const lastRowEndsAt = (actions.length - 1) * STAGGER + WINDOW;
  if (__DEV__ && lastRowEndsAt > 1) {
    throw new Error(
      `MoreActionsSheet stagger overshoots: last of ${actions.length} rows ` +
        `ends at progress ${lastRowEndsAt} — lower STAGGER or WINDOW`,
    );
  }

  const runAction = (run: () => void) => {
    void Haptics.selectionAsync();
    onClose();
    run();
  };

  const backdropStyle = useAnimatedStyle(() => {
    const fill = Math.min(1, progress.value / FILL_FRACTION);
    return {
      opacity: Math.min(1, progress.value * 5),
      transform: [{ translateY: (1 - fill) * screenH }],
    };
  });
  const closeStyle = useAnimatedStyle(() => ({
    opacity: progress.value,
    transform: [{ scale: 0.85 + 0.15 * progress.value }],
  }));

  const closeTop = anchor ? anchor.y - origin.y : null;
  const closeLeft = anchor ? anchor.x - origin.x : null;
  const closePos =
    anchor && closeTop !== null && closeLeft !== null
      ? {
          top: closeTop,
          left: closeLeft,
          width: anchor.width,
          height: anchor.height,
        }
      : { right: 16, bottom: insets.bottom + 8, width: 50, height: 50 };
  const rowsPos =
    anchor && closeTop !== null && closeLeft !== null
      ? {
          top: 0,
          left: 0,
          right: 0,
          height: Math.max(0, closeTop),
          justifyContent: "flex-end" as const,
          alignItems: "flex-end" as const,
          paddingRight: Math.max(0, screenW - (closeLeft + anchor.width)),
          paddingBottom: 12,
        }
      : {
          right: 16,
          bottom: insets.bottom + 8 + 50 + 12,
          alignItems: "flex-end" as const,
        };

  if (!mounted) {
    return null;
  }

  return (
    <View
      ref={rootRef}
      onLayout={handleRootLayout}
      style={[StyleSheet.absoluteFill, styles.overlay]}
    >
      <Animated.View style={[StyleSheet.absoluteFill, backdropStyle]}>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Close menu"
          style={StyleSheet.absoluteFill}
          onPress={onClose}
        >
          <BlurView
            intensity={32}
            tint="light"
            style={StyleSheet.absoluteFill}
          />
          <View
            style={[
              StyleSheet.absoluteFill,
              { backgroundColor: "rgba(255,255,255,0.9)" },
            ]}
          />
        </Pressable>
      </Animated.View>

      <View pointerEvents="box-none" style={[styles.absolute, rowsPos]}>
        {actions.map((action, index) => (
          <AnimatedActionRow
            key={action.key}
            action={action}
            rank={actions.length - 1 - index}
            progress={progress}
            onRun={runAction}
          />
        ))}
      </View>

      <Animated.View style={[styles.absolute, closePos, closeStyle]}>
        <CloseButton onPress={onClose} />
      </Animated.View>
    </View>
  );
}

function CloseButton({ onPress }: { onPress: () => void }) {
  const [pressed, setPressed] = useState(false);
  return (
    <Pressable
      onPress={onPress}
      onPressIn={() => setPressed(true)}
      onPressOut={() => setPressed(false)}
      accessibilityRole="button"
      accessibilityLabel="Close menu"
      className="h-full w-full items-center justify-center"
      style={{
        borderRadius: 78,
        backgroundColor: "#f5f5f5",
        transform: [{ scale: pressed ? 0.96 : 1 }],
      }}
    >
      <X size={28} color="#3C3C43" strokeWidth={2} opacity={ICON_OPACITY} />
    </Pressable>
  );
}

const styles = StyleSheet.create({
  overlay: {
    zIndex: 50,
    elevation: 50,
  },
  absolute: {
    position: "absolute",
  },
});
