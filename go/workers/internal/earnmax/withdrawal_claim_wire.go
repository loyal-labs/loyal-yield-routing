package earnmax

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"math"
	"time"
)

func qualifyProtectedWithdrawalClaim(ctx context.Context, client *chain.Client, c WithdrawalConsent, a *validatedWithdrawalAttempt) error {
	if client == nil || a == nil {
		return ErrWithdrawalClaimWireUnqualified
	}
	owner, err := solana.PublicKeyFromBase58(c.Owner)
	if err != nil {
		return err
	}
	settings, err := solana.PublicKeyFromBase58(c.Settings)
	if err != nil {
		return err
	}
	if err = c.Validate(owner, settings, time.Now()); err != nil {
		return err
	}
	if _, err = validateWithdrawalAttemptWire(c, a.wire, a.signature, a.blockhash, a.height, a.fee); err != nil {
		return err
	}
	genesis, err := client.GenesisHash(ctx)
	if err != nil {
		return err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return errors.New("claim chain is not mainnet")
	}
	tx, err := solana.TransactionFromBytes(a.wire)
	if err != nil {
		return err
	}
	reference, err := canonicalUint(c.ReferenceSlot)
	if err != nil {
		return err
	}
	authority, err := AuthorityOf(settings)
	if err != nil {
		return err
	}
	user, err := voltr.UserAccounts(Vault, LPMint, authority.key)
	if err != nil {
		return err
	}
	asset, err := spl.AssociatedTokenAddress(authority.key, AssetMint, solana.TokenProgramID)
	if err != nil {
		return err
	}
	seed, err := canonicalUint(c.PolicySeed)
	if err != nil || seed >= uint64(1<<53-1) {
		return errors.New("claim policy seed is unsupported")
	}
	policy, _, err := squads.PolicyAddress(settings, seed+1)
	if err != nil {
		return err
	}
	keys := []solana.PublicKey{settings, user.Receipt, asset, authority.key, policy}
	for _, lookup := range tx.Message.GetAddressTableLookups() {
		keys = append(keys, lookup.AccountKey)
	}
	slot, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentFinalized, reference)
	if err != nil {
		return err
	}
	if len(accounts) != len(keys) {
		return errors.New("claim account evidence incomplete")
	}
	state, err := squads.DecodeSettings(accounts[0])
	if err != nil {
		return err
	}
	current, err := authorizedWithdrawalPolicySeed(state, settings, owner)
	if err != nil {
		return err
	}
	if current != seed || accounts[4] != nil {
		return errors.New("claim settings generation or fresh policy changed")
	}
	if accounts[3] != nil && (accounts[3].Executable || accounts[3].Owner != solana.SystemProgramID || len(accounts[3].Data) != 0) {
		return errors.New("claim authority account is unusable")
	}
	if err = resolveWithdrawalClaimLookups(tx, slot, accounts[5:]); err != nil {
		return err
	}
	if err = checkProtectedWithdrawalClaim(tx, c, accounts[1], accounts[2]); err != nil {
		return err
	}
	if err = verifyProtectedWithdrawalGeneration(ctx, client, c, slot); err != nil {
		return err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return err
	}
	fee, feeSlot, err := client.QuotedMessageFee(ctx, message, rpc.CommitmentConfirmed, slot)
	if err != nil {
		return err
	}
	if fee != a.fee {
		return errors.New("claim fee differs from RPC quote")
	}
	valid, validSlot, err := client.BlockhashValid(ctx, tx.Message.RecentBlockhash, rpc.CommitmentConfirmed, feeSlot)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("claim blockhash expired")
	}
	_, trustedHeight, expirySlot, err := client.Blockhash(ctx, rpc.CommitmentConfirmed, validSlot)
	if err != nil {
		return err
	}
	if expirySlot < validSlot {
		return errors.New("claim expiry attestation is older than blockhash validation")
	}
	a.height = trustedHeight
	return nil
}

