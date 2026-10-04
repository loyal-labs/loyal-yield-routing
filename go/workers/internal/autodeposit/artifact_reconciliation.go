package autodeposit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

var ErrArtifactCreationProofPending = errors.New("autodeposit artifact creator proof remains unavailable")

type ArtifactRole string

const (
	ArtifactPolicy     ArtifactRole = "policy"
	ArtifactDelegation ArtifactRole = "delegation"
)

type ArtifactTarget struct {
	ControlTarget
	RootAuthority                                string
	PeriodLength, ExpiryTimestamp                *int64
	PolicySignature, DelegationSignature         *string
	PolicyConfirmedSlot, DelegationConfirmedSlot *int64
}
type ArtifactHistoryEntry struct {
	Signature string
	Slot      int64
	Failed    bool
}
type ArtifactReceipt struct {
	Signature                 string
	Slot                      int64
	Wire                      []byte
	PreLamports, PostLamports []uint64
	// These are the confirmed transaction metadata's loaded-address lists.
	// The reader also resolves the signed lookup keys independently from RPC.
	LoadedWritable, LoadedReadonly []string
	InnerInstructions              []solana.CompiledInstruction
}
type ArtifactHistory interface {
	ArtifactHistory(context.Context, string, int) ([]ArtifactHistoryEntry, error)
	ArtifactReceipt(context.Context, string) (ArtifactReceipt, error)
}

// Creation proof fields are deliberately private. Only the verifier below can
// issue a proof to the Store; account touches and caller-supplied JSON cannot.
type VerifiedArtifactCreationProof struct {
	target                                ArtifactTarget
	role                                  ArtifactRole
	account, signature, instructionSHA256 string
	slot                                  int64
}

type ArtifactProofReader struct {
	Wires   *SweepWireBuilder
	History ArtifactHistory
}

func (r *ArtifactProofReader) FindCreationProof(ctx context.Context, target ArtifactTarget, role ArtifactRole, minimumSlot int64) (VerifiedArtifactCreationProof, error) {
	if r == nil || r.Wires == nil || r.Wires.proxy == nil || r.History == nil {
		return VerifiedArtifactCreationProof{}, errors.New("artifact proof reader requires official builder and history")
	}
	if err := r.Wires.proveArtifactAccounts(ctx, target, minimumSlot); err != nil {
		return VerifiedArtifactCreationProof{}, err
	}
	account := target.Policy
	if role == ArtifactDelegation {
		account = target.RecurringDelegation
	} else if role != ArtifactPolicy {
		return VerifiedArtifactCreationProof{}, errors.New("unknown artifact role")
	}
	const maximumHistory = 32
	entries, err := r.History.ArtifactHistory(ctx, account, maximumHistory)
	if err != nil {
		return VerifiedArtifactCreationProof{}, err
	}
	if len(entries) > maximumHistory {
		return VerifiedArtifactCreationProof{}, errors.New("artifact history exceeds bound")
	}
	for _, entry := range entries {
		if entry.Failed || entry.Signature == "" || entry.Slot <= 0 {
			continue
		}
		receipt, err := r.History.ArtifactReceipt(ctx, entry.Signature)
		if err != nil {
			if errors.Is(err, ErrArtifactCreationProofPending) {
				continue
			}
			return VerifiedArtifactCreationProof{}, err
		}
		if receipt.Signature != entry.Signature || receipt.Slot != entry.Slot {
			return VerifiedArtifactCreationProof{}, errors.New("artifact history and receipt identities disagree")
		}
		proof, err := r.Wires.verifyArtifactCreator(ctx, target, role, receipt)
		if err == nil {
			return proof, nil
		}
		if !errors.Is(err, ErrArtifactCreationProofPending) {
			return VerifiedArtifactCreationProof{}, err
		}
	}
	return VerifiedArtifactCreationProof{}, ErrArtifactCreationProofPending
}

