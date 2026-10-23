import { writeFileSync } from "node:fs";
import { join, resolve } from "node:path";

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

const { getAllBlogSlugs } = await import(
  join(webApp, "src/features/blog/data.ts")
);
const blogSlugs: string[] = await getAllBlogSlugs();
const pagePaths = routes
  .filter((route) => route.kind === "page")
  .flatMap((route) => {
    if (route.path === "/blog/$slug")
      return blogSlugs.map((slug) => `/blog/${slug}`);
    if (route.path.includes("$"))
      throw new Error(`Unexpanded page route: ${route.path}`);
    return [route.path];
  });
writeFileSync(join(dist, "page-paths.json"), JSON.stringify(pagePaths));

for (const name of ["llms.txt", "llms-full.txt"]) {
  const route = await import(join(webApp, `src/app/${name}/route.ts`));
  writeFileSync(join(dist, name), await (await route.GET()).text());
}

console.log(`static files written to ${dist}`);
