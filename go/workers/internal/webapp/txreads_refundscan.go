package webapp

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/autodeposit"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Ports the Next policy-refunds scan
// (web/apps/web/src/app/api/smart-accounts/yield-optimization/policy-refunds/
// scan/route.ts -> earn-policy-refund.server.ts scanEarnPolicyRefunds :228 and
// earn-policy-refund-state.server.ts): the chain and DB liveness facts each
// route policy and the Earn vault need before a refund may run. A read-only
// unit: no DB writes, so re-running it changes nothing.

// txreadsSubscriptionsProgramKey is the Subscriptions program that owns
// recurring-delegation accounts (SUBSCRIPTIONS_PROGRAM_ID, @loyal-labs/actions
// constants.ts:14).
var txreadsSubscriptionsProgramKey = solana.MustPublicKeyFromBase58(autodeposit.SubscriptionsProgramID)

// Subscriptions-program recurring-delegation layout (@loyal-labs/actions
// constants.ts, same offsets as internal/autodeposit/delegation.go). A
// delegation is a zero-copy Anchor account: u8 discriminator, then pubkeys and
// u64 counters.
const (
	txreadsDelegationDiscriminator    = 3
	txreadsDelegationDataLen          = 211
	txreadsDelegationDelegatorOffset  = 3
	txreadsDelegationDelegateeOffset  = 35
	txreadsDelegationAuthorityOffset  = 107
	txreadsDelegationMintOffset       = 139
	txreadsDelegationPerPeriodOffset  = 195
	txreadsDelegationAmountPulledOffs = 203
)

