import { randomUUID } from "node:crypto";
import { gunzipSync } from "node:zlib";

import { MOBILE_DISTINCT_ID_PREFIX } from "@loyal-labs/shared";
import PostHog from "posthog-react-native";

import { env } from "@/config/env";
import { clearDatadogUser, identifyDatadogUser } from "@/lib/datadog/datadog";
import {
  identifyWallet,
  inReplayCohort,
  initAnalytics,
  resetAnalytics,
  setAnalyticsEnabled,
  track,
} from "../analytics";
import { initAttribution } from "../attribution";
import { APP_EVENTS } from "../app-events";
import { BRIDGE_EVENTS } from "../bridge-events";

jest.mock("@/config/env", () => ({
  env: { posthogKey: "", posthogHost: "https://ingest.invalid" },
}));
jest.mock("@/lib/datadog/datadog", () => ({
  clearDatadogUser: jest.fn(),
  identifyDatadogUser: jest.fn(),
}));
jest.mock("@/features/referrals/pending-code", () => ({
  storePending: jest.fn(),
}));
jest.mock("expo-linking", () => ({
  getInitialURL: async () => "loyal://open?utm_source=campaign",
  addEventListener: () => ({ remove() {} }),
}));
jest.mock("react-native-play-install-referrer", () => ({
  PlayInstallReferrer: { getInstallReferrerInfo() {} },
}));
jest.mock("expo", () => ({ isRunningInExpoGo: () => false }));
jest.mock("react-native", () => ({
  Dimensions: { get: () => ({ width: 390, height: 844 }) },
  Linking: { getInitialURL: async () => null },
  AppState: {
    currentState: "active",
    addEventListener: () => ({ remove() {} }),
  },
  Platform: {
    OS: process.env.TEST_MOBILE_PLATFORM === "android" ? "android" : "ios",
    Version: 35,
    constants: {},
    select: (options: { default?: unknown }) => options.default,
  },
}));
jest.mock("@posthog/react-native-plugin", () => {
  let recording = false;
  return {
    setup: jest.fn(async () => {}),
    identify: jest.fn(async () => {}),
    reset: jest.fn(async () => {}),
    setOptOut: jest.fn(async () => {}),
    startSession: jest.fn(),
    endSession: jest.fn(),
    startRecording: jest.fn(async () => {
      recording = true;
    }),
    stopRecording: jest.fn(async () => {
      recording = false;
    }),
    isEnabled: jest.fn(async () => recording),
  };
});

type Event = {
  event: string;
  distinct_id: string;
  properties: Record<string, unknown>;
};
const batch: Event[] = [];
const native = jest.requireMock("@posthog/react-native-plugin");

function address(sampled: boolean) {
  for (let i = 0; ; i++) {
    const value = `wallet-${i}`;
    if (inReplayCohort(MOBILE_DISTINCT_ID_PREFIX + value) === sampled)
      return value;
  }
}

test("the same identity stays in the same one-fifth replay cohort", () => {
  const ids = Array.from(
    { length: 10000 },
    (_, i) => MOBILE_DISTINCT_ID_PREFIX + i
  );
  const cohort = ids.filter(inReplayCohort);
  expect(ids.reverse().filter(inReplayCohort).reverse()).toEqual(cohort);
  expect(cohort.length).toBeGreaterThan(1900);
  expect(cohort.length).toBeLessThan(2100);
});

