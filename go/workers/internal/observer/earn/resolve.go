package earn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	sp "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
)

// Confirmed RPC proofs for direct Earn projection, ported from
// earn_reconciliation.rs resolve_rpc_mutation and its readers. Direct Earn
// projection follows the confirmed LaserStream frontier.

// errProofPending: the node cannot serve the transaction yet. The durable job
// queue retries it with backoff; the Rust in-process 2/5/10 s re-reads were a
// second retry layer on top of that queue and are not carried over.
var errProofPending = errors.New("transaction proof is not yet available at the requested commitment")

var (
	obligationDiscriminator = []byte{168, 206, 141, 106, 88, 76, 172, 167}
	reserveDiscriminator    = func() []byte { d := anchorAccountDiscriminator("Reserve"); return d[:] }()
	withdrawV2Discriminator = anchorInstructionDiscriminator("withdraw_obligation_collateral_and_redeem_reserve_collateral_v2")
)

const (
	obligationLength = 3344
	reserveLength    = 8624
	dustThreshold    = 10_000
)

func anchorAccountDiscriminator(name string) [8]byte {
	return [8]byte(sha256Sum("account:" + name)[:8])
}

func anchorInstructionDiscriminator(name string) []byte {
	return sha256Sum("global:" + name)[:8]
}

// jsonTransaction is a confirmed jsonParsed transaction.
type jsonTransaction map[string]any

func (t jsonTransaction) pointer(path ...any) any {
	var value any = map[string]any(t)
	for _, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			value = object[key]
		case int:
			array, ok := value.([]any)
			if !ok || key < 0 || key >= len(array) {
				return nil
			}
			value = array[key]
		}
	}
	return value
}

func asU64(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(number.String(), 10, 64)
	return parsed, err == nil
}

func (t jsonTransaction) slot() (uint64, bool) { return asU64(t.pointer("slot")) }

func (t jsonTransaction) accountKeys() []string {
	keys, _ := t.pointer("transaction", "message", "accountKeys").([]any)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		switch value := key.(type) {
		case string:
			out = append(out, value)
		case map[string]any:
			if pubkey, ok := value["pubkey"].(string); ok {
				out = append(out, pubkey)
			}
		}
	}
	return out
}

func (t jsonTransaction) accountSet() map[string]struct{} {
	set := map[string]struct{}{}
	for _, key := range t.accountKeys() {
		set[key] = struct{}{}
	}
	return set
}

func (t jsonTransaction) keyIndex(address string) int {
	keys, _ := t.pointer("transaction", "message", "accountKeys").([]any)
	for index, key := range keys {
		switch value := key.(type) {
		case string:
			if value == address {
				return index
			}
		case map[string]any:
			if value["pubkey"] == address {
				return index
			}
		}
	}
	return -1
}

func (t jsonTransaction) instructions() []map[string]any {
	var out []map[string]any
	outer, _ := t.pointer("transaction", "message", "instructions").([]any)
	for _, instruction := range outer {
		if object, ok := instruction.(map[string]any); ok {
			out = append(out, object)
		}
	}
	groups, _ := t.pointer("meta", "innerInstructions").([]any)
	for _, group := range groups {
		object, _ := group.(map[string]any)
		inner, _ := object["instructions"].([]any)
		for _, instruction := range inner {
			if object, ok := instruction.(map[string]any); ok {
				out = append(out, object)
			}
		}
	}
	return out
}

// readEarnTransaction is read_transaction_json: a confirmed jsonParsed
// transaction that must have succeeded.
func readEarnTransaction(ctx context.Context, rpc *solanarpc.Client, signature string) (jsonTransaction, error) {
	if _, err := solana.SignatureFromBase58(signature); err != nil {
		return nil, fmt.Errorf("invalid transaction signature: %w", err)
	}
	raw, found, err := rpc.Transaction(ctx, signature, "jsonParsed", confirmedCommitment)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProofPending
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var transaction jsonTransaction
	if err := decoder.Decode(&transaction); err != nil {
		return nil, err
	}
	if failure := transaction.pointer("meta", "err"); failure != nil {
		return nil, fmt.Errorf("transaction %s failed on chain", signature)
	}
	return transaction, nil
}

func (t jsonTransaction) slotAt(signature string, expected uint64, what string) (uint64, error) {
	slot, ok := t.slot()
	if !ok {
		return 0, fmt.Errorf("confirmed %stransaction has no slot", what)
	}
	if slot != expected {
		return 0, fmt.Errorf("transaction %s landed at slot %d, expected account-update slot %d", signature, slot, expected)
	}
	return slot, nil
}

