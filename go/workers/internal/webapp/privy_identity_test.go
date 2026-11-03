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
	"sync"
	"testing"
	"time"
)

func TestPrivyIdentityTokenFromRotatedKeyIsAcceptedAfterUnknownKid(t *testing.T) {
	b64 := base64.RawURLEncoding.EncodeToString
	pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
	newKey := func() (*ecdsa.PrivateKey, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key, "rotation-kid-" + b64(pad(key.X.Bytes()))
	}
	const appID = "rotation-app"
	now := time.Now()
	mint := func(key *ecdsa.PrivateKey, kid string) string {
		header, _ := json.Marshal(map[string]string{"alg": privyJWTAlg, "typ": privyJWTTyp, "kid": kid})
		claims, _ := json.Marshal(map[string]any{
			"sub": "did:privy:rotation", "iss": privyJWTIssuer, "aud": appID,
			"exp": now.Add(time.Hour).Unix(), "linked_accounts": "[]",
		})
		unsigned := b64(header) + "." + b64(claims)
		digest := sha256.Sum256([]byte(unsigned))
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return unsigned + "." + b64(append(pad(r.Bytes()), pad(s.Bytes())...))
	}

	var mu sync.Mutex
	published := map[string]*ecdsa.PublicKey{}
	publish := func(key *ecdsa.PrivateKey, kid string) {
		mu.Lock()
		defer mu.Unlock()
		published[kid] = &key.PublicKey
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		keys := []map[string]string{}
		for kid, pub := range published {
			keys = append(keys, map[string]string{"kty": "EC", "crv": "P-256", "kid": kid, "x": b64(pad(pub.X.Bytes())), "y": b64(pad(pub.Y.Bytes()))})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer server.Close()
	prev := privyJWKSBase
	privyJWKSBase = server.URL + "/apps/"
	t.Cleanup(func() { privyJWKSBase = prev })

	h := earnRoutes{Earn: Earn{HTTP: server.Client(), Privy: &Privy{AppID: appID}}}
	ctx := context.Background()

	first, firstKid := newKey()
	publish(first, firstKid)
	if _, err := h.parsePrivyIdentityToken(ctx, mint(first, firstKid), now); err != nil {
		t.Fatalf("token from the published key: %v", err)
	}

	stray, strayKid := newKey()
	if _, err := h.parsePrivyIdentityToken(ctx, mint(stray, strayKid), now); err == nil {
		t.Fatal("token with an unpublished kid was accepted")
	}

	rotated, rotatedKid := newKey()
	publish(rotated, rotatedKid)
	if _, err := h.parsePrivyIdentityToken(ctx, mint(rotated, rotatedKid), now.Add(time.Minute)); err != nil {
		t.Fatalf("token from a key Privy published after an unknown kid: %v", err)
	}
}
