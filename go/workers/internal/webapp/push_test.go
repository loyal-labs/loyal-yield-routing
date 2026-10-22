package webapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

func TestPushTokenSavedForSessionWalletWithoutSmartAccount(t *testing.T) {
	dsn := os.Getenv("LOYAL_WEB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local Postgres cluster")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/tmp/loyal-web-pg-") || config.ConnConfig.Database != "loyal_web_pilot" {
		t.Fatal("test requires the loyal_web_pilot database on a /tmp/loyal-web-pg-* Unix socket")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS push_tokens, app_user_smart_accounts, app_users;
CREATE TABLE app_users (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), provider text NOT NULL, subject_address text NOT NULL);
CREATE TABLE app_user_smart_accounts (user_id uuid NOT NULL, solana_env text NOT NULL, state text NOT NULL, settings_pda text NOT NULL);
CREATE TABLE push_tokens (token text PRIMARY KEY, telegram_user_id bigint, wallet_public_key text, platform text NOT NULL, updated_at timestamptz NOT NULL DEFAULT now());`); err != nil {
		t.Fatal(err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := jwt.NewKeys("", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(claims map[string]any) string {
		body, _ := json.Marshal(claims)
		unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(body)
		digest := sha256.Sum256([]byte(unsigned))
		signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	const wallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	exp := time.Now().Add(time.Hour).Unix()
	cookie := sign(map[string]any{"authMethod": "wallet", "subjectAddress": wallet, "displayAddress": wallet, "provider": "solana", "walletAddress": wallet, "exp": exp})
	bearer := sign(map[string]any{"sub": wallet, "aud": string(mobileEarnAudience), "exp": exp})

	r := chi.NewRouter()
	registerPush(humachi.New(r, apiConfig()), &Auth{keys: keys, appDB: pool}, pool)
	for _, tc := range []struct {
		name, header, value, token string
		status                     int
		body                       string
	}{
		{"cookie session", "Cookie", walletSessionCookie + "=" + cookie, "cookie-token", 200, `{"success":true}`},
		{"bearer session", "Authorization", "Bearer " + bearer, "bearer-token", 200, `{"success":true}`},
		{"no credentials", "", "", "anon-token", 401, `{"error":{"code":"unauthenticated","message":"No active auth session."}}`},
		{"tampered cookie", "Cookie", walletSessionCookie + "=" + cookie[:len(cookie)-4] + "AAAA", "tampered-token", 401, `{"error":{"code":"unauthenticated","message":"No active auth session."}}`},
		{"bearer sent as cookie", "Cookie", walletSessionCookie + "=" + bearer, "swap-cookie-token", 401, `{"error":{"code":"unauthenticated","message":"No active auth session."}}`},
		{"cookie sent as bearer", "Authorization", "Bearer " + cookie, "swap-bearer-token", 401, `{"error":{"code":"invalid_mobile_session","message":"Mobile Earn session is invalid or expired."}}`},
	} {
		req := httptest.NewRequest(http.MethodPost, pushPath, strings.NewReader(`{"token":"`+tc.token+`","platform":"ios"}`))
		req.Header.Set("Content-Type", "application/json")
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if got := strings.TrimSpace(w.Body.String()); w.Code != tc.status || got != tc.body {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, w.Code, got, tc.status, tc.body)
		}
	}

	rows, err := pool.Query(ctx, `SELECT token || '=' || wallet_public_key FROM push_tokens ORDER BY token`)
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bearer-token=" + wallet, "cookie-token=" + wallet}; strings.Join(stored, ",") != strings.Join(want, ",") {
		t.Fatalf("stored push tokens %v, want %v", stored, want)
	}
}
