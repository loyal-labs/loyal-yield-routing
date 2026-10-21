package multiply

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	computeUnitLimit   = uint32(400_000)
	maxTransactionFee  = uint64(20_000)
	solanaPacketBytes  = 1232

	altMetaBytes                    = 56
	altTypeOffset                   = 0
	altTypeLookupTable              = 1
	altDeactivationSlotOffset       = 4
	altLastExtendedStartIndexOffset = 20
	altAuthorityTagOffset           = 21
)

type ExecutorChain interface {
	GenesisHash(context.Context) (solana.Hash, error)
	Blockhash(context.Context, rpc.CommitmentType, uint64) (hash solana.Hash, lastValid, slot uint64, err error)
	Accounts(context.Context, []solana.PublicKey, rpc.CommitmentType, uint64) (uint64, []*chain.Account, error)
	Simulate(context.Context, []byte, rpc.SimulateTransactionOpts) (chain.Simulated, error)
	Receipt(context.Context, solana.Signature, rpc.CommitmentType) (chain.Receipt, error)
}

type Executor struct {
	Chain    ExecutorChain
	signer   ed25519.PrivateKey
	feePayer ed25519.PrivateKey
}

func NewExecutor(c ExecutorChain, signer ed25519.PrivateKey) (*Executor, error) {
	return NewExecutorWithFeePayer(c, signer, signer)
}

