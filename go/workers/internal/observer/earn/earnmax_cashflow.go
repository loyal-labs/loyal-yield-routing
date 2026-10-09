package earn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/solana-foundation/solana-go/v2"
)

// Earn MAX claim-custody cash flow, ported from earn_reconciliation.rs
// project_earn_max_account_update, read_confirmed_earn_max_account_transfer
// and project_earn_max_confirmed_cash_flow.

type custodyTransfer struct {
	wire                            []byte
	lookupTables                    map[solana.PublicKey]solana.PublicKeySlice
	transaction                     *solana.Transaction
	signature                       string
	slot                            uint64
	source, destination             solana.PublicKey
	sourcePre, sourcePost           uint64
	destinationPre, destinationPost uint64
}

// tokenAmount is token_amount_optional: nil when the account has no USDC row.
func tokenAmount(balances map[solana.PublicKey]chain.TokenBalance, account solana.PublicKey) *uint64 {
	row, ok := balances[account]
	if !ok || row.Mint != usdcMint {
		return nil
	}
	return &row.Amount
}

func orZero(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

// readCustodyTransfer is read_confirmed_earn_max_account_transfer.
func readCustodyTransfer(ctx context.Context, rpc *chain.Client, signature string, expectedSlot uint64, custody solana.PublicKey) (*custodyTransfer, error) {
	read, err := readExecution(ctx, rpc, signature)
	if err != nil {
		return nil, err
	}
	if read.Slot != expectedSlot {
		return nil, errors.New("Earn MAX account cash-flow RPC slot drifted from LaserStream")
	}
	if read.Err != nil {
		return nil, errors.New("Earn MAX account cash-flow transaction failed")
	}
	transaction, keys := read.Transaction, read.Keys
	loaded := keys[len(transaction.Message.AccountKeys):]
	tables := map[solana.PublicKey]solana.PublicKeySlice{}
	// Rebuild each table's addressed slots from the ordered loaded keys:
	// every table's writable indexes, then every table's readonly indexes.
	next := 0
	for _, readonly := range []bool{false, true} {
		for _, lookup := range transaction.Message.AddressTableLookups {
			indexes := lookup.WritableIndexes
			if readonly {
				indexes = lookup.ReadonlyIndexes
			}
			for _, index := range indexes {
				if next >= len(loaded) {
					return nil, errors.New("Earn MAX cash-flow loaded addresses do not cover the lookups")
				}
				slice := tables[lookup.AccountKey]
				for len(slice) <= int(index) {
					slice = append(slice, solana.PublicKey{})
				}
				slice[index] = loaded[next]
				tables[lookup.AccountKey] = slice
				next++
			}
		}
	}
	if next != len(loaded) {
		return nil, errors.New("Earn MAX cash-flow loaded addresses do not match the lookups")
	}
	claimIndex := -1
	for index, key := range keys {
		if key == custody {
			claimIndex = index
			break
		}
	}
	if claimIndex < 0 {
		return nil, errors.New("Earn MAX cash-flow token account is absent")
	}
	if claimIndex > math.MaxUint8 {
		return nil, errors.New("Earn MAX cash-flow account index exceeds u8")
	}
	claimPre, claimPost := orZero(tokenAmount(read.Pre, custody)), orZero(tokenAmount(read.Post, custody))
	if claimPre == claimPost {
		return nil, errors.New("Earn MAX claim-custody account update has no token delta")
	}
	increased := claimPost > claimPre
	amount := claimPost - claimPre
	if !increased {
		amount = claimPre - claimPost
	}
	type peer struct {
		key       solana.PublicKey
		pre, post uint64
	}
	var peers []peer
	for index, key := range keys {
		if index > math.MaxUint8 {
			return nil, errors.New("Earn MAX token account index exceeds u8")
		}
		if index == claimIndex {
			continue
		}
		pre, post := orZero(tokenAmount(read.Pre, key)), orZero(tokenAmount(read.Post, key))
		var opposite uint64
		if increased && pre > post {
			opposite = pre - post
		} else if !increased && post > pre {
			opposite = post - pre
		}
		if opposite == amount {
			peers = append(peers, peer{key, pre, post})
		}
	}
	if len(peers) != 1 {
		return nil, fmt.Errorf("Earn MAX claim-custody delta has %d exact counterparty token accounts", len(peers))
	}
	counterparty := peers[0]
	transfer := &custodyTransfer{wire: read.Wire, lookupTables: tables, transaction: transaction, signature: signature, slot: read.Slot}
	if increased {
		transfer.source, transfer.destination = counterparty.key, custody
		transfer.sourcePre, transfer.sourcePost = counterparty.pre, counterparty.post
		transfer.destinationPre, transfer.destinationPost = claimPre, claimPost
		return transfer, nil
	}
	// A root claim moves only between existing token accounts: missing rows
	// are unknown, not zero, so a create or close cannot hide behind it.
	for _, key := range []solana.PublicKey{custody, counterparty.key} {
		if tokenAmount(read.Pre, key) == nil || tokenAmount(read.Post, key) == nil {
			return nil, errors.New("Earn MAX root claim receipt omitted an actual account balance")
		}
	}
	transfer.source, transfer.destination = custody, counterparty.key
	transfer.sourcePre, transfer.sourcePost = claimPre, claimPost
	transfer.destinationPre, transfer.destinationPost = counterparty.pre, counterparty.post
	return transfer, nil
}

func usdcBalance(account solana.PublicKey, amount uint64) multiply.TokenBalance {
	return multiply.TokenBalance{Account: account.String(), Mint: multiply.USDCMint, TokenProgram: multiply.TokenProgram, AmountRaw: amount}
}

// projectEarnMaxAccountUpdate is project_earn_max_account_update.
// SnapshotApplicable reports whether a confirmed-state read of address,
// recovered without the transaction that changed it, can be applied to vault.
// Rust never enqueued such a job: every Rust Earn job carried its signature.
// Two facts are proven only by their transaction, so an unsigned job for them
// fails until it is dead-lettered. An Earn MAX claim-custody change needs its
// exact transfer; its custody history is recovered by signature instead
// (recoverEarnMaxGaps, Rust's enqueue_earn_max_rpc_gap_updates). A closed
// classic policy needs its closing transaction for the refund or cleanup.
func SnapshotApplicable(vault watch.Vault, address string, deleted bool) bool {
	if vault.EarnMax {
		vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
		return err == nil && address != associatedToken(vaultKey, usdcMint, tokenProgram).String()
	}
	return !deleted || !isPolicyDeletion(NormalizedUpdate{EventKind: "account_deleted", AccountPubkey: &address}, vault)
}

func (a *Application) projectEarnMaxAccountUpdate(ctx context.Context, update NormalizedUpdate, vault watch.Vault) error {
	idle := false
	for _, filter := range update.Filters {
		idle = idle || filter == watch.EarnIdleTokenAccounts
	}
	if !vault.EarnMax || !idle {
		return nil
	}
	settings, err := solana.PublicKeyFromBase58(vault.Settings)
	if err != nil {
		return err
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault.Vault)
	if err != nil {
		return err
	}
	custody := associatedToken(vaultKey, usdcMint, tokenProgram)
	if update.AccountPubkey == nil || *update.AccountPubkey != custody.String() {
		return nil
	}
	if update.Signature == nil {
		return errors.New("Earn MAX claim-custody update omitted transaction signature")
	}
	routeKey := fmt.Sprintf("earn-max:%s:%d", settings, vault.VaultIndex)
	if exists, err := a.multiply.OperationExistsForSignature(ctx, routeKey, *update.Signature); err != nil || exists {
		return err
	}
	current, err := a.routeEngineIsCurrent(ctx, routeKey)
	if err != nil || !current {
		return err
	}
	stored, err := a.multiply.LoadRouteState(ctx, routeKey)
	if err != nil {
		return err
	}
	if stored == nil {
		return errors.New("Earn MAX account update has no route state")
	}
	transfer, err := readCustodyTransfer(ctx, a.rpc, *update.Signature, update.Slot, custody)
	if err != nil {
		return err
	}
	state := stored.State
	switch {
	case transfer.destination == custody:
		if state.Goal == multiply.GoalWithdraw {
			return errors.New("Earn MAX deposit arrived while withdrawal is active; retry after claim")
		}
		_, err = a.projectCashFlow(ctx, settings, "", transfer)
	case transfer.source == custody:
		withdrawal := state.Withdrawal
		if withdrawal == nil {
			return errors.New("unrecognized Earn MAX claim-custody debit")
		}
		if withdrawal.Status != multiply.WithdrawalClaimable || withdrawal.DestinationAccount != transfer.destination.String() {
			return errors.New("Earn MAX claim-custody debit does not match a claimable withdrawal")
		}
		_, err = a.projectCashFlow(ctx, settings, withdrawal.RequestID, transfer)
	default:
		return errors.New("Earn MAX account update did not change claim custody")
	}
	return err
}

// routeEngineIsCurrent skips a route not on the current engine, as the Rust
// projection did after its version check.
func (a *Application) routeEngineIsCurrent(ctx context.Context, routeKey string) (bool, error) {
	var engine *string
	err := a.multiply.Pool().QueryRow(ctx, `SELECT state->>'engineVersion' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, routeKey).Scan(&engine)
	if err != nil {
		return false, err
	}
	return engine != nil && *engine == multiply.EngineVersion, nil
}

// validateRootClaim is the single Earn MAX payout validation call site: the
// claim must be the exact root-wallet transfer of the saved request. Its
// semantics are owned by multiply.ValidateWalletClaimReceipt.
func validateRootClaim(route *multiply.RouteState, requestID string, transfer *custodyTransfer) error {
	topology, err := multiply.TopologyForRoute(route)
	if err != nil {
		return err
	}
	source, destination := usdcBalance(transfer.source, transfer.sourcePre), usdcBalance(transfer.destination, transfer.destinationPre)
	sourceAfter, destinationAfter := usdcBalance(transfer.source, transfer.sourcePost), usdcBalance(transfer.destination, transfer.destinationPost)
	return multiply.ValidateWalletClaimReceipt(route, topology, requestID, &multiply.WalletClaimReceipt{
		Signature: transfer.signature, ConfirmationState: confirmedCommitment, ConfirmedSlot: transfer.slot, Wire: transfer.wire,
		LookupTables: transfer.lookupTables, SourceBefore: source, SourceAfter: sourceAfter, DestinationBefore: destination, DestinationAfter: destinationAfter,
	})
}

// projectCashFlow is project_earn_max_confirmed_cash_flow; requestID is
// empty for a deposit and the claimable request for a claim.
func (a *Application) projectCashFlow(ctx context.Context, settings solana.PublicKey, requestID string, transfer *custodyTransfer) (bool, error) {
	routeKey := fmt.Sprintf("earn-max:%s:0", settings)
	if exists, err := a.multiply.OperationExistsForSignature(ctx, routeKey, transfer.signature); err != nil || exists {
		return false, err
	}
	digest := sha256.Sum256([]byte(transfer.signature + ":account"))
	operationID := "cash-" + hex.EncodeToString(digest[:])[:32]
	if existing, err := a.multiply.LoadOperation(ctx, operationID); err != nil || existing != nil {
		return false, err
	}
	lease, err := a.multiply.LeaseRoute(ctx, routeKey, "earn-max-laserstream", time.Now().Add(30*time.Second))
	if err != nil {
		return false, err
	}
	if lease == nil {
		return false, errors.New("Earn MAX cash-flow route is actively leased; replay after release")
	}
	stored, err := a.multiply.LoadRouteState(ctx, routeKey)
	if err != nil {
		return false, err
	}
	if stored == nil {
		return false, errors.New("Earn MAX cash-flow route is not projected")
	}
	state := stored.State
	if transfer.sourcePre < transfer.sourcePost {
		return false, errors.New("Earn MAX cash flow source was not debited")
	}
	amount := transfer.sourcePre - transfer.sourcePost
	if amount > math.MaxInt64 {
		return false, errors.New("Earn MAX cash flow exceeds i64")
	}
	now := time.Now().UTC()
	action := multiply.ActionDepositClaimAsset
	if requestID == "" {
		if state.Position.Kind == "idle" {
			state.Position = multiply.NewIdlePosition(usdcBalance(transfer.destination, transfer.destinationPost))
		}
		state.ObservedSlot, state.ObservedAt = transfer.slot, now
		if err := state.AdmitDeposit(multiply.DepositEvidence{
			RequestID: "chain:" + transfer.signature, TransactionSignature: transfer.signature, WalletAccount: transfer.source.String(),
			WalletPreAmountRaw: transfer.sourcePre, WalletPostAmountRaw: transfer.sourcePost, VaultPreAmountRaw: transfer.destinationPre,
			VaultPostAmountRaw: transfer.destinationPost, AmountRaw: amount, ObservedSlot: transfer.slot, ObservedAt: now,
		}); err != nil {
			return false, err
		}
	} else {
		if state.Goal != multiply.GoalWithdraw || state.CurrentOperationID != nil || transfer.slot < state.ObservedSlot {
			return false, errors.New("Earn MAX root claim has no current wallet-owned request")
		}
		if state.Withdrawal == nil {
			return false, errors.New("Earn MAX claim route omitted withdrawal")
		}
		if err := validateRootClaim(state, requestID, transfer); err != nil {
			return false, err
		}
		withdrawal := state.Withdrawal
		if withdrawal.Status != multiply.WithdrawalClaimable || withdrawal.RequestID != requestID || withdrawal.DestinationAccount != transfer.destination.String() ||
			withdrawal.AmountRaw != amount || transfer.sourcePre < withdrawal.AmountRaw {
			return false, errors.New("Earn MAX claim did not match the exact claimable withdrawal")
		}
		signature := transfer.signature
		withdrawal.Status, withdrawal.ClaimSignature = multiply.WithdrawalClaimed, &signature
		state.Generation++
		state.Goal = multiply.GoalClaimed
		if transfer.sourcePost > 0 {
			state.Goal = multiply.GoalDeploy
		}
		state.Position = multiply.NewIdlePosition(usdcBalance(transfer.source, transfer.sourcePost))
		state.ObservedSlot, state.ObservedAt = transfer.slot, now
		action = multiply.ActionClaim
	}
	evidence, err := json.Marshal(map[string]any{"signature": transfer.signature, "observationSource": "exact_vault_ata", "slot": transfer.slot,
		"source": transfer.source.String(), "sourcePre": transfer.sourcePre, "sourcePost": transfer.sourcePost,
		"destination": transfer.destination.String(), "destinationPre": transfer.destinationPre, "destinationPost": transfer.destinationPost})
	if err != nil {
		return false, err
	}
	evidenceDigest := sha256.Sum256(evidence)
	reconciliation := hex.EncodeToString(evidenceDigest[:])
	signature, slot := transfer.signature, transfer.slot
	operation := &multiply.MultiplyOperation{
		OperationID: operationID, RouteKey: routeKey, Cycle: state.Cycle, EngineVersion: multiply.EngineVersion, Action: action,
		Status: multiply.StatusReconciled, IDempotencyKey: multiply.EngineVersion + ":cash-flow:" + transfer.signature,
		ExpectedEffects: multiply.ExpectedEffects{
			TokenAmountsBefore: []multiply.TokenAmountBefore{
				{Account: transfer.source.String(), Mint: multiply.USDCMint, AmountRaw: transfer.sourcePre},
				{Account: transfer.destination.String(), Mint: multiply.USDCMint, AmountRaw: transfer.destinationPre},
			},
			TokenDeltas: []multiply.TokenDelta{
				{Account: transfer.source.String(), Mint: multiply.USDCMint, RawDelta: -int64(amount)},
				{Account: transfer.destination.String(), Mint: multiply.USDCMint, RawDelta: int64(amount)},
			},
		},
		TransactionSignature: &signature, ConfirmedSlot: &slot, ReconciliationSHA256: &reconciliation, CreatedAt: now, UpdatedAt: now,
	}
	inserted, err := a.multiply.AdmitExternalOperation(ctx, lease, state, operation)
	if err != nil {
		return false, err
	}
	released, err := a.multiply.ReleaseLease(ctx, lease)
	if err != nil {
		return false, err
	}
	if !released {
		return false, errors.New("Earn MAX cash-flow projection lost its route lease")
	}
	return inserted, nil
}
