package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type SponsorChain interface {
	chain.LandChain
	Blockhash(ctx context.Context, commitment rpc.CommitmentType, minContextSlot uint64) (hash solana.Hash, lastValid, slot uint64, err error)
}

type provisioningOutcome string

const (
	outcomeExistingReady           provisioningOutcome = "existing_ready"
	outcomeReconciledReady         provisioningOutcome = "reconciled_ready"
	outcomeDelegatedRootSigner     provisioningOutcome = "delegated_root_signer"
	outcomeSponsoredExistingRecord provisioningOutcome = "sponsored_existing_record"
	outcomeSponsoredNewRecord      provisioningOutcome = "sponsored_new_record"
	outcomeRetriedFailedRecord     provisioningOutcome = "retried_failed_record"
)

type ensuredSmartAccount struct {
	SmartAccount smartAccountSummary
	Outcome      provisioningOutcome
}

type smartAccountRecord struct {
	ID                string
	UserID            string
	SettingsPda       string
	State             string
	CreationSignature *string
}

type smartAccountSummary struct {
	ProgramID           string
	SettingsPda         string
	SmartAccountAddress string
	CreationSignature   *string
}

type settingsResolution int

const (
	settingsMissing settingsResolution = iota + 1
	settingsOwnerMismatch
	settingsReady
)

const (
	ensureSmartAccountBudget = 5 * time.Minute
	sponsorLandBudget        = 150 * time.Second
	settingsUniqueIndex      = "app_user_smart_accounts_env_settings_uidx"
)

var reservationRetryDelays = []time.Duration{400 * time.Millisecond, 900 * time.Millisecond, 1800 * time.Millisecond, 2700 * time.Millisecond, 3600 * time.Millisecond}

var errSponsorNotConfigured = errors.New("SMART_ACCOUNT_SPONSOR_PK is not set")

func (h earnRoutes) ensureSmartAccount(ctx context.Context, userID string, wallet solana.PublicKey) (ensuredSmartAccount, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ensureSmartAccountBudget)
	defer cancel()
	existing, err := h.findSmartAccount(ctx, userID)
	if err != nil {
		return ensuredSmartAccount{}, err
	}
	if existing != nil {
		resolution, record, err := h.promoteSmartAccount(ctx, *existing, wallet)
		if err != nil {
			return ensuredSmartAccount{}, err
		}
		if resolution == settingsReady {
			outcome := outcomeReconciledReady
			if existing.State == "ready" {
				outcome = outcomeExistingReady
			}
			return ensured(record, outcome)
		}
		delegated, err := h.delegatedRootSigner(ctx, userID, wallet)
		if err != nil || delegated != nil {
			return delegatedOutcome(delegated, err)
		}
		outcome := outcomeSponsoredExistingRecord
		if existing.State == "failed" {
			outcome = outcomeRetriedFailedRecord
		}
		if resolution == settingsMissing {
			config, err := h.programConfig(ctx)
			if err != nil {
				return ensuredSmartAccount{}, err
			}
			ready, err := h.sponsorRecord(ctx, *existing, wallet, config.Treasury)
			if err != nil {
				return ensuredSmartAccount{}, err
			}
			return ensured(ready, outcome)
		}
		reserved, treasury, err := h.reserveSmartAccount(ctx, userID, existing)
		if err != nil {
			return ensuredSmartAccount{}, err
		}
		ready, err := h.sponsorRecord(ctx, reserved, wallet, treasury)
		if err != nil {
			return ensuredSmartAccount{}, err
		}
		return ensured(ready, outcome)
	}

	delegated, err := h.delegatedRootSigner(ctx, userID, wallet)
	if err != nil || delegated != nil {
		return delegatedOutcome(delegated, err)
	}
	record, treasury, err := h.reserveSmartAccount(ctx, userID, nil)
	if err != nil {
		return ensuredSmartAccount{}, err
	}
	outcome := outcomeSponsoredNewRecord
	if record.State == "ready" {
		resolution, promoted, err := h.promoteSmartAccount(ctx, record, wallet)
		if err != nil {
			return ensuredSmartAccount{}, err
		}
		if resolution == settingsReady {
			return ensured(promoted, outcomeExistingReady)
		}
		record, treasury, err = h.reserveSmartAccount(ctx, userID, &record)
		if err != nil {
			return ensuredSmartAccount{}, err
		}
		outcome = outcomeSponsoredExistingRecord
	}
	ready, err := h.sponsorRecord(ctx, record, wallet, treasury)
	if err != nil {
		return ensuredSmartAccount{}, err
	}
	return ensured(ready, outcome)
}

