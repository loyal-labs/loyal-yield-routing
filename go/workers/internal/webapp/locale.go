package webapp

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	localeCookie = "loyal_locale"
	localeParam  = "lang"
)

var locales = map[string]bool{"en": true, "ru": true, "zh": true}

var localePrefix = map[string]string{"ru": "/ru", "zh": "/zh"}

const localeCookieMaxAge = 60 * 60 * 24 * 365

var botUserAgent = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|preview|facebookexternalhit|embedly|whatsapp|telegram`)

func localeRedirect(w http.ResponseWriter, r *http.Request, translated map[string]bool) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	requested := r.URL.Query().Get(localeParam)
	if locales[requested] {
		target := *r.URL
		target.RawQuery = withoutParam(target.RawQuery, localeParam)
		http.SetCookie(w, &http.Cookie{
			Name:     localeCookie,
			Value:    requested,
			Path:     "/",
			MaxAge:   localeCookieMaxAge,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Cache-Control", "private, no-store")
		redirect(w, r, target.RequestURI(), http.StatusTemporaryRedirect)
		return true
	}

	if !translated[r.URL.Path] {
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
	redirect(w, r, localizedPath(preferred, r.URL), http.StatusTemporaryRedirect)
	return true
}

func translatedPaths(pages []string) map[string]bool {
	listed := make(map[string]bool, len(pages))
	for _, page := range pages {
		listed[page] = true
	}
	translated := map[string]bool{}
	for _, page := range pages {
		suffix := strings.TrimSuffix(page, "/")
		translated[page] = true
		for _, prefix := range localePrefix {
			translated[page] = translated[page] && listed[prefix+suffix]
		}
	}
	return translated
}

func withoutParam(rawQuery, name string) string {
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		key, keyErr := url.QueryUnescape(rawKey)
		if keyErr != nil {
			kept = append(kept, pair)
			continue
		}
		if key == name {
			continue
		}
		value, valueErr := url.QueryUnescape(rawValue)
		if valueErr != nil {
			kept = append(kept, pair)
			continue
		}
		kept = append(kept, encodeComponent(key)+"="+encodeComponent(value))
	}
	return strings.Join(kept, "&")
}

func encodeComponent(text string) string {
	var out strings.Builder
	for _, b := range []byte(text) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', strings.IndexByte("-_.!~*'()", b) >= 0:
			out.WriteByte(b)
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

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