// txreadsLocaleCompare orders account addresses the way the Next route's
// Array.sort((left, right) => left.localeCompare(right)) does: ICU collation
// folds letter case first, breaks the tie lowercase-before-uppercase, and puts
// digits before letters. A byte-wise Go sort would order "B" before "a"; ICU
// puts "a" first. Base58 strings only carry digits and ASCII letters.
func txreadsLocaleCompare(left, right string) int {
	for i := 0; i < len(left) && i < len(right); i++ {
		if order := txreadsCollationRank(left[i], right[i]); order != 0 {
			return order
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}

func txreadsCollationRank(a, b byte) int {
	rankA, lowerA := txreadsCharRank(a)
	rankB, lowerB := txreadsCharRank(b)
	if rankA != rankB {
		if rankA < rankB {
			return -1
		}
		return 1
	}
	if lowerA != lowerB {
		// Same letter, different case: lowercase sorts first.
		if lowerA {
			return -1
		}
		return 1
	}
	return 0
}

func txreadsCharRank(c byte) (int, bool) {
	switch {
	case c >= '1' && c <= '9':
		return int(c - '0'), true
	case c >= 'A' && c <= 'Z':
		return 9 + int(c-'A') + 1, false
	default:
		return 9 + int(c-'a') + 1, true
	}
}

func txreadsSortAddresses(values []string) {
	sort.SliceStable(values, func(i, j int) bool { return txreadsLocaleCompare(values[i], values[j]) < 0 })
}

// txreadsPolicySeed carries the policy-account fields toPolicySnapshot puts in
// a scan entry: seed, the policy state's kind name, and the state's account
// index (SpendingLimit's sourceAccountIndex, ProgramInteraction's
// accountIndex, nothing otherwise). Lamports rides along from the same read
// where the Next scan re-reads the accounts with getMultipleAccountsInfo.
type txreadsPolicySeed struct {
	address      solana.PublicKey
	lamports     uint64
	seed         uint64
	state        string
	accountIndex *uint8
}

var txreadsPolicyStateNames = map[uint8]string{
	0: "InternalFundTransfer",
	1: "SpendingLimit",
	2: "SettingsChange",
	3: "ProgramInteraction",
}

// txreadsDecodePolicyHeader walks the Squads Policy account's fixed header,
// mirroring squads.decodeHeader up to the state kind. Both SpendingLimit and
// ProgramInteraction lead their state body with the vault's account index.
func txreadsDecodePolicyHeader(data []byte) (txreadsPolicySeed, error) {
	cursor := &txreadsCursor{data: data}
	discriminator, err := cursor.take(8)
	if err != nil {
		return txreadsPolicySeed{}, err
	}
	if string(discriminator) != string(squads.PolicyDiscriminator[:]) {
		return txreadsPolicySeed{}, errors.New("account discriminator is not a Squads Policy account")
	}
	if _, err := cursor.take(32); err != nil { // settings
		return txreadsPolicySeed{}, err
	}
	seed, err := cursor.u64()
	if err != nil {
		return txreadsPolicySeed{}, err
	}
	if _, err := cursor.take(1); err != nil { // bump
		return txreadsPolicySeed{}, err
	}
	if _, err := cursor.u64(); err != nil { // transaction index
		return txreadsPolicySeed{}, err
	}
	if _, err := cursor.u64(); err != nil { // stale transaction index
		return txreadsPolicySeed{}, err
	}
	signerCount, err := cursor.u32()
	if err != nil {
		return txreadsPolicySeed{}, err
	}
	if signerCount > 32 {
		return txreadsPolicySeed{}, errors.New("too many policy signers")
	}
	if _, err := cursor.take(int(signerCount) * 33); err != nil { // key + permission mask each
		return txreadsPolicySeed{}, err
	}
	if _, err := cursor.take(2); err != nil { // threshold
		return txreadsPolicySeed{}, err
	}
	if _, err := cursor.take(4); err != nil { // time lock
		return txreadsPolicySeed{}, err
	}
	kind, err := cursor.take(1)
	if err != nil {
		return txreadsPolicySeed{}, err
	}
	name, ok := txreadsPolicyStateNames[kind[0]]
	if !ok {
		return txreadsPolicySeed{}, errors.New("policy state kind is unknown")
	}
	entry := txreadsPolicySeed{seed: seed, state: name}
	if kind[0] == 1 || kind[0] == 3 {
		index, err := cursor.take(1)
		if err != nil {
			return txreadsPolicySeed{}, err
		}
		entry.accountIndex = &index[0]
	}
	return entry, nil
}

type txreadsCursor struct {
	data   []byte
	offset int
}

func (c *txreadsCursor) take(count int) ([]byte, error) {
	if c.offset+count > len(c.data) {
		return nil, errors.New("policy account data ends early")
	}
	taken := c.data[c.offset : c.offset+count]
	c.offset += count
	return taken, nil
}

func (c *txreadsCursor) u64() (uint64, error) {
	taken, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(taken), nil
}

func (c *txreadsCursor) u32() (uint32, error) {
	taken, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(taken), nil
}

// txreadsScanPolicyAccounts ports fetchPolicyAccounts + toPolicySnapshot: the
// settings' policy accounts read off chain at confirmed, sorted by seed.
func (h earnRoutes) txreadsScanPolicyAccounts(ctx context.Context, settings solana.PublicKey) ([]txreadsPolicySeed, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	filters := []rpc.RPCFilter{
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: squads.PolicyDiscriminator[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: settings[:]}},
	}
	_, accounts, err := h.Chain.ProgramAccounts(ctx, squads.ProgramID, filters, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	policies := make([]txreadsPolicySeed, 0, len(accounts))
	for i := range accounts {
		entry, err := txreadsDecodePolicyHeader(accounts[i].Data)
		if err != nil {
			return nil, err
		}
		entry.address = accounts[i].Key
		entry.lamports = accounts[i].Lamports
		policies = append(policies, entry)
	}
	sort.SliceStable(policies, func(i, j int) bool { return policies[i].seed < policies[j].seed })
	return policies, nil
}

// txreadsAutodepositRow is one balance_sweep_targets row joined to its
// balance_sweep_policies policy, the metadata side of a recurring delegation
// the scan inspects.
type txreadsAutodepositRow struct {
	policyAccount       *string
	recurringDelegation *string
	expiryTimestamp     *uint64
	scheduledSweepCount int
	source              string
	targetID            *string
	targetActive        bool
	lifecycleStatus     string
}

// txreadsDelegationUsage resolves what the metadata rows say a delegation is
// doing (resolveRecurringDelegationUsage); protected delegations block their
// policy's refund.
func txreadsDelegationUsage(row txreadsAutodepositRow) (blockedReason *string, protected bool, usage string) {
	switch {
	case row.lifecycleStatus == "pending_delegation":
		reason := "Pending Autodeposit setup"
		return &reason, true, "pending"
	case row.scheduledSweepCount > 0:
		reason := "Scheduled Autodeposit sweep"
		return &reason, true, "scheduled"
	case row.targetID != nil && row.targetActive:
		reason := "Current Autodeposit delegation"
		return &reason, true, "current"
	case row.targetID != nil:
		reason := "Paused Autodeposit delegation"
		return &reason, true, "paused"
	default:
		return nil, false, "unused"
	}
}

func txreadsDelegationCanRefund(exists, protected bool, status string) bool {
	return exists && !protected && (status == "active" || status == "expired")
}

func txreadsStringPtr(value string) *string { return &value }

// txreadsInspectDelegation ports inspectRecurringDelegationAccount: the
// metadata row's usage facts plus the chain account's amounts, expiry and
// validity folded into one response entry.
func txreadsInspectDelegation(account *chain.Account, row txreadsAutodepositRow, nowSeconds uint64) EarnPolicyRefundRecurringDelegation {
	blockedReason, protected, usage := txreadsDelegationUsage(row)
	delegation := EarnPolicyRefundRecurringDelegation{
		Account:             "",
		BlockedReason:       blockedReason,
		ExpiryTimestamp:     txreadsU64StringPtr(row.expiryTimestamp),
		LifecycleStatus:     row.lifecycleStatus,
		Protected:           protected,
		ScheduledSweepCount: row.scheduledSweepCount,
		Source:              row.source,
		TargetActive:        row.targetActive,
		TargetID:            row.targetID,
		Usage:               usage,
	}
	if row.policyAccount != nil {
		delegation.PolicyAccount = txreadsStringPtr(*row.policyAccount)
	}
	if row.recurringDelegation == nil {
		return txreadsFinalizeDelegation(delegation, false, "missing")
	}
	delegation.Account = *row.recurringDelegation
	if account == nil {
		return txreadsFinalizeDelegation(delegation, false, "missing")
	}

	delegation.AmountPerPeriodRaw = txreadsU64At(account.Data, txreadsDelegationPerPeriodOffset)
	delegation.AmountPulledRaw = txreadsU64At(account.Data, txreadsDelegationAmountPulledOffs)
	delegation.Authority = txreadsKeyAt(account.Data, txreadsDelegationAuthorityOffset)
	delegation.Delegatee = txreadsKeyAt(account.Data, txreadsDelegationDelegateeOffset)
	delegation.Delegator = txreadsKeyAt(account.Data, txreadsDelegationDelegatorOffset)
	delegation.Mint = txreadsKeyAt(account.Data, txreadsDelegationMintOffset)
	delegation.Lamports = &account.Lamports

	valid := account.Owner == txreadsSubscriptionsProgramKey &&
		len(account.Data) >= txreadsDelegationDataLen &&
		account.Data[0] == txreadsDelegationDiscriminator
	if !valid {
		return txreadsFinalizeDelegation(delegation, true, "invalid")
	}
	if row.expiryTimestamp != nil && *row.expiryTimestamp > 0 && *row.expiryTimestamp <= nowSeconds {
		return txreadsFinalizeDelegation(delegation, true, "expired")
	}
	return txreadsFinalizeDelegation(delegation, true, "active")
}

func txreadsFinalizeDelegation(delegation EarnPolicyRefundRecurringDelegation, exists bool, status string) EarnPolicyRefundRecurringDelegation {
	delegation.Exists = exists
	delegation.Status = status
	delegation.Active = status == "active"
	delegation.CanRefund = txreadsDelegationCanRefund(exists, delegation.Protected, status)
	return delegation
}

func txreadsU64At(data []byte, offset int) *string {
	if len(data) < offset+8 {
		return nil
	}
	value := binary.LittleEndian.Uint64(data[offset : offset+8])
	return txreadsStringPtr(strconv.FormatUint(value, 10))
}

func txreadsKeyAt(data []byte, offset int) *string {
	if len(data) < offset+32 {
		return nil
	}
	return txreadsStringPtr(solana.PublicKeyFromBytes(data[offset : offset+32]).String())
}

func txreadsU64StringPtr(value *uint64) *string {
	if value == nil {
		return nil
	}
	return txreadsStringPtr(strconv.FormatUint(*value, 10))
}

// txreadsOpenDelegations ports findOpenWalletToEarnVaultRecurringDelegations:
// every Subscriptions recurring-delegation account from the wallet to the Earn
// vault, whether or not any metadata row still references it.
func (h earnRoutes) txreadsOpenDelegations(ctx context.Context, wallet, vaultPubkey solana.PublicKey) ([]EarnPolicyRefundRecurringDelegation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	filters := []rpc.RPCFilter{
		{DataSize: txreadsDelegationDataLen},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: []byte{txreadsDelegationDiscriminator}}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: txreadsDelegationDelegatorOffset, Bytes: wallet[:]}},
		{Memcmp: &rpc.RPCFilterMemcmp{Offset: txreadsDelegationDelegateeOffset, Bytes: vaultPubkey[:]}},
	}
	_, accounts, err := h.Chain.ProgramAccounts(ctx, txreadsSubscriptionsProgramKey, filters, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	nowSeconds := uint64(time.Now().Unix())
	delegations := make([]EarnPolicyRefundRecurringDelegation, 0, len(accounts))
	for i := range accounts {
		delegations = append(delegations, txreadsInspectDelegation(&accounts[i], txreadsAutodepositRow{
			recurringDelegation: txreadsStringPtr(accounts[i].Key.String()),
			source:              "chain",
			lifecycleStatus:     "chain_open",
		}, nowSeconds))
	}
	txreadsSortDelegations(delegations)
	return delegations, nil
}

