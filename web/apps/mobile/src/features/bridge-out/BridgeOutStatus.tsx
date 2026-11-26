import { BottomSheetScrollView } from "@gorhom/bottom-sheet";
import { Alert, Linking } from "react-native";

import { MascotReveal, SendingSpinner } from "@/components/sheets/sheetFacts";
import { Text, View } from "@/tw";

import Failure from "../../../assets/images/wallet/send_error_dog.svg";
import Success from "../../../assets/images/wallet/send_success_dog.svg";
import { BridgeButton } from "./BridgeOutParts";
import {
  type OutStep,
  relayTransactionUrl,
} from "@/features/bridge-out/use-bridge-out";

type Result = Exclude<OutStep, { kind: "form" } | { kind: "sending" }>;

export function BridgeOutStatus({
  step,
  onDone,
}: {
  step: Result;
  onDone: () => void;
}) {
  const pending = step.kind === "bridging";
  const failed = step.kind === "failed";
  const href = relayTransactionUrl(step.requestId);
  const description = pending
    ? "You can close this screen and continue using the app"
    : failed
    ? step.message
    : null;
  return (
    <>
      <BottomSheetScrollView
        contentContainerStyle={{
          flexGrow: 1,
          alignItems: "center",
          justifyContent: "center",
          gap: 20,
          paddingHorizontal: 32,
          paddingVertical: 24,
        }}
      >
        {pending ? (
          <SendingSpinner />
        ) : (
          <MascotReveal key={step.kind}>
            {failed ? (
              <Failure width={100} height={80} />
            ) : (
              <Success width={100} height={80} />
            )}
          </MascotReveal>
        )}
        <View className="items-center gap-[4px]">
          <Text
            accessibilityLiveRegion="polite"
            className="text-center text-[22px] leading-[28px] font-semibold text-black"
          >
            {pending ? "Bridging..." : failed ? "Bridge failed" : "Bridged"}
          </Text>
          {!!description && (
            <Text className="max-w-[250px] text-center text-[17px] leading-[22px] text-[#8A8A8E]">
              {description}
            </Text>
          )}
        </View>
      </BottomSheetScrollView>
      <View className="gap-[10px] px-[20px] pt-[16px] pb-[16px]">
        <BridgeButton label="Done" onPress={onDone} />
        {href && (
          <BridgeButton
            label="View transaction"
            tone="secondary"
            onPress={() => {
              void Linking.openURL(href).catch(() =>
                Alert.alert("Could not open transaction", "Please try again."),
              );
            }}
          />
        )}
      </View>
    </>
  );
}