// ownerLamportCredit is transaction_owner_lamport_credit (fee added back for
// the fee payer).
func (t jsonTransaction) ownerLamportCredit(owner string) (uint64, error) {
	if _, ok := t.pointer("transaction", "message", "accountKeys").([]any); !ok {
		return 0, errors.New("transaction has no account keys")
	}
	index := t.keyIndex(owner)
	if index < 0 {
		return 0, nil
	}
	pre, _ := asU64(t.pointer("meta", "preBalances", index))
	post, _ := asU64(t.pointer("meta", "postBalances", index))
	var fee uint64
	if index == 0 {
		fee, _ = asU64(t.pointer("meta", "fee"))
	}
	credit := post + fee
	if credit < post {
		credit = ^uint64(0)
	}
	if credit < pre {
		return 0, nil
	}
	return credit - pre, nil
}

func (t jsonTransaction) closedAccount(address string) bool {
	index := t.keyIndex(address)
	if index < 0 {
		return false
	}
	pre, okPre := asU64(t.pointer("meta", "preBalances", index))
	post, okPost := asU64(t.pointer("meta", "postBalances", index))
	return okPre && pre > 0 && okPost && post == 0
}

type tokenRow struct {
	owner  *string
	amount uint64
}

func (t jsonTransaction) tokenRows(name, mint string) (map[uint64]tokenRow, error) {
	rows := map[uint64]tokenRow{}
	values, _ := t.pointer("meta", name).([]any)
	for _, value := range values {
		row, _ := value.(map[string]any)
		if row["mint"] != mint {
			continue
		}
		index, ok := asU64(row["accountIndex"])
		if !ok {
			return nil, errors.New("token balance has no account index")
		}
		ui, _ := row["uiTokenAmount"].(map[string]any)
		text, ok := ui["amount"].(string)
		if !ok {
			return nil, errors.New("token balance has no raw amount")
		}
		amount, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return nil, err
		}
		var owner *string
		if value, ok := row["owner"].(string); ok {
			owner = &value
		}
		if _, exists := rows[index]; exists {
			return nil, fmt.Errorf("duplicate token balance account index %d in %s", index, name)
		}
		rows[index] = tokenRow{owner, amount}
	}
	return rows, nil
}

// ownerTokenDelta is transaction_owner_token_delta.
func (t jsonTransaction) ownerTokenDelta(mint, owner string) (*big.Int, error) {
	pre, err := t.tokenRows("preTokenBalances", mint)
	if err != nil {
		return nil, err
	}
	post, err := t.tokenRows("postTokenBalances", mint)
	if err != nil {
		return nil, err
	}
	delta := new(big.Int)
	indexes := map[uint64]struct{}{}
	for index := range pre {
		indexes[index] = struct{}{}
	}
	for index := range post {
		indexes[index] = struct{}{}
	}
	for index := range indexes {
		preRow, hasPre := pre[index]
		postRow, hasPost := post[index]
		var rowOwner *string
		if hasPost && postRow.owner != nil {
			rowOwner = postRow.owner
		} else if hasPre {
			rowOwner = preRow.owner
		}
		if rowOwner == nil || *rowOwner != owner {
			continue
		}
		delta.Add(delta, new(big.Int).SetUint64(postRow.amount))
		delta.Sub(delta, new(big.Int).SetUint64(preRow.amount))
	}
	return delta, nil
}

func (t jsonTransaction) hasEarnAnchor(vault watch.Vault) bool {
	accounts := t.accountSet()
	if _, ok := accounts[vault.Settings]; ok {
		return true
	}
	if _, ok := accounts[vault.Vault]; ok {
		return true
	}
	for _, binding := range vault.Accounts {
		switch binding.Role {
		case "smart_account", "policy", "vault", "idle_token", "obligation":
			if _, ok := accounts[binding.Pubkey]; ok {
				return true
			}
		}
	}
	return false
}

func (t jsonTransaction) kaminoWithdrawInstructions() []map[string]any {
	var out []map[string]any
	for _, instruction := range t.instructions() {
		if instruction["programId"] != klendProgram.String() {
			continue
		}
		encoded, ok := instruction["data"].(string)
		if !ok {
			continue
		}
		data, err := base58.Decode(encoded)
		if err == nil && bytes.HasPrefix(data, withdrawV2Discriminator) {
			out = append(out, instruction)
		}
	}
	return out
}

type cashFlowKind int

const (
	cashDeposit cashFlowKind = iota
	cashWithdrawal
	cashRefund
)

type cashFlow struct {
	kind       cashFlowKind
	mint       string
	amount     uint64
	refundKind string
}

