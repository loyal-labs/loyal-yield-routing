import type { Metadata } from "next";

import { SITE_URL } from "@/lib/seo/site";

export type Head = {
  meta: Array<Record<string, string>>;
  links: Array<Record<string, string>>;
};

// The exact head Next 15 renders for a Metadata, observed against production
// (askloyal.com), which head-diff.ts re-checks:
//   <title> | meta description | meta robots | link canonical | link
//   alternate (hreflang) | og:title og:description og:url og:site_name
//   og:locale og:image og:image:width og:image:height og:image:alt og:type |
//   twitter:card twitter:title twitter:description twitter:image
//   twitter:image:alt | link shortcut icon, link icon, link apple-touch-icon
// Two Next behaviours the shape depends on:
//  - Metadata merges shallowly per top-level key down the segment chain, so a
//    page's openGraph replaces the root's whole (og:site_name does not leak
//    onto pages that set their own openGraph).
//  - TanStack's HeadContent dedupes meta by name/property (deepest route
//    wins) but links only by exact equality. Root tags therefore cannot carry
//    contested keys: a root canonical would double every page's canonical.
// So the root layout's metadata (marked by metadataBase, which only it sets)
// is remembered and its call emits only the shell tags — title, description,
// icons. Every page's call merges its metadata over the root's and emits the
// full merged set; pages without metadata (Next: inherit everything) emit the
// root's set. Existing callers keep calling headFromMetadata(metadata).

type TitleDescriptor = { absolute?: string; default?: string; template?: string };

type MetaImage = { url: string | URL; width?: number | string; height?: number | string; alt?: string };

type OpenGraphView = {
  title?: string | null;
  description?: string | null;
  url?: string | URL | null;
  type?: string;
  siteName?: string | string[];
  locale?: string;
  images?: Array<string | URL | MetaImage> | string | URL;
  publishedTime?: string | Date;
  modifiedTime?: string | Date;
};

type TwitterView = {
  card?: string;
  title?: string;
  description?: string;
  images?: Array<string | URL | MetaImage> | string | URL;
};

type AlternatesView = { canonical?: string | URL | null; languages?: Record<string, string> | null };

type IconsView = Partial<Record<"icon" | "shortcut" | "apple", string | URL | Array<string | URL>>>;

type RobotsView = string | { index?: boolean; follow?: boolean; nocache?: boolean };

type MetadataView = {
  title?: Metadata["title"];
  description?: string | null;
  metadataBase?: URL | null;
  alternates?: AlternatesView | null;
  icons?: IconsView | null;
  openGraph?: OpenGraphView | null;
  twitter?: TwitterView | null;
  robots?: RobotsView | null;
};

type MergedMetadata = MetadataView & { metadataBase: URL };

let rootMetadata: MetadataView | undefined;

export function headFromMetadata(metadata: Metadata | undefined): Head {
  const view = metadata as MetadataView | undefined;
  if (view?.metadataBase) rootMetadata = view;

  const root = rootMetadata ?? view;
  const base = view?.metadataBase ?? root?.metadataBase ?? new URL(SITE_URL);
  if (view?.metadataBase) return shellHead({ ...view, metadataBase: base });
  const merged: MergedMetadata = { ...root, ...view, metadataBase: base };
  return fullHead(merged);
}

function shellHead(root: MergedMetadata): Head {
  const meta: Head["meta"] = [];
  const links: Head["links"] = [];
  const title = renderTitle(root, root);
  if (title) meta.push({ title });
  if (root.description) meta.push({ name: "description", content: root.description });
  pushIcons(links, root.icons);
  return { meta, links };
}

