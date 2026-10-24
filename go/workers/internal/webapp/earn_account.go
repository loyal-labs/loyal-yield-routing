package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const earnVaultIndex uint8 = 1

var EarnPolicyGrants = []Grant{
	TableGrant("loyal_yield.managed_vaults", "SELECT"),
	TableGrant("loyal_yield.route_policies", "SELECT"),
}

type LoyalCluster string

const (
	LoyalMainnetBeta LoyalCluster = "mainnet-beta"
	LoyalDevnet      LoyalCluster = "devnet"
)

func (c SolanaCluster) Loyal() LoyalCluster {
	if c == Devnet {
		return LoyalDevnet
	}
	return LoyalMainnetBeta
}

type EarnAccount struct {
	Principal
	Vault   solana.PublicKey
	Cluster LoyalCluster
}

func earnError(status int, code, message string) error {
	return &routeError{Detail: EarnMaxErrorDetail{Code: code, Message: message}, status: status}
}

func earnUnavailable(step string, err error, code, message string) error {
	var refused *routeError
	if errors.As(err, &refused) {
		return refused
	}
	slog.Warn("earn route unavailable", "step", step, "code", code, "error", err)
	return earnError(http.StatusServiceUnavailable, code, message)
}

func (h earnRoutes) signedIn() bool { return h.auth != nil && h.appDB != nil }

func (h earnRoutes) account(ctx context.Context, in SessionInput) (EarnAccount, error) {
	p, err := h.auth.Principal(ctx, in)
	if refusal, ok := authRefusal(err); ok {
		return EarnAccount{}, refusal
	}
	if err != nil {
		return EarnAccount{}, err
	}
	vault, _, err := squads.SmartAccountAddress(p.Settings, earnVaultIndex)
	if err != nil {
		return EarnAccount{}, err
	}
	return EarnAccount{Principal: p, Vault: vault, Cluster: h.Cluster.Loyal()}, nil
}

func (h earnRoutes) appUserID(ctx context.Context, wallet solana.PublicKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id string
	err := h.appDB.QueryRow(ctx, `SELECT id::text FROM app_users WHERE provider = 'solana' AND subject_address = $1`, wallet.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse(SmartAccountRequired, nil)
	}
	return id, err
}

type RoutePolicy struct {
	ID                   int64
	Cluster              *string
	PolicySeed           int64
	PolicyAccount        string
	DelegatedSigners     []string
	Threshold            int32
	RouteModes           []string
	StableMints          []string
	KaminoMarkets        []string
	KaminoLiquidityMints []string
	UniversePreset       *string
	RiskProfile          *string
	SwapLanes            json.RawMessage
	FirstSeenAt          time.Time
	LastSeenAt           time.Time
	LastSeenSlot         int64
	LastSeenSignature    string
}

type RoutePolicyPair struct {
	Route RoutePolicy
	Setup *RoutePolicy
}

func (h earnRoutes) activePolicyPair(ctx context.Context, account EarnAccount) (*RoutePolicyPair, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `WITH vault AS (
  SELECT vault_pubkey, active_policy_id, setup_policy_id
  FROM loyal_yield.managed_vaults
  WHERE active AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3
  ORDER BY last_seen_at DESC, id DESC
  LIMIT 1
)
SELECT p.id = vault.active_policy_id, p.id IS NOT DISTINCT FROM vault.setup_policy_id,
  p.id, p.cluster, p.policy_seed, p.policy_account, p.delegated_signers, p.threshold, p.route_modes,
  p.stable_mints, p.kamino_markets, p.kamino_liquidity_mints, p.universe_preset, p.risk_profile,
  p.swap_lanes, p.first_seen_at, p.last_seen_at, p.last_seen_slot, p.last_seen_signature
FROM vault
JOIN loyal_yield.route_policies p ON p.id IN (vault.active_policy_id, vault.setup_policy_id)
WHERE p.active AND p.authority = $4 AND p.settings = $1 AND p.vault_index = $2 AND p.vault_pubkey = vault.vault_pubkey`,
		account.Settings.String(), int16(earnVaultIndex), account.Vault.String(), account.Wallet.String())
	if err != nil {
		return nil, fmt.Errorf("read active route policy pair: %w", err)
	}
	defer rows.Close()
	var route, setup *RoutePolicy
	for rows.Next() {
		var isRoute, isSetup bool
		var p RoutePolicy
		if err := rows.Scan(&isRoute, &isSetup, &p.ID, &p.Cluster, &p.PolicySeed, &p.PolicyAccount, &p.DelegatedSigners, &p.Threshold, &p.RouteModes,
			&p.StableMints, &p.KaminoMarkets, &p.KaminoLiquidityMints, &p.UniversePreset, &p.RiskProfile,
			&p.SwapLanes, &p.FirstSeenAt, &p.LastSeenAt, &p.LastSeenSlot, &p.LastSeenSignature); err != nil {
			return nil, fmt.Errorf("read active route policy pair: %w", err)
		}
		if isRoute {
			route = &p
		}
		if isSetup {
			setup = &p
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active route policy pair: %w", err)
	}
	if route == nil {
		return nil, nil
	}
	return &RoutePolicyPair{Route: *route, Setup: setup}, nil
}
