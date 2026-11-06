package backyard

import (
	"testing"
	"time"
)

func TestBridgeCertificateBindsFinalEconomicRequest(t *testing.T) {
	for _, action := range []Action{VoltrAllocateToSquads, VoltrRestoreIdle, ReportNAV} {
		r := bridgeTestRequest(action, 20)
		if action == ReportNAV {
			r.AmountRaw = 0
			r = bindBridgeRequest(r)
		}
		if _, err := compileBridgeMessageForDelegate(rwa, r, mustKey(backyardDelegate)); err != nil {
			t.Fatal(err)
		}
		for _, change := range []func(*BridgeBuildRequest){func(r *BridgeBuildRequest) { r.AmountRaw = 50 }, func(r *BridgeBuildRequest) { r.Action = bridgeRestoreIdle }, func(r *BridgeBuildRequest) { r.Policy = backyardSettings }, func(r *BridgeBuildRequest) { r.Report.NAVAfterRaw++ }, func(r *BridgeBuildRequest) { r.LastValidBlockHeight++ }} {
			copied := r
			change(&copied)
			if copied == r {
				continue
			}
			if _, err := compileBridgeMessageForDelegate(rwa, copied, mustKey(backyardDelegate)); err == nil {
				t.Fatal("mutated certified bridge economics produced wire", action)
			}
		}
	}
	r := bridgeTestRequest(StageSquadsToVoltr, 20)
	r.net.vault = looping.squads
	r = bindBridgeRequest(r)
	if err := r.validateNAV(looping, time.Now()); err != nil {
		t.Fatal("Trustful fixture", err)
	}
	r.AmountRaw = 50
	if err := r.validateNAV(looping, time.Now()); err == nil {
		t.Fatal("Trustful stage amount escaped seal")
	}
}
