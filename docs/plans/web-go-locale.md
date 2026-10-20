# Locale redirect behaviour of apps/web — spec for the Go server half

What `web/apps/web/src/middleware.ts` plus
`web/apps/web/src/features/marketing/i18n/locale-redirect.ts` (with
`locale.ts`) do to every request, so G5 can reproduce it in Go. Every row
marked **checked on Vercel** was reproduced with curl against
`https://askloyal.com` / `https://app.askloyal.com` on 2026-10-20.

Constants (`locale.ts`): `LOCALES = en, ru, zh`; cookie
`loyal_locale`; query param `lang`; `TRANSLATED_PATHS = /, /earn, /agents,
/trust, /risks` (these five, exactly — nothing else is localized).
Localized twins: `/` → `/ru`, `/zh`; `/earn` → `/ru/earn`, `/zh/earn`; same
for `/agents`, `/trust`, `/risks`.

## Order of decisions

1. **Matcher** (`middleware.ts` config): the middleware never sees a path
   whose last segment contains a dot, nor `_next/*`, `api/*`,
   `favicon.ico`. Such requests are served directly, no redirect, no cookie.
2. **Host**: `Host` header minus port. On `app.askloyal.com` (the app host)
   the locale logic is skipped entirely: `/` is rewritten to `/app` (200,
   same URL in the browser), everything else passes through. No
   `loyal_locale` cookie is ever set on the app host.
3. `handleLocaleRedirect` on every other host, in this order:
   1. Method other than GET or HEAD → pass through unchanged.
   2. `?lang=<en|ru|zh>` → **307** to the same path and query with only the
      `lang` parameter removed, `Set-Cookie: loyal_locale=<locale>;
      Path=/; Max-Age=31536000; SameSite=lax` (no Expires beyond Max-Age's
      implied date, no Domain, no Secure, no HttpOnly),
      `Cache-Control: private, no-store`, **no Vary**.
   3. Path not in TRANSLATED_PATHS → pass through.
   4. Cookie `loyal_locale` with a valid locale value → pass through (an
      invalid value such as `fr` counts as absent).
   5. Bot User-Agent (`/bot|crawl|spider|slurp|preview|facebookexternalhit|
      embedly|whatsapp|telegram/i`) → pass through (crawlers must index all
      locales through hreflang).
   6. Accept-Language ranks no locale, or ranks `en` first → pass through.
   7. Otherwise **307** to the localized twin (query string kept),
      `Cache-Control: private, no-store`,
      `Vary: Accept-Language, Cookie`. No Set-Cookie.
4. Both redirect kinds are 307 with a path-only Location (no scheme/host).

`preferredLocale(Accept-Language)`: split on `,`; each entry is
`tag;q=…` with q defaulting to 1 and a non-numeric q counting 0; drop
q ≤ 0; sort by q descending, ties by original order; lowercase the primary
subtag (text before the first `-`); return the first that is in LOCALES,
else null. So `zh-TW` and `zh-HK` readers get the `zh` pages (Simplified
only); `en-US,ru;q=0.5` gets `en` → no redirect.

## Case table

Defaults for brevity: method GET, marketing host `askloyal.com`, browser
UA (not bot), no `loyal_locale` cookie, no `lang` param. Only the fields
that differ from the defaults are named. "—" = header absent.