// classifyCashFlow is classify_transaction_cash_flow.
func classifyCashFlow(t jsonTransaction, update NormalizedUpdate, vault watch.Vault) (*cashFlow, error) {
	if update.EventKind == "account_deleted" && update.AccountPubkey != nil {
		role := ""
		for _, binding := range vault.Accounts {
			if binding.Pubkey == *update.AccountPubkey {
				role = binding.Role
				break
			}
		}
		if role == "policy" || role == "idle_token" || role == "vault" {
			credit, err := t.ownerLamportCredit(vault.Wallet)
			if err != nil {
				return nil, err
			}
			if credit > 0 {
				kind := map[string]string{"policy": "policy", "idle_token": "vault_token_account", "vault": "vault_account"}[role]
				return &cashFlow{kind: cashRefund, amount: credit, refundKind: kind}, nil
			}
		}
	}
	if !t.hasEarnAnchor(vault) {
		return nil, nil
	}
	var result *cashFlow
	for _, stable := range earnStables {
		mint := stable.mint.String()
		delta, err := t.ownerTokenDelta(mint, vault.Wallet)
		if err != nil {
			return nil, err
		}
		if delta.Sign() == 0 {
			continue
		}
		if result != nil {
			return nil, errors.New("one Earn transaction changed more than one supported wallet mint")
		}
		kind := cashWithdrawal
		if delta.Sign() < 0 {
			kind = cashDeposit
			delta.Neg(delta)
		}
		if !delta.IsUint64() {
			return nil, errors.New("wallet token delta overflow")
		}
		result = &cashFlow{kind: kind, mint: mint, amount: delta.Uint64()}
	}
	return result, nil
}

// decodeTokenAccount is decode_token_account for SPL Token and Token-2022.
func decodeTokenAccount(account *solanarpc.Account) (mint, owner solana.PublicKey, amount uint64, err error) {
	data := account.Data
	switch account.Owner {
	case tokenProgram.String():
		if len(data) != 165 {
			return mint, owner, 0, errors.New("invalid SPL token account length")
		}
	case token2022Program.String():
		if len(data) < 165 || len(data) > 165 && (len(data) < 166 || data[165] != 2) {
			return mint, owner, 0, errors.New("invalid Token-2022 account layout")
		}
	default:
		return mint, owner, 0, fmt.Errorf("account has unsupported token program %s", account.Owner)
	}
	if data[108] == 0 || data[108] > 2 {
		return mint, owner, 0, errors.New("token account is not initialized")
	}
	return solana.PublicKeyFromBytes(data[0:32]), solana.PublicKeyFromBytes(data[32:64]), binary.LittleEndian.Uint64(data[64:72]), nil
}

type obligationState struct {
	market, owner solana.PublicKey
	deposits      []struct {
		reserve solana.PublicKey
		amount  uint64
	}
	open bool
}

func decodeObligation(data []byte) (obligationState, error) {
	var out obligationState
	if len(data) < obligationLength || !bytes.Equal(data[:8], obligationDiscriminator) {
		return out, errors.New("decode Kamino obligation")
	}
	out.market, out.owner = solana.PublicKeyFromBytes(data[32:64]), solana.PublicKeyFromBytes(data[64:96])
	for i := 0; i < 8; i++ {
		offset := 96 + i*136
		reserve := solana.PublicKeyFromBytes(data[offset : offset+32])
		amount := binary.LittleEndian.Uint64(data[offset+32 : offset+40])
		out.deposits = append(out.deposits, struct {
			reserve solana.PublicKey
			amount  uint64
		}{reserve, amount})
		out.open = out.open || amount > 0
	}
	for i := 0; i < 5; i++ {
		offset := 1208 + i*200
		out.open = out.open || !solana.PublicKeyFromBytes(data[offset:offset+32]).IsZero()
	}
	return out, nil
}

type reserveState struct {
	market, mint      solana.PublicKey
	collateralSupply  uint64
	totalLiquidityX60 *big.Int
}

func u128At(data []byte, offset int) *big.Int {
	value := new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[offset+8 : offset+16]))
	value.Lsh(value, 64)
	return value.Add(value, new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[offset:offset+8])))
}

// decodeReserve keeps reserve_total_liquidity_scaled's exact 2^60 arithmetic.
func decodeReserve(data []byte) (reserveState, error) {
	var out reserveState
	if len(data) < reserveLength || !bytes.Equal(data[:8], reserveDiscriminator) {
		return out, errors.New("decode Earn reserve")
	}
	out.market, out.mint = solana.PublicKeyFromBytes(data[32:64]), solana.PublicKeyFromBytes(data[128:160])
	out.collateralSupply = binary.LittleEndian.Uint64(data[2592:2600])
	total := new(big.Int).Lsh(new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[224:232])), 60)
	total.Add(total, u128At(data, 232))
	for _, fee := range []struct {
		offset int
		label  string
	}{{344, "accumulated protocol fees"}, {360, "accumulated referrer fees"}, {376, "pending referrer fees"}} {
		amount := u128At(data, fee.offset)
		if total.Cmp(amount) < 0 {
			return out, fmt.Errorf("reserve total liquidity underflow subtracting %s", fee.label)
		}
		total.Sub(total, amount)
	}
	out.totalLiquidityX60 = total
	return out, nil
}