func (b *SweepWireBuilder) proveArtifactAccounts(ctx context.Context, target ArtifactTarget, minimumSlot int64) error {
	if target.Nonce == nil || target.MaxAmountPerPeriod == nil || target.PeriodLength == nil || target.StartTimestamp == nil || target.ExpiryTimestamp == nil || *target.Nonce < 0 || *target.MaxAmountPerPeriod <= 0 || *target.PeriodLength <= 0 {
		return ErrArtifactCreationProofPending
	}
	// This additionally proves all canonical PDAs, mint, full execution policy
	// permissions/constraints, recurring identity/budget and token delegate.
	observation, err := b.ObserveControl(ctx, target.ControlTarget, minimumSlot)
	if err != nil {
		return err
	}
	if observation.status() != "active" {
		return ErrArtifactCreationProofPending
	}
	addresses := []string{target.Settings, target.Policy, target.RecurringDelegation}
	slot, accounts, err := b.read(ctx, addresses)
	if err != nil {
		return err
	}
	if slot < observation.ObservedSlot || len(accounts) != 3 {
		return errors.New("artifact account snapshot is behind control evidence")
	}
	if err = verifyArtifactRoot(accounts[0], target); err != nil {
		return err
	}
	for i := range accounts {
		if accounts[i].Address != addresses[i] || accounts[i].Executable {
			return errors.New("artifact account snapshot identity invalid")
		}
	}
	if accounts[1].Owner != squadsProgramID {
		return errors.New("artifact policy has foreign owner")
	}
	nonce := uint64(*target.Nonce)
	if _, err = RemainingDelegationAllowance(accounts[2].Owner, accounts[2].Data, DelegationIdentity{Account: target.RecurringDelegation, Delegator: target.Wallet, Delegatee: target.Vault, Mint: target.Mint, Nonce: &nonce}); err != nil {
		return err
	}
	if binary.LittleEndian.Uint64(accounts[2].Data[delegationPerPeriodOffset:delegationPerPeriodOffset+8]) != uint64(*target.MaxAmountPerPeriod) {
		return errors.New("artifact delegation budget changed between snapshots")
	}
	_, err = b.proxy.BuildCanonicalSubscriptionPolicy(ctx, fleet.CanonicalSubscriptionPolicyRequest{Settings: target.Settings, RootAuthority: target.RootAuthority, Payer: target.RootAuthority, DelegatedSigner: b.delegate.String(), PolicySeed: uint64(target.PolicySeed), Wallet: target.Wallet, Vault: target.Vault, MaxAmountPerPeriod: uint64(*target.MaxAmountPerPeriod), PolicyDataHex: hex.EncodeToString(accounts[1].Data)})
	return err
}

// The layout is pinned to Loyal smart-accounts core generated Settings.ts and
// the existing SDK/SVM SettingsWire fixture. It accepts the supported personal
// root shape only: threshold1, one root signer with mask7, no external settings
// authority. Multi-owner or handed-off roots require separate evidence.
func verifyArtifactRoot(a backyard.ConfirmedAccount, target ArtifactTarget) error {
	d := a.Data
	if a.Address != target.Settings || a.Owner != squadsProgramID || a.Executable || len(d) < 94 || !bytes.Equal(d[:8], []byte{223, 179, 163, 190, 177, 224, 67, 173}) {
		return errors.New("artifact root settings owner or layout invalid")
	}
	if base58Key(d[24:56]) != systemProgramZero || binary.LittleEndian.Uint16(d[56:58]) != 1 || binary.LittleEndian.Uint32(d[58:62]) != 0 || binary.LittleEndian.Uint64(d[70:78]) > binary.LittleEndian.Uint64(d[62:70]) {
		return errors.New("artifact root settings is not a supported personal root")
	}
	offset := 79
	switch d[78] {
	case 0:
	case 1:
		offset += 32
	default:
		return errors.New("artifact settings archival option invalid")
	}
	offset += 8
	if offset+5 > len(d) {
		return errors.New("artifact settings truncated before signers")
	}
	bump := d[offset]
	offset++
	count := binary.LittleEndian.Uint32(d[offset : offset+4])
	offset += 4
	if count != 1 || offset+33 > len(d) || base58Key(d[offset:offset+32]) != target.RootAuthority || d[offset+32] != 7 {
		return errors.New("artifact settings root signer differs from verified target authority")
	}
	offset += 33
	if offset+2 > len(d) {
		return errors.New("artifact settings truncated after signers")
	}
	offset++
	switch d[offset] {
	case 0:
		offset++
	case 1:
		offset += 9
	default:
		return errors.New("artifact settings policy-seed option invalid")
	}
	if offset >= len(d) {
		return errors.New("artifact settings missing reserved field")
	}
	offset++
	for _, v := range d[offset:] {
		if v != 0 {
			return errors.New("artifact settings unsupported trailing state")
		}
	}
	settings, expectedBump, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("settings"), d[8:24]}, mustKey(squadsProgramID))
	if err != nil || settings.String() != target.Settings || bump != expectedBump {
		return errors.New("artifact settings seed and bump differ from canonical root")
	}
	return nil
}

