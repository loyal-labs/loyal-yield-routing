package backyard

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// View is Backyard's LaserStream view of its own accounts; planning reads it
// instead of polling pooled RPC. A start-up read takes every account at
// finalized, each keeping its own slot; the stream replays from the oldest of
// those slots at confirmed, and applies an update only when its (slot,
// write_version) is newer than the held one.
//
// S, the slot the view is complete through, is the previous confirmed slot
// status. The observer's frontier (the highest slot of any handled update) is
// a replay cursor, not a completeness claim, and neither it nor Yellowstone
// orders a slot's account updates before that slot's confirmed status. So S
// lags one confirmed slot, assuming every account update of a slot arrives
// before the next confirmed slot's status. Accounts may hold writes newer
// than S; none older than S is missing.
type View struct {
	rpc       *chain.Client
	connector stream.Connector
	fixed     []string
	ctx       context.Context // owns the stream

	// control serializes reconnects and handoffs; it guards manager and subscribed.
	control    sync.Mutex
	manager    *stream.Manager
	subscribed time.Time

	mu       sync.Mutex
	accounts map[string]viewAccount
	// receipts maps every known withdrawal receipt to its discovery slot
	// until it is in the by-address filter, then to zero; pending counts the
	// nonzero ones.
	receipts           map[string]uint64
	pending            int
	seedLow, seedHigh  uint64
	confirmed, through uint64
	slotAt             time.Time
}

type viewAccount struct {
	ConfirmedAccount
	slot, writeVersion uint64
}

const (
	// Receipts are discovered by owner and memcmp. Those filters never match a
	// closed account (owner System, no data), so every known receipt is also
	// in the by-address filter, which delivers its close.
	viewAccountsFilter = "backyard_accounts"
	viewReceiptsFilter = "backyard_voltr_receipts"
	viewSlotsFilter    = "backyard_slots"
	viewLiveness       = 5 * time.Second
	viewStall          = 30 * time.Second
	viewReplaySlots    = 32
	// The provider replays 200,000 slots (observer runtime.go); a longer gap
	// needs a fresh start-up read.
	viewReplayWindow = 200_000 * 400 * time.Millisecond
)

// OpenView reads the view's accounts once and, given a connector, streams
// them. Without one the view is its start-up read, which one-shot operator
// commands plan from.
func OpenView(ctx context.Context, client *chain.Client, connector stream.Connector) (*View, error) {
	v := &View{rpc: client, connector: connector, fixed: viewAddresses(), ctx: ctx}
	if err := v.seed(ctx); err != nil {
		return nil, err
	}
	if connector == nil {
		v.through = v.seedLow
		return v, nil
	}
	return v, v.subscribe(v.seedLow + 1)
}

func (v *View) Close() {
	v.control.Lock()
	defer v.control.Unlock()
	if v.manager != nil {
		v.manager.Close()
	}
}

// viewAddresses is every fixed account a route-catalog lane observes, plus
// the pinned program identity accounts.
func viewAddresses() []string {
	addresses := []string{reportTicketPDA, budgetClockAddress, kaminoMarket, kaminoPrimeLiquiditySupply, kaminoUSDCLiquiditySupply,
		kaminoCollateralReserve, kaminoDebtReserve, kaminoPrimeCustody, kaminoPrimeUSDCObligation,
		voltr.ProgramID.String(), bridgeAdaptorProgram, voltrProgramDataAddress, adaptorProgramDataAddress}
	for _, lane := range []string{RouteID, PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC", autoAUTOPYUSD.Lane, ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		route, _ := runtimeRoute(lane)
		addresses = append(append(addresses, pinnedRouteNAVAddressesForRoute(route)...),
			route.CollateralLiquiditySupply, route.DebtLiquiditySupply, route.DebtFeeReceiver)
	}
	return uniqueNonzero(addresses)
}

