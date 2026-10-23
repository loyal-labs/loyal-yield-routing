import * as Clipboard from "expo-clipboard";
import { Stack, useRouter } from "expo-router";
import { useCallback, useState } from "react";
import { Alert } from "react-native";

import {
  confirmSeedBackupAndDeleteICloudCopies,
  isICloudBackupEnabled,
} from "@/lib/wallet/icloud-backup";
import { isICloudSyncSupported } from "@/lib/wallet/keypair-storage";
import { useWallet } from "@/lib/wallet/wallet-provider";
import { Pressable, ScrollView, Text, View } from "@/tw";

export default function SeedBackupScreen() {
  const wallet = useWallet();
  const router = useRouter();
  const [secretKey, setSecretKey] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const hasICloudCopies = isICloudSyncSupported() && isICloudBackupEnabled();

  const handleReveal = useCallback(() => {
    const hex = wallet.getSecretKeyHex();
    if (!hex) {
      Alert.alert("Wallet locked", "Unlock your wallet to see your secret key.");
      return;
    }
    setSecretKey(hex);
  }, [wallet]);

  const handleConfirm = useCallback(async () => {
    setBusy(true);
    try {
      await confirmSeedBackupAndDeleteICloudCopies();
      router.back();
    } catch (error) {
      console.warn("[wallet] seed backup confirm failed", error);
      Alert.alert("Error", "Could not finish. Your iCloud copies were kept.");
    } finally {
      setBusy(false);
    }
  }, [router]);

  return (
    <ScrollView className="flex-1 bg-white" contentContainerClassName="p-5 gap-4">
      <Stack.Screen options={{ title: "Back Up Secret Key" }} />
      <Text className="text-[17px] font-semibold">
        Your secret key is the only way to recover this wallet.
      </Text>
      <Text className="text-[15px] text-black/60">
        {hasICloudCopies
          ? "Write it down or store it in a password manager. Once you confirm, Loyal removes the copies of this wallet it keeps in iCloud, so a lost phone can only be recovered with this key."
          : "Write it down or store it in a password manager. If you lose this phone, this key is the only way back into your wallet."}
      </Text>

      {secretKey ? (
        <View className="gap-3 rounded-2xl bg-black/5 p-4">
          <Text selectable className="font-mono text-[13px]">
            {secretKey}
          </Text>
          <Pressable onPress={() => Clipboard.setStringAsync(secretKey)}>
            <Text className="text-[15px] font-semibold text-[#007AFF]">Copy</Text>
          </Pressable>
        </View>
      ) : (
        <Pressable
          onPress={handleReveal}
          className="items-center rounded-2xl bg-black/5 p-4"
        >
          <Text className="text-[15px] font-semibold">Show secret key</Text>
        </Pressable>
      )}

      <Pressable
        disabled={!secretKey || busy}
        onPress={() => void handleConfirm()}
        className={`items-center rounded-2xl p-4 ${secretKey && !busy ? "bg-black" : "bg-black/20"}`}
      >
        <Text className="text-[15px] font-semibold text-white">
          {hasICloudCopies
            ? "I saved it. Remove iCloud copies"
            : "I saved my secret key"}
        </Text>
      </Pressable>
      <Pressable onPress={() => router.back()} className="items-center p-2">
        <Text className="text-[15px] text-black/60">Not now</Text>
      </Pressable>
    </ScrollView>
  );
}