func txreadsSortDelegations(values []EarnPolicyRefundRecurringDelegation) {
	sort.SliceStable(values, func(i, j int) bool {
		return txreadsLocaleCompare(values[i].Account, values[j].Account) < 0
	})
}

// txreadsDelegationAccountsByAddress reads the metadata-referenced delegation
// accounts in one confirmed multi-read, absent accounts coming back nil.
func (h earnRoutes) txreadsDelegationAccountsByAddress(ctx context.Context, addresses []string) (map[string]*chain.Account, error) {
	if len(addresses) == 0 {
		return map[string]*chain.Account{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys := make([]solana.PublicKey, 0, len(addresses))
	seen := map[string]bool{}
	for _, address := range addresses {
		if seen[address] {
			continue
		}
		seen[address] = true
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			continue
		}
		keys = append(keys, key)
	}
	_, accounts, err := h.Chain.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	byAddress := make(map[string]*chain.Account, len(accounts))
	for i, account := range accounts {
		if account == nil || i >= len(keys) {
			continue
		}
		byAddress[keys[i].String()] = account
	}
	return byAddress, nil
}

// txreadsRefundDBState is EarnPolicyRefundDbState: the loyal_yield facts that
// say whether a policy or the vault is still in use.
type txreadsRefundDBState struct {
	activeAutodepositAccounts  map[string]bool
	activeManagedVaultAccounts map[string]bool
	activePositionAccounts     map[string]bool
	byPolicyAccount            map[string][]EarnPolicyRefundRecurringDelegation
	routePolicyAccounts        map[string]bool
}

const txreadsRefundPolicyRowsSQL = `SELECT id::text, policy_account
FROM loyal_yield.route_policies
WHERE settings = $1 AND vault_index = $2 AND policy_account = ANY($3)`

const txreadsRefundManagedVaultsSQL = `SELECT active_policy_id::text, setup_policy_id::text
FROM loyal_yield.managed_vaults
WHERE active = TRUE AND settings = $1 AND vault_index = $2`

const txreadsRefundActivePositionsSQL = `SELECT policy_account
FROM loyal_yield.user_yield_positions
WHERE settings = $1 AND vault_index = $2
  AND status = 'active'::loyal_yield.yield_position_status
  AND policy_account = ANY($3)`

const txreadsRefundAutodepositRowsSQL = `SELECT t.id::text, p.policy_account, t.recurring_delegation,
  t.recurring_delegation_expiry_timestamp, t.desired_active, t.chain_status
FROM loyal_yield.balance_sweep_policies p
INNER JOIN loyal_yield.balance_sweep_targets t ON t.balance_sweep_policy_id = p.id
WHERE p.active = TRUE AND p.settings = $1 AND p.policy_type = 'subscription_sweep'
  AND p.vault_index = $2 AND p.policy_account = ANY($3)
  AND t.policy_account = ANY($3) AND t.settings = $1 AND t.vault_index = $2
  AND t.chain_status <> 'closed'`

const txreadsRefundScheduledSweepsSQL = `SELECT pending.target_id::text, COUNT(*)::int FROM (
  SELECT target_id FROM loyal_yield.balance_sweep_surplus_lots
  WHERE target_id = ANY($1) AND status IN ('open', 'selected') AND remaining_amount_raw > 0
  UNION ALL
  SELECT target_id FROM loyal_yield.balance_sweep_lot_claims
  WHERE target_id = ANY($1) AND status = 'selected'
) pending GROUP BY pending.target_id`

// txreadsVaultDBFlags is findEarnVaultRefundDbState: vault-level liveness
// flags, any non-closed autodeposit target counting as active.
type txreadsVaultDBFlags struct {
	hasActiveAutodeposit  bool
	hasActiveManagedVault bool
	hasActivePosition     bool
}

const txreadsVaultFlagsSQL = `SELECT
  EXISTS(SELECT 1 FROM loyal_yield.user_yield_positions
    WHERE settings = $1 AND vault_index = $2 AND status = 'active'::loyal_yield.yield_position_status),
  EXISTS(SELECT 1 FROM loyal_yield.managed_vaults
    WHERE active = TRUE AND settings = $1 AND vault_index = $2),
  EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets
    WHERE settings = $1 AND vault_index = $2 AND chain_status <> 'closed')`

func (h earnRoutes) txreadsVaultDBFlags(ctx context.Context, account EarnAccount) (txreadsVaultDBFlags, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var flags txreadsVaultDBFlags
	err := h.YieldDB.QueryRow(ctx, txreadsVaultFlagsSQL,
		account.Settings.String(), int16(earnVaultIndex)).Scan(
		&flags.hasActivePosition, &flags.hasActiveManagedVault, &flags.hasActiveAutodeposit)
	return flags, err
}

// txreadsRefundDBState ports findEarnPolicyRefundDbState. With no policy
// accounts the DB contributes nothing and only the open wallet-to-vault
// delegations remain; the caller supplies those.
func (h earnRoutes) txreadsRefundDBState(ctx context.Context, account EarnAccount, policyAddresses []string, openDelegations []EarnPolicyRefundRecurringDelegation) (txreadsRefundDBState, error) {
	state := txreadsRefundDBState{
		activeAutodepositAccounts:  map[string]bool{},
		activeManagedVaultAccounts: map[string]bool{},
		activePositionAccounts:     map[string]bool{},
		byPolicyAccount:            map[string][]EarnPolicyRefundRecurringDelegation{},
		routePolicyAccounts:        map[string]bool{},
	}
	if len(policyAddresses) == 0 {
		return state, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	policyIDByAccount := map[string]string{}
	policyAccountByID := map[string]string{}
	rows, err := h.YieldDB.Query(ctx, txreadsRefundPolicyRowsSQL,
		account.Settings.String(), int16(earnVaultIndex), policyAddresses)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, policyAccount string
		if err := rows.Scan(&id, &policyAccount); err != nil {
			return state, err
		}
		state.routePolicyAccounts[policyAccount] = true
		policyIDByAccount[policyAccount] = id
		policyAccountByID[id] = policyAccount
	}
	if err := rows.Err(); err != nil {
		return state, err
	}

	vaultRows, err := h.YieldDB.Query(ctx, txreadsRefundManagedVaultsSQL,
		account.Settings.String(), int16(earnVaultIndex))
	if err != nil {
		return state, err
	}
	defer vaultRows.Close()
	for vaultRows.Next() {
		var activePolicyID, setupPolicyID *string
		if err := vaultRows.Scan(&activePolicyID, &setupPolicyID); err != nil {
			return state, err
		}
		for _, id := range []*string{activePolicyID, setupPolicyID} {
			if id == nil {
				continue
			}
			if policyAccount, ok := policyAccountByID[*id]; ok {
				state.activeManagedVaultAccounts[policyAccount] = true
			}
		}
	}
	if err := vaultRows.Err(); err != nil {
		return state, err
	}

	positionRows, err := h.YieldDB.Query(ctx, txreadsRefundActivePositionsSQL,
		account.Settings.String(), int16(earnVaultIndex), policyAddresses)
	if err != nil {
		return state, err
	}
	defer positionRows.Close()
	for positionRows.Next() {
		var policyAccount string
		if err := positionRows.Scan(&policyAccount); err != nil {
			return state, err
		}
		state.activePositionAccounts[policyAccount] = true
	}
	if err := positionRows.Err(); err != nil {
		return state, err
	}

	autodepositRows, err := h.txreadsAutodepositRows(ctx, account, policyAddresses)
	if err != nil {
		return state, err
	}
	sweepCounts, err := h.txreadsScheduledSweepCounts(ctx, autodepositRows)
	if err != nil {
		return state, err
	}
	autodepositPolicyAccounts := map[string]bool{}
	for i := range autodepositRows {
		autodepositRows[i].scheduledSweepCount = sweepCounts[*autodepositRows[i].targetID]
		if autodepositRows[i].policyAccount != nil {
			autodepositPolicyAccounts[*autodepositRows[i].policyAccount] = true
		}
	}
	accounts, err := h.txreadsDelegationAccountsByAddress(ctx, txreadsDelegationAddresses(autodepositRows))
	if err != nil {
		return state, err
	}
	nowSeconds := uint64(time.Now().Unix())
	for i := range autodepositRows {
		row := autodepositRows[i]
		if row.recurringDelegation == nil || row.policyAccount == nil {
			continue
		}
		delegation := txreadsInspectDelegation(accounts[*row.recurringDelegation], row, nowSeconds)
		state.byPolicyAccount[*row.policyAccount] = append(state.byPolicyAccount[*row.policyAccount], delegation)
	}
	for policyAccount := range autodepositPolicyAccounts {
		state.byPolicyAccount[policyAccount] = txreadsMergeDelegations(state.byPolicyAccount[policyAccount], openDelegations)
	}
	for policyAccount, delegations := range state.byPolicyAccount {
		for _, delegation := range delegations {
			if delegation.Exists && delegation.Protected {
				state.activeAutodepositAccounts[policyAccount] = true
				break
			}
		}
	}
	return state, nil
}

func txreadsDelegationAddresses(rows []txreadsAutodepositRow) []string {
	addresses := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.recurringDelegation != nil {
			addresses = append(addresses, *row.recurringDelegation)
		}
	}
	return addresses
}

