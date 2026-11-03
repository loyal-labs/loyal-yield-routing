package bridge

import "testing"

func TestFailedTransferRefunds(t *testing.T) {
	s, _ := TransferSubmitted.Advance("failure")
	s, _ = s.Advance("refund")
	if s != TransferRefunded {
		t.Fatalf("failure then refund ended %q, want %q", s, TransferRefunded)
	}
}
