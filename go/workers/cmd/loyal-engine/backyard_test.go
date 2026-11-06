package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
)

func TestBackyardRequiresExplicitVaultBeforeLoadingCredentials(t *testing.T) {
	for _, vault := range []string{"", "both", "rwa,looping"} {
		t.Setenv("BACKYARD_VAULT", vault)
		if err := runBackyard(context.Background(), "test", nil, nil); err == nil || !strings.Contains(err.Error(), "BACKYARD_VAULT") {
			t.Fatalf("vault %q did not fail before credentials: %v", vault, err)
		}
	}
}

func TestBackyardInFlightVaultScope(t *testing.T) {
	for _, vault := range []string{"rwa", "looping"} {
		route, _ := backyard.RouteKey(vault)
		got, err := backyardInFlightRoutes([]string{"--vault", vault})
		if err != nil || !slices.Equal(got, []string{route}) {
			t.Fatalf("vault %s: routes %v: %v", vault, got, err)
		}
	}
	got, err := backyardInFlightRoutes(nil)
	if err != nil || !slices.Equal(got, backyard.RouteKeys()) {
		t.Fatal("unscoped read must still count both vaults for the initial cutover")
	}
	for _, args := range [][]string{{"--vault"}, {"--vault", ""}, {"--vault", "both"}, {"--vault", "rwa", "looping"}, {"rwa"}} {
		if _, err := backyardInFlightRoutes(args); err == nil {
			t.Fatalf("accepted invalid scope %q", args)
		}
	}
}