func (h earnRoutes) txreadsAutodepositRows(ctx context.Context, account EarnAccount, policyAddresses []string) ([]txreadsAutodepositRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, txreadsRefundAutodepositRowsSQL,
		account.Settings.String(), int16(earnVaultIndex), policyAddresses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []txreadsAutodepositRow
	for rows.Next() {
		var (
			id              string
			policyAccount   string
			recurringDeleg  *string
			expiryTimestamp *uint64
			targetActive    bool
			lifecycleStatus string
		)
		if err := rows.Scan(&id, &policyAccount, &recurringDeleg, &expiryTimestamp, &targetActive, &lifecycleStatus); err != nil {
			return nil, err
		}
		out = append(out, txreadsAutodepositRow{
			policyAccount:       &policyAccount,
			recurringDelegation: recurringDeleg,
			expiryTimestamp:     expiryTimestamp,
			source:              "metadata",
			targetID:            &id,
			targetActive:        targetActive,
			lifecycleStatus:     lifecycleStatus,
		})
	}
	return out, rows.Err()
}

func (h earnRoutes) txreadsScheduledSweepCounts(ctx context.Context, rows []txreadsAutodepositRow) (map[string]int, error) {
	if len(rows) == 0 {
		return map[string]int{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	targetIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.targetID == nil {
			continue
		}
		id, err := strconv.ParseInt(*row.targetID, 10, 64)
		if err != nil {
			continue
		}
		targetIDs = append(targetIDs, id)
	}
	if len(targetIDs) == 0 {
		return map[string]int{}, nil
	}
	counts, err := h.YieldDB.Query(ctx, txreadsRefundScheduledSweepsSQL, targetIDs)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	byTarget := map[string]int{}
	for counts.Next() {
		var targetID string
		var count int
		if err := counts.Scan(&targetID, &count); err != nil {
			return nil, err
		}
		byTarget[targetID] = count
	}
	return byTarget, counts.Err()
}

// txreadsMergeDelegations ports mergeRecurringDelegations: chain facts win on
// the account itself, metadata wins on usage and protection.
func txreadsMergeDelegations(metadata, chainDelegations []EarnPolicyRefundRecurringDelegation) []EarnPolicyRefundRecurringDelegation {
	mergedByAccount := map[string]EarnPolicyRefundRecurringDelegation{}
	order := make([]string, 0, len(metadata)+len(chainDelegations))
	for _, delegation := range metadata {
		if _, seen := mergedByAccount[delegation.Account]; !seen {
			order = append(order, delegation.Account)
		}
		mergedByAccount[delegation.Account] = delegation
	}
	for _, delegation := range chainDelegations {
		if _, seen := mergedByAccount[delegation.Account]; !seen {
			order = append(order, delegation.Account)
		}
		merged := delegation
		if metadataDelegation, ok := mergedByAccount[delegation.Account]; ok {
			merged.BlockedReason = metadataDelegation.BlockedReason
			merged.CanRefund = txreadsDelegationCanRefund(delegation.Exists, metadataDelegation.Protected, delegation.Status)
			merged.LifecycleStatus = metadataDelegation.LifecycleStatus
			merged.PolicyAccount = metadataDelegation.PolicyAccount
			merged.Protected = metadataDelegation.Protected
			merged.ScheduledSweepCount = metadataDelegation.ScheduledSweepCount
			merged.TargetActive = metadataDelegation.TargetActive
			merged.TargetID = metadataDelegation.TargetID
			merged.Usage = metadataDelegation.Usage
		}
		mergedByAccount[delegation.Account] = merged
	}
	merged := make([]EarnPolicyRefundRecurringDelegation, 0, len(order))
	for _, account := range order {
		merged = append(merged, mergedByAccount[account])
	}
	txreadsSortDelegations(merged)
	return merged
}

// txreadsCollectDelegations ports collectRecurringDelegations: chain
// delegations first, then every metadata row's, deduplicated and sorted.
func txreadsCollectDelegations(byPolicyAccount map[string][]EarnPolicyRefundRecurringDelegation, chainDelegations []EarnPolicyRefundRecurringDelegation) []EarnPolicyRefundRecurringDelegation {
	byAccount := map[string]EarnPolicyRefundRecurringDelegation{}
	order := make([]string, 0)
	for _, delegation := range chainDelegations {
		if _, seen := byAccount[delegation.Account]; !seen {
			order = append(order, delegation.Account)
		}
		byAccount[delegation.Account] = delegation
	}
	for _, delegations := range byPolicyAccount {
		for _, delegation := range delegations {
			if _, seen := byAccount[delegation.Account]; !seen {
				order = append(order, delegation.Account)
			}
			byAccount[delegation.Account] = delegation
		}
	}
	collected := make([]EarnPolicyRefundRecurringDelegation, 0, len(order))
	for _, account := range order {
		collected = append(collected, byAccount[account])
	}
	txreadsSortDelegations(collected)
	return collected
}

// txreadsUSDCMint is getStablecoinMintForCluster(cluster, USDC)
// (@loyal-labs/actions constants.ts:136-146); dtarget_target.go carries the
// same two addresses.
func txreadsUSDCMint(cluster LoyalCluster) solana.PublicKey {
	if cluster == LoyalDevnet {
		return solana.MustPublicKeyFromBase58(dtargetDevnetUSDCMint)
	}
	return solana.MustPublicKeyFromBase58(dtargetUSDCMint)
}

// txreadsVaultSnapshot is fetchEarnVaultRefundSnapshot
// (smart-account-vaults client.ts:10339): the vault's SOL and every token
// account it owns across both token programs, plus its USDC ATA so the
// chain-funds check can exempt it.
type txreadsVaultSnapshot struct {
	lamports      uint64
	tokenAccounts []EarnVaultRefundTokenAccount
	vaultUsdcAta  solana.PublicKey
}

// txreadsVaultHoldsChainFunds is vaultHoldsChainFunds
// (earn-policy-refund.server.ts:88): any token account other than the vault's
// own USDC ATA still carrying a balance is a live on-chain position.
func txreadsVaultHoldsChainFunds(snapshot txreadsVaultSnapshot) bool {
	for _, tokenAccount := range snapshot.tokenAccounts {
		if tokenAccount.AmountRaw != "0" && tokenAccount.Account != snapshot.vaultUsdcAta.String() {
			return true
		}
	}
	return false
}

// txreadsVaultSnapshot reads the vault at confirmed. The SOL balance mirrors
// getVaultSweepLamportsOrZero (:3134): a failed read contributes zero instead
// of failing the scan.
func (h earnRoutes) txreadsVaultSnapshot(ctx context.Context, account EarnAccount, usdcMint solana.PublicKey) (txreadsVaultSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vault := account.Vault
	snapshot := txreadsVaultSnapshot{tokenAccounts: []EarnVaultRefundTokenAccount{}}
	usdcAta, err := spl.AssociatedTokenAddress(vault, usdcMint, solana.TokenProgramID)
	if err != nil {
		return snapshot, err
	}
	snapshot.vaultUsdcAta = usdcAta
	if _, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{vault}, rpc.CommitmentConfirmed, 0); err == nil && len(accounts) > 0 && accounts[0] != nil {
		snapshot.lamports = accounts[0].Lamports
	}
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		_, accounts, err := h.Chain.TokenAccounts(ctx, vault, program, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return snapshot, err
		}
		for i := range accounts {
			data := accounts[i].Data
			if len(data) < 72 {
				return snapshot, errors.New("vault token account data is too short")
			}
			mint := solana.PublicKeyFromBytes(data[:32])
			amount := binary.LittleEndian.Uint64(data[64:72])
			snapshot.tokenAccounts = append(snapshot.tokenAccounts, EarnVaultRefundTokenAccount{
				Account:   accounts[i].Key.String(),
				AmountRaw: strconv.FormatUint(amount, 10),
				IsUsdc:    mint == usdcMint,
				Lamports:  accounts[i].Lamports,
				Mint:      mint.String(),
			})
		}
	}
	sort.SliceStable(snapshot.tokenAccounts, func(i, j int) bool {
		return txreadsLocaleCompare(snapshot.tokenAccounts[i].Account, snapshot.tokenAccounts[j].Account) < 0
	})
	return snapshot, nil
}

