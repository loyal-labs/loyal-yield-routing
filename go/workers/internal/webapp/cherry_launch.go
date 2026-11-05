package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
)

// POST /api/cherry/launch ports web/apps/web/src/app/api/cherry/launch/route.ts
// (launch-attestation.ts, launch-token.ts, config.ts) and the one SDK call it
// makes, verifyLaunchToken (cherry_jwt.go). It is served only where
// CHERRY_MINIAPP_ID and CHERRY_MINIAPP_ORIGIN are set; without them the
// request keeps forwarding to Next, which would answer 503.
const (
	cherryLaunchPath          = "/api/cherry/launch"
	cherryDefaultIssuer       = "https://chat.cherry.fun"
	cherryDefaultJWKSURL      = "https://chat.cherry.fun/.well-known/jwks.json"
	cherryMaxFutureSkew       = 60
	cherryMaxLifetime         = 5 * 60
	cherryEmbedContextCookie  = "__Host-loyal_cherry_embed=1; Path=/; Secure; HttpOnly; SameSite=None; Partitioned"
	cherryErrNotVerifiedText  = "Cherry launch could not be verified."
	cherryErrRequestOriginMsg = "Cherry launch requests must come from the same origin."
)

type cherryConfig struct {
	appID    string
	origin   string
	issuer   string
	jwksURL  string
	verifier *cherryJWKS
}

type cherryFailure struct {
	code   string
	status int
}

func (f *cherryFailure) Error() string { return f.code }

type cherryLaunchInput struct {
	Origin         string `header:"Origin"`
	ForwardedProto string `header:"X-Forwarded-Proto"`
	Raw            rawJSON
	host           string
}

func (i *cherryLaunchInput) Resolve(ctx huma.Context) []error {
	i.host = ctx.Host()
	return nil
}

type cherryLaunchOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	ContentType  string `header:"Content-Type"`
	SetCookie    string `header:"Set-Cookie"`
	Body         []byte
}

type cherryLaunchResponse struct {
	WalletAddress string `json:"walletAddress"`
	RoomID        string `json:"roomId"`
	IssuedAt      int64  `json:"issuedAt"`
	ExpiresAt     int64  `json:"expiresAt"`
}

type cherryLaunchErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func cherryLaunchError(code, message string, status int) *cherryLaunchOutput {
	var body cherryLaunchErrorBody
	body.Error.Code, body.Error.Message = code, message
	return cherryLaunchJSON(status, body, "")
}

func cherryLaunchJSON(status int, body any, cookie string) *cherryLaunchOutput {
	encoded, _ := obsEncodeJSON(body)
	return &cherryLaunchOutput{Status: status, CacheControl: "no-store", ContentType: "application/json", SetCookie: cookie, Body: encoded}
}

func registerCherryLaunch(api huma.API) {
	appID := strings.TrimSpace(os.Getenv("CHERRY_MINIAPP_ID"))
	origin := strings.TrimSpace(os.Getenv("CHERRY_MINIAPP_ORIGIN"))
	if appID == "" || origin == "" {
		return
	}
	config, configErr := newCherryConfig(appID, origin)
	huma.Register(api, huma.Operation{
		OperationID: "post-cherry-launch",
		Method:      http.MethodPost,
		Path:        cherryLaunchPath,
		Summary:     "Verify a Cherry MiniApp launch token",
	}, func(ctx context.Context, in *cherryLaunchInput) (*cherryLaunchOutput, error) {
		return config.attest(ctx, configErr, in, time.Now()), nil
	})
	api.OpenAPI().Paths[cherryLaunchPath].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/json": {Schema: &huma.Schema{Type: "object"}},
	}}
}

func (c *cherryConfig) attest(ctx context.Context, configErr error, in *cherryLaunchInput, now time.Time) *cherryLaunchOutput {
	if requestOrigin := obsRequestOrigin(in.host, in.ForwardedProto); requestOrigin == "" || in.Origin != requestOrigin {
		return cherryLaunchError("invalid_request_origin", cherryErrRequestOriginMsg, http.StatusForbidden)
	}
	var body map[string]any
	if json.Unmarshal(in.Raw.data, &body) != nil {
		body = nil
	}
	token, tokenOK := body["launchToken"].(string)
	platform, _ := body["platform"].(string)
	if !tokenOK || (platform != "webview" && platform != "iframe") {
		return cherryLaunchError("invalid_cherry_launch_request", "A Cherry launch token is required.", http.StatusBadRequest)
	}
	unavailable := cherryLaunchError("cherry_launch_unavailable", cherryErrNotVerifiedText, http.StatusServiceUnavailable)
	if configErr != nil {
		return unavailable
	}
	launch, err := c.verify(ctx, token, now)
	var failure *cherryFailure
	switch {
	case errors.As(err, &failure):
		return cherryLaunchError(failure.code, cherryErrNotVerifiedText, failure.status)
	case err != nil:
		return unavailable
	}
	cookie := ""
	if platform == "iframe" {
		cookie = cherryEmbedContextCookie
	}
	return cherryLaunchJSON(http.StatusOK, launch, cookie)
}

