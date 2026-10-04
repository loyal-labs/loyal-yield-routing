package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"github.com/gagliardetto/solana-go"
)

// CanonicalSubscriptionPolicyRequest invokes the source action SDK's exact
// deployed policy matrix. PolicyDataHex, when present, additionally proves the
// current account's complete security properties and matrix, allowing only
// its transaction counters to have advanced since creation.
type CanonicalSubscriptionPolicyRequest struct {
	Settings           string `json:"settings"`
	RootAuthority      string `json:"rootAuthority"`
	Payer              string `json:"payer"`
	DelegatedSigner    string `json:"delegatedSigner"`
	PolicySeed         uint64 `json:"policySeed"`
	Wallet             string `json:"wallet"`
	Vault              string `json:"vault"`
	MaxAmountPerPeriod uint64 `json:"maxAmountPerPeriod"`
	PolicyDataHex      string `json:"policyDataHex"`
}

func (p *KLendProxy) BuildCanonicalSubscriptionPolicy(ctx context.Context, r CanonicalSubscriptionPolicyRequest) (RouteInstruction, error) {
	route, err := p.invoke(ctx, proxyRequest{1, "buildCanonicalSubscriptionPolicy", r})
	if err != nil {
		return RouteInstruction{}, err
	}
	if len(route.Public) != 0 || len(route.Protected) != 1 {
		return RouteInstruction{}, fmt.Errorf("canonical policy helper returned extra instructions")
	}
	ix := route.Protected[0]
	settings, err := solana.PublicKeyFromBase58(r.Settings)
	if err != nil {
		return ix, err
	}
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], r.PolicySeed)
	policy, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settings[:], seed[:]}, solana.MustPublicKeyFromBase58(SquadsProgram))
	if err != nil {
		return ix, err
	}
	accounts := []InstructionAccount{{Address: r.Settings, Writable: true}, {Address: r.Payer, Signer: true, Writable: true}, {Address: "11111111111111111111111111111111"}, {Address: SquadsProgram}, {Address: r.RootAuthority, Signer: true}, {Address: policy.String(), Writable: true}}
	if ix.Program != SquadsProgram || ix.Step != "canonical_subscription_policy_create" || len(ix.Accounts) != len(accounts) {
		return ix, fmt.Errorf("canonical policy helper creator identity invalid")
	}
	for i := range accounts {
		if ix.Accounts[i] != accounts[i] {
			return ix, fmt.Errorf("canonical policy helper creator account %d differs", i)
		}
	}
	disc := sha256.Sum256([]byte("global:execute_settings_transaction_sync"))
	if len(ix.Data) < 24 || !bytes.Equal(ix.Data[:8], disc[:8]) || ix.Data[8] != 1 || binary.LittleEndian.Uint32(ix.Data[9:13]) != 1 || ix.Data[13] != 7 || binary.LittleEndian.Uint64(ix.Data[14:22]) != r.PolicySeed || ix.Data[22] != 3 || ix.Data[23] != 1 {
		return ix, fmt.Errorf("canonical policy helper did not build one deployed index1 PolicyCreate")
	}
	return ix, nil
}

// DestinationSetupRequest builds one account-creation stage. It cannot move
// wallet liquidity or create/update policy permissions.
type DestinationSetupRequest struct {
	Stage  string                 `json:"stage"`
	Vault  string                 `json:"vault"`
	Payer  string                 `json:"payer"`
	Target KaminoPositionAccounts `json:"target"`
}

func (p *KLendProxy) BuildDestinationSetup(ctx context.Context, r DestinationSetupRequest) (KaminoSameMintRoute, error) {
	if r.Target.ObligationDepositReserves == nil {
		r.Target.ObligationDepositReserves = []string{}
	}
	if r.Target.ObligationBorrowReserves == nil {
		r.Target.ObligationBorrowReserves = []string{}
	}
	route, err := p.invoke(ctx, proxyRequest{1, "buildDestinationSetup", r})
	if err != nil {
		return KaminoSameMintRoute{}, err
	}
	if err = validateDestinationSetup(route, r); err != nil {
		return KaminoSameMintRoute{}, err
	}
	return route, nil
}

