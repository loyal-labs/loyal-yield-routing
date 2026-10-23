package webapp

// Ports the two earn-policy-refund.server.ts prepare branches the vault unit
// left on Next: kind "policy" (prepareEarnPolicyRefund :439-520, closing one
// route policy through an executeSettingsTransactionSync PolicyRemove) and
// kind "recurring_delegation" (:384-437, revoking one Subscriptions
// delegation). The read helpers are the scan twin's (txreads_refundscan.go);
// the close instruction is squads.PolicyApply with Replace, the same bytes the
// TS client builds (resolvePoliciesForClose + executeSettingsTransactionSync).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// parseRefundPublicKey is parsePublicKey (earn-policy-refund.server.ts:216):
// a malformed body address is an EarnPolicyRefundError, so the Next route
// answers 400 invalid_request naming the field.
func parseRefundPublicKey(value, label string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil || strings.TrimSpace(value) == "" {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_request", label+" is not a valid public key.")
	}
	return key, nil
}

// refundPrepareFailed is route.ts:107-111: every non-refund error inside
// prepareEarnPolicyRefund — chain reads, DB reads, builder failures — becomes
// 500 prepare_failed carrying the error's own message.
func refundPrepareFailed(err error) error {
	message := err.Error()
	if message == "" {
		message = "Failed to prepare refund."
	}
	return earnError(http.StatusInternalServerError, "prepare_failed", message)
}

// SUBSCRIPTIONS_REVOKE_DELEGATION (loyal-actions constants.ts:283); the revoke
// instruction's whole data is this one tag byte.
const subscriptionsRevokeDelegationTag = byte(3)

// prepareRecurringDelegationRefundKind ports the kind "recurring_delegation"
// branch (earn-policy-refund.server.ts:384-437): the merged delegation list
// from the scan state, then one revoke instruction the wallet signs and pays.
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
	dbState, err := h.txreadsRefundDBState(ctx, account, policyAddresses, openDelegations)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	// dbState.recurringDelegations is collectRecurringDelegations
	// (earn-policy-refund-state.server.ts:474): chain entries first, metadata
	// overrides behind them, localeCompare order.
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

	// createSubscriptionRevokeDelegationInstruction
	// (earn-policy-refund.server.ts:183-195): keys [authority s+w, delegation
	// w], data [3], the Subscriptions program.
	revoke := squads.Instruction{
		ProgramID: txreadsSubscriptionsProgramKey,
		Accounts: []solana.AccountMeta{
			{PublicKey: account.Wallet, IsSigner: true, IsWritable: true},
			{PublicKey: delegation, IsWritable: true},
		},
		Data: []byte{subscriptionsRevokeDelegationTag},
	}

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
			VaultIndex:          earnVaultIndex,
		},
	}}, nil
}

// resolvePolicyForClose is resolvePoliciesForClose
// (client.ts:12163-12186) for the one-policy refund: fetchPolicy re-reads the
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

