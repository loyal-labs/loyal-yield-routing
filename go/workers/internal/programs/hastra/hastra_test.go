package hastra

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// mainnetInstruction is one Hastra instruction as a mainnet transaction sent
// it (testdata/mainnet-instructions.json): a direct stake deposit
// (NH5Leb…), a direct redeem (4wRzry…) and a mint deposit (3G3Qoc…). The
// flags are the transaction's, and each signer also paid the fee.
type mainnetInstruction struct {
	Name, Signature, Program, Data string
	Accounts                       []struct {
		Pubkey           solana.PublicKey
		Signer, Writable bool
	}
}

func mainnetInstructions(t *testing.T) map[string]mainnetInstruction {
	t.Helper()
	raw, err := os.ReadFile("testdata/mainnet-instructions.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []mainnetInstruction
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]mainnetInstruction{}
	for _, ix := range list {
		out[ix.Name] = ix
	}
	return out
}

// ours rebuilds a mainnet instruction from its signer side and amount.
func ours(name string, keys []solana.PublicKey, amount uint64) *solana.GenericInstruction {
	switch name {
	case "stake deposit":
		return StakeDeposit(StakeAccounts{Signer: keys[7], UserWYLDS: keys[8], UserAUTO: keys[9]}, amount)
	case "stake redeem":
		return StakeRedeem(StakeAccounts{Signer: keys[4], UserWYLDS: keys[6], UserAUTO: keys[7]}, amount)
	default:
		return MintDeposit(MintAccounts{Signer: keys[5], UserUSDC: keys[6], UserWYLDS: keys[7]}, amount)
	}
}

// The builders send exactly what mainnet sent: the same program, data and
// accounts in order (so the PDAs derived from the source's seeds and the pool
// constants are the deployed ones), the same signer, and the same writable
// accounts but the fee-paying signer.
func TestBuildersMatchMainnetInstructions(t *testing.T) {
	for name, real := range mainnetInstructions(t) {
		data, err := hex.DecodeString(real.Data)
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]solana.PublicKey, len(real.Accounts))
		for i, a := range real.Accounts {
			keys[i] = a.Pubkey
		}
		ix := ours(name, keys, binary.LittleEndian.Uint64(data[AmountOffset:]))
		built, _ := ix.Data()
		if ix.ProgramID().String() != real.Program || hex.EncodeToString(built) != real.Data {
			t.Fatalf("%s: program %s data %x, mainnet sent %s %s", name, ix.ProgramID(), built, real.Program, real.Data)
		}
		metas := ix.Accounts()
		if len(metas) != len(real.Accounts) {
			t.Fatalf("%s: %d accounts, mainnet sent %d", name, len(metas), len(real.Accounts))
		}
		for i, a := range real.Accounts {
			m := metas[i]
			if m.PublicKey != a.Pubkey || m.IsSigner != a.Signer || !a.Signer && m.IsWritable != a.Writable {
				t.Fatalf("%s account %d: %s signer %t writable %t, mainnet sent %+v", name, i, m.PublicKey, m.IsSigner, m.IsWritable, a)
			}
		}
	}
}

