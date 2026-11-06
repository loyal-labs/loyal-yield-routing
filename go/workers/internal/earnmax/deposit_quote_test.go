package earnmax

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const depositQuoteGolden = "../../../../crates/squads-test-harness/fixtures/voltr/deposit-quote-go.json"

func capturedDepositState(t *testing.T) depositState {
	t.Helper()
	var captured struct {
		Addresses []string
		Result    struct{ Value []struct{ Data []string } }
	}
	raw, err := os.ReadFile(withdrawalClaimFixtures + "voltr-accounts.json")
	if err == nil {
		err = json.Unmarshal(raw, &captured)
	}
	if err != nil {
		t.Fatal(err)
	}
	data := func(address solana.PublicKey) []byte {
		for i, a := range captured.Addresses {
			if a == address.String() {
				b, err := base64.StdEncoding.DecodeString(captured.Result.Value[i].Data[0])
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
		}
		t.Fatalf("%s not captured", address)
		return nil
	}
	vaultData, mint := data(Vault), data(LPMint)
	vault, err := voltr.DecodeVault(&chain.Account{Key: Vault, Owner: voltr.ProgramID, Lamports: 1, Data: vaultData})
	if err != nil {
		t.Fatal(err)
	}
	authority := solana.PublicKey{1: 7, 31: 9}
	lpATA, err := spl.AssociatedTokenAddress(authority, LPMint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	return depositState{Authority: authority, LPATA: lpATA, VaultData: vaultData, Vault: vault, SupplyRaw: binary.LittleEndian.Uint64(mint[36:44]), AssetDecimals: 6, LPDecimals: mint[44]}
}

func TestDepositQuoteMatchesCapturedMintAndClientContract(t *testing.T) {
	st := capturedDepositState(t)
	now := time.Unix(1793920731, 0)
	quote, err := newDepositQuote(st, 250_000_000, now, now.Add(30*time.Second), 465888518, 465888550)
	if err != nil {
		t.Fatal(err)
	}
	if quote.LPMinRaw != "671669" {
		t.Fatalf("quoted minimum %s, captured Voltr minted 671669", quote.LPMinRaw)
	}
	quote.CertificateID = "golden"
	body, err := json.MarshalIndent(quote, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if os.Getenv("UPDATE_DEPOSIT_QUOTE_GOLDEN") == "1" {
		if err := os.WriteFile(depositQuoteGolden, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(depositQuoteGolden)
	if err != nil || !bytes.Equal(golden, body) {
		t.Fatalf("Go quote differs from the golden the TS client parses; rerun with UPDATE_DEPOSIT_QUOTE_GOLDEN=1 (err %v)", err)
	}
}

func TestDepositQuoteBoundsWidenAsManagementFeeAccrues(t *testing.T) {
	st := capturedDepositState(t)
	now := time.Unix(1793920731, 0)
	st.Vault.ManagerManagementFeeBPS = 500
	st.Vault.LastManagementFeeUpdateUnix = uint64(now.Add(-30 * 24 * time.Hour).Unix())
	quote, err := newDepositQuote(st, 10_000_000_000_000, now, now.Add(30*time.Second), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	lo, err := st.Vault.DepositLP(st.SupplyRaw, 6, st.LPDecimals, 10_000_000_000_000, now)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := st.Vault.DepositLP(st.SupplyRaw, 6, st.LPDecimals, 10_000_000_000_000, now.Add(31*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !(lo < hi) || quote.LPMinRaw != strconv.FormatUint(lo, 10) || quote.LPMaxRaw != strconv.FormatUint(hi, 10) {
		t.Fatalf("bounds [%s,%s], mint at observation %d and at expiry %d", quote.LPMinRaw, quote.LPMaxRaw, lo, hi)
	}
}

// Certified NAV above the published value mints extra LP to the depositor;
// only the issuance fee left in the vault pays holders for it.
func TestDepositWaitsWhileCertifiedNAVExceedsPublishedValuePastIssuanceFee(t *testing.T) {
	v := capturedDepositState(t).Vault
	nav := v.TotalValueRaw + v.TotalValueRaw*(uint64(v.IssuanceFeeBPS)+5)/10_000
	if depositPricePublished(nav, v) {
		t.Fatalf("deposit priced %d bps below certified NAV on a %d bps issuance fee", v.IssuanceFeeBPS+5, v.IssuanceFeeBPS)
	}
}
