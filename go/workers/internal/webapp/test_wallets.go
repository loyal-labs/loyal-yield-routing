package webapp

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/solana-foundation/solana-go/v2"
)

// Bridge and referrals stay closed until launch. The web client's copy of this list
// (web/apps/web/src/features/test-wallets.ts) only picks which UI to show.
type walletGate struct {
	all     bool
	wallets map[solana.PublicKey]struct{}
}

var ownerTestWallets = []string{
	"BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
	"62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
}

var testWallets = sync.OnceValue(func() walletGate {
	list, set := os.LookupEnv("LOYAL_TEST_WALLETS")
	if !set {
		list = strings.Join(ownerTestWallets, ",")
	}
	gate := walletGate{wallets: map[solana.PublicKey]struct{}{}}
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
		case entry == "*":
			gate.all = true
		default:
			key, err := solana.PublicKeyFromBase58(entry)
			if err != nil {
				slog.Warn("LOYAL_TEST_WALLETS entry skipped", "entry", entry, "error", err)
				continue
			}
			gate.wallets[key] = struct{}{}
		}
	}
	return gate
})

func (g walletGate) admits(wallet solana.PublicKey) bool {
	_, listed := g.wallets[wallet]
	return g.all || listed
}

func admitTestWallet(wallet solana.PublicKey) error {
	if testWallets().admits(wallet) {
		return nil
	}
	return earnError(http.StatusForbidden, "not_open", "This feature is not open yet.")
}
