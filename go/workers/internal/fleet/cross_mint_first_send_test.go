package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	solana "github.com/solana-foundation/solana-go/v2"
)

// These deterministic local keys and synthetic instruction fixtures test exact
// parsing/signature boundaries. They are not a policy readback or SVM proof.
func crossMintFirstSendWireFixture(t *testing.T) (CrossMintFirstSendRequest, CrossMintPolicyBindings) {
	t.Helper()
	q, plan, _ := crossMintPreparationFixture(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	signer := encodeBase58(key.Public().(ed25519.PublicKey))
	plan.Bindings.DelegatedSigner = signer
	body, _ := jupiterBuildForVault(t, q.Movement.VaultPubkey, 999, 999, 1)
	swap, err := validateJupiterEnvelope(body, plan, q.Movement.VaultPubkey, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapSquadsPolicy(plan.Bindings.Swap.PolicyAccount, signer, 0, []uint8{0}, []RouteInstruction{swap.Swap})
	if err != nil {
		t.Fatal(err)
	}
	addresses := requiredLookupTableAddresses([]RouteInstruction{wrapped})
	table := LookupTable{ID: 1, FamilyID: 1, Address: testPubkey(244), Addresses: addresses, Active: true, Generation: 0, LastVerifiedSlot: 1000}
	prep, _, err := compileV0Transaction(signer, testPubkey(245), append(computeBudgetInstructions(200000, 0), wrapped), []LookupTable{table}, 5000, 200000)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := solana.TransactionFromBytes(prep.UnsignedWire)
	if err != nil {
		t.Fatal(err)
	}
	copy(tx.Signatures[0][:], ed25519.Sign(key, prep.Message))
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	wireHash := sha256.Sum256(wire)
	return CrossMintFirstSendRequest{Movement: q.Movement, Leg: "swap", Purpose: "optimize_yield", PolicyAccount: plan.Bindings.Swap.PolicyAccount, MinimumSlot: 1000, SignedWire: wire, ExpectedWireSHA256: hex.EncodeToString(wireHash[:]), ExpectedMessageSHA256: prep.MessageSHA256, Signature: tx.Signatures[0].String(), RecentBlockhash: tx.Message.RecentBlockhash.String(), LastValidBlockHeight: 2000, SelectedALTs: []ExecutionALT{{TableID: 1, FamilyID: 1, Generation: 0, Address: table.Address, Addresses: addresses}}}, plan.Bindings
}

func TestCrossMintFirstSendSDKKeepsOriginalWireAndSwapEnvelope(t *testing.T) {
	input, binding := crossMintFirstSendWireFixture(t)
	before := bytes.Clone(input.SignedWire)
	_, message, actual, tables, err := decodeCrossMintFirstSendWire(input, binding.DelegatedSigner)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != 3 || len(tables) != 1 || tables[0].Generation != 0 {
		t.Fatal("registered generation zero or exact outer topology lost")
	}
	swap, index, err := decodeCrossMintSignedSwap(actual[2], binding, binding.DelegatedSigner)
	if err != nil || index != 0 || swap.Program != jupiterProgram {
		t.Fatalf("exact compact swap decode: index=%d err=%v", index, err)
	}
	wrapped, err := wrapSquadsPolicy(binding.Swap.PolicyAccount, binding.DelegatedSigner, 0, []uint8{index}, []RouteInstruction{swap})
	if err != nil {
		t.Fatal(err)
	}
	compiled, _, err := compileV0Transaction(binding.DelegatedSigner, input.RecentBlockhash, append(append([]RouteInstruction{}, actual[:2]...), wrapped), tables, 5000, 200000)
	if err != nil || !bytes.Equal(compiled.Message, message) || !bytes.Equal(before, input.SignedWire) {
		t.Fatalf("proof rebuilt or changed immutable old wire: %v", err)
	}
	actual[2].Data = bytes.Clone(actual[2].Data)
	actual[2].Data[17] = 1
	if _, _, err := decodeCrossMintSignedSwap(actual[2], binding, binding.DelegatedSigner); err != nil {
		// The canonical compact wrapper has another legal index; dialect validation
		// against its protected instruction must refuse a substituted selection.
	} else {
		_, plan, bank := crossMintPreparationFixture(t)
		input.Movement.CustodyAmountRaw = 999
		if err := validateCrossMintSignedSwap(swap, 1, plan, input.Movement, bank, 50, 50); err == nil {
			t.Fatal("route-v2 swap authorized at shared-v2 constraint index")
		}
	}
}

func TestCrossMintFirstSendRejectsMalformedJournalBeforeRPC(t *testing.T) {
	for name, mutate := range map[string]func(*CrossMintFirstSendRequest){
		"signature": func(r *CrossMintFirstSendRequest) {
			r.SignedWire[1] ^= 1
			h := sha256.Sum256(r.SignedWire)
			r.ExpectedWireSHA256 = hex.EncodeToString(h[:])
		},
		"trailing bytes": func(r *CrossMintFirstSendRequest) {
			r.SignedWire = append(r.SignedWire, 0)
			h := sha256.Sum256(r.SignedWire)
			r.ExpectedWireSHA256 = hex.EncodeToString(h[:])
		},
		"message hash":      func(r *CrossMintFirstSendRequest) { r.ExpectedMessageSHA256 = r.ExpectedWireSHA256 },
		"wire hash":         func(r *CrossMintFirstSendRequest) { r.ExpectedWireSHA256 = r.ExpectedMessageSHA256 },
		"blockhash":         func(r *CrossMintFirstSendRequest) { r.RecentBlockhash = testPubkey(246) },
		"unregistered ALT":  func(r *CrossMintFirstSendRequest) { r.SelectedALTs[0].TableID = 0 },
		"missing ALT":       func(r *CrossMintFirstSendRequest) { r.SelectedALTs = nil },
		"ALT member prefix": func(r *CrossMintFirstSendRequest) { r.SelectedALTs[0].Addresses = r.SelectedALTs[0].Addresses[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			input, b := crossMintFirstSendWireFixture(t)
			mutate(&input)
			if _, _, _, _, err := decodeCrossMintFirstSendWire(input, b.DelegatedSigner); err == nil {
				t.Fatal("malformed journal wire reached finalized chain readers")
			}
		})
	}
	if err := (*Revalidator)(nil).ValidateCrossMintFirstSend(context.Background(), CrossMintFirstSendRequest{}); err == nil {
		t.Fatal("missing concrete chain dependency accepted")
	}
}

// This runs the concrete validator and the retained Rust instruction builder
// against explicitly mocked finalized account and simulation replies. It proves
// orchestration and exact-wire use, not chain execution or provider correctness.
func TestRealKLendCrossMintFirstSendRechecksExactOldWithdrawal(t *testing.T) {
	q, plan, bank := crossMintPreparationFixture(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	signer := encodeBase58(key.Public().(ed25519.PublicKey))
	plan.Bindings.DelegatedSigner = signer
	plan.Bindings.Swap.ManifestFingerprint = fingerprintCrossMintManifest(plan.Bindings, []string{USDCMint, USDTMint, USDSMint}, tokenProgram)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(q.Movement.ExecutionPlan, &fields); err != nil {
		t.Fatal(err)
	}
	fields["policy_bindings"], _ = json.Marshal(plan.Bindings)
	q.Movement.ExecutionPlan, _ = json.Marshal(fields)
	r := &Revalidator{signer: signer, slotDuration: 400 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	route, err := BuildCrossMintLegs(KaminoSameMintRouteRequest{Vault: q.Movement.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position, WithdrawCollateralAmount: 999, DepositLiquidityAmount: 999})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := BuildIdleDeposit(KaminoIdleDepositRequest{Vault: q.Movement.VaultPubkey, Target: bank.source.Position, DepositLiquidityAmount: 999})
	if err != nil {
		t.Fatal(err)
	}
	for i, protected := range [][]RouteInstruction{{route.Protected[0], recovery.Protected[0]}, {route.Protected[1]}} {
		data, err := connectedEarnPolicyData(plan.Bindings.Settings, signer, protected, []KaminoPositionAccounts{bank.source.Position, bank.target.Position})
		if err != nil {
			t.Fatal(err)
		}
		seed := uint64(i + 1)
		address, bump, _ := derivePolicyAccount(plan.Bindings.Settings, seed)
		binary.LittleEndian.PutUint64(data[40:48], seed)
		data[48] = bump
		bank.accounts[address] = fixtureAccount(address, squads.ProgramID.String(), 1_000_000, data)
	}
	bank.accounts[plan.Bindings.Swap.PolicyAccount] = fixtureAccount(plan.Bindings.Swap.PolicyAccount, squads.ProgramID.String(), 1_000_000, connectedSwapPolicy(t, plan.Bindings, 3))
	var cert CrossMintPreflightCertificate
	if err := json.Unmarshal(q.Movement.PreflightCertification, &cert); err != nil {
		t.Fatal(err)
	}
	for _, observation := range []*CrossMintCertificatePolicy{&cert.FinalizedPolicyReadbacks.Withdraw, &cert.FinalizedPolicyReadbacks.Deposit, &cert.FinalizedPolicyReadbacks.Swap.CrossMintCertificatePolicy} {
		hash := sha256.Sum256(bank.accounts[observation.PolicyAccount].Data)
		observation.DataSHA256, observation.ContextSlot = hex.EncodeToString(hash[:]), bank.slot
	}
	cert.FinalizedPolicyReadbacks.Swap.ManifestFingerprint = plan.Bindings.Swap.ManifestFingerprint
	q.Movement.PreflightCertification, _ = json.Marshal(cert)
	instructions, _, _, policy, err := r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
	if err != nil {
		t.Fatal(err)
	}
	addresses := requiredLookupTableAddresses(instructions)
	table := LookupTable{ID: 1, FamilyID: 1, Generation: 0, Address: testPubkey(244), Addresses: addresses, Active: true, LastVerifiedSlot: bank.slot}
	data := make([]byte, 56+32*len(addresses))
	binary.LittleEndian.PutUint32(data, 1)
	binary.LittleEndian.PutUint64(data[4:12], math.MaxUint64)
	binary.LittleEndian.PutUint64(data[12:20], 900)
	for i, address := range addresses {
		fixtureKey(t, data, 56+32*i, address)
	}
	bank.accounts[table.Address] = fixtureAccount(table.Address, altProgram, 1_000_000, data)
	prep, _, err := compileV0Transaction(signer, testPubkey(245), append(computeBudgetInstructions(200_000, 0), instructions...), []LookupTable{table}, 5000, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := solana.TransactionFromBytes(prep.UnsignedWire)
	copy(tx.Signatures[0][:], ed25519.Sign(key, prep.Message))
	wire, _ := tx.MarshalBinary()
	wireHash := sha256.Sum256(wire)
	input := CrossMintFirstSendRequest{Movement: q.Movement, Leg: "withdraw", Purpose: "optimize_yield", PolicyAccount: policy, MinimumSlot: bank.slot, SignedWire: wire, ExpectedWireSHA256: hex.EncodeToString(wireHash[:]), ExpectedMessageSHA256: prep.MessageSHA256, Signature: tx.Signatures[0].String(), RecentBlockhash: tx.Message.RecentBlockhash.String(), LastValidBlockHeight: 2000, SelectedALTs: []ExecutionALT{{TableID: 1, FamilyID: 1, Generation: 0, Address: table.Address, Addresses: addresses}}}
	simulations := 0
	r.rpc = crossMintPrepareRPC(t, func(method string, params []json.RawMessage) any {
		switch method {
		case "getMultipleAccounts":
			var names []string
			var options struct{ Commitment string }
			_ = json.Unmarshal(params[0], &names)
			_ = json.Unmarshal(params[1], &options)
			if options.Commitment != "finalized" {
				t.Fatal("first-send account read downgraded commitment")
			}
			var values []any
			for _, name := range names {
				a, ok := bank.accounts[name]
				if !ok {
					t.Fatalf("unregistered mocked account: %s", name)
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			return map[string]any{"context": map[string]any{"slot": bank.slot}, "value": values}
		case "getEpochInfo":
			return map[string]any{"absoluteSlot": bank.slot, "blockHeight": 1900}
		case "simulateTransaction":
			var encoded string
			var options struct {
				Commitment             string
				ReplaceRecentBlockhash bool
			}
			_ = json.Unmarshal(params[0], &encoded)
			_ = json.Unmarshal(params[1], &options)
			if encoded != base64.StdEncoding.EncodeToString(wire) || options.Commitment != "finalized" || options.ReplaceRecentBlockhash {
				t.Fatal("first-send simulation altered original wire or commitment")
			}
			simulations++
			return map[string]any{"context": map[string]any{"slot": bank.slot}, "value": map[string]any{"err": nil, "unitsConsumed": 1000}}
		default:
			t.Fatalf("first-send attempted unexpected RPC: %s", method)
			return nil
		}
	})
	if err := r.ValidateCrossMintFirstSend(ctx, input); err != nil || simulations != 1 {
		t.Fatalf("exact unchanged first-send proof: simulations=%d err=%v", simulations, err)
	}
	recipient := bank.source.Position.VaultLiquidityATA
	account := bank.accounts[recipient]
	binary.LittleEndian.PutUint64(account.Data[64:72], 1)
	bank.accounts[recipient] = account
	if err := r.ValidateCrossMintFirstSend(ctx, input); err == nil || simulations != 1 {
		t.Fatalf("recipient balance drift reached old-wire simulation: count=%d err=%v", simulations, err)
	}
	binary.LittleEndian.PutUint64(account.Data[64:72], 0)
	bank.accounts[recipient] = account
	// A fresh policy envelope change must block the exact stored wire before
	// simulation. No replacement policy/quote/message is synthesized.
	a := bank.accounts[policy]
	a.Owner = solana.SystemProgramID
	bank.accounts[policy] = a
	if err := r.ValidateCrossMintFirstSend(ctx, input); err == nil || simulations != 1 {
		t.Fatalf("changed current policy reached old-wire simulation: count=%d err=%v", simulations, err)
	}
}