// Independently bind every official builder account and privilege, including
// vault rent payer and metadata/vanilla-obligation PDA seeds.
func validateDestinationSetup(route KaminoSameMintRoute, r DestinationSetupRequest) error {
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return err
	}
	market, err := solana.PublicKeyFromBase58(r.Target.Market)
	if err != nil {
		return err
	}
	program := solana.MustPublicKeyFromBase58(KLendProgram)
	metadata, _, err := solana.FindProgramAddress([][]byte{[]byte("user_meta"), vault[:]}, program)
	if err != nil {
		return err
	}
	var zero solana.PublicKey
	obligation, _, err := solana.FindProgramAddress([][]byte{{0}, {0}, vault[:], market[:], zero[:], zero[:]}, program)
	if err != nil {
		return err
	}
	authority, _, err := solana.FindProgramAddress([][]byte{[]byte("lma"), market[:]}, program)
	if err != nil {
		return err
	}
	if r.Target.Obligation != obligation.String() || r.Target.MarketAuthority != authority.String() {
		return fmt.Errorf("setup target PDA identity mismatch")
	}
	const system = "11111111111111111111111111111111"
	const rent = "SysvarRent111111111111111111111111111111111"
	const ataProgram = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	a := func(address string, signer, writable bool) InstructionAccount {
		return InstructionAccount{address, signer, writable}
	}
	var accounts []InstructionAccount
	var data []byte
	expectedProgram := KLendProgram
	protected := false
	discriminator := func(name string) []byte {
		h := sha256.Sum256([]byte("global:" + name))
		return append([]byte(nil), h[:8]...)
	}
	switch r.Stage {
	case "ata":
		ata, err := deriveATA(r.Vault, r.Target.LiquidityMint, r.Target.LiquidityTokenProgram)
		if err != nil || ata != r.Target.VaultLiquidityATA {
			return fmt.Errorf("setup custody ATA identity mismatch")
		}
		accounts = []InstructionAccount{a(r.Payer, true, true), a(ata, false, true), a(r.Vault, false, false), a(r.Target.LiquidityMint, false, false), a(system, false, false), a(r.Target.LiquidityTokenProgram, false, false)}
		data = []byte{1}
		expectedProgram = ataProgram
	case "metadata":
		accounts = []InstructionAccount{a(r.Vault, true, false), a(r.Vault, true, true), a(metadata.String(), false, true), a(KLendProgram, false, false), a(rent, false, false), a(system, false, false)}
		data = append(discriminator("init_user_metadata"), make([]byte, 32)...)
		protected = true
	case "obligation":
		accounts = []InstructionAccount{a(r.Vault, true, false), a(r.Vault, true, true), a(obligation.String(), false, true), a(r.Target.Market, false, false), a(system, false, false), a(system, false, false), a(metadata.String(), false, false), a(rent, false, false), a(system, false, false)}
		data = append(discriminator("init_obligation"), 0, 0)
		protected = true
	case "farm":
		farm, err := solana.PublicKeyFromBase58(r.Target.ReserveFarmState)
		if err != nil {
			return err
		}
		farmUser, _, err := solana.FindProgramAddress([][]byte{[]byte("user"), farm[:], obligation[:]}, solana.MustPublicKeyFromBase58(farmsProgram))
		if err != nil || farmUser.String() != r.Target.ObligationFarmUserState {
			return fmt.Errorf("setup farm PDA identity mismatch")
		}
		accounts = []InstructionAccount{a(r.Payer, true, true), a(r.Vault, false, false), a(obligation.String(), false, true), a(authority.String(), false, false), a(r.Target.Reserve, false, true), a(farm.String(), false, true), a(farmUser.String(), false, true), a(r.Target.Market, false, false), a(farmsProgram, false, false), a(rent, false, false), a(system, false, false)}
		data = append(discriminator("init_obligation_farms_for_reserve"), 0)
	default:
		return fmt.Errorf("unknown destination setup stage")
	}
	var instructions []RouteInstruction
	if protected {
		if len(route.Public) != 0 {
			return fmt.Errorf("protected setup has public instructions")
		}
		instructions = route.Protected
	} else {
		if len(route.Protected) != 0 {
			return fmt.Errorf("public setup has protected instructions")
		}
		instructions = route.Public
	}
	if len(instructions) != 1 {
		return fmt.Errorf("setup response must contain exactly one stage")
	}
	ix := instructions[0]
	if ix.Step != "kamino_setup_"+r.Stage || ix.Program != expectedProgram || !bytes.Equal(ix.Data, data) || len(ix.Accounts) != len(accounts) {
		return fmt.Errorf("setup builder wire drift")
	}
	for i, account := range accounts {
		if ix.Accounts[i] != account {
			return fmt.Errorf("setup builder account %d drift", i)
		}
	}
	return nil
}
