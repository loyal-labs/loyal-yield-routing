// Package jupiter holds the Jupiter v6 aggregator facts the workers use, each
// defined once: the program ID, the route instruction discriminators, the
// decoder for the legacy shared_accounts_route arguments, and the one swap/v1
// HTTP client that quotes and fetches swap instructions.
package jupiter

import (
	"encoding/binary"
	"errors"

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

// SharedAccountsRoute is the argument list of the legacy
// shared_accounts_route: id u8, route_plan Vec<RoutePlanStep>, in_amount
// u64, quoted_out_amount u64, slippage_bps u16, platform_fee_bps u8. Its
// route plan steps vary in length, so the amounts are read from the fixed
// 19-byte tail.
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
	if len(data) < 8+1+4+19 || [8]byte(data[:8]) != SharedAccountsRouteDiscriminator {
		return SharedAccountsRoute{}, errors.New("not a Jupiter shared_accounts_route")
	}
	tail := data[len(data)-19:]
	return SharedAccountsRoute{
		Steps:           binary.LittleEndian.Uint32(data[9:13]),
		InAmount:        binary.LittleEndian.Uint64(tail[0:8]),
		QuotedOutAmount: binary.LittleEndian.Uint64(tail[8:16]),
		SlippageBPS:     binary.LittleEndian.Uint16(tail[16:18]),
		PlatformFeeBPS:  tail[18],
	}, nil
}

// SharedRoute is the legacy shared_accounts_route account set, in its order;
// the route's remaining accounts follow it. The instruction itself comes from
// the swap API, so the slot list serves its policy constraint. PlatformFee is
// optional: ProgramID when the route takes no platform fee.
type SharedRoute[T any] struct {
	TokenProgram, ProgramAuthority, User, Source, ProgramSource, ProgramDestination T
	Destination, SourceMint, DestinationMint, PlatformFee, Token2022Program         T
	EventAuthority                                                                  T
}

func sharedRouteSlots[T any](a SharedRoute[T], fixed func(solana.PublicKey) T) []T {
	return []T{a.TokenProgram, a.ProgramAuthority, a.User, a.Source, a.ProgramSource, a.ProgramDestination,
		a.Destination, a.SourceMint, a.DestinationMint, a.PlatformFee, a.Token2022Program, a.EventAuthority, fixed(ProgramID)}
}

// SharedAccountsRouteAllowed admits shared_accounts_route over the allowed
// accounts, any route plan and amounts within bounds (Bounds, ArgsPrefix).
func SharedAccountsRouteAllowed(a SharedRoute[squads.Slot], fixed func(solana.PublicKey) squads.Slot, bounds ...squads.DataConstraintView) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, SharedAccountsRouteDiscriminator[:], sharedRouteSlots(a, fixed), bounds...)
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
// and the program, is free. Any route plan and amounts within bounds.
func RouteAllowed(a Route, remaining map[int]squads.Slot, bounds ...squads.DataConstraintView) squads.InstructionConstraintView {
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
	return squads.Allow(ProgramID, RouteDiscriminator[:], slots, bounds...)
}

// ArgsPrefix admits only instruction data whose arguments, after the
// discriminator, start with prefix: a fixed id and route plan.
func ArgsPrefix(prefix []byte) squads.DataConstraintView {
	return squads.DataConstraintView{DataOffset: 8, DataValue: squads.DataValueView{Kind: 5, Bytes: append([]byte(nil), prefix...)}, Operator: squads.OpEquals}
}

// Bounds are the route tail's limits: in_amount, at inAmountAt (after the
// route plan), at most maxIn; slippage_bps at most maxSlippageBPS; and no
// platform fee.
func Bounds(inAmountAt, maxIn uint64, maxSlippageBPS uint16) []squads.DataConstraintView {
	return []squads.DataConstraintView{
		squads.AtMost(inAmountAt, maxIn),
		{DataOffset: inAmountAt + 16, DataValue: squads.DataValueView{Kind: 1, U16: maxSlippageBPS}, Operator: squads.OpLessThanOrEqualTo},
		{DataOffset: inAmountAt + 18, DataValue: squads.DataValueView{Kind: 0}, Operator: squads.OpEquals},
	}
}
