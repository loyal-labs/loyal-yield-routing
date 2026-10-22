// Admin load-time harness: signs in, then times every admin route from the browser.
//
//   ADMIN_USER=... ADMIN_PASSWORD=... bun scripts/web/admin-perf.ts <host> --set warm|cold|nav|rebalance-all \
//     [--routes /earn/rebalance,/earn] [--samples 5] [--param communityId=<id>] \
//     [--idle <seconds slept before this process, recorded only>] [--json out.json]
//
// warm: one browser, one sign-in, a discarded pass over the routes, then --samples (5) passes;
//   each sample is a hard load in a fresh page (server warm, browser HTTP cache off, see below).
// cold: one fresh browser, one route, one sample. The caller sleeps first (outside any lock) and
//   passes --idle; the sign-in happens right before the measured load and only the login page and
//   POST /auth/login are fetched during it, so the first admin request is the measured one.
// rebalance-all: one load of /earn/rebalance scrolled to the bottom, printing each /api/ call's start, end and duration.
// nav: loads a "from" route, waits for it to settle, clicks the sidebar link to the target and
//   times the click to the new route's DOM; 3 pairs x --samples (3) passes.
//
// Per sample: shellMs = first time the sidebar is visible (since navigation start), dataMs = end of
// the last /api/ response, contentMs = first time at or after dataMs with main rendered and no
// skeleton in it, plus every /api/ call with status, duration and wire bytes (encodedBodySize).
// A sample settles after 1.5 s without an /api/ request in flight. For navs, times are from the
// click, and newDomMs = first moment on the target path with a skeleton or a changed h1/h2 in main.
// The viewport is 1440x8000 so sections that load when scrolled into view (metrics, earn) start on mount.
// Read-only: every non-GET request to the host except the sign-in POST is aborted and counted.
// Page.route disables the browser HTTP cache, so every load fetches its assets.
// Selectors: [data-slot=sidebar-inner] (sidebar), [data-slot=sidebar-inset] (main),
// [data-slot=skeleton] and .animate-pulse (skeletons).
import { readdirSync, statSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";

import { type Page, chromium } from "../../web/node_modules/playwright";

type Api = { label: string; url: string; status: number; startMs: number; endMs: number; durMs: number; ttfbMs: number; wireBytes: number; transferBytes: number };
type Sample = {
  route: string;
  url: string;
  kind: "load" | "nav";
  from?: string;
  pass: number;
  discarded: boolean;
  utcStart: string;
  utcEnd: string;
  shellMs: number | null;
  fcpMs: number | null;
  newDomMs: number | null;
  skeletonMs: number | null;
  pageDataEndMs: number | null;
  dataMs: number | null;
  contentMs: number | null;
  shellBeforeData: boolean | null;
  api: Api[];
  requests: number;
  bytes: number;
  errors: { non2xx: number; failed: number; consoleErrors: number; pageErrors: number; settleTimeout: number; redirectedToLogin: number };
  notes: string[];
  timeline: { t: number; path: string; skel: number; ready: boolean; h1: string }[];
};
type Probe = {
  shell: number | null;
  fcp: number | null;
  log: { t: number; path: string; skel: number; ready: boolean; h1: string }[];
  res: { name: string; initiatorType: string; startTime: number; responseStart: number; responseEnd: number; encodedBodySize: number; transferSize: number; responseStatus: number }[];
  now: number;
};

function flag(name: string): string | undefined {
  const index = process.argv.indexOf(`--${name}`);
  return index > 0 ? process.argv[index + 1] : undefined;
}

const host = (process.argv[2] ?? "").replace(/\/$/, "");
if (!/^https?:\/\//.test(host)) throw new Error("usage: admin-perf.ts <https://admin-host> --set warm|cold|nav");
const set = flag("set");
if (set !== "warm" && set !== "cold" && set !== "nav" && set !== "rebalance-all") throw new Error("--set warm|cold|nav|rebalance-all is required");
const user = process.env.ADMIN_USER;
const password = process.env.ADMIN_PASSWORD;
if (!user || !password) throw new Error("ADMIN_USER and ADMIN_PASSWORD are required in the environment");
const samplesPerRoute = Number(flag("samples") ?? (set === "warm" ? 5 : set === "nav" ? 3 : 1));
const idleDeclaredS = Number(flag("idle") ?? 0);
const jsonOut = flag("json");
const params = new Map<string, string>(
  process.argv.flatMap((value, index) => (process.argv[index - 1] === "--param" && value.includes("=") ? [[value.slice(0, value.indexOf("=")), value.slice(value.indexOf("=") + 1)] as [string, string]] : [])),
);
const routeFilter = flag("routes")
  ?.split(",")
  .map((value) => (value.startsWith("/") ? value : `/${value}`));

const routesDir = join(import.meta.dir, "../../web/apps/admin/src/routes/_admin");
function adminRoutes(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return adminRoutes(path);
    if (name !== "index.tsx") return [];
    return [`/${relative(routesDir, dir)}`.replace(/^\/\.$/, "/")];
  });
}
const allRoutes = adminRoutes(routesDir).sort();
const routes = set === "rebalance-all" ? ["/earn/rebalance"] : routeFilter ? allRoutes.filter((route) => routeFilter.includes(route)) : allRoutes;
if (routes.length === 0) throw new Error(`no route matches --routes; known: ${allRoutes.join(", ")}`);
if (set === "cold" && routes.length !== 1) throw new Error("--set cold takes exactly one route per process");

