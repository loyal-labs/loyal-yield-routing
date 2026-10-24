package webapp

// The earn-state family serves the wallet-state reads and the position
// reconciliation write:
//
//	GET  /api/smart-accounts/yield-optimization/earn-state
//	GET  /api/smart-accounts/yield-optimization/position
//	POST /api/smart-accounts/yield-optimization/position/reconcile
//	POST /api/smart-accounts/yield-optimization/realtime/token
//
// <- web/apps/web/src/app/api/smart-accounts/yield-optimization/{earn-state,
// position, position/reconcile, realtime/token}/route.ts (mobile twins
// .../mobile/earn/state and .../mobile/earn/autodeposit/state). Earn-state
// runs five independent
// loads, each degraded into loadErrors + null on failure exactly like
// loadEarnStatePart; the position load also runs the reconcile writers, so
// this is a read family with a bounded write blast radius.

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

var earnStateYieldGrants = []Grant{
	{"loyal_yield.managed_vaults", "SELECT"},
	{"loyal_yield.managed_vaults", "UPDATE"},
	{"loyal_yield.route_policies", "SELECT"},
	{"loyal_yield.user_yield_positions", "SELECT"},
	{"loyal_yield.user_yield_positions", "UPDATE"},
	{"loyal_yield.user_yield_position_holding_events", "SELECT"},
	{"loyal_yield.user_yield_position_holding_events", "INSERT"},
	{"loyal_yield.vault_position_snapshots", "INSERT"},
	{"loyal_yield.vault_position_snapshots", "UPDATE"},
	{"loyal_yield.vault_position_snapshot_positions", "SELECT"},
	{"loyal_yield.vault_position_snapshot_positions", "INSERT"},
	{"loyal_yield.vault_reserve_positions_current", "SELECT"},
	{"loyal_yield.vault_reserve_positions_current", "DELETE"},
	{"loyal_yield.vault_reserve_positions_current", "INSERT"},
	{"loyal_yield.vault_idle_token_balances_current", "SELECT"},
	{"loyal_yield.vault_idle_token_balances_current", "DELETE"},
	{"loyal_yield.vault_idle_token_balances_current", "INSERT"},
	{"loyal_yield.rebalance_decisions", "SELECT"},
	{"loyal_yield.earn_deposit_onboarding_attempts", "SELECT"},
	{"loyal_yield.balance_sweep_targets", "SELECT"},
	{"loyal_yield.balance_sweep_policies", "SELECT"},
	{"loyal_yield.balance_sweep_executions", "SELECT"},
	{"loyal_yield.balance_sweep_scheduled_slots", "SELECT"},
	{"loyal_yield.balance_sweep_surplus_lots", "SELECT"},
	{"loyal_yield.balance_sweep_lot_claims", "SELECT"},
	{"loyal_yield.cross_mint_swap_policies", "SELECT"},
	{"loyal_yield.cross_mint_vault_opt_ins", "SELECT"},
}

var earnStateFamily = earnFamily{
	name:        "earn state",
	yieldGrants: earnStateYieldGrants,
	ready:       func(h earnRoutes) bool { return h.signedIn() && h.YieldDB != nil && h.TimescaleURL != "" },
	register:    earnRoutes.registerState,
}

type earnStateOutput struct {
	// Next sets no explicit cache header on this route; no-store is pinned
	// here because every byte is per-wallet state.
	CacheControl string `header:"Cache-Control"`
	Body         EarnStateResponse
}

// stateLoad mirrors loadEarnStatePart: a failed part logs, sets its
// loadErrors key and falls back to nil. The data is captured by the closure.
func stateLoad(name string, failed *bool, load func() error) {
	if err := load(); err != nil {
		slog.Warn("earn-state failed to load part; returning null", "part", name, "error", err)
		*failed = true
	}
}

func (h earnRoutes) registerState(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-earn-state",
		Method:      http.MethodGet,
		Path:        "/api/smart-accounts/yield-optimization/earn-state",
		Summary:     "Complete Earn state for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 401, 409, 503),
	}, h.earnState)
	h.registerPosition(api)
	h.registerReconcile(api)
	h.registerRealtime(api)
}

// stateCurrentTotalAmountRaw ports loadCurrentTotalAmountRaw: the sum of the
// vault's user-facing reserve rows and all idle balances, falling back to the
// stored position holding when it is zero.
func (h earnRoutes) stateCurrentTotalAmountRaw(ctx context.Context, settings, wallet, vaultPubkey string) (int64, error) {
	reserveRows, err := h.stateCurrentReserveRows(ctx, settings, wallet, vaultPubkey)
	if err != nil {
		return 0, err
	}
	idleRows, err := h.stateCurrentIdleRows(ctx, settings, wallet, vaultPubkey)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, row := range reserveRows {
		total += row.AmountRaw
	}
	for _, row := range idleRows {
		total += row.AmountRaw
	}
	return total, nil
}

