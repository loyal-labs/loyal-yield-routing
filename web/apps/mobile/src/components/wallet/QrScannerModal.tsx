import { PostHogMaskView } from "posthog-react-native";
import { CameraView, type BarcodeScanningResult } from "expo-camera";
import { X } from "lucide-react-native";
import { useCallback } from "react";
import { Modal, StatusBar, StyleSheet, useWindowDimensions } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { Pressable, Text, View } from "@/tw";

export function QrScannerModal({
  visible,
  onClose,
  onScan,
  scanError,
  permissionGranted,
  canAskPermissionAgain,
  onRequestPermission,
}: {
  visible: boolean;
  onClose: () => void;
  onScan: (event: BarcodeScanningResult) => void;
  scanError: string | null;
  permissionGranted: boolean;
  canAskPermissionAgain: boolean;
  onRequestPermission: () => Promise<{ granted: boolean }>;
}) {
  const insets = useSafeAreaInsets();
  const { width: screenWidth, height: screenHeight } = useWindowDimensions();
  const frameSize = Math.min(280, Math.round(screenWidth * 0.72));
  const sideStripHeight = Math.max(0, (screenHeight - frameSize) / 2 - 40);

  const handleGrantPermission = useCallback(async () => {
    await onRequestPermission();
  }, [onRequestPermission]);

  return (
    <Modal
      visible={visible}
      animationType="fade"
      presentationStyle="overFullScreen"
      transparent
      statusBarTranslucent
      onRequestClose={onClose}
    >
      <StatusBar barStyle="light-content" />
      <PostHogMaskView style={qrScannerStyles.root}>
        {permissionGranted ? (
          <CameraView
            style={StyleSheet.absoluteFillObject}
            facing="back"
            barcodeScannerSettings={{ barcodeTypes: ["qr"] }}
            onBarcodeScanned={onScan}
          />
        ) : null}

        <View style={StyleSheet.absoluteFillObject} pointerEvents="none">
          <View style={[qrScannerStyles.dim, { height: sideStripHeight }]} />
          <View style={[qrScannerStyles.middleRow, { height: frameSize }]}>
            <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
            <View style={{ width: frameSize, height: frameSize }}>
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopLeftH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopLeftV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopRightH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketTopRightV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomLeftH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomLeftV]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomRightH]} />
              <View style={[qrScannerStyles.bracket, qrScannerStyles.bracketBottomRightV]} />
            </View>
            <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
          </View>
          <View style={[qrScannerStyles.dim, qrScannerStyles.flex1]} />
        </View>

        <View
          style={{
            position: "absolute",
            top: insets.top + 12,
            left: 0,
            right: 0,
            flexDirection: "row",
            alignItems: "center",
            justifyContent: "center",
            paddingHorizontal: 16,
          }}
        >
          <Text className="text-[17px] font-semibold text-white" style={{ lineHeight: 22 }}>
            Scan QR
          </Text>
          <Pressable
            onPress={onClose}
            accessibilityRole="button"
            accessibilityLabel="Close scanner"
            hitSlop={12}
            style={{
              position: "absolute",
              right: 16,
              top: -6,
              width: 36,
              height: 36,
              borderRadius: 18,
              alignItems: "center",
              justifyContent: "center",
              backgroundColor: "rgba(0,0,0,0.45)",
            }}
          >
            <X size={20} color="#fff" strokeWidth={2} />
          </Pressable>
        </View>

        <View
          style={{
            position: "absolute",
            bottom: insets.bottom + 28,
            left: 0,
            right: 0,
            paddingHorizontal: 32,
            alignItems: "center",
          }}
        >
          {permissionGranted ? (
            <Text className="text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
              Align a wallet QR code inside the frame
            </Text>
          ) : canAskPermissionAgain ? (
            <>
              <Text className="mb-3 text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
                Camera access is required to scan QR codes
              </Text>
              <Pressable
                onPress={handleGrantPermission}
                className="rounded-full bg-white px-5 py-2.5"
              >
                <Text className="text-[14px] font-semibold text-black">
                  Grant Camera Access
                </Text>
              </Pressable>
            </>
          ) : (
            <Text className="text-center text-[14px] text-white" style={{ opacity: 0.85 }}>
              Enable camera permission in device settings
            </Text>
          )}
          {scanError ? (
            <Text className="mt-3 text-center text-[13px] text-red-400">
              {scanError}
            </Text>
          ) : null}
        </View>
      </PostHogMaskView>
    </Modal>
  );
}

const SCANNER_DIM = "rgba(0, 0, 0, 0.65)";
const BRACKET_THICKNESS = 3;
const BRACKET_LENGTH = 26;
const BRACKET_OFFSET = -BRACKET_THICKNESS;

const qrScannerStyles = StyleSheet.create({
  root: { flex: 1, backgroundColor: "#000" },
  flex1: { flex: 1 },
  dim: { backgroundColor: SCANNER_DIM },
  middleRow: { flexDirection: "row" },
  bracket: { position: "absolute", backgroundColor: "#fff", borderRadius: 1.5 },
  bracketTopLeftH: {
    top: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketTopLeftV: {
    top: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketTopRightH: {
    top: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketTopRightV: {
    top: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketBottomLeftH: {
    bottom: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketBottomLeftV: {
    bottom: BRACKET_OFFSET,
    left: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
  bracketBottomRightH: {
    bottom: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_LENGTH,
    height: BRACKET_THICKNESS,
  },
  bracketBottomRightV: {
    bottom: BRACKET_OFFSET,
    right: BRACKET_OFFSET,
    width: BRACKET_THICKNESS,
    height: BRACKET_LENGTH,
  },
});