// txreadsVaultHoldsKaminoCollateral is vaultHoldsKaminoCollateral
// (earn-policy-refund.server.ts:101): every Safe market for every product
// mint, read through the shared holdings snapshot with no dependence on DB
// policy rows (they may be missing, ASK-2252), failing closed on unreadable
// reserves.
func (h earnRoutes) txreadsVaultHoldsKaminoCollateral(ctx context.Context, account EarnAccount) (bool, error) {
	mints := make([]string, 0, 6)
	for _, product := range dtargetProductMintsForCluster(account.Cluster) {
		mints = append(mints, product.Mint)
	}
	markets := dtargetSafeMarkets[account.Cluster]
	snapshot, err := earnprep.FetchWithdrawHoldingsSnapshot(ctx, h.Chain, earnprep.WithdrawHoldingsArgs{
		Cluster: string(account.Cluster),
		Policy: &earnprep.WithdrawPolicyUniverse{
			VaultIndex:           earnVaultIndex,
			StableMints:          mints,
			KaminoLiquidityMints: mints,
			KaminoMarkets:        markets,
		},
		SettingsPda:                 account.Settings,
		RequireCompleteReserveReads: true,
	})
	if err != nil {
		return false, err
	}
	for _, holding := range snapshot.Holdings {
		if holding.Kind == "kamino" && holding.AmountRaw > 0 {
			return true, nil
		}
	}
	return false, nil
}

