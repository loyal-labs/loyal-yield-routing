import type { MetadataRoute } from "next";

import { blogPostModifiedDate, getAllBlogPosts } from "@/features/blog";
import {
  alternateLanguages,
  type Locale,
  localizedHref,
  type TranslatedPath,
  TRANSLATED_PATHS,
} from "@/features/marketing/i18n/locale";
import { siteUrl } from "@/lib/seo/site";

/**
 * The sitemap, generated from the routes and the blog archive.
 *
 * Static by design. It reads post markdown through the blog loader, and
 * next.config.ts only traces public/blog/**\/*.md into the /blog function, so a
 * runtime sitemap would find no posts in production. Being generated at build
 * time it needs no tracing entry, and since posts arrive by commit and deploy,
 * a build-time snapshot is never stale.
 *
 * Deliberately absent: /app and its children (the authenticated wallet), /500,
 * /ingest (the analytics proxy), and /landing (an unlinked AI-chat demo) are
 * all disallowed in robots.txt, and paginated listings (/blog?page=N) carry no
 * content that isn't already reachable through the post URLs below.
 */

/**
 * Marketing routes and the date each was last meaningfully edited.
 *
 * An explicit map rather than a git-derived date: Vercel builds from a shallow
 * clone, so commit timestamps aren't dependable at build time, and a lastmod
 * that changes on every unrelated deploy teaches crawlers to ignore it. Update
 * the date here when you change a page's content.
 *
 * `translations` marks a route that also exists under a locale prefix (/ru,
 * /zh), with the date each translated copy was last meaningfully edited.
 * Update the date when you change that locale's dictionary for the page.
 */
type TranslationDates = Partial<Record<Exclude<Locale, "en">, string>>;

const TRANSLATED_ROUTE_DATES = {
  "/": {
    lastModified: "2026-09-22",
    translations: { ru: "2026-10-03", zh: "2026-10-10" },
  },
  "/earn": {
    lastModified: "2026-09-22",
    translations: { ru: "2026-10-03", zh: "2026-10-10" },
  },
  "/agents": {
    lastModified: "2026-05-28",
    translations: { ru: "2026-10-03", zh: "2026-10-10" },
  },
  "/trust": {
    lastModified: "2026-09-22",
    translations: { ru: "2026-10-03", zh: "2026-10-10" },
  },
  "/risks": {
    lastModified: "2026-09-22",
    translations: { ru: "2026-10-03", zh: "2026-10-10" },
  },
} satisfies Record<
  TranslatedPath,
  { lastModified: string; translations: TranslationDates }
>;

const STATIC_ROUTES: ReadonlyArray<
  | { path: string; lastModified: string; translations?: undefined }
  | { path: TranslatedPath; lastModified: string; translations: TranslationDates }
> = [
  ...TRANSLATED_PATHS.map((path) => ({ path, ...TRANSLATED_ROUTE_DATES[path] })),
  { path: "/privacy-policy", lastModified: "2026-02-23" },
];

export default async function sitemap(): Promise<MetadataRoute.Sitemap> {
  const posts = await getAllBlogPosts();

  const staticEntries = STATIC_ROUTES.flatMap((route) => {
    if (!route.translations) {
      return [{ url: siteUrl(route.path), lastModified: route.lastModified }];
    }
    const { path, lastModified, translations } = route;
    const alternates = { languages: alternateLanguages(path, siteUrl) };
    const translated = Object.entries(translations).map(
      ([locale, translatedAt]) => ({
        url: siteUrl(localizedHref(locale as Locale, path)),
        lastModified: translatedAt,
        alternates,
      })
    );
    return [{ url: siteUrl(path), lastModified, alternates }, ...translated];
  });

  const newest = posts.at(0);
  const blogIndex = {
    url: siteUrl("/blog"),
    lastModified: newest ? blogPostModifiedDate(newest) : undefined,
  };

  const postEntries = posts.map((post) => ({
    url: siteUrl(`/blog/${post.slug}`),
    lastModified: blogPostModifiedDate(post),
  }));

  return [...staticEntries, blogIndex, ...postEntries];
}
