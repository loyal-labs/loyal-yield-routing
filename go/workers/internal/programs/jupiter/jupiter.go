package jupiter

import (
	"encoding/binary"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

var ProgramID = solana.MustPublicKeyFromBase58("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")

var (
	SharedAccountsRouteDiscriminator   = anchor.Instruction("shared_accounts_route")
	RouteV2Discriminator               = anchor.Instruction("route_v2")
	SharedAccountsRouteV2Discriminator = anchor.Instruction("shared_accounts_route_v2")
)

// The legacy route tail follows the route plan and ends the
// shared_accounts_route data.
const (
	inAmountLen        = 8
	quotedOutAmountLen = 8
	slippageBPSLen     = 2
	platformFeeBPSLen  = 1
	routeTailLen       = inAmountLen + quotedOutAmountLen + slippageBPSLen + platformFeeBPSLen
)

type SharedAccountsRoute struct {
	Steps           uint32
	InAmount        uint64
	QuotedOutAmount uint64
	SlippageBPS     uint16
	PlatformFeeBPS  uint8
}

func DecodeSharedAccountsRoute(data []byte) (SharedAccountsRoute, error) {
	if len(data) < 8+1+4+routeTailLen || [8]byte(data[:8]) != SharedAccountsRouteDiscriminator {
		return SharedAccountsRoute{}, errors.New("not a Jupiter shared_accounts_route")
	}
	tail := data[len(data)-routeTailLen:]
	return SharedAccountsRoute{
		Steps:           binary.LittleEndian.Uint32(data[9:13]),
		InAmount:        binary.LittleEndian.Uint64(tail),
		QuotedOutAmount: binary.LittleEndian.Uint64(tail[inAmountLen:]),
		SlippageBPS:     binary.LittleEndian.Uint16(tail[inAmountLen+quotedOutAmountLen:]),
		PlatformFeeBPS:  tail[routeTailLen-platformFeeBPSLen],
	}, nil
}

// The platform fee and the positive-slippage fee are the V2FeesLen bytes at
// V2FeesOffset; while both are zero the route takes no fee account.
const (
	V2IDOffset             = 8
	V2InAmountOffset       = V2IDOffset + 1
	V2QuotedOutOffset      = V2InAmountOffset + inAmountLen
	V2SlippageOffset       = V2QuotedOutOffset + quotedOutAmountLen
	V2FeesOffset           = V2SlippageOffset + slippageBPSLen
	V2FeesLen              = platformFeeBPSV2Len + positiveSlippageBPSLen
	V2RoutePlanOffset      = V2FeesOffset + V2FeesLen
	platformFeeBPSV2Len    = 2
	positiveSlippageBPSLen = 2
)

type SharedRouteV2Args struct {
	ID                        uint8
	InAmount, QuotedOutAmount uint64
	SlippageBPS               uint16
}

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

const APIAuthorities = 8

func ProgramAuthority(id uint8) solana.PublicKey {
	return pda([]byte("authority"), []byte{id})
}

func Authorities(n int) []solana.PublicKey {
	out := make([]solana.PublicKey, n)
	for id := range out {
		out[id] = ProgramAuthority(uint8(id))
	}
	return out
}

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

var EventAuthority = pda([]byte("__event_authority"))

func pda(seeds ...[]byte) solana.PublicKey {
	key, _, err := solana.FindProgramAddress(seeds, ProgramID)
	if err != nil {
		panic(err)
	}
	return key
}

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
// account no slot pins, so the fee bytes must be zero.
//
// Output reaches only Destination, since Jupiter itself rejects a route whose
// venue token accounts or venue authority are not its program authority's
// (InvalidOutputTokenAccount, RequireKeysEqViolated; mainnet tamper
// simulation). The price is not bounded: id, in_amount, quoted_out_amount,
// slippage_bps and the route accounts are free, so a holder of the delegate
// key can sell the whole Source balance at any price through a pool it
// controls. data adds predicates at fixed offsets, such as a pinned route plan
// at V2RoutePlanOffset.
func SharedAccountsRouteV2Allowed(a SharedRouteV2Allowed, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	return squads.Allow(ProgramID, append([]squads.DataConstraintView{
		squads.DataBytes(0, SharedAccountsRouteV2Discriminator[:]),
		squads.DataBytes(V2FeesOffset, make([]byte, V2FeesLen)),
	}, data...), squads.Slots(ProgramID, sharedRouteV2Slots(a, squads.Pinned)))
}

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
