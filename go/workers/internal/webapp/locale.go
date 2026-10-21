package webapp

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Locale handling for the marketing site, ported from apps/web's middleware
// (web/apps/web/src/middleware.ts) and
// web/apps/web/src/features/marketing/i18n/locale-redirect.ts:
//
//  1. ?lang=<locale> (added by the language switch) stores the choice in a
//     cookie and 307s to the same URL without the parameter.
//  2. A visitor without a stored choice whose browser prefers a translated
//     language (Russian, Chinese) is sent from an English translated page to
//     its localized twin. Crawlers are left alone so they index every
//     version through hreflang.
//
// The middleware runs only on site hosts (middleware.ts:10); the app host
// never locale-redirects. Its matcher (middleware.ts:25) excludes any path
// containing a dot, so static files skip this entirely.

// localeCookie and localeParam are LOCALE_COOKIE and LOCALE_PARAM of
// locale.ts:5,7.
const (
	localeCookie = "loyal_locale"
	localeParam  = "lang"
)

// locales is LOCALES of locale.ts:1.
var locales = map[string]bool{"en": true, "ru": true, "zh": true}

// translatedPaths is TRANSLATED_PATHS of locale.ts:10-16: the marketing
// paths that exist in every locale.
var translatedPaths = map[string]bool{
	"/":       true,
	"/earn":   true,
	"/agents": true,
	"/trust":  true,
	"/risks":  true,
}

// localePrefix is LOCALE_PREFIX of locale.ts:19 for the non-English locales;
// English paths have no prefix.
var localePrefix = map[string]string{"ru": "/ru", "zh": "/zh"}

// localeCookieMaxAge is LOCALE_COOKIE_MAX_AGE of locale-redirect.ts:12.
const localeCookieMaxAge = 60 * 60 * 24 * 365

// botUserAgent is BOT_USER_AGENT_RE of locale-redirect.ts:13-14.
var botUserAgent = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|preview|facebookexternalhit|embedly|whatsapp|telegram`)

func localeRedirect(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	requested := r.URL.Query().Get(localeParam)
	if locales[requested] {
		target := *r.URL
		query := target.Query()
		query.Del(localeParam)
		target.RawQuery = query.Encode()
		http.SetCookie(w, &http.Cookie{
			Name:     localeCookie,
			Value:    requested,
			Path:     "/",
			MaxAge:   localeCookieMaxAge,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, target.RequestURI(), http.StatusTemporaryRedirect)
		return true
	}

	if !translatedPaths[r.URL.Path] {
		return false
	}
	if locales[cookieValue(r, localeCookie)] {
		return false
	}
	if botUserAgent.MatchString(r.Header.Get("User-Agent")) {
		return false
	}
	preferred := preferredLocale(r.Header.Get("Accept-Language"))
	if preferred == "" || preferred == "en" {
		return false
	}

	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Accept-Language, Cookie")
	http.Redirect(w, r, localizedPath(preferred, r.URL), http.StatusTemporaryRedirect)
	return true
}

// localizedPath is localizedHref (locale.ts:41-53) for a request URL: the
// locale's prefix before the path ("/" loses its slash), query kept.
func localizedPath(locale string, u *url.URL) string {
	target := localePrefix[locale]
	if u.Path != "/" {
		target += u.Path
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target
}

// preferredLocale is the supported locale the browser ranks highest in
// Accept-Language, or "" when it ranks none of them. "ru-RU,ru;q=0.9,en;q=0.8"
// → "ru"; "zh-CN,zh;q=0.9" → "zh"; "en-US,ru;q=0.5" → "en". Only the primary
// subtag counts, so zh-TW and zh-HK readers are offered the Simplified
// Chinese pages too (locale-redirect.ts:31-53).
func preferredLocale(acceptLanguage string) string {
	type tag struct {
		index    int
		language string
		quality  float64
	}
	var ranked []tag
	for index, part := range strings.Split(acceptLanguage, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		language, _, _ := strings.Cut(strings.ToLower(segments[0]), "-")
		quality := 1.0
		for _, param := range segments[1:] {
			if value, ok := strings.CutPrefix(strings.TrimSpace(param), "q="); ok {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = parsed
				} else {
					quality = 0
				}
				break
			}
		}
		ranked = append(ranked, tag{index: index, language: language, quality: quality})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].quality != ranked[j].quality {
			return ranked[i].quality > ranked[j].quality
		}
		return ranked[i].index < ranked[j].index
	})
	for _, t := range ranked {
		if t.quality > 0 && locales[t.language] {
			return t.language
		}
	}
	return ""
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}
