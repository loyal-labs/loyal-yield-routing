import Constants from "expo-constants";
import * as Device from "expo-device";

import { env } from "@/config/env";
import { earnHeaders, silentEarnToken } from "@/lib/solana/earn/earn-session";

/**
 * Lazily load expo-notifications to avoid crash in Expo Go (SDK 53+).
 * Returns null if the module can't be loaded.
 */
async function getNotificationsModule() {
  try {
    return await import("expo-notifications");
  } catch {
    console.log("expo-notifications not available (Expo Go?)");
    return null;
  }
}

/**
 * Configure notification display behavior. Call once on app boot.
 *
 * Also installs a default Android channel — Android 8+ drops notifications
 * silently if the sender doesn't target a registered channel, so we always
 * need at least one even for low-volume delivery.
 */
export async function setupNotificationHandler(): Promise<void> {
  const Notifications = await getNotificationsModule();
  if (!Notifications) return;

  Notifications.setNotificationHandler({
    handleNotification: async () => ({
      shouldShowAlert: true,
      shouldShowBanner: true,
      shouldShowList: true,
      shouldPlaySound: true,
      shouldSetBadge: true,
    }),
  });

  if (process.env.EXPO_OS === "android") {
    await Notifications.setNotificationChannelAsync("default", {
      name: "Default",
      importance: Notifications.AndroidImportance.HIGH,
      lockscreenVisibility: Notifications.AndroidNotificationVisibility.PUBLIC,
      sound: "default",
      vibrationPattern: [0, 250, 250, 250],
    });
  }
}

/**
 * Register for push notifications and return the Expo push token.
 * Returns null if registration fails, device doesn't support push, or running in Expo Go.
 */
export async function registerForPushNotifications(): Promise<string | null> {
  try {
    if (!Device.isDevice) {
      console.log("Push notifications require a physical device");
      return null;
    }

    const Notifications = await getNotificationsModule();
    if (!Notifications) return null;

    const { status: existingStatus } =
      await Notifications.getPermissionsAsync();
    let finalStatus = existingStatus;

    if (existingStatus !== "granted") {
      const { status } = await Notifications.requestPermissionsAsync();
      finalStatus = status;
    }

    if (finalStatus !== "granted") {
      console.log("Push notification permission not granted");
      return null;
    }

    // In standalone / dapp-store builds expoConfig.extra may be stripped
    // once the app goes through a real publish. Constants.easConfig is the
    // documented fallback — falling through to a hard-coded projectId is a
    // last-resort so we never quietly generate tokens under the wrong
    // Expo project (which silently fail to deliver).
    const projectId =
      Constants.expoConfig?.extra?.eas?.projectId ??
      (Constants as unknown as { easConfig?: { projectId?: string } })
        .easConfig?.projectId;

    if (!projectId) {
      console.error(
        "[push] Cannot generate Expo push token: no projectId in Constants",
      );
      return null;
    }

    const tokenData = await Notifications.getExpoPushTokenAsync({
      projectId,
    });

    return tokenData.data;
  } catch (error) {
    // Loud error so Datadog RUM's trackErrors picks it up — the
    // underlying cause (FCM misconfig, Google Play services unavailable,
    // network during token fetch) is invisible otherwise on prod builds.
    console.error("[push] getExpoPushTokenAsync failed:", error);
    return null;
  }
}

/**
 * Listen for notification taps. Returns a cleanup function, or null if unavailable.
 */
export async function addNotificationResponseListener(
  callback: (data: Record<string, unknown>) => void,
): Promise<(() => void) | null> {
  const Notifications = await getNotificationsModule();
  if (!Notifications) return null;

  const subscription =
    Notifications.addNotificationResponseReceivedListener((response) => {
      const data = response.notification.request.content.data;
      if (data) callback(data);
    });

  return () => subscription.remove();
}

/**
 * Send the push token to loyal-web, which keys it by the wallet in the
 * bearer. A wallet with no session token yet (an external wallet before its
 * first mint) is skipped without a prompt; the registrar retries on the next
 * app start.
 */
export async function registerPushToken(
  token: string,
  walletPublicKey: string,
): Promise<void> {
  try {
    const bearer = await silentEarnToken(walletPublicKey);
    if (!bearer) {
      console.log("[push] No Earn session for this wallet yet, skipping");
      return;
    }
    const response = await fetch(`${env.apiBaseUrl}/api/push-tokens`, {
      method: "POST",
      headers: { ...earnHeaders(), Authorization: `Bearer ${bearer}` },
      body: JSON.stringify({
        token,
        platform: process.env.EXPO_OS ?? "unknown",
      }),
    });
    if (!response.ok) {
      const bodyText = await response.text().catch(() => "");
      console.error(
        `[push] Backend rejected push token (${response.status}):`,
        bodyText.slice(0, 200),
      );
    }
  } catch (error) {
    console.error("[push] Failed to POST push token:", error);
  }
}
