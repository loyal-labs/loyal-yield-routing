package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
)

const (
	walletCompletionStale    = 30 * time.Second
	walletCompletionWait     = 5 * time.Second
	walletCompletionInterval = 100 * time.Millisecond
	walletCompletionColumns  = `wallet_address, state, processing_token, processing_started_at, user_id::text,
smart_account_address, settings_pda, provisioning_outcome, last_error_code, last_error_message`
)

type walletCompletion struct {
	WalletAddress       string
	State               string
	ProcessingToken     *string
	ProcessingStartedAt *time.Time
	UserID              *string
	SmartAccountAddress *string
	SettingsPda         *string
	ProvisioningOutcome *string
	LastErrorCode       *string
	LastErrorMessage    *string
}

func scanWalletCompletion(row pgx.Row) (*walletCompletion, error) {
	var c walletCompletion
	err := row.Scan(&c.WalletAddress, &c.State, &c.ProcessingToken, &c.ProcessingStartedAt, &c.UserID,
		&c.SmartAccountAddress, &c.SettingsPda, &c.ProvisioningOutcome, &c.LastErrorCode, &c.LastErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (h earnRoutes) findWalletCompletion(ctx context.Context, challengeHash string) (*walletCompletion, error) {
	return scanWalletCompletion(h.appDB.QueryRow(ctx,
		`SELECT `+walletCompletionColumns+` FROM app_wallet_auth_completions WHERE challenge_hash = $1 LIMIT 1`, challengeHash))
}

func (h earnRoutes) beginWalletCompletion(ctx context.Context, challengeHash, walletAddress, processingToken string, staleBefore time.Time) (*walletCompletion, error) {
	now := jsTime(time.Now())
	if _, err := h.appDB.Exec(ctx, `INSERT INTO app_wallet_auth_completions
(challenge_hash, wallet_address, solana_env, state, processing_token, processing_started_at, created_at, updated_at)
VALUES ($1, $2, $3, 'processing', $4, $5, $5, $5)
ON CONFLICT DO NOTHING`, challengeHash, walletAddress, smartAccountsSolanaEnv, processingToken, now); err != nil {
		return nil, fmt.Errorf("insert wallet auth completion: %w", err)
	}
	record, err := h.findWalletCompletion(ctx, challengeHash)
	if err != nil {
		return nil, fmt.Errorf("read wallet auth completion: %w", err)
	}
	if record == nil {
		return nil, errors.New("failed to create wallet auth completion record")
	}
	stale := record.State == "processing" && (record.ProcessingToken == nil || *record.ProcessingToken != processingToken) &&
		(record.ProcessingStartedAt == nil || !record.ProcessingStartedAt.After(staleBefore))
	if !stale {
		return record, nil
	}
	clause, args := `processing_started_at IS NULL`, []any{challengeHash, processingToken, now}
	if record.ProcessingStartedAt != nil {
		clause, args = `processing_started_at = $4`, append(args, *record.ProcessingStartedAt)
	}
	taken, err := scanWalletCompletion(h.appDB.QueryRow(ctx, `UPDATE app_wallet_auth_completions
SET processing_token = $2, processing_started_at = $3, updated_at = $3, last_error_code = NULL, last_error_message = NULL
WHERE challenge_hash = $1 AND state = 'processing' AND `+clause+`
RETURNING `+walletCompletionColumns, args...))
	if err != nil {
		return nil, fmt.Errorf("take over wallet auth completion: %w", err)
	}
	if taken != nil {
		return taken, nil
	}
	if reread, err := h.findWalletCompletion(ctx, challengeHash); err != nil {
		return nil, fmt.Errorf("read wallet auth completion: %w", err)
	} else if reread != nil {
		return reread, nil
	}
	return record, nil
}

func (h earnRoutes) awaitWalletCompletion(ctx context.Context, challengeHash string) (*walletCompletion, error) {
	for started := time.Now(); time.Since(started) < walletCompletionWait; {
		record, err := h.findWalletCompletion(ctx, challengeHash)
		if err != nil {
			return nil, fmt.Errorf("read wallet auth completion: %w", err)
		}
		if record != nil && (record.State == "completed" || record.State == "failed") {
			return record, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(walletCompletionInterval):
		}
	}
	record, err := h.findWalletCompletion(ctx, challengeHash)
	if err != nil {
		return nil, fmt.Errorf("read wallet auth completion: %w", err)
	}
	if record == nil {
		return nil, errors.New("wallet auth completion record disappeared during polling")
	}
	return record, nil
}

func walletReplayFailure(record *walletCompletion) error {
	code, message, status := "wallet_onboarding_failed", "Wallet onboarding could not be completed.", http.StatusBadGateway
	if record.LastErrorCode != nil {
		code = *record.LastErrorCode
	}
	if record.LastErrorMessage != nil {
		message = *record.LastErrorMessage
	}
	if code == "smart_account_reservation_conflict" {
		status = http.StatusConflict
	}
	return walletRefusal(status, code, message)
}

func (h earnRoutes) completeWalletAuth(ctx context.Context, challengeHash string, wallet solana.PublicKey) (WalletAuthUser, error) {
	walletAddress := wallet.String()
	processingToken := randomUUID()
	record, err := h.beginWalletCompletion(ctx, challengeHash, walletAddress, processingToken, time.Now().Add(-walletCompletionStale))
	if err != nil {
		return WalletAuthUser{}, err
	}
	if record.WalletAddress != walletAddress {
		return WalletAuthUser{}, walletRefusal(http.StatusConflict, "wallet_challenge_wallet_mismatch", "Wallet challenge was completed by another wallet.")
	}
	owned := record.State == "processing" && record.ProcessingToken != nil && *record.ProcessingToken == processingToken
	if record.State == "processing" && !owned {
		if record, err = h.awaitWalletCompletion(ctx, challengeHash); err != nil {
			return WalletAuthUser{}, err
		}
		if record.State == "processing" {
			return WalletAuthUser{}, walletRefusal(http.StatusConflict, "wallet_auth_completion_in_progress", "Wallet onboarding is already in progress. Please retry in a moment.")
		}
		if record.State != "completed" {
			return WalletAuthUser{}, walletReplayFailure(record)
		}
	}
	switch {
	case record.State == "completed":
		return h.replayWalletCompletion(ctx, record)
	case record.State == "failed":
		return WalletAuthUser{}, walletReplayFailure(record)
	}
	return h.onboardWallet(ctx, challengeHash, processingToken, wallet)
}

func (h earnRoutes) replayWalletCompletion(ctx context.Context, record *walletCompletion) (WalletAuthUser, error) {
	if record.UserID == nil || *record.UserID == "" || record.SmartAccountAddress == nil || *record.SmartAccountAddress == "" {
		return WalletAuthUser{}, errors.New("wallet auth completion record is missing replay data")
	}
	if record.SettingsPda != nil && *record.SettingsPda != "" {
		return walletSessionUser(record.WalletAddress, *record.SmartAccountAddress, *record.SettingsPda), nil
	}
	found, err := h.findSmartAccount(ctx, *record.UserID)
	if err != nil {
		return WalletAuthUser{}, err
	}
	if found == nil || found.State != "ready" {
		return WalletAuthUser{}, errors.New("wallet auth completion replay could not find a ready smart account")
	}
	summary, err := summaryOf(*found)
	if err != nil {
		return WalletAuthUser{}, err
	}
	return walletSessionUser(record.WalletAddress, summary.SmartAccountAddress, summary.SettingsPda), nil
}

func (h earnRoutes) onboardWallet(ctx context.Context, challengeHash, processingToken string, wallet solana.PublicKey) (WalletAuthUser, error) {
	userID, err := h.privyGetOrCreateUser(ctx, wallet, "")
	var ensured ensuredSmartAccount
	if err == nil {
		ensured, err = h.ensureSmartAccount(ctx, userID, wallet)
	}
	var smartAccountAddress string
	if err == nil {
		smartAccountAddress, err = h.markWalletCompleted(ctx, challengeHash, processingToken, userID, ensured)
		if err == nil {
			return walletSessionUser(wallet.String(), smartAccountAddress, ensured.SmartAccount.SettingsPda), nil
		}
	}
	code, message := "wallet_onboarding_failed", "Wallet onboarding could not be completed."
	var refused *routeError
	if errors.As(err, &refused) {
		code, message = refused.Detail.Code, refused.Detail.Message
	}
	if markErr := h.markWalletFailed(ctx, challengeHash, processingToken, code, message, userID); markErr != nil {
		slog.Warn("wallet onboarding failure not persisted", "challenge_hash", challengeHash, "wallet", wallet.String(), "error", markErr)
	}
	if refused != nil {
		return WalletAuthUser{}, refused
	}
	slog.Warn("wallet onboarding failed unexpectedly", "challenge_hash", challengeHash, "wallet", wallet.String(), "error", err)
	return WalletAuthUser{}, walletProvisioningUnavailable()
}

func (h earnRoutes) markWalletCompleted(ctx context.Context, challengeHash, processingToken, userID string, ensured ensuredSmartAccount) (string, error) {
	now := jsTime(time.Now())
	var smartAccountAddress *string
	err := h.appDB.QueryRow(ctx, `UPDATE app_wallet_auth_completions
SET state = 'completed', processing_token = NULL, processing_started_at = NULL, user_id = $3::uuid,
    smart_account_address = $4, settings_pda = $5, provisioning_outcome = $6,
    last_error_code = NULL, last_error_message = NULL, completed_at = $7, updated_at = $7
WHERE challenge_hash = $1 AND processing_token = $2
RETURNING smart_account_address`, challengeHash, processingToken, userID,
		ensured.SmartAccount.SmartAccountAddress, ensured.SmartAccount.SettingsPda, string(ensured.Outcome), now).Scan(&smartAccountAddress)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("failed to mark wallet auth completion completed")
	}
	if err != nil {
		return "", fmt.Errorf("mark wallet auth completion completed: %w", err)
	}
	if smartAccountAddress == nil {
		return "", nil
	}
	return *smartAccountAddress, nil
}

func (h earnRoutes) markWalletFailed(ctx context.Context, challengeHash, processingToken, code, message, userID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var user any
	if userID != "" {
		user = userID
	}
	tag, err := h.appDB.Exec(ctx, `UPDATE app_wallet_auth_completions
SET state = 'failed', processing_token = NULL, processing_started_at = NULL, user_id = COALESCE($5::uuid, user_id),
    last_error_code = $3, last_error_message = $4, completed_at = $6, updated_at = $6
WHERE challenge_hash = $1 AND processing_token = $2`, challengeHash, processingToken, code, message, user, jsTime(time.Now()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("failed to mark wallet auth completion failed")
	}
	return nil
}
