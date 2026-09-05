package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func signedBudgetFixture(t *testing.T) (phase3OperationAuthorization, PersistedOperation) {
	t.Helper()
	request := bridgeTestRequest(ReportNAV, 0)
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	input, err := encodePhase3BuildInput(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Phase3IntentDigest(request, encoded)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	// Unsigned local fixture; neither signer proof nor submission is claimed.
	operation := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: request.RecentBlockhash, LastValidBlockHeight: request.LastValidBlockHeight}
	return phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, SignedWireSHA256: operation.SignedWireSHA256, BuildInput: input}, operation
}

func TestPersistedSendInputRevaluesWithoutRebuildingWire(t *testing.T) {
	auth, operation := signedBudgetFixture(t)
	// JSON round-trip models the durable byte fields; object key ordering
	// cannot change the exact effects digest when PostgreSQL normalizes JSONB.
	encoded, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &auth); err != nil {
		t.Fatal(err)
	}
	original := sha256Bytes(operation.SignedWire)
	cost, err := revaluePhase3SignedInput(context.Background(), budgetBuildRPC(t, 5_000, 42), auth, operation)
	if err != nil || cost.TotalMicros <= 0 || cost.ValidThroughSlot != 74 || sha256Bytes(operation.SignedWire) != original {
		t.Fatalf("persisted repricing mismatch: %+v %v", cost, err)
	}
	_, err = revaluePhase3SignedInput(context.Background(), budgetBuildRPC(t, 20_000_000, 42), auth, operation)
	assertBudgetHold(t, err, "transaction_cap_exceeded")
	var validated *validatedSignedBudgetHold
	if !errors.As(err, &validated) {
		t.Fatal("valid expired wire cannot use proven-absence recovery")
	}
}

func TestPersistedSendInputRejectsIdentityDriftBeforeRPC(t *testing.T) {
	for _, mutate := range []func(*phase3OperationAuthorization, *PersistedOperation){
		func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.BuildInput = nil },
		func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.BuildInput.Kind = "policy-setup" },
		func(a *phase3OperationAuthorization, _ *PersistedOperation) {
			a.BuildInput.Request = append(a.BuildInput.Request, []byte(` {"extra":1}`)...)
		},
		func(a *phase3OperationAuthorization, _ *PersistedOperation) { a.IntentSHA256 = strings.Repeat("a", 64) },
		func(_ *phase3OperationAuthorization, o *PersistedOperation) { o.LastValidBlockHeight-- },
		func(_ *phase3OperationAuthorization, o *PersistedOperation) { o.TransactionSignature = "changed" },
		func(a *phase3OperationAuthorization, o *PersistedOperation) {
			o.SignedWire[len(o.SignedWire)-1] ^= 1
			o.SignedWireSHA256 = sha256Bytes(o.SignedWire)
			a.SignedWireSHA256 = o.SignedWireSHA256
		},
	} {
		auth, operation := signedBudgetFixture(t)
		mutate(&auth, &operation)
		_, err := revaluePhase3SignedInput(context.Background(), nil, auth, operation)
		var hold *BudgetHold
		var validated *validatedSignedBudgetHold
		if !errors.As(err, &hold) || errors.As(err, &validated) {
			t.Fatalf("untrusted persisted identity reached valuation or expiry: %v", err)
		}
	}
}
