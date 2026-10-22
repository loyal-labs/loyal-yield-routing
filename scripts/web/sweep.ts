// Screenshot every route of the route table on one origin, and report the ones
// that error. Run with the web workspace's Playwright:
//
//   bun scripts/web/sweep.ts --base https://askloyal.com --out ~/loyal/artifacts/web-go/baseline
//
// --app-base sets the origin for app routes (default: --base). The blog slug
// route uses --slug (default: the first post folder in web/apps/web/public/blog).
//
//   bun scripts/web/sweep.ts --app admin --base https://admin.example --out <dir> \
//     --param communityId=<id>
//
// signs in first with ADMIN_USER and ADMIN_PASSWORD from the environment, then
// screenshots every page under web/apps/admin/src/app/(admin).
import { mkdirSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

import { chromium } from "../../web/node_modules/playwright";
import { routes } from "../../web/apps/loyal/src/routes.table";

function arg(name: string, fallback?: string): string {
  const index = process.argv.indexOf(`--${name}`);
  const value = index > 0 ? process.argv[index + 1] : fallback;
  if (!value) throw new Error(`--${name} is required`);
  return value;
}

const app = arg("app", "loyal");
const base = arg("base");
const out = arg("out");

function adminPaths(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return adminPaths(path);
    if (name !== "page.tsx") return [];
    return [`/${relative(adminApp, dir)}`.replace(/\/$/, "").replace(/\[(\w+)\]/g, (_, key: string) => param(key)) || "/"];
  });
}

function param(key: string): string {
  const prefix = `${key}=`;
  const given = process.argv.find((value, index) => process.argv[index - 1] === "--param" && value.startsWith(prefix));
  if (!given) throw new Error(`--param ${key}=<value> is required`);
  return given.slice(prefix.length);
}

const adminApp = join(import.meta.dir, "../../web/apps/admin/src/app/(admin)");
const targets =
  app === "admin"
    ? adminPaths(adminApp).map((path) => ({ path, url: `${base}${path}` }))
    : (() => {
        const appBase = arg("app-base", base);
        const slug = arg(
          "slug",
          readdirSync(join(import.meta.dir, "../../web/apps/web/public/blog")).find((name) => name !== "authors"),
        );
        return routes
          .filter((r) => r.kind === "page")
          .map((row) => {
            const path = row.path.replace("$slug", slug);
            return { path, url: `${row.host === "app" ? appBase : base}${path}` };
          });
      })();

mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const failures: string[] = [];

if (app === "admin") {
  // Sign in through the login form: Playwright's request client under Bun
  // stalls on the empty 303 that /auth/login answers.
  await page.goto(`${base}/login?next=%2Foverview`, { waitUntil: "domcontentloaded" });
  await page.fill('input[name="login"]', process.env.ADMIN_USER ?? "");
  await page.fill('input[name="password"]', process.env.ADMIN_PASSWORD ?? "");
  await Promise.all([page.waitForURL(`${base}/overview`), page.click('button[type="submit"]')]);
  const signedIn = (await page.context().cookies(base)).some((cookie) => cookie.name === "loyal_admin_session");
  if (!signedIn) throw new Error(`admin sign-in failed at ${page.url()}`);
}

for (const { path, url } of targets) {
  const errors: string[] = [];
  page.removeAllListeners("pageerror");
  page.on("pageerror", (error) => errors.push(error.message));
  const response = await page.goto(url, { waitUntil: "load", timeout: 60_000 }).catch((error: Error) => {
    errors.push(error.message);
    return null;
  });
  await page.waitForLoadState("networkidle", { timeout: 10_000 }).catch(() => undefined);
  const name = path === "/" ? "index" : path.slice(1).replaceAll("/", "__");
  await page.screenshot({ path: join(out, `${name}.png`), fullPage: true }).catch(() => undefined);
  const status = response?.status() ?? 0;
  const ok = status > 0 && status < 400 && errors.length === 0;
  console.log(`${ok ? "ok  " : "FAIL"} ${status} ${url}${errors.length ? ` ${errors[0]}` : ""}`);
  if (!ok) failures.push(url);
}

await browser.close();
console.log(`${targets.length - failures.length}/${targets.length} rendered`);
process.exit(failures.length ? 1 : 0);
