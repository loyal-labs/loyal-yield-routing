package earnmax

import (
	"encoding/json"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"testing"
	"time"
)

func TestCurrentWithdrawalHealthExposesExactMissingValuationReason(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	raw, err := json.Marshal(WithdrawalHealth{Version: 1, Cluster: Cluster, Program: voltr.ProgramID.String(), RouteKey: RouteKey, Vault: Vault.String(), Status: HealthWaiting, Reason: "exit_unpriced", ObservedAt: now, ObservedSlot: 123})
	if err != nil {
		t.Fatal(err)
	}
	view := healthView(raw, now)
	if view.Status != HealthWaiting || view.Reason != "exit_unpriced" {
		t.Fatalf("reason hidden: %+v", view)
	}
	stale := healthView(raw, now.Add(3*time.Minute))
	if stale.Status != HealthUnavailable || stale.Reason != "" {
		t.Fatalf("stale reason presented current: %+v", stale)
	}
}
