package webapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
	"github.com/solana-foundation/solana-go/v2"
)

func TestEvmDepositPostChecksSessionBeforeBody(t *testing.T) {
	keys, err := jwt.NewKeys("isolated-session-test-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	api := humachi.New(router, apiConfig())
	earnRoutes{auth: &Auth{keys: keys}}.registerEvmDeposit(api)
	req := httptest.NewRequest(http.MethodPost, evmDepositPath, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusUnauthorized || body.Error.Code != "unauthenticated" {
		t.Fatalf("unauthenticated POST with empty claim = %d %s; want 401 unauthenticated", res.Code, res.Body.String())
	}
}

func evmDepositTestDB(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("EVM_DEPOSIT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set EVM_DEPOSIT_TEST_DATABASE_URL to a disposable local evm_deposit_*_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Database, "evm_deposit_") || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("requires a disposable local evm_deposit_*_test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reset := `DROP SCHEMA IF EXISTS loyal_yield CASCADE; DROP TABLE IF EXISTS app_users`
	if _, err := pool.Exec(ctx, reset); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, reset); err != nil {
			t.Error(err)
		}
	})
	setup := []string{`CREATE SCHEMA loyal_yield`, `CREATE TABLE app_users (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), provider text NOT NULL, subject_address text NOT NULL)`}
	for _, file := range []string{"0126_evm_deposit_routes.sql", "0127_evm_deposit_routes_shared_wallet.sql"} {
		migration, err := os.ReadFile("../../../../migrations/yield/" + file)
		if err != nil {
			t.Fatal(err)
		}
		setup = append(setup, string(migration))
	}
	for _, statement := range setup {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestEvmDepositRelaySignedRefusesEmbeddedWalletWhenPrivyLookupIsEmpty(t *testing.T) {
	for name, clientTypes := range map[string][]string{
		"privy":               {"privy"},
		"privy-v2":            {"privy-v2"},
		"external then privy": {"phantom", "privy-v2"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := evmDepositTestDB(t)
			ctx := context.Background()
			wallet := solana.NewWallet().PublicKey().String()
			if _, err := pool.Exec(ctx, `INSERT INTO app_users (provider, subject_address) VALUES ('solana', $1)`, wallet); err != nil {
				t.Fatal(err)
			}

			signing, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			const appID = "b3-test-app"
			kid := "b3-test-kid-" + name
			b64 := base64.RawURLEncoding.EncodeToString
			pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
			jwks, err := json.Marshal(map[string]any{"keys": []map[string]string{{
				"kty": "EC", "crv": "P-256", "kid": kid,
				"x": b64(pad(signing.X.Bytes())), "y": b64(pad(signing.Y.Bytes())),
			}}})
			if err != nil {
				t.Fatal(err)
			}
			privy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/jwks.json") {
					w.Write(jwks)
					return
				}
				w.Write([]byte(`{"data":[]}`))
			}))
			defer privy.Close()
			target, err := url.Parse(privy.URL)
			if err != nil {
				t.Fatal(err)
			}
			prevAPI, prevJWKS := privyAPIBase, privyJWKSBase
			privyAPIBase, privyJWKSBase = "https://privy.test/v1", "https://privy.test/v1/apps/"
			t.Cleanup(func() { privyAPIBase, privyJWKSBase = prevAPI, prevJWKS })

			accounts := []map[string]string{{"type": "wallet", "chain_type": "ethereum", "address": "0x" + strings.Repeat("ab", 20), "wallet_client_type": "privy"}}
			for _, clientType := range clientTypes {
				accounts = append(accounts, map[string]string{"type": "wallet", "chain_type": "solana", "address": wallet, "wallet_client_type": clientType})
			}
			linked, err := json.Marshal(accounts)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := json.Marshal(map[string]any{
				"sub": "did:privy:b3", "iss": privyJWTIssuer, "aud": appID,
				"exp": time.Now().Add(time.Hour).Unix(), "linked_accounts": string(linked),
			})
			if err != nil {
				t.Fatal(err)
			}
			header, err := json.Marshal(map[string]string{"alg": privyJWTAlg, "typ": privyJWTTyp, "kid": kid})
			if err != nil {
				t.Fatal(err)
			}
			unsigned := b64(header) + "." + b64(claims)
			digest := sha256.Sum256([]byte(unsigned))
			r, s, err := ecdsa.Sign(rand.Reader, signing, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			identityToken := unsigned + "." + b64(append(pad(r.Bytes()), pad(s.Bytes())...))

			keys, err := jwt.NewKeys("isolated-session-test-secret", "")
			if err != nil {
				t.Fatal(err)
			}
			signer, err := jwt.NewSigner("isolated-session-test-secret", "")
			if err != nil {
				t.Fatal(err)
			}
			session, err := signer.Mint(map[string]any{"sub": wallet}, mobileEarnAudience, time.Hour, time.Now())
			if err != nil {
				t.Fatal(err)
			}

			router := chi.NewRouter()
			api := humachi.New(router, apiConfig())
			earnRoutes{auth: &Auth{keys: keys}, Earn: Earn{
				YieldDB: pool,
				HTTP:    &http.Client{Transport: rewriteTransport{target}},
				Privy:   &Privy{AppID: appID, AppSecret: "secret"},
			}, appDB: pool}.registerEvmDeposit(api)
			body, err := json.Marshal(map[string]string{"privyIdentityToken": identityToken})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, evmDepositPath, strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+session)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)

			var got struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if res.Code != http.StatusConflict || got.Error.Code != "evm_deposit_privy_wallet" {
				t.Fatalf("POST for an embedded wallet = %d %s; want 409 evm_deposit_privy_wallet", res.Code, res.Body.String())
			}
			var rows int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.evm_deposit_routes`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("evm_deposit_routes has %d rows; an embedded wallet must write none", rows)
			}
		})
	}
}
