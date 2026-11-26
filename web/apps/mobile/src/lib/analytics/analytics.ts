import { MOBILE_DISTINCT_ID_PREFIX } from "@loyal-labs/shared";
import { isRunningInExpoGo } from "expo";
import PostHog from "posthog-react-native";
import { Platform } from "react-native";

import { env } from "@/config/env";
import { clearDatadogUser, identifyDatadogUser } from "@/lib/datadog/datadog";
import { mmkv } from "@/lib/storage";

const WORKSPACE = "mobile";
const OPT_IN_KEY = "settings_analytics_opt_in";

type AnalyticsProperties = NonNullable<Parameters<PostHog["capture"]>[1]>;
type WalletSource =
  | "created"
  | "imported"
  | "vault"
  | "mwa"
  | "deeplink"
  | "privy";
type WalletIdentity = { address: string; source: WalletSource };

let client: PostHog | undefined;
let wallet: WalletIdentity | undefined;
let replayWallet: string | undefined;

export function analyticsEnabled(): boolean {
  return mmkv.getBoolean(OPT_IN_KEY) ?? true;
}

function reportFailure(): void {
  console.warn("[analytics] PostHog operation failed");
}

// FNV-1a over the existing wallet identity gives the same bucket on both platforms.
export function inReplayCohort(distinctId: string): boolean {
  let hash = 2166136261;
  for (let i = 0; i < distinctId.length; i++) {
    hash = Math.imul(hash ^ distinctId.charCodeAt(i), 16777619);
  }
  return (hash >>> 0) % 5 === 0;
}

function getClient(): PostHog | undefined {
  if (
    !env.posthogKey ||
    !analyticsEnabled() ||
    (Platform.OS !== "ios" && Platform.OS !== "android")
  )
    return;
  if (!client) {
    client = new PostHog(env.posthogKey, {
      host: env.posthogHost,
      customStorage: {
        getItem: (key) => mmkv.getString(key) ?? null,
        setItem: mmkv.setString,
      },
      captureAppLifecycleEvents: false,
      capturePushNotificationOpened: false,
      capturePushNotificationSubscriptions: false,
      disableSurveys: true,
      errorTracking: { autocapture: false },
      // Wait for the current wallet before starting the native recorder.
      enableSessionReplay: false,
      sessionReplayConfig: {
        sampleRate: 1,
        maskAllTextInputs: true,
        maskAllImages: true,
        maskAllSandboxedViews: true,
        captureTouches: false,
        captureLog: false,
        captureNetworkTelemetry: false,
      },
    });
    void client.register({ workspace: WORKSPACE }).catch(reportFailure);
  }
  return client;
}

function withClient(action: (c: PostHog) => void | Promise<void>): void {
  try {
    const c = getClient();
    if (c && !c.optedOut) void Promise.resolve(action(c)).catch(reportFailure);
  } catch {
    reportFailure();
  }
}

export function initAnalytics(): void {
  withClient((c) => c.ready());
}

export function getWorkspaceEventName(event: string): string {
  const prefix = `[${WORKSPACE}] `;
  return event.startsWith(prefix) ? event : `${prefix}${event}`;
}

export function registerSuperProperties(properties: AnalyticsProperties): void {
  withClient((c) => c.register(properties));
}

export function track(event: string, properties?: AnalyticsProperties): void {
  withClient((c) => c.capture(getWorkspaceEventName(event), properties));
}

function stopReplay(c: PostHog): void {
  if (!replayWallet) return;
  replayWallet = undefined;
  void c.stopSessionRecording().catch(reportFailure);
}

function resetClient(c: PostHog): void {
  stopReplay(c);
  c.reset();
  void c.register({ workspace: WORKSPACE }).catch(reportFailure);
}

function bindWallet(c: PostHog, { address, source }: WalletIdentity): void {
  const id = MOBILE_DISTINCT_ID_PREFIX + address;
  const previous = c.getDistinctId();
  if (previous.startsWith(MOBILE_DISTINCT_ID_PREFIX) && previous !== id)
    resetClient(c);
  c.identify(id, {
    wallet_address: address,
    wallet_source: source,
    identity_provider: WORKSPACE,
    last_workspace: WORKSPACE,
  });
  if (
    replayWallet !== id &&
    inReplayCohort(id) &&
    !isRunningInExpoGo() &&
    (Platform.OS !== "android" || Number(Platform.Version) >= 26)
  ) {
    replayWallet = id;
    void c.startSessionRecording().catch(reportFailure);
  }
}

export function identifyWallet(publicKey: string, source: WalletSource): void {
  const identity = { address: publicKey, source };
  wallet = identity;
  identifyDatadogUser({
    id: MOBILE_DISTINCT_ID_PREFIX + publicKey,
    extraInfo: {
      wallet_address: publicKey,
      wallet_source: source,
      workspace: WORKSPACE,
    },
  });
  withClient((c) => bindWallet(c, identity));
}

export function updateUserProfile(properties: AnalyticsProperties): void {
  withClient((c) => c.setPersonProperties(properties));
}

export function setUserProfileOnce(properties: AnalyticsProperties): void {
  withClient((c) => c.setPersonProperties(undefined, properties));
}

export function resetAnalytics(): void {
  wallet = undefined;
  clearDatadogUser();
  withClient(resetClient);
}

export function setAnalyticsEnabled(enabled: boolean): void {
  mmkv.setBoolean(OPT_IN_KEY, enabled);
  try {
    if (!enabled) {
      if (client) {
        stopReplay(client);
        void client.optOut().catch(reportFailure);
      }
      return;
    }
    const c = getClient();
    if (!c) return;
    // A wallet may have been removed or switched while analytics was off.
    if (c.getDistinctId().startsWith(MOBILE_DISTINCT_ID_PREFIX) && !wallet)
      resetClient(c);
    void c.optIn().catch(reportFailure);
    if (wallet) bindWallet(c, wallet);
  } catch {
    reportFailure();
  }
}
