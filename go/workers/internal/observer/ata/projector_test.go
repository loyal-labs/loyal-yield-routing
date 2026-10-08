package ata

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectionEvidencePreservesSourceIntegerAndExplicitRawData(t *testing.T) {
	got, err := projectionEvidence(json.RawMessage(`{"amount":18446744073709551615,"raw_account_data_base64":"stale"}`), "actual")
	if err != nil || !strings.Contains(string(got), `18446744073709551615`) || strings.Contains(string(got), "stale") {
		t.Fatalf("captured evidence changed integer or retained stale raw data: %s %v", got, err)
	}
	for _, raw := range []string{`null`, `[]`, `"nonobject"`, `18446744073709551615`} {
		got, err := projectionEvidence(json.RawMessage(raw), "actual")
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(got, &object); err != nil || string(object["raw_evidence"]) != raw || string(object["raw_account_data_base64"]) != `"actual"` {
			t.Fatalf("retained nonobject evidence mapping differs: %s %v", got, err)
		}
	}
	if _, err := projectionEvidence(json.RawMessage(`{"truncated"`), ""); err == nil {
		t.Fatal("malformed evidence became an accepted observation")
	}
}
