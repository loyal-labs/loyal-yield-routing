package fleet

import (
	"encoding/json"
	"testing"
)

func TestRejectionCountsSummarizesFleetWithoutVaultIDs(t *testing.T) {
	rejections := make(map[int64]string)
	for id := int64(1); id <= 1600; id++ {
		rejections[id] = "no_eligible_target"
	}
	rejections[1601] = "target_capacity_exhausted"
	counts := rejectionCounts(rejections)
	if len(counts) != 2 || counts["no_eligible_target"] != 1600 || counts["target_capacity_exhausted"] != 1 {
		t.Fatalf("unexpected summary: %v", counts)
	}
	encoded, err := json.Marshal(counts)
	if err != nil || len(encoded) > 100 {
		t.Fatalf("summary grew with fleet size: bytes=%d error=%v", len(encoded), err)
	}
	if len(rejections) != 1601 || rejections[1600] != "no_eligible_target" {
		t.Fatal("logging changed the detailed planning results")
	}
	encoded, err = json.Marshal(rejectionCounts(nil))
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("empty fleet summary: %s error=%v", encoded, err)
	}
}
