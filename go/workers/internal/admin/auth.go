package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

const (
	SessionCookie     = "loyal_admin_session"
	SessionTTLSeconds = 60 * 60 * 24 * 3
)

type Session struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

type Auth struct {
	User     string
	Password string
	Now      func() time.Time
}

func (a Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a Auth) ValidCredentials(user, password string) bool {
	if a.User == "" || a.Password == "" {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.User))
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.Password))
	return userOK&passwordOK == 1
}

func (a Auth) NewSession(user string) Session {
	issued := a.now().Unix()
	return Session{Sub: user, Iat: issued, Exp: issued + SessionTTLSeconds}
}

func (a Auth) sign(session Session) string {
	unsigned := segment(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + segment(session)
	mac := hmac.New(sha256.New, []byte(a.Password))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func segment(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func (a Auth) Verify(token string) (Session, bool) {
	if token == "" || a.Password == "" {
		return Session{}, false
	}
	claims, err := jwt.Keys{HS256: []byte(a.Password)}.Verify(token, jwt.NoAudience, a.now())
	if err != nil {
		return Session{}, false
	}
	var payload struct {
		Sub *string  `json:"sub"`
		Iat *float64 `json:"iat"`
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(claims, &payload) != nil || payload.Sub == nil || payload.Iat == nil || payload.Exp == nil {
		return Session{}, false
	}
	return Session{Sub: *payload.Sub, Iat: int64(*payload.Iat), Exp: int64(*payload.Exp)}, true
}

func (a Auth) session(r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return Session{}, false
	}
	return a.Verify(cookie.Value)
}

func (a Auth) setCookie(w http.ResponseWriter, user string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: a.sign(a.NewSession(user)), Path: "/",
		MaxAge: SessionTTLSeconds, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func SafeNextPath(value string, base *url.URL) (string, bool) {
	if !strings.HasPrefix(value, "/") || base == nil {
		return "", false
	}
	value = strings.NewReplacer("\t", "", "\n", "", "\r", "", `\`, "/").Replace(value)
	ref, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	target := base.ResolveReference(ref)
	if target.Scheme != base.Scheme || target.Host != base.Host {
		return "", false
	}
	next := target.EscapedPath()
	if strings.HasPrefix(next, "//") {
		return "", false
	}
	if target.RawQuery != "" || target.ForceQuery {
		next += "?" + target.RawQuery
	}
	if target.Fragment != "" {
		next += "#" + target.EscapedFragment()
	}
	return next, true
}

func requestBase(r *http.Request) *url.URL {
	return &url.URL{Scheme: "https", Host: r.Host}
}

func (a Auth) postLoginPath(value string, r *http.Request) string {
	next, ok := SafeNextPath(value, requestBase(r))
	if !ok || next == "/" {
		return landingPath
	}
	return next
}

func loginRedirect(nextPath string, invalid bool) string {
	query := url.Values{}
	if invalid {
		query.Set("error", "invalid")
	}
	if nextPath != "" && nextPath != landingPath {
		query.Set("next", nextPath)
	}
	if len(query) == 0 {
		return "/login"
	}
	return "/login?" + query.Encode()
}

func (a Auth) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil && err != http.ErrNotMultipart {
		http.Redirect(w, r, loginRedirect(landingPath, true), http.StatusSeeOther)
		return
	}
	nextPath := landingPath
	if values, ok := r.PostForm["next"]; ok && len(values) > 0 {
		nextPath = a.postLoginPath(values[0], r)
	}
	login, hasLogin := r.PostForm["login"]
	password, hasPassword := r.PostForm["password"]
	if !hasLogin || !hasPassword || !a.ValidCredentials(login[0], password[0]) {
		http.Redirect(w, r, loginRedirect(nextPath, true), http.StatusSeeOther)
		return
	}
	a.setCookie(w, login[0])
	http.Redirect(w, r, nextPath, http.StatusSeeOther)
}

func Logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

var publicPaths = map[string]bool{"/login": true, "/auth/login": true, "/logout": true, "/favicon.ico": true, "/icon.svg": true}

func publicAsset(path string) bool {
	if strings.HasPrefix(path, "/api/") {
		return false
	}
	last := path[strings.LastIndex(path, "/")+1:]
	return strings.HasPrefix(path, "/_next/") || strings.Contains(last, ".") && !strings.HasSuffix(last, ".")
}

func (a Auth) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if publicAsset(path) {
			next.ServeHTTP(w, r)
			return
		}
		session, ok := a.session(r)
		if path == "/login" {
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			destination, safe := SafeNextPath(r.URL.Query().Get("next"), requestBase(r))
			if !safe || destination == "/" || destination == "/login" {
				destination = landingPath
			}
			a.setCookie(w, session.Sub)
			http.Redirect(w, r, destination, http.StatusFound)
			return
		}
		if publicPaths[path] {
			next.ServeHTTP(w, r)
			return
		}
		if ok {
			a.setCookie(w, session.Sub)
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		target := "/login"
		if nextPath, safe := SafeNextPath(r.URL.RequestURI(), requestBase(r)); safe {
			target += "?" + url.Values{"next": {nextPath}}.Encode()
		}
		status := http.StatusSeeOther
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			status = http.StatusFound
		}
		http.Redirect(w, r, target, status)
	})
}
