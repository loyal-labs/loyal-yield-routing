package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

// EVM deposit address: GET and POST /api/funding/evm-deposit-address. Ports
// web/apps/web/src/app/api/funding/evm-deposit-address/route.ts and adds the
// external-wallet kind. A Privy-embedded Solana wallet gets a Privy deposit
// account that bridges every arrival to USDC on Solana; the user signs its
// creation once. An external Solana wallet's 0x address is the user's Privy
// embedded EVM wallet, and each bridge out of it is a Relay transaction the
// user signs.
var earnEvmDepositFamily = earnFamily{
	name:      "evm deposit address",
	appGrants: []Grant{TableGrant("app_users", "SELECT")},
	yieldGrants: []Grant{
		TableGrant("loyal_yield.evm_deposit_routes", "SELECT"),
		TableGrant("loyal_yield.evm_deposit_routes", "INSERT"),
	},
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Privy != nil && h.Privy.AppID != "" && h.Privy.AppSecret != ""
	},
	register: earnRoutes.registerEvmDeposit,
}

const (
	evmDepositPath       = "/api/funding/evm-deposit-address"
	evmDepositSignWindow = 10 * time.Minute
)

type EvmDepositKind string

const (
	PrivyAutobridge EvmDepositKind = "privy_autobridge"
	RelaySigned     EvmDepositKind = "relay_signed"
)

type evmDepositRoute struct {
	Kind    EvmDepositKind
	Address string
}

var evmAddress = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

type PrivyDepositAsset struct {
	Asset bridge.Asset `json:"asset"`
	Chain string       `json:"chain"`
}

type PrivyDepositSource struct {
	Mode   string              `json:"mode"`
	Values []PrivyDepositAsset `json:"values"`
}

type PrivyDepositBody struct {
	Type        string             `json:"type"`
	Source      PrivyDepositSource `json:"source"`
	Destination PrivyDepositAsset  `json:"destination"`
}

type PrivyDepositHeaders struct {
	AppID         string `json:"privy-app-id"`
	RequestExpiry string `json:"privy-request-expiry"`
}

type PrivyDepositRequest struct {
	Version int                 `json:"version"`
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Body    PrivyDepositBody    `json:"body"`
	Headers PrivyDepositHeaders `json:"headers"`
}

func (h earnRoutes) privyDepositRequest(walletID string, requestExpiryMs int64) PrivyDepositRequest {
	var sources []PrivyDepositAsset
	for _, network := range bridge.Networks {
		for _, token := range network.Tokens {
			sources = append(sources, PrivyDepositAsset{Asset: token.Asset, Chain: string(network.Chain)})
		}
	}
	return PrivyDepositRequest{
		Version: 1,
		Method:  http.MethodPost,
		URL:     privyDepositRoutesURL(walletID),
		Body: PrivyDepositBody{
			Type:        "inline_route",
			Source:      PrivyDepositSource{Mode: "include", Values: sources},
			Destination: PrivyDepositAsset{Asset: bridge.USDC, Chain: "solana"},
		},
		Headers: PrivyDepositHeaders{AppID: h.Privy.AppID, RequestExpiry: strconv.FormatInt(requestExpiryMs, 10)},
	}
}

type EvmDepositAddressResponse struct {
	Address       *string              `json:"address"`
	Kind          EvmDepositKind       `json:"kind" enum:"privy_autobridge,relay_signed"`
	ToSign        *PrivyDepositRequest `json:"toSign,omitempty"`
	RequestExpiry *int64               `json:"requestExpiry,omitempty"`
	Chains        []bridge.Chain       `json:"chains"`
	Assets        []bridge.Asset       `json:"assets"`
	Minimums      map[string]int       `json:"minimums"`
}

func evmDepositResponse(kind EvmDepositKind, address *string) *evmDepositOutput {
	out := EvmDepositAddressResponse{Address: address, Kind: kind, Minimums: map[string]int{}}
	seen := map[bridge.Asset]bool{}
	for _, network := range bridge.Networks {
		out.Chains = append(out.Chains, network.Chain)
		if network.MinUSD > 0 {
			out.Minimums[string(network.Chain)] = network.MinUSD
		}
		for _, token := range network.Tokens {
			if !seen[token.Asset] {
				seen[token.Asset] = true
				out.Assets = append(out.Assets, token.Asset)
			}
		}
	}
	return &evmDepositOutput{Body: out}
}

func evmDepositFound(route evmDepositRoute) *evmDepositOutput {
	return evmDepositResponse(route.Kind, &route.Address)
}

type EvmDepositAddressRequest struct {
	Signature          string `json:"signature,omitempty"`
	RequestExpiry      int64  `json:"requestExpiry,omitempty"`
	PrivyIdentityToken string `json:"privyIdentityToken,omitempty"`
}

type evmDepositInput struct {
	SessionInput
	Raw rawJSON
}