const navPairs: [string, string][] = [
  ["/earn", "/earn/rebalance"],
  ["/earn/rebalance", "/vault-integrations/earn-max"],
  ["/metrics", "/earn"],
];

const gitSha = (() => {
  const run = (args: string[]) => Bun.spawnSync(["git", "-C", import.meta.dir, ...args]).stdout.toString().trim();
  return `${run(["rev-parse", "HEAD"]) || "unknown"}${run(["status", "--porcelain", "--", "."]) ? "+dirty" : ""}`;
})();

const probeSource = () => {
  const state: Probe = { shell: null, fcp: null, log: [], res: [], now: 0 };
  (window as unknown as { __perf: Probe }).__perf = state;
  performance.setResourceTimingBufferSize(2000);
  let lastKey = "";
  let last = 0;
  let queued = false;
  const check = () => {
    queued = false;
    const now = performance.now();
    last = now;
    if (state.shell === null) {
      const sidebar = document.querySelector('[data-slot="sidebar-inner"]');
      const rect = sidebar?.getBoundingClientRect();
      if (rect && rect.width > 0 && rect.height > 0) state.shell = now;
    }
    const main = document.querySelector('[data-slot="sidebar-inset"]');
    const skel = main ? [...main.querySelectorAll('[data-slot="skeleton"], .animate-pulse')].filter((el) => el.checkVisibility()).length : 0;
    const ready = !!main && skel === 0 && (main.textContent ?? "").trim().length > 0;
    const h1 = (main?.querySelector("h1, h2")?.textContent ?? "").trim().slice(0, 40);
    const key = `${location.pathname}|${skel > 0}|${ready}|${!!main}|${h1}`;
    if (key !== lastKey) {
      lastKey = key;
      state.log.push({ t: now, path: location.pathname, skel, ready, h1 });
    }
  };
  const schedule = () => {
    if (queued) return;
    queued = true;
    const wait = Math.max(0, 40 - (performance.now() - last));
    setTimeout(() => {
      let ran = false;
      const run = () => {
        if (ran) return;
        ran = true;
        check();
      };
      requestAnimationFrame(run);
      setTimeout(run, 120);
    }, wait);
  };
  new MutationObserver(schedule).observe(document, { childList: true, subtree: true, characterData: true });
  const tick = () => {
    schedule();
    setTimeout(tick, 100);
  };
  tick();
};