function fullHead(merged: MergedMetadata): Head {
  const meta: Head["meta"] = [];
  const links: Head["links"] = [];
  const title = renderTitle(merged, rootMetadata);
  if (title) meta.push({ title });
  if (merged.description) meta.push({ name: "description", content: merged.description });
  const robots = robotsContent(merged.robots);
  if (robots) meta.push({ name: "robots", content: robots });

  const og = merged.openGraph;
  if (og) {
    if (og.title) meta.push({ property: "og:title", content: og.title });
    if (og.description) meta.push({ property: "og:description", content: og.description });
    const url = resolveUrl(og.url, merged.metadataBase);
    if (url) meta.push({ property: "og:url", content: url });
    const siteName = typeof og.siteName === "string" ? og.siteName : og.siteName?.join(", ");
    if (siteName) meta.push({ property: "og:site_name", content: siteName });
    if (og.locale) meta.push({ property: "og:locale", content: og.locale });
    for (const image of resolveImages(og.images, merged.metadataBase)) {
      meta.push({ property: "og:image", content: image.url });
      if (image.width) meta.push({ property: "og:image:width", content: image.width });
      if (image.height) meta.push({ property: "og:image:height", content: image.height });
      if (image.alt) meta.push({ property: "og:image:alt", content: image.alt });
    }
    if (og.type) meta.push({ property: "og:type", content: og.type });
    if (og.publishedTime) meta.push({ property: "article:published_time", content: isoTime(og.publishedTime) });
    if (og.modifiedTime) meta.push({ property: "article:modified_time", content: isoTime(og.modifiedTime) });
  }

  const twitter = merged.twitter;
  if (twitter) {
    if (twitter.card) meta.push({ name: "twitter:card", content: twitter.card });
    if (twitter.title) meta.push({ name: "twitter:title", content: twitter.title });
    if (twitter.description) meta.push({ name: "twitter:description", content: twitter.description });
    for (const image of resolveImages(twitter.images, merged.metadataBase)) {
      meta.push({ name: "twitter:image", content: image.url });
      if (image.alt) meta.push({ name: "twitter:image:alt", content: image.alt });
    }
  }

  const canonical = resolveUrl(merged.alternates?.canonical ?? undefined, merged.metadataBase);
  if (canonical) links.push({ rel: "canonical", href: canonical });
  for (const [lang, href] of Object.entries(merged.alternates?.languages ?? {})) {
    const resolved = resolveUrl(href, merged.metadataBase);
    if (resolved) links.push({ rel: "alternate", hrefLang: lang, href: resolved });
  }
  pushIcons(links, merged.icons);
  return { meta, links };
}

function renderTitle(metadata: MetadataView, parent: MetadataView | undefined): string | undefined {
  const title = metadata.title as string | TitleDescriptor | null | undefined;
  if (typeof title === "string") {
    const parentTitle = parent?.title as TitleDescriptor | string | null | undefined;
    const template = typeof parentTitle === "object" && parentTitle !== null ? parentTitle.template : undefined;
    return template ? template.replace("%s", title) : title;
  }
  if (title && typeof title === "object") return title.absolute ?? title.default ?? undefined;
  return undefined;
}

function robotsContent(robots: RobotsView | null | undefined): string | undefined {
  if (typeof robots === "string") return robots || undefined;
  if (!robots) return undefined;
  const parts: string[] = [];
  if (robots.index !== undefined) parts.push(robots.index ? "index" : "noindex");
  if (robots.follow !== undefined) parts.push(robots.follow ? "follow" : "nofollow");
  if (robots.nocache) parts.push("noarchive");
  return parts.length > 0 ? parts.join(", ") : undefined;
}

type ResolvedImage = { url: string; width?: string; height?: string; alt?: string };

function resolveImages(images: OpenGraphView["images"], base: URL): ResolvedImage[] {
  const list = Array.isArray(images) ? images : images ? [images] : [];
  return list
    .map((image): ResolvedImage | undefined => {
      if (typeof image === "string" || image instanceof URL) {
        const url = resolveUrl(image, base);
        return url ? { url } : undefined;
      }
      const url = resolveUrl(image.url, base);
      if (!url) return undefined;
      return {
        url,
        width: image.width === undefined ? undefined : String(image.width),
        height: image.height === undefined ? undefined : String(image.height),
        alt: image.alt,
      };
    })
    .filter((image) => image !== undefined);
}

// Icon hrefs pass through as authored: Next keeps them relative ("/icon.svg"
// renders as-is), unlike canonical/og URLs which resolve against
// metadataBase.
function pushIcons(links: Head["links"], icons: IconsView | undefined | null): void {
  if (!icons) return;
  for (const [rel, key] of [
    ["shortcut icon", "shortcut"],
    ["icon", "icon"],
    ["apple-touch-icon", "apple"],
  ] as const) {
    const value = icons[key];
    for (const href of Array.isArray(value) ? value : value ? [value] : []) {
      links.push({ rel, href: href instanceof URL ? href.toString() : href });
    }
  }
}

function isoTime(value: string | Date): string {
  return typeof value === "string" ? value : value.toISOString();
}

// Next trims an origin-only absolute URL to the bare origin.
function resolveUrl(value: string | URL | null | undefined, base: URL): string | undefined {
  if (value === null || value === undefined || value === "") return undefined;
  const resolved = new URL(value instanceof URL ? value.toString() : value, base);
  if (resolved.pathname === "/") return resolved.origin;
  return resolved.toString();
}
