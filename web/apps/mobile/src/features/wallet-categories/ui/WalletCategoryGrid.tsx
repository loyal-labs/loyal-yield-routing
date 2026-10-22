import { ChevronRight, Plus } from "lucide-react-native";
import { Pressable, Text, View } from "react-native";
import { Skeleton } from "@/components/Skeleton";
import { EarnMaxIcon } from "@/features/earn-max/EarnMaxIcon";
import type { CardSourceRect } from "../routes";
import { CryptoGlyph, EarnGlyph, StablecoinsGlyph } from "./CategoryGlyphs";
import { formatApyBps, splitUsd } from "../model/format";
function Balance({ value }: { value: number }) {
  const { whole, cents } = splitUsd(value);
  return (
    <Text
      style={{ fontFamily: "Geist_600SemiBold", fontSize: 20, lineHeight: 24 }}
    >
      {whole}
      <Text style={{ color: "#AFADB3" }}>{cents}</Text>
    </Text>
  );
}
function ApyBadge({
  apy,
  compact = false,
  loading = false,
}: {
  apy: number | null;
  compact?: boolean;
  loading?: boolean;
}) {
  if (loading && apy == null) {
    return (
      <Skeleton
        style={{ width: compact ? 64 : 80, height: compact ? 16 : 24 }}
      />
    );
  }
  return (
    <View
      style={{
        alignSelf: "flex-start",
        backgroundColor: "rgba(52,199,89,0.14)",
        borderRadius: compact ? 6 : 8,
        paddingHorizontal: compact ? 4 : 8,
        paddingVertical: compact ? 1 : 2,
      }}
    >
      <Text
        style={{
          color: "#4BB34B",
          fontFamily: "Geist_500Medium",
          fontSize: compact ? 11 : 16,
          lineHeight: compact ? 14 : 20,
        }}
      >
        {apy == null ? "—" : formatApyBps(apy)}
        {compact ? " APY" : ""}
      </Text>
    </View>
  );
}
export function WalletCategoryGrid({
  earnUsd,
  earnMonthUsd,
  earnLoading,
  earnApyBps,
  earnMaxUsd,
  earnMaxLoading,
  earnMaxApyBps,
  stablecoinsUsd,
  cryptoUsd,
  onPressEarn,
  onPressEarnMax,
  onPressStablecoins,
  onPressCrypto,
}: {
  earnUsd: number;
  earnMonthUsd: number | null;
  earnLoading?: boolean;
  earnApyBps: number | null;
  earnMaxUsd: number | null;
  earnMaxLoading?: boolean;
  earnMaxApyBps: number | null;
  stablecoinsUsd: number;
  cryptoUsd: number;
  onPressEarn: () => void;
  onPressEarnMax: () => void;
  onPressStablecoins: (rect?: CardSourceRect) => void;
  onPressCrypto: (rect?: CardSourceRect) => void;
}) {
  return (
    <View style={{ paddingHorizontal: 16, gap: 8, flex: 1 }}>
      <View style={{ flexDirection: "row", gap: 8, paddingVertical: 8 }}>
        {[
          {
            label: "Stablecoins",
            value: stablecoinsUsd,
            Glyph: StablecoinsGlyph,
            onPress: onPressStablecoins,
          },
          {
            label: "Crypto",
            value: cryptoUsd,
            Glyph: CryptoGlyph,
            onPress: onPressCrypto,
          },
        ].map(({ label, value, Glyph, onPress }) => (
          <Pressable
            key={label}
            accessibilityRole="button"
            onPress={() => onPress()}
            style={{
              flex: 1,
              gap: 4,
              paddingVertical: 8,
              paddingHorizontal: 16,
            }}
          >
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: label === "Crypto" ? 6 : 8,
              }}
            >
              <Glyph size={24} />
              <View style={{ flexDirection: "row", alignItems: "center" }}>
                <Text
                  style={{
                    color: "#8A8A8E",
                    fontFamily: "Geist_400Regular",
                    fontSize: 15,
                    lineHeight: 20,
                  }}
                >
                  {label}
                </Text>
                <ChevronRight size={16} color="#AFADB3" />
              </View>
            </View>
            <Balance value={value} />
          </Pressable>
        ))}
      </View>
      <View style={{ flexDirection: "row", gap: 8, flex: 1, minHeight: 240 }}>
        {[
          {
            label: "Earn",
            value: earnUsd,
            apy: earnApyBps,
            loading: earnLoading,
            earned: earnMonthUsd,
            onPress: onPressEarn,
          },
          {
            label: "Earn MAX",
            value: earnMaxUsd,
            apy: earnMaxApyBps,
            loading: earnMaxLoading,
            earned: null,
            onPress: onPressEarnMax,
          },
        ].map(({ label, value, apy, loading, earned, onPress }) => (
          <Pressable
            key={label}
            accessibilityRole="button"
            onPress={onPress}
            style={{
              flex: 1,
              padding: 16,
              borderRadius: 26,
              backgroundColor: "rgba(0,0,0,0.04)",
              justifyContent: "space-between",
            }}
          >
            <View
              style={{ flexDirection: "row", alignItems: "center", gap: 8 }}
            >
              {label === "Earn" ? <EarnGlyph size={24} /> : <EarnMaxIcon />}
              <Text
                style={{
                  fontFamily: "Geist_500Medium",
                  fontSize: 15,
                  lineHeight: 20,
                }}
              >
                {label}
              </Text>
            </View>
            {!value && !loading ? (
              <View
                style={{
                  flex: 1,
                  alignItems: "center",
                  justifyContent: "center",
                }}
              >
                <View style={{ height: 36, alignItems: "center" }}>
                  <Plus size={36} color="#AFADB3" strokeWidth={2} />
                  <Text
                    style={{
                      position: "absolute",
                      top: 40,
                      width: 140,
                      textAlign: "center",
                      color: "#8A8A8E",
                      fontFamily: "Geist_400Regular",
                      fontSize: 15,
                      lineHeight: 20,
                    }}
                  >
                    Start earning
                  </Text>
                </View>
              </View>
            ) : (
              <View style={{ flex: 1 }} />
            )}
            <View
              style={{
                gap: 4,
                alignSelf: "stretch",
                minHeight: 64,
                justifyContent: "flex-end",
              }}
            >
              {value || loading ? (
                <>
                  <ApyBadge apy={apy} compact loading={loading} />
                  {loading ? (
                    <Skeleton style={{ width: 96, height: 24 }} />
                  ) : (
                    <Balance value={value!} />
                  )}
                  {loading && label === "Earn" && earned === null ? (
                    <Skeleton style={{ width: 100, height: 20 }} />
                  ) : earned !== null ? (
                    <Text
                      style={{
                        color: earned < 0 ? "#F9363C" : "#4BB34B",
                        fontFamily: "Geist_400Regular",
                        fontSize: 15,
                        lineHeight: 20,
                      }}
                    >
                      {earned < 0 ? "−" : "+"}${Math.abs(earned).toFixed(2)}{" "}
                      (30D)
                    </Text>
                  ) : null}
                </>
              ) : (
                <>
                  <Text
                    style={{
                      color: "#8A8A8E",
                      fontFamily: "Geist_400Regular",
                      fontSize: 15,
                      lineHeight: 20,
                    }}
                  >
                    Average APY
                  </Text>
                  <ApyBadge apy={apy} />
                </>
              )}
            </View>
          </Pressable>
        ))}
      </View>
    </View>
  );
}