func (h earnRoutes) earnState(ctx context.Context, in *sessionRequest) (*earnStateOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn-state auth", err, "unauthenticated", "No active auth session.")
	}
	settings := account.Settings.String()
	wallet := account.Wallet.String()
	vaultPubkey := account.Vault.String()

	var positionFailed, policyFailed, onboardingFailed, autodepositFailed, autoswapFailed bool

	var position *stateYieldPositionRow
	stateLoad("position", &positionFailed, func() error {
		var err error
		position, err = h.reconciledActiveYieldPosition(ctx, account)
		return err
	})

	var pair *RoutePolicyPair
	stateLoad("policy", &policyFailed, func() error {
		var err error
		pair, err = h.activePolicyPair(ctx, account)
		return err
	})

	var attempt *stateOnboardingAttemptRow
	stateLoad("onboarding", &onboardingFailed, func() error {
		var err error
		attempt, err = h.stateOnboardingAttempt(ctx, settings, wallet, vaultPubkey)
		return err
	})

	var autodeposit *EarnStateAutodeposit
	stateLoad("autodeposit", &autodepositFailed, func() error {
		target, policy, err := h.stateAutodepositState(ctx, settings, wallet)
		if err != nil || target == nil {
			return err
		}
		deposited, err := h.statePeriodDepositSum(ctx, target)
		if err != nil {
			return err
		}
		sweeps := []statePendingSweepRow{}
		if stateAutodepositStatus(target.Active, target.ChainStatus) == "active" {
			sweeps, err = h.statePendingSweeps(ctx, target)
			if err != nil {
				return err
			}
		}
		serialized := serializeEarnStateAutodeposit(*target, policy, deposited, sweeps)
		autodeposit = &serialized
		return nil
	})

	var autoswap *EarnStateAutoswap
	var autoswapIndex *EarnStateAutoswapIndex
	stateLoad("autoswap", &autoswapFailed, func() error {
		var err error
		autoswap, autoswapIndex, err = h.stateCrossMintSnapshot(ctx, string(account.Cluster), settings, wallet, vaultPubkey)
		return err
	})

	var positionOut *EarnStatePosition
	totalRaw := int64(0)
	if position != nil {
		totalRaw, err = h.stateCurrentTotalAmountRaw(ctx, settings, wallet, position.VaultPubkey)
		if err != nil {
			slog.Warn("earn-state failed to load current holdings total", "error", err)
			totalRaw = position.CurrentAmountRaw
		}
		if totalRaw <= 0 {
			totalRaw = position.CurrentAmountRaw
		}
		serialized := serializeEarnStatePosition(*position, totalRaw)
		positionOut = &serialized
	}

	loadErrors := map[string]bool{}
	for name, failed := range map[string]bool{
		"position": positionFailed, "policy": policyFailed,
		"onboarding": onboardingFailed, "autodeposit": autodepositFailed,
		"autoswap": autoswapFailed,
	} {
		if failed {
			loadErrors[name] = true
		}
	}

	canEnroll := false
	if raw := os.Getenv("EARN_AUTOSWAP_ENABLED_WALLETS"); raw != "" {
		enabled, envErr := earnAutoswapEnrollmentEnabled(raw, wallet)
		if envErr != nil {
			slog.Warn("earn-state autoswap enrollment configuration is invalid", "error", envErr)
		}
		canEnroll = enabled
	}

	canonical, _, err := squads.SmartAccountAddress(account.Settings, 0)
	if err != nil {
		return nil, earnUnavailable("earn-state canonical vault", err, "state_unavailable", "Earn state is unavailable.")
	}

	var policyOut *EarnStatePolicy
	if pair != nil {
		policyOut = serializeEarnStatePolicy(pair.Route, pair.Setup, int(earnVaultIndex), vaultPubkey)
	}
	nextStep := stateOnboardingNextStep(attempt, position != nil, pair != nil, pair != nil && pair.Setup != nil)

	return &earnStateOutput{
		CacheControl: "no-store",
		Body: EarnStateResponse{
			Autodeposit:           autodeposit,
			Autoswap:              autoswap,
			AutoswapIndex:         autoswapIndex,
			AutoswapAvailable:     autoswap != nil || (canEnroll && position != nil),
			CanonicalVaultPubkey:  canonical.String(),
			LoadErrors:            loadErrors,
			Onboarding:            serializeEarnStateOnboarding(attempt, nextStep),
			Policy:                policyOut,
			Position:              positionOut,
			PolicySignerPublicKey: h.PolicySigner.String(),
			ProgramID:             squads.ProgramID.String(),
			SettingsPda:           settings,
			Vault:                 EarnStateVault{AccountIndex: int(earnVaultIndex), Pubkey: vaultPubkey},
		},
	}, nil
}
