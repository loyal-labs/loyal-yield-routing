package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// POST /api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare
// ports web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/
// cleanup/prepare/route.ts:75-322: the vault state read, the projected or
// client-sent minimum context slot, the zero proof, the autodeposit close
// decision, and the prepared cleanup wire. h.account covers the principal and
// the wallet-controls assert (service.ts:177-219); every remaining failure
// folds into 500 prepare_failed with the error's own text, like the Next
// catch-all.
const earnCleanupPreparePath = "/api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare"

type earnCleanupPrepareInput struct {
	SessionInput
	Raw rawJSON
}

type earnCleanupPrepareOutput struct {
	Body EarnCleanupPrepareResponse
}

func (h earnRoutes) registerCleanupPrepare(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-withdrawals-cleanup-prepare",
		Method:      http.MethodPost,
		Path:        earnCleanupPreparePath,
		Summary:     "Prepare an Earn full-exit cleanup for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 409, 500, 503),
	}, h.earnCleanupPrepare)
	documentJSONBody[EarnCleanupPrepareRequest](api, earnCleanupPreparePath)
}

func (h earnRoutes) earnCleanupPrepare(ctx context.Context, in *earnCleanupPrepareInput) (*earnCleanupPrepareOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, cleanupPrepareFailed(err)
	}
	body, err := h.cleanupPrepare(ctx, account, in.Raw.data)
	if err != nil {
		return nil, err
	}
	return &earnCleanupPrepareOutput{Body: *body}, nil
}

// cleanupPrepareFailed is the route's catch-all (:303-321): log, then 500
// prepare_failed carrying the error's message verbatim.
func cleanupPrepareFailed(err error) error {
	slog.Error("[earn-withdraw-cleanup-prepare] prepare failed", "error", err)
	return earnError(http.StatusInternalServerError, "prepare_failed", err.Error())
}

// cleanupPrepare is the route body in Next order (:99-302): vault state, body
// parse, slot decision, zero proof, autodeposit close decision, then the
// builder. The estimated refund lamports ride on last, summed from the policy
// accounts the route reads (:291-302).
func (h earnRoutes) cleanupPrepare(ctx context.Context, account EarnAccount, raw []byte) (*EarnCleanupPrepareResponse, error) {
	state, err := h.findCleanupVaultState(ctx, false, account.Wallet.String(), account.Settings.String(), account.Vault)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	if state == nil {
		return nil, earnError(http.StatusConflict, "missing_earn_policy", "No active Earn accounts were found to close.")
	}
	body, err := parseJSONBody(raw)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", "Invalid request body.")
	}
	minContextSlot, err := cleanupMinContextSlot(body)
	if err != nil {
		return nil, err
	}
	if minContextSlot == nil {
		if minContextSlot, err = h.cleanupProjectedSlot(ctx, account); err != nil {
			return nil, err
		}
	}
	proof, err := verifyCleanupFullExitZeroBalances(ctx, h.Chain, time.Now, string(account.Cluster), state.Route, account.Settings, *minContextSlot)
	if err != nil {
		slog.Error("[earn-withdraw-cleanup-prepare] zero proof retryable", "error", err,
			"minContextSlot", *minContextSlot, "settings", account.Settings.String(), "walletAddress", account.Wallet.String())
		return nil, earnError(http.StatusServiceUnavailable, "full_exit_verification_retryable", err.Error())
	}
	if proof.Status != "policy_close_required" {
		return nil, earnError(http.StatusConflict, "full_exit_incomplete", "Earn balances remain above the full-exit dust tolerance. Resume withdrawal before closing policies.")
	}
	autodepositClose, err := h.cleanupAutodepositClose(ctx, account)
	if err != nil {
		return nil, err
	}
	routeAccount, err := solana.PublicKeyFromBase58(state.Route.PolicyAccount)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	policyAccounts := []solana.PublicKey{routeAccount}
	if state.Setup != nil {
		setupAccount, err := solana.PublicKeyFromBase58(state.Setup.PolicyAccount)
		if err != nil {
			return nil, cleanupPrepareFailed(err)
		}
		policyAccounts = append(policyAccounts, setupAccount)
	}
	_, policyInfos, err := h.Chain.Accounts(ctx, policyAccounts, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	var estimatedRefund uint64
	for _, info := range policyInfos {
		if info != nil {
			estimatedRefund += info.Lamports
		}
	}
	input := cleanupCloseInput{
		Cluster:            string(account.Cluster),
		FeePayer:           account.Wallet,
		PolicySigner:       h.PolicySigner,
		SettingsPda:        account.Settings,
		Vault:              account.Vault,
		Wallet:             account.Wallet,
		RoutePolicyAccount: routeAccount,
		RoutePolicySeed:    uint64(state.Route.PolicySeed),
		AutodepositClose:   autodepositClose,
	}
	if state.Setup != nil {
		setupAccount, err := solana.PublicKeyFromBase58(state.Setup.PolicyAccount)
		if err != nil {
			return nil, cleanupPrepareFailed(err)
		}
		setupSeed := uint64(state.Setup.PolicySeed)
		input.SetupPolicyAccount, input.SetupPolicySeed = &setupAccount, &setupSeed
	}
	for _, entry := range proof.CleanupTokenAccounts {
		account, err := cleanupVaultTokenAccountOf(entry)
		if err != nil {
			return nil, cleanupPrepareFailed(err)
		}
		input.VaultTokenAccounts = append(input.VaultTokenAccounts, account)
	}
	wire, err := buildEarnUsdcCleanup(ctx, h.Chain, input)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	wire.EstimatedRefundLamports = &estimatedRefund
	return &EarnCleanupPrepareResponse{PreparedCleanup: *wire}, nil
}