function median(values: number[]): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid]! : (sorted[mid - 1]! + sorted[mid]!) / 2;
}
const nums = (values: (number | null | undefined)[]) => values.filter((value): value is number => typeof value === "number");
const round = (value: number | null) => (value === null ? null : Math.round(value));

function apiLabel(url: string): string {
  const parsed = new URL(url);
  const section = parsed.searchParams.get("section");
  return `${parsed.pathname}${section ? `?section=${section}` : ""}`;
}

const aborted: { method: string; url: string }[] = [];
let loginPosts = 0;

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1440, height: 8000 } });
let signingIn = true;
await context.route("**/*", (route) => {
  const request = route.request();
  const url = request.url();
  if (!url.startsWith(host)) return route.continue();
  const path = new URL(url).pathname;
  if (signingIn) {
    if (request.method() === "POST" && path === "/auth/login") {
      loginPosts += 1;
      return route.continue();
    }
    if (request.method() === "GET" && request.isNavigationRequest() && path !== "/login") return route.abort();
    if (request.method() === "GET") return route.continue();
  }
  if (request.method() === "GET") return route.continue();
  aborted.push({ method: request.method(), url });
  return route.abort();
});

async function signIn(): Promise<void> {
  const page = await context.newPage();
  await page.goto(`${host}/login`, { waitUntil: "domcontentloaded" });
  await page.fill('input[name="login"]', user ?? "");
  await page.fill('input[name="password"]', password ?? "");
  await Promise.all([page.waitForResponse((response) => new URL(response.url()).pathname === "/auth/login"), page.click('button[type="submit"]')]);
  await page.close();
  signingIn = false;
  if (!(await context.cookies(host)).some((cookie) => cookie.name === "loyal_admin_session")) throw new Error("admin sign-in failed");
}

async function discoverParam(routeTemplate: string, key: string): Promise<string | undefined> {
  const listPath = routeTemplate.slice(0, routeTemplate.indexOf("/$"));
  const page = await context.newPage();
  try {
    await page.goto(`${host}${listPath}`, { waitUntil: "load", timeout: 60_000 });
    const link = page.locator(`main a[href^="${listPath}/"]`).first();
    await link.waitFor({ timeout: 30_000 });
    const href = (await link.getAttribute("href")) ?? "";
    return href.slice(listPath.length + 1).split(/[/?#]/)[0] || undefined;
  } catch {
    return undefined;
  } finally {
    await page.close();
  }
}

function resolve(routeTemplate: string): string | undefined {
  let missing = false;
  const path = routeTemplate.replace(/\$(\w+)/g, (_, key: string) => {
    const value = params.get(key);
    if (!value) missing = true;
    return value ?? "";
  });
  return missing ? undefined : path;
}

type Tracker = { inFlight: number; seen: number; lastActivity: number; failed: number; consoleErrors: number; pageErrors: number; requests: number; consoleTexts: string[] };
function track(page: Page): Tracker {
  const tracker: Tracker = { inFlight: 0, seen: 0, lastActivity: Date.now(), failed: 0, consoleErrors: 0, pageErrors: 0, requests: 0, consoleTexts: [] };
  const isApi = (url: string) => url.startsWith(host) && new URL(url).pathname.startsWith("/api/");
  page.on("request", (request) => {
    tracker.requests += 1;
    if (!isApi(request.url())) return;
    tracker.inFlight += 1;
    tracker.seen += 1;
    tracker.lastActivity = Date.now();
  });
  const done = (failed: boolean) => (request: { url(): string }) => {
    if (!isApi(request.url())) return;
    tracker.inFlight -= 1;
    tracker.lastActivity = Date.now();
    if (failed) tracker.failed += 1;
  };
  page.on("requestfinished", done(false));
  page.on("requestfailed", done(true));
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    tracker.consoleErrors += 1;
    if (tracker.consoleTexts.length < 3) tracker.consoleTexts.push(`console ${message.text().slice(0, 160)}`);
  });
  page.on("pageerror", (error) => {
    tracker.pageErrors += 1;
    if (tracker.consoleTexts.length < 3) tracker.consoleTexts.push(`pageerror ${error.message.slice(0, 160)}`);
  });
  return tracker;
}