func NewRecoveryExecutorContext(ctx context.Context, c ExecutorChain) (*Executor, error) {
	if ctx == nil || c == nil {
		return nil, errors.New("recovery executor requires context and RPC")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	genesis, err := c.GenesisHash(probe)
	if probe.Err() != nil {
		return nil, probe.Err()
	}
	if err != nil {
		return nil, err
	}
	if genesis.String() != mainnetGenesisHash {
		return nil, errors.New("recovery RPC is not mainnet-beta")
	}
	return &Executor{Chain: c}, nil
}

func NewExecutorWithFeePayer(c ExecutorChain, feePayer, signer ed25519.PrivateKey) (*Executor, error) {
	return NewExecutorWithFeePayerContext(context.Background(), c, feePayer, signer)
}

func NewExecutorWithFeePayerContext(ctx context.Context, c ExecutorChain, feePayer, signer ed25519.PrivateKey) (*Executor, error) {
	if ctx == nil {
		return nil, errors.New("multiply executor requires caller context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPrivateKey(signer) || !validPrivateKey(feePayer) {
		return nil, errors.New("multiply executor requires an ed25519 delegate key")
	}
	if c == nil {
		return nil, errors.New("multiply executor requires a chain client")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	genesis, err := c.GenesisHash(ctx)
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, fmt.Errorf("genesis preflight: %w", cancellation)
	}
	if err != nil {
		return nil, fmt.Errorf("genesis preflight: %w", err)
	}
	if genesis.String() != mainnetGenesisHash {
		return nil, fmt.Errorf("rpc is not mainnet-beta (genesis %s)", genesis)
	}
	return &Executor{Chain: c, signer: append(ed25519.PrivateKey(nil), signer...), feePayer: append(ed25519.PrivateKey(nil), feePayer...)}, nil
}

func (e *Executor) Delegate() solana.PublicKey {
	return solana.PublicKey(e.signer[32:])
}

func (e *Executor) PrepareAndSign(ctx context.Context, built *BuiltOperation, policy solana.PublicKey, accountIndex uint8, constraintIndexes []byte, minContextSlot uint64) (*SignedOperation, uint64, error) {
	if built != nil && len(built.PolicyInstructions) == 1 && built.PolicyInstructions[0].ProgramID == solana.TokenProgramID {
		return nil, 0, errors.New("delegate cannot sign wallet-owned token claims")
	}
	if built == nil || len(built.PolicyInstructions) != 1 {
		return nil, 0, errors.New("policy stage must contain exactly one terminal instruction")
	}
	if len(constraintIndexes) != 1 || len(built.PolicyInstructions[0].Accounts) > 252 || len(built.PolicyInstructions[0].Data) > 65535 {
		return nil, 0, errors.New("terminal policy instruction exceeds bounds")
	}
	if len(built.LookupTables) > 4 {
		return nil, 0, errors.New("lookup table count exceeds four")
	}
	seenTables := map[solana.PublicKey]bool{}
	for _, key := range built.LookupTables {
		if seenTables[key] {
			return nil, 0, errors.New("duplicated lookup table")
		}
		seenTables[key] = true
	}
	if !validPrivateKey(e.signer) || !validPrivateKey(e.feePayer) {
		return nil, 0, errors.New("executor lost its signing capability")
	}
	blockhash, lastValid, slot, err := e.Chain.Blockhash(ctx, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch blockhash: %w", err)
	}
	if minContextSlot > slot {
		slot = minContextSlot
	}
	if built.QuoteContextSlot != nil {
		if *built.QuoteContextSlot == 0 {
			return nil, 0, errors.New("swap quote omitted its context slot")
		}
		if *built.QuoteContextSlot > slot {
			slot = *built.QuoteContextSlot
		}
	}
	inner := squads.Instruction{ProgramID: built.PolicyInstructions[0].ProgramID, Data: built.PolicyInstructions[0].Data}
	for _, meta := range built.PolicyInstructions[0].Accounts {
		inner.Accounts = append(inner.Accounts, solana.AccountMeta{PublicKey: meta.PubKey, IsSigner: meta.IsSigner, IsWritable: meta.IsWritable})
	}
	terminal, err := squads.ExecuteTransactionSyncV2(squads.ExecuteSync{Policy: policy, Signer: e.Delegate(), AccountIndex: accountIndex, ConstraintIndexes: constraintIndexes, Inner: []squads.Instruction{inner}})
	if err != nil {
		return nil, 0, err
	}
	outer := append(computeBudgetPreamble(), built.PreInstructions...)
	instructions := make([]solana.Instruction, 0, len(outer)+1)
	for _, ix := range outer {
		instructions = append(instructions, sdkInstruction(ix))
	}
	terminalMetas := make(solana.AccountMetaSlice, len(terminal.Accounts))
	for i := range terminal.Accounts {
		terminalMetas[i] = &terminal.Accounts[i]
	}
	instructions = append(instructions, solana.NewInstruction(terminal.ProgramID, terminalMetas, terminal.Data))
	feePayer := solana.PublicKey(e.feePayer[32:])
	opts := []solana.TransactionOption{solana.TransactionPayer(feePayer)}
	if len(built.LookupTables) > 0 {
		tables, err := e.lookupTables(ctx, built.LookupTables)
		if err != nil {
			return nil, 0, err
		}
		if len(tables) != len(built.LookupTables) {
			return nil, 0, errors.New("lookup table result count mismatch")
		}
		for _, key := range built.LookupTables {
			if len(tables[key]) == 0 || len(tables[key]) > 256 {
				return nil, 0, errors.New("lookup table has invalid address count")
			}
		}
		opts = append(opts, solana.TransactionAddressTables(tables))
	}
	tx, err := solana.NewTransaction(instructions, blockhash, opts...)
	if err != nil {
		return nil, 0, err
	}
	if len(built.LookupTables) > 0 {
		tx.Message.SetVersion(solana.MessageVersionV0)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}
	fee, err := chain.MessageFee(message)
	if err != nil {
		return nil, 0, err
	}
	if fee > maxTransactionFee {
		return nil, 0, fmt.Errorf("transaction fee %d exceeds %d", fee, maxTransactionFee)
	}
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		var private ed25519.PrivateKey
		switch key {
		case feePayer:
			private = e.feePayer
		case e.Delegate():
			private = e.signer
		default:
			return nil
		}
		sdk := solana.PrivateKey(private)
		return &sdk
	})
	if err != nil {
		return nil, 0, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}
	if len(wire) > solanaPacketBytes {
		return nil, 0, fmt.Errorf("multiply packet is %d bytes, exceeds %d", len(wire), solanaPacketBytes)
	}
	signed, err := NewSignedOperation(wire, tx.Signatures[0].String(), blockhash.String(), lastValid)
	return signed, slot, err
}

