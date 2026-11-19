package earnmax

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestReferralCapitalEvidenceRecordedReceipts(t *testing.T) {
	const name = "cancel_across_gap"
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var f rpcFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	reader := fixtureReader(t, name)
	want := map[string]string{"3c8QnE7hh": "deposit", "4SZL6f7UA": "request", "zgk1RwmZE": "claim"}
	seen := 0
	for sig := range f.Transactions {
		expected, ok := want[sig[:9]]
		if !ok {
			continue
		}
		s := solana.MustSignatureFromBase58(sig)
		ex, err := reader.chain.Execution(context.Background(), s, rpc.CommitmentFinalized)
		if err != nil {
			t.Fatal(err)
		}
		m, err := CapitalMovementAt(ex, s, recordedAuthority)
		if err != nil || m == nil || len(m.Events) != 1 || m.Events[0].Kind != expected || m.Events[0].UnixTime != ex.BlockTime.Unix() {
			t.Fatalf("%s: %#v, %v", expected, m, err)
		}
		seen++
		if expected == "deposit" {
			if m.Events[0].NetAssetNumerator == "" || m.Events[0].NetAssetDenominator == 0 || m.FreeAfter-m.FreeBefore != m.Events[0].LP {
				t.Fatal("missing actual net LP value")
			}
		}
		// Another program can log identical Anchor data. Its inner logs are not a receipt.
		for i, l := range ex.Logs {
			ex.Logs[i] = strings.ReplaceAll(l, voltr.ProgramID.String(), solana.SystemProgramID.String())
		}
		if _, err := CapitalMovementAt(ex, s, recordedAuthority); err == nil {
			t.Fatal("accepted the wrong emitting program")
		}
	}
	if seen != len(want) {
		t.Fatalf("exercised %d receipts, want %d", seen, len(want))
	}
}

func TestReferralCapitalCancellationUsesRefundAndBurn(t *testing.T) {
	user, err := voltr.UserAccounts(Vault, LPMint, recordedAuthority.Key())
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 160)
	tag := sha256.Sum256([]byte("event:CancelRequestWithdrawVaultEvent"))
	copy(raw, tag[:8])
	copy(raw[8:], Vault[:])
	copy(raw[40:], recordedAuthority.key[:])
	copy(raw[72:], user.Receipt[:])
	binary.LittleEndian.PutUint64(raw[104:], 80)
	binary.LittleEndian.PutUint64(raw[112:], 20)
	binary.LittleEndian.PutUint64(raw[152:], 5678)
	e, ok, err := capitalEvent(raw, recordedAuthority.Key(), user.Receipt)
	if err != nil || !ok || e.Kind != "cancel" || e.LP != 80 || e.BurnedLP != 20 || e.UnixTime != 5678 {
		t.Fatalf("%+v, %v", e, err)
	}
	raw[72] ^= 1
	if _, ok, _ := capitalEvent(raw, recordedAuthority.Key(), user.Receipt); ok {
		t.Fatal("wrong receipt accepted")
	}
}
