package autodeposit

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPreparedAttemptRejectsWireHashMismatchBeforeCustody(t *testing.T) {
	store := &Store{}
	_, err := store.PersistPreparedAttempt(context.Background(), PreparedAttempt{
		AmountRaw: 1, SourcePreBalanceRaw: 1, Signature: "persisted-signature",
		SignedTransactionBase64: base64.StdEncoding.EncodeToString([]byte("wire")),
		SignedTransactionSHA256: strings.Repeat("a", 64),
	}, "owner")
	if err == nil || !strings.Contains(err.Error(), "wire hash mismatch") {
		t.Fatalf("wire mismatch reached custody: %v", err)
	}
}
