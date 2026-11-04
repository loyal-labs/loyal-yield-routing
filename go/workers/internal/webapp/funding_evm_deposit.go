package webapp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
		TableGrant("loyal_yield.relay_deposit_addresses", "SELECT"),
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

type DepositRouteKind string

const (
	EVMRoute                 DepositRouteKind = "evm"
	RelayDepositAddressRoute DepositRouteKind = "relay_deposit_address"
)

type evmDepositRoute struct {
	Kind    EvmDepositKind
	Address string
	Set     bridge.RouteSet
}

type walletLocks struct {
	mu   sync.Mutex
	held map[solana.PublicKey]*walletLock
}

type walletLock struct {
	turn chan struct{}
	refs int
}

var evmDepositCreates = walletLocks{held: map[solana.PublicKey]*walletLock{}}

func (l *walletLocks) lock(ctx context.Context, wallet solana.PublicKey) (unlock func(), err error) {
	l.mu.Lock()
	entry := l.held[wallet]
	if entry == nil {
		entry = &walletLock{turn: make(chan struct{}, 1)}
		l.held[wallet] = entry
	}
	entry.refs++
	l.mu.Unlock()
	select {
	case entry.turn <- struct{}{}:
		return func() {
			<-entry.turn
			l.release(wallet, entry)
		}, nil
	case <-ctx.Done():
		l.release(wallet, entry)
		return nil, ctx.Err()
	}
}

func (l *walletLocks) release(wallet solana.PublicKey, entry *walletLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(l.held, wallet)
	}
}

var evmAddress = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

