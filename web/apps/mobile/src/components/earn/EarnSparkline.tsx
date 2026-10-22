import { useIsFocused } from "@react-navigation/native";
import { useEffect, useMemo, useState } from "react";
import { StyleSheet, Text, View } from "react-native";
import Animated, {
  cancelAnimation,
  Easing,
  useAnimatedStyle,
  useSharedValue,
  withDelay,
  withTiming,
} from "react-native-reanimated";

import type { EarnForecastSummary } from "@/lib/solana/earn/earn-api";

import { getLoyalSamples } from "./earnForecastModel";

const WINDOW_MS = 30 * 24 * 60 * 60 * 1000;
const BAR_COUNT = 30;
const SMOOTH_RADIUS = 1;
const BAR_GAP = 6;
const BAR_RADIUS = 6;
const MIN_BAR = 4;
const TOP_INSET = 2;
const BAR_COLOR = "rgba(50, 182, 124, 0.6)";
const AXIS_DIM = "rgba(60, 60, 67, 0.4)";
const BAR_DURATION = 420;
const BAR_STAGGER = 16;

type BarsData = { norm: number[]; minPct: number; maxPct: number };

function parseTime(observedAt: string): number {
  const t = Date.parse(observedAt);
  return Number.isFinite(t) ? t : 0;
}

function resampleArray(values: number[], n: number): number[] {
  if (values.length <= 1 || n <= 1) {
    return values.slice();
  }
  return Array.from({ length: n }, (_, i) => {
    const idx = Math.round((i / (n - 1)) * (values.length - 1));
    return values[idx];
  });
}

function movingAverage(values: number[], radius: number): number[] {
  const n = values.length;
  return values.map((_, i) => {
    let sum = 0;
    let count = 0;
    for (let j = i - radius; j <= i + radius; j += 1) {
      if (j >= 0 && j < n) {
        sum += values[j];
        count += 1;
      }
    }
    return count > 0 ? sum / count : values[i];
  });
}

function buildBars(summary: EarnForecastSummary | null): BarsData | null {
  const all = getLoyalSamples(summary);
  if (all.length === 0) {
    return null;
  }
  const sorted = [...all].sort(
    (a, b) => parseTime(a.observedAt) - parseTime(b.observedAt),
  );
  const newest = parseTime(sorted[sorted.length - 1].observedAt);
  const cutoff = newest - WINDOW_MS;
  let windowed = sorted.filter((s) => parseTime(s.observedAt) >= cutoff);
  if (windowed.length < 2) {
    windowed = sorted.slice(-BAR_COUNT);
  }
  const raw = windowed.map((s) => s.apyBps / 100);
  if (raw.length === 0) {
    return null;
  }
  const pct = movingAverage(resampleArray(raw, BAR_COUNT), SMOOTH_RADIUS);
  const minPct = Math.min(...pct);
  const maxPct = Math.max(...pct);
  const span = maxPct - minPct;
  return {
    norm: pct.map((p) => (span > 0 ? (p - minPct) / span : 0.5)),
    minPct,
    maxPct,
  };
}

function formatPct(value: number): string {
  return `${value.toFixed(2)}%`;
}

function Bar({ height, delay }: { height: number; delay: number }) {
  const grow = useSharedValue(0);
  useEffect(() => {
    grow.value = withDelay(
      delay,
      withTiming(1, { duration: BAR_DURATION, easing: Easing.out(Easing.cubic) }),
    );
    return () => cancelAnimation(grow);
  }, [grow, delay]);
  const style = useAnimatedStyle(() => ({ height: height * grow.value }));
  return <Animated.View style={[styles.bar, style]} />;
}

export function EarnSparkline({
  summary,
  replayKey = 0,
}: {
  summary: EarnForecastSummary | null;
  replayKey?: number;
}) {
  const [chartHeight, setChartHeight] = useState(0);
  const data = useMemo(() => buildBars(summary), [summary]);
  const isFocused = useIsFocused();

  const heights = useMemo(() => {
    if (!data || chartHeight <= 0) {
      return null;
    }
    const usable = Math.max(0, chartHeight - TOP_INSET - MIN_BAR);
    return data.norm.map((n) => MIN_BAR + n * usable);
  }, [data, chartHeight]);

  return (
    <View style={styles.root} pointerEvents="none">
      <View style={styles.axisTop}>
        <Text style={[styles.axisLabel, styles.axisTitle]}>30D APY</Text>
        <Text style={styles.axisLabel}>
          {data ? formatPct(data.maxPct) : ""}
        </Text>
      </View>
      <View
        style={styles.chart}
        onLayout={(e) => setChartHeight(e.nativeEvent.layout.height)}
      >
        {heights && isFocused ? (
          <View key={replayKey} style={styles.bars}>
            {heights.map((h, i) => (
              <Bar key={i} height={h} delay={i * BAR_STAGGER} />
            ))}
          </View>
        ) : null}
      </View>
      <View style={styles.axisBottom}>
        <Text style={styles.axisLabel}>
          {data ? formatPct(data.minPct) : ""}
        </Text>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  root: {
    flex: 1,
    alignSelf: "stretch",
  },
  axisTop: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
  },
  axisBottom: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "flex-end",
  },
  chart: {
    flex: 1,
    minHeight: 56,
    alignSelf: "stretch",
  },
  bars: {
    flex: 1,
    flexDirection: "row",
    alignItems: "flex-end",
    gap: BAR_GAP,
  },
  bar: {
    flex: 1,
    minWidth: 1,
    borderRadius: BAR_RADIUS,
    backgroundColor: BAR_COLOR,
  },
  axisLabel: {
    fontFamily: "Geist_400Regular",
    fontSize: 13,
    lineHeight: 16,
    color: AXIS_DIM,
  },
  axisTitle: {
    fontFamily: "Geist_500Medium",
  },
});
