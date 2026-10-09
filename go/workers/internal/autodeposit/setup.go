package autodeposit

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

type SetupStage string

const (
	SetupATA             SetupStage = "ata"
	SetupMetadata        SetupStage = "metadata"
	SetupObligation      SetupStage = "obligation"
	SetupFarm            SetupStage = "farm"
	maxSetupRentLamports            = 25_000_000
)

type DestinationSetupPlan struct {
	Stage             SetupStage `json:"stage"`
	Account           string     `json:"account"`
	PolicyAccount     string     `json:"policyAccount,omitempty"`
	Route             TopUpRoute `json:"route"`
	RentTopUpLamports uint64     `json:"rentTopUpLamports"`
	ObservedSlot      int64      `json:"observedSlot"`
}

// SetupAttempt is one signed destination setup stage, landed in-process.
type SetupAttempt struct {
	ClaimToken string
	Plan       DestinationSetupPlan
	Wire       BuiltWire
}

func (a SetupAttempt) durable() DurableAttempt {
	return DurableAttempt{ClaimToken: a.ClaimToken, OperationKind: OperationKind("setup_" + string(a.Plan.Stage)), Signature: a.Wire.Signature, SignedTransactionBase64: a.Wire.SignedTransactionBase64, SignedTransactionSHA256: a.Wire.SignedTransactionSHA256, RecentBlockhash: a.Wire.RecentBlockhash, LastValidBlockHeight: a.Wire.LastValidBlockHeight}
}

type SetupRentReader func(context.Context, int) (uint64, error)
type DestinationSetupBuilder interface {
	InspectDestinationSetup(context.Context, DepositPlan) (*DestinationSetupPlan, error)
	BuildDestinationSetup(context.Context, DepositPlan, DestinationSetupPlan, string, int64) (BuiltWire, error)
	ReadbackDestinationSetup(context.Context, DepositPlan, DestinationSetupPlan, int64) error
}

func NewSweepWireBuilderWithSetup(key ed25519.PrivateKey, read AccountReader, rent SetupRentReader) (*SweepWireBuilder, error) {
	if rent == nil {
		return nil, errors.New("destination setup requires a bounded rent reader")
	}
	b, err := NewSweepWireBuilder(key, read)
	if err != nil {
		return nil, err
	}
	b.rent = rent
	return b, nil
}
func validateDestinationSetupPlan(plan DepositPlan, setup DestinationSetupPlan) error {
	if err := validatePlanPublicKeys(plan); err != nil {
		return err
	}
	if setup.ObservedSlot <= 0 || setup.RentTopUpLamports > maxSetupRentLamports {
		return errors.New("setup observation/rent bound invalid")
	}
	vault := mustKey(plan.Target.VaultPubkey)
	obligationKey, err := kamino.VanillaObligation(vault, mustKey(plan.Market))
	if err != nil {
		return err
	}
	obligation := obligationKey.String()
	if setup.Route.Obligation != obligation || setup.Route.Position.Obligation != obligation || setup.Route.Position.Reserve != plan.Reserve || setup.Route.Position.Market != plan.Market || setup.Route.Position.LiquidityMint != plan.LiquidityMint || setup.Route.Position.LiquidityTokenProgram != splTokenID || setup.Route.Position.VaultLiquidityATA != plan.Target.VaultUsdcAta {
		return errors.New("setup route differs from frozen deposit identity")
	}
	var account string
	switch setup.Stage {
	case SetupATA:
		var ata solana.PublicKey
		ata, err = spl.AssociatedTokenAddress(vault, mustKey(USDCMint), solana.TokenProgramID)
		account = ata.String()
	case SetupMetadata:
		var metadata solana.PublicKey
		metadata, err = kamino.UserMetadataAddress(vault)
		account = metadata.String()
	case SetupObligation:
		account = obligation
	case SetupFarm:
		farm, keyErr := solana.PublicKeyFromBase58(setup.Route.Position.ReserveFarmState)
		if keyErr != nil {
			return keyErr
		}
		user, keyErr := kamino.ObligationFarmUserState(farm, obligationKey)
		if keyErr != nil {
			return keyErr
		}
		account = user.String()
		if setup.Route.Position.ObligationFarmUserState != account {
			return errors.New("setup farm derivation differs")
		}
	default:
		return errors.New("unknown setup stage")
	}
	if err != nil || account != setup.Account {
		return errors.New("setup account differs from derived frozen identity")
	}
	if setup.Stage == SetupMetadata || setup.Stage == SetupObligation {
		if setup.PolicyAccount == "" || (setup.PolicyAccount != plan.Target.RoutePolicyAccount && setup.PolicyAccount != plan.Target.SetupPolicyAccount) {
			return errors.New("setup policy differs from frozen authorized policies")
		}
	} else if setup.PolicyAccount != "" || setup.RentTopUpLamports != 0 {
		return errors.New("public setup stage has protected policy/rent funding")
	}
	return nil
}

