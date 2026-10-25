package webapp

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/yield-optimization/deposits/prepare <- web/apps/web/src/app/api/smart-accounts/yield-optimization/deposits/prepare/route.ts (+ the account gate of web/apps/web/src/app/api/smart-accounts/mobile/earn/deposit/prepare/route.ts for bearer sessions)
var earnDprepFamily = earnFamily{
	name:        "earn deposit prepare",
	appGrants:   earnPrivyFamily.appGrants,
	yieldGrants: append(append([]Grant{}, earnStateYieldGrants...), TableGrant("loyal_yield.earn_deposit_onboarding_attempts", "SELECT")),
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.Chain != nil && !h.PolicySigner.IsZero() && h.Timescale != nil
	},
	register: func(h earnRoutes, api huma.API) { dprepRoutesFor(h).register(api) },
}

const earnDepositsPreparePath = "/api/smart-accounts/yield-optimization/deposits/prepare"

type dprepRoutes struct {
	earnRoutes
	reserves  dtargetQuerier
	blockTime dtargetBlockTimeFunc
}

func dprepRoutesFor(h earnRoutes) dprepRoutes {
	return dprepRoutes{earnRoutes: h, reserves: h.Timescale, blockTime: dprepChainBlockTime(h.Chain)}
}

func dprepChainBlockTime(client *chain.Client) dtargetBlockTimeFunc {
	return func(ctx context.Context, slot int64) (int64, bool, error) {
		if client == nil || slot <= 0 {
			return 0, false, nil
		}
		at, err := client.BlockTime(ctx, uint64(slot))
		if errors.Is(err, chain.ErrNotFound) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return at.Unix(), true, nil
	}
}

type dprepRequestBody struct {
	AmountRaw string `json:"amountRaw"`
	Mint      string `json:"mint"`
}

type dprepInput struct {
	SessionInput
	Raw rawJSON
}

type dprepOutput struct {
	Body dprepResponse
}

func (h dprepRoutes) register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-deposits-prepare",
		Method:      http.MethodPost,
		Path:        earnDepositsPreparePath,
		Summary:     "Prepare an Earn deposit, creating the Earn policies and, for a mobile session, the smart account when absent",
		Responses:   errorResponses[routeError](api, 400, 401, 402, 409, 500, 502),
	}, h.prepare)
	documentJSONBody[dprepRequestBody](api, earnDepositsPreparePath)
}

func (h dprepRoutes) prepare(ctx context.Context, in *dprepInput) (*dprepOutput, error) {
	session, err := h.auth.Session(in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, depositPrepareFailed(err)
	}
	amount, mint, err := dprepParseBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	cluster := h.Cluster.Loyal()
	product, err := dtargetResolveEarnProductAsset(cluster, mint.String())
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "unsupported_mint", err.Error())
	}
	_, mobile := in.bearer()

	principal, err := h.principal(ctx, session, mobile, amount, product)
	if err != nil {
		return nil, err
	}
	vault, _, err := squads.SmartAccountAddress(principal.Settings, earnprep.EarnVaultIndex)
	if err != nil {
		return nil, depositPrepareFailed(err)
	}
	account := EarnAccount{Principal: principal, Vault: vault, Cluster: cluster}

	prepared, err := h.prepareDeposit(ctx, account, amount, product)
	if err != nil {
		var refused *routeError
		if errors.As(err, &refused) {
			return nil, refused
		}
		slog.Error("[earn-deposit-prepare] prepare failed",
			"amountRaw", amount, "cluster", string(cluster), "mint", product.Mint,
			"error", err, "settings", account.Settings.String(), "walletAddress", account.Wallet.String())
		return nil, depositPrepareFailed(err)
	}
	response := dprepResponse{PreparedDeposit: *prepared}
	if mobile {
		clusterText, programID := string(cluster), squads.ProgramID.String()
		settingsText, smartAccountText := principal.Settings.String(), principal.SmartAccount.String()
		response.Cluster, response.ProgramID = &clusterText, &programID
		response.SettingsPda, response.SmartAccountAddress = &settingsText, &smartAccountText
	}
	return &dprepOutput{Body: response}, nil
}

func (h dprepRoutes) principal(ctx context.Context, session Session, mobile bool, amount uint64, product dtargetProductMint) (Principal, error) {
	settings, smartAccount, err := session.account(ctx)
	var refused *AuthError
	if mobile && errors.As(err, &refused) && refused.Failure == SmartAccountRequired {
		return h.gate(ctx, session.Wallet, amount, product)
	}
	if err == nil {
		claimed := Principal{Wallet: session.Wallet, Settings: settings, SmartAccount: smartAccount}
		if err = h.auth.controls(ctx, claimed); err == nil {
			return claimed, nil
		}
	}
	if refusal, ok := authRefusal(err); ok {
		return Principal{}, refusal
	}
	return Principal{}, depositPrepareFailed(err)
}

var dprepUnsignedInteger = regexp.MustCompile(`^\d+$`)

func dprepParseBody(raw []byte) (uint64, solana.PublicKey, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return 0, solana.PublicKey{}, err
	}
	invalid := func(message string) error { return earnError(http.StatusBadRequest, "invalid_request", message) }
	record, ok := parsed.(map[string]any)
	if !ok {
		if _, isArray := parsed.([]any); !isArray {
			return 0, solana.PublicKey{}, invalid("Request body must be an object.")
		}
		record = map[string]any{}
	}
	text, ok := record["amountRaw"].(string)
	if !ok || !dprepUnsignedInteger.MatchString(strings.TrimSpace(text)) {
		return 0, solana.PublicKey{}, invalid("amountRaw must be an unsigned integer string.")
	}
	value, _ := new(big.Int).SetString(strings.TrimSpace(text), 10)
	if value.Sign() <= 0 {
		return 0, solana.PublicKey{}, invalid("amountRaw must be greater than 0.")
	}
	if !value.IsUint64() {
		return 0, solana.PublicKey{}, invalid("amountRaw must be an unsigned integer string.")
	}
	mintText, ok := record["mint"].(string)
	if !ok {
		return 0, solana.PublicKey{}, invalid("mint must be a mint public key.")
	}
	mint, err := solana.PublicKeyFromBase58(mintText)
	if err != nil {
		return 0, solana.PublicKey{}, invalid("Invalid public key input")
	}
	return value.Uint64(), mint, nil
}