// Each constraint admits its mainnet instruction at any amount, and refuses
// it with the signer's source or destination account, or the signer, swapped
// for a foreign one, or with any program account swapped.
func TestAllowedAdmitsMainnetInstructionsAndRefusesForeignAccounts(t *testing.T) {
	foreign := solana.MustPublicKeyFromBase58("5ZWxEY76gtz46trW7wqCPVqJoJVbQXd7WZ5Bzif9EArH")
	for name, real := range mainnetInstructions(t) {
		data, _ := hex.DecodeString(real.Data)
		keys := make([]solana.PublicKey, len(real.Accounts))
		for i, a := range real.Accounts {
			keys[i] = a.Pubkey
		}
		var allowed squads.InstructionConstraintView
		var signer, source, destination int
		switch name {
		case "stake deposit":
			signer, source, destination = 7, 8, 9
			allowed = StakeDepositAllowed(PinStake(StakeAccounts{Signer: keys[7], UserWYLDS: keys[8], UserAUTO: keys[9]}))
		case "stake redeem":
			signer, source, destination = 4, 7, 6
			allowed = StakeRedeemAllowed(PinStake(StakeAccounts{Signer: keys[4], UserWYLDS: keys[6], UserAUTO: keys[7]}))
		default:
			signer, source, destination = 5, 6, 7
			allowed = MintDepositAllowed(PinMint(MintAccounts{Signer: keys[5], UserUSDC: keys[6], UserWYLDS: keys[7]}))
		}
		if len(allowed.AccountConstraints) != len(keys) {
			t.Fatalf("%s: %d of %d slots pinned", name, len(allowed.AccountConstraints), len(keys))
		}
		admits := func(keys []solana.PublicKey, data []byte) bool {
			metas := make([]solana.AccountMeta, len(keys))
			for i, key := range keys {
				metas[i] = solana.AccountMeta{PublicKey: key}
			}
			return squads.Admits(allowed, squads.Instruction{ProgramID: solana.MustPublicKeyFromBase58(real.Program), Accounts: metas, Data: data}, nil)
		}
		if !admits(keys, data) {
			t.Fatalf("%s: the mainnet instruction is refused", name)
		}
		other := append([]byte(nil), data...)
		binary.LittleEndian.PutUint64(other[AmountOffset:], 1<<62)
		if !admits(keys, other) {
			t.Fatalf("%s: another amount is refused", name)
		}
		for i := range keys {
			swapped := append([]solana.PublicKey(nil), keys...)
			swapped[i] = foreign
			if admits(swapped, data) {
				role := map[int]string{signer: "signer", source: "source", destination: "destination"}[i]
				t.Fatalf("%s: account %d (%s) swapped for a foreign one is admitted", name, i, role)
			}
		}
		wrong := append([]byte(nil), data...)
		copy(wrong, RedeemDiscriminator[:])
		if name == "stake redeem" {
			copy(wrong, DepositDiscriminator[:])
		}
		if admits(keys, wrong) {
			t.Fatalf("%s: another instruction's discriminator is admitted", name)
		}
	}
}

// The quotes round as the program does: a deposit and a redeem floor, and
// AUTOToRedeem is the least AUTO whose redeem covers the wYLDS asked for.
func TestStakePriceQuotesRoundAsTheProgram(t *testing.T) {
	scale := uint64(1_000_000_000_000_000_000)
	p := StakePrice{Price: new(big.Int).SetUint64(1_023_948_000_000_000_000), Scale: scale, Timestamp: 100, MaxStaleness: 3600}
	if got, err := p.AUTOForWYLDS(1_000_000); err != nil || got != 976_612 { // 1e6·1e18/1.023948e18 = 976612.4…
		t.Fatalf("deposit 1 wYLDS: %d %v", got, err)
	}
	if got, err := p.WYLDSForAUTO(976_612); err != nil || got != 999_999 { // the round trip loses the floor
		t.Fatalf("redeem: %d %v", got, err)
	}
	for _, want := range []uint64{1, 2, 999_999, 1_000_000, 22_000_000, 1 << 40} {
		shares, err := p.AUTOToRedeem(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.WYLDSForAUTO(shares)
		if err != nil || got < want {
			t.Fatalf("redeeming %d AUTO pays %d < %d (%v)", shares, got, want, err)
		}
		if shares > 1 {
			if less, err := p.WYLDSForAUTO(shares - 1); err == nil && less >= want {
				t.Fatalf("%d AUTO already pays %d >= %d", shares-1, less, want)
			}
		}
	}
	if _, err := p.AUTOForWYLDS(1); err == nil {
		t.Fatal("a deposit that mints nothing was quoted")
	}
	if !p.Fresh(3700) || p.Fresh(3701) {
		t.Fatal("staleness is not the program's: now - timestamp <= max")
	}
	if _, err := (StakePrice{Price: big.NewInt(0), Scale: scale}).WYLDSForAUTO(1); err == nil {
		t.Fatal("an uninitialized price was quoted")
	}
}

// The price decodes from its Anchor layout and nothing else.
func TestDecodeStakePrice(t *testing.T) {
	data := make([]byte, stakePriceConfigSize)
	copy(data, StakePriceConfigDiscriminator[:])
	binary.LittleEndian.PutUint64(data[136:], 1_023_948_000_000_000_000)
	binary.LittleEndian.PutUint64(data[152:], 1_000_000_000_000_000_000)
	binary.LittleEndian.PutUint64(data[160:], 1_791_700_000)
	binary.LittleEndian.PutUint64(data[168:], 3600)
	account := &chain.Account{Key: StakePriceConfig, Owner: StakeProgramID, Lamports: 1, Data: data}
	p, err := DecodeStakePrice(account)
	if err != nil || p.Price.Uint64() != 1_023_948_000_000_000_000 || p.Scale != 1e18 || p.Timestamp != 1_791_700_000 || p.MaxStaleness != 3600 {
		t.Fatalf("%+v %v", p, err)
	}
	account.Owner = MintProgramID
	if _, err := DecodeStakePrice(account); err == nil {
		t.Fatal("a foreign owner decoded")
	}
}
