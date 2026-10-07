package backyard_test

import (
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// The Backyard engine composes into the shared process runtime as a plain lane.
var _ engine.Lane = (*backyard.Engine)(nil)

// The shared engine instance identity is the only owner dialect the Backyard
// lease accepts: what loyal-engine computes, the route fence admits.
func TestBackyardLeaseOwnerMatchesSharedEngineInstanceIdentity(t *testing.T) {
	release := "sha-" + strings.Repeat("d", 40)
	for _, instance := range []string{"backyard-eu-1", "srv-legacy", "loyal_backyard-2", strings.Repeat("x", 80)} {
		owner, err := engine.InstanceOwner("backyard", instance, release)
		if err != nil {
			t.Fatal(err)
		}
		if !backyard.ValidLeaseOwner(owner) {
			t.Fatalf("Backyard rejected the shared engine instance identity %q", owner)
		}
	}
	for _, owner := range []string{"worker:retail:backyard-eu-1:" + release, "render:srv-legacy:" + release, "worker:backyard:" + strings.Repeat("x", 81) + ":" + release} {
		if backyard.ValidLeaseOwner(owner) {
			t.Fatalf("Backyard accepted foreign owner %q", owner)
		}
	}
}
