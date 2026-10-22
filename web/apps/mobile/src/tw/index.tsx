import {
  useCssElement,
  useNativeVariable as useFunctionalVariable,
} from "react-native-css";

import {
  resolveTextFontProps,
} from "@/tw/font-family";
import { Link as RouterLink } from "expo-router";
import Animated from "react-native-reanimated";
import React from "react";
import {
  Platform,
  View as RNView,
  Text as RNText,
  Pressable as RNPressable,
  ScrollView as RNScrollView,
  TouchableHighlight as RNTouchableHighlight,
  TextInput as RNTextInput,
  StyleSheet,
  type TextStyle,
  type ViewStyle,
} from "react-native";
import { SafeAreaView as RNSafeAreaView } from "react-native-safe-area-context";

const MONO_FONT_FAMILY = Platform.select({
  ios: "Menlo",
  default: "monospace",
});

export const Link = (
  props: React.ComponentProps<typeof RouterLink> & { className?: string }
) => {
  return useCssElement(RouterLink, props, { className: "style" });
};

Link.Trigger = RouterLink.Trigger;
Link.Menu = RouterLink.Menu;
Link.MenuAction = RouterLink.MenuAction;
Link.Preview = RouterLink.Preview;

export const useCSSVariable =
  process.env.EXPO_OS !== "web"
    ? useFunctionalVariable
    : (variable: string) => `var(${variable})`;

export type ViewProps = React.ComponentProps<typeof RNView> & {
  className?: string;
};

export const View = (props: ViewProps) => {
  return useCssElement(RNView, props, { className: "style" });
};
View.displayName = "CSS(View)";

export const Text = (
  props: React.ComponentProps<typeof RNText> & { className?: string }
) => {
  const { className, fontFamily } = resolveTextFontProps(
    props.className,
    StyleSheet.flatten(props.style as TextStyle | TextStyle[] | undefined)
      ?.fontFamily,
    MONO_FONT_FAMILY,
  );
  const style = fontFamily ? [{ fontFamily }, props.style] : props.style;
  return useCssElement(
    RNText,
    { ...props, className, style },
    { className: "style" },
  );
};
Text.displayName = "CSS(Text)";

export const ScrollView = (
  props: React.ComponentProps<typeof RNScrollView> & {
    className?: string;
    contentContainerClassName?: string;
  }
) => {
  return useCssElement(RNScrollView, props, {
    className: "style",
    contentContainerClassName: "contentContainerStyle",
  });
};
ScrollView.displayName = "CSS(ScrollView)";

export const Pressable = (
  props: React.ComponentProps<typeof RNPressable> & { className?: string }
) => {
  return useCssElement(RNPressable, props, { className: "style" });
};
Pressable.displayName = "CSS(Pressable)";

export const TextInput = (
  props: React.ComponentProps<typeof RNTextInput> & { className?: string }
) => {
  const { className, fontFamily } = resolveTextFontProps(
    props.className,
    StyleSheet.flatten(props.style as TextStyle | TextStyle[] | undefined)
      ?.fontFamily,
    MONO_FONT_FAMILY,
  );
  const style = fontFamily ? [{ fontFamily }, props.style] : props.style;
  return useCssElement(
    RNTextInput,
    { ...props, className, style },
    { className: "style" },
  );
};
TextInput.displayName = "CSS(TextInput)";

export const SafeAreaView = (
  props: React.ComponentProps<typeof RNSafeAreaView> & { className?: string }
) => {
  return useCssElement(RNSafeAreaView, props, { className: "style" });
};
SafeAreaView.displayName = "CSS(SafeAreaView)";

export const AnimatedScrollView = (
  props: React.ComponentProps<typeof Animated.ScrollView> & {
    className?: string;
    contentClassName?: string;
    contentContainerClassName?: string;
  }
) => {
  // @ts-expect-error: useCssElement generic type depth exceeds TS limit with Animated.ScrollView
  return useCssElement(Animated.ScrollView, props, {
    className: "style",
    contentClassName: "contentContainerStyle",
    contentContainerClassName: "contentContainerStyle",
  });
};

type RemappedHighlightStyle = ViewStyle &
  Partial<{
    underlayColor: React.ComponentProps<
      typeof RNTouchableHighlight
    >["underlayColor"];
  }>;

function XXTouchableHighlight(
  props: React.ComponentProps<typeof RNTouchableHighlight>
) {
  const { underlayColor, ...style } =
    (StyleSheet.flatten(props.style) || {}) as RemappedHighlightStyle;
  return (
    <RNTouchableHighlight
      underlayColor={underlayColor}
      {...props}
      style={style}
    />
  );
}

export const TouchableHighlight = (
  props: React.ComponentProps<typeof RNTouchableHighlight>
) => {
  return useCssElement(XXTouchableHighlight, props, { className: "style" });
};
TouchableHighlight.displayName = "CSS(TouchableHighlight)";