func verifyProtectedWithdrawalGeneration(ctx context.Context, source WithdrawalGenerationSource, c WithdrawalConsent, atLeast uint64) error {
	settings, err := solana.PublicKeyFromBase58(c.Settings)
	if err != nil {
		return err
	}
	authority, err := AuthorityOf(settings)
	if err != nil {
		return err
	}
	current, err := ReadWithdrawalRequest(ctx, source, authority.key, atLeast)
	if err != nil {
		return err
	}
	reference, err := canonicalUint(c.ReferenceSlot)
	if err != nil || current.WithdrawalGeneration != c.WithdrawalGeneration || reference < current.Slot || reference > atLeast {
		return errors.New("claim request generation or reference slot changed")
	}
	return verifyRequestInstruction(current.Execution, c, authority.key)
}

func resolveWithdrawalClaimLookups(tx *solana.Transaction, slot uint64, accounts []*chain.Account) error {
	lookups := tx.Message.GetAddressTableLookups()
	if len(lookups) != len(accounts) {
		return errors.New("claim lookup evidence incomplete")
	}
	tables := map[solana.PublicKey]solana.PublicKeySlice{}
	for i, lookup := range lookups {
		a := accounts[i]
		if _, ok := tables[lookup.AccountKey]; ok {
			return errors.New("claim repeats lookup table")
		}
		if a == nil || a.Key != lookup.AccountKey || a.Owner != solana.AddressLookupTableProgramID || a.Executable || len(a.Data) < 56 || (len(a.Data)-56)%32 != 0 || (len(a.Data)-56)/32 > 256 || binary.LittleEndian.Uint32(a.Data[:4]) != 1 || binary.LittleEndian.Uint64(a.Data[4:12]) != math.MaxUint64 || binary.LittleEndian.Uint64(a.Data[12:20]) >= slot {
			return errors.New("claim ALT is absent, inactive or not warmed")
		}
		addresses := solana.PublicKeySlice{}
		for at := 56; at < len(a.Data); at += 32 {
			addresses = append(addresses, solana.PublicKeyFromBytes(a.Data[at:at+32]))
		}
		seen := map[uint8]bool{}
		for _, index := range append(append([]uint8{}, lookup.WritableIndexes...), lookup.ReadonlyIndexes...) {
			if int(index) >= len(addresses) || seen[index] {
				return errors.New("claim ALT index repeated or out of range")
			}
			seen[index] = true
		}
		if len(seen) == 0 {
			return errors.New("claim ALT unused")
		}
		tables[lookup.AccountKey] = addresses
	}
	if len(lookups) > 0 {
		if err := tx.Message.SetAddressTables(tables); err != nil {
			return err
		}
		return tx.Message.ResolveLookups()
	}
	return nil
}

func claimInstruction(program solana.PublicKey, data []byte, keys ...solana.PublicKey) squads.Instruction {
	ix := squads.Instruction{ProgramID: program, Data: data}
	for _, key := range keys {
		ix.Accounts = append(ix.Accounts, solana.AccountMeta{PublicKey: key})
	}
	return ix
}
func sameClaimInstruction(a, b squads.Instruction) bool {
	if a.ProgramID != b.ProgramID || !bytes.Equal(a.Data, b.Data) || len(a.Accounts) != len(b.Accounts) {
		return false
	}
	for i := range a.Accounts {
		if a.Accounts[i].PublicKey != b.Accounts[i].PublicKey {
			return false
		}
	}
	return true
}
func claimSystemTransfer(from, to solana.PublicKey, amount uint64) squads.Instruction {
	return claimInstruction(solana.SystemProgramID, binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), amount), from, to)
}
func checkedClaimTransfer(source, destination, authority solana.PublicKey, amount uint64) squads.Instruction {
	data := binary.LittleEndian.AppendUint64([]byte{12}, amount)
	return claimInstruction(solana.TokenProgramID, append(data, 6), source, AssetMint, destination, authority)
}