func validPrivateKey(key ed25519.PrivateKey) bool {
	return len(key) == ed25519.PrivateKeySize && bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]))
}

func sdkInstruction(ix Instruction) solana.Instruction {
	metas := make(solana.AccountMetaSlice, len(ix.Accounts))
	for i, meta := range ix.Accounts {
		metas[i] = &solana.AccountMeta{PublicKey: meta.PubKey, IsSigner: meta.IsSigner, IsWritable: meta.IsWritable}
	}
	return solana.NewInstruction(ix.ProgramID, metas, ix.Data)
}

func computeBudgetPreamble() []Instruction {
	ix := spl.SetComputeUnitLimit(computeUnitLimit)
	return []Instruction{{ProgramID: ix.ProgID, Data: ix.DataBytes}}
}

type PolicyEvidence struct {
	Account           solana.PublicKey
	DataSHA256        string
	ConstraintIndexes []byte
}

func (e *Executor) EnsureExactPolicy(ctx context.Context, topology *EarnMaxTopology, plan *ActionPlan, built *BuiltOperation) (*PolicyEvidence, error) {
	if plan == nil || plan.Action == ActionClaim {
		return nil, errors.New("claim belongs to the app root-wallet owner; delegate execution is forbidden")
	}
	if !plan.StrategyKey.Valid() {
		return nil, errors.New("delegate operation has no strategy")
	}
	config, err := topology.Strategy(plan.StrategyKey)
	if err != nil {
		return nil, err
	}
	policy, ok := config.PolicyForAction(plan.Action)
	if !ok {
		return nil, errors.New("strategy has no policy for action")
	}
	family, err := FamilyForAction(plan.Action)
	if err != nil {
		return nil, err
	}
	account, _, err := e.policyAccount(ctx, policy.Account, 0)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("exact ProgramInteraction policy is absent")
	}
	expected, err := CanonicalConstraints(topology, family)
	if err != nil {
		return nil, err
	}
	matches, err := CurrentPolicyMatches(account, policy, e.Delegate(), expected, topology.VaultIndex)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, fmt.Errorf("stable production policy %s is not installed", policy.Account)
	}
	constraintIndexes, err := ConstraintIndexes(config, plan.Action, built.PolicyInstructions)
	if err != nil {
		return nil, err
	}
	return &PolicyEvidence{
		Account:           policy.Account,
		DataSHA256:        PolicyDataHash(account.Data),
		ConstraintIndexes: constraintIndexes,
	}, nil
}

func decodeVerifiedTransaction(wire []byte) (*solana.Transaction, error) {
	if len(wire) == 0 || len(wire) > solanaPacketBytes {
		return nil, errors.New("signed wire size is invalid")
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return nil, err
	}
	if len(tx.Signatures) < 1 || len(tx.Signatures) > 2 || int(tx.Message.Header.NumRequiredSignatures) != len(tx.Signatures) {
		return nil, errors.New("invalid signing authority count")
	}
	canonical, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(wire, canonical) {
		return nil, errors.New("wire is not canonical or contains trailing bytes")
	}
	if err := tx.VerifySignatures(); err != nil {
		return nil, err
	}
	return tx, nil
}

