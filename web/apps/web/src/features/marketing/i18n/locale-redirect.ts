import { type NextRequest, NextResponse } from "next/server";

import {
  LOCALE_COOKIE,
  LOCALE_PARAM,
  LOCALES,
  type Locale,
  localizedHref,
  TRANSLATED_PATHS,
} from "./locale";

const LOCALE_COOKIE_MAX_AGE = 60 * 60 * 24 * 365;
const BOT_USER_AGENT_RE =
  /bot|crawl|spider|slurp|preview|facebookexternalhit|embedly|whatsapp|telegram/i;

function isLocale(value: string | null | undefined): value is Locale {
  return (LOCALES as readonly string[]).includes(value ?? "");
}

function isTranslatedPath(path: string): boolean {
  return (TRANSLATED_PATHS as readonly string[]).includes(path);
}

function preferredLocale(acceptLanguage: string | null): Locale | null {
  if (!acceptLanguage) {
    return null;
  }
  const ranked = acceptLanguage
    .split(",")
    .map((part, index) => {
      const [tag = "", ...params] = part.trim().split(";");
      const qParam = params.find((param) => param.trim().startsWith("q="));
      const quality = qParam ? Number(qParam.trim().slice(2)) : 1;
      const language = tag.toLowerCase().split("-")[0];
      return { index, language, quality: Number.isNaN(quality) ? 0 : quality };
    })
    .filter(({ quality }) => quality > 0)
    .sort((a, b) => b.quality - a.quality || a.index - b.index);

  for (const { language } of ranked) {
    if (isLocale(language)) {
      return language;
    }
  }
  return null;
}

export function handleLocaleRedirect(
  request: NextRequest
): NextResponse | null {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return null;
  }

  const url = request.nextUrl;
  const requested = url.searchParams.get(LOCALE_PARAM);

  if (isLocale(requested)) {
    const target = url.clone();
    target.searchParams.delete(LOCALE_PARAM);
    const response = NextResponse.redirect(target, 307);
    response.cookies.set(LOCALE_COOKIE, requested, {
      maxAge: LOCALE_COOKIE_MAX_AGE,
      path: "/",
      sameSite: "lax",
    });
    response.headers.set("Cache-Control", "private, no-store");
    return response;
  }

  if (!isTranslatedPath(url.pathname)) {
    return null;
  }
  if (isLocale(request.cookies.get(LOCALE_COOKIE)?.value)) {
    return null;
  }
  if (BOT_USER_AGENT_RE.test(request.headers.get("user-agent") ?? "")) {
    return null;
  }
  const preferred = preferredLocale(request.headers.get("accept-language"));
  if (preferred === null || preferred === "en") {
    return null;
  }

  const target = url.clone();
  target.pathname = localizedHref(preferred, url.pathname);
  const response = NextResponse.redirect(target, 307);
  response.headers.set("Cache-Control", "private, no-store");
  response.headers.set("Vary", "Accept-Language, Cookie");
  return response;
}
