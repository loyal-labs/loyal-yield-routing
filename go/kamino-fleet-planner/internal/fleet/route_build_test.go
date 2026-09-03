package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func routeFixture(t *testing.T) KaminoSameMintRouteRequest {
	t.Helper()
	raw, err := os.ReadFile("../../../../verification/kamino-fleet-parity/kamino-route-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var request KaminoSameMintRouteRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	return request
}
func fixtureProxy(t *testing.T, output string) *KLendProxy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy")
	script := "#!/bin/sh\ncat '" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	proxy, err := NewKLendProxy(path)
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}
func TestKLendProxyAcceptsRustReferenceOutput(t *testing.T) {
	output, err := filepath.Abs("../../../../verification/kamino-fleet-parity/kamino-route-v1-output.json")
	if err != nil {
		t.Fatal(err)
	}
	route, err := fixtureProxy(t, output).Build(context.Background(), routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(route.Public) != 4 || len(route.Protected) != 2 {
		t.Fatal("incomplete route")
	}
}
func TestKLendProxyRejectsMalformedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"public":[],"protected":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureProxy(t, path).Build(context.Background(), routeFixture(t)); err == nil {
		t.Fatal("malformed proxy output accepted")
	}
}
