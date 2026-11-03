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

type privyJWKSFixture struct {
	t       *testing.T
	appID   string
	now     time.Time
	h       earnRoutes
	mu      sync.Mutex
	pub     map[string]*ecdsa.PublicKey
	onFetch func()
}

func newPrivyJWKSFixture(t *testing.T, appID string) *privyJWKSFixture {
	f := &privyJWKSFixture{t: t, appID: appID, now: time.Now(), pub: map[string]*ecdsa.PublicKey{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		hook := f.onFetch
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		keys := []map[string]string{}
		for kid, pub := range f.pub {
			keys = append(keys, map[string]string{"kty": "EC", "crv": "P-256", "kid": kid, "x": privyB64(privyPad(pub.X.Bytes())), "y": privyB64(privyPad(pub.Y.Bytes()))})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(server.Close)
	prev := privyJWKSBase
	privyJWKSBase = server.URL + "/apps/"
	t.Cleanup(func() { privyJWKSBase = prev })
	f.h = earnRoutes{Earn: Earn{HTTP: server.Client(), Privy: &Privy{AppID: appID}}}
	return f
}

func privyB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func privyPad(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

func (f *privyJWKSFixture) newKey() (*ecdsa.PrivateKey, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	return key, "rotation-kid-" + privyB64(privyPad(key.X.Bytes()))
}

func (f *privyJWKSFixture) publish(key *ecdsa.PrivateKey, kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pub[kid] = &key.PublicKey
}

func (f *privyJWKSFixture) mint(key *ecdsa.PrivateKey, kid string) string {
	header, _ := json.Marshal(map[string]string{"alg": privyJWTAlg, "typ": privyJWTTyp, "kid": kid})
	claims, _ := json.Marshal(map[string]any{
		"sub": "did:privy:rotation", "iss": privyJWTIssuer, "aud": f.appID,
		"exp": f.now.Add(time.Hour).Unix(), "linked_accounts": "[]",
	})
	unsigned := privyB64(header) + "." + privyB64(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return unsigned + "." + privyB64(append(privyPad(r.Bytes()), privyPad(s.Bytes())...))
}

func TestPrivyIdentityTokenFromRotatedKeyIsAcceptedAfterUnknownKid(t *testing.T) {
	f := newPrivyJWKSFixture(t, "rotation-app")
	ctx := context.Background()

	first, firstKid := f.newKey()
	f.publish(first, firstKid)
	if _, err := f.h.parsePrivyIdentityToken(ctx, f.mint(first, firstKid), f.now); err != nil {
		t.Fatalf("token from the published key: %v", err)
	}

	stray, strayKid := f.newKey()
	if _, err := f.h.parsePrivyIdentityToken(ctx, f.mint(stray, strayKid), f.now); err == nil {
		t.Fatal("token with an unpublished kid was accepted")
	}

	rotated, rotatedKid := f.newKey()
	f.publish(rotated, rotatedKid)
	if _, err := f.h.parsePrivyIdentityToken(ctx, f.mint(rotated, rotatedKid), f.now.Add(time.Minute)); err != nil {
		t.Fatalf("token from a key Privy published after an unknown kid: %v", err)
	}
}

func TestPrivyJWKSFetchAbandonedByClientDoesNotArmCooldown(t *testing.T) {
	f := newPrivyJWKSFixture(t, "abandoned-app")

	first, firstKid := f.newKey()
	f.publish(first, firstKid)

	inFlight := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.onFetch = func() {
		once.Do(func() {
			close(inFlight)
			<-release
		})
	}
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.parsePrivyIdentityToken(ctx, f.mint(first, firstKid), f.now)
	}()
	<-inFlight
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	rotated, rotatedKid := f.newKey()
	f.publish(rotated, rotatedKid)
	if _, err := f.h.parsePrivyIdentityToken(context.Background(), f.mint(rotated, rotatedKid), f.now.Add(time.Minute)); err != nil {
		t.Fatalf("token from a key Privy published after a dropped request: %v", err)
	}
}
