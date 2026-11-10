import { expect, mock, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";

mock.module("next/navigation", () => ({ useRouter: () => ({ push() {}, refresh() {} }) }));
const { default: EarnMaxPage, verdict } = await import("./page");
import type { EarnMaxProps } from "./earn-max-data";

const missing = { ok: false, value: null, error: "Not part of this fixture" };
const route: NonNullable<EarnMaxProps["route"]["value"]> = {
  aumUsd: 12, navUsd: 10, ltvRatio: 0.33, collateralUsd: 15, debtUsd: 5,
  upApproved: false, currentApy: null, currentOperationId: null, goal: null,
  lastHoldReason: null, leaseOwner: null, leverageTarget: null, leverageTargetAt: null,
  leverageWatch: null, navFresh: false, reportAgeS: 56,
  reportObservedAt: "2026-11-09T15:59:04Z", reportSlot: 123,
  routeStatus: "active", selectorAction: null, selectorReason: null, updatedAgeS: 3,
  version: "579af44", workerOwner: "worker:backyard:rwa:sha-579af446b0fdcb783e9befb366660342647ed5b4",
  withdrawalStatus: null,
};

test("pending publication preserves known valuation, while missing or stale data still alarms", () => {
  expect(verdict(route, [], 0.33)).toEqual({ bad: false, text: "Valuation available; NAV report update pending." });
  expect(verdict({ ...route, navFresh: true }, [], 0.33).text).toBe("Reporting normally.");
  expect(verdict({ ...route, navUsd: null }, [], 0.33).text).toBe("Current valuation is unavailable.");
  expect(verdict({ ...route, updatedAgeS: 181 }, [], 0.33).text).toBe("Worker observations are stale.");
  expect(verdict({ ...route, reportAgeS: 4501 }, [], 0.33).text).toBe("NAV reports have stalled.");
  expect(verdict(route, [{ reason: "held" }], 0.33).bad).toBe(true);
  expect(verdict(route, null, 0.33).bad).toBe(true);
});

test("the actual vault page renders worker identity and separates unsigned refusals", () => {
  const props = {
    selectedVault: { id: "loyal", name: "Loyal Earn MAX", address: "test-vault", worker: "loyal-earn-max.service", squads: "test-squads", cashAta: "test-cash", idleAta: "test-idle" },
    loadedAt: "2026-11-09T16:00:00Z", route: { ok: true, value: route },
    health: { ok: true, value: {
      navAll: 100, navReconciled: 97, navFailed: 3, navRefusedBeforeSending: 2,
      refusedBeforeSending: 0, failedSteps: [], navFailureReasons: [
        { reason: "hold:stale", count: 1, beforeSending: true },
        { reason: "hold:chain_refused", count: 1, beforeSending: true },
        { reason: "signature_absent_after_blockhash_expiry", count: 1, beforeSending: false },
      ],
    } },
    latches: { ok: true, value: { open: [], cleared: [] } },
    apy: missing, balances: missing, borrowing: missing, history: missing,
    moves: missing, navSeries: missing, vault: missing,
    backyard: { available: false, error: "Not part of this fixture" },
  } as EarnMaxProps;
  const html = renderToStaticMarkup(<EarnMaxPage {...props} />);
  expect(html).toContain("Loyal Earn MAX");
  expect(html).toContain("Backyard");
  expect(html).toContain("Valuation available; NAV report update pending.");
  expect(html).toContain("579af44");
  expect(html).toContain("Last reporting worker");
  expect(html).toContain("2 refused before sending");
  expect(html).toContain("1 signed attempts failed");
  expect(html).toContain("Freshness check refused · not sent");
  expect(html).not.toContain("No lease holder");
  expect(html).not.toContain("marked stale");
  props.route.value = { ...route, workerOwner: null, version: null };
  expect(renderToStaticMarkup(<EarnMaxPage {...props} />)).toContain("Release not recorded by this worker");
});

test("both vault tabs show the whole portfolio estimate and never substitute the working lane rate", () => {
  for (const id of ["loyal", "backyard"]) {
    const props = {
      selectedVault: { id, name: id === "loyal" ? "Loyal Earn MAX" : "Backyard", address: "vault", worker: "worker", squads: "squads", cashAta: "cash", idleAta: "idle" },
      loadedAt: "2026-11-10T10:00:00Z",
      route: { ok: true, value: { ...route, currentApy: {
        apyPct: 4.93, lane: "Maple/syrupUSDC/USDC", flat: false, level: 1.1,
        observedAt: "2026-11-10T09:59:00Z",
        portfolio: { apyBps: 555, totalValueRaw: 2_016e6, idleUsdcRaw: 916e6, positions: [
          { lane: "AUTO/AUTO/PYUSD", equityRaw: 1_000e6, collateralValueRaw: 1_000e6, debtValueRaw: 0, apyBps: 1069 },
          { lane: "Maple/syrupUSDC/USDC", equityRaw: 100e6, collateralValueRaw: 110e6, debtValueRaw: 10e6, apyBps: 493 },
        ] },
      } } },
      health: missing, latches: missing, apy: missing, balances: missing,
      borrowing: missing, history: missing, moves: missing, navSeries: missing, vault: missing,
      backyard: { available: false, error: "Not part of this fixture" },
    } as EarnMaxProps;
    let html = renderToStaticMarkup(<EarnMaxPage {...props} />);
    expect(html).toContain("Estimated vault APY");
    expect(html).toContain("5.55%");
    expect(html).toContain("AUTO/AUTO/PYUSD");
    expect(html).toContain("10.69%");
    expect(html).toContain("$916.00");
    expect(html).toContain("$10.00 borrowed");
    expect(html).toContain("Realized APY");
    expect(html).not.toContain("Realized APY, 7 days");
    const apy = props.route.value!.currentApy!;
    apy.observedAt = "2026-11-10T09:40:00Z";
    html = renderToStaticMarkup(<EarnMaxPage {...props} />);
    expect(html).toContain("A fresh estimate covering every holding is unavailable");
    expect(html).not.toContain("5.55%");
    expect(html).not.toContain("Vault APY breakdown");
    apy.observedAt = "2026-11-10T09:59:00Z";
    apy.portfolio = null;
    html = renderToStaticMarkup(<EarnMaxPage {...props} />);
    expect(html).not.toContain("4.93%");
    expect(html).toContain("A fresh estimate covering every holding is unavailable");
  }
});
