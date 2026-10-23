package webapp

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
)

// Ports web/apps/web/src/app/api/smart-accounts/yield-optimization/
// cross-mint/toggle/route.ts and cross-mint/delete/readiness/route.ts.

const (
	crossMintTogglePath           = "/api/smart-accounts/yield-optimization/cross-mint/toggle"
	crossMintDeleteReadinessPath  = "/api/smart-accounts/yield-optimization/cross-mint/delete/readiness"

	// route.ts:106-108 and :148 (toggle), :108-110 (delete readiness).
	crossMintNotInstalledMessage = "Autoswap is not installed."
	// route.ts:150-152 and :110-112.
	crossMintStaleMessage = "Autoswap state changed. Refresh and try again."
)

var crossMintDigitsOnly = regexp.MustCompile(`^\d+$`)

// parseCrossMintGeneration ports the shared generation check (toggle
// route.ts:50-57, delete readiness route.ts:46-59): a /^\d+$/ string greater
// than zero. A value past int64 can never match the bigint column, so the
// caller takes the stale-or-missing shortcut instead of sending it to
// Postgres.
func parseCrossMintGeneration(value any) (uint64, bool, bool) {
	text, isText := value.(string)
	if !isText || !crossMintDigitsOnly.MatchString(text) {
		return 0, false, false
	}
	generation, err := strconv.ParseUint(text, 10, 64)
	if err != nil || generation == 0 {
		return 0, false, false
	}
	if generation > uint64(int64(maxInt64)) {
		return generation, true, true
	}
	return generation, false, true
}

const maxInt64 = int64(^uint64(0) >> 1)

// parseCrossMintToggleBody ports parseRequest (toggle route.ts:45-62).
func parseCrossMintToggleBody(raw []byte) (enabled bool, generation uint64, overflow bool, err error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return false, 0, false, err
	}
	// !value || typeof value !== "object": null and scalars are required-input
	// refusals, an array is an object and falls into the field checks.
	record, isObject := parsed.(map[string]any)
	if parsed == nil || !isObject {
		return false, 0, false, earnError(http.StatusBadRequest, "invalid_request", "Autoswap toggle input is required.")
	}
	inputEnabled, isBool := record["enabled"].(bool)
	if !isBool {
		return false, 0, false, earnError(http.StatusBadRequest, "invalid_request", "Autoswap toggle input is invalid.")
	}
	generation, overflow, ok := parseCrossMintGeneration(record["expectedGeneration"])
	if !ok {
		return false, 0, false, earnError(http.StatusBadRequest, "invalid_request", "Autoswap toggle input is invalid.")
	}
	return inputEnabled, generation, overflow, nil
}

// parseCrossMintReadinessBody ports parseExpectedGeneration (delete readiness
// route.ts:46-59): everything invalid says the same sentence.
func parseCrossMintReadinessBody(raw []byte) (generation uint64, overflow bool, err error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return 0, false, err
	}
	record, _ := parsed.(map[string]any)
	generation, overflow, ok := parseCrossMintGeneration(record["expectedGeneration"])
	if !ok {
		return 0, false, earnError(http.StatusBadRequest, "invalid_request", "Autoswap deletion generation is invalid.")
	}
	return generation, overflow, nil
}

// crossMintTransition applies setEarnCrossMintEnabled and reads the fresh
// enrollment the TS function returns (repository.server.ts:223-245); nil
// enrollment is the missing outcome.
func crossMintTransition(ctx context.Context, h earnRoutes, scope crossMintScope, enabled bool, generation uint64, overflow bool) (*crossMintEnrollment, string, error) {
	var kind string
	var err error
	if overflow {
		kind, err = setEarnCrossMintEnabledHuge(ctx, h.YieldDB, scope)
	} else {
		kind, err = setEarnCrossMintEnabled(ctx, h.YieldDB, scope, enabled, int64(generation))
	}
	if err != nil {
		return nil, "", err
	}
	if kind == "missing" {
		return nil, kind, nil
	}
	enrollment, err := crossMintEnrollmentAfterTransition(ctx, h.YieldDB, scope)
	if err != nil {
		return nil, "", err
	}
	if enrollment == nil {
		return nil, "missing", nil
	}
	return enrollment, kind, nil
}

// CrossMintToggleRequest documents the request body for OpenAPI; the handler
// parses the raw bytes so the TS error messages carry over verbatim.
type CrossMintToggleRequest struct {
	Enabled            *bool   `json:"enabled,omitempty"`
	ExpectedGeneration *string `json:"expectedGeneration,omitempty"`
}

// CrossMintToggleResponse mirrors the toggle route's body (route.ts:153-157).
type CrossMintToggleResponse struct {
	Enabled    bool   `json:"enabled"`
	Generation string `json:"generation"`
	Status     string `json:"status"`
}

type crossMintToggleInput struct {
	SessionInput
	Raw rawJSON
}

type crossMintToggleOutput struct {
	Body CrossMintToggleResponse
}