func ensured(record smartAccountRecord, outcome provisioningOutcome) (ensuredSmartAccount, error) {
	summary, err := summaryOf(record)
	if err != nil {
		return ensuredSmartAccount{}, err
	}
	return ensuredSmartAccount{SmartAccount: summary, Outcome: outcome}, nil
}

func delegatedOutcome(summary *smartAccountSummary, err error) (ensuredSmartAccount, error) {
	if err != nil {
		return ensuredSmartAccount{}, err
	}
	return ensuredSmartAccount{SmartAccount: *summary, Outcome: outcomeDelegatedRootSigner}, nil
}
func (h earnRoutes) promoteSmartAccount(ctx context.Context, record smartAccountRecord, wallet solana.PublicKey) (settingsResolution, smartAccountRecord, error) {
	settings, err := h.readSettings(ctx, record.SettingsPda)
	if err != nil {
		return 0, record, err
	}
	if settings == nil {
		return settingsMissing, record, nil
	}
	if !hasSigner(settings, wallet) {
		return settingsOwnerMismatch, record, nil
	}
	if record.State == "ready" {
		return settingsReady, record, nil
	}
	ready, err := h.markSmartAccountReady(ctx, record, record.CreationSignature)
	if err != nil {
		return 0, record, err
	}
	return settingsReady, ready, nil
}
func (h earnRoutes) sponsorRecord(ctx context.Context, record smartAccountRecord, wallet solana.PublicKey, treasury solana.PublicKey) (smartAccountRecord, error) {
	for attempt := 0; ; attempt++ {
		signature, err := h.createSponsoredSmartAccount(ctx, record.SettingsPda, treasury, wallet)
		if err == nil {
			return h.markSmartAccountReady(ctx, record, &signature)
		}
		resolution, promoted, promoteErr := h.promoteSmartAccount(ctx, record, wallet)
		if promoteErr != nil {
			return smartAccountRecord{}, promoteErr
		}
		if resolution == settingsReady {
			return promoted, nil
		}
		if resolution == settingsOwnerMismatch && attempt == 0 {
			record, treasury, err = h.reserveSmartAccount(ctx, record.UserID, &record)
			if err != nil {
				return smartAccountRecord{}, err
			}
			continue
		}
		failure := provisioningFailure(err)
		if _, markErr := h.markSmartAccountFailed(ctx, record, failure.Detail.Code, failure.Detail.Message); markErr != nil {
			return smartAccountRecord{}, markErr
		}
		return smartAccountRecord{}, failure
	}
}
func provisioningFailure(err error) *routeError {
	var refused *routeError
	if errors.As(err, &refused) {
		return refused
	}
	if errors.Is(err, errSponsorNotConfigured) {
		return &routeError{Detail: EarnMaxErrorDetail{Code: "smart_account_sponsor_not_configured",
			Message: "Sponsored smart account creation is not configured on this environment."}, status: http.StatusInternalServerError}
	}
	return &routeError{Detail: EarnMaxErrorDetail{Code: "smart_account_provisioning_failed",
		Message: chain.Scrub(err.Error())}, status: http.StatusBadGateway}
}
func (h earnRoutes) createSponsoredSmartAccount(ctx context.Context, settingsPda string, treasury, wallet solana.PublicKey) (string, error) {
	if h.Sponsor == nil {
		return "", errSponsorNotConfigured
	}
	if h.SponsorChain == nil {
		return "", errors.New("sponsored smart account create has no chain sender")
	}
	settings, err := solana.PublicKeyFromBase58(settingsPda)
	if err != nil {
		return "", fmt.Errorf("stored settings_pda %q is not a public key", settingsPda)
	}
	blockhash, lastValid, _, err := h.SponsorChain.Blockhash(ctx, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return "", err
	}
	tx, err := earnprep.SponsoredCreate{Sponsor: h.Sponsor.PublicKey(), Treasury: treasury, Settings: settings, Wallet: wallet, Blockhash: blockhash}.Compile()
	if err != nil {
		return "", err
	}
	if err := h.Sponsor.Cosign(tx); err != nil {
		return "", err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	signature := tx.Signatures[0].String()
	if err := h.SponsorChain.SendWire(ctx, wire, false); err != nil {
		return "", err
	}
	landCtx, cancel := context.WithTimeout(ctx, sponsorLandBudget)
	defer cancel()
	landed, err := chain.Land(landCtx, h.SponsorChain, chain.Attempt{Wire: wire, Signature: signature, LastValidBlockHeight: lastValid, Required: chain.Finalized},
		chain.LandResendEvery, func(context.Context) error { return nil })
	if err != nil {
		return "", fmt.Errorf("smart account creation %s was not finalized: %w", signature, err)
	}
	switch landed.Kind {
	case chain.Failed:
		return "", fmt.Errorf("smart account creation %s failed on-chain: %s", signature, landed.Err)
	case chain.Expired:
		return "", fmt.Errorf("smart account creation %s expired before it was finalized", signature)
	}
	signers, err := h.readSettings(ctx, settingsPda)
	if err != nil {
		return "", err
	}
	if signers == nil || !hasSigner(signers, wallet) {
		return "", fmt.Errorf("smart account creation %s finalized but settings %s does not list %s as a signer", signature, settingsPda, wallet)
	}
	return signature, nil
}
func (h earnRoutes) reserveSmartAccount(ctx context.Context, userID string, replace *smartAccountRecord) (smartAccountRecord, solana.PublicKey, error) {
	var lastConflict error
	for attempt := 0; attempt <= len(reservationRetryDelays); attempt++ {
		config, err := h.programConfig(ctx)
		if err != nil {
			return smartAccountRecord{}, solana.PublicKey{}, err
		}
		settings, err := earnprep.SettingsForIndex(new(big.Int).Add(config.SmartAccountIndex, big.NewInt(1)))
		if err != nil {
			return smartAccountRecord{}, solana.PublicKey{}, err
		}
		record, err := h.reserveProvisioning(ctx, userID, settings.String(), replace)
		if err == nil {
			return record, config.Treasury, nil
		}
		if !isSettingsReservationConflict(err) {
			return smartAccountRecord{}, solana.PublicKey{}, err
		}
		lastConflict = err
		if attempt < len(reservationRetryDelays) {
			select {
			case <-ctx.Done():
				return smartAccountRecord{}, solana.PublicKey{}, ctx.Err()
			case <-time.After(reservationRetryDelays[attempt]):
			}
		}
	}
	slog.Warn("smart account settings PDA reservation exhausted", "attempts", len(reservationRetryDelays)+1, "error", lastConflict, "userId", userID)
	return smartAccountRecord{}, solana.PublicKey{}, earnError(http.StatusConflict, "smart_account_reservation_conflict",
		"We're still setting up your Earn account — a lot of people are signing up right now. Please try again in a moment.")
}

func isSettingsReservationConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (pgErr.ConstraintName == "" || pgErr.ConstraintName == settingsUniqueIndex)
}