async function settle(tracker: Tracker, minSeen: number): Promise<boolean> {
  const start = Date.now();
  while (Date.now() - start < 90_000) {
    if (tracker.seen >= minSeen && tracker.inFlight <= 0 && Date.now() - tracker.lastActivity >= 1500) return true;
    await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  }
  return false;
}

async function rendered(page: Page): Promise<void> {
  const start = Date.now();
  while (Date.now() - start < 20_000) {
    const ready = await page.evaluate(() => (window as unknown as { __perf: Probe }).__perf.log.at(-1)?.ready === true);
    if (ready) return;
    await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  }
}

async function collect(page: Page, tracker: Tracker, base: { route: string; url: string; kind: "load" | "nav"; from?: string; pass: number; discarded: boolean; utcStart: string }, settled: boolean, mark: number, h1Before = ""): Promise<Sample> {
  const probe = await page.evaluate((): Probe => {
    const state = (window as unknown as { __perf: Probe }).__perf;
    const paint = performance.getEntriesByName("first-contentful-paint")[0];
    const res = performance.getEntriesByType("resource").map((entry) => {
      const timing = entry as PerformanceResourceTiming & { responseStatus?: number };
      return {
        name: timing.name,
        initiatorType: timing.initiatorType,
        startTime: timing.startTime,
        responseStart: timing.responseStart,
        responseEnd: timing.responseEnd,
        encodedBodySize: timing.encodedBodySize,
        transferSize: timing.transferSize,
        responseStatus: timing.responseStatus ?? 0,
      };
    });
    return { shell: state.shell, fcp: paint ? paint.startTime : null, log: state.log, res, now: performance.now() };
  });
  const notes: string[] = [];
  const api: Api[] = probe.res
    .filter((entry) => entry.name.startsWith(`${host}/api/`) && entry.startTime >= mark && (entry.initiatorType === "fetch" || entry.initiatorType === "xmlhttprequest"))
    .map((entry) => ({
      label: apiLabel(entry.name),
      url: entry.name.slice(host.length),
      status: entry.responseStatus,
      startMs: entry.startTime - mark,
      endMs: entry.responseEnd - mark,
      durMs: entry.responseEnd - entry.startTime,
      ttfbMs: entry.responseStart - entry.startTime,
      wireBytes: entry.encodedBodySize,
      transferBytes: entry.transferSize,
    }));
  const dataMs = api.length ? Math.max(...api.map((call) => call.endMs)) : null;
  const pageData = api.find((call) => call.label.startsWith("/api/admin/page/"));
  const pageDataEndMs = pageData?.endMs ?? null;
  const targetPath = new URL(base.url).pathname;
  const log = probe.log.filter((entry) => entry.t >= mark).map((entry) => ({ ...entry, t: entry.t - mark }));
  const firstNew = base.kind === "nav" ? log.findIndex((entry) => entry.path === targetPath && (entry.skel > 0 || entry.h1 !== h1Before)) : 0;
  const onTarget = firstNew < 0 ? [] : log.slice(firstNew).filter((entry) => entry.path === targetPath || base.kind === "load");
  let contentAt: number | null = null;
  if (dataMs !== null) {
    const stateAtData = onTarget.filter((entry) => entry.t <= dataMs).at(-1);
    contentAt = stateAtData?.ready ? dataMs : (onTarget.find((entry) => entry.ready && entry.t >= dataMs)?.t ?? null);
  }
  const shellMs = base.kind === "load" ? probe.shell : null;
  const newDomMs = base.kind === "nav" ? (onTarget[0]?.t ?? null) : null;
  const skeletonMs = onTarget.find((entry) => entry.skel > 0)?.t ?? null;
  const firstPaint = shellMs ?? newDomMs;
  if (page.url().includes("/login")) notes.push("redirected to login");
  if (!settled) notes.push("did not settle in 90 s");
  notes.push(...tracker.consoleTexts);
  const non2xx = api.filter((call) => call.status === 0 || call.status >= 400).length;
  const overall = probe.res.filter((entry) => entry.startTime >= mark);
  return {
    ...base,
    utcEnd: new Date().toISOString(),
    shellMs: round(shellMs),
    fcpMs: base.kind === "load" ? round(probe.fcp) : null,
    newDomMs: round(newDomMs),
    skeletonMs: round(skeletonMs),
    pageDataEndMs: round(pageDataEndMs),
    dataMs: round(dataMs),
    contentMs: round(contentAt),
    shellBeforeData: firstPaint !== null && (pageDataEndMs ?? dataMs) !== null ? firstPaint < (pageDataEndMs ?? dataMs ?? 0) : null,
    api: api.map((call) => ({ ...call, startMs: Math.round(call.startMs), endMs: Math.round(call.endMs), durMs: Math.round(call.durMs), ttfbMs: Math.round(call.ttfbMs) })),
    requests: overall.length + 1,
    bytes: overall.reduce((sum, entry) => sum + entry.transferSize, 0),
    errors: {
      non2xx,
      failed: tracker.failed,
      consoleErrors: tracker.consoleErrors,
      pageErrors: tracker.pageErrors,
      settleTimeout: settled ? 0 : 1,
      redirectedToLogin: page.url().includes("/login") ? 1 : 0,
    },
    notes,
    timeline: log.map((entry) => ({ ...entry, t: Math.round(entry.t) })),
  };
}

