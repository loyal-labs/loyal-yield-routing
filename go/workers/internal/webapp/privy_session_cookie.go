package webapp

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Privy sign-in, part 2 of 3: the session cookie the minted JWT lands in.
// Ports web/apps/web/src/features/identity/server/session-cookie.ts
// resolveCookieOptions (:109-176) and getSessionCookieName (:291-295) — the
// same name rules, domain rules and cookie attributes the Next route sets —
// plus the AUTH_JWT_TTL_SECONDS default of one week (:154-157).

var (
	privyIPv4Hosts = [...]*regexp.Regexp{
		regexp.MustCompile(`^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$`),
		regexp.MustCompile(`^10\.\d{1,3}\.\d{1,3}\.\d{1,3}$`),
		regexp.MustCompile(`^192\.168\.\d{1,3}\.\d{1,3}$`),
		regexp.MustCompile(`^172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}$`),
	}
)

func privySessionTTL() (int, error) {
	raw := strings.TrimSpace(os.Getenv("AUTH_JWT_TTL_SECONDS"))
	if raw == "" {
		return 60 * 60 * 24 * 7, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("AUTH_JWT_TTL_SECONDS must be a positive integer")
	}
	return seconds, nil
}

func privySessionTTLDuration() (time.Duration, error) {
	seconds, err := privySessionTTL()
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds) * time.Second, nil
}

func privyCookieAllowLocalhost() bool {
	raw := os.Getenv("AUTH_COOKIE_ALLOW_LOCALHOST")
	if raw == "" {
		return true
	}
	return raw == "true"
}

func ParseCookieParentDomains(raw string) []string {
	if raw == "" {
		return nil
	}
	seen := map[string]bool{}
	var domains []string
	for _, entry := range strings.Split(raw, ",") {
		normalized := privyNormalizeHostname(entry)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		domains = append(domains, normalized)
	}
	return domains
}

func privyCookiePreviewFallback() bool {
	return os.Getenv("VERCEL_ENV") == "preview"
}

func privyCookiePrimaryValue(value string) string {
	primary := strings.TrimSpace(strings.Split(value, ",")[0])
	return primary
}

func privyNormalizeHostname(hostname string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
}

func privyLocalDevHostname(hostname string) bool {
	switch {
	case hostname == "localhost",
		strings.HasSuffix(hostname, ".localhost"),
		hostname == "::1",
		hostname == "[::1]":
		return true
	}
	for _, pattern := range privyIPv4Hosts {
		if pattern.MatchString(hostname) {
			return true
		}
	}
	return false
}

func privySessionCookieName(cookieHeader, forwardedHost, host string) string {
	if headerCookie(cookieHeader, cherryContextCookie) == "1" {
		return cherrySessionCookie
	}
	primary := privyCookiePrimaryValue(forwardedHost)
	if primary == "" {
		primary = host
	}
	hostname, _, err := net.SplitHostPort(primary)
	if err != nil {
		hostname = primary
	}
	if previewHost.MatchString(hostname) {
		return previewSessionCookie
	}
	return walletSessionCookie
}

func privyMatchParentDomain(domains []string, hostname string) (string, bool) {
	for _, domain := range domains {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			return domain, true
		}
	}
	return "", false
}

func privySessionCookieHeader(domains []string, name, token string, ttl int, forwardedProto, forwardedHost, host, scheme string) (string, error) {
	proto := privyCookiePrimaryValue(forwardedProto)
	if proto == "" {
		proto = scheme
	}
	hostHeader := privyCookiePrimaryValue(forwardedHost)
	if hostHeader == "" {
		hostHeader = host
	}
	hostname, _, err := net.SplitHostPort(hostHeader)
	if err != nil {
		hostname = hostHeader
	}
	hostname = privyNormalizeHostname(hostname)

	set := func(cookie http.Cookie) string {
		cookie.Name = name
		cookie.Value = token
		cookie.Path = "/"
		cookie.MaxAge = ttl
		return cookie.String()
	}

	switch {
	case name == cherrySessionCookie:
		// createCherryPartitionedSessionCookieOptions: the __Host- name
		// already pins the path and forbids a domain attribute.
		return set(http.Cookie{
			HttpOnly:    true,
			Secure:      true,
			SameSite:    http.SameSiteNoneMode,
			Partitioned: true,
		}), nil
	case privyLocalDevHostname(hostname):
		if !privyCookieAllowLocalhost() {
			return "", fmt.Errorf("Local development hosts are not allowed for auth session cookies")
		}
		return set(http.Cookie{
			HttpOnly: true,
			Secure:   false,
			SameSite: http.SameSiteLaxMode,
		}), nil
	default:
		domain, matched := privyMatchParentDomain(domains, hostname)
		if !matched {
			if privyCookiePreviewFallback() {
				return set(http.Cookie{
					HttpOnly: true,
					Secure:   true,
					SameSite: http.SameSiteLaxMode,
				}), nil
			}
			if len(domains) == 0 {
				return "", fmt.Errorf("AUTH_COOKIE_PARENT_DOMAIN is not set")
			}
			return "", fmt.Errorf("Host %q is not allowed for auth session cookies", hostname)
		}
		return set(http.Cookie{
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Domain:   domain,
		}), nil
	}
}