func (h earnRoutes) programConfig(ctx context.Context) (earnprep.ProgramConfig, error) {
	address, err := earnprep.ProgramConfigAddress()
	if err != nil {
		return earnprep.ProgramConfig{}, err
	}
	_, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{address}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return earnprep.ProgramConfig{}, fmt.Errorf("read program config: %w", err)
	}
	return earnprep.DecodeProgramConfig(accounts[0])
}

func (h earnRoutes) findSmartAccount(ctx context.Context, userID string) (*smartAccountRecord, error) {
	record, err := scanSmartAccount(h.appDB.QueryRow(ctx,
		`SELECT id::text, user_id::text, settings_pda, state, creation_signature
FROM app_user_smart_accounts
WHERE user_id = $1 AND solana_env = $2`,
		userID, smartAccountsSolanaEnv))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read app user smart account: %w", err)
	}
	return &record, nil
}

func scanSmartAccount(row pgx.Row) (smartAccountRecord, error) {
	var record smartAccountRecord
	err := row.Scan(&record.ID, &record.UserID, &record.SettingsPda, &record.State, &record.CreationSignature)
	return record, err
}

const smartAccountColumns = `id::text, user_id::text, settings_pda, state, creation_signature`

func (h earnRoutes) reserveProvisioning(ctx context.Context, userID, settingsPda string, replace *smartAccountRecord) (smartAccountRecord, error) {
	inserted, err := scanSmartAccount(h.appDB.QueryRow(ctx,
		`INSERT INTO app_user_smart_accounts
  (user_id, solana_env, settings_pda, state, creation_signature, last_checked_at, last_error_code, last_error_message, created_at, updated_at)
VALUES ($1, $2, $3, 'provisioning', NULL, now(), NULL, NULL, now(), now())
ON CONFLICT (user_id, solana_env) DO NOTHING
RETURNING `+smartAccountColumns,
		userID, smartAccountsSolanaEnv, settingsPda))
	if err == nil {
		return inserted, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return smartAccountRecord{}, err
	}
	existing, err := h.findSmartAccount(ctx, userID)
	if err != nil {
		return smartAccountRecord{}, err
	}
	if existing == nil {
		return smartAccountRecord{}, errors.New("failed to reserve app user smart account provisioning")
	}
	if replace == nil {
		if existing.State == "provisioning" || existing.State == "ready" {
			return *existing, nil
		}
		return smartAccountRecord{}, errors.New("smart account provisioning reservation already exists and was not replaceable")
	}
	replaced, err := scanSmartAccount(h.appDB.QueryRow(ctx,
		`UPDATE app_user_smart_accounts
SET settings_pda = $5, state = 'provisioning', creation_signature = NULL, last_checked_at = now(),
    last_error_code = NULL, last_error_message = NULL, updated_at = now()
WHERE id = $1 AND user_id = $2 AND solana_env = $3 AND settings_pda = $4
RETURNING `+smartAccountColumns,
		replace.ID, userID, smartAccountsSolanaEnv, replace.SettingsPda, settingsPda))
	if errors.Is(err, pgx.ErrNoRows) {
		return smartAccountRecord{}, errors.New("smart account provisioning reservation was superseded before it could be replaced")
	}
	return replaced, err
}
func (h earnRoutes) markSmartAccountReady(ctx context.Context, record smartAccountRecord, signature *string) (smartAccountRecord, error) {
	ready, err := scanSmartAccount(h.appDB.QueryRow(ctx,
		`UPDATE app_user_smart_accounts
SET state = 'ready', creation_signature = COALESCE($5, creation_signature), last_checked_at = now(),
    last_error_code = NULL, last_error_message = NULL, updated_at = now()
WHERE id = $1 AND user_id = $2 AND solana_env = $3 AND settings_pda = $4
RETURNING `+smartAccountColumns,
		record.ID, record.UserID, smartAccountsSolanaEnv, record.SettingsPda, signature))
	if errors.Is(err, pgx.ErrNoRows) {
		return smartAccountRecord{}, errors.New("smart account provisioning reservation was superseded before it could be marked ready")
	}
	if err != nil {
		return smartAccountRecord{}, fmt.Errorf("mark app user smart account ready: %w", err)
	}
	return ready, nil
}
func (h earnRoutes) markSmartAccountFailed(ctx context.Context, record smartAccountRecord, code, message string) (smartAccountRecord, error) {
	failed, err := scanSmartAccount(h.appDB.QueryRow(ctx,
		`UPDATE app_user_smart_accounts
SET state = 'failed', last_checked_at = now(),
    last_error_code = $5, last_error_message = $6, updated_at = now()
WHERE id = $1 AND user_id = $2 AND solana_env = $3 AND settings_pda = $4
RETURNING `+smartAccountColumns,
		record.ID, record.UserID, smartAccountsSolanaEnv, record.SettingsPda, code, message))
	if errors.Is(err, pgx.ErrNoRows) {
		return smartAccountRecord{}, errors.New("smart account provisioning reservation was superseded before it could be marked failed")
	}
	if err != nil {
		return smartAccountRecord{}, fmt.Errorf("mark app user smart account failed: %w", err)
	}
	return failed, nil
}

