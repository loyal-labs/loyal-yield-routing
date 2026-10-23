package webapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Privy sign-in, part 3 of 3: identity writes. Ports
// web/apps/web/src/features/chat/server/app-user.ts getOrCreateCurrentUser
// (app_users upsert + app_user_wallets upsert) and the read/reconcile half of
// web/apps/web/src/features/smart-accounts/service.ts ensureUserSmartAccount.
// The one seam Go does NOT port is sponsorRecord: a sponsored on-chain
// smart-account create would need the sponsor cosigner, which only
// earn_deposit.go may reach (signer_test.go), so that path stops with a
// retryable 503 and is requested as a shared-file change in the REPORT.
const privySmartAccountEnv = smartAccountsSolanaEnv

// privyGetOrCreateUser ports getOrCreateCurrentUser (app-user.ts:161-200):
// find by (provider, subject_address), else insert with ON CONFLICT DO
// NOTHING and re-read after a lost race, then refresh metadata and upsert the
// wallet row in every branch. TS commits each statement separately; here the
// four statements share one short transaction so a crash cannot leave a user
// row without its wallet row — the re-run writes the same rows either way.
func (h earnRoutes) privyGetOrCreateUser(ctx context.Context, wallet solana.PublicKey, email string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := h.appDB.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin privy user upsert: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	address := wallet.String()
	userID, err := privyFindUser(ctx, tx, address)
	if err != nil {
		return "", err
	}
	if userID == "" {
		userID, err = privyCreateUser(ctx, tx, address, email)
		if err != nil {
			return "", err
		}
	}
	if err := privyTouchUser(ctx, tx, userID, email); err != nil {
		return "", err
	}
	if err := privyUpsertUserWallet(ctx, tx, userID, address); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit privy user upsert: %w", err)
	}
	return userID, nil
}

func privyFindUser(ctx context.Context, tx pgx.Tx, address string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM app_users WHERE provider = 'solana' AND subject_address = $1`,
		address).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read app user by principal: %w", err)
	}
	return id, nil
}

func privyCreateUser(ctx context.Context, tx pgx.Tx, address, email string) (string, error) {
	var id string
	// Conflict target mirrors the app_users_provider_subject_uidx unique
	// index the Drizzle insert races on (app-user.ts:87).
	err := tx.QueryRow(ctx,
		`INSERT INTO app_users (provider, subject_address, email, created_at, updated_at)
VALUES ('solana', $1, $2, now(), now())
ON CONFLICT DO NOTHING
RETURNING id::text`,
		address, privyOptionalText(email)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		raced, findErr := privyFindUser(ctx, tx, address)
		if findErr != nil {
			return "", findErr
		}
		if raced == "" {
			return "", errors.New("failed to provision app user")
		}
		return raced, nil
	}
	if err != nil {
		return "", fmt.Errorf("insert app user: %w", err)
	}
	return id, nil
}

func privyTouchUser(ctx context.Context, tx pgx.Tx, userID, email string) error {
	if email == "" {
		_, err := tx.Exec(ctx, `UPDATE app_users SET updated_at = now() WHERE id = $1`, userID)
		if err != nil {
			return fmt.Errorf("touch app user metadata: %w", err)
		}
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE app_users SET updated_at = now(), email = $2 WHERE id = $1`, userID, email)
	if err != nil {
		return fmt.Errorf("update app user metadata: %w", err)
	}
	return nil
}

