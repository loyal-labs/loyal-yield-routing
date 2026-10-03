package backyard_test

import (
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// The Backyard engine composes into the shared process runtime as a plain
// lane. Keeping the assertion in this external test preserves the domain
// package's no-engine-import direction while pinning the composition contract
// root's loyal-engine command relies on.
var _ engine.Lane = (*backyard.Engine)(nil)

// The platform-neutral Backyard owner must stay byte-identical to the shared
// engine instance identity, so a mixed retail/Backyard process never produces
// two owner dialects for the same fencing rules. The domain package does not
// import engine; this external test is the drift alarm.
func TestBackyardLeaseOwnerMatchesSharedEngineInstanceIdentity(t *testing.T) {
	release := "sha-" + strings.Repeat("d", 40)
	for _, instance := range []string{"backyard-eu-1", "srv-legacy", "loyal_backyard-2", strings.Repeat("x", 80)} {
		want, err := engine.InstanceOwner("backyard", instance, release)
		if err != nil {
			t.Fatal(err)
		}
		got, err := (backyard.RuntimeConfig{InstanceID: instance, ImageVersion: release}).LeaseOwner()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Backyard owner %q drifted from the shared engine instance identity %q", got, want)
		}
	}
	for _, instance := range []string{strings.Repeat("x", 81), "backyard eu"} {
		if _, err := engine.InstanceOwner("backyard", instance, release); err == nil {
			t.Fatalf("engine accepted instance %q that Backyard rejects", instance)
		}
		if _, err := (backyard.RuntimeConfig{InstanceID: instance, ImageVersion: release}).LeaseOwner(); err == nil {
			t.Fatalf("Backyard accepted instance %q that engine rejects", instance)
		}
	}
}