async function loadSample(route: string, path: string, pass: number, discarded: boolean, scrollToBottom = false): Promise<Sample> {
  const page = await context.newPage();
  await page.bringToFront();
  await page.addInitScript(probeSource);
  if (scrollToBottom) await page.addInitScript(() => setInterval(() => document.documentElement && window.scrollTo(0, document.documentElement.scrollHeight), 100));
  const tracker = track(page);
  const utcStart = new Date().toISOString();
  await page.goto(`${host}${path}`, { waitUntil: "commit", timeout: 60_000 }).catch(() => undefined);
  const settled = await settle(tracker, 1);
  await rendered(page);
  const sample = await collect(page, tracker, { route, url: `${host}${path}`, kind: "load", pass, discarded, utcStart }, settled, 0);
  await page.close();
  return sample;
}

async function navSample(from: string, to: string, pass: number): Promise<Sample> {
  const page = await context.newPage();
  await page.bringToFront();
  await page.addInitScript(probeSource);
  const tracker = track(page);
  await page.goto(`${host}${from}`, { waitUntil: "commit", timeout: 60_000 }).catch(() => undefined);
  const warm = await settle(tracker, 1);
  const utcStart = new Date().toISOString();
  const before = tracker.seen;
  const { mark, h1Before } = await page.evaluate(() => ({ mark: performance.now(), h1Before: (document.querySelector('[data-slot="sidebar-inset"]')?.querySelector("h1, h2")?.textContent ?? "").trim().slice(0, 40) }));
  await page.locator(`[data-slot="sidebar-inner"] a[href="${to}"]`).first().click({ timeout: 30_000 });
  const settled = await settle(tracker, before + 1);
  await rendered(page);
  const sample = await collect(page, tracker, { route: to, url: `${host}${to}`, kind: "nav", from, pass, discarded: false, utcStart }, settled, mark, h1Before);
  if (!warm) sample.notes.push("from page did not settle");
  await page.close();
  return sample;
}

