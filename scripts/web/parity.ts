// Drive the built bin/loyal-web over HTTP and compare what it answers with what
// loyal-app's own sources say: the headers() of next.config.ts, and the locale
// redirects of middleware.ts / locale-redirect.ts, each evaluated by Next's own
// matchers and a real NextRequest. Exits 1 on any difference.
//
//   bun scripts/web/parity.ts [--bin bin/loyal-web]
import type { Subprocess } from "bun";
import { type AddressInfo, createServer } from "node:net";

import nextConfig from "../../web/apps/web/next.config";
import { LOCALES, TRANSLATED_PATHS } from "../../web/apps/web/src/features/marketing/i18n/locale";
import { config as middlewareConfig, middleware } from "../../web/apps/web/src/middleware";
import * as pageStaticInfo from "../../web/node_modules/next/dist/build/analysis/get-page-static-info";
import type { MiddlewareMatcher } from "../../web/node_modules/next/dist/build/analysis/get-page-static-info";
import { buildCustomRoute } from "../../web/node_modules/next/dist/server/lib/router-utils/filesystem";
import { getMiddlewareRouteMatcher } from "../../web/node_modules/next/dist/shared/lib/router/utils/middleware-route-matcher";
import { matchHas } from "../../web/node_modules/next/dist/shared/lib/router/utils/prepare-destination";
import { NextRequest } from "../../web/node_modules/next/server";

const APP_HOST = "app.askloyal.com";
const SITE_HOST = "askloyal.com";

const HEADER_SOURCE_PATHS: Record<string, string> = {
  "/.well-known/apple-app-site-association": "/.well-known/apple-app-site-association",
  "/wallet-workspace/facelift/:asset(earn-banner-.*)": "/wallet-workspace/facelift/earn-banner-a.png",
  "/app/cherry": "/app/cherry",
  "/((?!app/cherry/?$).*)": "/earn",
};

const HEADER_CONTROL_PATHS = [
  "/",
  "/does-not-exist",
  "/app/cherry/",
  "/app/cherry/x",
  "/app/cherrypick",
  "/wallet-workspace/facelift/other.png?v=1",
];

const STATIC_SERVER_HEADERS = new Set(["content-type", "cache-control"]);

const GO_ONLY_HEADERS: Record<string, string> = {
  "x-loyal-release": "the build stamp loyal-web adds to every response",
  date: "set by net/http",
  "content-length": "set by net/http",
  "content-type": "the static server infers it from the file",
  "cache-control": "the static server sets per-file caching",
  "last-modified": "http.FileServer",
  etag: "http.FileServer",
  "accept-ranges": "http.FileServer",
};

type Outcome =
  | { kind: "continue"; cookies: string[] }
  | { kind: "error"; status: number }
  | { kind: "rewrite"; to: string }
  | { kind: "redirect"; status: number; location: string; cookies: string[]; cacheControl: string | null; vary: string | null };

type LocaleCase = { host: string; method: string; target: string; acceptLanguage: string; cookie: string; userAgent: string };

type KnownDifference = {
  name: string;
  reason: string;
  go: (expected: Outcome, c: LocaleCase) => Outcome | null;
};

function sortedByKey(location: string): string {
  const url = new URL(location, "http://localhost");
  if (!url.search) return location;
  const key = (pair: string) => decodeURIComponent((pair.split("=")[0] ?? "").replaceAll("+", " "));
  const pairs = url.search.slice(1).split("&").sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0));
  return `${url.pathname}?${pairs.join("&")}`;
}