// seed is the start-up read. It is not a fallback: planning never reads RPC
// for these accounts, and it runs again only after a gap longer than the
// provider's replay window.
func (v *View) seed(ctx context.Context) error {
	accounts, receipts := map[string]viewAccount{}, map[string]uint64{}
	low, high := uint64(math.MaxUint64), uint64(0)
	held := func(account ConfirmedAccount, slot uint64) {
		// A finalized read is the end of its slot: no write of that slot is newer.
		accounts[account.Address] = viewAccount{account, slot, math.MaxUint64}
		low, high = min(low, slot), max(high, slot)
	}
	// ProgramData images are read alone: the Voltr image is over a megabyte
	// and batch responses are size-capped.
	programData := []string{voltrProgramDataAddress, adaptorProgramDataAddress}
	rest := slices.DeleteFunc(slices.Clone(v.fixed), func(address string) bool { return slices.Contains(programData, address) })
	for _, batch := range [][]string{rest, programData[:1], programData[1:]} {
		keys, err := publicKeys(batch)
		if err != nil {
			return err
		}
		slot, read, err := v.rpc.Accounts(ctx, keys, rpc.CommitmentFinalized, 0)
		if err != nil {
			return unavailable(err)
		}
		for i, account := range read {
			if account == nil {
				held(ConfirmedAccount{Address: batch[i]}, slot)
			} else {
				held(confirmedAccount(batch[i], account), slot)
			}
		}
	}
	vault := solana.MustPublicKeyFromBase58(bridgeVoltrVault)
	slot, read, err := v.rpc.ProgramAccounts(ctx, voltr.ProgramID, voltr.WithdrawalReceiptFilters(vault), rpc.CommitmentFinalized, 0)
	if err != nil {
		return unavailable(err)
	}
	for _, account := range read {
		held(confirmedAccount(account.Key.String(), &account), slot)
		receipts[account.Key.String()] = 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.accounts, v.receipts, v.pending, v.seedLow, v.seedHigh, v.confirmed, v.through = accounts, receipts, 0, low, high, 0, 0
	return nil
}

// request subscribes the fixed accounts and known receipts by address, new
// receipts by owner and memcmp, and confirmed slots.
func (v *View) request(from uint64) *pb.SubscribeRequest {
	v.mu.Lock()
	addresses := slices.Clone(v.fixed)
	for address := range v.receipts {
		addresses = append(addresses, address)
	}
	v.mu.Unlock()
	slices.Sort(addresses)
	discovery := &pb.SubscribeRequestFilterAccounts{Owner: []string{voltr.ProgramID.String()}}
	for _, filter := range voltr.WithdrawalReceiptFilters(solana.MustPublicKeyFromBase58(bridgeVoltrVault)) {
		discovery.Filters = append(discovery.Filters, &pb.SubscribeRequestFilterAccountsFilter{Filter: &pb.SubscribeRequestFilterAccountsFilter_Memcmp{
			Memcmp: &pb.SubscribeRequestFilterAccountsFilterMemcmp{Offset: filter.Memcmp.Offset, Data: &pb.SubscribeRequestFilterAccountsFilterMemcmp_Bytes{Bytes: filter.Memcmp.Bytes}},
		}})
	}
	byCommitment, confirmed := true, pb.CommitmentLevel_CONFIRMED
	return &pb.SubscribeRequest{
		Accounts:   map[string]*pb.SubscribeRequestFilterAccounts{viewAccountsFilter: {Account: addresses}, viewReceiptsFilter: discovery},
		Slots:      map[string]*pb.SubscribeRequestFilterSlots{viewSlotsFilter: {FilterByCommitment: &byCommitment}},
		Commitment: &confirmed,
		FromSlot:   &from,
	}
}

// subscribe runs under control or before the view is shared.
func (v *View) subscribe(from uint64) error {
	manager := stream.NewManager(v.connector, v, stream.Config{ReplayOverlapSlots: viewReplaySlots})
	v.subscribed = time.Now()
	if err := manager.Start(v.ctx, v.request(from)); err != nil {
		return err
	}
	v.manager = manager
	return nil
}

// Handle applies one stream update. It never blocks on IO.
func (v *View) Handle(_ context.Context, update *pb.SubscribeUpdate) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if slot := update.GetSlot(); slot != nil {
		if slot.Status == pb.SlotStatus_SLOT_CONFIRMED && slot.Slot > v.confirmed {
			v.through, v.confirmed = v.confirmed, slot.Slot
		}
		v.slotAt = time.Now()
		return nil
	}
	write := update.GetAccount()
	if write.GetAccount() == nil {
		return nil
	}
	info := write.Account
	if len(info.Pubkey) != solana.PublicKeyLength || len(info.Owner) != solana.PublicKeyLength {
		return fmt.Errorf("LaserStream account update has a malformed key")
	}
	address := solana.PublicKeyFromBytes(info.Pubkey).String()
	if held, ok := v.accounts[address]; ok && (write.Slot < held.slot || write.Slot == held.slot && info.WriteVersion <= held.writeVersion) {
		return nil
	}
	// A closed account reads like an absent RPC account: its address alone.
	account := ConfirmedAccount{Address: address}
	if info.Lamports > 0 {
		account = ConfirmedAccount{Address: address, Owner: solana.PublicKeyFromBytes(info.Owner).String(), Lamports: info.Lamports, Data: info.Data, Executable: info.Executable}
	}
	v.accounts[address] = viewAccount{account, write.Slot, info.WriteVersion}
	if _, known := v.receipts[address]; !known && slices.Contains(update.Filters, viewReceiptsFilter) {
		v.receipts[address], v.pending = write.Slot, v.pending+1
	}
	return nil
}

