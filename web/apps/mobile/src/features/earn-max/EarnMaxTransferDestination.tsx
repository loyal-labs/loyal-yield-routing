import { Image, Text, View } from "react-native";
import Loop from "../../../assets/images/earn-max/loop.svg";
import Stablecoins from "../../../assets/images/earn-max/stablecoins.svg";
import WithdrawalLoop from "../../../assets/images/earn-max/withdraw-loop.svg";
export function EarnMaxTransferDestination({
  withdrawal = false,
  apyBps,
  walletAddress,
  walletBalance = 0,
}: {
  withdrawal?: boolean;
  apyBps?: number | null;
  walletAddress?: string | null;
  walletBalance?: number;
}) {
  if (!withdrawal) {
    return (
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: 12,
          paddingHorizontal: 16,
          height: 60,
        }}
      >
        <View
          style={{
            width: 48,
            height: 48,
            borderRadius: 12,
            backgroundColor: "rgba(0,0,0,0.04)",
            alignItems: "center",
            justifyContent: "center",
          }}
        >
          <Loop />
        </View>
        <View style={{ flex: 1, alignItems: "flex-start" }}>
          <Text
            style={{
              fontFamily: "Geist_400Regular",
              fontSize: 15,
              lineHeight: 20,
              color: "rgba(60,60,67,0.6)",
            }}
          >
            to Earn MAX · RWA Loop
          </Text>
          <View
            style={{
              borderRadius: 8,
              backgroundColor: "rgba(52,199,89,0.14)",
              paddingHorizontal: 8,
              paddingVertical: 2,
            }}
          >
            <Text
              style={{
                fontFamily: "Geist_500Medium",
                fontSize: 16,
                lineHeight: 20,
                letterSpacing: 0.06,
                color: "#4BB34B",
              }}
            >
              {apyBps == null ? "—" : `${(apyBps / 100).toFixed(2)}% APY`}
            </Text>
          </View>
        </View>
      </View>
    );
  }
  const [whole, cents] = walletBalance
    .toLocaleString("en-US", {
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    })
    .split(".");
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 12,
        paddingHorizontal: 16,
        height: 60,
      }}
    >
      <Stablecoins />
      <View style={{ flex: 1 }}>
        <Text
          style={{
            fontFamily: "Geist_400Regular",
            fontSize: 15,
            lineHeight: 20,
            color: "rgba(60,60,67,0.6)",
          }}
          numberOfLines={1}
        >
          to Stablecoins
          {walletAddress
            ? ` · ${walletAddress.slice(0, 4)}…${walletAddress.slice(-4)}`
            : ""}
        </Text>
        <Text
          style={{
            fontFamily: "Geist_600SemiBold",
            fontSize: 20,
            lineHeight: 24,
            color: "#000",
          }}
        >
          ${whole}
          <Text style={{ color: "rgba(60,60,67,0.4)" }}>.{cents}</Text>
        </Text>
      </View>
    </View>
  );
}

export function EarnMaxWithdrawalIcon() {
  return (
    <View style={{ width: 48, height: 48 }}>
      <Image
        source={require("../../../assets/images/earn/usdc.png")}
        style={{ width: 32, height: 32, borderRadius: 16 }}
      />
      <View
        style={{
          position: "absolute",
          right: 0,
          bottom: 0,
          width: 32,
          height: 32,
          borderRadius: 16,
          backgroundColor: "#F9363C",
          alignItems: "center",
          justifyContent: "center",
        }}
      >
        <WithdrawalLoop />
      </View>
    </View>
  );
}
