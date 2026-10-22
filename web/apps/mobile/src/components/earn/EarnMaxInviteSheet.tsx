import {
  BottomSheetBackdrop,
  BottomSheetModal,
  BottomSheetTextInput,
  BottomSheetView,
} from "@gorhom/bottom-sheet";
import { useCallback, useEffect, useRef, useState } from "react";
import { ActivityIndicator, Pressable, StyleSheet, Text } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { useEarnMax } from "@/hooks/wallet/useEarnMax";
import type { Signer } from "@/lib/wallet/signer";

const COLOR_CHIP_BG = "#F2F2F7";
const COLOR_BODY_DIM = "rgba(60, 60, 67, 0.6)";
const COLOR_BRAND_RED = "#F9363C";

const usd = new Intl.NumberFormat("en-US", {
  currency: "USD",
  style: "currency",
});

export function EarnMaxInviteSheet({
  open,
  onClose,
  walletAddress,
  signer,
}: {
  open: boolean;
  onClose: () => void;
  walletAddress: string | null;
  signer: Signer | null;
}) {
  const sheetRef = useRef<BottomSheetModal>(null);
  const insets = useSafeAreaInsets();
  const earnMax = useEarnMax({
    walletAddress: open ? walletAddress : null,
    signer,
  });
  const [code, setCode] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [codeError, setCodeError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      sheetRef.current?.present();
    } else {
      sheetRef.current?.dismiss();
    }
  }, [open]);

  const { connect, redeem, refresh, read } = earnMax;
  const handleRedeem = useCallback(async () => {
    const trimmed = code.trim();
    if (!trimmed || submitting) {
      return;
    }
    setSubmitting(true);
    setCodeError(null);
    const result = await redeem(trimmed);
    setSubmitting(false);
    if (result === "redeemed") {
      setCode("");
    } else {
      setCodeError(
        result === "invalid"
          ? "Invalid code"
          : "Could not check the code. Try again.",
      );
    }
  }, [code, redeem, submitting]);

  const handleConnect = useCallback(async () => {
    if (submitting) {
      return;
    }
    setSubmitting(true);
    await connect();
    setSubmitting(false);
  }, [connect, submitting]);

  const handlePrimary = useCallback(() => {
    if (read?.kind === "invite_required") {
      void handleRedeem();
    } else if (read?.kind === "session_required") {
      void handleConnect();
    } else if (read?.kind === "failed") {
      void refresh();
    } else {
      sheetRef.current?.dismiss();
    }
  }, [handleConnect, handleRedeem, read, refresh]);

  const renderBackdrop = useCallback(
    (props: React.ComponentProps<typeof BottomSheetBackdrop>) => (
      <BottomSheetBackdrop
        {...props}
        disappearsOnIndex={-1}
        appearsOnIndex={0}
        opacity={0.2}
      />
    ),
    [],
  );

  const primaryLabel =
    read?.kind === "invite_required"
      ? "Unlock"
      : read?.kind === "session_required"
      ? "Sign to open Earn MAX"
      : read?.kind === "failed"
      ? "Try again"
      : "Done";

  return (
    <BottomSheetModal
      ref={sheetRef}
      enablePanDownToClose
      backdropComponent={renderBackdrop}
      onDismiss={onClose}
      handleComponent={null}
      backgroundStyle={styles.sheetBackground}
      keyboardBlurBehavior="restore"
    >
      <BottomSheetView style={styles.container}>
        <Text style={styles.heading}>Earn MAX</Text>

        {read === null ? (
          <ActivityIndicator style={styles.body} color="#000" />
        ) : read.kind === "invite_required" ? (
          <>
            <Text style={[styles.bodyText, styles.body]}>
              Enter your invite code to unlock Earn MAX.
            </Text>
            <BottomSheetTextInput
              value={code}
              onChangeText={setCode}
              onSubmitEditing={() => void handleRedeem()}
              placeholder="Invite code"
              placeholderTextColor={COLOR_BODY_DIM}
              autoCapitalize="characters"
              autoCorrect={false}
              editable={!submitting}
              accessibilityLabel="Invite code"
              style={styles.input}
            />
            {codeError ? (
              <Text style={styles.errorText}>{codeError}</Text>
            ) : null}
          </>
        ) : read.kind === "session_required" ? (
          <>
            <Text style={[styles.bodyText, styles.body]}>
              Sign a message with your wallet to open Earn MAX.
            </Text>
            {read.notice ? (
              <Text style={styles.errorText}>{read.notice}</Text>
            ) : null}
          </>
        ) : read.kind === "failed" ? (
          <Text style={[styles.bodyText, styles.body]}>{read.message}</Text>
        ) : (
          <Text style={[styles.bodyText, styles.body]}>
            Balance {usd.format(read.summary.summary?.balanceUsd ?? 0)}
          </Text>
        )}

        <Pressable
          onPress={handlePrimary}
          disabled={submitting}
          accessibilityRole="button"
          accessibilityLabel={primaryLabel}
          style={({ pressed }) => [
            styles.cta,
            { marginBottom: insets.bottom + 12 },
            (pressed || submitting) && styles.ctaPressed,
          ]}
        >
          {submitting ? (
            <ActivityIndicator color="#FFF" />
          ) : (
            <Text style={styles.ctaLabel}>{primaryLabel}</Text>
          )}
        </Pressable>
      </BottomSheetView>
    </BottomSheetModal>
  );
}

const styles = StyleSheet.create({
  sheetBackground: {
    backgroundColor: "#FFF",
    borderTopLeftRadius: 38,
    borderTopRightRadius: 38,
  },
  container: {
    paddingHorizontal: 20,
    paddingTop: 28,
  },
  heading: {
    fontFamily: "Geist_600SemiBold",
    fontSize: 24,
    lineHeight: 29,
    letterSpacing: -0.48,
    color: "#000",
    paddingHorizontal: 2,
  },
  body: {
    paddingTop: 16,
    paddingBottom: 20,
    paddingHorizontal: 2,
  },
  bodyText: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_BODY_DIM,
  },
  input: {
    height: 50,
    borderRadius: 16,
    backgroundColor: COLOR_CHIP_BG,
    paddingHorizontal: 16,
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    color: "#000",
  },
  errorText: {
    fontFamily: "Geist_400Regular",
    fontSize: 15,
    lineHeight: 20,
    color: COLOR_BRAND_RED,
    paddingTop: 8,
    paddingHorizontal: 2,
  },
  cta: {
    height: 50,
    borderRadius: 78,
    backgroundColor: "#000",
    alignItems: "center",
    justifyContent: "center",
    marginTop: 20,
  },
  ctaPressed: {
    opacity: 0.85,
  },
  ctaLabel: {
    fontFamily: "Geist_500Medium",
    fontSize: 17,
    lineHeight: 22,
    color: "#FFF",
  },
});