const startedUtc = new Date().toISOString();
const samples: Sample[] = [];
const skipped: string[] = [];
await signIn();

const targets: { template: string; path: string }[] = [];
for (const template of routes) {
  let path = resolve(template);
  if (!path && set !== "cold") {
    const key = /\$(\w+)/.exec(template)?.[1] ?? "";
    const found = await discoverParam(template, key);
    if (found) params.set(key, found);
    path = resolve(template);
  }
  if (path) targets.push({ template, path });
  else skipped.push(`${template}: needs --param`);
}

if (set === "warm") {
  for (let pass = 0; pass <= samplesPerRoute; pass += 1) {
    for (const target of targets) {
      samples.push(await loadSample(target.template, target.path, pass, pass === 0));
      await new Promise((resolveWait) => setTimeout(resolveWait, 300));
    }
  }
} else if (set === "rebalance-all") {
  samples.push(await loadSample(routes[0]!, routes[0]!, 1, false, true));
} else if (set === "cold") {
  for (const target of targets) samples.push(await loadSample(target.template, target.path, 1, false));
} else {
  for (let pass = 1; pass <= samplesPerRoute; pass += 1) {
    for (const [from, to] of navPairs) {
      samples.push(await navSample(from, to, pass));
      await new Promise((resolveWait) => setTimeout(resolveWait, 300));
    }
  }
}
await browser.close();
const endedUtc = new Date().toISOString();

type Row = { route: string; from?: string; n: number; shellMs: number | null; shellRange: [number, number] | null; newDomMs: number | null; dataMs: number | null; dataRange: [number, number] | null; contentMs: number | null; pageDataMs: number | null; slowest: { label: string; ms: number } | null; apiWire: number; errors: number; shellBeforeData: string; api: { label: string; medianMs: number; minMs: number; maxMs: number; wireBytes: number; status: string }[] };
const kept = samples.filter((sample) => !sample.discarded);
const groups = new Map<string, Sample[]>();
for (const sample of kept) {
  const key = `${sample.from ?? ""}>${sample.route}`;
  groups.set(key, [...(groups.get(key) ?? []), sample]);
}
const rows: Row[] = [...groups.values()].map((group) => {
  const labels = [...new Set(group.flatMap((sample) => sample.api.map((call) => call.label)))];
  const api = labels
    .map((label) => {
      const calls = group.flatMap((sample) => sample.api.filter((call) => call.label === label));
      const durs = calls.map((call) => call.durMs);
      return {
        label,
        medianMs: Math.round(median(durs) ?? 0),
        minMs: Math.min(...durs),
        maxMs: Math.max(...durs),
        wireBytes: Math.round(median(calls.map((call) => call.wireBytes)) ?? 0),
        status: [...new Set(calls.map((call) => call.status))].join("/"),
      };
    })
    .sort((a, b) => b.medianMs - a.medianMs);
  const range = (values: (number | null)[]): [number, number] | null => {
    const present = nums(values);
    return present.length ? [Math.min(...present), Math.max(...present)] : null;
  };
  const before = group.filter((sample) => sample.shellBeforeData === true).length;
  return {
    route: group[0]!.route,
    from: group[0]!.from,
    n: group.length,
    shellMs: round(median(nums(group.map((sample) => sample.shellMs)))),
    shellRange: range(group.map((sample) => sample.shellMs)),
    newDomMs: round(median(nums(group.map((sample) => sample.newDomMs)))),
    dataMs: round(median(nums(group.map((sample) => sample.dataMs)))),
    dataRange: range(group.map((sample) => sample.dataMs)),
    contentMs: round(median(nums(group.map((sample) => sample.contentMs)))),
    pageDataMs: round(median(nums(group.map((sample) => sample.pageDataEndMs)))),
    slowest: api[0] ? { label: api[0].label, ms: api[0].medianMs } : null,
    apiWire: api.reduce((sum, call) => sum + call.wireBytes, 0),
    errors: group.reduce((sum, sample) => sum + sample.errors.non2xx + sample.errors.failed + sample.errors.consoleErrors + sample.errors.pageErrors + sample.errors.settleTimeout + sample.errors.redirectedToLogin, 0),
    shellBeforeData: `${before}/${group.length}`,
    api,
  };
});

