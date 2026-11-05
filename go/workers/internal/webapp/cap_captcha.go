package webapp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

const (
	capChallengePath    = "/api/cap/challenge"
	capRedeemPath       = "/api/cap/redeem"
	capScope            = "wallet-signin"
	capNonceKeyPrefix   = "cap-nonce:"
	capTokenKeyPrefix   = "cap-token:"
	capChallengeCount   = 50
	capChallengeSize    = 32
	capDifficulty       = 4
	capMaxCount         = 1000
	capMaxSize          = 256
	capMaxDifficulty    = 16
	capChallengeTTL     = 10 * time.Minute
	capRedeemTokenTTL   = 20 * time.Minute
	capInstrumentKey    = "cap:enc-v1"
	capMinSecretLength  = 16
	capStoreTimeout     = 10 * time.Second
	capChallengeVersion = 2
)

type capChallengeShape struct {
	C int `json:"c"`
	S int `json:"s"`
	D int `json:"d"`
}

type CapChallengeResponse struct {
	Challenge       capChallengeShape `json:"challenge"`
	Token           string            `json:"token"`
	Expires         int64             `json:"expires"`
	Instrumentation string            `json:"instrumentation"`
}

type CapRedeemResponse struct {
	Success bool   `json:"success"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
}

type CapRedeemRequest struct {
	_         struct{}  `additionalProperties:"true"`
	Token     string    `json:"token"`
	Solutions []float64 `json:"solutions"`
}

type capRefusal struct {
	Success bool   `json:"success"`
	Reason  string `json:"error"`
	status  int
}

func (e *capRefusal) Error() string  { return e.Reason }
func (e *capRefusal) GetStatus() int { return e.status }

type capChallengeClaims struct {
	N   string `json:"n"`
	C   int    `json:"c"`
	S   int    `json:"s"`
	D   int    `json:"d"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Sk  string `json:"sk"`
	Ei  string `json:"ei"`
}

type capInstrumentMeta struct {
	ID                     string   `json:"id"`
	ExpectedVals           []int32  `json:"expectedVals"`
	Vars                   []string `json:"vars"`
	BlockAutomatedBrowsers bool     `json:"blockAutomatedBrowsers"`
	Expires                int64    `json:"expires"`
}

type capChallengeOutput struct{ Body CapChallengeResponse }

type capRedeemInput struct{ Raw rawJSON }

type capRedeemOutput struct{ Body CapRedeemResponse }

func (h earnRoutes) registerCap(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-cap-challenge",
		Method:      http.MethodPost,
		Path:        capChallengePath,
		Summary:     "Issue a Cap proof-of-work captcha challenge for wallet sign-in",
		Responses:   errorResponses[routeError](api, 500),
	}, h.capChallenge)
	huma.Register(api, huma.Operation{
		OperationID: "post-cap-redeem",
		Method:      http.MethodPost,
		Path:        capRedeemPath,
		Summary:     "Redeem a solved Cap challenge for a single-use captcha token",
		Responses:   errorResponses[capRefusal](api, 400, 403, 500),
	}, h.capRedeem)
	documentJSONBody[CapRedeemRequest](api, capRedeemPath)
}

func (h earnRoutes) capChallenge(_ context.Context, _ *struct{}) (*capChallengeOutput, error) {
	now := time.Now()
	expires := now.Add(capChallengeTTL).UnixMilli()
	instrumentation, meta, err := generateCapInstrumentation(expires)
	if err != nil {
		return nil, err
	}
	sealedMeta, err := capEncrypt(h.WalletAuth.Cap, meta)
	if err != nil {
		return nil, err
	}
	claims, err := marshalJS(capChallengeClaims{
		N: randomHex(25), C: capChallengeCount, S: capChallengeSize, D: capDifficulty,
		Exp: expires, Iat: now.UnixMilli(), Sk: capScope, Ei: sealedMeta,
	})
	if err != nil {
		return nil, err
	}
	return &capChallengeOutput{Body: CapChallengeResponse{
		Challenge:       capChallengeShape{C: capChallengeCount, S: capChallengeSize, D: capDifficulty},
		Token:           h.WalletAuth.Cap.SealHS256(claims),
		Expires:         expires,
		Instrumentation: instrumentation,
	}}, nil
}

