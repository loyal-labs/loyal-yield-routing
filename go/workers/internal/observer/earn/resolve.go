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

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
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

// earnTransaction is a confirmed transaction that succeeded.
type earnTransaction struct{ chain.Execution }

// readExecution reads a confirmed transaction. A node that cannot serve it
// yet leaves the proof pending; that is not proof of absence.
func readExecution(ctx context.Context, rpc *chain.Client, signature string) (chain.Execution, error) {
	key, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return chain.Execution{}, fmt.Errorf("invalid transaction signature: %w", err)
	}
	read, err := rpc.Execution(ctx, key, confirmedCommitment)
	if errors.Is(err, chain.ErrNotFound) {
		return chain.Execution{}, errProofPending
	}
	return read, err
}

// readEarnTransaction is read_transaction_json: a confirmed transaction that
// must have succeeded.
func readEarnTransaction(ctx context.Context, rpc *chain.Client, signature string) (earnTransaction, error) {
	read, err := readExecution(ctx, rpc, signature)
	if err != nil {
		return earnTransaction{}, err
	}
	if read.Err != nil {
		return earnTransaction{}, fmt.Errorf("transaction %s failed on chain", signature)
	}
	return earnTransaction{read}, nil
}

func (t earnTransaction) accountSet() map[string]struct{} {
	set := map[string]struct{}{}
	for _, key := range t.Keys {
		set[key.String()] = struct{}{}
	}
	return set
}

func (t earnTransaction) keyIndex(address string) int {
	for index, key := range t.Keys {
		if key.String() == address {
			return index
		}
	}
	return -1
}

func lamportsAt(balances []uint64, index int) (uint64, bool) {
	if index < 0 || index >= len(balances) {
		return 0, false
	}
	return balances[index], true
}

func (t earnTransaction) slotAt(signature string, expected uint64) (uint64, error) {
	if t.Slot != expected {
		return 0, fmt.Errorf("transaction %s landed at slot %d, expected account-update slot %d", signature, t.Slot, expected)
	}
	return t.Slot, nil
}

// ownerLamportCredit is transaction_owner_lamport_credit (fee added back for
// the fee payer).
func (t earnTransaction) ownerLamportCredit(owner string) uint64 {
	index := t.keyIndex(owner)
	if index < 0 {
		return 0
	}
	pre, _ := lamportsAt(t.PreLamports, index)
	post, _ := lamportsAt(t.PostLamports, index)
	var fee uint64
	if index == 0 {
		fee = t.Fee
	}
	credit := post + fee
	if credit < post {
		credit = ^uint64(0)
	}
	if credit < pre {
		return 0
	}
	return credit - pre
}

func (t earnTransaction) closedAccount(address string) bool {
	index := t.keyIndex(address)
	pre, okPre := lamportsAt(t.PreLamports, index)
	post, okPost := lamportsAt(t.PostLamports, index)
	return okPre && pre > 0 && okPost && post == 0
}

// ownerTokenDelta is transaction_owner_token_delta: an account counts for
// owner by its post-balance owner, or its pre-balance owner once closed.
func (t earnTransaction) ownerTokenDelta(mint solana.PublicKey, owner string) *big.Int {
	delta := new(big.Int)
	for _, post := range t.Post {
		if post.Mint == mint && post.Owner.String() == owner {
			delta.Add(delta, new(big.Int).SetUint64(post.Amount))
		}
	}
	for account, pre := range t.Pre {
		if pre.Mint != mint {
			continue
		}
		rowOwner := pre.Owner
		if post, ok := t.Post[account]; ok && post.Mint == mint {
			rowOwner = post.Owner
		}
		if rowOwner.String() == owner {
			delta.Sub(delta, new(big.Int).SetUint64(pre.Amount))
		}
	}
	return delta
}