func PersistedTransaction(operation *MultiplyOperation) ([]byte, error) {
	if operation == nil {
		return nil, errors.New("operation is missing")
	}
	wire := operation.SignedWire
	digest := sha256.Sum256(wire)
	if operation.SignedWireSHA256 == nil || hexEncode(digest[:]) != *operation.SignedWireSHA256 {
		return nil, errors.New("persisted wire hash drifted")
	}
	tx, err := decodeVerifiedTransaction(wire)
	if err != nil {
		return nil, err
	}
	if operation.TransactionSignature == nil || tx.Signatures[0].String() != *operation.TransactionSignature {
		return nil, errors.New("persisted signature drifted")
	}
	if operation.RecentBlockhash == nil || tx.Message.RecentBlockhash.String() != *operation.RecentBlockhash {
		return nil, errors.New("persisted blockhash drifted")
	}
	hash, err := MessageSHA256(wire)
	if err != nil {
		return nil, err
	}
	if operation.MessageSHA256 == nil || hash != *operation.MessageSHA256 {
		return nil, errors.New("persisted message hash drifted")
	}
	return append([]byte(nil), wire...), nil
}

func MessageSHA256(wire []byte) (string, error) {
	tx, err := decodeVerifiedTransaction(wire)
	if err != nil {
		return "", err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(message)
	return hexEncode(digest[:]), nil
}

func (e *Executor) Simulate(ctx context.Context, signed *SignedOperation, minContextSlot uint64) (chain.Simulated, error) {
	opts := rpc.SimulateTransactionOpts{SigVerify: true, Commitment: rpc.CommitmentConfirmed}
	if minContextSlot > 0 {
		opts.MinContextSlot = &minContextSlot
	}
	return e.Chain.Simulate(ctx, signed.Wire, opts)
}

// VerifyExpectedEffects checks confirmed balances against the persisted pre-transaction anchors.
func VerifyExpectedEffects(effects *ExpectedEffects, action MultiplyAction, after *ObservedRoute, topology *EarnMaxTopology) error {
	if effects == nil || after == nil || topology == nil {
		return errors.New("effect evidence is missing")
	}
	seen := map[string]bool{}
	for _, delta := range effects.TokenDeltas {
		identity := delta.Account + ":" + delta.Mint
		if seen[identity] {
			return errors.New("duplicated token effect")
		}
		seen[identity] = true
		var anchor *TokenAmountBefore
		for i := range effects.TokenAmountsBefore {
			value := &effects.TokenAmountsBefore[i]
			if value.Account == delta.Account && value.Mint == delta.Mint {
				if anchor != nil {
					return errors.New("duplicate persisted token anchor")
				}
				anchor = value
			}
		}
		if anchor == nil {
			return errors.New("operation omitted its pre-transaction token amount")
		}
		post, ok := observedTokenAmount(after, delta.Account, delta.Mint)
		if !ok {
			return errors.New("post-state omitted expected custody identity")
		}
		if delta.RawDelta < 0 {
			if post > anchor.AmountRaw {
				return errors.New("source custody increased")
			}
			spent := anchor.AmountRaw - post
			expected := uint64(-(delta.RawDelta + 1)) + 1
			valid := spent == expected
			switch action {
			case ActionSwapCollateralToDebt:
				valid = spent > 0 && spent <= expected
			case ActionRepayDebt:
				if effects.ObligationBefore != nil && expected >= effects.ObligationBefore.DebtRaw {
					valid = spent > 0 && spent <= anchor.AmountRaw
				}
			}
			if !valid {
				return errors.New("confirmed source custody delta violated exact input or maximum")
			}
		} else if delta.RawDelta > 0 {
			if post < anchor.AmountRaw || post-anchor.AmountRaw < uint64(delta.RawDelta) {
				return errors.New("confirmed destination custody delta missed minimum")
			}
		}
	}
	if effects.ObligationDelta != nil {
		before := effects.ObligationBefore
		delta := effects.ObligationDelta
		if before == nil || before.Obligation != delta.Obligation {
			return errors.New("obligation anchor is missing or has different identity")
		}
		obligation, err := solana.PublicKeyFromBase58(delta.Obligation)
		if err != nil {
			return err
		}
		post := positionForObligation(after, obligation, topology)
		if post == nil {
			return errors.New("post-state omitted obligation")
		}
		direction := func(delta int64, pre, post uint64) bool {
			return delta == 0 || (delta > 0 && post > pre) || (delta < 0 && post < pre)
		}
		if !direction(delta.CollateralRawDelta, before.CollateralRaw, post.CollateralDepositedRaw) || !direction(delta.DebtRawDelta, before.DebtRaw, post.DebtRaw) {
			return errors.New("confirmed obligation movement had wrong direction")
		}
		debtMagnitude := uint64(delta.DebtRawDelta)
		if delta.DebtRawDelta < 0 {
			debtMagnitude = uint64(-(delta.DebtRawDelta + 1)) + 1
		}
		if action == ActionRepayDebt && debtMagnitude >= before.DebtRaw && post.DebtRaw != 0 {
			return errors.New("repay-all left confirmed obligation debt")
		}
	}
	return nil
}

func observedTokenAmount(observed *ObservedRoute, account, mint string) (uint64, bool) {
	if observed == nil {
		return 0, false
	}
	var amount uint64
	found := false
	for _, v := range observed.balances() {
		if v.Account == account && v.Mint == mint {
			if found && amount != v.AmountRaw {
				return 0, false
			}
			amount = v.AmountRaw
			found = true
		}
	}
	return amount, found
}

func positionForObligation(observed *ObservedRoute, obligation solana.PublicKey, topology *EarnMaxTopology) *StrategyObservation {
	for _, post := range observed.Strategies {
		if post == nil {
			continue
		}
		config, err := topology.Strategy(post.StrategyKey)
		if err == nil && config.Obligation == obligation {
			return post
		}
	}
	return nil
}

func hexEncode(value []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(value)*2)
	for _, b := range value {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}

func (e *Executor) lookupTables(ctx context.Context, keys []solana.PublicKey) (map[solana.PublicKey]solana.PublicKeySlice, error) {
	if len(keys) == 0 || len(keys) > 4 {
		return nil, errors.New("lookup table count is invalid")
	}
	seen := map[solana.PublicKey]bool{}
	for _, key := range keys {
		if seen[key] {
			return nil, errors.New("duplicate lookup table")
		}
		seen[key] = true
	}
	_, accounts, err := e.Chain.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	tables := make(map[solana.PublicKey]solana.PublicKeySlice, len(keys))
	for i, account := range accounts {
		if account == nil || account.Owner != solana.AddressLookupTableProgramID || account.Executable {
			return nil, errors.New("lookup table is absent or invalid")
		}
		data := account.Data
		if len(data) < altMetaBytes || (len(data)-altMetaBytes)%solana.PublicKeyLength != 0 ||
			binary.LittleEndian.Uint32(data[altTypeOffset:]) != altTypeLookupTable ||
			binary.LittleEndian.Uint64(data[altDeactivationSlotOffset:]) != math.MaxUint64 ||
			data[altAuthorityTagOffset] > 1 {
			return nil, errors.New("lookup table state is inactive or malformed")
		}
		count := (len(data) - altMetaBytes) / solana.PublicKeyLength
		if count == 0 || count > 256 || int(data[altLastExtendedStartIndexOffset]) > count {
			return nil, errors.New("lookup table address count is invalid")
		}
		addresses := make(solana.PublicKeySlice, count)
		for j := range addresses {
			start := altMetaBytes + j*solana.PublicKeyLength
			copy(addresses[j][:], data[start:start+solana.PublicKeyLength])
		}
		tables[keys[i]] = addresses
	}
	return tables, nil
}

func (e *Executor) policyAccount(ctx context.Context, key solana.PublicKey, minContextSlot uint64) (*chain.Account, uint64, error) {
	slot, accounts, err := e.Chain.Accounts(ctx, []solana.PublicKey{key}, rpc.CommitmentConfirmed, minContextSlot)
	if err != nil {
		return nil, 0, err
	}
	if accounts[0] != nil && (accounts[0].Executable || accounts[0].Owner != squads.ProgramID) {
		return nil, 0, errors.New("policy account has invalid owner or context")
	}
	return accounts[0], slot, nil
}
