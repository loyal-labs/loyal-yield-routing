package earn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
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

type rpcTokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	UITokenAmount struct {
		Amount   string `json:"amount"`
		Decimals int    `json:"decimals"`
	} `json:"uiTokenAmount"`
}

type rpcBase64Transaction struct {
	Slot        uint64   `json:"slot"`
	Transaction []string `json:"transaction"`
	Meta        *struct {
		Err               json.RawMessage    `json:"err"`
		PreTokenBalances  *[]rpcTokenBalance `json:"preTokenBalances"`
		PostTokenBalances *[]rpcTokenBalance `json:"postTokenBalances"`
		LoadedAddresses   *struct {
			Writable []string `json:"writable"`
			Readonly []string `json:"readonly"`
		} `json:"loadedAddresses"`
	} `json:"meta"`
}

// tokenAmount is token_amount_optional: nil when the account has no row.
func tokenAmount(balances *[]rpcTokenBalance, index int, mint string) (*uint64, error) {
	if balances == nil {
		return nil, errors.New("Earn MAX cash-flow transaction omitted token balances")
	}
	var found *rpcTokenBalance
	for i := range *balances {
		row := &(*balances)[i]
		if row.AccountIndex != index || row.Mint != mint {
			continue
		}
		if found != nil {
			return nil, errors.New("Earn MAX cash-flow token balance is ambiguous")
		}
		found = row
	}
	if found == nil {
		return nil, nil
	}
	if found.UITokenAmount.Decimals != 6 {
		return nil, errors.New("Earn MAX cash-flow token balance is ambiguous")
	}
	amount, err := strconv.ParseUint(found.UITokenAmount.Amount, 10, 64)
	if err != nil {
		return nil, errors.New("Earn MAX cash-flow token amount is invalid")
	}
	return &amount, nil
}

func orZero(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

// readCustodyTransfer is read_confirmed_earn_max_account_transfer.
func readCustodyTransfer(ctx context.Context, rpc *solanarpc.Client, signature string, expectedSlot uint64, custody solana.PublicKey) (*custodyTransfer, error) {
	if _, err := solana.SignatureFromBase58(signature); err != nil {
		return nil, err
	}
	raw, found, err := rpc.Transaction(ctx, signature, "base64", confirmedCommitment)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errProofPending
	}
	var response rpcBase64Transaction
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if response.Slot != expectedSlot {
		return nil, errors.New("Earn MAX account cash-flow RPC slot drifted from LaserStream")
	}
	if len(response.Transaction) < 1 {
		return nil, errors.New("Earn MAX account cash-flow transaction bytes did not decode")
	}
	wire, err := base64.StdEncoding.DecodeString(response.Transaction[0])
	if err != nil {
		return nil, errors.New("Earn MAX account cash-flow transaction bytes did not decode")
	}
	transaction, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return nil, errors.New("Earn MAX account cash-flow transaction bytes did not decode")
	}
	meta := response.Meta
	if meta == nil {
		return nil, errors.New("Earn MAX account cash-flow metadata was missing")
	}
	if len(meta.Err) > 0 && string(meta.Err) != "null" {
		return nil, errors.New("Earn MAX account cash-flow transaction failed")
	}
	keys := append([]solana.PublicKey(nil), transaction.Message.AccountKeys...)
	tables := map[solana.PublicKey]solana.PublicKeySlice{}
	if meta.LoadedAddresses != nil {
		var loaded []solana.PublicKey
		for _, value := range append(append([]string(nil), meta.LoadedAddresses.Writable...), meta.LoadedAddresses.Readonly...) {
			key, err := solana.PublicKeyFromBase58(value)
			if err != nil {
				return nil, err
			}
			loaded = append(loaded, key)
		}
		keys = append(keys, loaded...)
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
	} else if transaction.Message.IsVersioned() {
		return nil, errors.New("Earn MAX cash-flow transaction omitted loaded addresses")
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
	mint := usdcMint.String()
	claimPreRaw, err := tokenAmount(meta.PreTokenBalances, claimIndex, mint)
	if err != nil {
		return nil, err
	}
	claimPostRaw, err := tokenAmount(meta.PostTokenBalances, claimIndex, mint)
	if err != nil {
		return nil, err
	}
	claimPre, claimPost := orZero(claimPreRaw), orZero(claimPostRaw)
	if claimPre == claimPost {
		return nil, errors.New("Earn MAX claim-custody account update has no token delta")
	}
	increased := claimPost > claimPre
	amount := claimPost - claimPre
	if !increased {
		amount = claimPre - claimPost
	}
	type peer struct {
		index     int
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
		preRaw, err := tokenAmount(meta.PreTokenBalances, index, mint)
		if err != nil {
			return nil, err
		}
		postRaw, err := tokenAmount(meta.PostTokenBalances, index, mint)
		if err != nil {
			return nil, err
		}
		pre, post := orZero(preRaw), orZero(postRaw)
		var opposite uint64
		if increased && pre > post {
			opposite = pre - post
		} else if !increased && post > pre {
			opposite = post - pre
		}
		if opposite == amount {
			peers = append(peers, peer{index, key, pre, post})
		}
	}
	if len(peers) != 1 {
		return nil, fmt.Errorf("Earn MAX claim-custody delta has %d exact counterparty token accounts", len(peers))
	}
	counterparty := peers[0]
	transfer := &custodyTransfer{wire: wire, lookupTables: tables, transaction: transaction, signature: signature, slot: response.Slot}
	if increased {
		transfer.source, transfer.destination = counterparty.key, custody
		transfer.sourcePre, transfer.sourcePost = counterparty.pre, counterparty.post
		transfer.destinationPre, transfer.destinationPost = claimPre, claimPost
		return transfer, nil
	}
	// A root claim moves only between existing token accounts: missing rows
	// are unknown, not zero, so a create or close cannot hide behind it.
	for _, index := range []int{claimIndex, counterparty.index} {
		pre, err := tokenAmount(meta.PreTokenBalances, index, mint)
		if err != nil {
			return nil, err
		}
		post, err := tokenAmount(meta.PostTokenBalances, index, mint)
		if err != nil {
			return nil, err
		}
		if pre == nil || post == nil {
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