func (t earnTransaction) hasEarnAnchor(vault watch.Vault) bool {
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

// kaminoWithdrawAccounts lists the accounts of every Kamino withdraw v2 the
// transaction ran, outer or inner.
func (t earnTransaction) kaminoWithdrawAccounts() [][]solana.PublicKey {
	var out [][]solana.PublicKey
	add := func(program uint16, indexes []uint16, data []byte) {
		if int(program) >= len(t.Keys) || t.Keys[program] != klendProgram || !bytes.HasPrefix(data, withdrawV2Discriminator) {
			return
		}
		accounts := make([]solana.PublicKey, len(indexes))
		for i, index := range indexes {
			if int(index) < len(t.Keys) {
				accounts[i] = t.Keys[index]
			}
		}
		out = append(out, accounts)
	}
	for _, compiled := range t.Transaction.Message.Instructions {
		add(compiled.ProgramIDIndex, compiled.Accounts, compiled.Data)
	}
	for _, group := range t.Inner {
		for _, compiled := range group.Instructions {
			add(compiled.ProgramIDIndex, compiled.Accounts, compiled.Data)
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
func classifyCashFlow(t earnTransaction, update NormalizedUpdate, vault watch.Vault) (*cashFlow, error) {
	if update.EventKind == "account_deleted" && update.AccountPubkey != nil {
		role := ""
		for _, binding := range vault.Accounts {
			if binding.Pubkey == *update.AccountPubkey {
				role = binding.Role
				break
			}
		}
		if role == "policy" || role == "idle_token" || role == "vault" {
			if credit := t.ownerLamportCredit(vault.Wallet); credit > 0 {
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
		delta := t.ownerTokenDelta(stable.mint, vault.Wallet)
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
		result = &cashFlow{kind: kind, mint: stable.mint.String(), amount: delta.Uint64()}
	}
	return result, nil
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
func readCleanupProof(ctx context.Context, rpc *chain.Client, vault watch.Vault, minimum uint64) (cleanupProof, error) {
	addresses := make([]solana.PublicKey, 0, len(vault.Accounts))
	for _, binding := range vault.Accounts {
		key, err := solana.PublicKeyFromBase58(binding.Pubkey)
		if err != nil {
			return cleanupProof{}, err
		}
		addresses = append(addresses, key)
	}
	_, accounts, err := rpc.Accounts(ctx, addresses, confirmedCommitment, minimum)
	if err != nil {
		return cleanupProof{}, err
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return cleanupProof{}, err
	}
	proof := cleanupProof{balancesZero: true}
	sawPolicy, policies := false, 0
	for index, binding := range vault.Accounts {
		account := accounts[index]
		switch binding.Role {
		case "policy":
			policies++
			if account != nil {
				if account.Owner != squads.ProgramID {
					return proof, fmt.Errorf("policy account %s has unexpected owner %s", binding.Pubkey, account.Owner)
				}
				sawPolicy = true
			}
		case "idle_token":
			if account == nil {
				continue
			}
			held, err := spl.DecodeTokenAccount(account)
			if err != nil {
				return proof, err
			}
			if held.Owner != vaultKey {
				return proof, fmt.Errorf("idle account %s belongs to %s, expected %s", binding.Pubkey, held.Owner, vaultKey)
			}
			if held.Amount > 0 && (!knownStable(held.Mint) || held.Amount >= dustThreshold) {
				proof.balancesZero = false
			}
		case "obligation":
			if account == nil {
				continue
			}
			if account.Owner != klendProgram {
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
func blockingTokenInventory(ctx context.Context, rpc *chain.Client, vault solana.PublicKey, minimum uint64) (bool, error) {
	for _, program := range []solana.PublicKey{tokenProgram, token2022Program} {
		_, accounts, err := rpc.TokenAccounts(ctx, vault, program, confirmedCommitment, minimum)
		if err != nil {
			return false, fmt.Errorf("get token accounts by owner: %w", err)
		}
		for _, account := range accounts {
			address := account.Key
			held, err := spl.DecodeTokenAccount(&account)
			if err != nil {
				return false, fmt.Errorf("decode owner token account %s: %w", address, err)
			}
			if held.Owner != vault {
				return false, fmt.Errorf("token inventory query returned account %s for owner %s", address, held.Owner)
			}
			if held.Amount == 0 {
				continue
			}
			productIdle := false
			for _, stable := range earnStables {
				if stable.program == program && stable.mint == held.Mint && associatedToken(vault, stable.mint, stable.program) == address {
					productIdle = true
				}
			}
			if !productIdle || held.Amount >= dustThreshold {
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
func resolveMutation(ctx context.Context, rpc *chain.Client, update NormalizedUpdate, vault watch.Vault, context ReconciliationContext) (EarnMutation, error) {
	if isPolicyDeletion(update, vault) {
		if update.Signature == nil {
			return EarnMutation{}, errors.New("closed policy update has no transaction signature")
		}
		signature := *update.Signature
		transaction, err := readEarnTransaction(ctx, rpc, signature)
		if err != nil {
			return EarnMutation{}, err
		}
		slot, err := transaction.slotAt(signature, update.Slot)
		if err != nil {
			return EarnMutation{}, err
		}
		proof, err := readCleanupProof(ctx, rpc, vault, update.Slot)
		if err != nil {
			return EarnMutation{}, err
		}
		if transaction.ownerLamportCredit(vault.Wallet) > 0 {
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
	slot, err := transaction.slotAt(signature, update.Slot)
	if err != nil {
		return EarnMutation{}, err
	}
	var flow *cashFlow
	if update.EventKind == "refund_cleanup_repair" {
		credit := transaction.ownerLamportCredit(vault.Wallet)
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
func readVaultSnapshot(ctx context.Context, rpc *chain.Client, vault watch.Vault, policy PolicyMatchInput, minimum uint64) (vaultSnapshot, error) {
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return vaultSnapshot{}, err
	}
	obligations, err := bindingKeys(vault, "obligation")
	if err != nil {
		return vaultSnapshot{}, err
	}
	discoverySlot, discovered := minimum, []*chain.Account(nil)
	if len(obligations) > 0 {
		if discoverySlot, discovered, err = rpc.Accounts(ctx, obligations, confirmedCommitment, minimum); err != nil {
			return vaultSnapshot{}, err
		}
	}
	reserveSet := map[solana.PublicKey]struct{}{}
	for _, account := range discovered {
		if account == nil || account.Owner != klendProgram {
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
	if len(addresses) == 0 {
		// Nothing bound: no reserve or idle balance to snapshot.
		return vaultSnapshot{observedSlot: minimum}, nil
	}
	slot, accounts, err := rpc.Accounts(ctx, addresses, confirmedCommitment, max(discoverySlot, minimum))
	if err != nil {
		return vaultSnapshot{}, err
	}
	reserveAccounts := map[solana.PublicKey]reserveState{}
	for index, address := range reserves {
		account := accounts[len(obligations)+index]
		if account == nil {
			continue
		}
		if account.Owner != klendProgram {
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
	snapshot := vaultSnapshot{observedSlot: slot}
	for _, account := range accounts[:len(obligations)] {
		if account == nil || account.Owner != klendProgram {
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
				PlanningMetadata: metadata("earn_laserstream_complete_snapshot", slot)})
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
		account := accounts[len(obligations)+len(reserves)+index]
		if account == nil {
			continue
		}
		held, err := spl.DecodeTokenAccount(account)
		if err != nil {
			return vaultSnapshot{}, err
		}
		if held.Owner != vaultKey {
			return vaultSnapshot{}, fmt.Errorf("Earn idle account %s belongs to %s, expected %s", address, held.Owner, vaultKey)
		}
		snapshot.idles = append(snapshot.idles, EarnIdleTokenMutation{Mint: held.Mint.String(), AmountRaw: held.Amount, Owner: held.Owner.String(),
			TokenAccount: address.String(), ObservedSlot: slot, SourceCommitment: confirmedCommitment})
	}
	return snapshot, nil
}

// drainedObligationTarget is read_drained_obligation_withdraw_target: a
// full withdrawal that closed or emptied the obligation still proves its
// reserve from the transaction's own Kamino withdraw accounts.
func drainedObligationTarget(ctx context.Context, rpc *chain.Client, update NormalizedUpdate, vault watch.Vault, policy PolicyMatchInput, transaction earnTransaction, mint string) (*EarnReserveMutation, error) {
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
	for _, accounts := range transaction.kaminoWithdrawAccounts() {
		if len(accounts) < 6 {
			return nil, errors.New("Kamino withdrawal instruction has too few accounts")
		}
		owner, obligation, market, reserve, instructionMint := accounts[0], accounts[1], accounts[2], accounts[4], accounts[5]
		if owner != vaultKey || !containsKey(watched, obligation) || instructionMint.String() != mint {
			continue
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
	slot, accounts, err := rpc.Accounts(ctx, []solana.PublicKey{target.obligation, target.reserve}, confirmedCommitment, update.Slot)
	if err != nil {
		return nil, err
	}
	// The PDA may have been funded again since; a transaction that closed it
	// already proves the drain.
	if account := accounts[0]; account != nil && !transaction.closedAccount(target.obligation.String()) {
		if account.Owner != klendProgram {
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
	account := accounts[1]
	if account == nil {
		return nil, errors.New("full-withdraw reserve account is unavailable")
	}
	if account.Owner != klendProgram {
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
		PlanningMetadata: metadata("earn_laserstream_drained_obligation_withdrawal", slot)}, nil
}

// readCashFlowProof is read_cash_flow_proof.
func readCashFlowProof(ctx context.Context, rpc *chain.Client, update NormalizedUpdate, vault watch.Vault, route PolicyMatchInput, setup *PolicyMatchInput, transaction earnTransaction, flow cashFlow) (EarnMutation, error) {
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
		if flow.kind == cashWithdrawal && len(transaction.kaminoWithdrawAccounts()) > 0 {
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