const totals = {
  samples: kept.length,
  requests: kept.reduce((sum, sample) => sum + sample.requests, 0),
  bytes: kept.reduce((sum, sample) => sum + sample.bytes, 0),
  non2xx: kept.reduce((sum, sample) => sum + sample.errors.non2xx, 0),
  failed: kept.reduce((sum, sample) => sum + sample.errors.failed, 0),
  consoleErrors: kept.reduce((sum, sample) => sum + sample.errors.consoleErrors, 0),
  pageErrors: kept.reduce((sum, sample) => sum + sample.errors.pageErrors, 0),
  settleTimeouts: kept.reduce((sum, sample) => sum + sample.errors.settleTimeout, 0),
  abortedNonGet: aborted.length,
  loginPosts,
};

const pad = (value: string | number | null, width: number) => String(value ?? "-").padEnd(width);
const kb = (bytes: number) => `${(bytes / 1000).toFixed(1)}kB`;
console.log(`admin-perf ${set} ${host} sha=${gitSha} ${startedUtc} .. ${endedUtc} idle=${idleDeclaredS}s`);
console.log(`${pad("route", 40)}${pad("n", 3)}${pad(set === "nav" ? "newDom" : "shell", 8)}${pad("data", 8)}${pad("content", 8)}${pad("paint<data", 11)}${pad("slowest api", 48)}${pad("api wire", 10)}err`);
for (const row of rows) {
  const name = row.from ? `${row.from} -> ${row.route}` : row.route;
  const slow = row.slowest ? `${row.slowest.label} ${row.slowest.ms}ms` : "-";
  console.log(`${pad(name, 40)}${pad(row.n, 3)}${pad(set === "nav" ? row.newDomMs : row.shellMs, 8)}${pad(row.dataMs, 8)}${pad(row.contentMs, 8)}${pad(row.shellBeforeData, 11)}${pad(slow.slice(0, 46), 48)}${pad(kb(row.apiWire), 10)}${row.errors}`);
  for (const call of row.api) console.log(`    ${pad(call.label.slice(0, 60), 62)}${pad(`${call.medianMs}ms`, 8)}${pad(`${call.minMs}-${call.maxMs}`, 12)}${pad(kb(call.wireBytes), 10)}${call.status}`);
}
if (set === "rebalance-all") {
  const calls = [...samples[0]!.api].sort((a, b) => a.startMs - b.startMs);
  console.log(`${pad("start", 8)}${pad("end", 8)}${pad("dur", 8)}${pad("status", 8)}${pad("wire", 10)}request`);
  for (const call of calls) console.log(`${pad(call.startMs, 8)}${pad(call.endMs, 8)}${pad(call.durMs, 8)}${pad(call.status, 8)}${pad(kb(call.wireBytes), 10)}${call.url.slice(0, 90)}`);
  const peak = Math.max(...calls.map((call) => calls.filter((other) => other.startMs <= call.startMs && other.endMs > call.startMs).length));
  console.log(`requests ${calls.length} peak in flight ${peak}`);
}
console.log(`totals ${JSON.stringify(totals)}`);
if (skipped.length) console.log(`skipped ${skipped.join("; ")}`);
if (aborted.length) console.log(`aborted non-GET ${JSON.stringify(aborted)}`);

if (jsonOut) {
  writeFileSync(jsonOut, `${JSON.stringify({ harness: "admin-perf", sha: gitSha, host, set, startedUtc, endedUtc, idleDeclaredS, samplesPerRoute, params: Object.fromEntries(params), skipped, totals, aborted, summary: rows, samples }, null, 1)}\n`);
}