test("the native SDK sends existing and bridge events and enforces account and consent boundaries", async () => {
  const capture = jest.spyOn(PostHog.prototype, "capture");
  const identify = jest.spyOn(PostHog.prototype, "identify");
  const fetch = jest
    .spyOn(globalThis, "fetch")
    .mockImplementation(async (url: RequestInfo | URL, options?: RequestInit) => {
      if (new URL(String(url)).pathname === "/batch/") {
        const body = options?.body;
        const json =
          typeof body === "string"
            ? body
            : gunzipSync(
                Buffer.from(await new Response(body).arrayBuffer())
              ).toString();
        batch.push(...JSON.parse(json).batch);
      }
      return new Response(
        JSON.stringify({ flags: {}, sessionRecording: true })
      );
    });

  track(APP_EVENTS.opened);
  identifyWallet(address(false), "privy");
  expect(identifyDatadogUser).toHaveBeenCalled();
  expect(fetch).not.toHaveBeenCalled();
  Object.assign(env, { posthogKey: randomUUID() });
  setAnalyticsEnabled(false);
  initAnalytics();
  track(APP_EVENTS.opened);
  expect(capture).not.toHaveBeenCalled();
  expect(fetch).not.toHaveBeenCalled();

  setAnalyticsEnabled(true);
  const c = identify.mock.contexts[0] as PostHog;
  expect(c).toBeDefined();
  try {
    await c.ready();
    track(APP_EVENTS.opened);
    const unsubscribe = initAttribution();
    await Promise.resolve();
    unsubscribe();
    track(BRIDGE_EVENTS.receiveAddressCopied, { network_family: "evm" });
    track(BRIDGE_EVENTS.outResult, { outcome: "failed" });
    await c.flush();
    expect(native.startRecording).not.toHaveBeenCalled();
    expect(native.setup).not.toHaveBeenCalled();
    const id = MOBILE_DISTINCT_ID_PREFIX + address(false);
    const named = batch.filter((e) => e.event.startsWith("[mobile] "));
    expect(named.map((e) => e.event)).toEqual([
      "[mobile] App Opened",
      "[mobile] Bridge Receive Address Copied",
      "[mobile] Bridge Out Result",
    ]);
    for (const event of named) {
      expect(event).toMatchObject({
        distinct_id: id,
        properties: { workspace: "mobile" },
      });
    }
    expect(named[1].properties).toMatchObject({
      utm_source: "campaign",
      network_family: "evm",
    });
    expect(named[2].properties).toMatchObject({ outcome: "failed" });
    expect(batch.find((e) => e.event === "$identify")).toMatchObject({
      distinct_id: id,
      properties: {
        $set: {
          wallet_address: address(false),
          wallet_source: "privy",
          identity_provider: "mobile",
          last_workspace: "mobile",
        },
      },
    });
    expect(batch.map((e) => e.properties.$set_once)).toContainEqual({
      initial_attribution_source: "deep_link",
      initial_utm_source: "campaign",
    });
    expect(batch.map((e) => e.properties.$set)).toContainEqual({
      attribution_source: "deep_link",
      utm_source: "campaign",
    });

    const start = jest.spyOn(PostHog.prototype, "startSessionRecording");
    identifyWallet(address(true), "mwa");
    await start.mock.results[0].value;
    expect(native.setup.mock.lastCall[2].sessionReplay).toMatchObject({
      enabled: true,
      sdkReplayConfig: {
        sampleRate: 1,
        maskAllImages: true,
        maskAllTextInputs: true,
        maskAllSandboxedViews: true,
        captureTouches: false,
        captureLog: false,
        captureNetworkTelemetry: false,
      },
    });
    expect(native.startRecording).toHaveBeenCalledTimes(1);
    const stop = jest.spyOn(PostHog.prototype, "stopSessionRecording");
    identifyWallet(address(false), "privy");
    await stop.mock.results[0].value;
    expect(await native.isEnabled()).toBe(false);
    expect(c.getDistinctId()).toBe(id);
    identifyWallet(address(true), "mwa");
    await start.mock.results[1].value;
    expect(await native.isEnabled()).toBe(true);
    const optOut = jest.spyOn(PostHog.prototype, "optOut");
    const nativeOptedOut = new Promise<void>((resolve) => {
      native.setOptOut.mockImplementation(async (off: boolean) => {
        if (off) resolve();
      });
    });
    setAnalyticsEnabled(false);
    await Promise.all([optOut.mock.results[0].value, nativeOptedOut]);
    expect(c.optedOut).toBe(true);
    expect(await native.isEnabled()).toBe(false);
    const before = capture.mock.calls.length;
    track(BRIDGE_EVENTS.outSubmit);
    expect(capture).toHaveBeenCalledTimes(before);
    resetAnalytics();
    expect(clearDatadogUser).toHaveBeenCalled();
    setAnalyticsEnabled(true);
    expect(c.getDistinctId()).not.toMatch(/^mob:/);
    track(APP_EVENTS.opened);
    await c.flush();
    expect(batch.at(-1)?.properties.utm_source).toBeUndefined();
    expect(batch.at(-1)?.properties.workspace).toBe("mobile");
  } finally {
    await c.shutdown();
    jest.restoreAllMocks();
  }
});
