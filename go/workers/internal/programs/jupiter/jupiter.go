// Package jupiter holds the Jupiter v6 aggregator facts the workers use, each
// defined once: the program ID, the route instruction discriminators, the
// shared_accounts_route_v2 account order its policy constraint reads, the
// decoder for the legacy shared_accounts_route arguments, and the one
// swap/v1 HTTP client that quotes and fetches swap instructions.
//
// The swap API is swap/v1 at LiteBase or KeyedBase, and neither needs a key
// (see their const). A swap a policy admits is shared_accounts_route_v2, which
// the API returns only when /quote asks for instructionVersion=V2 and
// /swap-instructions passes useSharedAccounts; otherwise it returns the legacy
// shared_accounts_route, whose amounts follow its variable-length route plan
// where no fixed offset reaches them. The v2 constraint keeps a swap's output
// in the owner's destination account; it does not bound the swap's price
// (SharedAccountsRouteV2Allowed).
package jupiter

import (
	"encoding/binary"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// ProgramID is the Jupiter v6 aggregator program.
var ProgramID = solana.MustPublicKeyFromBase58("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")

// Anchor discriminators, sha256("global:<instruction>")[:8], of the route
// instructions we sign or authorize.
var (
	RouteDiscriminator                 = [8]byte{229, 23, 203, 151, 122, 227, 173, 42}
	SharedAccountsRouteDiscriminator   = [8]byte{193, 32, 155, 51, 65, 214, 156, 129}
	RouteV2Discriminator               = [8]byte{187, 100, 250, 204, 49, 196, 175, 20}
	SharedAccountsRouteV2Discriminator = [8]byte{209, 152, 83, 147, 124, 254, 216, 233}
)

// The route arguments after the route plan, which end the legacy route data:
// (Legacy, with SharedAccountsRoute, SharedRouteAllowed, Route, RouteAllowed,
// ArgsPrefix and Bounds: Backyard's installed swap literals read them until part
// B moves them to SharedAccountsRouteV2Allowed, then they go.)
// in_amount u64, quoted_out_amount u64, slippage_bps u16 and platform_fee_bps
// u8, at these offsets from in_amount.
const (
	QuotedOutAfterInAmount   = 8
	SlippageAfterInAmount    = 16
	PlatformFeeAfterInAmount = 18
	RouteTailLen             = 19
)

// SharedAccountsRoute is the argument list of the legacy
// shared_accounts_route: id u8, route_plan Vec<RoutePlanStep>, then the route
// tail. Its route plan steps vary in length, so the amounts are read from the
// fixed tail.
type SharedAccountsRoute struct {
	Steps           uint32 // route_plan length
	InAmount        uint64
	QuotedOutAmount uint64
	SlippageBPS     uint16
	PlatformFeeBPS  uint8
}

// DecodeSharedAccountsRoute decodes legacy shared_accounts_route data: its
// discriminator, then at least the id, the route plan length and the tail.
func DecodeSharedAccountsRoute(data []byte) (SharedAccountsRoute, error) {
	if len(data) < 8+1+4+RouteTailLen || [8]byte(data[:8]) != SharedAccountsRouteDiscriminator {
		return SharedAccountsRoute{}, errors.New("not a Jupiter shared_accounts_route")
	}
	tail := data[len(data)-RouteTailLen:]
	return SharedAccountsRoute{
		Steps:           binary.LittleEndian.Uint32(data[9:13]),
		InAmount:        binary.LittleEndian.Uint64(tail),
		QuotedOutAmount: binary.LittleEndian.Uint64(tail[QuotedOutAfterInAmount:]),
		SlippageBPS:     binary.LittleEndian.Uint16(tail[SlippageAfterInAmount:]),
		PlatformFeeBPS:  tail[PlatformFeeAfterInAmount],
	}, nil
}

// SharedRouteAllowed is what a policy admits in each account of the legacy
// shared_accounts_route, in its order; the route's remaining accounts follow
// it and stay free, as does the trailing program account. The instruction
// itself comes from the swap API, so this order serves the constraint alone.
// PlatformFee is optional: ProgramID when the route takes no platform fee.
type SharedRouteAllowed struct {
	TokenProgram, ProgramAuthority, User, Source, ProgramSource, ProgramDestination squads.Slot
	Destination, SourceMint, DestinationMint, PlatformFee, Token2022Program         squads.Slot
	EventAuthority                                                                  squads.Slot
}

// SharedAccountsRouteAllowed admits shared_accounts_route over the allowed
// accounts, any route plan and amounts the data predicates after its
// discriminator admit (Bounds, ArgsPrefix).
func SharedAccountsRouteAllowed(a SharedRouteAllowed, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return allow(SharedAccountsRouteDiscriminator, data, []squads.Slot{a.TokenProgram, a.ProgramAuthority, a.User,
		a.Source, a.ProgramSource, a.ProgramDestination, a.Destination, a.SourceMint, a.DestinationMint, a.PlatformFee,
		a.Token2022Program, a.EventAuthority, squads.Any})
}

// Route is the legacy route account set a policy constrains, in its order;
// the route's remaining accounts, among them the source mint and its token
// program, follow it. TokenProgram is the destination's. DestinationAccount
// and PlatformFee are optional: ProgramID when absent. The instruction itself
// comes from the swap API, so only its policy constraint reads this list.
type Route struct {
	TokenProgram, User, Source, Destination, DestinationAccount, DestinationMint squads.Slot
	PlatformFee, EventAuthority                                                  squads.Slot
}

// RouteAllowed admits route over the allowed accounts and the remaining
// accounts it pins, by instruction position; every other remaining account,
// and the program, is free. Any route plan and amounts the data predicates
// after its discriminator admit.
func RouteAllowed(a Route, remaining map[int]squads.Slot, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	slots := []squads.Slot{a.TokenProgram, a.User, a.Source, a.Destination, a.DestinationAccount, a.DestinationMint,
		a.PlatformFee, a.EventAuthority, squads.Any}
	named := len(slots)
	for position, slot := range remaining {
		if position < named {
			panic("a remaining account follows the route's own accounts")
		}
		for len(slots) <= position {
			slots = append(slots, squads.Any)
		}
		slots[position] = slot
	}
	return allow(RouteDiscriminator, data, slots)
}

func allow(discriminator [8]byte, data []squads.DataConstraintView, slots []squads.Slot) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, append([]squads.DataConstraintView{squads.DataBytes(0, discriminator[:])}, data...), slots)
}

