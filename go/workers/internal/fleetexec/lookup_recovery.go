package fleetexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"

	sdk "github.com/gagliardetto/solana-go"
)

// lookupProof can only be produced by verifying the actual packet, receipt and
// coherent account effects. The store never accepts a caller's terminal flag.
type lookupProof struct {
	binding                     string
	state                       LookupAttemptState
	readbackSlot, finalizedSlot int64
	readback, receipt           json.RawMessage
}
type lookupRecovery struct {
	proof *lookupProof
	wait  string
}

func lookupSameAddresses(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func lookupCloseReady(snapshot LookupSnapshot, expected uint64) bool {
	if snapshot.Absent || snapshot.DeactivationSlot != expected || expected == math.MaxUint64 || snapshot.Slot <= 0 || uint64(snapshot.Slot) <= expected || len(snapshot.SlotHashes) == 0 {
		return false
	}
	for _, slot := range snapshot.SlotHashes {
		if slot == expected {
			return false
		}
	}
	return true
}
func lookupUnchanged(intent LookupIntent, snapshot LookupSnapshot) bool {
	if intent.Kind == LookupCreate || intent.Kind == LookupRollover {
		return snapshot.Absent
	}
	if snapshot.Absent || snapshot.Owner != lookupProgram || snapshot.Authority != intent.Authority || !lookupSameAddresses(snapshot.Addresses, intent.Prefix) {
		return false
	}
	if intent.Kind == LookupClose {
		return intent.ExpectedDeactivationSlot != nil && snapshot.DeactivationSlot == *intent.ExpectedDeactivationSlot
	}
	return snapshot.DeactivationSlot == math.MaxUint64
}
func lookupReadback(snapshot LookupSnapshot, proof string) json.RawMessage {
	h := sha256.Sum256(snapshot.Data)
	data, _ := json.Marshal(map[string]any{"address": snapshot.Address, "owner": snapshot.Owner, "data_hash": hex.EncodeToString(h[:]), "observed_slot": snapshot.Slot, "proof": proof, "absent": snapshot.Absent, "lamports": snapshot.Lamports, "authority": snapshot.Authority, "ordered_addresses": snapshot.Addresses, "deactivation_slot": snapshot.DeactivationSlot, "last_extended_slot": snapshot.LastExtendedSlot, "slot_hashes": snapshot.SlotHashes})
	return data
}
func lookupEqualJSON(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var av, bv any
	return json.Unmarshal([]byte(a), &av) == nil && json.Unmarshal([]byte(b), &bv) == nil && reflect.DeepEqual(av, bv)
}
func lookupReceiptProof(attempt LookupAttempt, receipt *LookupReceipt) (json.RawMessage, error) {
	if receipt == nil || receipt.Slot <= 0 || receipt.Signature != attempt.Wire.TransactionSignature || !bytes.Equal(receipt.Wire, attempt.Wire.SignedTransaction) {
		return nil, errors.New("lookup finalized receipt differs from owned packet")
	}
	tx, err := sdk.TransactionFromBytes(receipt.Wire)
	if err != nil {
		return nil, err
	}
	if len(receipt.Addresses) != len(tx.Message.AccountKeys) || len(receipt.PreLamports) != len(receipt.Addresses) || len(receipt.PostLamports) != len(receipt.Addresses) {
		return nil, errors.New("lookup receipt balance indices are incomplete")
	}
	table, payer := -1, -1
	for i, key := range tx.Message.AccountKeys {
		if receipt.Addresses[i] != key.String() {
			return nil, errors.New("lookup receipt balance account differs from packet")
		}
		if key.String() == attempt.Intent.TableAddress {
			table = i
		}
		if key.String() == attempt.Intent.Payer {
			payer = i
		}
	}
	if table < 0 || payer < 0 || table == payer {
		return nil, errors.New("lookup table/refund account aliases payer or is absent")
	}
	pre, post := receipt.PreLamports[table], receipt.PostLamports[table]
	payerPre, payerPost := receipt.PreLamports[payer], receipt.PostLamports[payer]
	if receipt.Err != "" {
		if !json.Valid([]byte(receipt.Err)) || receipt.Err == "null" || pre != post || payerPre < receipt.FeeLamports || payerPost != payerPre-receipt.FeeLamports {
			return nil, errors.New("lookup failed receipt has unexplained SOL effects")
		}
	} else if attempt.Intent.Kind == LookupClose {
		if pre == 0 || post != 0 || payerPre > math.MaxUint64-pre || payerPre+pre < receipt.FeeLamports || payerPost != payerPre+pre-receipt.FeeLamports {
			return nil, errors.New("lookup close receipt does not refund exact table rent")
		}
	} else {
		if post < pre || post-pre > math.MaxUint64-receipt.FeeLamports || payerPre < post-pre+receipt.FeeLamports || payerPost != payerPre-(post-pre+receipt.FeeLamports) {
			return nil, errors.New("lookup receipt rent/fee movement is unexplained")
		}
		if (attempt.Intent.Kind == LookupCreate || attempt.Intent.Kind == LookupRollover) && (pre != 0 || post == 0) {
			return nil, errors.New("lookup create did not fund a new table")
		}
		if attempt.Intent.Kind == LookupDeactivate && pre != post {
			return nil, errors.New("lookup deactivation unexpectedly moved rent")
		}
	}
	var chainErr any
	if receipt.Err != "" {
		if err := json.Unmarshal([]byte(receipt.Err), &chainErr); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(map[string]any{"signature": receipt.Signature, "message_hash": attempt.Wire.MessageHash, "signed_transaction_sha256": attempt.Wire.SignedTransactionHash, "slot": receipt.Slot, "commitment": "finalized", "err": chainErr, "fee_lamports": receipt.FeeLamports, "table_pre_lamports": pre, "table_post_lamports": post, "payer_pre_lamports": payerPre, "payer_post_lamports": payerPost})
	return data, err
}
func recoverLookup(attempt LookupAttempt, status SignatureStatus, receipt *LookupReceipt, snapshot LookupSnapshot) (lookupRecovery, error) {
	if err := proveLookupWire(attempt.Intent, attempt.Wire); err != nil {
		return lookupRecovery{}, err
	}
	if snapshot.Address != attempt.Intent.TableAddress || snapshot.Slot <= 0 {
		return lookupRecovery{}, errors.New("lookup readback identity/frontier mismatch")
	}
	if !status.Found || !status.Finalized {
		return lookupRecovery{wait: "owned signature has no finalized receipt"}, nil
	}
	if receipt == nil {
		return lookupRecovery{wait: "finalized packet history unavailable"}, nil
	}
	if status.Slot != receipt.Slot || !lookupEqualJSON(status.Err, receipt.Err) {
		return lookupRecovery{}, errors.New("lookup signature and receipt disagree")
	}
	receiptProof, err := lookupReceiptProof(attempt, receipt)
	if err != nil {
		return lookupRecovery{}, err
	}
	if snapshot.Slot < receipt.Slot {
		return lookupRecovery{wait: "readback is below finalized receipt"}, nil
	}
	proof := &lookupProof{binding: lookupProofBinding(attempt), readbackSlot: snapshot.Slot, finalizedSlot: receipt.Slot, receipt: receiptProof}
	if receipt.Err != "" {
		if !lookupUnchanged(attempt.Intent, snapshot) {
			return lookupRecovery{}, errors.New("lookup failed receipt does not have unchanged table proof")
		}
		proof.state = LookupFailed
		proof.readback = lookupReadback(snapshot, "failed_receipt_no_effect")
		return lookupRecovery{proof: proof}, nil
	}
	intent := attempt.Intent
	if intent.Kind == LookupClose {
		if !snapshot.Absent {
			return lookupRecovery{}, errors.New("lookup finalized close still has a table")
		}
	} else {
		expected := append(append([]string(nil), intent.Prefix...), intent.Extension...)
		if snapshot.Absent || snapshot.Owner != lookupProgram || snapshot.Authority != intent.Authority || !lookupSameAddresses(snapshot.Addresses, expected) {
			return lookupRecovery{}, errors.New("lookup finalized mutation has owner/authority/ordered membership drift")
		}
		switch intent.Kind {
		case LookupCreate, LookupRollover, LookupExtend:
			if snapshot.DeactivationSlot != math.MaxUint64 || (len(intent.Extension) > 0 && snapshot.LastExtendedSlot != uint64(receipt.Slot)) || (len(intent.Extension) == 0 && snapshot.LastExtendedSlot != 0) {
				return lookupRecovery{}, errors.New("lookup growth metadata differs from receipt")
			}
			if snapshot.Slot <= int64(snapshot.LastExtendedSlot) {
				return lookupRecovery{wait: "lookup membership is not warmed in finalized bank"}, nil
			}
		case LookupDeactivate:
			if snapshot.DeactivationSlot != uint64(receipt.Slot) {
				return lookupRecovery{}, errors.New("lookup deactivation slot differs from receipt")
			}
		default:
			return lookupRecovery{}, fmt.Errorf("lookup signed recovery cannot verify kind %q", intent.Kind)
		}
	}
	proof.state = LookupReconciled
	proof.readback = lookupReadback(snapshot, "exact_finalized_effect")
	return lookupRecovery{proof: proof}, nil
}

func lookupProofBinding(attempt LookupAttempt) string {
	intent := attempt.Intent
	intent.Prefix = append([]string{}, intent.Prefix...)
	intent.Extension = append([]string{}, intent.Extension...)
	encoded, _ := json.Marshal(struct {
		SigningSlot         int64
		Intent              LookupIntent
		Signature, WireHash string
	}{attempt.SigningContextSlot, intent, attempt.Wire.TransactionSignature, attempt.Wire.SignedTransactionHash})
	h := sha256.Sum256(encoded)
	return hex.EncodeToString(h[:])
}