type evmDepositOutput struct {
	Body EvmDepositAddressResponse
}

func (h earnRoutes) registerEvmDeposit(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-funding-evm-deposit-address",
		Method:      http.MethodGet,
		Path:        evmDepositPath,
		Summary:     "The signed-in wallet's 0x deposit address, or what it takes to get one",
		Responses:   errorResponses[routeError](api, 401, 403, 409, 503),
	}, h.evmDepositAddress)
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-evm-deposit-address",
		Method:      http.MethodPost,
		Path:        evmDepositPath,
		Summary:     "Create the signed-in wallet's 0x deposit address",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 409, 502, 503),
	}, h.createEvmDepositAddress)
	documentJSONBody[EvmDepositAddressRequest](api, evmDepositPath)
}

func evmDepositUnavailable(step string, err error) error {
	return earnUnavailable(step, err, "evm_deposit_unavailable", "Deposits from other chains are unavailable. Please try again.")
}

func (h earnRoutes) evmDepositWallet(in SessionInput) (solana.PublicKey, error) {
	session, err := h.auth.Session(in)
	if refusal, ok := authRefusal(err); ok {
		return solana.PublicKey{}, refusal
	}
	if err != nil {
		return solana.PublicKey{}, evmDepositUnavailable("evm deposit session", err)
	}
	return session.Wallet, nil
}

func (h earnRoutes) evmDepositAddress(ctx context.Context, in *sessionRequest) (*evmDepositOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	route, err := h.storedEvmDepositRoute(ctx, wallet)
	if err != nil {
		return nil, evmDepositUnavailable("evm deposit read", err)
	}
	if route != nil {
		return evmDepositFound(*route), nil
	}
	walletID, err := h.privyWalletID(ctx, "solana", wallet.String())
	if err != nil {
		return nil, evmDepositUnavailable("privy wallet lookup", err)
	}
	if walletID == "" {
		return evmDepositResponse(RelaySigned, nil), nil
	}
	existing, err := h.privyDepositRoutes(ctx, walletID)
	if err != nil {
		return nil, evmDepositUnavailable("privy deposit routes", err)
	}
	if address, ok := evmDepositAddressOf(existing); ok {
		userID, err := h.evmDepositAppUser(ctx, wallet)
		if err != nil {
			return nil, err
		}
		return h.storeEvmDepositRoute(ctx, wallet, userID, evmDepositRoute{Kind: PrivyAutobridge, Address: address})
	}
	expiry := time.Now().Add(evmDepositSignWindow).UnixMilli()
	toSign := h.privyDepositRequest(walletID, expiry)
	out := evmDepositResponse(PrivyAutobridge, nil)
	out.Body.ToSign, out.Body.RequestExpiry = &toSign, &expiry
	return out, nil
}

func (h earnRoutes) createEvmDepositAddress(ctx context.Context, in *evmDepositInput) (*evmDepositOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	claim, err := parseEvmDepositClaim(in.Raw.data)
	if err != nil {
		return nil, err
	}
	route, err := h.storedEvmDepositRoute(ctx, wallet)
	if err != nil {
		return nil, evmDepositUnavailable("evm deposit read", err)
	}
	if route != nil {
		return evmDepositFound(*route), nil
	}
	userID, err := h.evmDepositAppUser(ctx, wallet)
	if err != nil {
		return nil, err
	}
	if claim.PrivyIdentityToken != "" {
		return h.createRelaySignedRoute(ctx, wallet, userID, claim.PrivyIdentityToken)
	}
	return h.createAutobridgeRoute(ctx, wallet, userID, claim.Signature, claim.RequestExpiry)
}

// evmDepositAppUser runs before any Privy call, so a wallet with no app user
// is refused before Privy creates an account the insert could not record.
func (h earnRoutes) evmDepositAppUser(ctx context.Context, wallet solana.PublicKey) (string, error) {
	userID, err := h.appUserID(ctx, wallet)
	if refusal, ok := authRefusal(err); ok {
		return "", refusal
	}
	if err != nil {
		return "", evmDepositUnavailable("evm deposit app user", err)
	}
	return userID, nil
}

func parseEvmDepositClaim(raw []byte) (EvmDepositAddressRequest, error) {
	var body struct {
		Signature          *string `json:"signature"`
		RequestExpiry      *int64  `json:"requestExpiry"`
		PrivyIdentityToken *string `json:"privyIdentityToken"`
	}
	invalid := earnError(http.StatusBadRequest, "invalid_body", "signature or privyIdentityToken required.")
	if json.Unmarshal(raw, &body) != nil {
		return EvmDepositAddressRequest{}, invalid
	}
	signed := body.Signature != nil && *body.Signature != "" && body.RequestExpiry != nil && *body.RequestExpiry > 0
	identified := body.PrivyIdentityToken != nil && *body.PrivyIdentityToken != ""
	switch {
	case signed && body.PrivyIdentityToken == nil:
		return EvmDepositAddressRequest{Signature: *body.Signature, RequestExpiry: *body.RequestExpiry}, nil
	case identified && body.Signature == nil && body.RequestExpiry == nil:
		return EvmDepositAddressRequest{PrivyIdentityToken: *body.PrivyIdentityToken}, nil
	}
	return EvmDepositAddressRequest{}, invalid
}