type rootSignerMembership struct {
	SmartAccountAddress string
	SettingsPda         string
	SourceSignature     *string
	SourceSlot          *int64
}

func (h earnRoutes) readSettings(ctx context.Context, settingsPda string) (*squads.Settings, error) {
	address, err := solana.PublicKeyFromBase58(settingsPda)
	if err != nil {
		return nil, fmt.Errorf("stored settings_pda %q is not a public key", settingsPda)
	}
	_, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{address}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, fmt.Errorf("read settings %s: %w", settingsPda, err)
	}
	if accounts[0] == nil || accounts[0].Owner != squads.ProgramID || accounts[0].Executable {
		return nil, nil
	}
	settings, err := squads.DecodeSettings(accounts[0])
	if err != nil {
		return nil, fmt.Errorf("decode settings %s: %w", settingsPda, err)
	}
	return &settings, nil
}

func hasSigner(settings *squads.Settings, wallet solana.PublicKey) bool {
	for _, signer := range settings.Signers {
		if signer.Key == wallet {
			return true
		}
	}
	return false
}

func summaryOf(record smartAccountRecord) (smartAccountSummary, error) {
	settings, err := solana.PublicKeyFromBase58(record.SettingsPda)
	if err != nil {
		return smartAccountSummary{}, fmt.Errorf("stored settings_pda %q is not a public key", record.SettingsPda)
	}
	smartAccount, _, err := squads.SmartAccountAddress(settings, 0)
	if err != nil {
		return smartAccountSummary{}, fmt.Errorf("derive smart account for %s: %w", record.SettingsPda, err)
	}
	return smartAccountSummary{
		ProgramID:           squads.ProgramID.String(),
		SettingsPda:         record.SettingsPda,
		SmartAccountAddress: smartAccount.String(),
		CreationSignature:   record.CreationSignature,
	}, nil
}

