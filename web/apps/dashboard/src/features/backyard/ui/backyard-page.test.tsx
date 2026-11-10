import { afterAll, expect, mock, test } from "bun:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import type { BackyardApi } from "../types";
import { tokenAmount } from "./format";

let load: any = { state: "loading" };
let clock = Date.parse("2026-11-09T17:30:00Z");
let now = clock;
let start: (() => () => void) | undefined;
let poll: (() => Promise<void>) | undefined;
const original = {
  fetch: globalThis.fetch,
  setInterval: globalThis.setInterval,
  clearInterval: globalThis.clearInterval,
  now: Date.now,
};
Date.now = () => clock;
mock.module("react", () => ({
  ...React,
  default: React,
  useState: (initial: any) =>
    initial?.state === "loading"
      ? [
          load,
          (next: any) => {
            load = typeof next === "function" ? next(load) : next;
          },
        ]
      : initial === Date.now
      ? [
          now,
          (next: number) => {
            now = next;
          },
        ]
      : // eslint-disable-next-line react-hooks/rules-of-hooks -- This test intercepts only the polling state.
        React.useState(initial),
  useEffect: (effect: any) => {
    start ??= effect;
  },
}));
const { BackyardPage } = await import("./backyard-page");
const unavailable = { ok: false as const, value: null };
const at = new Date(clock).toISOString();
const fixture: BackyardApi = {
  loadedAt: at,
  route: {
    ok: true,
    value: {
      aumUsd: 120,
      collateralUsd: 200,
      debtUsd: 80,
      custodyUsd: 0,
      currentApy: {
        apyPct: 12,
        flat: false,
        lane: "OnRe/ONyc/USDC",
        level: 1.75,
        observedAt: at,
      },
      currentOperationId: null,
      lastHoldReason: null,
      leverageTarget: 1.75,
      leverageWatch: null,
      ltvRatio: 0.4,
      navFresh: true,
      navUsd: 120,
      reportAgeS: 60,
      reportObservedAt: at,
      routeStatus: "running",
      selectorAction: null,
      selectorReason: null,
      upApproved: true,
      updatedAgeS: 60,
      voltrIdleUsd: 0,
    },
  },
  risk: {
    ok: true,
    value: {
      lane: "OnRe/ONyc/USDC",
      observedAt: at,
      hardLtvRatio: 0.63,
      maxLtvRatio: 0.66,
      liquidationRatio: 0.75,
    },
  },
  health: {
    ok: true,
    value: {
      navAll: 24,
      navReconciled: 24,
      openStops: [],
      refusedBeforeSending: 0,
    },
  },
  moves: {
    ok: true,
    value: [
      {
        action: "SWAP_COLLATERAL_TO_STABLE_STEP",
        amountRaw: "1000000000",
        token: "ONyc",
        decimals: 9,
        at,
        reason: "release",
        signature: "fixture-signature",
        status: "reconciled",
        strategyKey: "OnRe/ONyc/USDC",
      },
    ],
  },
  borrowing: unavailable,
  breakdown: unavailable,
  voltr: unavailable,
  history: { ok: true, value: [] },
  moveDays: { ok: true, value: [] },
  navSeries: { ok: true, value: [] },
};
let stop: (() => void) | undefined;
afterAll(() => {
  stop?.();
  globalThis.fetch = original.fetch;
  globalThis.setInterval = original.setInterval;
  globalThis.clearInterval = original.clearInterval;
  Date.now = original.now;
  mock.restore();
});

test("mint amounts retain decimal precision and unknown units stay unavailable", () => {
  expect(tokenAmount("1000000000", 9, "ONyc")).toBe("1 ONyc");
  expect(tokenAmount("1000000", 6, "USDC")).toBe("1 USDC");
  expect(tokenAmount("9007199254740993000", 9, "ONyc")).toBe(
    "9,007,199,254.740993 ONyc"
  );
  expect(tokenAmount("1", 9, "ONyc")).toBe("0.000000001 ONyc");
  expect(tokenAmount("1000000000", null, null)).toBe("—");
});

test("market limits render correctly and failed or stale reads cannot show healthy", async () => {
  const respond = (body: BackyardApi) => {
    globalThis.fetch = mock(
      async () => new Response(JSON.stringify(body))
    ) as any;
  };
  const render = () => renderToStaticMarkup(React.createElement(BackyardPage));
  respond(fixture);
  globalThis.setInterval = ((callback: any) => {
    poll = callback;
    return 1;
  }) as any;
  globalThis.clearInterval = (() => {}) as any;
  expect(render()).toContain("Loading the vault");
  stop = start!();
  await Bun.sleep(5);
  expect(render()).toContain("Healthy and earning.");
  expect(render()).toContain("hard limit 63.0%, liquidation at 75.0%");
  expect(render()).toContain("1 ONyc");

  clock += 180_000;
  globalThis.fetch = mock(async () => {
    throw new Error("offline");
  }) as any;
  await poll!();
  expect(render()).toContain("Updates unavailable");
  expect(render()).not.toContain("Healthy and earning.");
  expect(render()).toContain("4 min");

  respond(fixture);
  await poll!();
  expect(render()).toContain("Updates unavailable");
  const recovered = { ...fixture, loadedAt: new Date(clock).toISOString() };
  respond(recovered);
  await poll!();
  expect(render()).toContain("Healthy and earning.");
  respond({ ...recovered, health: unavailable });
  await poll!();
  expect(render()).not.toContain("Healthy and earning.");
  respond({ ...recovered, risk: unavailable });
  await poll!();
  expect(render()).toContain("Current risk limits are unavailable.");
});