func (h earnRoutes) capRedeem(ctx context.Context, in *capRedeemInput) (*capRedeemOutput, error) {
	var body any
	if json.Unmarshal(in.Raw.data, &body) != nil || !jsTruthy(body) {
		return nil, &capRefusal{Reason: "invalid_body", status: http.StatusBadRequest}
	}
	refuse := func(reason string) error { return &capRefusal{Reason: reason, status: http.StatusForbidden} }
	reason, nonceKey, nonceExpires := h.validateCapChallenge(body, time.Now())
	if reason != "" {
		return nil, refuse(reason)
	}
	claimed, err := h.putCaptchaKey(ctx, nonceKey, nonceExpires)
	if err != nil {
		return nil, refuse("nonce_store_error")
	}
	if !claimed {
		return nil, refuse("already_redeemed")
	}
	id, secret := randomHex(8), randomHex(15)
	expires := time.Now().Add(capRedeemTokenTTL)
	if _, err := h.putCaptchaKey(ctx, capTokenKeyPrefix+id+":"+sha256Hex(secret), expires); err != nil {
		return nil, fmt.Errorf("store cap redeem token: %w", err)
	}
	return &capRedeemOutput{Body: CapRedeemResponse{Success: true, Token: id + ":" + secret, Expires: expires.UnixMilli()}}, nil
}

func (h earnRoutes) validateCapChallenge(body any, now time.Time) (reason, nonceKey string, nonceExpires time.Time) {
	object, isObject := body.(map[string]any)
	if _, isArray := body.([]any); !isObject && !isArray {
		return "invalid_body", "", time.Time{}
	}
	token, _ := object["token"].(string)
	if token == "" {
		return "missing_token", "", time.Time{}
	}
	solutions, ok := object["solutions"].([]any)
	if !ok {
		return "missing_solutions", "", time.Time{}
	}
	raw, ok := h.WalletAuth.Cap.OpenHS256(token)
	if !ok {
		return "invalid_token", "", time.Time{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return "invalid_token", "", time.Time{}
	}
	if claims["f"] == float64(capChallengeVersion) {
		return "invalid_token", "", time.Time{}
	}
	if claims["sk"] != capScope {
		return "scope_mismatch", "", time.Time{}
	}
	exp, _ := claims["exp"].(float64)
	nowMs := float64(now.UnixMilli())
	if exp == 0 || exp < nowMs {
		return "expired", "", time.Time{}
	}
	count, okCount := jsInteger(claims["c"], 1, capMaxCount)
	size, okSize := jsInteger(claims["s"], 1, capMaxSize)
	difficulty, okDifficulty := jsInteger(claims["d"], 1, capMaxDifficulty)
	if !okCount || !okSize || !okDifficulty {
		return "invalid_token", "", time.Time{}
	}
	if len(solutions) != count {
		return "invalid_solutions", "", time.Time{}
	}
	for _, solution := range solutions {
		if _, ok := solution.(float64); !ok {
			return "invalid_solutions", "", time.Time{}
		}
	}
	tokenHash := fnv1a(2166136261, token)
	for i, solution := range solutions {
		saltSeed := fnv1a(tokenHash, strconv.Itoa(i+1))
		salt := capPRNG(saltSeed, size)
		target := capPRNG(fnv1a(saltSeed, "d"), difficulty)
		digest := sha256.Sum256([]byte(salt + jsNumberString(solution.(float64))))
		if !strings.HasPrefix(hex.EncodeToString(digest[:]), target) {
			return "invalid_solution", "", time.Time{}
		}
	}
	if sealed, ok := claims["ei"].(string); ok && sealed != "" {
		if reason := verifyCapInstrumentation(h.WalletAuth.Cap, sealed, object, nowMs); reason != "" {
			return reason, "", time.Time{}
		}
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token[strings.LastIndexByte(token, '.')+1:], "="))
	if err != nil {
		return "invalid_token", "", time.Time{}
	}
	ttl := max(1, exp-nowMs)
	return "", capNonceKeyPrefix + hex.EncodeToString(signature), now.Add(time.Duration(ttl) * time.Millisecond)
}