func (h earnRoutes) earnCrossMintToggle(ctx context.Context, in *crossMintToggleInput) (*crossMintToggleOutput, error) {
	// Next wraps everything below the env gate in one catch that answers 409
	// autoswap_toggle_failed with error.message (toggle route.ts:158-164).
	toggleFailed := func(err error) error {
		return earnError(http.StatusConflict, "autoswap_toggle_failed", err.Error())
	}

	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("principal", err, "earn_cross_mint_unavailable", "Autoswap is temporarily unavailable.")
	}
	enabled, generation, overflow, err := parseCrossMintToggleBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	if h.Cluster != Mainnet {
		return nil, earnError(http.StatusConflict, "unsupported_cluster", "Autoswap requires mainnet.")
	}
	scope := crossMintScope{
		Authority:   account.Wallet.String(),
		Cluster:     string(account.Cluster),
		Settings:    account.Settings.String(),
		VaultIndex:  int16(earnVaultIndex),
		VaultPubkey: account.Vault.String(),
	}

	// toggle route.ts:100-141: the enable path proves the installed state and
	// its on-chain artifacts before touching the opt-in row.
	if enabled {
		state, err := findEarnCrossMintState(ctx, h.YieldDB, scope)
		if err != nil {
			return nil, toggleFailed(err)
		}
		if state == nil {
			return nil, earnError(http.StatusNotFound, "autoswap_not_found", crossMintNotInstalledMessage)
		}
		if len(state.Policies) != 2 {
			return nil, earnError(http.StatusConflict, "autoswap_policy_unavailable", "Autoswap permissions are not ready. Keep it paused and try again after refreshing.")
		}
		if err := assertEarnCrossMintCanonicalArtifacts(ctx, h.Chain, account.Settings, state.BoundPolicies, state.DailySourceMintSpendingCap, state.MaxSlippageBps, h.PolicySigner); err != nil {
			return nil, toggleFailed(err)
		}
	}

	enrollment, kind, err := crossMintTransition(ctx, h, scope, enabled, generation, overflow)
	if err != nil {
		return nil, toggleFailed(err)
	}
	if kind == "missing" {
		return nil, earnError(http.StatusNotFound, "autoswap_not_found", crossMintNotInstalledMessage)
	}
	if kind == "stale" {
		return nil, toggleFailed(errors.New(crossMintStaleMessage))
	}
	status := "paused"
	if enrollment.Enabled {
		status = "on"
	}
	return &crossMintToggleOutput{Body: CrossMintToggleResponse{
		Enabled:    enrollment.Enabled,
		Generation: enrollment.Generation,
		Status:     status,
	}}, nil
}

// CrossMintDeleteReadinessRequest documents the request body for OpenAPI.
type CrossMintDeleteReadinessRequest struct {
	ExpectedGeneration *string `json:"expectedGeneration,omitempty"`
}

// CrossMintDeleteReadinessResponse mirrors the delete-readiness body
// (delete/readiness/route.ts:132-136).
type CrossMintDeleteReadinessResponse struct {
	ExpectedGeneration string   `json:"expectedGeneration"`
	Policies           []string `json:"policies"`
	Status             string   `json:"status"`
}

type crossMintDeleteReadinessInput struct {
	SessionInput
	Raw rawJSON
}

type crossMintDeleteReadinessOutput struct {
	Body CrossMintDeleteReadinessResponse
}

func (h earnRoutes) earnCrossMintDeleteReadiness(ctx context.Context, in *crossMintDeleteReadinessInput) (*crossMintDeleteReadinessOutput, error) {
	// The delete route's catch answers 409 delete_readiness_failed with
	// error.message, after letting smart-account provisioning refusals through
	// unchanged (delete/readiness/route.ts:137-148) — those ride h.account.
	failed := func(err error) error {
		return earnError(http.StatusConflict, "delete_readiness_failed", err.Error())
	}

	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("principal", err, "earn_cross_mint_unavailable", "Autoswap deletion readiness is temporarily unavailable.")
	}
	generation, overflow, err := parseCrossMintReadinessBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	if h.Cluster != Mainnet {
		return nil, earnError(http.StatusConflict, "unsupported_cluster", "Autoswap requires mainnet.")
	}
	scope := crossMintScope{
		Authority:   account.Wallet.String(),
		Cluster:     string(account.Cluster),
		Settings:    account.Settings.String(),
		VaultIndex:  int16(earnVaultIndex),
		VaultPubkey: account.Vault.String(),
	}

	enrollment, kind, err := crossMintTransition(ctx, h, scope, false, generation, overflow)
	if err != nil {
		return nil, failed(err)
	}
	if kind == "missing" {
		return nil, earnError(http.StatusNotFound, "autoswap_not_found", crossMintNotInstalledMessage)
	}
	if kind == "stale" {
		return nil, failed(errors.New(crossMintStaleMessage))
	}

	// delete/readiness/route.ts:114-120: a movement still in flight keeps the
	// policies on chain.
	movement, err := hasNonTerminalEarnCrossMintMovement(ctx, h.YieldDB, scope)
	if err != nil {
		return nil, failed(err)
	}
	if movement {
		return nil, earnError(http.StatusConflict, "movement_in_progress", "Autoswap is paused. Delete it after the current movement reaches a safe final state.")
	}

	// delete/readiness/route.ts:122-136: the enrolled pair read at confirmed
	// commitment; whatever survived is still to delete.
	policies := make([]string, 0, len(enrollment.BoundPolicies))
	for _, bound := range enrollment.BoundPolicies {
		policies = append(policies, bound.Account)
	}
	existing, err := fetchCrossMintPolicyExistence(ctx, h.Chain, policies)
	if err != nil {
		return nil, failed(err)
	}
	remaining := make([]string, 0, len(policies))
	for i, policy := range policies {
		if existing[i] {
			remaining = append(remaining, policy)
		}
	}
	status := "ready"
	if len(remaining) == 0 {
		status = "off"
	}
	return &crossMintDeleteReadinessOutput{Body: CrossMintDeleteReadinessResponse{
		ExpectedGeneration: enrollment.Generation,
		Policies:           remaining,
		Status:             status,
	}}, nil
}