func (h earnRoutes) delegatedRootSigner(ctx context.Context, userID string, wallet solana.PublicKey) (*smartAccountSummary, error) {
	rows, err := h.appDB.Query(ctx,
		`SELECT smart_account_address, settings_pda, source_signature, source_slot
FROM app_smart_account_signers
WHERE solana_env = $1 AND signer_address = $2 AND scope = 'root_settings' AND state = 'active'
ORDER BY source_slot DESC, updated_at DESC, settings_pda DESC`,
		smartAccountsSolanaEnv, wallet.String())
	if err != nil {
		return nil, fmt.Errorf("read root signer memberships: %w", err)
	}
	defer rows.Close()
	var memberships []rootSignerMembership
	for rows.Next() {
		var membership rootSignerMembership
		if err := rows.Scan(&membership.SmartAccountAddress, &membership.SettingsPda,
			&membership.SourceSignature, &membership.SourceSlot); err != nil {
			return nil, fmt.Errorf("read root signer memberships: %w", err)
		}
		memberships = append(memberships, membership)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read root signer memberships: %w", err)
	}

	for _, membership := range memberships {
		settings, err := solana.PublicKeyFromBase58(membership.SettingsPda)
		if err != nil {
			return nil, fmt.Errorf("membership settings_pda %q is not a public key", membership.SettingsPda)
		}
		expected, _, err := squads.SmartAccountAddress(settings, 0)
		if err != nil {
			return nil, fmt.Errorf("derive canonical smart account for %s: %w", membership.SettingsPda, err)
		}
		if membership.SmartAccountAddress != expected.String() {
			continue
		}
		root, err := h.readSettings(ctx, membership.SettingsPda)
		if err != nil {
			return nil, earnUnavailable("root settings", err,
				"smart_account_provisioning_unavailable", "Could not verify your Earn account. Please try again.")
		}
		var rootSigner *squads.Signer
		if root != nil {
			for i := range root.Signers {
				if root.Signers[i].Key == wallet {
					rootSigner = &root.Signers[i]
					break
				}
			}
		}
		if rootSigner == nil {
			if err := h.markRootSignerRemoved(ctx, membership.SettingsPda, wallet.String()); err != nil {
				return nil, err
			}
			continue
		}
		err = h.upsertActiveRootSigner(ctx, rootSignerUpsert{
			Membership: membership,
			Wallet:     wallet.String(),
			UserID:     userID,
			Permission: int32(rootSigner.Permissions),
		})
		if err != nil {
			return nil, err
		}
		return &smartAccountSummary{
			ProgramID:           squads.ProgramID.String(),
			SettingsPda:         membership.SettingsPda,
			SmartAccountAddress: membership.SmartAccountAddress,
		}, nil
	}
	return nil, nil
}