const KNOWN_DIFFERENCES: KnownDifference[] = [
  {
    name: "app host root",
    reason: "the Vite SPA routes by pathname, so a server rewrite would render the landing route at /",
    go: (expected, c) =>
      expected.kind === "rewrite" && c.host.startsWith(APP_HOST)
        ? { kind: "redirect", status: 302, location: expected.to, cookies: [], cacheControl: null, vary: null }
        : null,
  },
  {
    name: "app host root drops the query",
    reason: "router.go redirects to the constant /app, so ?ref=x on the app host root is lost; the rewrite keeps it",
    go: (expected) =>
      expected.kind === "redirect" && expected.status === 302 && expected.location.startsWith("/app?")
        ? { ...expected, location: "/app" }
        : null,
  },
  {
    name: "matcher exclusions",
    reason: "middleware.ts skips /_next* and /api* prefixes; webapp skips only paths with a dot and /api/*, so ?lang= still redirects on /_next*, /apiary",
    go: (_, c) => {
      const pathname = new URL(c.target, "http://localhost").pathname;
      const skippedByNextOnly = !middlewareMatches(pathname) && !pathname.includes(".") && !pathname.startsWith("/api/");
      return skippedByNextOnly ? nextOutcome(c, true) : null;
    },
  },
  {
    name: "query order",
    reason: "localeRedirect re-encodes the query with url.Values.Encode, which sorts keys; Next keeps the request's order",
    go: (expected) =>
      expected.kind === "redirect" && expected.cookies.length > 0
        ? { ...expected, location: sortedByKey(expected.location) }
        : null,
  },
];

const BROWSER_UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Safari/605.1.15";
const BOT_UAS = [
  "Googlebot/2.1",
  "facebookexternalhit/1.1",
  "Slackbot-LinkExpanding 1.0",
  "TelegramBot (like TwitterBot)",
  "Twitterbot/1.0",
  "WhatsApp/2.23",
  "Applebot/0.1",
  "Embedly/0.2",
  "Mozilla/5.0 Slurp",
  "Baiduspider/2.0",
  "Mozilla/5.0 Link Preview",
  "SomeCrawler/1.0",
  "Mozilla/5.0 SPIDER",
];
const OTHER_UAS = ["", "curl/8.4.0", "Mozilla/5.0 (X11; Linux x86_64) Firefox/120.0", "Robo/1.0"];

const OTHER_PATHS = ["/blog", "/ru", "/earn/", "/EARN", "/earn.txt", "/_next/static/x", "/_nextx", "/api/x", "/apiary", "/favicon.ico", "/app"];

const LANGUAGE_NOISE = [
  "",
  "*",
  "fr",
  "fr,de;q=0.8",
  "RU",
  " ru ; q=0.3 ",
  "ru;q=",
  "ru;q=abc",
  "ru;q=0.5,zh;q=0.5",
  "zh;q=0.5,ru;q=0.5",
  "en;q=0.4,ru;q=0.5",
  "en;q=0.5,ru;q=0.5",
  "zh-TW,zh;q=0.9,en;q=0.8",
  "zh-HK",
];

function acceptLanguages(): string[] {
  const out = [...LANGUAGE_NOISE];
  for (const locale of LOCALES) {
    out.push(locale, `${locale}-XX,${locale};q=0.9,en;q=0.8`, `en-US,${locale};q=0.5`, `${locale};q=0`, `en;q=0.1,${locale};q=0.2`);
  }
  return out;
}

function cookies(): string[] {
  return ["", ...LOCALES.map((locale) => `loyal_locale=${locale}`), "loyal_locale=xx", "other=1"];
}

function queries(): string[] {
  const out = ["", "?a=1", "?a=1&b=2", "?b=1&a=2", "?x=%20y%2F", "?x=a+b", "?lang=", "?lang=xx", "?lang=RU"];
  for (const locale of LOCALES) {
    out.push(`?lang=${locale}`, `?lang=${locale}&a=1`, `?a=1&lang=${locale}&b=2`, `?b=2&lang=${locale}&a=1`);
  }
  out.push(`?lang=${LOCALES[1]}&lang=${LOCALES[LOCALES.length - 1]}`);
  return out;
}

