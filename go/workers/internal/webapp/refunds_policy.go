package webapp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func parseRefundPublicKey(value, label string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_request", label+" is not a valid public key.")
	}
	return key, nil
}

func refundPrepareFailed(err error) error {
	message := err.Error()
	if message == "" {
		message = "Failed to prepare refund."
	}
	return earnError(http.StatusInternalServerError, "prepare_failed", message)
}

// The revoke instruction's whole data is this one tag byte.
const subscriptionsRevokeDelegationTag = byte(3)

func buildRecurringDelegationRefundOperation(feePayer, delegation solana.PublicKey) squads.Instruction {
	return squads.Instruction{
		ProgramID: txreadsSubscriptionsProgramKey,
		Accounts: []solana.AccountMeta{
			{PublicKey: feePayer, IsSigner: true, IsWritable: true},
			{PublicKey: delegation, IsWritable: true},
		},
		Data: []byte{subscriptionsRevokeDelegationTag},
	}
}

func (h earnRoutes) prepareRecurringDelegationRefundKind(ctx context.Context, account EarnAccount, delegationAddress string) (*refundPrepareOutput, error) {
	delegation, err := parseRefundPublicKey(delegationAddress, "recurringDelegation")
	if err != nil {
		return nil, err
	}

	policies, err := h.txreadsScanPolicyAccounts(ctx, account.Settings)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	policyAddresses := make([]string, 0, len(policies))
	for _, policy := range policies {
		policyAddresses = append(policyAddresses, policy.address.String())
	}
	openDelegations, err := h.txreadsOpenDelegations(ctx, account.Wallet, account.Vault)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	dbState, err := h.txreadsRefundDBState(ctx, account, earnprep.EarnVaultIndex, policyAddresses, openDelegations)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	delegations := txreadsCollectDelegations(dbState.byPolicyAccount, openDelegations)

	var entry *EarnPolicyRefundRecurringDelegation
	for i := range delegations {
		if delegations[i].Account == delegationAddress {
			entry = &delegations[i]
			break
		}
	}
	if entry == nil {
		return nil, earnError(http.StatusNotFound, "delegation_not_found", "Recurring delegation account was not found.")
	}
	if !entry.CanRefund {
		reason := "Recurring delegation is not reclaimable."
		if entry.BlockedReason != nil {
			reason = *entry.BlockedReason
		}
		return nil, earnError(http.StatusConflict, "delegation_protected", reason)
	}

	revoke := buildRecurringDelegationRefundOperation(account.Wallet, delegation)

	return &refundPrepareOutput{Body: EarnPolicyRefundPrepareResponse{
		PreparedRecurringDelegationRefund: &WirePreparedEarnRecurringDelegationRefund{
			EstimatedRefundLamports: entry.Lamports,
			Prepared: WirePreparedOperation{
				Instructions:         []WirePreparedInstruction{wirePreparedInstruction(revoke)},
				LookupTableAccounts:  []WireAddressLookupTable{},
				Operation:            "earnRecurringDelegationRentRefund",
				Payer:                account.Wallet.String(),
				ProgramID:            txreadsSubscriptionsProgramKey.String(),
				RequiresConfirmation: true,
			},
			RecurringDelegation: *entry,
			SettingsPda:         account.Settings.String(),
			VaultIndex:          earnprep.EarnVaultIndex,
		},
	}}, nil
}

// resolvePolicyForClose is resolvePoliciesForClose
// for the one-policy refund: fetchPolicy re-reads the
// account and refuses a policy owned by another settings PDA. The failure
// texts are the generated fetcher's and the resolver's own; the Next route's
// catch turns both into 500 prepare_failed.
func resolvePolicyForClose(ctx context.Context, h earnRoutes, policy, settings solana.PublicKey) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{policy}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return err
	}
	read := accounts[0]
	if read == nil {
		return errors.New("Unable to find Policy account at " + policy.String())
	}
	if len(read.Data) < 40 || string(read.Data[:8]) != string(squads.PolicyDiscriminator[:]) {
		return errors.New("Unable to find Policy account at " + policy.String())
	}
	if !solana.PublicKeyFromBytes(read.Data[8:40]).Equals(settings) {
		return errors.New("Policy belongs to another vault.")
	}
	return nil
}

func (h earnRoutes) preparePolicyRefundKind(ctx context.Context, account EarnAccount, policyAddress string) (*refundPrepareOutput, error) {
	policyAccount, err := parseRefundPublicKey(policyAddress, "policyAccount")
	if err != nil {
		return nil, err
	}

	policies, err := h.txreadsScanPolicyAccounts(ctx, account.Settings)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	_, infoReads, err := h.Chain.Accounts(ctx, []solana.PublicKey{policyAccount}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	info := infoReads[0]
	if info == nil {
		return nil, earnError(http.StatusConflict, "policy_closed", "Policy account is already closed.")
	}
	policyLamports := info.Lamports

	var overviewPolicy *txreadsPolicySeed
	for i := range policies {
		if policies[i].address == policyAccount {
			overviewPolicy = &policies[i]
			break
		}
	}
	if overviewPolicy == nil {
		return nil, earnError(http.StatusForbidden, "policy_not_owned", "Policy does not belong to this smart account.")
	}

	index := txreadsPolicyVaultIndex(*overviewPolicy)
	facts, err := h.txreadsReadVaultFacts(ctx, account, index, []string{policyAccount.String()})
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	policy := txreadsScanPolicy(*overviewPolicy, map[uint8]txreadsVaultFacts{index: facts})
	policy.Lamports = &policyLamports
	if policy.BlockedReason != nil {
		return nil, earnError(http.StatusConflict, "policy_active", *policy.BlockedReason)
	}

	if err := resolvePolicyForClose(ctx, h, policyAccount, account.Settings); err != nil {
		return nil, refundPrepareFailed(err)
	}
	closeInstruction, err := earnprep.WithdrawPolicyRemoveSync(account.Settings, account.Wallet, account.Wallet, []solana.PublicKey{policyAccount})
	if err != nil {
		return nil, refundPrepareFailed(err)
	}

	return &refundPrepareOutput{Body: EarnPolicyRefundPrepareResponse{
		PreparedRefund: &WirePreparedEarnPolicyRefund{
			EstimatedRefundLamports: policyLamports,
			Policy:                  policy,
			Prepared: WirePreparedOperation{
				Instructions:         []WirePreparedInstruction{wirePreparedInstruction(closeInstruction)},
				LookupTableAccounts:  []WireAddressLookupTable{},
				Operation:            "executeSettingsTransactionSync",
				Payer:                account.Wallet.String(),
				ProgramID:            squads.ProgramID.String(),
				RequiresConfirmation: false,
			},
			SettingsPda: account.Settings.String(),
			VaultIndex:  index,
		},
	}}, nil
}
