import { env } from "@/config/env";

/**
 * Centralized OneSignal wrapper — all OneSignal SDK access goes through this
 * module; never import "react-native-onesignal" elsewhere.
 *
 * Lazily loaded like expo-notifications above it in this folder: binaries
 * built before OneSignal was added (and OTA-updated bundles running on them)
 * lack the native module, so a top-level import would crash on boot.
 */
async function getOneSignal() {
  if (!env.oneSignalAppId) return null;
  try {
    return await import("react-native-onesignal");
  } catch {
    console.log("react-native-onesignal not available (old binary?)");
    return null;
  }
}

// The SDK drops calls made before initialize() (login forwards straight to
// native), and callers like PushTokenRegistrar run their effects before the
// root layout's init effect. Every non-init wrapper call waits for init to
// settle first — "settled" includes the skipped/unavailable paths, where the
// call then no-ops via getOneSignal.
let markInitSettled!: () => void;
const initSettled = new Promise<void>((resolve) => {
  markInitSettled = resolve;
});

/**
 * Initialize the OneSignal SDK. Call once on app boot, before rendering the
 * main content. No-ops when EXPO_PUBLIC_ONESIGNAL_APP_ID is unset or the
 * native module is missing. Does NOT request push permission itself — the
 * wallet-gated PushTokenRegistrar calls requestOneSignalPermission(), so
 * OneSignal owns the OS prompt.
 */
export async function initOneSignal(): Promise<void> {
  try {
    if (!env.oneSignalAppId) {
      console.log("[onesignal] EXPO_PUBLIC_ONESIGNAL_APP_ID not set, skipping");
      return;
    }
    const mod = await getOneSignal();
    if (!mod) return;

    if (__DEV__) {
      mod.OneSignal.Debug.setLogLevel(mod.LogLevel.Verbose);
    }
    mod.OneSignal.initialize(env.oneSignalAppId);
  } catch (error) {
    console.error("[onesignal] initialize failed:", error);
  } finally {
    markInitSettled();
  }
}

export async function loginOneSignal(externalId: string): Promise<void> {
  await initSettled;
  const mod = await getOneSignal();
  mod?.OneSignal.login(externalId);
}

export async function logoutOneSignal(): Promise<void> {
  await initSettled;
  const mod = await getOneSignal();
  mod?.OneSignal.logout();
}

export async function addOneSignalEmail(email: string): Promise<void> {
  await initSettled;
  const mod = await getOneSignal();
  mod?.OneSignal.User.addEmail(email);
}

export async function addOneSignalSms(phone: string): Promise<void> {
  await initSettled;
  const mod = await getOneSignal();
  mod?.OneSignal.User.addSms(phone);
}

export async function setOneSignalTags(
  tags: Record<string, string>,
): Promise<void> {
  await initSettled;
  const mod = await getOneSignal();
  mod?.OneSignal.User.addTags(tags);
}

/**
 * Show the OS push prompt via OneSignal. Users who previously denied are
 * NOT redirected to app settings (no fallback), so this is safe to call on
 * every cold start. Resolves to whether permission is granted; false when
 * the module is missing or the request fails.
 */
export async function requestOneSignalPermission(): Promise<boolean> {
  await initSettled;
  const mod = await getOneSignal();
  if (!mod) return false;
  try {
    return await mod.OneSignal.Notifications.requestPermission(false);
  } catch (error) {
    console.error("[onesignal] requestPermission failed:", error);
    return false;
  }
}

/**
 * The device's OneSignal push subscription ID, or null when OneSignal is
 * unavailable or has not assigned one yet (the first launch gets it
 * asynchronously; addOneSignalSubscriptionIdListener reports it then).
 */
export async function getOneSignalSubscriptionId(): Promise<string | null> {
  await initSettled;
  const mod = await getOneSignal();
  if (!mod) return null;
  try {
    return await mod.OneSignal.User.pushSubscription.getIdAsync();
  } catch (error) {
    console.error("[onesignal] getIdAsync failed:", error);
    return null;
  }
}

/**
 * Call `onId` whenever the push subscription changes and has an ID. Returns
 * an unsubscribe function, or null when OneSignal is unavailable.
 */
export async function addOneSignalSubscriptionIdListener(
  onId: (subscriptionId: string) => void,
): Promise<(() => void) | null> {
  await initSettled;
  const mod = await getOneSignal();
  if (!mod) return null;

  const handler = (event: { current: { id?: string } }) => {
    if (event.current.id) onId(event.current.id);
  };
  mod.OneSignal.User.pushSubscription.addEventListener("change", handler);
  return () => {
    mod.OneSignal.User.pushSubscription.removeEventListener("change", handler);
  };
}

/**
 * Listen for notification taps. Hands the payload's additionalData to
 * `onRoute`; returns an unsubscribe function, or null when OneSignal is
 * unavailable.
 */
export async function addOneSignalClickListener(
  onRoute: (route: string) => void,
): Promise<(() => void) | null> {
  await initSettled;
  const mod = await getOneSignal();
  if (!mod) return null;

  const handler = (event: { notification: { additionalData?: object } }) => {
    const data = event.notification.additionalData;
    const route = (data as { route?: unknown } | undefined)?.route;
    if (typeof route === "string") onRoute(route);
  };
  mod.OneSignal.Notifications.addEventListener("click", handler);
  return () => {
    mod.OneSignal.Notifications.removeEventListener("click", handler);
  };
}
