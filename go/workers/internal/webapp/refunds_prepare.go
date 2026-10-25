package webapp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Ports POST /api/smart-accounts/yield-optimization/policy-refunds/prepare
// (web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/
// prepare/route.ts -> earn-policy-refund.server.ts prepareEarnPolicyRefund
// :324-521) for all three refund kinds: the vault-accounts refund here, the
// policy close and the recurring-delegation revoke in refunds_policy.go.

const refundsPreparePath = "/api/smart-accounts/yield-optimization/policy-refunds/prepare"

const (
	// earn-policy-refund.server.ts:351-364; client.ts:10513-10517.
	vaultEmptyMessage = "Nothing to refund on the Earn vault."
	// client.ts:10456-10458 — a plain Error, so Next answers 500 prepare_failed.
	refundBlockedMessage = "Earn vault still holds token balances outside its USDC account; withdraw the position before refunding vault accounts."
	// client.ts:10514-10516 — also unreachable behind the route's canRefund
	// gate, but the message and status are part of the contract.
	refundNothingMessage = "Nothing to refund: the Earn vault holds no SOL and no token accounts."
)

type refundPrepareKind uint8

const (
	refundKindPolicy refundPrepareKind = iota
	refundKindRecurringDelegation
	refundKindVault
)

type refundPrepareBody struct {
	kind                refundPrepareKind
	policyAccount       string
	recurringDelegation string
}

// parseRefundPrepareBody ports parseEarnPolicyRefundPrepareRequestBody
// (earn-policy-refund-contracts.shared.ts:145-183), message for message.
func parseRefundPrepareBody(raw []byte) (refundPrepareBody, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return refundPrepareBody{}, err
	}
	// assertRecord (contracts.shared.ts:137-143) only refuses non-objects: an
	// array passes and then misses every field, so it dies on the policyAccount
	// check like TS, not on the object check.
	record := map[string]any{}
	switch value := parsed.(type) {
	case map[string]any:
		record = value
	case []any:
	default:
		return refundPrepareBody{}, earnError(400, "invalid_request", "Request body must be an object.")
	}

	kind, hasKind := record["kind"]
	kindText, kindIsText := kind.(string)
	if kindIsText && kindText == "recurring_delegation" {
		text, isText := record["recurringDelegation"].(string)
		if !isText || strings.TrimSpace(text) == "" {
			return refundPrepareBody{}, earnError(400, "invalid_request", "recurringDelegation must be a non-empty string.")
		}
		return refundPrepareBody{kind: refundKindRecurringDelegation, recurringDelegation: strings.TrimSpace(text)}, nil
	}
	if kindIsText && kindText == "vault" {
		return refundPrepareBody{kind: refundKindVault}, nil
	}
	// JSON null or a non-string kind is present, so it is neither undefined
	// nor "policy" and takes the TS kind check before the policyAccount read.
	if hasKind && (!kindIsText || kindText != "policy") {
		return refundPrepareBody{}, earnError(400, "invalid_request", "kind must be one of policy, recurring_delegation, or vault.")
	}
	text, isText := record["policyAccount"].(string)
	if !isText || strings.TrimSpace(text) == "" {
		return refundPrepareBody{}, earnError(400, "invalid_request", "policyAccount must be a non-empty string.")
	}
	return refundPrepareBody{kind: refundKindPolicy, policyAccount: strings.TrimSpace(text)}, nil
}

// makeVaultSignerWritable ports makeSignerWritable (client.ts:2984-2996): the
// vault signs every inner instruction by CPI, so its meta must carry signer
// and writable.
func makeVaultSignerWritable(ix solana.Instruction, vault solana.PublicKey) (squads.Instruction, error) {
	compiled, err := squads.InstructionOf(ix)
	if err != nil {
		return squads.Instruction{}, err
	}
	for i := range compiled.Accounts {
		if compiled.Accounts[i].PublicKey == vault && compiled.Accounts[i].IsSigner {
			compiled.Accounts[i].IsWritable = true
		}
	}
	return compiled, nil
}

func vaultTransferCheckedInner(vaultAta, mint, walletAta, vault solana.PublicKey, amount uint64, decimals uint8) (squads.Instruction, error) {
	transfer := spl.TransferChecked(solana.TokenProgramID, vaultAta, mint, walletAta, vault, amount, decimals)
	return makeVaultSignerWritable(transfer, vault)
}

