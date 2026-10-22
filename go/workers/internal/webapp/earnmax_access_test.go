package webapp

import "testing"

func TestEarnMaxReadAccessNeedsMainnetButNotInviteStorage(t *testing.T) {
	// No database or redemption exists: authenticated mainnet wallets may read.
	h := earnMaxRoutes{cfg: EarnMax{Cluster: Mainnet}}
	if err := h.admit(); err != nil {
		t.Fatalf("mainnet access without invite storage: %v", err)
	}
	h.cfg.Cluster = Devnet
	if err := h.admit(); err == nil {
		t.Fatal("opening access must not enable the mainnet vault on devnet")
	}
}