// redeemableLiquidity is collateral_to_redeemable_liquidity.
func redeemableLiquidity(collateralSupply uint64, totalX60 *big.Int, collateral uint64) (uint64, error) {
	if collateral == 0 {
		return 0, nil
	}
	if collateralSupply == 0 || totalX60.Sign() == 0 {
		return collateral, nil
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(collateral), totalX60)
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(collateralSupply), 60)
	value := numerator.Quo(numerator, denominator)
	if !value.IsUint64() {
		return 0, errors.New("redeemable liquidity amount does not fit u64")
	}
	return value.Uint64(), nil
}

func bindingKeys(vault watch.Vault, role string) ([]solana.PublicKey, error) {
	var out []solana.PublicKey
	for _, binding := range vault.Accounts {
		if binding.Role != role {
			continue
		}
		key, err := solana.PublicKeyFromBase58(binding.Pubkey)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, nil
}

func addressStrings(keys []solana.PublicKey) []string { return keyStrings(keys) }

// readAccounts reads confirmed accounts at or after minimum and enforces
// the context slot.
func readAccounts(ctx context.Context, rpc *solanarpc.Client, addresses []string, minimum uint64, what string) (solanarpc.AccountsResponse, error) {
	if len(addresses) == 0 {
		return solanarpc.AccountsResponse{Slot: minimum}, nil
	}
	response, err := rpc.MultipleAccounts(ctx, addresses, confirmedCommitment, &minimum)
	if err != nil {
		return response, err
	}
	if response.Slot < minimum {
		return response, fmt.Errorf("%s context slot %d is below minimum %d", what, response.Slot, minimum)
	}
	return response, nil
}

type cleanupProof struct{ balancesZero, policiesClosed bool }

func knownStable(mint solana.PublicKey) bool {
	for _, stable := range earnStables {
		if stable.mint == mint {
			return true
		}
	}
	return false
}

// readCleanupProof is read_cleanup_proof.
func readCleanupProof(ctx context.Context, rpc *solanarpc.Client, vault watch.Vault, minimum uint64) (cleanupProof, error) {
	addresses := make([]string, 0, len(vault.Accounts))
	for _, binding := range vault.Accounts {
		if _, err := solana.PublicKeyFromBase58(binding.Pubkey); err != nil {
			return cleanupProof{}, err
		}
		addresses = append(addresses, binding.Pubkey)
	}
	response, err := rpc.MultipleAccounts(ctx, addresses, confirmedCommitment, &minimum)
	if err != nil {
		return cleanupProof{}, err
	}
	if response.Slot < minimum {
		return cleanupProof{}, fmt.Errorf("cleanup proof context slot %d is below minimum %d", response.Slot, minimum)
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return cleanupProof{}, err
	}
	proof := cleanupProof{balancesZero: true}
	sawPolicy, policies := false, 0
	for index, binding := range vault.Accounts {
		account := response.Accounts[index]
		switch binding.Role {
		case "policy":
			policies++
			if account != nil {
				if account.Owner != sp.Program.String() {
					return proof, fmt.Errorf("policy account %s has unexpected owner %s", binding.Pubkey, account.Owner)
				}
				sawPolicy = true
			}
		case "idle_token":
			if account == nil {
				continue
			}
			mint, owner, amount, err := decodeTokenAccount(account)
			if err != nil {
				return proof, err
			}
			if owner != vaultKey {
				return proof, fmt.Errorf("idle account %s belongs to %s, expected %s", binding.Pubkey, owner, vaultKey)
			}
			if amount > 0 && (!knownStable(mint) || amount >= dustThreshold) {
				proof.balancesZero = false
			}
		case "obligation":
			if account == nil {
				continue
			}
			if account.Owner != klendProgram.String() {
				return proof, fmt.Errorf("obligation %s has unexpected owner %s", binding.Pubkey, account.Owner)
			}
			obligation, err := decodeObligation(account.Data)
			if err != nil {
				return proof, err
			}
			if obligation.owner != vaultKey {
				return proof, fmt.Errorf("obligation %s belongs to %s, expected %s", binding.Pubkey, obligation.owner, vaultKey)
			}
			if obligation.open {
				proof.balancesZero = false
			}
		}
	}
	if policies == 0 {
		return proof, errors.New("cleanup proof has no policy bindings")
	}
	blocking, err := blockingTokenInventory(ctx, rpc, vaultKey, minimum)
	if err != nil {
		return proof, err
	}
	if blocking {
		proof.balancesZero = false
	}
	proof.policiesClosed = !sawPolicy
	return proof, nil
}

// blockingTokenInventory is vault_has_blocking_token_inventory: any non-dust
// or non-product token balance blocks cleanup.
func blockingTokenInventory(ctx context.Context, rpc *solanarpc.Client, vault solana.PublicKey, minimum uint64) (bool, error) {
	for _, program := range []solana.PublicKey{tokenProgram, token2022Program} {
		slot, accounts, err := rpc.TokenAccountsByOwner(ctx, vault.String(), program.String(), confirmedCommitment, minimum)
		if err != nil {
			return false, fmt.Errorf("get token accounts by owner: %w", err)
		}
		if slot < minimum {
			return false, fmt.Errorf("token inventory context slot %d is below minimum %d", slot, minimum)
		}
		for _, keyed := range accounts {
			address, err := solana.PublicKeyFromBase58(keyed.Pubkey)
			if err != nil {
				return false, err
			}
			mint, owner, amount, err := decodeTokenAccount(&keyed.Account)
			if err != nil {
				return false, fmt.Errorf("decode owner token account %s: %w", address, err)
			}
			if owner != vault {
				return false, fmt.Errorf("token inventory query returned account %s for owner %s", address, owner)
			}
			if amount == 0 {
				continue
			}
			productIdle := false
			for _, stable := range earnStables {
				if stable.program == program && stable.mint == mint && associatedToken(vault, stable.mint, stable.program) == address {
					productIdle = true
				}
			}
			if !productIdle || amount >= dustThreshold {
				return true, nil
			}
		}
	}
	return false, nil
}

func isPolicyDeletion(update NormalizedUpdate, vault watch.Vault) bool {
	if update.EventKind != "account_deleted" || update.AccountPubkey == nil {
		return false
	}
	for _, binding := range vault.Accounts {
		if binding.Role == "policy" && binding.Pubkey == *update.AccountPubkey {
			return true
		}
	}
	return false
}

// resolveMutation is resolve_rpc_mutation.
func resolveMutation(ctx context.Context, rpc *solanarpc.Client, update NormalizedUpdate, vault watch.Vault, context ReconciliationContext) (EarnMutation, error) {
	if isPolicyDeletion(update, vault) {
		if update.Signature == nil {
			return EarnMutation{}, errors.New("closed policy update has no transaction signature")
		}
		signature := *update.Signature
		transaction, err := readEarnTransaction(ctx, rpc, signature)
		if err != nil {
			return EarnMutation{}, err
		}
		slot, err := transaction.slotAt(signature, update.Slot, "policy-close ")
		if err != nil {
			return EarnMutation{}, err
		}
		proof, err := readCleanupProof(ctx, rpc, vault, update.Slot)
		if err != nil {
			return EarnMutation{}, err
		}
		credit, err := transaction.ownerLamportCredit(vault.Wallet)
		if err != nil {
			return EarnMutation{}, err
		}
		if credit > 0 {
			return EarnMutation{Refund: &EarnRefundMutation{Cluster: vault.Environment, FullCleanup: proof.balancesZero && proof.policiesClosed,
				Settings: vault.Settings, VaultIndex: vault.VaultIndex, VaultPubkey: vault.Vault, Wallet: vault.Wallet,
				RefundSignature: signature, ConfirmedSlot: slot, RefundKind: "policy"}}, nil
		}
		if !proof.balancesZero || !proof.policiesClosed {
			return EarnMutation{}, nil
		}
		return EarnMutation{Cleanup: &EarnCleanupMutation{Settings: vault.Settings, VaultIndex: vault.VaultIndex, VaultPubkey: vault.Vault,
			CleanupSignature: signature, ConfirmedSlot: update.Slot}}, nil
	}
	if update.Signature == nil {
		return EarnMutation{}, nil
	}
	signature := *update.Signature
	transaction, err := readEarnTransaction(ctx, rpc, signature)
	if err != nil {
		return EarnMutation{}, err
	}
	slot, err := transaction.slotAt(signature, update.Slot, "")
	if err != nil {
		return EarnMutation{}, err
	}
	var flow *cashFlow
	if update.EventKind == "refund_cleanup_repair" {
		credit, err := transaction.ownerLamportCredit(vault.Wallet)
		if err != nil {
			return EarnMutation{}, err
		}
		if credit == 0 || !transaction.hasEarnAnchor(vault) {
			return EarnMutation{}, errors.New("refund cleanup repair has no anchored wallet refund proof")
		}
		flow = &cashFlow{kind: cashRefund, amount: credit, refundKind: "account"}
	} else if flow, err = classifyCashFlow(transaction, update, vault); err != nil {
		return EarnMutation{}, err
	}
	if flow == nil {
		return EarnMutation{}, nil
	}
	if flow.kind == cashRefund {
		proof, err := readCleanupProof(ctx, rpc, vault, update.Slot)
		if err != nil {
			return EarnMutation{}, err
		}
		if update.EventKind == "refund_cleanup_repair" && !(proof.balancesZero && proof.policiesClosed) {
			return EarnMutation{}, errors.New("refund cleanup repair cannot prove zero balances and closed policies")
		}
		kind := flow.refundKind
		if kind == "" {
			kind = "account"
		}
		return EarnMutation{Refund: &EarnRefundMutation{Cluster: vault.Environment, FullCleanup: proof.balancesZero && proof.policiesClosed,
			Settings: vault.Settings, VaultIndex: vault.VaultIndex, VaultPubkey: vault.Vault, Wallet: vault.Wallet,
			RefundSignature: signature, ConfirmedSlot: slot, RefundKind: kind}}, nil
	}
	if context.RoutePolicy == nil {
		return EarnMutation{}, fmt.Errorf("cash flow %s arrived before its route policy projection", signature)
	}
	return readCashFlowProof(ctx, rpc, update, vault, *context.RoutePolicy, context.SetupPolicy, transaction, *flow)
}

type vaultSnapshot struct {
	observedSlot uint64
	reserves     []EarnReserveMutation
	idles        []EarnIdleTokenMutation
}

func keySetOf(values []string) (map[solana.PublicKey]struct{}, error) {
	out := map[solana.PublicKey]struct{}{}
	for _, value := range values {
		key, err := solana.PublicKeyFromBase58(value)
		if err != nil {
			return nil, err
		}
		out[key] = struct{}{}
	}
	return out, nil
}

func metadata(kind string, slot uint64) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"kind": kind, "slot": slot})
	return encoded
}

