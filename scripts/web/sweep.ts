// Screenshot every route of the route table on one origin, and report the ones
// that error. Run with the web workspace's Playwright:
//
//   bun scripts/web/sweep.ts --base https://askloyal.com --out ~/loyal/artifacts/web-go/baseline
//
// --app-base sets the origin for app routes (default: --base). The blog slug
// route uses --slug (default: the first post folder in web/apps/web/public/blog).
import { mkdirSync, readdirSync } from "node:fs";
import { join } from "node:path";

import { chromium } from "../../web/node_modules/playwright";
import { routes } from "../../web/apps/loyal/src/routes.table";

function arg(name: string, fallback?: string): string {
  const index = process.argv.indexOf(`--${name}`);
  const value = index > 0 ? process.argv[index + 1] : fallback;
  if (!value) throw new Error(`--${name} is required`);
  return value;
}

const base = arg("base");
const appBase = arg("app-base", base);
const out = arg("out");
const slug = arg(
  "slug",
  readdirSync(join(import.meta.dir, "../../web/apps/web/public/blog")).find((name) => name !== "authors"),
);

mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const failures: string[] = [];

for (const row of routes.filter((r) => r.kind === "page")) {
  const path = row.path.replace("$slug", slug);
  const url = `${row.host === "app" ? appBase : base}${path}`;
  const errors: string[] = [];
  page.removeAllListeners("pageerror");
  page.on("pageerror", (error) => errors.push(error.message));
  const response = await page.goto(url, { waitUntil: "load", timeout: 60_000 }).catch((error: Error) => {
    errors.push(error.message);
    return null;
  });
  // The app pages poll and stream for as long as they are open, so network
  // idle is a best-effort settle before the screenshot, never a gate.
  await page.waitForLoadState("networkidle", { timeout: 10_000 }).catch(() => undefined);
  const name = path === "/" ? "index" : path.slice(1).replaceAll("/", "__");
  await page.screenshot({ path: join(out, `${name}.png`), fullPage: true }).catch(() => undefined);
  const status = response?.status() ?? 0;
  const ok = status > 0 && status < 400 && errors.length === 0;
  console.log(`${ok ? "ok  " : "FAIL"} ${status} ${url}${errors.length ? ` ${errors[0]}` : ""}`);
  if (!ok) failures.push(url);
}

await browser.close();
console.log(`${routes.filter((r) => r.kind === "page").length - failures.length}/${routes.filter((r) => r.kind === "page").length} rendered`);
process.exit(failures.length ? 1 : 0);