// sync is the stream upkeep each read does first, never more than once at a
// time. A failed or stalled stream reconnects once from S−32 (after a gap
// longer than the replay window, from a fresh start-up read); the next read
// makes the next attempt. Discovered receipts are handed off into the
// by-address filter from their discovery slot, so a close is not missed.
func (v *View) sync(ctx context.Context) {
	if v.connector == nil || !v.control.TryLock() {
		return
	}
	defer v.control.Unlock()
	failed := v.manager == nil
	if !failed {
		select {
		case <-v.manager.Errors():
			failed = true
		default:
		}
	}
	v.mu.Lock()
	last, from := v.slotAt, max(v.through-min(v.through, viewReplaySlots), v.seedLow+1)
	var pending []string
	discovered := uint64(math.MaxUint64)
	for address, slot := range v.receipts {
		if slot > 0 {
			pending, discovered = append(pending, address), min(discovered, slot)
		}
	}
	v.mu.Unlock()
	var err error
	switch {
	case failed || time.Since(last) > viewStall && time.Since(v.subscribed) > viewStall:
		if v.manager != nil {
			v.manager.Close()
			v.manager = nil
		}
		if !last.IsZero() && time.Since(last) > viewReplayWindow {
			if err = v.seed(ctx); err != nil {
				return
			}
			from, pending = v.seedLow+1, nil
		}
		err = v.subscribe(min(from, discovered))
	case len(pending) > 0:
		err = v.manager.Handoff(ctx, v.request(discovered))
	}
	if err == nil {
		v.mu.Lock()
		for _, address := range pending {
			v.receipts[address], v.pending = 0, v.pending-1
		}
		v.mu.Unlock()
	}
}

// read returns addresses and the vault's open withdrawal receipts at S >=
// minSlot. Only an optional address may be absent; an address outside the
// view is an error, never an absence.
func (v *View) read(ctx context.Context, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, []programAccount, error) {
	if v == nil {
		return 0, nil, nil, fmt.Errorf("account view is required")
	}
	v.sync(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case v.connector != nil && v.through < v.seedHigh:
		return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view has not passed its start-up read"))
	case v.connector != nil && time.Since(v.slotAt) > viewLiveness:
		return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view has no recent confirmed slot"))
	case v.pending > 0:
		return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view has a receipt filter change pending"))
	case int64(v.through) < minSlot:
		return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view slot %d is behind %d", v.through, minSlot))
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, address := range addresses {
		held, ok := v.accounts[address]
		if !ok {
			return 0, nil, nil, fmt.Errorf("account %s is not in the view", address)
		}
		if held.Lamports == 0 && !slices.Contains(optional, address) {
			return 0, nil, nil, fmt.Errorf("required account %s is absent", address)
		}
		accounts[i] = held.ConfirmedAccount
	}
	var receipts []programAccount
	for address := range v.receipts {
		if held := v.accounts[address]; held.Lamports > 0 {
			receipts = append(receipts, programAccount{Address: address, Account: held.ConfirmedAccount})
		}
	}
	return int64(v.through), accounts, receipts, nil
}
