import { StyleSheet, View } from "react-native";

import { Text } from "@/tw";

import type { WalletCategory } from "../model/categorize";
import { splitUsd } from "../model/format";
import { CryptoGlyph, StablecoinsGlyph } from "./CategoryGlyphs";

const MUTED = "rgba(60, 60, 67, 0.6)";
const CENTS_DIM = "rgba(60, 60, 67, 0.4)";

export function CategoryCardFace({
  category,
  title,
  usd,
}: {
  category: WalletCategory;
  title: string;
  usd: number;
}) {
  const { whole, cents } = splitUsd(usd);
  return (
    <>
      <View>
        {category === "stablecoins" ? (
          <StablecoinsGlyph size={40} />
        ) : (
          <CryptoGlyph size={40} />
        )}
      </View>
      <View style={{ gap: 4 }}>
        <Text className="text-[15px]" style={{ color: MUTED, lineHeight: 20 }}>
          {title}
        </Text>
        <Text style={styles.faceValue} numberOfLines={1}>
          {whole}
          <Text style={styles.faceCents}>{cents}</Text>
        </Text>
      </View>
    </>
  );
}

const styles = StyleSheet.create({
  faceValue: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 20,
    lineHeight: 24,
    letterSpacing: -0.22,
    color: "#000000",
  },
  faceCents: {
    fontFamily: "Geist_600SemiBold",
    color: CENTS_DIM,
  },
});