type PrivyDepositAsset struct {
	Asset string `json:"asset"`
	Chain string `json:"chain"`
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

var privyDepositDestination = PrivyDepositAsset{Asset: "usdc", Chain: "solana"}

// privyLegacy13Sources is the source list every legacy-13 account holds on
// Privy, read back from all six on 2026-11-02.
var privyLegacy13Sources = []PrivyDepositAsset{
	{"usdc", "ethereum"}, {"usdt", "ethereum"}, {"eth", "ethereum"},
	{"usdc", "base"}, {"usdt", "base"}, {"eth", "base"},
	{"usdc", "arbitrum"}, {"usdt", "arbitrum"}, {"eth", "arbitrum"},
	{"usdc", "polygon"}, {"usdt", "polygon"},
	{"usdc", "bsc"}, {"usdt", "bsc"},
}

// privySources names tokens by contract address, since Privy refuses its own
// aliases for some (chain, asset) pairs, such as usdc on Tempo.
func privySources(set bridge.RouteSet) []PrivyDepositAsset {
	if set == bridge.Legacy13 {
		return privyLegacy13Sources
	}
	var sources []PrivyDepositAsset
	for _, network := range set.Networks() {
		for _, token := range network.Tokens {
			sources = append(sources, PrivyDepositAsset{Asset: token.Address, Chain: string(network.Chain)})
		}
	}
	return sources
}

func privyRouteSet(source PrivyDepositSource) (bridge.RouteSet, bool) {
	if source.Mode != "include" {
		return "", false
	}
	held := map[PrivyDepositAsset]bool{}
	for _, v := range source.Values {
		held[PrivyDepositAsset{Asset: strings.ToLower(v.Asset), Chain: strings.ToLower(v.Chain)}] = true
	}
	for _, set := range bridge.RouteSets {
		sources := privySources(set)
		missing := slices.ContainsFunc(sources, func(s PrivyDepositAsset) bool { return !held[s] })
		if len(held) == len(sources) && !missing {
			return set, true
		}
	}
	return "", false
}

func (h earnRoutes) privyDepositRequest(walletID string, requestExpiryMs int64) PrivyDepositRequest {
	return PrivyDepositRequest{
		Version: 1,
		Method:  http.MethodPost,
		URL:     privyDepositRoutesURL(walletID),
		Body: PrivyDepositBody{
			Type:        "inline_route",
			Source:      PrivyDepositSource{Mode: "include", Values: privySources(bridge.Current)},
			Destination: privyDepositDestination,
		},
		Headers: PrivyDepositHeaders{AppID: h.Privy.AppID, RequestExpiry: strconv.FormatInt(requestExpiryMs, 10)},
	}
}

type EvmDepositToken struct {
	Asset       bridge.Asset `json:"asset"`
	Symbol      string       `json:"symbol"`
	Name        string       `json:"name"`
	Decimals    uint8        `json:"decimals"`
	Stable      bool         `json:"stable"`
	Icon        *string      `json:"icon" doc:"Image file in the receive sheet's assets; null while there is none"`
	FeePercent  *string      `json:"feePercent" doc:"Privy auto-bridge cost as a percent of a 100-token deposit, from Privy's deposit quote; null for relay_signed or when the quote failed"`
	FeeUSD      *string      `json:"feeUsd" doc:"Relay's fee in USD on a minimum deposit, for relay_deposit_address entries; null otherwise or until a quote has answered"`
	ReceiveUSDC *string      `json:"receiveUsdc" doc:"USDC on Solana a minimum deposit pays, from the same quote as feeUsd; null otherwise or until a quote has answered"`
}

type EvmDepositNetwork struct {
	Route     DepositRouteKind  `json:"route" enum:"evm,relay_deposit_address" doc:"How address is obtained: evm entries share the wallet's 0x deposit address; a relay_deposit_address entry's address is minted by POST /api/funding/tron-deposit-address"`
	Address   *string           `json:"address" doc:"This entry's deposit address; null until it exists"`
	Chain     bridge.Chain      `json:"chain"`
	Name      string            `json:"name"`
	ShortName string            `json:"shortName" doc:"The chain's name inside a sentence, as in the Ethereum network"`
	Icon      *string           `json:"icon" doc:"Image file in the receive sheet's assets; null while there is none"`
	QRIcon    *string           `json:"qrIcon" doc:"Image file for the middle of the deposit QR; null while there is none"`
	Assets    []bridge.Asset    `json:"assets"`
	Tokens    []EvmDepositToken `json:"tokens"`
	MinUSD    int               `json:"minUsd" doc:"Smallest deposit worth bridging in USD; 0 means none"`
}

func iconFile(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

type EvmDepositAddressResponse struct {
	Address       *string              `json:"address"`
	Kind          EvmDepositKind       `json:"kind" enum:"privy_autobridge,relay_signed"`
	ToSign        *PrivyDepositRequest `json:"toSign,omitempty"`
	RequestExpiry *int64               `json:"requestExpiry,omitempty"`
	Networks      []EvmDepositNetwork  `json:"networks"`
	Chains        []bridge.Chain       `json:"chains"`
	Assets        []bridge.Asset       `json:"assets"`
	Minimums      map[string]int       `json:"minimums"`
}

func (h earnRoutes) evmDepositResponse(ctx context.Context, wallet solana.PublicKey, kind EvmDepositKind, address *string, set bridge.RouteSet) (*evmDepositOutput, error) {
	if address != nil {
		cased := bridge.ChecksumAddress(*address)
		address = &cased
	}
	networks := set.Networks()
	var fees map[depositCell]*string
	if kind == PrivyAutobridge {
		fees = h.privyDepositFees(networks)
	}
	out := EvmDepositAddressResponse{Address: address, Kind: kind, Networks: []EvmDepositNetwork{}, Minimums: map[string]int{}}
	seen := map[bridge.Asset]bool{}
	for _, network := range networks {
		item := EvmDepositNetwork{Route: EVMRoute, Address: address, Chain: network.Chain, Name: network.Name, ShortName: network.Short, Icon: iconFile(network.Icon), QRIcon: iconFile(cmp.Or(network.QRIcon, network.Icon)), MinUSD: network.MinUSD}
		out.Chains = append(out.Chains, network.Chain)
		if network.MinUSD > 0 {
			out.Minimums[string(network.Chain)] = network.MinUSD
		}
		for _, token := range network.Tokens {
			info := bridge.Assets[token.Asset]
			item.Assets = append(item.Assets, token.Asset)
			item.Tokens = append(item.Tokens, EvmDepositToken{
				Asset:      token.Asset,
				Symbol:     info.Symbol,
				Name:       info.Name,
				Decimals:   token.Decimals,
				Stable:     info.Stable,
				Icon:       iconFile(info.Icon),
				FeePercent: fees[depositCell{network.Chain, token.Asset}],
			})
			if !seen[token.Asset] {
				seen[token.Asset] = true
				out.Assets = append(out.Assets, token.Asset)
			}
		}
		out.Networks = append(out.Networks, item)
	}
	tron := h.tronDepositNetwork(ctx, wallet)
	at := slices.IndexFunc(out.Networks, func(n EvmDepositNetwork) bool { return n.Chain == bridge.Ethereum }) + 1
	out.Networks = slices.Insert(out.Networks, at, tron)
	return &evmDepositOutput{Body: out}, nil
}

// chains, assets and minimums exclude Tron because the prod client reads them
// with the shared 0x address; W1 and T1c ship in one deploy.
func (h earnRoutes) tronDepositNetwork(ctx context.Context, wallet solana.PublicKey) EvmDepositNetwork {
	route := bridge.Tron
	item := EvmDepositNetwork{Route: RelayDepositAddressRoute, Chain: route.Chain, Name: route.Name, ShortName: route.Short, Icon: iconFile(route.Icon), QRIcon: iconFile(route.Icon), MinUSD: route.MinUSD, Assets: []bridge.Asset{route.Asset}}
	row, err := h.storedTronDepositAddress(ctx, wallet)
	if err != nil {
		slog.Warn("tron deposit address read failed", "wallet", wallet.String(), "err", err)
	}
	if row != nil {
		item.Address = &row.Address
	}
	info := bridge.Assets[route.Asset]
	price := h.tronDepositPrice(wallet)
	item.Tokens = []EvmDepositToken{{
		Asset:       route.Asset,
		Symbol:      info.Symbol,
		Name:        info.Name,
		Decimals:    route.Decimals,
		Stable:      info.Stable,
		Icon:        iconFile(info.Icon),
		FeeUSD:      price.FeeUSD,
		ReceiveUSDC: price.ReceiveUSDC,
	}}
	return item
}

func (h earnRoutes) evmDepositFound(ctx context.Context, wallet solana.PublicKey, route evmDepositRoute) (*evmDepositOutput, error) {
	return h.evmDepositResponse(ctx, wallet, route.Kind, &route.Address, route.Set)
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
		return h.evmDepositFound(ctx, wallet, *route)
	}
	walletID, err := h.privyWalletID(ctx, "solana", wallet.String())
	if err != nil {
		return nil, evmDepositUnavailable("privy wallet lookup", err)
	}
	if walletID == "" {
		return h.evmDepositResponse(ctx, wallet, RelaySigned, nil, bridge.Current)
	}
	existing, err := h.privyDepositRoutes(ctx, walletID)
	if err != nil {
		return nil, evmDepositUnavailable("privy deposit routes", err)
	}
	if account, ok := evmDepositAccountOf(existing); ok {
		userID, err := h.evmDepositAppUser(ctx, wallet)
		if err != nil {
			return nil, err
		}
		return h.adoptPrivyDepositAccount(ctx, wallet, userID, account)
	}
	expiry := time.Now().Add(evmDepositSignWindow).UnixMilli()
	toSign := h.privyDepositRequest(walletID, expiry)
	out, err := h.evmDepositResponse(ctx, wallet, PrivyAutobridge, nil, bridge.Current)
	if err != nil {
		return nil, err
	}
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
		return h.evmDepositFound(ctx, wallet, *route)
	}
	userID, err := h.evmDepositAppUser(ctx, wallet)
	if err != nil {
		return nil, err
	}
	if claim.PrivyIdentityToken != "" {
		return h.createRelaySignedRoute(ctx, wallet, userID, claim.PrivyIdentityToken)
	}
	if claim.RequestExpiry <= time.Now().UnixMilli() {
		return nil, earnError(http.StatusBadRequest, "evm_deposit_signature_expired", "Signature expired, try again.")
	}
	unlock, err := evmDepositCreates.lock(ctx, wallet)
	if err != nil {
		return nil, evmDepositUnavailable("evm deposit lock", err)
	}
	defer unlock()
	create, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelCreate()
	return h.createAutobridgeRoute(create, wallet, userID, claim.Signature, claim.RequestExpiry)
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
	route, err := h.storedEvmDepositRoute(ctx, wallet)
	if err != nil {
		return nil, evmDepositUnavailable("evm deposit read", err)
	}
	if route != nil {
		return h.evmDepositFound(ctx, wallet, *route)
	}
	walletID, err := h.privyWalletID(ctx, "solana", wallet.String())
	if err != nil {
		return nil, evmDepositUnavailable("privy wallet lookup", err)
	}
	if walletID == "" {
		return nil, earnError(http.StatusConflict, "evm_deposit_external_wallet", "Deposits from other chains need a Loyal-created wallet.")
	}
	existing, err := h.privyDepositRoutes(ctx, walletID)
	if err != nil {
		return nil, evmDepositUnavailable("privy deposit routes", err)
	}
	if account, ok := evmDepositAccountOf(existing); ok {
		return h.adoptPrivyDepositAccount(ctx, wallet, userID, account)
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
	account, ok := evmDepositAccountOf(created)
	if !ok {
		return nil, earnError(http.StatusBadGateway, "evm_deposit_unavailable", "Privy returned no EVM deposit address.")
	}
	return h.storeEvmDepositRoute(ctx, wallet, userID, evmDepositRoute{Kind: PrivyAutobridge, Address: account.DepositAddress, Set: bridge.Current})
}

// adoptPrivyDepositAccount records an account Privy already holds for the
// wallet, with the set its own source list names.
func (h earnRoutes) adoptPrivyDepositAccount(ctx context.Context, wallet solana.PublicKey, userID string, account privyDepositAddressRoute) (*evmDepositOutput, error) {
	set, ok := privyRouteSet(account.Source)
	if !ok {
		source, _ := json.Marshal(account.Source)
		return nil, evmDepositUnavailable("privy deposit account", fmt.Errorf("account %s converts an unknown source list %s", account.DepositAddress, source))
	}
	return h.storeEvmDepositRoute(ctx, wallet, userID, evmDepositRoute{Kind: PrivyAutobridge, Address: account.DepositAddress, Set: set})
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

func evmDepositAccountOf(routes []privyDepositAddressRoute) (privyDepositAddressRoute, bool) {
	for _, route := range routes {
		if address := strings.ToLower(route.DepositAddress); evmAddress.MatchString(address) {
			route.DepositAddress = address
			return route, true
		}
	}
	return privyDepositAddressRoute{}, false
}

func (h earnRoutes) storedEvmDepositRoute(ctx context.Context, wallet solana.PublicKey) (*evmDepositRoute, error) {
	var route evmDepositRoute
	var set *string
	err := h.YieldDB.QueryRow(ctx, `SELECT kind, evm_address, route_set FROM loyal_yield.evm_deposit_routes WHERE solana_wallet = $1`,
		wallet.String()).Scan(&route.Kind, &route.Address, &set)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case route.Kind == RelaySigned:
		route.Set = bridge.Current
	case set == nil:
		return nil, fmt.Errorf("evm deposit route of %s has no route set", wallet)
	default:
		var ok bool
		if route.Set, ok = bridge.ParseRouteSet(*set); !ok {
			return nil, fmt.Errorf("evm deposit route of %s has unknown route set %q", wallet, *set)
		}
	}
	return &route, nil
}

func (h earnRoutes) storeEvmDepositRoute(ctx context.Context, wallet solana.PublicKey, userID string, route evmDepositRoute) (*evmDepositOutput, error) {
	var set *string
	if route.Kind == PrivyAutobridge {
		name := string(route.Set)
		set = &name
	}
	if _, err := h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.evm_deposit_routes (solana_wallet, app_user_id, kind, evm_address, route_set)
VALUES ($1, $2::text::uuid, $3, $4, $5)
ON CONFLICT DO NOTHING`, wallet.String(), userID, string(route.Kind), route.Address, set); err != nil {
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
	return h.evmDepositFound(ctx, wallet, *stored)
}