func verifyCapInstrumentation(key *jwt.HS256, sealed string, body map[string]any, nowMs float64) string {
	var meta capInstrumentMeta
	if !capDecrypt(key, sealed, &meta) {
		return "instr_corrupted"
	}
	if meta.Expires != 0 && nowMs > float64(meta.Expires) {
		return "instr_expired"
	}
	switch {
	case body["instr_blocked"] == true:
		if meta.BlockAutomatedBrowsers {
			return "instr_automated_browser"
		}
		return ""
	case body["instr_timeout"] == true:
		return "instr_timeout"
	case jsTruthy(body["instr"]):
		return capInstrumentationResult(meta, body["instr"])
	default:
		return "instr_missing"
	}
}

func capInstrumentationResult(meta capInstrumentMeta, output any) string {
	object, isObject := output.(map[string]any)
	if _, isArray := output.([]any); !isObject && !isArray {
		return "missing_output"
	}
	if id, ok := object["i"].(string); !ok || id != meta.ID {
		return "id_mismatch"
	}
	state, isState := object["state"].(map[string]any)
	if _, isArray := object["state"].([]any); !isState && !isArray {
		return "invalid_state"
	}
	if meta.Vars == nil || meta.ExpectedVals == nil {
		return "invalid_meta"
	}
	for i, name := range meta.Vars {
		value, ok := state[name].(float64)
		if !ok || i >= len(meta.ExpectedVals) || value != float64(meta.ExpectedVals[i]) {
			return "failed_challenge"
		}
	}
	return ""
}

func (h earnRoutes) verifyCaptchaToken(ctx context.Context, token string) bool {
	id, secret, _ := strings.Cut(token, ":")
	secret, _, _ = strings.Cut(secret, ":")
	if id == "" || secret == "" {
		return false
	}
	consumed, err := h.consumeCaptchaKey(ctx, capTokenKeyPrefix+id+":"+sha256Hex(secret))
	return err == nil && consumed
}

func (h earnRoutes) putCaptchaKey(ctx context.Context, key string, expiresAt time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, capStoreTimeout)
	defer cancel()
	tx, err := h.appDB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM app_captcha_keys WHERE expires_at < $1`, jsTime(time.Now())); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO app_captcha_keys (key, expires_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, key, jsTime(expiresAt))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (h earnRoutes) consumeCaptchaKey(ctx context.Context, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, capStoreTimeout)
	defer cancel()
	tag, err := h.appDB.Exec(ctx, `DELETE FROM app_captcha_keys WHERE key = $1 AND expires_at > $2`, key, jsTime(time.Now()))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func capEncrypt(key *jwt.HS256, data any) (string, error) {
	plaintext, err := marshalJS(data)
	if err != nil {
		return "", err
	}
	aead, err := capAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)
	ciphertext, tag := sealed[:len(sealed)-aead.Overhead()], sealed[len(sealed)-aead.Overhead():]
	return base64.RawURLEncoding.EncodeToString(slicesConcat(nonce, tag, ciphertext)), nil
}

func capDecrypt(key *jwt.HS256, blob string, into any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(blob, "="))
	if err != nil || len(raw) < 28 {
		return false
	}
	aead, err := capAEAD(key)
	if err != nil {
		return false
	}
	plaintext, err := aead.Open(nil, raw[:12], slicesConcat(raw[28:], raw[12:28]), nil)
	return err == nil && json.Unmarshal(plaintext, into) == nil
}

func capAEAD(key *jwt.HS256) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key.DeriveHS256(capInstrumentKey))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func slicesConcat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func fnv1a(state uint32, text string) uint32 {
	for i := 0; i < len(text); i++ {
		state ^= uint32(text[i])
		state += state<<1 + state<<4 + state<<7 + state<<8 + state<<24
	}
	return state
}

func capPRNG(state uint32, length int) string {
	var out strings.Builder
	for out.Len() < length {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		fmt.Fprintf(&out, "%08x", state)
	}
	return out.String()[:length]
}

func jsInteger(value any, low, high int) (int, bool) {
	number, ok := value.(float64)
	if !ok || number != math.Trunc(number) || number < float64(low) || number > float64(high) {
		return 0, false
	}
	return int(number), true
}

func jsNumberString(n float64) string {
	if n == 0 {
		return "0"
	}
	if abs := math.Abs(n); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(n, 'e', -1, 64), "e")
	sign, digits := exponent[:1], strings.TrimLeft(exponent[1:], "0")
	return mantissa + "e" + sign + digits
}

func jsTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	default:
		return true
	}
}

func jsTime(t time.Time) time.Time { return t.Truncate(time.Millisecond) }

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func marshalJS(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