// privyUpsertUserWallet mirrors upsertWalletForUser (app-user.ts:112-131):
// the wallet address is globally unique, so the conflict target is the
// app_user_wallets_wallet_address_uidx index and a re-run refreshes the same
// row instead of failing.
func privyUpsertUserWallet(ctx context.Context, tx pgx.Tx, userID, address string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO app_user_wallets (user_id, wallet_address, verified_at, last_used_at, created_at, updated_at)
VALUES ($1, $2, now(), now(), now(), now())
ON CONFLICT (wallet_address) DO UPDATE
SET user_id = EXCLUDED.user_id,
    verified_at = EXCLUDED.verified_at,
    last_used_at = EXCLUDED.last_used_at,
    updated_at = EXCLUDED.updated_at`,
		userID, address)
	if err != nil {
		return fmt.Errorf("upsert app user wallet: %w", err)
	}
	return nil
}

func privyOptionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// privySponsorshipNeeded marks the one seam the packet forbids signing:
// sponsorRecord would send a sponsored on-chain smart-account create from
// SMART_ACCOUNT_SPONSOR_PK (onchain.ts:222-275). Only earn_deposit.go may
// reach the sponsor cosigner, so Privy complete stops here with a retryable
// 503 and asks for the provisioning shape as a shared-file change.
func privySponsorshipNeeded() error {
	return earnError(http.StatusServiceUnavailable, "smart_account_provisioning_sponsorship_required",
		"Your Earn account is still being set up. Please try again shortly.")
}

type privySmartAccountRecord struct {
	ID                string
	UserID            string
	SettingsPda       string
	State             string
	CreationSignature *string
}

type privySmartAccount struct {
	ProgramID           string
	SettingsPda         string
	SmartAccountAddress string
	CreationSignature   *string
}

type privyRootSignerMembership struct {
	SmartAccountAddress string
	SettingsPda         string
	SourceSignature     *string
	SourceSlot          *int64
}

// privyEnsureSmartAccount ports ensureUserSmartAccount minus sponsorship
// (service.ts:499-676). Every branch the TS machine resolves without sending
// an on-chain create is served — existing ready, reconciled ready, delegated
// root signer — and the branches that would sponsor a create return
// privySponsorshipNeeded instead of signing.
func (h earnRoutes) privyEnsureSmartAccount(ctx context.Context, userID string, wallet solana.PublicKey) (privySmartAccount, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	record, err := h.privyFindSmartAccount(ctx, userID, wallet.String())
	if err != nil {
		return privySmartAccount{}, err
	}
	if record != nil {
		settings, err := h.privyReadSettings(ctx, record.SettingsPda)
		if err != nil {
			return privySmartAccount{}, earnUnavailable("privy reconcile settings", err,
				"smart_account_provisioning_unavailable", "Could not verify your Earn account. Please try again.")
		}
		if settings != nil && privyHasSigner(settings, wallet) {
			if record.State == "ready" {
				return privySmartAccountSummary(*record)
			}
			ready, err := h.privyMarkSmartAccountReady(ctx, *record)
			if err != nil {
				return privySmartAccount{}, earnUnavailable("privy mark ready", err,
					"smart_account_provisioning_unavailable", "Could not confirm your Earn account. Please try again.")
			}
			return privySmartAccountSummary(*ready)
		}
		summary, err := h.privyDelegatedRootSigner(ctx, userID, wallet)
		if err != nil {
			return privySmartAccount{}, err
		}
		if summary != nil {
			return *summary, nil
		}
		return privySmartAccount{}, privySponsorshipNeeded()
	}
	summary, err := h.privyDelegatedRootSigner(ctx, userID, wallet)
	if err != nil {
		return privySmartAccount{}, err
	}
	if summary != nil {
		return *summary, nil
	}
	return privySmartAccount{}, privySponsorshipNeeded()
}

func (h earnRoutes) privyFindSmartAccount(ctx context.Context, userID, wallet string) (*privySmartAccountRecord, error) {
	var record privySmartAccountRecord
	err := h.appDB.QueryRow(ctx,
		`SELECT id::text, user_id::text, settings_pda, state, creation_signature
FROM app_user_smart_accounts
WHERE user_id = $1 AND solana_env = $2`,
		userID, privySmartAccountEnv).
		Scan(&record.ID, &record.UserID, &record.SettingsPda, &record.State, &record.CreationSignature)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read app user smart account: %w", err)
	}
	return &record, nil
}

// privyReadSettings ports findSettingsSignerAddresses' on-chain half
// (onchain.ts:116-160): nil settings means the account is absent or not owned
// by the smart-account program — the TS "missing" reconciliation outcome. A
// decode failure of an owned account is a hard error, as a throwing SDK fetch
// is in TS.
func (h earnRoutes) privyReadSettings(ctx context.Context, settingsPda string) (*squads.Settings, error) {
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

func privyHasSigner(settings *squads.Settings, wallet solana.PublicKey) bool {
	for _, signer := range settings.Signers {
		if signer.Key == wallet {
			return true
		}
	}
	return false
}

func privySmartAccountSummary(record privySmartAccountRecord) (privySmartAccount, error) {
	settings, err := solana.PublicKeyFromBase58(record.SettingsPda)
	if err != nil {
		return privySmartAccount{}, fmt.Errorf("stored settings_pda %q is not a public key", record.SettingsPda)
	}
	smartAccount, _, err := squads.SmartAccountAddress(settings, 0)
	if err != nil {
		return privySmartAccount{}, fmt.Errorf("derive smart account for %s: %w", record.SettingsPda, err)
	}
	return privySmartAccount{
		ProgramID:           squads.ProgramID.String(),
		SettingsPda:         record.SettingsPda,
		SmartAccountAddress: smartAccount.String(),
		CreationSignature:   record.CreationSignature,
	}, nil
}

// privyMarkSmartAccountReady ports markAppUserSmartAccountReady
// (repository.ts:313-355): the guarded WHERE is the reservation's fencing, so
// a row superseded by a concurrent signup fails instead of marking a foreign
// PDA ready. creation_signature is left untouched, as the TS reconcile path
// passes no creationSignature.
func (h earnRoutes) privyMarkSmartAccountReady(ctx context.Context, record privySmartAccountRecord) (*privySmartAccountRecord, error) {
	tag, err := h.appDB.Exec(ctx,
		`UPDATE app_user_smart_accounts
SET state = 'ready', last_checked_at = now(), last_error_code = NULL, last_error_message = NULL, updated_at = now()
WHERE id = $1 AND user_id = $2 AND solana_env = $3 AND settings_pda = $4`,
		record.ID, record.UserID, privySmartAccountEnv, record.SettingsPda)
	if err != nil {
		return nil, fmt.Errorf("mark app user smart account ready: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New("smart account provisioning reservation was superseded before it could be marked ready")
	}
	ready := record
	ready.State = "ready"
	return &ready, nil
}

// privyDelegatedRootSigner ports resolveDelegatedRootSignerMembership
// (service.ts:436-497): a wallet that signs someone's root settings keeps its
// own smart account without any sponsored create. Each stale membership row
// is marked removed exactly as markRootSmartAccountSignerRemoved does, and
// each confirmed one is refreshed via upsertActiveRootSmartAccountSigner.
func (h earnRoutes) privyDelegatedRootSigner(ctx context.Context, userID string, wallet solana.PublicKey) (*privySmartAccount, error) {
	rows, err := h.appDB.Query(ctx,
		`SELECT smart_account_address, settings_pda, source_signature, source_slot
FROM app_smart_account_signers
WHERE solana_env = $1 AND signer_address = $2 AND scope = 'root_settings' AND state = 'active'
ORDER BY source_slot DESC, updated_at DESC, settings_pda DESC`,
		privySmartAccountEnv, wallet.String())
	if err != nil {
		return nil, fmt.Errorf("read root signer memberships: %w", err)
	}
	defer rows.Close()
	var memberships []privyRootSignerMembership
	for rows.Next() {
		var membership privyRootSignerMembership
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
		root, err := h.privyReadSettings(ctx, membership.SettingsPda)
		if err != nil {
			return nil, earnUnavailable("privy root settings", err,
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
			if err := h.privyMarkRootSignerRemoved(ctx, membership.SettingsPda, wallet.String()); err != nil {
				return nil, err
			}
			continue
		}
		err = h.privyUpsertActiveRootSigner(ctx, privyRootSignerUpsert{
			Membership: membership,
			Wallet:     wallet.String(),
			UserID:     userID,
			Permission: int32(rootSigner.Permissions),
		})
		if err != nil {
			return nil, err
		}
		return &privySmartAccount{
			ProgramID:           squads.ProgramID.String(),
			SettingsPda:         membership.SettingsPda,
			SmartAccountAddress: membership.SmartAccountAddress,
			CreationSignature:   membership.SourceSignature,
		}, nil
	}
	return nil, nil
}

// privyMarkRootSignerRemoved ports markRootSmartAccountSignerRemoved
// (repository.ts:635-669) with the no-source call shape: source_signature and
// source_slot are cleared. Affecting zero rows is fine there and here.
func (h earnRoutes) privyMarkRootSignerRemoved(ctx context.Context, settingsPda, signerAddress string) error {
	_, err := h.appDB.Exec(ctx,
		`UPDATE app_smart_account_signers
SET state = 'removed', source_signature = NULL, source_slot = NULL, removed_at = now(), last_checked_at = now(), updated_at = now()
WHERE solana_env = $1 AND settings_pda = $2 AND scope = 'root_settings' AND signer_address = $3`,
		privySmartAccountEnv, settingsPda, signerAddress)
	if err != nil {
		return fmt.Errorf("mark root signer removed: %w", err)
	}
	return nil
}

type privyRootSignerUpsert struct {
	Membership privyRootSignerMembership
	Wallet     string
	UserID     string
	Permission int32
}

// privyUpsertActiveRootSigner ports upsertActiveRootSmartAccountSigner
// (repository.ts:572-633): the four-column conflict target is the
// app_smart_account_signers_env_settings_scope_signer_uidx index, so a
// concurrent refresh or a re-login updates the same row instead of
// duplicating it.
func (h earnRoutes) privyUpsertActiveRootSigner(ctx context.Context, up privyRootSignerUpsert) error {
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
		privySmartAccountEnv, up.Membership.SmartAccountAddress, up.Membership.SettingsPda, up.Wallet,
		up.Permission, up.Membership.SourceSignature, up.Membership.SourceSlot, up.UserID)
	if err != nil {
		return fmt.Errorf("upsert active root signer: %w", err)
	}
	return nil
}