func (b *SweepWireBuilder) verifyArtifactCreator(ctx context.Context, target ArtifactTarget, role ArtifactRole, receipt ArtifactReceipt) (VerifiedArtifactCreationProof, error) {
	var proof VerifiedArtifactCreationProof
	if target.Nonce == nil || target.MaxAmountPerPeriod == nil || target.PeriodLength == nil || target.StartTimestamp == nil || target.ExpiryTimestamp == nil {
		return proof, ErrArtifactCreationProofPending
	}
	if receipt.Slot <= 0 || len(receipt.Wire) == 0 || len(receipt.Wire) > solanaPacketBytes {
		return proof, ErrArtifactCreationProofPending
	}
	tx, err := solana.TransactionFromBytes(receipt.Wire)
	if err != nil {
		return proof, err
	}
	if err = validateSignedMessageHeader(tx); err != nil {
		return proof, err
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, receipt.Wire) {
		return proof, errors.New("artifact creator packet is not canonical")
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0].String() != receipt.Signature {
		return proof, errors.New("artifact creator signature differs from exact packet")
	}
	if err = tx.VerifySignatures(); err != nil {
		return proof, err
	}
	signers := tx.Message.Signers()
	required := target.RootAuthority
	if role == ArtifactDelegation {
		required = target.Wallet
	}
	foundSigner := false
	for _, signer := range signers {
		if signer.String() == required {
			foundSigner = true
		}
	}
	if !foundSigner {
		return proof, ErrArtifactCreationProofPending
	}
	if err = b.resolvePersistedLookups(ctx, tx); err != nil {
		return proof, err
	}
	keys := tx.Message.AccountKeys
	if len(receipt.PreLamports) != len(keys) || len(receipt.PostLamports) != len(keys) {
		return proof, errors.New("artifact creator receipt balance account vector invalid")
	}
	if len(tx.Message.GetAddressTableLookups()) == 0 && (len(receipt.LoadedWritable) != 0 || len(receipt.LoadedReadonly) != 0) {
		return proof, errors.New("legacy artifact receipt unexpectedly contains loaded addresses")
	}
	if len(tx.Message.GetAddressTableLookups()) > 0 {
		writable, readonly := 0, 0
		for _, lookup := range tx.Message.GetAddressTableLookups() {
			writable += len(lookup.WritableIndexes)
			readonly += len(lookup.ReadonlyIndexes)
		}
		if len(receipt.LoadedWritable) != writable || len(receipt.LoadedReadonly) != readonly {
			return proof, errors.New("artifact creator loaded-address vector invalid")
		}
		loaded := append(append([]string(nil), receipt.LoadedWritable...), receipt.LoadedReadonly...)
		for i, value := range loaded {
			if keys[len(keys)-len(loaded)+i].String() != value {
				return proof, errors.New("artifact creator loaded addresses differ from signed lookups")
			}
		}
	}
	account := target.Policy
	if role == ArtifactDelegation {
		account = target.RecurringDelegation
	} else if role != ArtifactPolicy {
		return proof, errors.New("unknown artifact role")
	}
	accountIndex := -1
	for i, key := range keys {
		if key.String() == account {
			accountIndex = i
		}
	}
	if accountIndex < 0 || receipt.PreLamports[accountIndex] != 0 || receipt.PostLamports[accountIndex] == 0 {
		return proof, ErrArtifactCreationProofPending
	}
	// The source builders create these artifacts with direct outer actions.
	// Inner receipt metadata is not signed intent and cannot establish a creator.
	for _, compiled := range tx.Message.Instructions {
		if int(compiled.ProgramIDIndex) >= len(keys) {
			return proof, errors.New("artifact creator program index out of range")
		}
		program := keys[compiled.ProgramIDIndex].String()
		if role == ArtifactPolicy && program != squadsProgramID || role == ArtifactDelegation && program != SubscriptionsProgramID {
			continue
		}
		var expected fleet.RouteInstruction
		if role == ArtifactPolicy {
			expected, err = b.proxy.BuildCanonicalSubscriptionPolicy(ctx, fleet.CanonicalSubscriptionPolicyRequest{Settings: target.Settings, RootAuthority: target.RootAuthority, Payer: keys[0].String(), DelegatedSigner: b.delegate.String(), PolicySeed: uint64(target.PolicySeed), Wallet: target.Wallet, Vault: target.Vault, MaxAmountPerPeriod: uint64(*target.MaxAmountPerPeriod)})
			if err != nil {
				return proof, err
			}
			if !canonicalSettingsCreatorData(compiled.Data, expected.Data) {
				continue
			}
		} else {
			if len(compiled.Data) != 49 || compiled.Data[0] != 2 {
				continue
			}
			expected = fleet.RouteInstruction{Program: SubscriptionsProgramID, Data: make([]byte, 49), Accounts: []fleet.InstructionAccount{{Address: target.Wallet, Signer: true, Writable: true}, {Address: target.SubscriptionAuthority}, {Address: target.RecurringDelegation, Writable: true}, {Address: target.Vault}, {Address: systemProgramZero}}}
			expected.Data[0] = 2
			for i, value := range []uint64{uint64(*target.Nonce), uint64(*target.MaxAmountPerPeriod), uint64(*target.PeriodLength), uint64(*target.StartTimestamp), uint64(*target.ExpiryTimestamp), binary.LittleEndian.Uint64(compiled.Data[41:49])} {
				binary.LittleEndian.PutUint64(expected.Data[1+i*8:], value)
			}
			if !bytes.Equal(compiled.Data, expected.Data) {
				continue
			}
		}
		if len(compiled.Accounts) != len(expected.Accounts) {
			continue
		}
		matches := true
		for i, index := range compiled.Accounts {
			if int(index) >= len(keys) {
				return proof, errors.New("artifact creator account index out of range")
			}
			a := expected.Accounts[i]
			writable, e := tx.Message.IsWritable(keys[index])
			if e != nil {
				return proof, e
			}
			if keys[index].String() != a.Address || a.Signer && !tx.Message.IsSigner(keys[index]) || a.Writable && !writable {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		digest := sha256.Sum256(compiled.Data)
		return VerifiedArtifactCreationProof{target: target, role: role, account: account, signature: receipt.Signature, slot: receipt.Slot, instructionSHA256: hex.EncodeToString(digest[:])}, nil
	}
	return proof, ErrArtifactCreationProofPending
}

func canonicalSettingsCreatorData(actual, expected []byte) bool {
	if len(expected) == 0 || expected[len(expected)-1] != 0 || len(actual) < len(expected) || !bytes.Equal(actual[:len(expected)-1], expected[:len(expected)-1]) {
		return false
	}
	memo := actual[len(expected)-1:]
	if memo[0] == 0 {
		return len(memo) == 1
	}
	if memo[0] != 1 || len(memo) < 5 {
		return false
	}
	size := binary.LittleEndian.Uint32(memo[1:5])
	return size <= 512 && int(size) == len(memo)-5 && utf8.Valid(memo[5:])
}

// ReconcileArtifacts is used under the existing control outbox's live lease;
// it does not create another queue or claim financial custody.
type ArtifactReconciler struct {
	Store  *Store
	Reader *ArtifactProofReader
}

func (r *ArtifactReconciler) ReconcileArtifacts(ctx context.Context, request ReconciliationRequest, owner string) error {
	if r.Store == nil || r.Reader == nil {
		return errors.New("artifact reconciliation requires store and proof reader")
	}
	target, err := r.Store.LoadArtifactTarget(ctx, request.TargetID)
	if err != nil {
		return err
	}
	if target == nil {
		return ErrArtifactCreationProofPending
	}
	for _, role := range []ArtifactRole{ArtifactPolicy, ArtifactDelegation} {
		if role == ArtifactPolicy && target.PolicySignature != nil && target.PolicyConfirmedSlot != nil || role == ArtifactDelegation && target.DelegationSignature != nil && target.DelegationConfirmedSlot != nil {
			continue
		}
		proof, err := r.Reader.FindCreationProof(ctx, *target, role, request.RequestedSlot)
		if err != nil {
			return fmt.Errorf("%s creator: %w", role, err)
		}
		if err = r.Store.BackfillArtifactCreationProof(ctx, request, owner, proof); err != nil {
			return err
		}
	}
	return nil
}
