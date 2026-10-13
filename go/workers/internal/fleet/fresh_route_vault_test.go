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

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Regression for the Go fleet path failing every cycle when the target
// obligation did not exist yet: loadFreshRoute required it ("required account
// <target obligation> is absent"), so a vault whose best market had no
// obligation never moved. Rust initializes it inside the route (ee8715ad,
// docs/plans/same-mint-obligation-ready-policy-verifier.md: withdraw, init and
// deposit in one transaction). The same read must also tolerate an unfunded
// vault PDA (rehearsal shadow-approved-account-diagnostic-20261004T0009Z): a
// Squads vault is a system account only while it holds lamports.
//
// The withdraw empties the source obligation, so KLend closes it and pays its
// rent to the vault before init_obligation: the payer tops up nothing.
func TestFreshRouteSetsUpAMissingTargetObligationInsideTheRoute(t *testing.T) {
	fresh, r := loadFixtureFreshRoute(t, 2_000_000_000_000)
	in := fresh.input
	if !in.TargetObligationMissing || in.VaultRentTopUpLamports != 0 || in.Payer != r.signer || in.SourceFarmUserMissing || in.TargetFarmUserMissing || fresh.evidence.Anchors.TargetCollateralRaw != 0 {
		t.Fatalf("setup facts not read from chain: %+v", in)
	}
	// The route is observed at its own read's context slot (the second read).
	if fresh.evidence.Slot != 1001 || fresh.evidence.Anchors.MinimumSlot != 1001 {
		t.Fatalf("observation slot %d, want the read's own 1001", fresh.evidence.Slot)
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
	want := []string{"kamino_refresh_reserve", "kamino_refresh_reserve", "kamino_refresh_obligation", "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2*", "kamino_init_obligation*", "kamino_refresh_obligation", "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2*"}
	if len(steps) != len(want) {
		t.Fatalf("route %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("route %v, want %v", steps, want)
		}
	}
}

// The payer funds only what the vault lacks once the source obligation's rent
// is back: all of it when the withdraw leaves the obligation open, the
// difference when the closed obligation held less than the new one's rent.
func TestFreshRouteTopsUpWhatTheClosedSourceDoesNotReturn(t *testing.T) {
	const rent = uint64(23_942_400)
	for name, c := range map[string]struct {
		obligation func(*chain.Account)
		want       uint64
	}{
		"another deposit keeps it open": {func(a *chain.Account) {
			fixtureKey(t, a.Data, 96+136, testIdentity(9))
			binary.LittleEndian.PutUint64(a.Data[96+136+32:], 1)
		}, rent},
		"a borrow keeps it open": {func(a *chain.Account) { fixtureKey(t, a.Data, 1208, testIdentity(9)) }, rent},
		"closed below the rent":  {func(a *chain.Account) { a.Lamports = rent - 1_000 }, 1_000},
		"closed leaving dust":    {func(a *chain.Account) { a.Lamports = rent + 1_000 }, emptyAccountRent - 1_000},
	} {
		t.Run(name, func(t *testing.T) {
			r, lease := fixtureFreshRouteObligation(t, 2_000_000_000_000, nil, c.obligation)
			fresh, err := r.loadFreshRoute(context.Background(), lease)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.input.VaultRentTopUpLamports != c.want {
				t.Fatalf("top-up %d, want %d", fresh.input.VaultRentTopUpLamports, c.want)
			}
		})
	}
}

// loadFixtureFreshRoute reads a same-mint route moving 1e9 collateral units
// (the planned 1e9 liquidity) out of a source reserve with collateralSupply
// collateral against 2e12 liquidity, into a target with no obligation.
func loadFixtureFreshRoute(t *testing.T, collateralSupply uint64) (freshSameMint, *Revalidator) {
	t.Helper()
	r, lease := fixtureFreshRoute(t, collateralSupply, nil)
	fresh, err := r.loadFreshRoute(context.Background(), lease)
	if err != nil {
		t.Fatalf("fresh read refused a route whose target obligation the route creates: %v", err)
	}
	return fresh, r
}

// fixtureFreshRoute serves that chain over RPC and returns a revalidator
// reading it with the route's lease. mutate, when set, edits the source and
// target reserve accounts before they are served.
func fixtureFreshRoute(t *testing.T, collateralSupply uint64, mutate func(source, target *chain.Account)) (*Revalidator, RevalidationLease) {
	t.Helper()
	return fixtureFreshRouteObligation(t, collateralSupply, mutate, nil)
}

// fixtureFreshRouteObligation is fixtureFreshRoute whose source obligation,
// which holds its rent and only the moved deposit, obligation may edit.
func fixtureFreshRouteObligation(t *testing.T, collateralSupply uint64, mutate func(source, target *chain.Account), obligation func(*chain.Account)) (*Revalidator, RevalidationLease) {
	t.Helper()
	vault := testIdentity(4)
	source := ReserveIdentity{Address: testIdentity(1), Market: testIdentity(40), Mint: USDCMint}
	target := ReserveIdentity{Address: testIdentity(2), Market: testIdentity(41), Mint: USDCMint}
	const amount, rent = uint64(1_000_000_000), uint64(23_942_400)
	held := map[string]chain.Account{}
	var sourcePosition decodedRoutePosition
	for i, identity := range []ReserveIdentity{source, target} {
		account := reserveFixture(identity, 1_000_000_000_000, 1_000_000_000_000)
		fixtureKey(t, account.Data, 408, tokenProgram)
		fixtureKey(t, account.Data, 2560, testIdentity(byte(90+i)))
		fixtureKey(t, account.Data, 160, testIdentity(byte(92+i)))
		fixtureKey(t, account.Data, 2600, testIdentity(byte(94+i)))
		binary.LittleEndian.PutUint64(account.Data[2592:2600], collateralSupply)
		held[identity.Address] = account
		decoded, err := decodeRouteReserve(&account, vault)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sourcePosition = decoded
		}
	}
	if mutate != nil {
		sourceAccount, targetAccount := held[source.Address], held[target.Address]
		mutate(&sourceAccount, &targetAccount)
		held[source.Address], held[target.Address] = sourceAccount, targetAccount
	}
	sourceObligation := fixtureAccount(sourcePosition.Obligation, kamino.ProgramID.String(), rent, make([]byte, kamino.ObligationSize))
	copy(sourceObligation.Data, kamino.ObligationDiscriminator[:])
	fixtureKey(t, sourceObligation.Data, 32, source.Market)
	fixtureKey(t, sourceObligation.Data, 64, vault)
	fixtureKey(t, sourceObligation.Data, 96, source.Address)
	binary.LittleEndian.PutUint64(sourceObligation.Data[128:136], amount)
	if obligation != nil {
		obligation(&sourceObligation)
	}
	held[sourcePosition.Obligation] = sourceObligation
	ata := fixtureAccount(sourcePosition.Position.VaultLiquidityATA, tokenProgram, 1, make([]byte, 165))
	fixtureKey(t, ata.Data, 0, USDCMint)
	fixtureKey(t, ata.Data, 32, vault)
	ata.Data[108] = 1
	held[sourcePosition.Position.VaultLiquidityATA] = ata
	policy := testIdentity(5)
	held[policy] = fixtureAccount(policy, squads.ProgramID.String(), 1, []byte{1})
	// A pooled endpoint whose nodes each answer at their own slot: the route
	// never asks getSlot, and a lagging node refuses any minContextSlot floor.
	bank := 999
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &call); err != nil {
			t.Error(err)
		}
		var floor struct {
			MinContextSlot uint64 `json:"minContextSlot"`
		}
		if len(call.Params) > 1 {
			_ = json.Unmarshal(call.Params[len(call.Params)-1], &floor)
		}
		if call.Method == "getSlot" || floor.MinContextSlot > 0 {
			t.Errorf("%s read with a slot floor %d", call.Method, floor.MinContextSlot)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32016, "message": "Minimum context slot has not been reached"}})
			return
		}
		var result any
		switch call.Method {
		case "getRecentPerformanceSamples":
			result = testPerformanceSamples
		case "getMinimumBalanceForRentExemption":
			var size int
			_ = json.Unmarshal(call.Params[0], &size)
			switch size {
			case kamino.ObligationSize:
				result = rent
			case 0:
				result = emptyAccountRent
			default:
				t.Errorf("rent asked for %d bytes", size)
			}
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(call.Params[0], &addresses)
			values := make([]any, len(addresses))
			for i, address := range addresses {
				// The target obligation and the vault are absent on chain.
				if account, ok := held[address]; ok {
					values[i] = map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}
				}
			}
			bank++
			result = map[string]any{"context": map[string]any{"slot": bank}, "value": values}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	r := &Revalidator{rpc: testChain(t, server.URL), signer: testIdentity(6)}
	lease := RevalidationLease{VaultPubkey: vault, SourceReserve: source.Address, TargetReserve: target.Address, LiquidityMint: USDCMint, PolicyAccount: policy, LiquidityAmountRaw: amount, PrincipalUSDMicros: int64(amount)}
	return r, lease
}