function localeCases(): LocaleCase[] {
  const seen = new Set<string>();
  const out: LocaleCase[] = [];
  const add = (c: Partial<LocaleCase>) => {
    const full: LocaleCase = {
      host: SITE_HOST,
      method: "GET",
      target: "/earn",
      acceptLanguage: "ru",
      cookie: "",
      userAgent: BROWSER_UA,
      ...c,
    };
    const key = JSON.stringify(full);
    if (!seen.has(key)) {
      seen.add(key);
      out.push(full);
    }
  };

  const paths = [...TRANSLATED_PATHS, ...OTHER_PATHS];
  const languages = acceptLanguages();
  for (const method of ["GET", "HEAD", "POST"]) {
    for (const path of paths) {
      for (const acceptLanguage of languages) {
        for (const cookie of cookies()) add({ method, target: path, acceptLanguage, cookie });
      }
    }
  }
  for (const userAgent of [...BOT_UAS, ...OTHER_UAS]) {
    for (const path of TRANSLATED_PATHS) {
      for (const locale of LOCALES) add({ target: path, acceptLanguage: locale, userAgent });
    }
  }
  for (const query of queries()) {
    for (const path of [...TRANSLATED_PATHS, "/blog", "/_next/static/x", "/api/x", "/favicon.ico"]) {
      for (const acceptLanguage of ["", ...LOCALES]) {
        for (const method of ["GET", "HEAD", "POST"]) add({ method, target: `${path}${query}`, acceptLanguage });
      }
    }
  }
  for (const host of [APP_HOST, `${APP_HOST}:443`, "other.example", "ASKLOYAL.com"]) {
    for (const path of ["/", "/app", "/earn", "/blog"]) {
      for (const query of ["", "?lang=ru", "?a=1"]) {
        for (const acceptLanguage of ["", "ru", "zh"]) add({ host, target: `${path}${query}`, acceptLanguage });
      }
    }
  }
  return out;
}

const { getMiddlewareMatchers } = pageStaticInfo as unknown as {
  getMiddlewareMatchers(matcher: unknown, config: unknown): MiddlewareMatcher[];
};
const matchesMiddleware = getMiddlewareRouteMatcher(getMiddlewareMatchers(middlewareConfig.matcher, nextConfig));
const middlewareMatches = (pathname: string) => matchesMiddleware(pathname, { headers: {} } as never, {});

function nextOutcome(c: LocaleCase, ignoreMatcher = false): Outcome {
  const base = new URL(`http://${c.host}`);
  const url = new URL(c.target, base);
  if (!ignoreMatcher && !middlewareMatches(url.pathname)) return { kind: "continue", cookies: [] };
  const headers = new Headers({ host: c.host, "user-agent": c.userAgent });
  if (c.acceptLanguage) headers.set("accept-language", c.acceptLanguage);
  if (c.cookie) headers.set("cookie", c.cookie);
  const request = new NextRequest(url, { method: c.method, headers });
  return describe(middleware(request), base);
}

function describe(response: Response, base: URL): Outcome {
  const rewrite = response.headers.get("x-middleware-rewrite");
  if (rewrite) {
    const to = new URL(rewrite);
    return { kind: "rewrite", to: `${to.pathname}${to.search}` };
  }
  if (response.headers.get("x-middleware-next")) return { kind: "continue", cookies: normalizedCookies(response) };
  return redirectOutcome(response, base);
}

function redirectOutcome(response: Response, base: URL): Outcome {
  const location = new URL(response.headers.get("location") ?? "", base);
  return {
    kind: "redirect",
    status: response.status,
    location: location.origin === base.origin ? `${location.pathname}${location.search}` : location.href,
    cookies: normalizedCookies(response),
    cacheControl: response.headers.get("cache-control"),
    vary: response.headers.get("vary"),
  };
}

function normalizedCookies(response: Response): string[] {
  return response.headers.getSetCookie().map(normalizeCookie).sort();
}

function normalizeCookie(cookie: string): string {
  const [pair, ...attributes] = cookie.split(";").map((part) => part.trim());
  const kept = attributes.map((attribute) => attribute.toLowerCase()).filter((attribute) => !attribute.startsWith("expires="));
  return [pair, ...kept.sort()].join("; ");
}

function goOutcome(response: Response, host: string): Outcome {
  if (response.status >= 500) return { kind: "error", status: response.status };
  if (response.status < 300 || response.status >= 400) return { kind: "continue", cookies: normalizedCookies(response) };
  return redirectOutcome(response, new URL(`http://${host}`));
}

