package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"
)

// CrossMintFirstSendRequest carries the immutable journal wire, not a new route
// proposal. The journal owner checks the durable identities again after this
// read-only chain proof and before recording first broadcast intent.
type CrossMintFirstSendRequest struct {
	Movement                                  CrossMintPreparationMovement
	Leg, Purpose, PolicyAccount               string
	MinimumSlot                               int64
	SignedWire                                []byte
	ExpectedWireSHA256, ExpectedMessageSHA256 string
	Signature, RecentBlockhash                string
	LastValidBlockHeight                      int64
	SelectedALTs                              []ExecutionALT
	ExternalALTs                              []CrossMintExternalALT
}

// ValidateCrossMintFirstSend verifies the old, unchanged signed message against
// current finalized protocol state. It never fetches a replacement quote, signs,
// acquires movement authority or broadcasts.
func (r *Revalidator) ValidateCrossMintFirstSend(ctx context.Context, input CrossMintFirstSendRequest) error {
	if r == nil || r.rpc == nil || r.signer == "" || input.MinimumSlot <= 0 {
		return errors.New("cross-mint first send requires concrete chain reader and exact journal evidence")
	}
	cert, err := ValidateCrossMintPreflightCertificate(input.Movement, time.Now())
	if err != nil {
		return err
	}
	var plan crossMintPlan
	if json.Unmarshal(input.Movement.ExecutionPlan, &plan) != nil || plan.Bindings.DelegatedSigner != r.signer || input.Movement.DecisionID <= 0 || input.Movement.OpportunityID <= 0 || input.Movement.OptimizerEpochID <= 0 || input.Movement.VaultID <= 0 {
		return errors.New("cross-mint first-send movement identity is incomplete")
	}
	q := CrossMintPreparationRequest{Movement: input.Movement, Leg: input.Leg, Purpose: input.Purpose}
	m := input.Movement
	switch {
	case input.Leg == "withdraw" && input.Purpose == "optimize_yield":
		if m.Phase != "source_reserve" || m.CustodyVersion != 0 || m.CustodyMint != m.SourceMint {
			return errors.New("withdraw first-send phase changed")
		}
	case input.Leg == "swap" && input.Purpose == "optimize_yield":
		if m.Phase != "source_idle" || m.CustodyMint != m.SourceMint {
			return errors.New("swap first-send attribution changed")
		}
	case input.Leg == "deposit" && input.Purpose == "recover_source":
		if m.Phase != "source_idle" || m.CustodyMint != m.SourceMint {
			return errors.New("source recovery first-send attribution changed")
		}
	case input.Leg == "deposit" && (input.Purpose == "optimize_yield" || input.Purpose == "fallback_target"):
		if m.Phase != "target_idle" || m.CustodyMint != m.TargetMint || (input.Purpose == "optimize_yield" && m.ActiveTargetReserve != m.IntendedTargetReserve) || (input.Purpose == "fallback_target" && m.ActiveTargetReserve == m.IntendedTargetReserve) {
			return errors.New("target deposit first-send attribution changed")
		}
	default:
		return errors.New("unsupported cross-mint first-send leg or purpose")
	}
	if input.Leg != "withdraw" && (m.CustodyVersion <= 0 || m.CustodyAmountRaw <= 0 || m.CustodyObservedBalanceRaw == nil || *m.CustodyObservedBalanceRaw < m.CustodyAmountRaw || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0) {
		return errors.New("first-send movement lacks reconciled custody anchors")
	}
	tx, message, actual, tables, err := decodeCrossMintFirstSendWire(input, r.signer)
	if err != nil {
		return err
	}
	if input.LastValidBlockHeight <= 0 || tx.Message.RecentBlockhash.String() != input.RecentBlockhash {
		return errors.New("first-send blockhash metadata changed")
	}
	floor := max(input.MinimumSlot, cert.FinalizedPolicyReadbacks.Withdraw.ContextSlot, cert.FinalizedPolicyReadbacks.Swap.ContextSlot, cert.FinalizedPolicyReadbacks.Deposit.ContextSlot)
	if m.CustodyReconciledSlot != nil {
		floor = max(floor, *m.CustodyReconciledSlot)
	}
	floor = max(floor, int64(plan.Bindings.Withdraw.ObservedSlot), int64(plan.Bindings.Deposit.ObservedSlot), int64(plan.Bindings.Swap.ObservedSlot))
	for _, external := range input.ExternalALTs {
		floor = max(floor, external.ObservedSlot, external.UsableAfterSlot)
	}
	bank, err := r.loadCrossMintRouteBank(ctx, q, plan, nil, floor)
	if err != nil {
		return err
	}
	if input.Leg == "withdraw" {
		if cert.JupiterBuild.TargetObligation != bank.target.Obligation {
			return errors.New("first-send initial withdrawal lost certified target obligation")
		}
		original, err := crossMintCertificateAmount(cert.JupiterBuild.InputPreBalanceRaw)
		if err != nil || binary.LittleEndian.Uint64(bank.accounts[bank.source.Position.VaultLiquidityATA].Data[64:72]) != original {
			return errors.New("first-send initial withdrawal recipient aggregate changed since source certification")
		}
		for _, observation := range []CrossMintCertificatePolicy{cert.FinalizedPolicyReadbacks.Withdraw, cert.FinalizedPolicyReadbacks.Deposit, cert.FinalizedPolicyReadbacks.Swap.CrossMintCertificatePolicy} {
			account := bank.accounts[observation.PolicyAccount]
			hash := sha256.Sum256(account.Data)
			if account.Owner != SquadsProgram || account.Executable || account.Lamports == 0 || hex.EncodeToString(hash[:]) != observation.DataSHA256 {
				return errors.New("first-send initial withdrawal policy data changed since actual source certification")
			}
		}
	}
	if input.Leg != "withdraw" {
		account, ok := bank.accounts[m.CustodyAccount]
		if !ok || validateVaultTokenAccount(account, m.CustodyMint, m.VaultPubkey) != nil || binary.LittleEndian.Uint64(account.Data[64:72]) != uint64(*m.CustodyObservedBalanceRaw) {
			return errors.New("first-send aggregate differs from attributable custody anchor")
		}
	}
	tables, err = r.verifyFinalizedLookupTables(ctx, tables, bank.slot)
	if err != nil {
		return err
	}
	// Provider snapshots remain committed even if compilation chose a managed
	// table instead. Verify every full vector against actual finalized accounts.
	var providerTables []LookupTable
	for _, external := range input.ExternalALTs {
		providerTables = append(providerTables, LookupTable{Address: external.Address, Addresses: external.Addresses, Active: true})
	}
	if _, err := r.verifyFinalizedLookupTables(ctx, providerTables, bank.slot); err != nil {
		return err
	}
	height, err := r.rpc.BlockHeight(ctx, "finalized")
	if err != nil {
		return err
	}
	if height > input.LastValidBlockHeight {
		return errors.New("first-send original blockhash expired")
	}
	if len(actual) < 3 || actual[0].Program != computeProgram || len(actual[0].Accounts) != 0 || len(actual[0].Data) != 5 || actual[0].Data[0] != 2 || actual[1].Program != computeProgram || len(actual[1].Accounts) != 0 || len(actual[1].Data) != 9 || actual[1].Data[0] != 3 {
		return errors.New("first-send budget topology is not the retained two-instruction prefix")
	}
	compute := uint64(binary.LittleEndian.Uint32(actual[0].Data[1:]))
	if compute == 0 || compute > defaultComputeLimit {
		return errors.New("first-send compute limit exceeds retained bound")
	}
	var expected []RouteInstruction
	if input.Leg == "swap" {
		if len(actual) != 3 {
			return errors.New("first-send swap has unexpected outer instructions")
		}
		swap, index, err := decodeCrossMintSignedSwap(actual[2], plan.Bindings, r.signer)
		if err != nil {
			return err
		}
		if input.PolicyAccount != plan.Bindings.Swap.PolicyAccount {
			return errors.New("first-send swap policy identity changed")
		}
		if err := validateCrossMintSignedSwap(swap, index, plan, m, bank, r.crossMintMaxSlippageBPS, r.crossMintMaxValueLossBPS); err != nil {
			return err
		}
		policy, table, limits, err := decodeStrictSwapPolicy(bank.accounts[input.PolicyAccount].Data)
		if err != nil {
			return err
		}
		account := bank.accounts[input.PolicyAccount]
		if account.Owner != SquadsProgram || account.Executable || account.Lamports == 0 {
			return errors.New("first-send swap policy envelope changed")
		}
		dialect := "route_v2"
		if index == 1 {
			dialect = "shared_accounts_route_v2"
		}
		if err := validateCrossMintSwapPolicy(policy, table, limits, plan.Bindings, swap, dialect); err != nil {
			return err
		}
		wrapped, err := wrapSquadsPolicy(input.PolicyAccount, r.signer, plan.Bindings.VaultIndex, []uint8{index}, []RouteInstruction{swap})
		if err != nil {
			return err
		}
		expected = []RouteInstruction{wrapped}
	} else {
		var policy string
		expected, _, _, policy, err = r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
		if err != nil {
			return err
		}
		if policy != input.PolicyAccount {
			return errors.New("first-send bound Kamino policy identity changed")
		}
	}
	// A pure compilation with the OLD blockhash proves instruction order, exact
	// data, static account flags and lookup indexes. No RPC or signer is involved.
	canonical, _, err := compileV0Transaction(r.signer, input.RecentBlockhash, append(append([]RouteInstruction{}, actual[:2]...), expected...), tables, 1, compute)
	if err != nil || !bytes.Equal(canonical.Message, message) {
		return errors.New("first-send signed message differs from current canonical protected leg")
	}
	sim, err := r.rpc.simulateExactTransaction(ctx, input.SignedWire, bank.slot, "finalized")
	if err != nil {
		return err
	}
	if !sim.Succeeded || sim.UnitsConsumed == 0 || sim.UnitsConsumed > compute || sim.WireSHA256 != input.ExpectedWireSHA256 {
		return errors.New("first-send exact old signed-wire simulation failed")
	}
	finalBank, err := r.loadCrossMintRouteBank(ctx, q, plan, nil, bank.slot)
	if err != nil {
		return err
	}
	if !sameCrossMintPreparationBank(bank, finalBank) || time.Since(bank.observedAt) > 15*time.Second {
		return errors.New("first-send finalized bank changed during unchanged-wire proof")
	}
	return ctx.Err()
}