// txreadsPolicyBlockedReason is getBlockedReason
// (earn-policy-refund.server.ts:63): funds visible on chain block every
// policy refund before the read-model flags are consulted.
func txreadsPolicyBlockedReason(activeAutodeposit, activeManagedVault, referencedByActivePosition, vaultHoldsChainFunds bool) *string {
	switch {
	case vaultHoldsChainFunds:
		return txreadsStringPtr("Vault still holds funds on chain")
	case referencedByActivePosition:
		return txreadsStringPtr("Active Earn position")
	case activeAutodeposit:
		return txreadsStringPtr("Protected recurring delegation")
	case activeManagedVault:
		return txreadsStringPtr("Active Earn vault policy")
	default:
		return nil
	}
}

// txreadsVaultBlockedReason is buildVaultRefundEntry's ladder
// (earn-policy-refund.server.ts:150-159); the vault's own autodeposit flag
// reads "Active Autodeposit", not the per-policy wording.
func txreadsVaultBlockedReason(snapshot txreadsVaultSnapshot, flags txreadsVaultDBFlags) *string {
	holdsChainFunds := txreadsVaultHoldsChainFunds(snapshot)
	switch {
	case holdsChainFunds:
		return txreadsStringPtr("Vault still holds funds on chain")
	case flags.hasActivePosition:
		return txreadsStringPtr("Active Earn position")
	case flags.hasActiveAutodeposit:
		return txreadsStringPtr("Active Autodeposit")
	case flags.hasActiveManagedVault:
		return txreadsStringPtr("Active Earn vault policy")
	default:
		return nil
	}
}

