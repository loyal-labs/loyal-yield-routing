import { useState } from "react";

import { Pressable, ScrollView, Text, View } from "@/tw";

type Props = {
  secretKeyHex: string;
  onComplete: () => void;
};

export function SeedBackupScreen({ secretKeyHex, onComplete }: Props) {
  const [revealed, setRevealed] = useState(false);

  return (
    <ScrollView className="flex-1 bg-white" contentContainerClassName="p-6 pt-20 gap-4">
      <Text className="text-[22px] font-semibold">Back up your secret key</Text>
      <Text className="text-[15px] text-black/60">
        Your secret key is the only way to recover this wallet. It stays on this
        phone and is not backed up anywhere else, so if you lose or reset the
        phone, this key is the only way back in. Write it down or store it in a
        password manager.
      </Text>

      {revealed ? (
        <View className="gap-3 rounded-2xl bg-black/5 p-4">
          <Text selectable className="font-mono text-[13px]">
            {secretKeyHex}
          </Text>
        </View>
      ) : (
        <Pressable
          onPress={() => setRevealed(true)}
          className="items-center rounded-2xl bg-black/5 p-4"
        >
          <Text className="text-[15px] font-semibold">Show secret key</Text>
        </Pressable>
      )}

      <Pressable
        disabled={!revealed}
        onPress={onComplete}
        className={`items-center rounded-2xl p-4 ${revealed ? "bg-black" : "bg-black/20"}`}
      >
        <Text className="text-[15px] font-semibold text-white">
          I saved my secret key
        </Text>
      </Pressable>
    </ScrollView>
  );
}