// readVaultSnapshot is read_complete_vault_snapshot.
func readVaultSnapshot(ctx context.Context, rpc *solanarpc.Client, vault watch.Vault, policy PolicyMatchInput, minimum uint64) (vaultSnapshot, error) {
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return vaultSnapshot{}, err
	}
	obligations, err := bindingKeys(vault, "obligation")
	if err != nil {
		return vaultSnapshot{}, err
	}
	discovery, err := readAccounts(ctx, rpc, addressStrings(obligations), minimum, "Earn discovery")
	if err != nil && len(obligations) > 0 {
		return vaultSnapshot{}, err
	}
	reserveSet := map[solana.PublicKey]struct{}{}
	for _, account := range discovery.Accounts {
		if account == nil || account.Owner != klendProgram.String() {
			continue
		}
		obligation, err := decodeObligation(account.Data)
		if err != nil {
			return vaultSnapshot{}, fmt.Errorf("decode discovered Earn obligation: %w", err)
		}
		if obligation.owner != vaultKey {
			continue
		}
		for _, deposit := range obligation.deposits {
			if !deposit.reserve.IsZero() {
				reserveSet[deposit.reserve] = struct{}{}
			}
		}
	}
	reserves := make([]solana.PublicKey, 0, len(reserveSet))
	for key := range reserveSet {
		reserves = append(reserves, key)
	}
	sort.Slice(reserves, func(i, j int) bool { return bytes.Compare(reserves[i][:], reserves[j][:]) < 0 })
	idle, err := bindingKeys(vault, "idle_token")
	if err != nil {
		return vaultSnapshot{}, err
	}
	addresses := append(append(append([]solana.PublicKey(nil), obligations...), reserves...), idle...)
	response, err := rpc.MultipleAccounts(ctx, addressStrings(addresses), confirmedCommitment, ptr(max(discovery.Slot, minimum)))
	if err != nil {
		return vaultSnapshot{}, err
	}
	if response.Slot < minimum {
		return vaultSnapshot{}, fmt.Errorf("Earn snapshot context slot %d is below minimum %d", response.Slot, minimum)
	}
	reserveAccounts := map[solana.PublicKey]reserveState{}
	for index, address := range reserves {
		account := response.Accounts[len(obligations)+index]
		if account == nil {
			continue
		}
		if account.Owner != klendProgram.String() {
			return vaultSnapshot{}, fmt.Errorf("Earn reserve %s has unexpected owner %s", address, account.Owner)
		}
		state, err := decodeReserve(account.Data)
		if err != nil {
			return vaultSnapshot{}, err
		}
		reserveAccounts[address] = state
	}
	markets, err := keySetOf(policy.KaminoMarkets)
	if err != nil {
		return vaultSnapshot{}, err
	}
	mints, err := keySetOf(policy.KaminoLiquidityMints)
	if err != nil {
		return vaultSnapshot{}, err
	}
	snapshot := vaultSnapshot{observedSlot: response.Slot}
	for _, account := range response.Accounts[:len(obligations)] {
		if account == nil || account.Owner != klendProgram.String() {
			continue
		}
		obligation, err := decodeObligation(account.Data)
		if err != nil {
			return vaultSnapshot{}, fmt.Errorf("decode canonical Earn obligation: %w", err)
		}
		if _, allowed := markets[obligation.market]; obligation.owner != vaultKey || !allowed {
			continue
		}
		for _, deposit := range obligation.deposits {
			if deposit.reserve.IsZero() {
				continue
			}
			reserve, ok := reserveAccounts[deposit.reserve]
			if _, allowedMint := mints[reserve.mint]; !ok || reserve.market != obligation.market || !allowedMint {
				continue
			}
			amount, err := redeemableLiquidity(reserve.collateralSupply, reserve.totalLiquidityX60, deposit.amount)
			if err != nil {
				return vaultSnapshot{}, err
			}
			market := obligation.market.String()
			snapshot.reserves = append(snapshot.reserves, EarnReserveMutation{Reserve: deposit.reserve.String(), Market: &market,
				LiquidityMint: reserve.mint.String(), AmountRaw: amount, HasValue: amount > 0,
				PlanningMetadata: metadata("earn_laserstream_complete_snapshot", response.Slot)})
		}
	}
	sort.SliceStable(snapshot.reserves, func(i, j int) bool { return snapshot.reserves[i].Reserve < snapshot.reserves[j].Reserve })
	deduped := snapshot.reserves[:0]
	for _, reserve := range snapshot.reserves {
		if len(deduped) > 0 && deduped[len(deduped)-1].Reserve == reserve.Reserve {
			continue
		}
		deduped = append(deduped, reserve)
	}
	snapshot.reserves = deduped
	for index, address := range idle {
		account := response.Accounts[len(obligations)+len(reserves)+index]
		if account == nil {
			continue
		}
		mint, owner, amount, err := decodeTokenAccount(account)
		if err != nil {
			return vaultSnapshot{}, err
		}
		if owner != vaultKey {
			return vaultSnapshot{}, fmt.Errorf("Earn idle account %s belongs to %s, expected %s", address, owner, vaultKey)
		}
		snapshot.idles = append(snapshot.idles, EarnIdleTokenMutation{Mint: mint.String(), AmountRaw: amount, Owner: owner.String(),
			TokenAccount: address.String(), ObservedSlot: response.Slot, SourceCommitment: confirmedCommitment})
	}
	return snapshot, nil
}