// InspectDestinationSetup never treats a malformed/foreign existing account
// as missing. It plans one dependency-ordered stage against a coherent read.
func (b *SweepWireBuilder) InspectDestinationSetup(ctx context.Context, plan DepositPlan) (*DestinationSetupPlan, error) {
	if err := validatePlanPublicKeys(plan); err != nil {
		return nil, err
	}
	vault := mustKey(plan.Target.VaultPubkey)
	obligationKey, err := kamino.VanillaObligation(vault, mustKey(plan.Market))
	if err != nil {
		return nil, err
	}
	metadataKey, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return nil, err
	}
	obligation, metadata := obligationKey.String(), metadataKey.String()
	vaultATA, err := spl.AssociatedTokenAddress(vault, mustKey(USDCMint), solana.TokenProgramID)
	ata := vaultATA.String()
	if err != nil || ata != plan.Target.VaultUsdcAta {
		return nil, errors.New("setup custody is not the frozen vault ATA")
	}
	addresses := []string{plan.Reserve, plan.Market, plan.Target.VaultPubkey, ata, metadata, obligation}
	slot, accounts, err := b.read(ctx, 0, addresses, plan.Target.VaultPubkey, ata, metadata, obligation)
	if err != nil {
		return nil, err
	}
	if slot <= 0 || len(accounts) != len(addresses) {
		return nil, errors.New("setup account snapshot incomplete")
	}
	by := make(map[string]*chain.Account, len(addresses))
	for i, a := range accounts {
		by[addresses[i]] = a
	}
	route, err := reserveRoute(by[plan.Reserve], plan.Target.VaultPubkey)
	if err != nil {
		return nil, errors.New("setup reserve evidence invalid")
	}
	if route.Position.Market != plan.Market || route.Position.LiquidityMint != USDCMint || route.Position.LiquidityTokenProgram != splTokenID || by[plan.Market].Owner != kamino.ProgramID || by[plan.Market].Executable {
		return nil, errors.New("setup reserve/market/token identity changed")
	}
	stage := SetupStage("")
	account := ""
	if by[ata] == nil {
		stage = SetupATA
		account = ata
	} else if err := validateSetupAccount(plan, DestinationSetupPlan{Stage: SetupATA, Account: ata, Route: route}, by[ata]); err != nil {
		return nil, err
	}
	// Check existing dependencies even when an earlier dependency is missing.
	if by[metadata] == nil {
		if stage == "" {
			stage = SetupMetadata
			account = metadata
		}
	} else if err := validateSetupAccount(plan, DestinationSetupPlan{Stage: SetupMetadata, Account: metadata, Route: route}, by[metadata]); err != nil {
		return nil, err
	}
	if by[obligation] == nil {
		if stage == "" {
			stage = SetupObligation
			account = obligation
		}
	} else if err := validateSetupAccount(plan, DestinationSetupPlan{Stage: SetupObligation, Account: obligation, Route: route}, by[obligation]); err != nil {
		return nil, err
	}
	if stage == "" && route.Position.ObligationFarmUserState != "" {
		farm := route.Position.ObligationFarmUserState
		farmSlot, farmAccounts, err := b.read(ctx, slot, []string{farm}, farm)
		if err != nil {
			return nil, err
		}
		slot = farmSlot
		if farmAccounts[0] == nil {
			stage = SetupFarm
			account = farm
		} else if err := validateSetupAccount(plan, DestinationSetupPlan{Stage: SetupFarm, Account: farm, Route: route}, farmAccounts[0]); err != nil {
			return nil, err
		}
	}
	if stage == "" {
		return nil, nil
	}
	setup := DestinationSetupPlan{Stage: stage, Account: account, Route: route, ObservedSlot: slot}
	if stage == SetupMetadata || stage == SetupObligation {
		if b.rent == nil {
			return nil, errors.New("setup requires rent reader before signing")
		}
		size := kamino.ObligationSize
		if stage == SetupMetadata {
			size = kamino.UserMetadataSize
		}
		rent, err := b.rent(ctx, size)
		if err != nil {
			return nil, err
		}
		if rent == 0 || rent > maxSetupRentLamports {
			return nil, errors.New("setup rent exceeds source-backed bound")
		}
		var vaultLamports uint64
		if vaultAccount := by[plan.Target.VaultPubkey]; vaultAccount != nil {
			if vaultAccount.Owner != solana.SystemProgramID || vaultAccount.Executable || len(vaultAccount.Data) != 0 {
				return nil, errors.New("setup vault rent account is not plain system custody")
			}
			vaultLamports = vaultAccount.Lamports
		}
		if vaultLamports < rent {
			setup.RentTopUpLamports = rent - vaultLamports
		}
		// The exact constraint matcher chooses the route policy when it already
		// authorizes init, otherwise the explicitly linked setup policy.
		for _, policy := range []string{plan.Target.RoutePolicyAccount, plan.Target.SetupPolicyAccount} {
			if policy == "" {
				continue
			}
			candidate := setup
			candidate.PolicyAccount = policy
			if _, err := b.setupInstructions(ctx, plan, candidate, true); err == nil {
				setup.PolicyAccount = policy
				break
			}
		}
		if setup.PolicyAccount == "" {
			return nil, errors.New("no scoped policy authorizes the missing setup stage")
		}
	}
	instructions, err := b.setupInstructions(ctx, plan, setup, true)
	if err != nil {
		return nil, err
	}
	if _, err = b.transaction(solana.Hash{}, instructions); err != nil {
		return nil, err
	}
	return &setup, nil
}

