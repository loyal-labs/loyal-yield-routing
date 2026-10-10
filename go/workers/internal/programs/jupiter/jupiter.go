// Package jupiter holds the Jupiter v6 aggregator facts the workers use, each
// defined once: the program ID, the route instruction discriminators, the
// shared_accounts_route_v2 account order and argument offsets its policy
// constraint reads, the decoder for the legacy shared_accounts_route
// arguments (Multiply still swaps through it), and the one swap/v1 HTTP client
// that quotes and fetches swap instructions.
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
	SharedAccountsRouteDiscriminator   = [8]byte{193, 32, 155, 51, 65, 214, 156, 129}
	RouteV2Discriminator               = [8]byte{187, 100, 250, 204, 49, 196, 175, 20}
	SharedAccountsRouteV2Discriminator = [8]byte{209, 152, 83, 147, 124, 254, 216, 233}
)

// The legacy route tail, after the route plan, which ends the legacy
// shared_accounts_route data: in_amount u64, quoted_out_amount u64,
// slippage_bps u16 and platform_fee_bps u8.
const routeTailLen = 19

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
	if len(data) < 8+1+4+routeTailLen || [8]byte(data[:8]) != SharedAccountsRouteDiscriminator {
		return SharedAccountsRoute{}, errors.New("not a Jupiter shared_accounts_route")
	}
	tail := data[len(data)-routeTailLen:]
	return SharedAccountsRoute{
		Steps:           binary.LittleEndian.Uint32(data[9:13]),
		InAmount:        binary.LittleEndian.Uint64(tail),
		QuotedOutAmount: binary.LittleEndian.Uint64(tail[8:]),
		SlippageBPS:     binary.LittleEndian.Uint16(tail[16:]),
		PlatformFeeBPS:  tail[18],
	}, nil
}

// shared_accounts_route_v2's arguments, each at a fixed offset ahead of its
// variable-length route plan: id u8 @8, in_amount u64 @9, quoted_out_amount
// u64 @17, slippage_bps u16 @25, platform_fee_bps u16 @27,
// positive_slippage_bps u16 @29, then route_plan Vec<RoutePlanStepV2> @31.
// The platform fee and the positive-slippage fee are the 4 bytes at
// V2FeesOffset; while both are zero the route takes no fee account.
const (
	V2IDOffset        = 8
	V2InAmountOffset  = 9
	V2QuotedOutOffset = 17
	V2SlippageOffset  = 25
	V2FeesOffset      = 27
	V2FeesLen         = 4
	V2RoutePlanOffset = 31
)

// SharedRouteV2Args are shared_accounts_route_v2's arguments ahead of its
// route plan, but the fees, which a policy pins to zero.
type SharedRouteV2Args struct {
	ID                        uint8
	InAmount, QuotedOutAmount uint64
	SlippageBPS               uint16
}

// DecodeSharedAccountsRouteV2Args reads shared_accounts_route_v2 data's fixed
// arguments, after its discriminator and before its route plan.
func DecodeSharedAccountsRouteV2Args(data []byte) (SharedRouteV2Args, error) {
	if len(data) < V2RoutePlanOffset+4 || [8]byte(data[:8]) != SharedAccountsRouteV2Discriminator {
		return SharedRouteV2Args{}, errors.New("not a Jupiter shared_accounts_route_v2")
	}
	return SharedRouteV2Args{
		ID:              data[V2IDOffset],
		InAmount:        binary.LittleEndian.Uint64(data[V2InAmountOffset:]),
		QuotedOutAmount: binary.LittleEndian.Uint64(data[V2QuotedOutOffset:]),
		SlippageBPS:     binary.LittleEndian.Uint16(data[V2SlippageOffset:]),
	}, nil
}

// ProgramAuthorities is how many program authorities shared routes run
// through: ids 0 to 15, each the PDA ["authority", [id]], each holding its own
// token accounts. The swap API picks the id per request.
const ProgramAuthorities = 16

// APIAuthorities is how many of them the swap API routes through: ids 0 to 7
// were every id in 190 live /swap-instructions answers on
// 2026-10-09 (every Backyard edge, several amounts), none 8 to 15. A policy
// that pins only these refuses a swap through ids 8 to 15, which the API has
// not returned.
const APIAuthorities = 8

// ProgramAuthority is the shared-route program authority of id.
func ProgramAuthority(id uint8) solana.PublicKey {
	return pda([]byte("authority"), []byte{id})
}

// Authorities are the program authorities of ids 0 to n-1.
func Authorities(n int) []solana.PublicKey {
	out := make([]solana.PublicKey, n)
	for id := range out {
		out[id] = ProgramAuthority(uint8(id))
	}
	return out
}

// AuthorityTokenAccounts are the associated token accounts of mint that the
// program authorities of ids 0 to n-1 hold, under tokenProgram.
func AuthorityTokenAccounts(n int, mint, tokenProgram solana.PublicKey) []solana.PublicKey {
	out := make([]solana.PublicKey, n)
	for id, authority := range Authorities(n) {
		account, err := spl.AssociatedTokenAddress(authority, mint, tokenProgram)
		if err != nil {
			panic(err)
		}
		out[id] = account
	}
	return out
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
// it controls. data adds predicates at fixed offsets, such as a pinned route
// plan at V2RoutePlanOffset.
func SharedAccountsRouteV2Allowed(a SharedRouteV2Allowed, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, append([]squads.DataConstraintView{
		squads.DataBytes(0, SharedAccountsRouteV2Discriminator[:]),
		squads.DataBytes(V2FeesOffset, make([]byte, V2FeesLen)),
	}, data...), squads.Slots(ProgramID, sharedRouteV2Slots(a, squads.Pinned)))
}

// OwnSwapAllowed is the account set of owner swapping sourceMint for
// destinationMint between its own associated token accounts, through any
// program authority: each slot pins the one account it can honestly hold, and
// the authority and its two token accounts pin all ProgramAuthorities of them.
// Each token program is its mint's own, so Token-2022 mints work.
func OwnSwapAllowed(owner, sourceMint, sourceTokenProgram, destinationMint, destinationTokenProgram solana.PublicKey) (SharedRouteV2Allowed, error) {
	source, err := spl.AssociatedTokenAddress(owner, sourceMint, sourceTokenProgram)
	if err != nil {
		return SharedRouteV2Allowed{}, err
	}
	destination, err := spl.AssociatedTokenAddress(owner, destinationMint, destinationTokenProgram)
	if err != nil {
		return SharedRouteV2Allowed{}, err
	}
	return SharedRouteV2Allowed{ProgramAuthority: squads.Pin(Authorities(ProgramAuthorities)...), User: squads.Pin(owner), Source: squads.Pin(source),
		ProgramSource:      squads.Pin(AuthorityTokenAccounts(ProgramAuthorities, sourceMint, sourceTokenProgram)...),
		ProgramDestination: squads.Pin(AuthorityTokenAccounts(ProgramAuthorities, destinationMint, destinationTokenProgram)...),
		Destination:        squads.Pin(destination), SourceMint: squads.Pin(sourceMint), DestinationMint: squads.Pin(destinationMint),
		SourceTokenProgram: squads.Pin(sourceTokenProgram), DestinationTokenProgram: squads.Pin(destinationTokenProgram)}, nil
}
