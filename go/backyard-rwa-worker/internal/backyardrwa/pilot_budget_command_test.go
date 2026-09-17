package backyardrwa

import (
	"context"
	"strings"
	"testing"
)

func TestPilotActivationCommandRefusesUnpinnedOrSecretBearingConfig(t *testing.T) {
	for _, route := range []string{"", "another-route"} {
		_, err := RunPilotBudgetActivation(context.Background(), "database-secret", "https://rpc.invalid/key-secret", route)
		assertBudgetHold(t, err, "invalid_pilot_activation_config")
	}
	_, err := RunPilotBudgetActivation(context.Background(), "", "https://rpc.invalid/key-secret", productionRouteKey)
	assertBudgetHold(t, err, "invalid_pilot_activation_config")
	_, err = RunPilotBudgetActivation(context.Background(), "postgres://%database-secret", "https://rpc.invalid/key-secret", productionRouteKey)
	assertBudgetHold(t, err, "pilot_activation_database_unavailable")
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "invalid/") {
		t.Fatal("activation leaked config")
	}
}