// cleanupVaultTokenAccountOf maps one zero-proof cleanup entry into the
// builder's input shape (route.ts:269-275); a malformed entry is a plain
// builder failure.
func cleanupVaultTokenAccountOf(entry withdrawCleanupTokenAccount) (cleanupVaultTokenAccount, error) {
	address, err := solana.PublicKeyFromBase58(entry.Address)
	if err != nil {
		return cleanupVaultTokenAccount{}, err
	}
	mint, err := solana.PublicKeyFromBase58(entry.Mint)
	if err != nil {
		return cleanupVaultTokenAccount{}, err
	}
	program, err := solana.PublicKeyFromBase58(entry.TokenProgramID)
	if err != nil {
		return cleanupVaultTokenAccount{}, err
	}
	amount, err := strconv.ParseUint(entry.AmountRaw, 10, 64)
	if err != nil {
		return cleanupVaultTokenAccount{}, err
	}
	return cleanupVaultTokenAccount{Address: address, AmountRaw: amount, Decimals: entry.Decimals, Mint: mint, TokenProgramID: program}, nil
}

var cleanupDigits = regexp.MustCompile(`^[0-9]+$`)

// cleanupMinContextSlot ports parseMinContextSlot and the route's branch
// (:120-136): a present minContextSlot must be a digit string naming a safe
// integer; nil means the body carried none and the projection read decides.
func cleanupMinContextSlot(body any) (*uint64, error) {
	object, ok := body.(map[string]any)
	if !ok {
		return nil, nil
	}
	requested, present := object["minContextSlot"]
	if !present {
		return nil, nil
	}
	text, isString := requested.(string)
	if !isString || !cleanupDigits.MatchString(text) {
		return nil, earnError(http.StatusBadRequest, "invalid_request", "minContextSlot must be a non-negative safe integer string.")
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value > 1<<53-1 {
		return nil, earnError(http.StatusBadRequest, "invalid_request", "minContextSlot must be a non-negative safe integer string.")
	}
	return &value, nil
}

// cleanupProjectedSlot ports the PROJECTION_RETRY_DELAYS_MS loop (:138-167):
// the newest full withdrawal's confirmed slot is the minimum context slot,
// and a projection that has not landed answers 503 with Retry-After: 1.
func (h earnRoutes) cleanupProjectedSlot(ctx context.Context, account EarnAccount) (*uint64, error) {
	pending := func() (*uint64, error) {
		return nil, &wprepRetryError{routeError{Detail: EarnMaxErrorDetail{
			Code:    "full_withdrawal_projection_pending",
			Message: "The confirmed Earn withdrawal is still updating. Retry cleanup.",
		}, status: http.StatusServiceUnavailable}}
	}
	var slot int64
	for _, delay := range cleanupProjectionRetries {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		var err error
		if slot, err = h.cleanupLatestFullWithdrawalSlot(ctx, account); err != nil {
			return nil, cleanupPrepareFailed(err)
		}
		if slot != 0 {
			break
		}
	}
	if slot <= 0 || slot > 1<<53-1 {
		return pending()
	}
	value := uint64(slot)
	return &value, nil
}

// cleanupAutodepositClose ports the route's autodeposit decision (:209-248):
// a live target with its sweep policy still on chain closes ahead of the
// cleanup; a vanished policy row is reconciled closed instead. No target, no
// policy account or no delegation means nothing rides.
func (h earnRoutes) cleanupAutodepositClose(ctx context.Context, account EarnAccount) (*cleanupAutodepositCloseInput, error) {
	target, _, err := h.stateAutodepositState(ctx, account.Settings.String(), account.Wallet.String())
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	if target == nil || target.PolicyAccount == "" || target.RecurringDelegation == nil {
		return nil, nil
	}
	policyAccount, err := solana.PublicKeyFromBase58(target.PolicyAccount)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	_, infos, err := h.Chain.Accounts(ctx, []solana.PublicKey{policyAccount}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	if len(infos) == 0 || infos[0] == nil {
		if err := h.reconcileMissingAutodepositPolicy(ctx, target); err != nil {
			return nil, cleanupPrepareFailed(err)
		}
		return nil, nil
	}
	delegation, err := solana.PublicKeyFromBase58(*target.RecurringDelegation)
	if err != nil {
		return nil, cleanupPrepareFailed(err)
	}
	return &cleanupAutodepositCloseInput{Policy: policyAccount, RecurringDelegation: delegation}, nil
}

// reconcileMissingAutodepositPolicy ports reconcileMissingOnChainEarnAutodepositPolicy
// (earn-autodeposit-repository.server.ts:1689-1758): the target whose sweep
// policy vanished from chain closes in place — its scheduled sweeps cancelled
// and both the sweep policy row and the target flipped closed. The TS runs
// the three writes as separate pool statements; one transaction here cannot
// leave an open target behind a closed policy after a crash, and a second run
// finds the closed target and changes nothing (the conflict-free re-run key
// is the target's id, resolved by its unique policy_account).
func (h earnRoutes) reconcileMissingAutodepositPolicy(ctx context.Context, target *stateAutodepositTargetRow) error {
	existing, err := h.findAutodepositTargetByPolicy(ctx, target.PolicyAccount)
	if err != nil {
		return err
	}
	if existing == nil {
		return errors.New("Autodeposit target does not exist.")
	}
	if existing.Settings != target.Settings || existing.Wallet != target.Wallet || int(existing.VaultIndex) != int(target.VaultIndex) {
		return errors.New("Autodeposit target does not match the wallet.")
	}
	now := time.Now()
	signature := "reconciled_missing_policy:" + target.PolicyAccount
	tx, err := h.YieldDB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := cancelScheduledAutodepositTransactionsForCloseTx(ctx, tx, now, existing.ID); err != nil {
		return err
	}
	if existing.LifecycleStatus != "closed" {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_policies
SET active = false, close_signature = $2, closed_at = $3, last_seen_at = $3, last_seen_signature = $2
WHERE policy_account = $1`, target.PolicyAccount, signature, now); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRow(ctx, `UPDATE loyal_yield.balance_sweep_targets
SET desired_active = false, close_signature = $2, closed_at = $3, last_seen_at = $3, last_seen_signature = $2, chain_status = 'closed'
WHERE policy_account = $1 RETURNING id`, target.PolicyAccount, signature, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("Failed to reconcile missing autodeposit policy.")
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