func validateSetupAccount(plan DepositPlan, setup DestinationSetupPlan, a *chain.Account) error {
	if a == nil || a.Key.String() != setup.Account || a.Executable {
		return errors.New("setup readback address/executable mismatch")
	}
	switch setup.Stage {
	case SetupATA:
		// Identity only: custody residue is the controller's idle-tolerance
		// decision before the pull, as in the TS executor.
		if _, err := usdcTokenAccount(a, plan.Target.VaultPubkey); err != nil {
			return err
		}
	case SetupMetadata:
		if metadata, err := kamino.DecodeUserMetadata(a); err != nil || metadata.Owner.String() != plan.Target.VaultPubkey {
			return errors.New("setup metadata identity invalid")
		}
	case SetupObligation:
		obligation, err := kamino.DecodeObligation(a)
		if err != nil || obligation.LendingMarket.String() != plan.Market || obligation.Owner.String() != plan.Target.VaultPubkey {
			return errors.New("setup obligation identity invalid")
		}
		for _, reserve := range obligation.DepositReserves() {
			if reserve.String() != plan.Reserve {
				return errors.New("setup obligation contains foreign deposit")
			}
		}
		if len(obligation.BorrowReserves()) != 0 {
			return errors.New("setup obligation contains borrow")
		}
	case SetupFarm:
		farm, err := kamino.DecodeFarmUserState(a)
		if err != nil || farm.FarmState.String() != setup.Route.Position.ReserveFarmState || farm.Owner.String() != plan.Target.VaultPubkey || !farm.IsFarmDelegated || farm.Delegatee.String() != setup.Route.Obligation {
			return errors.New("setup farm identity invalid")
		}
	default:
		return errors.New("unknown setup account stage")
	}
	return nil
}

func (b *SweepWireBuilder) setupInstructions(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, wrap bool) ([]fleet.RouteInstruction, error) {
	if err := validateDestinationSetupPlan(plan, setup); err != nil {
		return nil, err
	}
	built, err := fleet.BuildDestinationSetup(fleet.DestinationSetupRequest{Stage: string(setup.Stage), Vault: plan.Target.VaultPubkey, Payer: b.delegate.String(), Target: setup.Route.Position})
	if err != nil {
		return nil, err
	}
	out := []fleet.RouteInstruction{}
	if setup.RentTopUpLamports > 0 {
		if setup.RentTopUpLamports > maxSetupRentLamports || setup.Stage != SetupMetadata && setup.Stage != SetupObligation {
			return nil, errors.New("invalid setup rent transfer")
		}
		out = append(out, fleet.RouteInstructionOf("", spl.SystemTransfer(b.delegate, mustKey(plan.Target.VaultPubkey), setup.RentTopUpLamports)))
	}
	for i, ix := range append(append([]fleet.RouteInstruction{}, built.Public...), built.Protected...) {
		if wrap && i >= len(built.Public) {
			if ix, err = b.wrapWithPolicy(ctx, plan, setup.PolicyAccount, ix); err != nil {
				return nil, err
			}
		}
		out = append(out, ix)
	}
	return out, nil
}
func (b *SweepWireBuilder) BuildDestinationSetup(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, blockhash string, height int64) (BuiltWire, error) {
	instructions, err := b.setupInstructions(ctx, plan, setup, true)
	if err != nil {
		return BuiltWire{}, err
	}
	hash, err := solana.HashFromBase58(blockhash)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.sign(plan, hash, height, instructions)
}
func (b *SweepWireBuilder) ReadbackDestinationSetup(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, minSlot int64) error {
	if err := validateDestinationSetupPlan(plan, setup); err != nil {
		return err
	}
	_, accounts, err := b.read(ctx, max(minSlot, setup.ObservedSlot), []string{setup.Account})
	if err != nil {
		return err
	}
	return validateSetupAccount(plan, setup, accounts[0])
}