// ArgsPrefix admits only instruction data whose arguments, after the
// discriminator, start with prefix: a fixed id and route plan.
func ArgsPrefix(prefix []byte) squads.DataConstraintView {
	return squads.DataBytes(8, prefix)
}

// Bounds bound the route tail at inAmountAt: in_amount at most maxIn,
// slippage_bps at most maxSlippageBPS and no platform fee. The offsets are
// fixed, so they bound the tail only when the route plan has the length the
// policy was installed from; under a route plan of another length they read
// other bytes. Only a policy that also pins the route plan (ArgsPrefix) bounds
// in_amount whatever plan the swap API returns.
func Bounds(inAmountAt, maxIn uint64, maxSlippageBPS uint16) []squads.DataConstraintView {
	return []squads.DataConstraintView{
		squads.DataU64(inAmountAt, squads.OpLessThanOrEqualTo, maxIn),
		squads.DataU16(inAmountAt+SlippageAfterInAmount, squads.OpLessThanOrEqualTo, maxSlippageBPS),
		squads.DataU8(inAmountAt+PlatformFeeAfterInAmount, squads.OpEquals, 0),
	}
}

// shared_accounts_route_v2's arguments, each at a fixed offset ahead of its
// variable-length route plan: id u8 @8, in_amount u64 @9, quoted_out_amount
// u64 @17, slippage_bps u16 @25, platform_fee_bps u16 @27,
// positive_slippage_bps u16 @29, then route_plan Vec<RoutePlanStepV2> @31.
// The platform fee and the positive-slippage fee are the 4 bytes at
// V2FeesOffset; while both are zero the route takes no fee account.
const (
	V2IDOffset        = 8
	V2QuotedOutOffset = 17
	V2FeesOffset      = 27
	V2FeesLen         = 4
)

// ProgramAuthorities is how many program authorities shared routes run
// through: ids 0 to 15, each the PDA ["authority", [id]], each holding its own
// token accounts. The swap API picks the id per request.
const ProgramAuthorities = 16

// ProgramAuthority is the shared-route program authority of id.
func ProgramAuthority(id uint8) solana.PublicKey {
	return pda([]byte("authority"), []byte{id})
}

// EventAuthority is the Anchor event authority, ["__event_authority"].
var EventAuthority = pda([]byte("__event_authority"))

func pda(seeds ...[]byte) solana.PublicKey {
	key, _, err := solana.FindProgramAddress(seeds, ProgramID)
	if err != nil {
		panic(err)
	}
	return key
}