func ptr[T any](value T) *T { return &value }

// drainedObligationTarget is read_drained_obligation_withdraw_target: a
// full withdrawal that closed or emptied the obligation still proves its
// reserve from the transaction's own Kamino withdraw accounts.
func drainedObligationTarget(ctx context.Context, rpc *solanarpc.Client, update NormalizedUpdate, vault watch.Vault, policy PolicyMatchInput, transaction jsonTransaction, mint string) (*EarnReserveMutation, error) {
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return nil, err
	}
	watched, err := bindingKeys(vault, "obligation")
	if err != nil {
		return nil, err
	}
	if len(watched) == 0 {
		return nil, nil
	}
	type candidate struct{ obligation, reserve, market, mint solana.PublicKey }
	candidates := map[candidate]struct{}{}
	for _, instruction := range transaction.kaminoWithdrawInstructions() {
		accounts, ok := instruction["accounts"].([]any)
		if !ok {
			return nil, errors.New("Kamino withdrawal instruction has no accounts")
		}
		if len(accounts) < 6 {
			return nil, errors.New("Kamino withdrawal instruction has too few accounts")
		}
		account := func(index int, label string) (solana.PublicKey, error) {
			value, ok := accounts[index].(string)
			if !ok {
				return solana.PublicKey{}, fmt.Errorf("Kamino withdrawal %s is not a public key", label)
			}
			key, err := solana.PublicKeyFromBase58(value)
			if err != nil {
				return solana.PublicKey{}, fmt.Errorf("invalid Kamino withdrawal %s", label)
			}
			return key, nil
		}
		obligation, err := account(1, "obligation")
		if err != nil {
			return nil, err
		}
		instructionMint, err := account(5, "liquidity mint")
		if err != nil {
			return nil, err
		}
		owner, err := account(0, "owner")
		if err != nil {
			return nil, err
		}
		if owner != vaultKey || !containsKey(watched, obligation) || instructionMint.String() != mint {
			continue
		}
		reserve, err := account(4, "reserve")
		if err != nil {
			return nil, err
		}
		market, err := account(2, "market")
		if err != nil {
			return nil, err
		}
		candidates[candidate{obligation, reserve, market, instructionMint}] = struct{}{}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if len(candidates) > 1 {
		return nil, errors.New("closed Earn obligation transaction has more than one withdrawal target")
	}
	var target candidate
	for value := range candidates {
		target = value
	}
	marketAllowed, mintAllowed := false, false
	for _, value := range policy.KaminoMarkets {
		marketAllowed = marketAllowed || value == target.market.String()
	}
	for _, value := range policy.KaminoLiquidityMints {
		mintAllowed = mintAllowed || value == mint
	}
	if !marketAllowed || !mintAllowed {
		return nil, errors.New("closed Earn obligation withdrawal target is outside the route policy")
	}
	response, err := rpc.MultipleAccounts(ctx, []string{target.obligation.String(), target.reserve.String()}, confirmedCommitment, &update.Slot)
	if err != nil {
		return nil, err
	}
	if response.Slot < update.Slot {
		return nil, fmt.Errorf("full-withdraw reserve context slot %d is below minimum %d", response.Slot, update.Slot)
	}
	// The PDA may have been funded again since; a transaction that closed it
	// already proves the drain.
	if account := response.Accounts[0]; account != nil && !transaction.closedAccount(target.obligation.String()) {
		if account.Owner != klendProgram.String() {
			return nil, fmt.Errorf("full-withdraw obligation %s has unexpected owner %s", target.obligation, account.Owner)
		}
		obligation, err := decodeObligation(account.Data)
		if err != nil {
			return nil, fmt.Errorf("decode full-withdraw Kamino obligation: %w", err)
		}
		if obligation.owner != vaultKey || obligation.open {
			return nil, fmt.Errorf("full-withdraw obligation %s still has an open position", target.obligation)
		}
	}
	account := response.Accounts[1]
	if account == nil {
		return nil, errors.New("full-withdraw reserve account is unavailable")
	}
	if account.Owner != klendProgram.String() {
		return nil, fmt.Errorf("full-withdraw reserve %s has unexpected owner %s", target.reserve, account.Owner)
	}
	reserve, err := decodeReserve(account.Data)
	if err != nil {
		return nil, fmt.Errorf("decode full-withdraw Kamino reserve: %w", err)
	}
	if reserve.market != target.market || reserve.mint != target.mint {
		return nil, errors.New("full-withdraw reserve state does not match its transaction accounts")
	}
	market := target.market.String()
	return &EarnReserveMutation{Reserve: target.reserve.String(), Market: &market, LiquidityMint: target.mint.String(),
		PlanningMetadata: metadata("earn_laserstream_drained_obligation_withdrawal", response.Slot)}, nil
}

