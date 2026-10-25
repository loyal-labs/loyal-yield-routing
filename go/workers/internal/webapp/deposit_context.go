package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/earn-max/context: resolve the signed-in wallet's
// Earn smart account, sponsoring the on-chain create through
// earn_sponsor.go's ensureSmartAccount when no ready account exists, after
// the $1 USDC gate. Nothing here signs anything itself and
// no transaction is built outside the seam.
const earnMaxContextPath = "/api/smart-accounts/earn-max/context"

// earnMaxContextMinUsdc is BigInt(1_000_000): the $1 minimum
// in raw USDC base units (6 decimals).
const earnMaxContextMinUsdc = uint64(1_000_000)

// earnMaxContextUsdcMint is VOLTR_ASSET_MINT
// , the mainnet USDC whose
// spendable balance the gate reads.
var earnMaxContextUsdcMint = solana.MustPublicKeyFromBase58(earncatalog.USDC)

// EarnMaxContextRequest is the POST body. The Next route dispatches
// summary/activity/context on one path; this dedicated path implies context,
// so an explicit action must still be exactly "context" and
// RN's `{"action":"context"}` parses unchanged.
type EarnMaxContextRequest struct {
	Action *string `json:"action,omitempty"`
}

type earnMaxContextInput struct {
	SessionInput
	Raw rawJSON
}

type EarnMaxContextResponse struct {
	Cluster       string `json:"cluster"`
	WalletAddress string `json:"walletAddress"`
	SettingsPda   string `json:"settingsPda"`
	ProgramID     string `json:"programId"`
}

type earnMaxContextOutput struct {
	Body EarnMaxContextResponse
}

func (h earnRoutes) registerEarnMaxContext(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-max-context",
		Method:      http.MethodPost,
		Path:        earnMaxContextPath,
		Summary:     "Resolve or provision the signed-in wallet's Earn MAX account",
		Responses:   errorResponses[routeError](api, 400, 401, 402, 403, 409, 500, 502),
	}, h.earnMaxContext)
	documentJSONBody[EarnMaxContextRequest](api, earnMaxContextPath)
}

func earnMaxContextAction(raw []byte) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var body EarnMaxContextRequest
	if json.Unmarshal(raw, &body) != nil {
		return earnError(http.StatusBadRequest, "invalid_request", "Unknown Earn MAX action.")
	}
	if body.Action != nil && *body.Action != "context" {
		return earnError(http.StatusBadRequest, "invalid_request", "Unknown Earn MAX action.")
	}
	return nil
}

func (h earnRoutes) earnMaxContext(ctx context.Context, in *earnMaxContextInput) (*earnMaxContextOutput, error) {
	if err := earnMaxContextAction(in.Raw.data); err != nil {
		return nil, err
	}
	session, err := h.auth.Session(in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, earnMaxContextFailed("session", err)
	}
	if h.Cluster != Mainnet {
		return nil, earnError(http.StatusConflict, "unsupported_cluster", "Earn MAX requires mainnet.")
	}
	settingsPda, err := h.earnMaxContextSettings(ctx, session.Wallet)
	if err != nil {
		return nil, err
	}
	return &earnMaxContextOutput{Body: EarnMaxContextResponse{
		Cluster:       string(Mainnet),
		WalletAddress: session.Wallet.String(),
		SettingsPda:   settingsPda,
		ProgramID:     squads.ProgramID.String(),
	}}, nil
}

// earnMaxContextSettings returns a settings PDA only after a chain read lists
// the wallet as a signer: promoteSmartAccount, createSponsoredSmartAccount or
// delegatedRootSigner (earn_sponsor.go).
func (h earnRoutes) earnMaxContextSettings(ctx context.Context, wallet solana.PublicKey) (string, error) {
	userID, err := h.earnMaxContextUserID(ctx, wallet)
	if err != nil {
		return "", earnMaxContextFailed("app user", err)
	}
	if userID != "" {
		existing, err := h.findSmartAccount(ctx, userID)
		if err != nil {
			return "", earnMaxContextFailed("smart account", err)
		}
		if existing != nil && existing.State == "ready" {
			resolution, promoted, err := h.promoteSmartAccount(ctx, *existing, wallet)
			if err != nil {
				return "", earnMaxContextFailed("smart account", err)
			}
			if resolution == settingsReady {
				return promoted.SettingsPda, nil
			}
		}
	}
	balance, err := h.earnMaxWalletUsdcBalance(ctx, wallet)
	if err != nil {
		return "", earnMaxContextFailed("wallet usdc balance", err)
	}
	if err := earnMaxContextBalanceGate(balance); err != nil {
		return "", err
	}
	userID, err = h.privyGetOrCreateUser(ctx, wallet, "")
	if err != nil {
		return "", earnMaxContextFailed("app user", err)
	}
	ensured, err := h.ensureSmartAccount(ctx, userID, wallet)
	if err != nil {
		return "", earnMaxContextFailed("smart account provisioning", err)
	}
	return ensured.SmartAccount.SettingsPda, nil
}

func (h earnRoutes) earnMaxContextUserID(ctx context.Context, wallet solana.PublicKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id string
	err := h.appDB.QueryRow(ctx, `SELECT id::text FROM app_users WHERE provider = 'solana' AND subject_address = $1`, wallet.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read app user by principal: %w", err)
	}
	return id, nil
}

// earnMaxWalletUsdcBalance sums the wallet's USDC token accounts whose state
// is "initialized" — a frozen account holds but does not spend, so it does
// not count.
func (h earnRoutes) earnMaxWalletUsdcBalance(ctx context.Context, wallet solana.PublicKey) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, accounts, err := h.Chain.TokenAccounts(ctx, wallet, depositTokenProgramID, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return 0, err
	}
	var total uint64
	for i := range accounts {
		token, err := spl.DecodeTokenAccount(&accounts[i])
		if err != nil || token.Frozen || token.Mint != earnMaxContextUsdcMint {
			continue
		}
		total += token.Amount
	}
	return total, nil
}

// earnMaxContextBalanceGate: the shipped clients send no deposit amount, so a
// wallet holding less than $1 USDC cannot have its setup sponsored.
func earnMaxContextBalanceGate(balance uint64) error {
	if balance < earnMaxContextMinUsdc {
		return earnError(http.StatusPaymentRequired, "insufficient_balance",
			"At least $1 USDC is needed in your wallet to set up Earn MAX.")
	}
	return nil
}

// earnMaxContextFailed passes a provisioning refusal through with its own
// status, code and message; anything else is logged and answered 500
// earn_max_unavailable — this route's shipped contract, not EARN.md's generic
// 503.
func earnMaxContextFailed(step string, err error) error {
	var refused *routeError
	if errors.As(err, &refused) {
		return refused
	}
	slog.Warn("earn max context failed", "step", step, "error", err)
	return earnError(http.StatusInternalServerError, "earn_max_unavailable", "Earn MAX is temporarily unavailable.")
}
