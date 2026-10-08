package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression for the Go fleet path failing every cycle when the target
// obligation did not exist yet: loadFreshRoute required it ("required account
// <target obligation> is absent"), so a vault whose best market had no
// obligation never moved. Rust initializes it inside the route (ee8715ad,
// docs/plans/same-mint-obligation-ready-policy-verifier.md: withdraw, init and
// deposit in one transaction). The same read must also tolerate an unfunded
// vault PDA (rehearsal shadow-approved-account-diagnostic-20261004T0009Z): a
// Squads vault is a system account only while it holds lamports.
func TestFreshRouteSetsUpAMissingTargetObligationInsideTheRoute(t *testing.T) {
	const rent = uint64(23_942_400)
	fresh, r := loadFixtureFreshRoute(t, 2_000_000_000_000)
	in := fresh.input
	if !in.TargetObligationMissing || in.VaultRentTopUpLamports != rent || in.Payer != r.signer || in.SourceFarmUserMissing || in.TargetFarmUserMissing || fresh.evidence.Anchors.TargetCollateralRaw != 0 {
		t.Fatalf("setup facts not read from chain: %+v", in)
	}
	route, err := BuildSameMintRoute(in)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, ix := range route {
		step := ix.Step
		if ix.Protected {
			step += "*"
		}
		steps = append(steps, step)
	}
	want := []string{"kamino_refresh_reserve", "kamino_refresh_reserve", "kamino_refresh_obligation", "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2*", "system_transfer_vault_rent_top_up", "kamino_init_obligation*", "kamino_refresh_obligation", "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2*"}
	if len(steps) != len(want) {
		t.Fatalf("route %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("route %v, want %v", steps, want)
		}
	}
}

// loadFixtureFreshRoute reads a same-mint route moving 1e9 collateral units
// (the planned 1e9 liquidity) out of a source reserve with collateralSupply
// collateral against 2e12 liquidity, into a target with no obligation.
func loadFixtureFreshRoute(t *testing.T, collateralSupply uint64) (freshSameMint, *Revalidator) {
	t.Helper()
	vault := testIdentity(4)
	source := ReserveIdentity{Address: testIdentity(1), Market: testIdentity(40), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(2), Market: testIdentity(41), Mint: USDCMint}
	const amount, rent = uint64(1_000_000_000), uint64(23_942_400)
	chain := map[string]Account{}
	var sourcePosition decodedRoutePosition
	for i, identity := range []ReserveIdentity{source, target} {
		account := reserveFixture(identity, 1_000_000_000_000, 1_000_000_000_000)
		fixtureKey(t, account.Data, 408, tokenProgram)
		fixtureKey(t, account.Data, 2560, testIdentity(byte(90+i)))
		fixtureKey(t, account.Data, 160, testIdentity(byte(92+i)))
		fixtureKey(t, account.Data, 2600, testIdentity(byte(94+i)))
		binary.LittleEndian.PutUint64(account.Data[2592:2600], collateralSupply)
		chain[account.Address] = account
		decoded, err := decodeRouteReserve(account, vault)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sourcePosition = decoded
		}
	}
	obligation := Account{Address: sourcePosition.Obligation, Owner: KLendProgram, Lamports: 1, Data: make([]byte, obligationLength)}
	copy(obligation.Data, []byte{168, 206, 141, 106, 88, 76, 172, 167})
	fixtureKey(t, obligation.Data, 32, source.Market)
	fixtureKey(t, obligation.Data, 64, vault)
	fixtureKey(t, obligation.Data, 96, source.Address)
	binary.LittleEndian.PutUint64(obligation.Data[128:136], amount)
	chain[obligation.Address] = obligation
	ata := Account{Address: sourcePosition.Position.VaultLiquidityATA, Owner: tokenProgram, Lamports: 1, Data: make([]byte, 165)}
	fixtureKey(t, ata.Data, 0, USDCMint)
	fixtureKey(t, ata.Data, 32, vault)
	ata.Data[108] = 1
	chain[ata.Address] = ata
	policy := testIdentity(5)
	chain[policy] = Account{Address: policy, Owner: SquadsProgram, Lamports: 1, Data: []byte{1}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			t.Error(err)
		}
		var result any = int64(1000)
		switch call.Method {
		case "getMinimumBalanceForRentExemption":
			var size int
			_ = json.Unmarshal(call.Params[0], &size)
			if size != obligationLength {
				t.Errorf("rent asked for %d bytes", size)
			}
			result = rent
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(call.Params[0], &addresses)
			values := make([]any, len(addresses))
			for i, address := range addresses {
				// The target obligation and the vault are absent on chain.
				if account, ok := chain[address]; ok {
					values[i] = map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
				}
			}
			result = map[string]any{"context": map[string]any{"slot": 1000}, "value": values}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	r := &Revalidator{rpc: NewRPCClient(server.URL), slotDuration: 400 * time.Millisecond, signer: testIdentity(6)}
	lease := RevalidationLease{VaultPubkey: vault, SourceReserve: source.Address, TargetReserve: target.Address, LiquidityMint: USDCMint, PolicyAccount: policy, LiquidityAmountRaw: amount, PrincipalUSDMicros: int64(amount)}
	fresh, err := r.loadFreshRoute(context.Background(), lease)
	if err != nil {
		t.Fatalf("fresh read refused a route whose target obligation the route creates: %v", err)
	}
	return fresh, r
}

// A setup route pays rent, so it never goes to a fee-only payer (b1ad5b1a).
func TestSetupRouteIsNeverFeeOnly(t *testing.T) {
	r := &Revalidator{signer: testIdentity(6), feeOnlyPayers: []string{testIdentity(7)}}
	if payer := r.feePayer(context.Background(), "localnet", RevalidationLease{}, 1, false); payer != r.signer {
		t.Fatalf("setup route paid by %s", payer)
	}
}

// The payer funds exactly the vault's rent deficit, and never more than the
// retained cap.
func TestVaultRentTopUp(t *testing.T) {
	for _, c := range []struct{ rent, vault, want uint64 }{{23_942_400, 0, 23_942_400}, {23_942_400, 1_000_000, 22_942_400}, {23_942_400, 23_942_400, 0}, {23_942_400, 30_000_000, 0}} {
		if got, err := vaultRentTopUp(c.rent, c.vault); err != nil || got != c.want {
			t.Fatalf("top-up(%d,%d)=%d %v", c.rent, c.vault, got, err)
		}
	}
	if _, err := vaultRentTopUp(maxObligationRentLamports+1, 0); err == nil {
		t.Fatal("rent above the cap was funded")
	}
}