func (h earnRoutes) createAutobridgeRoute(ctx context.Context, wallet solana.PublicKey, userID, signature string, requestExpiryMs int64) (*evmDepositOutput, error) {
	if requestExpiryMs <= time.Now().UnixMilli() {
		return nil, earnError(http.StatusBadRequest, "evm_deposit_signature_expired", "Signature expired, try again.")
	}
	walletID, err := h.privyWalletID(ctx, "solana", wallet.String())
	if err != nil {
		return nil, evmDepositUnavailable("privy wallet lookup", err)
	}
	if walletID == "" {
		return nil, earnError(http.StatusConflict, "evm_deposit_external_wallet", "Deposits from other chains need a Loyal-created wallet.")
	}
	created, err := h.privyCreateDepositRoutes(ctx, h.privyDepositRequest(walletID, requestExpiryMs), signature)
	var refused *privyRefused
	if errors.As(err, &refused) {
		slog.Warn("privy refused evm deposit account", "status", refused.status, "body", refused.body, "wallet", wallet.String())
		return nil, earnError(http.StatusConflict, "evm_deposit_refused", "Could not create your deposit address. Please try again.")
	}
	if err != nil {
		return nil, evmDepositUnavailable("privy deposit create", err)
	}
	address, ok := evmDepositAddressOf(created)
	if !ok {
		return nil, earnError(http.StatusBadGateway, "evm_deposit_unavailable", "Privy returned no EVM deposit address.")
	}
	return h.storeEvmDepositRoute(ctx, wallet, userID, evmDepositRoute{Kind: PrivyAutobridge, Address: address})
}

func (h earnRoutes) createRelaySignedRoute(ctx context.Context, wallet solana.PublicKey, userID, identityToken string) (*evmDepositOutput, error) {
	identity, err := h.verifyPrivyIdentityToken(ctx, identityToken)
	if err != nil {
		return nil, err
	}
	var linked, embedded bool
	for _, w := range identity.Wallets {
		if w.Address == wallet.String() {
			linked, embedded = true, embedded || w.Embedded
		}
	}
	if !linked {
		return nil, earnError(http.StatusForbidden, "privy_wallet_not_linked", "Wallet is not linked to the authenticated Privy user.")
	}
	if embedded {
		return nil, earnError(http.StatusConflict, "evm_deposit_privy_wallet", "This wallet takes deposits from other chains through Privy auto-bridge.")
	}
	for _, address := range identity.EmbeddedEVMWallets {
		if address = strings.ToLower(address); evmAddress.MatchString(address) {
			return h.storeEvmDepositRoute(ctx, wallet, userID, evmDepositRoute{Kind: RelaySigned, Address: address})
		}
	}
	return nil, earnError(http.StatusConflict, "evm_wallet_missing", "Your Privy account has no embedded EVM wallet yet.")
}

func evmDepositAddressOf(routes []privyDepositAddressRoute) (string, bool) {
	for _, route := range routes {
		if address := strings.ToLower(route.DepositAddress); evmAddress.MatchString(address) {
			return address, true
		}
	}
	return "", false
}

func (h earnRoutes) storedEvmDepositRoute(ctx context.Context, wallet solana.PublicKey) (*evmDepositRoute, error) {
	var route evmDepositRoute
	err := h.YieldDB.QueryRow(ctx, `SELECT kind, evm_address FROM loyal_yield.evm_deposit_routes WHERE solana_wallet = $1`,
		wallet.String()).Scan(&route.Kind, &route.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (h earnRoutes) storeEvmDepositRoute(ctx context.Context, wallet solana.PublicKey, userID string, route evmDepositRoute) (*evmDepositOutput, error) {
	if _, err := h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.evm_deposit_routes (solana_wallet, app_user_id, kind, evm_address)
VALUES ($1, $2::text::uuid, $3, $4)
ON CONFLICT DO NOTHING`, wallet.String(), userID, string(route.Kind), route.Address); err != nil {
		return nil, evmDepositUnavailable("evm deposit insert", err)
	}
	stored, err := h.storedEvmDepositRoute(ctx, wallet)
	if err != nil {
		return nil, evmDepositUnavailable("evm deposit read", err)
	}
	if stored == nil {
		slog.Warn("evm deposit address already routes to another wallet", "wallet", wallet.String(), "address", route.Address)
		return nil, earnError(http.StatusConflict, "evm_deposit_address_in_use", "This deposit address already belongs to another wallet.")
	}
	return evmDepositFound(*stored), nil
}
