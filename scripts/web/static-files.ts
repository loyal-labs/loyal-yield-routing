import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { routes } from "../../web/apps/loyal/src/routes.table";

const root = join(import.meta.dir, "../..");
const invocationCwd = process.cwd();
const dist = resolve(
  invocationCwd,
  process.argv[2] ?? join(root, "web/apps/loyal/dist")
);
const webApp = join(root, "web/apps/web");
const fontGoogleCompat = join(
  root,
  "web/apps/loyal/src/next-compat/font-google.ts"
);

function prepareAppsWebImports(): void {
  Bun.plugin({
    name: "loyal-web-static-files",
    setup(build) {
      build.module("server-only", () => ({
        contents: "export {};",
        loader: "js",
      }));
      build.module("next/font/google", () => ({
        contents: `export * from ${JSON.stringify(fontGoogleCompat)};`,
        loader: "ts",
      }));
    },
  });
  process.chdir(webApp);
}

process.env.NODE_ENV ??= "production";
prepareAppsWebImports();

type SitemapEntry = {
  url: string;
  lastModified?: string | Date;
  changeFrequency?:
    | "always"
    | "hourly"
    | "daily"
    | "weekly"
    | "monthly"
    | "yearly"
    | "never";
  priority?: number;
  alternates?: { languages?: Record<string, string> };
};

function xmlEscape(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/'/g, "&apos;")
    .replace(/"/g, "&quot;");
}

function serializeSitemap(entries: SitemapEntry[]): string {
  const urls = entries.map((entry) => {
    const lines = [`<url>`, `<loc>${xmlEscape(entry.url)}</loc>`];
    for (const [hreflang, href] of Object.entries(
      entry.alternates?.languages ?? {}
    )) {
      lines.push(
        `<xhtml:link rel="alternate" hreflang="${xmlEscape(
          hreflang
        )}" href="${xmlEscape(href)}" />`
      );
    }
    if (entry.lastModified !== undefined) {
      const lastmod =
        entry.lastModified instanceof Date
          ? entry.lastModified.toISOString()
          : entry.lastModified;
      lines.push(`<lastmod>${xmlEscape(lastmod)}</lastmod>`);
    }
    if (entry.changeFrequency !== undefined) {
      lines.push(`<changefreq>${entry.changeFrequency}</changefreq>`);
    }
    if (entry.priority !== undefined) {
      lines.push(`<priority>${entry.priority}</priority>`);
    }
    lines.push(`</url>`);
    return lines.join("\n");
  });

  return (
    [
      `<?xml version="1.0" encoding="UTF-8"?>`,
      `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">`,
      ...urls,
      `</urlset>`,
    ].join("\n") + "\n"
  );
}

const { default: sitemap } = await import(join(webApp, "src/app/sitemap.ts"));
writeFileSync(join(dist, "sitemap.xml"), serializeSitemap(await sitemap()));

const { getAllBlogSlugs, getBlogPosts } = await import(
  join(webApp, "src/features/blog/data.ts")
);
const blogSlugs: string[] = await getAllBlogSlugs();
const pages = routes
  .filter((route) => route.kind === "page")
  .flatMap((route) => {
    if (route.path === "/blog/$slug")
      return blogSlugs.map((slug) => ({ ...route, path: `/blog/${slug}` }));
    if (route.path.includes("$"))
      throw new Error(`Unexpanded page route: ${route.path}`);
    return [route];
  });
writeFileSync(
  join(dist, "page-paths.json"),
  JSON.stringify(pages.map((page) => page.path))
);

for (const name of ["llms.txt", "llms-full.txt"]) {
  const route = await import(join(webApp, `src/app/${name}/route.ts`));
  writeFileSync(join(dist, name), await (await route.GET()).text());
}

const { totalPages: blogPages } = await getBlogPosts({ page: 1 });
const prerendered = [
  ...pages
    .filter((page) => page.host === "site")
    .map((page) => ({
      href: page.path,
      file:
        page.path === "/"
          ? "__root/index.html"
          : `${page.path.slice(1)}/index.html`,
    })),
  ...Array.from({ length: blogPages - 1 }, (_, index) => ({
    href: `/blog?page=${index + 2}`,
    file: `blog/__page/${index + 2}/index.html`,
  })),
];

const prerender = await import(
  join(root, "web/apps/loyal/build/prerender/prerender.js")
);
const shell = readFileSync(join(dist, "index.html"), "utf8");

function replaceOnce(
  html: string,
  pattern: RegExp,
  replacement: string
): string {
  const matches = html.match(new RegExp(pattern.source, `${pattern.flags}g`));
  if (matches?.length !== 1)
    throw new Error(
      `index.html changed shape: ${pattern} matched ${
        matches?.length ?? 0
      } times`
    );
  return html.replace(pattern, () => replacement);
}

const hoisted = /^(?:<title>[^<]*<\/title>|<meta [^>]*\/>|<link [^>]*\/>)/;

for (const { href, file } of prerendered) {
  let body: string = await prerender.render(href);
  let head = "";
  for (let tag = body.match(hoisted); tag; tag = body.match(hoisted)) {
    head += tag[0];
    body = body.slice(tag[0].length);
  }
  if (!head.includes("<title>"))
    throw new Error(`${href}: no <title> rendered`);
  let html = replaceOnce(
    shell,
    /<html lang="en"/,
    `<html lang="${prerender.htmlLang(href)}"`
  );
  html = replaceOnce(html, /<\/head>/, `${head}</head>`);
  html = replaceOnce(
    html,
    /<body class="antialiased"/,
    `<body class="antialiased ${prerender.bodyClass}"`
  );
  html = replaceOnce(
    html,
    /<div id="root">[\s\S]*?<div id="app-load-error"[\s\S]*?<\/div>/,
    `<div id="root" data-prerendered>${body}</div>`
  );
  mkdirSync(dirname(join(dist, file)), { recursive: true });
  writeFileSync(join(dist, file), html);
}

console.log(`static files written to ${dist}`);