func (c *cherryConfig) verify(ctx context.Context, token string, now time.Time) (cherryLaunchResponse, error) {
	token = jsTrim(token)
	if token == "" {
		return cherryLaunchResponse{}, &cherryFailure{"invalid_cherry_launch_token", http.StatusBadRequest}
	}
	claims, err := c.verifier.verify(ctx, token, c.appID, c.origin, now)
	if err != nil {
		slog.Debug("cherry launch token refused", "error", err)
		return cherryLaunchResponse{}, &cherryFailure{"invalid_cherry_launch_token", http.StatusUnauthorized}
	}
	if issuer, _ := claims["iss"].(string); issuer != c.issuer {
		return cherryLaunchResponse{}, &cherryFailure{"invalid_cherry_issuer", http.StatusUnauthorized}
	}
	subject, _ := claims["sub"].(string)
	wallet, err := solana.PublicKeyFromBase58(subject)
	if err != nil {
		return cherryLaunchResponse{}, &cherryFailure{"invalid_cherry_wallet", http.StatusUnauthorized}
	}
	roomID, err := jsTrimmedClaim(claims, "room_id")
	if err != nil {
		return cherryLaunchResponse{}, err
	}
	tokenID, err := jsTrimmedClaim(claims, "jti")
	if err != nil {
		return cherryLaunchResponse{}, err
	}
	issuedAt, issuedOK := jsInteger(claims["iat"])
	expiresAt, expiresOK := jsInteger(claims["exp"])
	current := now.Unix()
	if roomID == "" || tokenID == "" || !issuedOK || !expiresOK ||
		issuedAt <= 0 ||
		expiresAt <= issuedAt ||
		expiresAt-issuedAt > cherryMaxLifetime ||
		issuedAt > current+cherryMaxFutureSkew ||
		expiresAt <= current {
		return cherryLaunchResponse{}, &cherryFailure{"invalid_cherry_context", http.StatusUnauthorized}
	}
	return cherryLaunchResponse{WalletAddress: wallet.String(), RoomID: roomID, IssuedAt: issuedAt, ExpiresAt: expiresAt}, nil
}

// jsTrimmedClaim is payload.claim?.trim(): a string trims, an absent or null
// claim is empty, and any other type is the TypeError Next surfaces as 503.
func jsTrimmedClaim(claims map[string]any, name string) (string, error) {
	switch v := claims[name].(type) {
	case nil:
		return "", nil
	case string:
		return jsTrim(v), nil
	}
	return "", fmt.Errorf("claim %s is not a string", name)
}

func jsInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := number.Float64()
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

func newCherryConfig(appID, rawOrigin string) (*cherryConfig, error) {
	origin, err := cherryLaunchURL(rawOrigin, "CHERRY_MINIAPP_ORIGIN")
	if err != nil {
		return nil, err
	}
	rawIssuer := cherryDefaultIssuer
	if value, set := os.LookupEnv("CHERRY_MINIAPP_ISSUER"); set {
		rawIssuer = strings.TrimSpace(value)
	}
	issuer, _, err := cherryNormalizedURL(rawIssuer, "CHERRY_MINIAPP_ISSUER")
	if err != nil {
		return nil, err
	}
	rawJWKS := cherryDefaultJWKSURL
	if value, set := os.LookupEnv("CHERRY_MINIAPP_JWKS_URL"); set {
		rawJWKS = strings.TrimSpace(value)
	}
	jwksURL, err := cherryEndpointURL(rawJWKS, "CHERRY_MINIAPP_JWKS_URL")
	if err != nil {
		return nil, err
	}
	return &cherryConfig{appID: appID, origin: origin, issuer: issuer, jwksURL: jwksURL, verifier: newCherryJWKS(jwksURL)}, nil
}

func cherryNormalizedURL(value, name string) (string, *url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil, fmt.Errorf("%s must be an absolute URL", name)
	}
	if u.User != nil {
		return "", nil, fmt.Errorf("%s must not contain URL credentials", name)
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if u.Scheme != "https" && !(local && u.Scheme == "http") {
		return "", nil, fmt.Errorf("%s must use HTTPS outside local development", name)
	}
	return obsURLOrigin(u), u, nil
}

func cherryURLPath(u *url.URL) string {
	if path := removeDotSegments(u.EscapedPath()); path != "" {
		return path
	}
	return "/"
}

func cherryEndpointURL(value, name string) (string, error) {
	origin, u, err := cherryNormalizedURL(value, name)
	if err != nil {
		return "", err
	}
	if u.Fragment != "" || strings.Contains(value, "#") {
		return "", fmt.Errorf("%s must not contain a URL fragment", name)
	}
	query := ""
	if u.RawQuery != "" {
		query = "?" + u.RawQuery
	}
	return origin + cherryURLPath(u) + query, nil
}

func cherryLaunchURL(value, name string) (string, error) {
	origin, u, err := cherryNormalizedURL(value, name)
	if err != nil {
		return "", err
	}
	if u.RawQuery != "" || strings.ContainsAny(value, "?#") {
		return "", fmt.Errorf("%s must not contain a query or fragment", name)
	}
	if path := cherryURLPath(u); path != "/" {
		origin += path
	}
	return origin, nil
}