// preparePolicyRefundKind ports the kind "policy" branch
// (earn-policy-refund.server.ts:439-520): liveness facts for the one policy,
// then one executeSettingsTransactionSync carrying its PolicyRemove.
func (h earnRoutes) preparePolicyRefundKind(ctx context.Context, account EarnAccount, policyAddress string) (*refundPrepareOutput, error) {
	policyAccount, err := parseRefundPublicKey(policyAddress, "policyAccount")
	if err != nil {
		return nil, err
	}

	// Promise.all (earn-policy-refund.server.ts:440-451); the order below is
	// the array's order, so the first failure to surface matches on reads that
	// fail together.
	policies, err := h.txreadsScanPolicyAccounts(ctx, account.Settings)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	_, infoReads, err := h.Chain.Accounts(ctx, []solana.PublicKey{policyAccount}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	info := infoReads[0]
	policyAddresses := make([]string, 0, len(policies))
	for _, policy := range policies {
		policyAddresses = append(policyAddresses, policy.address.String())
	}
	openDelegations, err := h.txreadsOpenDelegations(ctx, account.Wallet, account.Vault)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	dbState, err := h.txreadsRefundDBState(ctx, account, []string{policyAccount.String()}, openDelegations)
	if err != nil {
		return nil, refundPrepareFailed(err)
	}
	snapshot, err := fetchEarnVaultRefundSnapshot(ctx, h.Chain, account.Vault, earnUSDCMint(account.Cluster))
	if err != nil {
		return nil, refundPrepareFailed(err)
	}

	// getAccountInfo null check (:452-458).
	var policyLamports uint64
	if info != nil {
		policyLamports = info.Lamports
	} else {
		return nil, earnError(http.StatusConflict, "policy_closed", "Policy account is already closed.")
	}

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

	// findSingleEarnPolicyRefundDbState
	// (earn-policy-refund-state.server.ts:754-787): the one policy's slice of
	// the shared DB state.
	address := policyAccount.String()
	recurringDelegations := dbState.byPolicyAccount[address]
	if recurringDelegations == nil {
		recurringDelegations = []EarnPolicyRefundRecurringDelegation{}
	}
	singleActiveAutodeposit := dbState.activeAutodepositAccounts[address]
	singleActiveManagedVault := dbState.activeManagedVaultAccounts[address]
	singleReferencedByPosition := dbState.activePositionAccounts[address]

	// getBlockedReason (:471-481): chain funds first, the Kamino obligation
	// read only when the token accounts are clean. vaultHoldsChainFunds
	// (earn-policy-refund.server.ts:88) over the same snapshot the route read.
	holdsChainFunds := false
	for _, tokenAccount := range snapshot.TokenAccounts {
		if tokenAccount.Amount > 0 && tokenAccount.Address != snapshot.VaultUsdcATA {
			holdsChainFunds = true
			break
		}
	}
	if !holdsChainFunds {
		holds, err := h.txreadsVaultHoldsKaminoCollateral(ctx, account)
		if err != nil {
			return nil, refundPrepareFailed(err)
		}
		holdsChainFunds = holds
	}
	blockedReason := txreadsPolicyBlockedReason(singleActiveAutodeposit, singleActiveManagedVault, singleReferencedByPosition, holdsChainFunds)

	policy := EarnPolicyRefundScanPolicy{
		Account:                    overviewPolicy.address.String(),
		AccountIndex:               overviewPolicy.accountIndex,
		ActiveAutodeposit:          singleActiveAutodeposit,
		ActiveManagedVault:         singleActiveManagedVault,
		BlockedReason:              blockedReason,
		CanRefund:                  blockedReason == nil,
		DBPresent:                  dbState.routePolicyAccounts[address],
		Lamports:                   &policyLamports,
		RecurringDelegations:       recurringDelegations,
		ReferencedByActivePosition: singleReferencedByPosition,
		Seed:                       strconv.FormatUint(overviewPolicy.seed, 10),
		State:                      overviewPolicy.state,
	}
	if blockedReason != nil {
		return nil, earnError(http.StatusConflict, "policy_active", *blockedReason)
	}

	// prepareClosePoliciesSync (client.ts:11791-11816): the resolver re-read
	// plus one executeSettingsTransactionSync with one PolicyRemove, whose
	// bytes squads.PolicyApply.Replace already builds.
	if err := resolvePolicyForClose(ctx, h, policyAccount, account.Settings); err != nil {
		return nil, refundPrepareFailed(err)
	}
	apply := squads.PolicyApply{
		Settings:  account.Settings,
		RentPayer: account.Wallet,
		Signer:    account.Wallet,
		Replace:   []solana.PublicKey{policyAccount},
	}
	closeInstruction, err := apply.Instruction()
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
			VaultIndex:  earnVaultIndex,
		},
	}}, nil
}
