// Diff the <head> a page should have: fetch the Vercel deployment's server
// HTML, render the local loyal-web page with Playwright, and compare title,
// meta[name|property] content and the SEO head links (canonical, alternate,
// icon, shortcut icon, apple-touch-icon). Exits 1 on any difference.
//
//   bun scripts/web/head-diff.ts --vercel https://askloyal.com \
//     --local http://127.0.0.1:18081 /trust /blog
//
// Not compared, deliberately: tag order, Next's preloads/stylesheets/script
// tags, and next-size-adjust (the next/font fallback internal, which has no
// rendering effect).
import { chromium } from "../../web/node_modules/playwright";

function args(): { vercel: string; local: string; paths: string[] } {
  const argv = process.argv.slice(2);
  let vercel: string | undefined;
  let local: string | undefined;
  const paths: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--vercel") vercel = argv[++i];
    else if (argv[i] === "--local") local = argv[++i];
    else if (argv[i].startsWith("/")) paths.push(argv[i]);
  }
  if (!vercel || !local || paths.length === 0) {
    throw new Error("usage: head-diff.ts --vercel <origin> --local <origin> <path>...");
  }
  return { vercel, local, paths };
}

type HeadSnapshot = {
  title: string;
  metas: Map<string, string[]>;
  links: Map<string, string[]>;
};

const COMPARED_LINK_RELS = new Set(["canonical", "alternate", "icon", "shortcut icon", "apple-touch-icon"]);

function push(map: Map<string, string[]>, key: string, value: string): void {
  const values = map.get(key);
  if (values) values.push(value);
  else map.set(key, [value]);
}

function extractAttributes(tag: string): Map<string, string> {
  const attrs = new Map<string, string>();
  for (const [, name, value] of tag.matchAll(/([a-zA-Z:_.-]+)="([^"]*)"/g)) {
    attrs.set(name.toLowerCase(), value);
  }
  return attrs;
}

async function vercelHead(origin: string, path: string): Promise<HeadSnapshot> {
  const response = await fetch(`${origin}${path}`, { redirect: "follow" });
  if (!response.ok) throw new Error(`${origin}${path} -> ${response.status}`);
  const html = await response.text();
  const head = html.slice(0, html.indexOf("</head>"));

  const titleMatch = head.match(/<title>([\s\S]*?)<\/title>/);
  const metas = new Map<string, string[]>();
  const links = new Map<string, string[]>();
  for (const tag of head.matchAll(/<(meta|link)\b[^>]*>/g)) {
    const attrs = extractAttributes(tag[0]);
    if (tag[1] === "meta") {
      const key = attrs.get("name") ?? attrs.get("property");
      if (key) push(metas, key, attrs.get("content") ?? "");
    } else {
      const rel = attrs.get("rel") ?? "";
      if (COMPARED_LINK_RELS.has(rel)) {
        push(links, `${rel}|${attrs.get("hreflang") ?? ""}`, attrs.get("href") ?? "");
      }
    }
  }
  return { title: (titleMatch?.[1] ?? "").trim(), metas, links };
}

async function localHead(origin: string, path: string, page: import("playwright").Page): Promise<HeadSnapshot> {
  await page.goto(`${origin}${path}`, { waitUntil: "networkidle", timeout: 60_000 });
  const snapshot = await page.evaluate(() => ({
    title: document.title,
    metas: [...document.head.querySelectorAll<HTMLMetaElement>("meta[name], meta[property]")].map((meta) => [
      meta.getAttribute("name") ?? meta.getAttribute("property") ?? "",
      meta.getAttribute("content") ?? "",
    ]),
    links: [...document.head.querySelectorAll<HTMLLinkElement>("link[rel]")].map((link) => [
      link.getAttribute("rel") ?? "",
      link.getAttribute("hreflang") ?? "",
      link.getAttribute("href") ?? "",
    ]),
  }));
  const metas = new Map<string, string[]>();
  const links = new Map<string, string[]>();
  for (const [key, content] of snapshot.metas) {
    if (key === "next-size-adjust") continue;
    push(metas, key, content);
  }
  for (const [rel, hreflang, href] of snapshot.links) {
    if (COMPARED_LINK_RELS.has(rel)) push(links, `${rel}|${hreflang}`, href);
  }
  return { title: snapshot.title, metas, links };
}

function diffOne(kind: string, key: string, vercel: string[] | undefined, local: string[] | undefined): string | undefined {
  if (JSON.stringify(vercel ?? []) === JSON.stringify(local ?? [])) return undefined;
  return `  ${kind} ${key}:\n    vercel: ${JSON.stringify(vercel ?? null)}\n    local:  ${JSON.stringify(local ?? null)}`;
}

function diffHead(vercel: HeadSnapshot, local: HeadSnapshot): string[] {
  const differences: string[] = [];
  const title = diffOne("title", "", vercel.title ? [vercel.title] : undefined, local.title ? [local.title] : undefined);
  if (title) differences.push(title);
  for (const key of new Set([...vercel.metas.keys(), ...local.metas.keys()])) {
    const line = diffOne("meta", key, vercel.metas.get(key), local.metas.get(key));
    if (line) differences.push(line);
  }
  for (const key of new Set([...vercel.links.keys(), ...local.links.keys()])) {
    const line = diffOne("link", key, vercel.links.get(key), local.links.get(key));
    if (line) differences.push(line);
  }
  return differences;
}

const { vercel: vercelOrigin, local: localOrigin, paths } = args();
const browser = await chromium.launch();
const page = await browser.newPage();
let anyDifference = false;

for (const path of paths) {
  let differences: string[];
  try {
    const [vercel, local] = await Promise.all([vercelHead(vercelOrigin, path), localHead(localOrigin, path, page)]);
    differences = diffHead(vercel, local);
  } catch (error) {
    differences = [`  ${(error as Error).message}`];
  }
  if (differences.length > 0) anyDifference = true;
  console.log(`${path}: ${differences.length === 0 ? "same" : "DIFFERENT"}`);
  for (const line of differences) console.log(line);
}

await browser.close();
process.exit(anyDifference ? 1 : 0);
