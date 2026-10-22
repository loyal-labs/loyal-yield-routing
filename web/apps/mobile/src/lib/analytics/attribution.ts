import * as Linking from "expo-linking";
import { Platform } from "react-native";

import {
  registerSuperProperties,
  setUserProfileOnce,
  updateUserProfile,
} from "@/lib/analytics/analytics";
import { mmkv } from "@/lib/storage";

const ATTRIBUTION_KEYS = [
  "twclid",
  "utm_source",
  "utm_medium",
  "utm_campaign",
  "utm_term",
  "utm_content",
] as const;

const INSTALL_REFERRER_CHECKED_KEY = "attribution.installReferrerChecked";

export function parseAttributionParams(
  input: string,
): Record<string, string> | null {
  const withoutFragment = input.split("#")[0];
  const queryIndex = withoutFragment.indexOf("?");
  const query =
    queryIndex >= 0 ? withoutFragment.slice(queryIndex + 1) : withoutFragment;
  const params = new URLSearchParams(query);
  const out: Record<string, string> = {};
  for (const key of ATTRIBUTION_KEYS) {
    const value = params.get(key);
    if (value) out[key] = value;
  }
  return Object.keys(out).length > 0 ? out : null;
}

function applyAttribution(source: string, params: Record<string, string>): void {
  registerSuperProperties(params);
  updateUserProfile({ ...params, attribution_source: source });
  const firstTouch: Record<string, string> = {
    initial_attribution_source: source,
  };
  for (const [key, value] of Object.entries(params)) {
    firstTouch[`initial_${key}`] = value;
  }
  setUserProfileOnce(firstTouch);
}

function captureFromUrl(url: string): void {
  const params = parseAttributionParams(url);
  if (params) applyAttribution("deep_link", params);
}

// Android only: the Play Install Referrer survives the store install, so it
// works without any URL open. Read once per install. iOS: no-op.
function captureInstallReferrerOnce(): void {
  if (Platform.OS !== "android") return;
  if (mmkv.getBoolean(INSTALL_REFERRER_CHECKED_KEY)) return;
  try {
    // Lazy require: binaries built before this dependency existed (OTA
    // recipients) lack the native module; the catch keeps them working.
    const { PlayInstallReferrer } =
      // eslint-disable-next-line @typescript-eslint/no-require-imports
      require("react-native-play-install-referrer") as typeof import("react-native-play-install-referrer");
    PlayInstallReferrer.getInstallReferrerInfo((info, error) => {
      if (error || !info) return;
      mmkv.setBoolean(INSTALL_REFERRER_CHECKED_KEY, true);
      const params = parseAttributionParams(info.installReferrer ?? "");
      if (params) applyAttribution("install_referrer", params);
    });
  } catch {
  }
}

export function initAttribution(): () => void {
  void Linking.getInitialURL().then((url) => {
    if (url) captureFromUrl(url);
  });
  const subscription = Linking.addEventListener("url", ({ url }) => {
    captureFromUrl(url);
  });
  captureInstallReferrerOnce();
  return () => subscription.remove();
}