// emptyAccountRent is the rent-exempt minimum of an account with no data.
const emptyAccountRent = uint64(890_880)

// A setup route pays rent, so it never goes to a fee-only payer (b1ad5b1a).
func TestSetupRouteIsNeverFeeOnly(t *testing.T) {
	r := &Revalidator{signer: testIdentity(6), feeOnlyPayers: []string{testIdentity(7)}}
	if payer := r.feePayer(context.Background(), "localnet", RevalidationLease{}, false); payer != r.signer {
		t.Fatalf("setup route paid by %s", payer)
	}
}

// The payer funds exactly the vault's rent deficit, and never more than the
// retained cap.
// A vault left between 1 lamport and its rent-exempt minimum fails the
// transaction (InsufficientFundsForRent), so the payer lifts it to the minimum.
func TestVaultRentTopUp(t *testing.T) {
	for _, c := range []struct{ rent, vault, want uint64 }{
		{23_942_400, 0, 23_942_400}, {23_942_400, 1_000_000, 22_942_400}, {23_942_400, 23_942_400, 0}, {23_942_400, 30_000_000, 0},
		{23_942_400, 23_942_401, emptyAccountRent - 1}, {23_942_400, 23_942_400 + emptyAccountRent - 1, 1}, {23_942_400, 23_942_400 + emptyAccountRent, 0},
	} {
		got, err := vaultRentTopUp(c.rent, emptyAccountRent, c.vault)
		if err != nil || got != c.want {
			t.Fatalf("top-up(%d,%d)=%d %v", c.rent, c.vault, got, err)
		}
		if left := c.vault + got - c.rent; left != 0 && left < emptyAccountRent {
			t.Fatalf("vault left at %d lamports", left)
		}
	}
	if _, err := vaultRentTopUp(maxObligationRentLamports+1, emptyAccountRent, 0); err == nil {
		t.Fatal("rent above the cap was funded")
	}
}
