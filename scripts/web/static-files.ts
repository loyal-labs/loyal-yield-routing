// Writes the three SEO files that are Next routes in apps/web into the built
// Vite dist, so the Go server can serve them as plain files:
//
//   bun scripts/web/static-files.ts <dist-dir>   (default: web/apps/loyal/dist)
//
//   sitemap.xml     apps/web src/app/sitemap.ts (default export)
//   llms.txt        apps/web src/app/llms.txt/route.ts (GET)
//   llms-full.txt   apps/web src/app/llms-full.txt/route.ts (GET)
//
// It also drops the SPA shell into the directories the copied public/blog
// tree puts under dist/blog/: webserve's seed handler serves a directory
// listing for an existing path, which would otherwise shadow the /blog and
// /blog/<slug> client routes with an index of post media. Same workaround
// apps/web itself made on Vercel, which is why the post body is not
// index.md (see features/blog/data.ts): keep an index.* out of the shadowed
// URL by putting the page there instead.

// Both llms handlers and the sitemap read the blog archive through apps/web's
// own loader, so their output here is by construction what Vercel serves. The
// script imports those modules under bun, which needs two shims: their import
// chain pulls in `server-only` (a marker that throws outside a React server)
// and `next/font/google` (bundler-generated). Both are answered by the same
// src/next-compat stubs the Vite build aliases in, registered as a runtime
// plugin before the imports. apps/web resolves public/blog relative to its own
// cwd, so the script runs from there; the dist argument is resolved first.
import { readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";

const root = join(import.meta.dir, "../..");
const dist = resolve(process.argv[2] ?? join(root, "web/apps/loyal/dist"));
const webApp = join(root, "web/apps/web");
const fontGoogleCompat = join(
  root,
  "web/apps/loyal/src/next-compat/font-google.ts"
);

Bun.plugin({
  name: "loyal-web-static-files",
  setup(build) {
    build.module("server-only", () => ({ contents: "export {};", loader: "js" }));
    build.module("next/font/google", () => ({
      contents: `export * from ${JSON.stringify(fontGoogleCompat)};`,
      loader: "ts",
    }));
  },
});

process.chdir(webApp);

// One MetadataRoute.Sitemap entry, as apps/web's sitemap.ts returns it.
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

// Serialises entries the way Next renders an app-router sitemap: one unindented
// <url> block per entry, loc, then the hreflang alternates, then lastmod.
function serializeSitemap(entries: SitemapEntry[]): string {
  const urls = entries.map((entry) => {
    const lines = [`<url>`, `<loc>${xmlEscape(entry.url)}</loc>`];
    for (const [hreflang, href] of Object.entries(
      entry.alternates?.languages ?? {}
    )) {
      lines.push(
        `<xhtml:link rel="alternate" hreflang="${xmlEscape(hreflang)}" href="${xmlEscape(href)}" />`
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

  return [
    `<?xml version="1.0" encoding="UTF-8"?>`,
    `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">`,
    ...urls,
    `</urlset>`,
  ].join("\n") + "\n";
}

const { default: sitemap } = await import(join(webApp, "src/app/sitemap.ts"));
writeFileSync(join(dist, "sitemap.xml"), serializeSitemap(await sitemap()));

for (const name of ["llms.txt", "llms-full.txt"]) {
  const route = await import(join(webApp, `src/app/${name}/route.ts`));
  writeFileSync(join(dist, name), await (await route.GET()).text());
}

// The SPA shell, byte-for-byte, at the paths the blog asset tree shadows.
const { getAllBlogSlugs } = await import(join(webApp, "src/features/blog"));
const shell = readFileSync(join(dist, "index.html"));
writeFileSync(join(dist, "blog/index.html"), shell);
for (const slug of await getAllBlogSlugs()) {
  writeFileSync(join(dist, "blog", slug, "index.html"), shell);
}

console.log(`static files written to ${dist}`);
