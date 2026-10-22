import { MARKETING_PAGE_SLUGS, type MarketingPageSlug } from "../registry";

export const LOCALES = ["en", "ru", "zh"] as const;
export type Locale = (typeof LOCALES)[number];

export const LOCALE_COOKIE = "loyal_locale";
export const LOCALE_PARAM = "lang";

type MarketingPath = `/${MarketingPageSlug}`;

const MARKETING_PATHS: readonly MarketingPath[] = MARKETING_PAGE_SLUGS.map(
  (slug): MarketingPath => `/${slug}`
);

export const TRANSLATED_PATHS = [
  "/",
  ...MARKETING_PATHS,
  "/trust",
  "/risks",
] as const;
export type TranslatedPath = (typeof TRANSLATED_PATHS)[number];

const LOCALE_PREFIX: Record<Locale, string> = { en: "", ru: "/ru", zh: "/zh" };

export const LOCALE_TAGS: Record<Locale, string> = {
  en: "en",
  ru: "ru",
  zh: "zh-Hans",
};

function isTranslatedPath(path: string): path is TranslatedPath {
  return (TRANSLATED_PATHS as readonly string[]).includes(path);
}

export function localizedHref(locale: Locale, href: string): string {
  const prefix = LOCALE_PREFIX[locale];
  if (prefix === "" || !href.startsWith("/")) {
    return href;
  }
  const hashIndex = href.indexOf("#");
  const path = hashIndex === -1 ? href : href.slice(0, hashIndex);
  const hash = hashIndex === -1 ? "" : href.slice(hashIndex);
  if (!isTranslatedPath(path)) {
    return href;
  }
  return `${prefix}${path === "/" ? "" : path}${hash}`;
}

export function alternateLanguages(
  path: TranslatedPath,
  toUrl: (href: string) => string = (href) => href
): Record<string, string> {
  const languages: Record<string, string> = {};
  for (const locale of LOCALES) {
    languages[LOCALE_TAGS[locale]] = toUrl(localizedHref(locale, path));
  }
  languages["x-default"] = toUrl(path);
  return languages;
}
