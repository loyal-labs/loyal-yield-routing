package backyard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/stream"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// View is Backyard's LaserStream view of its own accounts; planning reads it
// instead of polling pooled RPC. A start-up read takes every account at
// finalized, each keeping its own slot, and the stream replays from the
// oldest of those slots at confirmed.
//
// The view is exactly the chain at S, the slot it is complete through: the
// previous confirmed slot status. The observer's frontier (the highest slot of
// any handled update) is a replay cursor, not a completeness claim, and
// neither it nor Yellowstone orders a slot's account updates before that
// slot's confirmed status. So S lags one confirmed slot, assuming every
// account update of a slot arrives before the next confirmed slot's status.
// Writes above S wait in arrival order until S reaches their slot; a write at
// or below S that arrives late is applied and counted in a warning.
type View struct {
	rpc       *chain.Client
	connector stream.Connector
	fixed     []string
	ctx       context.Context // owns the stream
	now       func() time.Time

	// control serializes reconnects and handoffs; it guards manager and subscribed.
	control    sync.Mutex
	manager    *stream.Manager
	subscribed time.Time

	mu       sync.Mutex
	accounts map[string]viewAccount
	queued   []viewAccount
	// receipts maps every known withdrawal receipt to its discovery slot
	// until it is in the by-address filter, then to zero; pending counts the
	// nonzero ones.
	receipts           map[string]uint64
	pending            int
	seedLow, seedHigh  uint64
	confirmed, through uint64
	advanced           chan struct{} // closed and replaced whenever S moves
	slotAt             time.Time     // arrival of the last slot status: stall detection only
	// session counts delivering stream sessions; the Manager hands each its
	// own context. A new session replays already applied slots, so the view
	// is not read until that session confirms a new slot.
	session    int
	sessionCtx context.Context
	replaying  bool
	late       int
}

type viewAccount struct {
	ConfirmedAccount
	slot, writeVersion uint64
	session            int // 0: the start-up read, the end of its slot
}

// supersedes reports whether w replaces held. write_version is node-local,
// so it orders writes of one slot only within one session; across sessions
// the later arrival wins, except over the start-up read.
func (w viewAccount) supersedes(held viewAccount) bool {
	switch {
	case w.slot != held.slot:
		return w.slot > held.slot
	case held.session == 0:
		return false
	case held.session == w.session:
		return w.writeVersion > held.writeVersion
	}
	return true
}

const (
	// Receipts are discovered by owner and memcmp. Those filters never match a
	// closed account (owner System, no data), so every known receipt is also
	// in the by-address filter, which delivers its close.
	viewAccountsFilter = "backyard_accounts"
	viewReceiptsFilter = "backyard_voltr_receipts"
	viewSlotsFilter    = "backyard_slots"
	// Chain time at S trails the wall clock by confirmation, the one-slot lag
	// and the stream: a few seconds. 10s leaves room for Clock drift while a
	// view tens of slots behind stops planning.
	viewMaxClockAge = 10 * time.Second
	viewStall       = 30 * time.Second
	viewReplaySlots = 32
	// The provider replays 200,000 slots (observer runtime.go).
	viewReplayWindow = 200_000 * 400 * time.Millisecond
)

// OpenView reads the view's accounts once and, given a connector, streams
// them until ctx ends. Without one the view is its start-up read, which
// one-shot commands plan from, labelled with its newest read slot: decoders
// treat any slot an account carries above the label as the future, and each
// read is the finalized state of its own slot, all within a second.
func OpenView(ctx context.Context, client *chain.Client, connector stream.Connector) (*View, error) {
	v := &View{rpc: client, connector: connector, fixed: viewAddresses(), ctx: ctx, now: time.Now,
		accounts: map[string]viewAccount{}, receipts: map[string]uint64{}, advanced: make(chan struct{}), seedLow: math.MaxUint64}
	if err := v.seed(ctx); err != nil {
		return nil, err
	}
	if connector == nil {
		v.through = v.seedHigh
		return v, nil
	}
	return v, v.subscribe(v.seedLow + 1)
}

// viewAddresses is every fixed account a route-catalog lane observes, prices,
// builds or admits against, plus the pinned program identity accounts. The
// stream bills by the byte, so no oracle or AMM pool is among them.
func viewAddresses() []string {
	metadata, _ := kamino.UserMetadataAddress(kaminoKey(bridgeVault))
	addresses := []string{reportTicketPDA, budgetClockAddress, kaminoMarket, kaminoPrimeLiquiditySupply, kaminoUSDCLiquiditySupply,
		kaminoCollateralReserve, kaminoDebtReserve, kaminoPrimeCustody, kaminoPrimeUSDCObligation,
		voltr.ProgramID.String(), bridgeAdaptorProgram, voltrProgramDataAddress, adaptorProgramDataAddress,
		bridgeVault, bridgeDelegate, metadata.String(), solana.SysVarRentPubkey.String(), solana.SystemProgramID.String(),
		bridgeUSDC, budgetSOLReserve, budgetWrappedSOLMint}
	for _, lane := range []string{RouteID, PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC", autoAUTOPYUSD.Lane, ethenaUSDePYUSD.Lane, primePRIMEPYUSD.Lane, primePRIMEUSDS.Lane} {
		route, _ := runtimeRoute(lane)
		addresses = append(append(addresses, pinnedRouteNAVAddressesForRoute(route)...),
			route.CollateralLiquiditySupply, route.DebtLiquiditySupply, route.DebtFeeReceiver, route.Kamino.CollateralMint, route.Kamino.DebtMint,
			route.CollateralReceiptMint, route.CollateralReceiptSupply, route.CollateralFarm, route.ObligationCollateralFarm, route.DebtFarm, route.ObligationDebtFarm)
	}
	return uniqueNonzero(addresses)
}

