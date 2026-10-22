import { View } from "react-native";
import Back from "../../../assets/images/earn-max/coin-back.svg";
import Middle from "../../../assets/images/earn-max/coin-middle.svg";
import Front from "../../../assets/images/earn-max/coin-front.svg";
export function EarnMaxIcon() {
  return (
    <View
      style={{
        width: 24,
        height: 24,
        backgroundColor: "black",
        borderRadius: 6,
        overflow: "hidden",
      }}
    >
      <View
        style={{
          position: "absolute",
          left: 7.5,
          top: 13.5,
          transform: [{ rotate: "65.34deg" }],
        }}
      >
        <Back />
      </View>
      <View
        style={{
          position: "absolute",
          left: 7.5,
          top: 5.5,
          transform: [{ rotate: "47.01deg" }],
        }}
      >
        <Middle />
      </View>
      <View
        style={{
          position: "absolute",
          left: 6,
          top: -4,
          transform: [{ rotate: "28.28deg" }],
        }}
      >
        <Front />
      </View>
    </View>
  );
}