func decodeCrossMintFirstSendWire(input CrossMintFirstSendRequest, signer string) (*solana.Transaction, []byte, []RouteInstruction, []LookupTable, error) {
	fail := func(msg string) (*solana.Transaction, []byte, []RouteInstruction, []LookupTable, error) {
		return nil, nil, nil, nil, errors.New(msg)
	}
	if len(input.SignedWire) == 0 || len(input.SignedWire) > SolanaPacketLimit {
		return fail("first-send signed wire exceeds packet bound")
	}
	hash := sha256.Sum256(input.SignedWire)
	if hex.EncodeToString(hash[:]) != input.ExpectedWireSHA256 {
		return fail("first-send durable wire hash mismatch")
	}
	tx, err := solana.TransactionFromBytes(input.SignedWire)
	if err != nil {
		return fail("first-send signed wire is malformed")
	}
	encoded, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(encoded, input.SignedWire) || tx.Message.GetVersion() != solana.MessageVersionV0 || len(tx.Signatures) != 1 || tx.Message.Header.NumRequiredSignatures != 1 || len(tx.Message.AccountKeys) == 0 || tx.Message.AccountKeys[0].String() != signer || tx.Message.Header.NumReadonlySignedAccounts != 0 || int(tx.Message.Header.NumReadonlyUnsignedAccounts) > len(tx.Message.AccountKeys)-1 || tx.Signatures[0].String() != input.Signature || tx.Message.RecentBlockhash.String() != input.RecentBlockhash {
		return fail("first-send signed wire identity or canonical signer topology changed")
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil || tx.VerifySignatures() != nil {
		return fail("first-send signature does not verify over exact stored message")
	}
	messageHash := sha256.Sum256(message)
	if hex.EncodeToString(messageHash[:]) != input.ExpectedMessageSHA256 {
		return fail("first-send durable message hash mismatch")
	}
	if len(tx.Message.AddressTableLookups) == 0 {
		return fail("first-send lacks exact ALT lookup vector")
	}
	if err := validateCrossMintExternalALTs(input.ExternalALTs); err != nil {
		return fail("first-send external ALT snapshot is malformed")
	}
	byAddress := map[string]LookupTable{}
	managed := map[string]bool{}
	for _, selected := range input.SelectedALTs {
		if selected.TableID <= 0 || selected.FamilyID <= 0 || selected.Generation < 0 || selected.MutationEpoch < 0 || managed[selected.Address] {
			return fail("first-send selected managed ALT identity changed")
		}
		if _, err := CrossMintExternalAddressHash(selected.Addresses); err != nil {
			return fail("first-send managed ALT vector is malformed")
		}
		managed[selected.Address] = true
		byAddress[selected.Address] = LookupTable{ID: selected.TableID, FamilyID: selected.FamilyID, Generation: selected.Generation, MutationEpoch: selected.MutationEpoch, BindingID: selected.BindingID, Address: selected.Address, Addresses: append([]string{}, selected.Addresses...), Active: true}
	}
	for _, external := range input.ExternalALTs {
		if original, ok := byAddress[external.Address]; ok {
			if !reflect.DeepEqual(original.Addresses, external.Addresses) {
				return fail("first-send managed/provider copies disagree")
			}
		} else {
			byAddress[external.Address] = LookupTable{Address: external.Address, Addresses: append([]string{}, external.Addresses...), Active: true}
		}
	}
	loaded := map[solana.PublicKey]solana.PublicKeySlice{}
	var tables []LookupTable
	seen := map[string]bool{}
	managedIndex := 0
	for _, l := range tx.Message.AddressTableLookups {
		address := l.AccountKey.String()
		table, ok := byAddress[address]
		if !ok || seen[address] {
			return fail("first-send wire lookup has unknown or repeated identity")
		}
		seen[address] = true
		if managed[address] {
			if managedIndex >= len(input.SelectedALTs) || input.SelectedALTs[managedIndex].Address != address {
				return fail("first-send managed selection order differs from wire")
			}
			managedIndex++
		}
		keys := make(solana.PublicKeySlice, len(table.Addresses))
		for j, address := range table.Addresses {
			key, err := solana.PublicKeyFromBase58(address)
			if err != nil {
				return fail("first-send ALT member vector is malformed")
			}
			keys[j] = key
		}
		indexes := map[uint8]bool{}
		for _, index := range append(append([]uint8{}, l.WritableIndexes...), l.ReadonlyIndexes...) {
			if int(index) >= len(keys) || indexes[index] {
				return fail("first-send ALT lookup index is repeated or out of range")
			}
			indexes[index] = true
		}
		loaded[l.AccountKey] = keys
		tables = append(tables, table)
	}
	if managedIndex != len(input.SelectedALTs) {
		return fail("first-send selected managed ALT does not contribute to wire")
	}
	if tx.Message.SetAddressTables(loaded) != nil || tx.Message.ResolveLookups() != nil {
		return fail("first-send SDK could not resolve exact selected ALTs")
	}
	keys, err := tx.Message.GetAllKeys()
	if err != nil || len(keys) > 256 {
		return fail("first-send account key vector exceeds index space")
	}
	unique := map[solana.PublicKey]bool{}
	for _, key := range keys {
		if unique[key] {
			return fail("first-send account vector has duplicate loaded identity")
		}
		unique[key] = true
	}
	var instructions []RouteInstruction
	for _, compiled := range tx.Message.Instructions {
		if int(compiled.ProgramIDIndex) >= len(keys) {
			return fail("first-send program index is out of range")
		}
		metas, e := compiled.ResolveInstructionAccounts(&tx.Message)
		if e != nil {
			return fail("first-send instruction indexes are invalid")
		}
		ix := RouteInstruction{Program: keys[compiled.ProgramIDIndex].String(), Data: bytes.Clone(compiled.Data)}
		for _, meta := range metas {
			ix.Accounts = append(ix.Accounts, InstructionAccount{Address: meta.PublicKey.String(), Signer: meta.IsSigner, Writable: meta.IsWritable})
		}
		instructions = append(instructions, ix)
	}
	return tx, message, instructions, tables, nil
}

// This parses only the small retained Squads compact instruction envelope;
// Solana transaction/message decoding and ALT resolution above are SDK-owned.
func decodeCrossMintSignedSwap(outer RouteInstruction, b CrossMintPolicyBindings, signer string) (RouteInstruction, uint8, error) {
	var inner RouteInstruction
	fail := func() (RouteInstruction, uint8, error) {
		return inner, 0, errors.New("first-send Squads compact single-swap envelope is not canonical")
	}
	if outer.Program != SquadsProgram || len(outer.Accounts) < 4 || outer.Accounts[0].Address != b.Swap.PolicyAccount || outer.Accounts[1].Address != SquadsProgram || outer.Accounts[2].Address != signer || len(outer.Data) < 29 {
		return fail()
	}
	prefix := []byte{90, 81, 187, 81, 39, 70, 128, 78, b.VaultIndex, 1, 1, 1, 1}
	data := outer.Data
	if !bytes.Equal(data[:13], prefix) || binary.LittleEndian.Uint32(data[13:17]) != 1 || data[17] > 1 || data[18] != 1 || data[19] != b.VaultIndex || uint64(binary.LittleEndian.Uint32(data[20:24])) != uint64(len(data)-24) {
		return fail()
	}
	compact := data[24:]
	if len(compact) < 5 || compact[0] != 1 {
		return fail()
	}
	count := int(compact[2])
	end := 3 + count
	if count == 0 || end+2 > len(compact) || int(compact[1])+3 >= len(outer.Accounts) || int(binary.LittleEndian.Uint16(compact[end:end+2])) != len(compact)-end-2 {
		return fail()
	}
	inner.Program = outer.Accounts[int(compact[1])+3].Address
	inner.Data = bytes.Clone(compact[end+2:])
	for _, index := range compact[3:end] {
		if int(index)+3 >= len(outer.Accounts) {
			return fail()
		}
		inner.Accounts = append(inner.Accounts, InstructionAccount{Address: outer.Accounts[int(index)+3].Address})
	}
	core := 10
	if len(inner.Data) >= 8 && bytes.Equal(inner.Data[:8], jupiterSharedV2Discriminator) {
		core = 12
	}
	if len(inner.Accounts) < core+14 || len(inner.Accounts) > core+16 {
		return fail()
	}
	// Compact CPI bytes encode account indexes, not local flags. Recover only the
	// canonical flags defined by the strict retained Jupiter/AlphaQ topology,
	// then exact recompilation below proves the outer transaction's actual flags.
	if core == 10 {
		inner.Accounts[0].Signer = true
		for _, i := range []int{1, 2} {
			inner.Accounts[i].Writable = true
		}
	} else {
		inner.Accounts[1].Signer = true
		for _, i := range []int{2, 3, 4, 5} {
			inner.Accounts[i].Writable = true
		}
	}
	for _, i := range []int{3, 4, 5, 6, 7, 10} {
		inner.Accounts[core+i].Writable = true
	}
	canonical, e := wrapSquadsPolicy(b.Swap.PolicyAccount, signer, b.VaultIndex, []uint8{data[17]}, []RouteInstruction{inner})
	if e != nil || !bytes.Equal(canonical.Data, outer.Data) || len(canonical.Accounts) != len(outer.Accounts) {
		return fail()
	}
	for i, a := range canonical.Accounts {
		if a.Address != outer.Accounts[i].Address {
			return fail()
		}
	}
	return inner, data[17], nil
}

func validateCrossMintSignedSwap(ix RouteInstruction, index uint8, plan crossMintPlan, m CrossMintPreparationMovement, bank crossMintPreparationBank, localSlippage, localValueLoss uint16) error {
	amountOffset, slipOffset, core := 8, 24, 10
	if index == 1 {
		amountOffset, slipOffset, core = 9, 25, 12
	}
	if len(ix.Data) < slipOffset+3 || len(ix.Accounts) <= core+2 || uint64(m.CustodyAmountRaw) > plan.Bindings.Swap.DailySourceMintSpendingCap {
		return errors.New("first-send signed swap is truncated or exceeds source spending cap")
	}
	amount := binary.LittleEndian.Uint64(ix.Data[amountOffset : amountOffset+8])
	quoted := binary.LittleEndian.Uint64(ix.Data[amountOffset+8 : amountOffset+16])
	slippage := binary.LittleEndian.Uint16(ix.Data[slipOffset : slipOffset+2])
	if amount != uint64(m.CustodyAmountRaw) || slippage == 0 || localSlippage == 0 || localValueLoss == 0 || slippage > min(localSlippage, plan.Bindings.Swap.MaxSlippageBPS) {
		return errors.New("first-send signed swap differs from immutable custody or slippage limit")
	}
	inputATA, e := deriveATA(m.VaultPubkey, m.SourceMint, mustStableProgram(m.SourceMint))
	if e != nil {
		return e
	}
	outputATA, e := deriveATA(m.VaultPubkey, m.TargetMint, mustStableProgram(m.TargetMint))
	if e != nil {
		return e
	}
	if inputATA != m.CustodyAccount {
		return errors.New("first-send source ATA differs from reconciled custody")
	}
	raw := rawJupiterBuild{InputMint: m.SourceMint, OutputMint: m.TargetMint, InAmount: strconv.FormatUint(amount, 10), OutAmount: strconv.FormatUint(quoted, 10), SlippageBPS: slippage}
	routes := []rawJupiterRoute{{SwapInfo: rawJupiterSwapInfo{AMMKey: ix.Accounts[core+2].Address, InputMint: m.SourceMint, OutputMint: m.TargetMint}, BPS: 10000}}
	dialect, e := validateJupiterSwap(ix, raw, routes, m.VaultPubkey, inputATA, outputATA)
	if e != nil {
		return e
	}
	if (index == 0 && dialect != "route_v2") || (index == 1 && dialect != "shared_accounts_route_v2") {
		return errors.New("first-send swap constraint index substituted")
	}
	minimum, e := minimumEconomicOutput(amount, min(localValueLoss, plan.ValueLoss))
	if e != nil {
		return e
	}
	profitable, e := minimumProfitableCrossMintOutput(m.ExecutionPlan, amount, bank.sourceEconomics.SupplyAPYBPS, bank.targetEconomics.SupplyAPYBPS)
	if e != nil {
		return e
	}
	if bank.targetEconomics.LastUpdateStale || bank.targetEconomics.EconomicLifetimeMillis <= 0 || bank.targetEconomics.TotalSupplyUSDMicros <= minimumReserveSupplyUSDMicros || bank.targetEconomics.SupplyAPYBPS < 0 || bank.targetEconomics.SupplyAPYBPS >= 5000 || thresholdFor(quoted, slippage) < max(minimum, profitable) {
		return fmt.Errorf("first-send unchanged signed swap no longer meets finalized economics")
	}
	return nil
}