// txreadsVaultRefundEntry is buildVaultRefundEntry
// (earn-policy-refund.server.ts:141): everything here is chain-derived, so it
// surfaces even when the DB rows for the position are long gone. The scan's
// copy is never null; NextResponse.json always carries the entry.
func txreadsVaultRefundEntry(account EarnAccount, snapshot txreadsVaultSnapshot, flags txreadsVaultDBFlags) *EarnPolicyRefundVaultEntry {
	blockedReason := txreadsVaultBlockedReason(snapshot, flags)
	total := snapshot.lamports
	for _, tokenAccount := range snapshot.tokenAccounts {
		total += tokenAccount.Lamports
	}
	return &EarnPolicyRefundVaultEntry{
		Account:                 account.Vault.String(),
		BlockedReason:           blockedReason,
		CanRefund:               blockedReason == nil && total > 0,
		Lamports:                snapshot.lamports,
		TokenAccounts:           snapshot.tokenAccounts,
		TotalRefundableLamports: total,
	}
}

// txreadsScanPolicyRefunds ports scanEarnPolicyRefunds
// (earn-policy-refund.server.ts:228): the chain and DB liveness facts each
// route policy and the Earn vault need before a refund may run.
func (h earnRoutes) txreadsScanPolicyRefunds(ctx context.Context, account EarnAccount) (EarnPolicyRefundScanResponse, error) {
	policies, err := h.txreadsScanPolicyAccounts(ctx, account.Settings)
	if err != nil {
		return EarnPolicyRefundScanResponse{}, err
	}
	policyAddresses := make([]string, 0, len(policies))
	for _, policy := range policies {
		policyAddresses = append(policyAddresses, policy.address.String())
	}
	openDelegations, err := h.txreadsOpenDelegations(ctx, account.Wallet, account.Vault)
	if err != nil {
		return EarnPolicyRefundScanResponse{}, err
	}
	dbState, err := h.txreadsRefundDBState(ctx, account, policyAddresses, openDelegations)
	if err != nil {
		return EarnPolicyRefundScanResponse{}, err
	}
	snapshot, err := h.txreadsVaultSnapshot(ctx, account, txreadsUSDCMint(account.Cluster))
	if err != nil {
		return EarnPolicyRefundScanResponse{}, err
	}
	vaultFlags, err := h.txreadsVaultDBFlags(ctx, account)
	if err != nil {
		return EarnPolicyRefundScanResponse{}, err
	}
	holdsChainFunds := txreadsVaultHoldsChainFunds(snapshot)
	if !holdsChainFunds {
		holds, err := h.txreadsVaultHoldsKaminoCollateral(ctx, account)
		if err != nil {
			return EarnPolicyRefundScanResponse{}, err
		}
		holdsChainFunds = holds
	}

	scanPolicies := make([]EarnPolicyRefundScanPolicy, 0, len(policies))
	for _, policy := range policies {
		address := policy.address.String()
		activeManagedVault := dbState.activeManagedVaultAccounts[address]
		activeAutodeposit := dbState.activeAutodepositAccounts[address]
		referencedByActivePosition := dbState.activePositionAccounts[address]
		blockedReason := txreadsPolicyBlockedReason(activeAutodeposit, activeManagedVault, referencedByActivePosition, holdsChainFunds)
		lamports := policy.lamports
		delegations := dbState.byPolicyAccount[address]
		if delegations == nil {
			delegations = []EarnPolicyRefundRecurringDelegation{}
		}
		scanPolicies = append(scanPolicies, EarnPolicyRefundScanPolicy{
			Account:                    address,
			AccountIndex:               policy.accountIndex,
			ActiveAutodeposit:          activeAutodeposit,
			ActiveManagedVault:         activeManagedVault,
			BlockedReason:              blockedReason,
			CanRefund:                  blockedReason == nil,
			DBPresent:                  dbState.routePolicyAccounts[address],
			Lamports:                   &lamports,
			RecurringDelegations:       delegations,
			ReferencedByActivePosition: referencedByActivePosition,
			Seed:                       strconv.FormatUint(policy.seed, 10),
			State:                      policy.state,
		})
	}

	return EarnPolicyRefundScanResponse{
		Policies:             scanPolicies,
		RecurringDelegations: txreadsCollectDelegations(dbState.byPolicyAccount, openDelegations),
		SettingsPDA:          account.Settings.String(),
		Vault:                txreadsVaultRefundEntry(account, snapshot, vaultFlags),
		VaultIndex:           earnVaultIndex,
		VaultPubkey:          account.Vault.String(),
	}, nil
}