// SharedRouteV2 is shared_accounts_route_v2's account set in its order; the
// event authority and the program end it. The route's own accounts follow:
// a platform fee account when a fee is set, then each venue's accounts.
type SharedRouteV2[T any] struct {
	ProgramAuthority, User, Source, ProgramSource, ProgramDestination, Destination T
	SourceMint, DestinationMint, SourceTokenProgram, DestinationTokenProgram       T
}

type SharedRouteV2Allowed = SharedRouteV2[squads.Slot]

func sharedRouteV2Slots[T any](a SharedRouteV2[T], fixed func(solana.PublicKey) T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{
		squads.ReadOnly(a.ProgramAuthority), squads.Signing(a.User), squads.Writable(a.Source), squads.Writable(a.ProgramSource),
		squads.Writable(a.ProgramDestination), squads.Writable(a.Destination), squads.ReadOnly(a.SourceMint),
		squads.ReadOnly(a.DestinationMint), squads.ReadOnly(a.SourceTokenProgram), squads.ReadOnly(a.DestinationTokenProgram),
		squads.ReadOnly(fixed(EventAuthority)), squads.ReadOnly(fixed(ProgramID)),
	}
}

// SharedAccountsRouteV2Allowed admits shared_accounts_route_v2 over the
// allowed accounts with no platform or positive-slippage fee: a fee pays the
// account at 12, which no slot pins, so the fee bytes must be zero.
//
// What it guarantees: output reaches only Destination, since Jupiter itself
// rejects a route whose venue token accounts or venue authority are not its
// program authority's (InvalidOutputTokenAccount, RequireKeysEqViolated;
// mainnet tamper simulation, policy's TestSharedRouteV2TamperOnMainnet).
// What it does not: the price. id, in_amount, quoted_out_amount, slippage_bps
// and the route accounts are free by the owner's no-limits choice for swaps,
// and the delegate writes quoted_out as well as slippage, so a holder of the
// delegate key can sell the whole Source balance at any price through a pool
// it controls.
func SharedAccountsRouteV2Allowed(a SharedRouteV2Allowed) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, []squads.DataConstraintView{
		squads.DataBytes(0, SharedAccountsRouteV2Discriminator[:]),
		squads.DataBytes(V2FeesOffset, make([]byte, V2FeesLen)),
	}, squads.Slots(ProgramID, sharedRouteV2Slots(a, squads.Pinned)))
}

// OwnSwapAllowed is the account set of owner swapping sourceMint for
// destinationMint between its own associated token accounts, through any
// program authority: each slot pins the one account it can honestly hold, and
// the authority and its two token accounts pin all ProgramAuthorities of them.
// Each token program is its mint's own, so Token-2022 mints work.
func OwnSwapAllowed(owner, sourceMint, sourceTokenProgram, destinationMint, destinationTokenProgram solana.PublicKey) (SharedRouteV2Allowed, error) {
	var authorities, programSources, programDestinations []solana.PublicKey
	for id := range ProgramAuthorities {
		authority := ProgramAuthority(uint8(id))
		source, err := spl.AssociatedTokenAddress(authority, sourceMint, sourceTokenProgram)
		if err != nil {
			return SharedRouteV2Allowed{}, err
		}
		destination, err := spl.AssociatedTokenAddress(authority, destinationMint, destinationTokenProgram)
		if err != nil {
			return SharedRouteV2Allowed{}, err
		}
		authorities, programSources, programDestinations = append(authorities, authority), append(programSources, source), append(programDestinations, destination)
	}
	source, err := spl.AssociatedTokenAddress(owner, sourceMint, sourceTokenProgram)
	if err != nil {
		return SharedRouteV2Allowed{}, err
	}
	destination, err := spl.AssociatedTokenAddress(owner, destinationMint, destinationTokenProgram)
	if err != nil {
		return SharedRouteV2Allowed{}, err
	}
	return SharedRouteV2Allowed{ProgramAuthority: squads.Pin(authorities...), User: squads.Pin(owner), Source: squads.Pin(source),
		ProgramSource: squads.Pin(programSources...), ProgramDestination: squads.Pin(programDestinations...),
		Destination: squads.Pin(destination), SourceMint: squads.Pin(sourceMint), DestinationMint: squads.Pin(destinationMint),
		SourceTokenProgram: squads.Pin(sourceTokenProgram), DestinationTokenProgram: squads.Pin(destinationTokenProgram)}, nil
}