// readCashFlowProof is read_cash_flow_proof.
func readCashFlowProof(ctx context.Context, rpc *solanarpc.Client, update NormalizedUpdate, vault watch.Vault, route PolicyMatchInput, setup *PolicyMatchInput, transaction jsonTransaction, flow cashFlow) (EarnMutation, error) {
	signature := *update.Signature
	snapshot, err := readVaultSnapshot(ctx, rpc, vault, route, update.Slot)
	if err != nil {
		return EarnMutation{}, err
	}
	accounts := transaction.accountSet()
	var target *EarnReserveMutation
	for _, reserve := range snapshot.reserves {
		if _, touched := accounts[reserve.Reserve]; reserve.LiquidityMint == flow.mint && touched {
			value := reserve
			target = &value
			break
		}
	}
	if target == nil && flow.kind == cashWithdrawal {
		if target, err = drainedObligationTarget(ctx, rpc, update, vault, route, transaction, flow.mint); err != nil {
			return EarnMutation{}, err
		}
	}
	if target == nil {
		if flow.kind == cashWithdrawal && len(transaction.kaminoWithdrawInstructions()) > 0 {
			return EarnMutation{}, fmt.Errorf("withdrawal cash flow %s has no provable Kamino target", signature)
		}
		// A wallet credit without a Kamino withdraw is an idle-vault sweep
		// (full-exit dust return), not a reserve withdrawal: retrying can never
		// prove a target and would block the vault's later jobs.
		return EarnMutation{}, nil
	}
	var remaining uint64
	for _, reserve := range snapshot.reserves {
		if reserve.LiquidityMint == flow.mint {
			if remaining+reserve.AmountRaw < remaining {
				return EarnMutation{}, errors.New("Earn reserve balance overflow")
			}
			remaining += reserve.AmountRaw
		}
	}
	for _, idle := range snapshot.idles {
		if idle.Mint == flow.mint {
			if remaining+idle.AmountRaw < remaining {
				return EarnMutation{}, errors.New("Earn remaining balance overflow")
			}
			remaining += idle.AmountRaw
		}
	}
	if flow.kind == cashDeposit {
		return EarnMutation{Deposit: &EarnDepositMutation{RoutePolicy: route, SetupPolicy: setup, DepositSignature: signature,
			DepositSlot: update.Slot, ObservedSlot: snapshot.observedSlot, DepositMint: flow.mint, PrincipalAmountRaw: flow.amount,
			TargetReserve: target.Reserve, Market: target.Market, LiquidityMint: flow.mint, TargetSupplyAPYBPS: target.SupplyAPYBPS,
			Wallet: vault.Wallet, SmartAccountAddress: vault.Vault, ReserveState: snapshot.reserves, IdleState: snapshot.idles}}, nil
	}
	return EarnMutation{Withdrawal: &EarnWithdrawalMutation{RoutePolicy: route, WithdrawalSignature: signature, ConfirmedSlot: update.Slot,
		ObservedSlot: snapshot.observedSlot, Wallet: vault.Wallet, VaultPubkey: vault.Vault, TargetReserve: target.Reserve, Market: target.Market,
		LiquidityMint: flow.mint, WithdrawnAmountRaw: flow.amount, RemainingAmountRaw: remaining, ReserveState: snapshot.reserves,
		IdleState: snapshot.idles}}, nil
}

func sha256Sum(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
