package admin

import "testing"

func TestBackyardUnknownCurrentNAVDoesNotFallBackToHistoricalReport(t *testing.T) {
	state := map[string]any{"aumUsdMicros": "900", "observation": map[string]any{"navKnown": false, "reportedNavRaw": "800", "navRaw": "800", "positionDebtRaw": "40"}}
	if backyardBigInt(state, []string{"aumUsdMicros", "aumRaw"}) != nil || backyardBigInt(state, []string{"navUsdMicros", "reportedNavRaw", "navRaw"}) != nil {
		t.Fatal("unknown NAV fell back to prior report")
	}
	if debt := backyardBigInt(state, []string{"positionDebtRaw"}); debt == nil || *debt != "40" {
		t.Fatal("unknown NAV hid current debt")
	}
	delete(state["observation"].(map[string]any), "navKnown")
	if backyardBigInt(state, []string{"reportedNavRaw"}) == nil {
		t.Fatal("legacy report compatibility lost")
	}
}
