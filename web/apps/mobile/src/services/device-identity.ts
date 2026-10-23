// Stable per-install identity for telemetry (ASK-2097): a UUID generated on
// first launch and persisted in MMKV, plus the OS fleet the device belongs
// to. This is the join key that lets ClickStack pull a device's whole
// telemetry trail across sessions and wallets — the wallet address alone
// misses everything that happens before auth, and the error channel carried
// no correlator at all.

import { isCanonicalUuidV4 } from "@loyal-labs/shared";
import { Platform } from "react-native";

import { mmkv } from "@/lib/storage";

export type MobileDevicePlatform = "android" | "ios";

const DEVICE_ID_KEY = "observability.deviceId";

function generateUuid(): string {
  const cryptoApi = (globalThis as { crypto?: { randomUUID?: () => string } })
    .crypto;
  if (cryptoApi?.randomUUID) return cryptoApi.randomUUID();
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (char) => {
    const random = Math.trunc(Math.random() * 16);
    const value = char === "x" ? random : (random % 4) + 8;
    return value.toString(16);
  });
}

export function getObservabilityDeviceId(): string {
  const existing = mmkv.getString(DEVICE_ID_KEY);
  if (existing && isCanonicalUuidV4(existing)) return existing;
  const generated = generateUuid();
  mmkv.setString(DEVICE_ID_KEY, generated);
  return generated;
}

export function getObservabilityDevicePlatform(): MobileDevicePlatform {
  return Platform.OS === "ios" ? "ios" : "android";
}