func decodeWithdrawalInner(data []byte, tail []solana.AccountMeta) ([]squads.Instruction, error) {
	bad := errors.New("claim inner transaction is malformed or has unused accounts")
	if len(data) == 0 || data[0] == 0 || data[0] > 5 {
		return nil, bad
	}
	at := 1
	out := []squads.Instruction{}
	used := map[int]bool{}
	for range int(data[0]) {
		if at+2 > len(data) {
			return nil, bad
		}
		program, n := int(data[at]), int(data[at+1])
		at += 2
		if program >= len(tail) || at+n+2 > len(data) {
			return nil, bad
		}
		used[program] = true
		ix := squads.Instruction{ProgramID: tail[program].PublicKey}
		for _, index := range data[at : at+n] {
			if int(index) >= len(tail) {
				return nil, bad
			}
			used[int(index)] = true
			ix.Accounts = append(ix.Accounts, tail[index])
		}
		at += n
		size := int(binary.LittleEndian.Uint16(data[at : at+2]))
		at += 2
		if at+size > len(data) {
			return nil, bad
		}
		ix.Data = bytes.Clone(data[at : at+size])
		at += size
		out = append(out, ix)
	}
	if at != len(data) || len(used) != len(tail) {
		return nil, bad
	}
	seen := map[solana.PublicKey]bool{}
	for _, meta := range tail {
		if seen[meta.PublicKey] {
			return nil, bad
		}
		seen[meta.PublicKey] = true
	}
	return out, nil
}
func decodeOwnerWithdrawalSync(ix squads.Instruction, settings, owner solana.PublicKey) ([]squads.Instruction, error) {
	d := ix.Data
	if ix.ProgramID != squads.ProgramID || len(ix.Accounts) < 3 || ix.Accounts[0].PublicKey != settings || ix.Accounts[1].PublicKey != squads.ProgramID || ix.Accounts[2].PublicKey != owner || len(d) < 16 || !bytes.Equal(d[:8], squads.ExecuteTransactionSyncV2Discriminator[:]) || !bytes.Equal(d[8:11], []byte{AccountIndex, 1, 0}) || int(binary.LittleEndian.Uint32(d[11:15])) != len(d)-15 {
		return nil, errors.New("claim owner sync envelope is invalid")
	}
	return decodeWithdrawalInner(d[15:], ix.Accounts[3:])
}

