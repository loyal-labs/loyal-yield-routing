package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
)

var earnLaunchFamily = earnFamily{
	name:     "launch",
	ready:    func(h earnRoutes) bool { return h.signedIn() },
	register: earnRoutes.registerLaunch,
}

const launchPath = "/api/funding/launch"

// Bridge and referrals stay closed until launch. Routes refuse through admit; the web app
// reads the same answer from launchPath to decide what to show.
type TestWallets struct {
	all     bool
	wallets map[solana.PublicKey]struct{}
}

var ownerTestWallets = []string{
	"BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
	"62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
}

func TestWalletsFromEnv() TestWallets {
	list, set := os.LookupEnv("LOYAL_TEST_WALLETS")
	if !set {
		list = strings.Join(ownerTestWallets, ",")
	}
	return parseTestWallets(list)
}

func parseTestWallets(list string) TestWallets {
	gate := TestWallets{wallets: map[solana.PublicKey]struct{}{}}
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
}

func (g TestWallets) open(wallet solana.PublicKey) bool {
	_, listed := g.wallets[wallet]
	return g.all || listed
}

func (g TestWallets) admit(wallet solana.PublicKey) error {
	if g.open(wallet) {
		return nil
	}
	return earnError(http.StatusForbidden, "not_open", "This feature is not open yet.")
}

type LaunchResponse struct {
	Launched bool `json:"launched"`
}

type launchOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         LaunchResponse
}

func (h earnRoutes) registerLaunch(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-launch",
		Method:      http.MethodGet,
		Path:        launchPath,
		Summary:     "Whether bridge and referrals are open to the signed-in wallet, or to everyone when signed out",
		Responses:   errorResponses[routeError](api, 503),
	}, h.launch)
}

func (h earnRoutes) launch(ctx context.Context, in *sessionRequest) (*launchOutput, error) {
	session, err := h.auth.Session(in.SessionInput)
	var signedOut *AuthError
	switch {
	case errors.As(err, &signedOut):
		return &launchOutput{CacheControl: "no-store", Body: LaunchResponse{Launched: h.TestWallets.all}}, nil
	case err != nil:
		return nil, earnUnavailable("launch session", err, "launch_unavailable", "The launch gate is unavailable.")
	}
	return &launchOutput{CacheControl: "no-store", Body: LaunchResponse{Launched: h.TestWallets.open(session.Wallet)}}, nil
}