// vaultCloseAccountInner ports createCloseAccountInstruction(account,
// destination, owner, [], tokenProgram): data [9], metas [account w,
// destination w, authority s], then makeSignerWritable on the vault.
func vaultCloseAccountInner(tokenProgram, account, destination, vault solana.PublicKey) squads.Instruction {
	return squads.Instruction{
		ProgramID: tokenProgram,
		Accounts: []solana.AccountMeta{
			{PublicKey: account, IsWritable: true},
			{PublicKey: destination, IsWritable: true},
			{PublicKey: vault, IsSigner: true, IsWritable: true},
		},
		Data: []byte{9},
	}
}

func vaultSystemTransferInner(vault, wallet solana.PublicKey, lamports uint64) (squads.Instruction, error) {
	return squads.InstructionOf(spl.SystemTransfer(vault, wallet, lamports))
}

// earnRefundBlockedError is client.ts:10455-10459: a plain Error on the Next
// side, so it becomes 500 prepare_failed rather than an EarnPolicyRefundError.
type earnRefundBlockedError struct{}

func (earnRefundBlockedError) Error() string { return refundBlockedMessage }

// buildVaultRefundOperation ports client.ts:10432-10573 from the second
// snapshot the TS builder reads for itself: idle USDC out, every token account
// closed, the vault PDA's SOL swept, one member-sync execution carrying them.
func buildVaultRefundOperation(snapshot earnVaultRefundSnapshot, usdcMint, feePayer, settings solana.PublicKey, accountIndex uint8) ([]WirePreparedInstruction, error) {
	// client.ts:10450-10459. Between the route's gate and this second snapshot
	// the vault can have gained a balance; the refund still refuses.
	for _, tokenAccount := range snapshot.TokenAccounts {
		if tokenAccount.Amount > 0 && tokenAccount.Address != snapshot.VaultUsdcATA {
			return nil, earnRefundBlockedError{}
		}
	}

	walletUsdcAta, err := spl.AssociatedTokenAddress(feePayer, usdcMint, solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	var idleAmount uint64
	for _, tokenAccount := range snapshot.TokenAccounts {
		if tokenAccount.Address == snapshot.VaultUsdcATA {
			idleAmount = tokenAccount.Amount
			break
		}
	}

	var inner []squads.Instruction
	if idleAmount > 0 {
		transfer, err := vaultTransferCheckedInner(snapshot.VaultUsdcATA, usdcMint, walletUsdcAta, snapshot.Vault, idleAmount, 6)
		if err != nil {
			return nil, err
		}
		inner = append(inner, transfer)
	}
	// client.ts:10485-10500: every token account closes, in snapshot order.
	for _, tokenAccount := range snapshot.TokenAccounts {
		inner = append(inner, vaultCloseAccountInner(tokenAccount.Program, tokenAccount.Address, feePayer, snapshot.Vault))
	}
	if snapshot.Lamports > 0 {
		sweep, err := vaultSystemTransferInner(snapshot.Vault, feePayer, snapshot.Lamports)
		if err != nil {
			return nil, err
		}
		inner = append(inner, sweep)
	}
	if len(inner) == 0 {
		return nil, errors.New(refundNothingMessage)
	}

	// client.ts:10519-10537: instructionsToSynchronousTransactionDetailsV2
	// plus prepare.executeTransactionSyncV2 — one instruction, no LUTs.
	execute, err := earnprep.ExecuteMemberSync(earnprep.MemberSync{
		Settings:     settings,
		Vault:        snapshot.Vault,
		AccountIndex: accountIndex,
		Members:      []solana.PublicKey{feePayer},
		Inner:        inner,
	})
	if err != nil {
		return nil, err
	}

	// client.ts:10538-10558: the wallet USDC ATA create rides ahead of the
	// execution only when the idle transfer does.
	instructions := make([]WirePreparedInstruction, 0, 2)
	if idleAmount > 0 {
		create, err := squads.InstructionOf(spl.CreateIdempotentATA(feePayer, feePayer, usdcMint, solana.TokenProgramID))
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, wirePreparedInstruction(create))
	}
	return append(instructions, wirePreparedInstruction(execute)), nil
}

func wirePreparedInstruction(ix squads.Instruction) WirePreparedInstruction {
	keys := make([]WirePreparedInstructionKey, len(ix.Accounts))
	for i, meta := range ix.Accounts {
		keys[i] = WirePreparedInstructionKey{IsSigner: meta.IsSigner, IsWritable: meta.IsWritable, Pubkey: meta.PublicKey.String()}
	}
	return WirePreparedInstruction{
		DataBase64: base64.StdEncoding.EncodeToString(ix.Data),
		Keys:       keys,
		ProgramID:  ix.ProgramID.String(),
	}
}

// RefundPrepareRequest documents the request body for OpenAPI; the handler
// parses the raw bytes so the TS error messages carry over verbatim.
type RefundPrepareRequest struct {
	Kind                *string `json:"kind,omitempty" enum:"policy,recurring_delegation,vault"`
	PolicyAccount       *string `json:"policyAccount,omitempty"`
	RecurringDelegation *string `json:"recurringDelegation,omitempty"`
}