function same(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

type Failure = { what: string; expected: unknown; actual: unknown; next?: unknown };

const fired = new Set<string>();
let redirects = 0;
let headerChecks = 0;

async function compareLocale(origin: string, c: LocaleCase): Promise<Failure | null> {
  const headers: Record<string, string> = { host: c.host, "user-agent": c.userAgent };
  if (c.acceptLanguage) headers["accept-language"] = c.acceptLanguage;
  if (c.cookie) headers.cookie = c.cookie;
  const response = await fetch(`${origin}${c.target}`, { method: c.method, headers, redirect: "manual" });
  await response.arrayBuffer();
  const actual = goOutcome(response, c.host);
  const next = nextOutcome(c);
  if (next.kind === "redirect") redirects++;
  let expected = next;
  for (const difference of KNOWN_DIFFERENCES) {
    const shifted = difference.go(expected, c);
    if (shifted && !same(shifted, expected)) {
      fired.add(difference.name);
      expected = shifted;
    }
  }
  return same(expected, actual) ? null : { what: describeCase(c), expected, actual, next };
}

function describeCase(c: LocaleCase): string {
  return `${c.method} ${c.host}${c.target} accept-language=${JSON.stringify(c.acceptLanguage)} cookie=${JSON.stringify(c.cookie)} ua=${JSON.stringify(c.userAgent)}`;
}

type HeaderRoute = ReturnType<typeof buildCustomRoute> & {
  source: string;
  headers: { key: string; value: string }[];
  has?: { type: string; key: string; value?: string }[];
};

function headerUrls(routes: HeaderRoute[]): string[] {
  const urls = new Set<string>();
  for (const route of routes) {
    const path = HEADER_SOURCE_PATHS[route.source];
    if (path === undefined) {
      throw new Error(`next.config.ts headers() source ${route.source} has no test URL: add it to HEADER_SOURCE_PATHS in scripts/web/parity.ts`);
    }
    urls.add(path);
    for (const has of route.has ?? []) {
      if (has.type !== "query") {
        throw new Error(`next.config.ts headers() source ${route.source} has a "${has.type}" condition: teach scripts/web/parity.ts to drive it`);
      }
      urls.add(`${path}?${has.key}=1`);
    }
  }
  for (const path of HEADER_CONTROL_PATHS) urls.add(path);
  return [...urls];
}

function matchingRoutes(routes: HeaderRoute[], target: string): HeaderRoute[] {
  const url = new URL(target, "http://localhost");
  const query = Object.fromEntries(url.searchParams);
  return routes.filter(
    (route) => route.match(url.pathname) && matchHas({ headers: {} } as never, query, route.has as never, undefined as never),
  );
}

async function compareHeaders(origin: string): Promise<Failure[]> {
  const items = (await nextConfig.headers?.()) ?? [];
  const routes = items.map((item) => buildCustomRoute("header", item as never, "", false)) as unknown as HeaderRoute[];
  const failures: Failure[] = [];
  for (const source of Object.keys(HEADER_SOURCE_PATHS)) {
    if (!routes.some((route) => route.source === source)) {
      failures.push({ what: `HEADER_SOURCE_PATHS lists ${source} but next.config.ts headers() no longer has it`, expected: source, actual: null });
    }
  }
  const known = new Set(routes.flatMap((route) => route.headers.map(({ key }) => key.toLowerCase())));
  const checked = new Map<string, number>();
  for (const target of headerUrls(routes)) {
    const matching = matchingRoutes(routes, target);
    const expected = new Map(matching.flatMap((route) => route.headers.map(({ key, value }): [string, string] => [key.toLowerCase(), value])));
    const response = await fetch(`${origin}${target}`, { redirect: "manual" });
    await response.arrayBuffer();
    for (const route of matching) checked.set(route.source, (checked.get(route.source) ?? 0) + route.headers.length);
    for (const key of known) {
      const want = expected.get(key) ?? null;
      const got = response.headers.get(key);
      headerChecks++;
      if (want === null && STATIC_SERVER_HEADERS.has(key)) continue;
      if (want !== got) failures.push({ what: `GET ${target} header ${key}`, expected: want, actual: got });
    }
    for (const key of response.headers.keys()) {
      if (!known.has(key) && !(key in GO_ONLY_HEADERS)) {
        failures.push({ what: `GET ${target} header ${key} is not sent by next.config.ts`, expected: null, actual: response.headers.get(key) });
      }
    }
  }
  for (const source of Object.keys(HEADER_SOURCE_PATHS)) {
    if (!checked.get(source)) failures.push({ what: `no header was checked for ${source}`, expected: ">0 checks", actual: 0 });
  }
  return failures;
}

async function pool<T>(items: T[], size: number, work: (item: T) => Promise<Failure | null>): Promise<Failure[]> {
  const failures: Failure[] = [];
  let next = 0;
  await Promise.all(
    Array.from({ length: size }, async () => {
      while (next < items.length) {
        const failure = await work(items[next++]);
        if (failure) failures.push(failure);
      }
    }),
  );
  return failures;
}

function argValue(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
}

async function freePort(): Promise<number> {
  const probe = createServer();
  await new Promise<void>((resolve) => probe.listen(0, "127.0.0.1", resolve));
  const { port } = probe.address() as AddressInfo;
  await new Promise((resolve) => probe.close(resolve));
  return port;
}

async function start(bin: string, upstream: string): Promise<{ origin: string; server: Subprocess }> {
  const port = await freePort();
  const server = Bun.spawn([bin], {
    env: {
      ...process.env,
      LOYAL_WEB_ADDRESS: `127.0.0.1:${port}`,
      LOYAL_WEB_APP_HOST: APP_HOST,
      LOYAL_WEB_UPSTREAM: upstream,
    },
    stdout: "ignore",
    stderr: "ignore",
  });
  const origin = `http://127.0.0.1:${port}`;
  for (let attempt = 0; attempt < 100; attempt++) {
    if (server.exitCode !== null) throw new Error(`${bin} exited with ${server.exitCode}`);
    try {
      await fetch(`${origin}/healthz`);
      return { origin, server };
    } catch {
      await Bun.sleep(100);
    }
  }
  server.kill();
  throw new Error(`${bin} did not answer /healthz`);
}

async function main(): Promise<number> {
  const bin = argValue("--bin") ?? new URL("../../bin/loyal-web", import.meta.url).pathname;
  const upstream = Bun.serve({ port: 0, fetch: () => new Response("upstream") });
  const { origin, server } = await start(bin, `http://127.0.0.1:${upstream.port}`);
  try {
    const cases = localeCases();
    const failures = [
      ...(await compareHeaders(origin)),
      ...(await pool(cases, 32, (c) => compareLocale(origin, c))),
    ];
    const expectedRelease = process.env.NEXT_PUBLIC_GIT_COMMIT_HASH;
    if (expectedRelease) {
      const response = await fetch(`${origin}/readyz`);
      const actualRelease = response.headers.get("x-loyal-release");
      if (response.status !== 200 || actualRelease !== expectedRelease) {
        failures.push({ what: "built binary readiness and release stamp", expected: { status: 200, release: expectedRelease }, actual: { status: response.status, release: actualRelease } });
      }
    }
    const floor = LOCALES.length * TRANSLATED_PATHS.length;
    if (cases.length < floor) failures.push({ what: "too few locale cases ran", expected: `>= ${floor}`, actual: cases.length });
    if (redirects < floor) failures.push({ what: "too few locale redirects expected by Next", expected: `>= ${floor}`, actual: redirects });
    if (headerChecks === 0) failures.push({ what: "no header was checked", expected: ">0", actual: 0 });
    for (const difference of KNOWN_DIFFERENCES) {
      if (!fired.has(difference.name)) {
        failures.push({ what: `known difference "${difference.name}" no longer occurs: delete it from KNOWN_DIFFERENCES`, expected: null, actual: null });
      }
    }
    for (const failure of failures.slice(0, 25)) {
      const next = failure.next === undefined ? "" : `\n  next: ${JSON.stringify(failure.next)}`;
      console.error(`FAIL ${failure.what}${next}\n  want: ${JSON.stringify(failure.expected)}\n  go:   ${JSON.stringify(failure.actual)}`);
    }
    if (failures.length > 25) console.error(`... and ${failures.length - 25} more`);
    console.log(`parity: ${headerChecks} header checks, ${cases.length} locale cases (${redirects} redirects), known differences seen: ${[...fired].join(", ")}; ${failures.length} unexplained`);
    return failures.length === 0 ? 0 : 1;
  } finally {
    server.kill();
    await upstream.stop(true);
  }
}

process.exit(await main());