func (h earnRoutes) markRootSignerRemoved(ctx context.Context, settingsPda, signerAddress string) error {
	_, err := h.appDB.Exec(ctx,
		`UPDATE app_smart_account_signers
SET state = 'removed', source_signature = NULL, source_slot = NULL, removed_at = now(), last_checked_at = now(), updated_at = now()
WHERE solana_env = $1 AND settings_pda = $2 AND scope = 'root_settings' AND signer_address = $3`,
		smartAccountsSolanaEnv, settingsPda, signerAddress)
	if err != nil {
		return fmt.Errorf("mark root signer removed: %w", err)
	}
	return nil
}

type rootSignerUpsert struct {
	Membership rootSignerMembership
	Wallet     string
	UserID     string
	Permission int32
}

func (h earnRoutes) upsertActiveRootSigner(ctx context.Context, up rootSignerUpsert) error {
	_, err := h.appDB.Exec(ctx,
		`INSERT INTO app_smart_account_signers
  (solana_env, smart_account_address, settings_pda, signer_address, scope, state,
   permission_mask, source_signature, source_slot, activated_at, removed_at,
   last_checked_at, user_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'root_settings', 'active', $5, $6, $7, now(), NULL, now(), $8, now(), now())
ON CONFLICT (solana_env, settings_pda, scope, signer_address) DO UPDATE
SET smart_account_address = EXCLUDED.smart_account_address,
    state = 'active',
    permission_mask = EXCLUDED.permission_mask,
    source_signature = EXCLUDED.source_signature,
    source_slot = EXCLUDED.source_slot,
    removed_at = NULL,
    last_checked_at = EXCLUDED.last_checked_at,
    user_id = EXCLUDED.user_id,
    updated_at = EXCLUDED.updated_at`,
		smartAccountsSolanaEnv, up.Membership.SmartAccountAddress, up.Membership.SettingsPda, up.Wallet,
		up.Permission, up.Membership.SourceSignature, up.Membership.SourceSlot, up.UserID)
	if err != nil {
		return fmt.Errorf("upsert active root signer: %w", err)
	}
	return nil
}