type refundPrepareInput struct {
	SessionInput
	Raw rawJSON
}

type refundPrepareOutput struct {
	Body EarnPolicyRefundPrepareResponse
}

func (h earnRoutes) registerRefunds(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-policy-refunds-prepare",
		Method:      http.MethodPost,
		Path:        refundsPreparePath,
		Summary:     "Prepare the Earn vault-accounts rent refund for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 409, 500, 503),
	}, h.earnPolicyRefundPrepare)
	documentJSONBody[RefundPrepareRequest](api, refundsPreparePath)
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-cross-mint-toggle",
		Method:      http.MethodPost,
		Path:        crossMintTogglePath,
		Summary:     "Enable or pause the Earn cross-mint autoswap for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 409, 503),
	}, h.earnCrossMintToggle)
	documentJSONBody[CrossMintToggleRequest](api, crossMintTogglePath)
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-cross-mint-delete-readiness",
		Method:      http.MethodPost,
		Path:        crossMintDeleteReadinessPath,
		Summary:     "Check which Autoswap policies remain on chain before a delete",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 409, 503),
	}, h.earnCrossMintDeleteReadiness)
	documentJSONBody[CrossMintDeleteReadinessRequest](api, crossMintDeleteReadinessPath)
}

func (h earnRoutes) earnPolicyRefundPrepare(ctx context.Context, in *refundPrepareInput) (*refundPrepareOutput, error) {
	unavailable := func(step string, err error) error {
		return earnUnavailable(step, err, "earn_refund_unavailable", "The Earn refund is temporarily unavailable.")
	}

	// Next order (route.ts:51-73): principal, body, controls. h.account folds
	// the controls check into the principal read, so a request that both fails
	// controls and carries a malformed body gets Go's 409 before the 400.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, unavailable("principal", err)
	}
	body, err := parseRefundPrepareBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	switch body.kind {
	case refundKindRecurringDelegation:
		return h.prepareRecurringDelegationRefundKind(ctx, account, body.recurringDelegation)
	case refundKindPolicy:
		return h.preparePolicyRefundKind(ctx, account, body.policyAccount)
	}

	usdcMint := earnUSDCMint(account.Cluster)
	// earn-policy-refund.server.ts:342-345 reads the DB flags and a snapshot
	// together; the gates judge the same read.
	state, err := findEarnVaultRefundDbState(ctx, h.YieldDB, account.Settings.String())
	if err != nil {
		return nil, unavailable("vault refund state", err)
	}
	firstSnapshot, err := fetchEarnVaultRefundSnapshot(ctx, h.Chain, account.Vault, usdcMint)
	if err != nil {
		return nil, unavailable("vault refund snapshot", err)
	}
	vault := buildEarnVaultRefundEntry(state, firstSnapshot)
	if vault.BlockedReason != nil {
		return nil, earnError(409, "vault_active", *vault.BlockedReason)
	}
	if !vault.CanRefund {
		return nil, earnError(409, "vault_empty", vaultEmptyMessage)
	}

	// client.prepareEarnVaultAccountsRefund re-reads the snapshot and builds
	// from what it read (client.ts:10437-10440).
	secondSnapshot, err := fetchEarnVaultRefundSnapshot(ctx, h.Chain, account.Vault, usdcMint)
	if err != nil {
		return nil, unavailable("vault refund snapshot", err)
	}
	instructions, err := buildVaultRefundOperation(secondSnapshot, usdcMint, account.Wallet, account.Settings, earnprep.EarnVaultIndex)
	if err != nil {
		var blocked earnRefundBlockedError
		if errors.As(err, &blocked) {
			return nil, earnError(500, "prepare_failed", refundBlockedMessage)
		}
		return nil, earnError(500, "prepare_failed", err.Error())
	}

	return &refundPrepareOutput{Body: EarnPolicyRefundPrepareResponse{
		PreparedVaultRefund: &PreparedEarnVaultRefund{
			EstimatedRefundLamports: vault.TotalRefundableLamports,
			Prepared: WirePreparedOperation{
				Instructions:         instructions,
				LookupTableAccounts:  []WireAddressLookupTable{},
				Operation:            "earnVaultAccountsRefund",
				Payer:                account.Wallet.String(),
				ProgramID:            squads.ProgramID.String(),
				RequiresConfirmation: true,
			},
			SettingsPda: account.Settings.String(),
			Vault:       vault,
			VaultIndex:  earnprep.EarnVaultIndex,
		},
	}}, nil
}