// checkProtectedWithdrawalClaim accepts exactly the official claim: the owner
// pays for both USDC ATAs, then inside the vault Voltr pays the authority ATA
// and the authority moves the ATA's evidenced balance plus the floor to the
// owner, so the floor is met only by new proceeds and donated USDC can only add.
func checkProtectedWithdrawalClaim(tx *solana.Transaction, c WithdrawalConsent, receiptAccount, assetAccount *chain.Account) error {
	bad := errors.New("claim wire does not match approved protected operation")
	if tx.Message.GetVersion() != solana.MessageVersionV0 && tx.Message.GetVersion() != solana.MessageVersionLegacy {
		return bad
	}
	owner, err := solana.PublicKeyFromBase58(c.Owner)
	if err != nil {
		return err
	}
	settings, err := solana.PublicKeyFromBase58(c.Settings)
	if err != nil {
		return err
	}
	authority, err := AuthorityOf(settings)
	if err != nil {
		return err
	}
	seed, err := canonicalUint(c.PolicySeed)
	if err != nil || seed >= uint64(1<<53-1) {
		return bad
	}
	floor, err := canonicalUint(c.MinClaimUSDC)
	if err != nil || floor == 0 {
		return bad
	}
	receipt, err := voltr.DecodeWithdrawalReceipt(receiptAccount)
	if err != nil {
		return err
	}
	user, err := voltr.UserAccounts(Vault, LPMint, authority.key)
	if err != nil {
		return err
	}
	lp, err := canonicalUint(c.LPRaw)
	if err != nil || receiptAccount.Key != user.Receipt || receipt.Vault != Vault || receipt.User != authority.key || receipt.LPEscrowedRaw != lp {
		return bad
	}
	source, err := spl.AssociatedTokenAddress(authority.key, AssetMint, solana.TokenProgramID)
	if err != nil {
		return err
	}
	destination, err := spl.AssociatedTokenAddress(owner, AssetMint, solana.TokenProgramID)
	if err != nil {
		return err
	}
	policy, _, err := squads.PolicyAddress(settings, seed+1)
	if err != nil {
		return err
	}
	protocol, _, err := solana.FindProgramAddress([][]byte{[]byte("protocol")}, voltr.ProgramID)
	if err != nil {
		return err
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, LPMint, solana.TokenProgramID)
	if err != nil {
		return err
	}
	guard := claimSystemTransfer(authority.key, owner, 0)
	guard.Accounts = append(guard.Accounts, solana.AccountMeta{PublicKey: user.Receipt})
	equalBytes := func(value []byte) squads.DataConstraintView {
		return squads.DataConstraintView{DataOffset: 0, DataValue: squads.DataValueView{Kind: squads.ValueBytes, Bytes: value}, Operator: squads.OpEquals}
	}
	receiptState := equalBytes(receiptAccount.Data[72:104])
	receiptState.DataOffset = 72
	constraints := []squads.InstructionConstraintView{{ProgramID: solana.SystemProgramID, AccountConstraints: []squads.AccountConstraintView{{AccountIndex: 2, Owner: &voltr.ProgramID, AccountData: []squads.DataConstraintView{receiptState}}}, DataConstraints: []squads.DataConstraintView{equalBytes(guard.Data)}}}
	create, err := (squads.PolicyApply{Settings: settings, RentPayer: owner, Signer: owner, Delegate: owner, Seed: seed + 1, VaultIndex: AccountIndex, Constraints: constraints}).Instruction()
	if err != nil {
		return err
	}
	remove, err := (squads.PolicyApply{Settings: settings, RentPayer: owner, Signer: owner, Replace: []solana.PublicKey{policy}}).Instruction()
	if err != nil {
		return err
	}
	outer := []squads.Instruction{
		claimInstruction(solana.SPLAssociatedTokenAccountProgramID, []byte{1}, owner, destination, owner, AssetMint, solana.SystemProgramID, solana.TokenProgramID),
		claimInstruction(solana.SPLAssociatedTokenAccountProgramID, []byte{1}, owner, source, authority.key, AssetMint, solana.SystemProgramID, solana.TokenProgramID),
		create,
	}
	baseline := uint64(0)
	if assetAccount != nil {
		if assetAccount.Key != source || len(assetAccount.Data) != 165 {
			return bad
		}
		token, e := spl.DecodeTokenAccount(assetAccount)
		if e != nil || token.Program != solana.TokenProgramID || token.Mint != AssetMint || token.Owner != authority.key || token.Frozen || token.IsNative || token.HasCloseAuthority || token.Delegate != nil {
			return bad
		}
		baseline = token.Amount
	}
	if baseline > math.MaxUint64-floor {
		return bad
	}
	expected := []squads.Instruction{
		claimInstruction(voltr.ProgramID, voltr.WithdrawVaultDiscriminator[:], authority.key, protocol, Vault, AssetMint, LPMint, escrow, IdleATA, IdleAuth, source, user.Receipt, solana.TokenProgramID, solana.TokenProgramID, solana.SystemProgramID),
		checkedClaimTransfer(source, destination, authority.key, baseline+floor),
	}
	keys, err := tx.Message.GetAllKeys()
	if err != nil || len(keys) > 256 {
		return bad
	}
	unique := map[solana.PublicKey]bool{}
	for _, key := range keys {
		if unique[key] {
			return bad
		}
		unique[key] = true
	}
	actual := []squads.Instruction{}
	tags := map[byte]bool{}
	used := map[solana.PublicKey]bool{}
	for _, compiled := range tx.Message.Instructions {
		if int(compiled.ProgramIDIndex) >= len(keys) {
			return bad
		}
		metas, e := compiled.ResolveInstructionAccounts(&tx.Message)
		if e != nil {
			return bad
		}
		ix := squads.Instruction{ProgramID: keys[compiled.ProgramIDIndex], Data: compiled.Data}
		used[ix.ProgramID] = true
		for _, m := range metas {
			ix.Accounts = append(ix.Accounts, *m)
			used[m.PublicKey] = true
		}
		if ix.ProgramID == solana.ComputeBudget {
			if len(ix.Accounts) != 0 || len(ix.Data) < 1 || tags[ix.Data[0]] {
				return bad
			}
			tags[ix.Data[0]] = true
			switch ix.Data[0] {
			case 1:
				if len(ix.Data) != 5 {
					return bad
				}
				n := binary.LittleEndian.Uint32(ix.Data[1:])
				if n < 32768 || n > 256000 || n%1024 != 0 {
					return bad
				}
			case 2:
				if len(ix.Data) != 5 || binary.LittleEndian.Uint32(ix.Data[1:]) == 0 || binary.LittleEndian.Uint32(ix.Data[1:]) > 1_400_000 {
					return bad
				}
			case 3:
				if len(ix.Data) != 9 {
					return bad
				}
			default:
				return bad
			}
			continue
		}
		actual = append(actual, ix)
	}
	if len(actual) != len(outer)+3 {
		return bad
	}
	for i := range outer {
		if !sameClaimInstruction(actual[i], outer[i]) {
			return fmt.Errorf("%w: outer instruction %d", bad, i)
		}
	}
	check := actual[len(outer)]
	decoded, err := squads.DecodeExecuteTransactionSyncV2(check)
	if err != nil || decoded.Policy != policy || decoded.Signer != owner || decoded.AccountIndex != AccountIndex || !bytes.Equal(decoded.ConstraintIndexes, []byte{0}) || len(decoded.Inner) != 1 || !sameClaimInstruction(decoded.Inner[0], guard) {
		return fmt.Errorf("%w: generation check", bad)
	}
	if len(check.Data) < 24 {
		return bad
	}
	if _, err = decodeWithdrawalInner(check.Data[24:], check.Accounts[3:]); err != nil {
		return err
	}
	inner, err := decodeOwnerWithdrawalSync(actual[len(outer)+1], settings, owner)
	if err != nil || len(inner) != len(expected) {
		return fmt.Errorf("%w: owner execution", bad)
	}
	// The client reads the balance after the finalized evidence; a larger
	// moved amount only raises the floor, so USDC donated between the two reads
	// cannot be used to refuse the claim.
	if d := inner[1].Data; len(d) == 10 && d[0] == 12 {
		if moved := binary.LittleEndian.Uint64(d[1:9]); moved > baseline+floor {
			expected[1] = checkedClaimTransfer(source, destination, authority.key, moved)
		}
	}
	for i := range inner {
		if !sameClaimInstruction(inner[i], expected[i]) {
			return fmt.Errorf("%w: inner instruction %d (accounts %d, expected %d; data %x, expected %x)", bad, i, len(inner[i].Accounts), len(expected[i].Accounts), inner[i].Data, expected[i].Data)
		}
	}
	if !sameClaimInstruction(actual[len(actual)-1], remove) {
		return fmt.Errorf("%w: cleanup", bad)
	}
	if len(used) != len(keys) {
		return errors.New("claim has unused account keys")
	}
	writable := map[solana.PublicKey]bool{owner: true, settings: true, policy: true, authority.key: true, source: true, destination: true, Vault: true, LPMint: true, escrow: true, IdleATA: true, IdleAuth: true, user.Receipt: true}
	for i, key := range keys {
		isSigner := i < int(tx.Message.Header.NumRequiredSignatures)
		isWritable, e := tx.Message.IsWritable(key)
		if e != nil || isSigner != (key == owner) || isWritable != writable[key] {
			return errors.New("claim account privileges differ from canonical operation")
		}
	}
	return nil
}
