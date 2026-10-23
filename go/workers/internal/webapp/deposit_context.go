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

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/earn-max/context ports the context action of
// web/apps/web/src/app/api/smart-accounts/mobile/earn-max/route.ts POST
// (features/earn-max/server/mobile.ts:38-121): resolve the signed-in wallet's
// Earn smart account, sponsoring the on-chain create through
// earn_sponsor.go's ensureSmartAccount when no ready account exists, after
// the $1 USDC gate (mobile.ts:79-94). Nothing here signs anything itself and
// no transaction is built outside the seam.
const earnMaxContextPath = "/api/smart-accounts/earn-max/context"

// earnMaxContextMinUsdc is BigInt(1_000_000) (mobile.ts:82): the $1 minimum
// in raw USDC base units (6 decimals).
const earnMaxContextMinUsdc = uint64(1_000_000)

// earnMaxContextUsdcMint is VOLTR_ASSET_MINT
// (smart-account-vaults/src/voltr/program.ts:30), the mainnet USDC whose
// spendable balance the gate reads.
var earnMaxContextUsdcMint = solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")

// EarnMaxContextRequest is the POST body. The Next route dispatches
// summary/activity/context on one path; this dedicated path implies context,
// so an explicit action must still be exactly "context" (mobile.ts:40) and
// RN's `{"action":"context"}` parses unchanged.
type EarnMaxContextRequest struct {
	Action *string `json:"action,omitempty"`
}

type earnMaxContextInput struct {
	SessionInput
	Raw rawJSON
}

// EarnMaxContextResponse is mobile.ts:114-121's json(...) body, fields in
// the TS object-literal order.
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

// earnMaxContextAction mirrors the action validation the Next route runs
// before anything else (mobile.ts:39-42): a body that is not JSON, or whose
// action is not one the route serves, is 400 invalid_request. The dedicated
// path only serves context, so an explicit action must be "context"; an
// absent action or empty body reads as context.
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

// earnMaxContext is the context action of the Next POST (mobile.ts:38-121)
// on its dedicated path: the same order — action validation, wallet session,
// mainnet gate, ready-account short circuit, $1 USDC gate, user
// get-or-create, sponsored provisioning — and the same bodies.
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

// earnMaxContextSettings is mobile.ts:67-108: use the wallet's ready smart
// account when one exists (findReadyCurrentUserSmartAccount's state and
// on-chain signer check, mobile.ts:73-78), otherwise require $1 USDC
// (mobile.ts:80-88), create the app user (mobile.ts:89-94) and resolve the
// account through the sponsored-provisioning seam (mobile.ts:95-100). The
// seam's chain reads are also the wallet-controls assert the Next route runs
// at mobile.ts:109-113: findSettingsSignerAddresses (onchain.ts:116-152) is
// that assert's own chain read, so an ensured account is verified by
// construction.
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
	if balance < earnMaxContextMinUsdc {
		return "", earnError(http.StatusPaymentRequired, "insufficient_balance",
			"At least $1 USDC is needed in your wallet to set up Earn MAX.")
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

// earnMaxContextUserID is findCurrentUser (app-user.ts:154): "" when the
// wallet has no app user row yet, leaving the create to privyGetOrCreateUser
// after the balance gate, as mobile.ts:89 orders it.
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

// earnMaxWalletUsdcBalance is readEarnMaxWalletUsdcBalance
// (voltr/summary.server.ts:44-60): the wallet's token accounts at confirmed,
// summed over the ones for the USDC mint whose state is "initialized" — a
// frozen account holds but does not spend, so it does not count.
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

// earnMaxContextFailed is the Next POST's catch (mobile.ts:132-148): a
// provisioning refusal keeps its own status, code and message, and anything
// else is logged without the request body and answered 500
// earn_max_unavailable — the status the mobile route ships, kept over
// EARN.md's generic 503 because this port's contract is that route.
func earnMaxContextFailed(step string, err error) error {
	var refused *routeError
	if errors.As(err, &refused) {
		return refused
	}
	slog.Warn("earn max context failed", "step", step, "error", err)
	return earnError(http.StatusInternalServerError, "earn_max_unavailable", "Earn MAX is temporarily unavailable.")
}
