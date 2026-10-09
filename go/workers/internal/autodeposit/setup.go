package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/programs/system"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
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
func metadataKey(vault string) (string, error) {
	key, err := solana.PublicKeyFromBase58(vault)
	if err != nil {
		return "", err
	}
	pda, err := findProgramAddress([][]byte{[]byte("user_meta"), key[:]}, KLendProgramID)
	return base58Key(pda[:]), err
}
func accountDiscriminator(name string) []byte {
	h := sha256.Sum256([]byte("account:" + name))
	return h[:8]
}

func validateDestinationSetupPlan(plan DepositPlan, setup DestinationSetupPlan) error {
	if err := validatePlanPublicKeys(plan); err != nil {
		return err
	}
	if setup.ObservedSlot <= 0 || setup.RentTopUpLamports > maxSetupRentLamports {
		return errors.New("setup observation/rent bound invalid")
	}
	vault := mustKey(plan.Target.VaultPubkey)
	market := mustKey(plan.Market)
	obligation, err := vanillaObligationKey(vault, market)
	if err != nil {
		return err
	}
	if setup.Route.Obligation != obligation || setup.Route.Position.Obligation != obligation || setup.Route.Position.Reserve != plan.Reserve || setup.Route.Position.Market != plan.Market || setup.Route.Position.LiquidityMint != plan.LiquidityMint || setup.Route.Position.LiquidityTokenProgram != splTokenID || setup.Route.Position.VaultLiquidityATA != plan.Target.VaultUsdcAta {
		return errors.New("setup route differs from frozen deposit identity")
	}
	var account string
	switch setup.Stage {
	case SetupATA:
		account, err = deriveVaultATA(vault, mustKey(USDCMint), mustKey(splTokenID))
	case SetupMetadata:
		account, err = metadataKey(plan.Target.VaultPubkey)
	case SetupObligation:
		account = obligation
	case SetupFarm:
		farm, keyErr := solana.PublicKeyFromBase58(setup.Route.Position.ReserveFarmState)
		if keyErr != nil {
			return keyErr
		}
		obl := mustKey(obligation)
		pda, keyErr := findProgramAddress([][]byte{[]byte("user"), farm[:], obl[:]}, farmsProgramID)
		if keyErr != nil {
			return keyErr
		}
		account = base58Key(pda[:])
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
	market := mustKey(plan.Market)
	obligation, err := vanillaObligationKey(vault, market)
	if err != nil {
		return nil, err
	}
	metadata, err := metadataKey(plan.Target.VaultPubkey)
	if err != nil {
		return nil, err
	}
	ata, err := deriveVaultATA(vault, mustKey(USDCMint), mustKey(splTokenID))
	if err != nil || ata != plan.Target.VaultUsdcAta {
		return nil, errors.New("setup custody is not the frozen vault ATA")
	}
	addresses := []string{plan.Reserve, plan.Market, plan.Target.VaultPubkey, ata, metadata, obligation}
	slot, accounts, err := b.read(ctx, addresses, plan.Target.VaultPubkey, ata, metadata, obligation)
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
	reserve := by[plan.Reserve]
	if reserve.Owner.String() != KLendProgramID || reserve.Executable || len(reserve.Data) != reserveDataLength || hexPrefix(reserve.Data[:8]) != reserveDiscriminator {
		return nil, errors.New("setup reserve evidence invalid")
	}
	route := decodeReservePosition(plan.Reserve, reserve.Data)
	if route.Position.Market != plan.Market || route.Position.LiquidityMint != USDCMint || route.Position.LiquidityTokenProgram != splTokenID || by[plan.Market].Owner.String() != KLendProgramID || by[plan.Market].Executable {
		return nil, errors.New("setup reserve/market/token identity changed")
	}
	route.Obligation = obligation
	route.Position.Obligation = obligation
	route.Position.VaultLiquidityATA = ata
	if farm := route.Position.ReserveFarmState; farm != "" {
		farmKey := mustKey(farm)
		obl := mustKey(obligation)
		user, err := findProgramAddress([][]byte{[]byte("user"), farmKey[:], obl[:]}, farmsProgramID)
		if err != nil {
			return nil, err
		}
		route.Position.ObligationFarmUserState = base58Key(user[:])
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
		farmSlot, farmAccounts, err := b.read(ctx, []string{farm}, farm)
		if err != nil {
			return nil, err
		}
		if farmSlot < slot || len(farmAccounts) != 1 {
			return nil, errors.New("setup farm observation stale")
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
		size := obligationDataLength
		if stage == SetupMetadata {
			size = 1032
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
	if _, err = b.transaction([32]byte{}, instructions); err != nil {
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
		if err := validateVaultUSDCATA(a, setup.Account, plan.Target.VaultPubkey); err != nil {
			return err
		}
		// Identity only: custody residue is the controller's idle-tolerance
		// decision before the pull, as in the TS executor.
		if len(a.Data) != splTokenAccountLength || a.Data[108] != 1 {
			return errors.New("setup custody token account is uninitialized")
		}
	case SetupMetadata:
		if a.Owner.String() != KLendProgramID || len(a.Data) != 1032 || !bytes.Equal(a.Data[:8], accountDiscriminator("UserMetadata")) || base58Key(a.Data[80:112]) != plan.Target.VaultPubkey {
			return errors.New("setup metadata identity invalid")
		}
	case SetupObligation:
		if a.Owner.String() != KLendProgramID || len(a.Data) != obligationDataLength || !bytes.Equal(a.Data[:8], obligationDiscriminator[:]) || base58Key(a.Data[32:64]) != plan.Market || base58Key(a.Data[64:96]) != plan.Target.VaultPubkey {
			return errors.New("setup obligation identity invalid")
		}
		for i := 0; i < obligationDepositCount; i++ {
			offset := obligationDepositsOffset + i*obligationDepositStride
			reserve := base58Key(a.Data[offset : offset+32])
			if reserve != systemProgramZero && reserve != plan.Reserve {
				return errors.New("setup obligation contains foreign deposit")
			}
		}
		for i := 0; i < obligationBorrowCount; i++ {
			offset := obligationBorrowsOffset + i*obligationBorrowStride
			if base58Key(a.Data[offset:offset+32]) != systemProgramZero {
				return errors.New("setup obligation contains borrow")
			}
		}
	case SetupFarm:
		// Pinned Farms Codama UserState layout: farm16, owner48, delegatee480.
		if a.Owner.String() != farmsProgramID || len(a.Data) != 920 || !bytes.Equal(a.Data[:8], []byte{72, 177, 85, 249, 76, 167, 186, 126}) || base58Key(a.Data[16:48]) != setup.Route.Position.ReserveFarmState || base58Key(a.Data[48:80]) != plan.Target.VaultPubkey || a.Data[80] != 1 || base58Key(a.Data[480:512]) != setup.Route.Obligation {
			return errors.New("setup farm identity invalid")
		}
	default:
		return errors.New("unknown setup account stage")
	}
	return nil
}

func (b *SweepWireBuilder) setupInstructions(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, wrap bool) ([]compiledInstruction, error) {
	if err := validateDestinationSetupPlan(plan, setup); err != nil {
		return nil, err
	}
	built, err := fleet.BuildDestinationSetup(fleet.DestinationSetupRequest{Stage: string(setup.Stage), Vault: plan.Target.VaultPubkey, Payer: b.delegate.String(), Target: setup.Route.Position})
	if err != nil {
		return nil, err
	}
	out := []compiledInstruction{}
	if setup.RentTopUpLamports > 0 {
		if setup.RentTopUpLamports > maxSetupRentLamports || setup.Stage != SetupMetadata && setup.Stage != SetupObligation {
			return nil, errors.New("invalid setup rent transfer")
		}
		ix := system.NewTransferInstruction(setup.RentTopUpLamports, b.delegate, mustKey(plan.Target.VaultPubkey)).Build()
		data, err := ix.Data()
		if err != nil {
			return nil, err
		}
		compiled := compiledInstruction{program: system.ProgramID, data: data}
		for _, a := range ix.Accounts() {
			compiled.accounts = append(compiled.accounts, accountMeta{a.PublicKey, a.IsSigner, a.IsWritable})
		}
		out = append(out, compiled)
	}
	for i, ix := range append(append([]fleet.RouteInstruction{}, built.Public...), built.Protected...) {
		compiled := compiledInstruction{program: mustKey(ix.Program), data: ix.Data}
		for _, a := range ix.Accounts {
			compiled.accounts = append(compiled.accounts, accountMeta{mustKey(a.Address), a.Signer, a.Writable})
		}
		if wrap && i >= len(built.Public) {
			compiled, err = b.wrapWithPolicy(ctx, plan, setup.PolicyAccount, compiled)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, compiled)
	}
	return out, nil
}
func (b *SweepWireBuilder) BuildDestinationSetup(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, blockhash string, height int64) (BuiltWire, error) {
	instructions, err := b.setupInstructions(ctx, plan, setup, true)
	if err != nil {
		return BuiltWire{}, err
	}
	hash, err := blockhash32(blockhash)
	if err != nil {
		return BuiltWire{}, err
	}
	return b.sign(plan, hash, height, instructions)
}
func (b *SweepWireBuilder) ReadbackDestinationSetup(ctx context.Context, plan DepositPlan, setup DestinationSetupPlan, minSlot int64) error {
	if err := validateDestinationSetupPlan(plan, setup); err != nil {
		return err
	}
	slot, accounts, err := b.read(ctx, []string{setup.Account})
	if err != nil {
		return err
	}
	if slot < minSlot || slot < setup.ObservedSlot || len(accounts) != 1 {
		return errors.New("setup readback is older than confirmed transaction")
	}
	return validateSetupAccount(plan, setup, accounts[0])
}