// seed is the start-up read, before the view is shared. It is not a
// fallback: planning never reads RPC for these accounts.
func (v *View) seed(ctx context.Context) error {
	held := func(account ConfirmedAccount, slot uint64) {
		v.accounts[account.Address] = viewAccount{ConfirmedAccount: account, slot: slot}
		v.seedLow, v.seedHigh = min(v.seedLow, slot), max(v.seedHigh, slot)
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
		v.receipts[account.Key.String()] = 0
	}
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

// Handle takes one stream update. It never blocks on IO.
func (v *View) Handle(ctx context.Context, update *pb.SubscribeUpdate) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx != v.sessionCtx {
		v.sessionCtx, v.session, v.replaying = ctx, v.session+1, true
	}
	if slot := update.GetSlot(); slot != nil {
		v.slotAt = time.Now()
		if slot.Status != pb.SlotStatus_SLOT_CONFIRMED || slot.Slot <= v.confirmed {
			return nil
		}
		v.through, v.confirmed, v.replaying = v.confirmed, slot.Slot, false
		v.queued = slices.DeleteFunc(v.queued, func(write viewAccount) bool {
			if write.slot > v.through {
				return false
			}
			v.apply(write)
			return true
		})
		close(v.advanced)
		v.advanced = make(chan struct{})
		if v.late > 0 {
			slog.Warn("backyard view applied account writes that arrived after their slot was complete", "count", v.late, "through", v.through)
			v.late = 0
		}
		return nil
	}
	write := update.GetAccount()
	if write.GetAccount() == nil {
		return nil
	}
	info := write.Account
	address := solana.PublicKeyFromBytes(info.Pubkey).String()
	// A closed account reads like an absent RPC account: its address alone.
	account := ConfirmedAccount{Address: address}
	if info.Lamports > 0 {
		account = ConfirmedAccount{Address: address, Owner: solana.PublicKeyFromBytes(info.Owner).String(), Lamports: info.Lamports, Data: info.Data, Executable: info.Executable}
	}
	// Discovery is recorded on arrival so the handoff starts at once.
	if _, known := v.receipts[address]; !known && slices.Contains(update.Filters, viewReceiptsFilter) {
		v.receipts[address], v.pending = write.Slot, v.pending+1
	}
	held := viewAccount{account, write.Slot, info.WriteVersion, v.session}
	if held.slot > v.through {
		v.queued = append(v.queued, held)
		return nil
	}
	if !v.replaying {
		v.late++
	}
	v.apply(held)
	return nil
}

func (v *View) apply(write viewAccount) {
	if held, ok := v.accounts[write.Address]; !ok || write.supersedes(held) {
		v.accounts[write.Address] = write
	}
}

// sync is the stream upkeep each read does first, never more than once at a
// time. A failed or stalled stream reconnects once from S−32; the next read
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

// slot is S of the live view: the "current slot" of planning. A view read's
// floor is a snapshot or decision slot, or our own receipt slot, never an RPC
// node's slot; an RPC read that must not predate the view takes S as its
// minContextSlot and keeps its own context slot.
func (v *View) slot(ctx context.Context) (int64, error) {
	slot, _, _, err := v.read(ctx, nil, 0)
	return slot, err
}

// errViewReplayGap stops the worker: a stream silent for longer than the
// provider replays cannot resume, and a restart takes a new start-up read.
var errViewReplayGap = errors.New("account view stream gap exceeds the provider replay window")

// read returns addresses and the vault's open withdrawal receipts at S >=
// minSlot, waiting for S to get there no longer than a live view's chain time
// may trail. Only an optional address may be absent; an address outside the
// view is an error, never an absence.
func (v *View) read(ctx context.Context, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, []programAccount, error) {
	if v == nil {
		return 0, nil, nil, fmt.Errorf("account view is required")
	}
	v.sync(ctx)
	wait, cancel := context.WithTimeout(ctx, viewMaxClockAge)
	defer cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	for int64(v.through) < minSlot {
		advanced := v.advanced
		v.mu.Unlock()
		select {
		case <-wait.Done():
		case <-advanced:
		}
		v.mu.Lock()
		if wait.Err() != nil && int64(v.through) < minSlot {
			return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view slot %d is behind %d", v.through, minSlot))
		}
	}
	if v.connector != nil {
		clockAge := v.now().Sub(time.Unix(clockUnixTimestamp([]ConfirmedAccount{v.accounts[budgetClockAddress].ConfirmedAccount}), 0))
		switch {
		case !v.slotAt.IsZero() && time.Since(v.slotAt) > viewReplayWindow:
			return 0, nil, nil, errViewReplayGap
		case v.through < v.seedHigh || v.replaying:
			return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view is replaying"))
		case clockAge > viewMaxClockAge:
			return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view chain time at slot %d is %s old", v.through, clockAge))
		case v.pending > 0:
			return 0, nil, nil, confirmedObservationUnavailable(fmt.Errorf("account view has a receipt filter change pending"))
		}
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