| # | Method | Host | Path + query | loyal_locale cookie | Accept-Language | User-Agent | Status | Location | Set-Cookie | Cache-Control | Vary | Checked |
|---|--------|------|--------------|--------------------|-----------------|------------|--------|----------|------------|---------------|------|---------|
| 1 | GET | marketing | `/trust` | — | `ru` | browser | 307 | `/ru/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 2 | GET | marketing | `/` | — | `ru` | browser | 307 | `/ru` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 3 | GET | marketing | `/` | — | `zh` | browser | 307 | `/zh` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel (row 2 locale variant) |
| 4 | GET | marketing | `/trust` | `ru` | `ru` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 5 | GET | marketing | `/trust` | `fr` (invalid) | `ru` | browser | 307 | `/ru/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 6 | GET | marketing | `/trust?lang=zh` | — | — | browser | 307 | `/trust` | `loyal_locale=zh; Path=/; Max-Age=31536000; SameSite=lax` | `private, no-store` | — | checked on Vercel |
| 7 | GET | marketing | `/trust?lang=ru&keep=1` | — | — | browser | 307 | `/trust?keep=1` | `loyal_locale=ru; Path=/; Max-Age=31536000; SameSite=lax` | `private, no-store` | — | checked on Vercel |
| 8 | GET | marketing | `/trust?lang=xx` | — | — | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 9 | GET | marketing | `/trust?lang=` (empty) | — | `ru` | browser | 307 | `/ru/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 10 | GET | marketing | `/privacy-policy` | — | `ru` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 11 | GET | marketing | `/blog` | — | `ru` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 12 | GET | marketing | `/500` | — | `ru` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | derived (not in TRANSLATED_PATHS, same as rows 10–11) |
| 13 | HEAD | marketing | `/trust` | — | `ru` | browser | 307 | `/ru/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 14 | POST | marketing | `/trust` | — | `ru` | browser | no redirect (Next answered 405 for a page POST) | — | — | — | — | checked on Vercel |
| 15 | GET | marketing | `/trust` | — | `ru` | `Googlebot/2.1` | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 16 | GET | marketing | `/trust` | — | `zh-TW,zh;q=0.9` | browser | 307 | `/zh/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 17 | GET | marketing | `/trust` | — | `ru;q=0.8, zh;q=0.9` | browser | 307 | `/zh/trust` | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 18 | GET | marketing | `/trust` | — | `ru;q=0.5, zh;q=0.5` | browser | 307 | `/ru/trust` (tie → earlier position wins) | — | `private, no-store` | `Accept-Language, Cookie` | checked on Vercel |
| 19 | GET | marketing | `/trust` | — | `en-US,en;q=0.9` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 20 | GET | marketing | `/trust` | — | — (header absent) | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel |
| 21 | GET | marketing | `/trust` | — | `fr-FR,fr;q=0.9` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel (no supported locale ranked) |
| 22 | GET | marketing | `/ru` | — | `en` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | checked on Vercel (`/ru` itself is not a TRANSLATED_PATH — no redirect back) |
| 23 | GET | marketing | `/icon-dev.svg` | — | `ru` | browser | 200 | — | — | — | — | checked on Vercel (dotted last segment: matcher skips, no redirect) |
| 24 | GET | marketing | `/robots.txt` | — | `ru` | browser | 200 | — | — | — | — | checked on Vercel (matcher skip) |
| 25 | GET | marketing | `/favicon.ico` | — | `ru` | browser | 200 | — | — | — | — | checked on Vercel (matcher skip) |
| 26 | GET | app | `/` | — | `ru` | browser | 200 (rewritten to `/app`, URL unchanged) | — | — | — | — | checked on Vercel |
| 27 | GET | app | `/trust` | — | `ru` | browser | 200 | — | — | — | — | checked on Vercel (locale logic never runs on the app host) |
| 28 | GET | app | `/?lang=ru` | — | — | browser | 200 | — | — (cookie NOT set on app host) | — | — | checked on Vercel |
| 29 | GET | marketing | `/_next/static/chunk.js` | — | `ru` | browser | 200 | — | — | — | — | derived from the matcher (`_next` never reaches the middleware) |
| 30 | GET | marketing | `/earn` | — | `ru` | browser | 307 | `/ru/earn` | — | `private, no-store` | `Accept-Language, Cookie` | derived (same rule as row 1, other translated path) |
| 31 | GET | marketing | `/ru/earn` | — | `en` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | derived (already-localized path is not in TRANSLATED_PATHS) |
| 32 | GET | marketing | `/trust` | — | `ru;q=0` | browser | 200 | — | — | `public, max-age=0, must-revalidate` | Next's page Vary | derived (q ≤ 0 entries are dropped) |
| 33 | GET | marketing | `/trust` | — | `zh-HK,zh;q=0.7` | browser | 307 | `/zh/trust` | — | `private, no-store` | `Accept-Language, Cookie` | derived (primary subtag only; same as row 16) |

Notes for the Go implementation:

- The redirect target keeps the query string minus `lang`; Location is
  path-only (relative), status exactly 307.
- `Cache-Control: private, no-store` on both redirect kinds; only the
  Accept-Language redirect sets `Vary: Accept-Language, Cookie`.
- The cookie has no `Domain` and no `Secure` attribute as served today.
- `?lang=en` behaves like rows 6–7 (stores `en`, redirects to the bare
  URL) — it is how the footer language switch pins English.
- 200 rows show Next's own cache headers on Vercel
  (`public, max-age=0, must-revalidate`); the Go server keeps its own cache
  tiers — what this spec pins is **redirect vs no redirect**, the Location,
  the Set-Cookie and the redirect cache headers, not the 200-page headers.
