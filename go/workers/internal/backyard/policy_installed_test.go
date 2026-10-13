package backyard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

// installedLiteralSHA256 pins each policy literal Backyard executes through,
// by name: the sha256 of its JSON. These are the literals installed on
// Backyard's Settings (each matched exactly one account of the 2026-10-11
// finalized policy read at slot 455818425). A literal that changes no longer
// finds its installed account, and every leg through it holds: change one
// only together with the on-chain policy, never alone.
var installedLiteralSHA256 = map[string]string{
	"AUTO/AUTO/PYUSD":                                   "56f71a8e826b667cd1dfd606d87cfcc58c3bb29304e4cbce352571589591cb4f",
	"AUTO/AUTO/wYLDS":                                   "87f8c04f62efb467f2aac63281d97b409fd8125cf385a3e7f89bf94af9791e10",
	"CollateralLifecycle":                               "cb31f4fcf3f4d3a1a5fbdbf11260853a61daf2d5e996c6d1835829317cc37df1",
	"DebtLifecycle":                                     "730b23aef4ab468b975ca1a072763ce5738328f15bdea70ac7e0c82134561379",
	"Ethena/USDe/PYUSD borrow":                          "288921f4fdecefb4bb5480e3463ba6b43480ef3ca866ed47176e16e3112d50f3",
	"Ethena/USDe/PYUSD deposit":                         "86f2b7f1553d23f1db784be595d03b35736ad01eb79439cc39e76dde2da2b3b3",
	"Ethena/USDe/PYUSD repay":                           "7a04e0e51f2ff239a326141afdf8b9571fdc50c4a5dc1731dee7aa1ff7ced362",
	"Ethena/USDe/PYUSD withdraw":                        "3e38b44470102a6cb72794dc74a087aa61978a909cb03c6caf4d1b3c7aa0f435",
	"Maple/syrupUSDC/USDC INITIALIZE_KAMINO_OBLIGATION": "93605770fbf4999abce0202c6ba2eed0c67cb02a3edc86ba3f369c7b94e950c4",
	"OnRe/ONyc/USDC INITIALIZE_KAMINO_OBLIGATION":       "eb2f133140048bb7391ea07662a2e69286aa07073ba4166a285c274677f801c5",
	"PRIME->USDC/PRIME->USDS swap":                      "09409309d3d3c45c684e4f5c1e3c51800e0ded273839f445fe9a45c714b0f8d4",
	"PRIME->USDG/PRIME->PYUSD swap":                     "5860db59d41a18afac8b6e1f3b5c6ece27b7165750fb175819bd405abb7134fe",
	"PYUSD->AUTO/PYUSD->USDe swap":                      "7355b784fd423c3287216a6728853d8f558022ee64c4bd8bc5043ea66476f8f8",
	"PYUSD->PRIME/PYUSD->syrupUSDC swap":                "9561e484ecbd3eecfcbdb93e2ce30403dc5137ff6723fbec333dea520a2b6bc0",
	"PYUSD->USDC/PYUSD->USDS swap":                      "75c0654b7045a0a457311f1728cd7c917ba5e515f17345c1139c6ed3e1cb1653",
	"Prime/PRIME/PYUSD borrow":                          "6d59e38651b661ff07737ee8caeca9c6efe0f8d9891afaa5e359d87be3a51b87",
	"Prime/PRIME/PYUSD deposit":                         "33d30a7e3d13f7b62c81ab0dba1a58956a2bb7e24c96bb6a4ea060ea6fe9af69",
	"Prime/PRIME/PYUSD repay":                           "5de9d13ce03bc4bbe4ee5c6db69f95ce91622387cc041ce4fac96eb7dd3c3a64",
	"Prime/PRIME/PYUSD withdraw":                        "4f55376814c5de7fcef9cbdbe048a82a3775c6f05cff04d4005f41db95243cd7",
	"Prime/PRIME/USDC INITIALIZE_KAMINO_OBLIGATION":     "544fc6358f0750b01fd45162b6efec593ecff884d2c06894e80a890324c29c94",
	"Prime/PRIME/USDS borrow":                           "611259c42b1deadb31ee29f15a03c45a57955447f8ef69f2c1e7a02a32849f6b",
	"Prime/PRIME/USDS deposit":                          "31c75e20b36bac596eb255f614d3b09157084921e4158c666038493978cffcc9",
	"Prime/PRIME/USDS repay":                            "06762f593d7e80ac516a2a039b3d95c12f586dac9fb9672070f01ce76438fd17",
	"Prime/PRIME/USDS withdraw":                         "a2ac020180c51ca41ee0492ed6b96bcc3187ad98697bd4817c515631260278ba",
	"REPORT_NAV":                                        "81908c5dcb47fc4edc7fb1aca10aeec7559766c01a1efbb1ecabf942a1e24cf5",
	"STAGE_SQUADS_TO_VOLTR":                             "ebe1a33d37838ac48e60c697c8ff4711472a07fc276e11b789e0e6fae3e73d17",
	"SwapRoutesA":                                       "224676c217d4e548f66d08d6734ebc2796da34c1839b0dc6bc2851915411cef1",
	"SwapRoutesB":                                       "65a61e484088c9aa366591ce0cbff25a10213ba60ecf833b3ed6d5b31a70588d",
	"USDC->AUTO/AUTO->USDC/AUTO->PYUSD swap":            "ad43f828cd5d3f691610d287afad0bd0583feaa0baf72b1ec8fffe12373e781f",
	"USDC->ONyc/USDC->PRIME swap":                       "cfd71375c79e0d48f8531d46f75c04ea267450b375df875a3343a34b1ee278d0",
	"USDC->USDG/USDC->PYUSD swap":                       "d5d67ee20c1277321e058f4af353d54830039f2b5becfef0c4bd28d5fe22bebe",
	"USDC->USDS/USDG->PYUSD swap":                       "aa767d224db46c72ef13a335e0a4e78815215a516019c93a2cc0456011ef0e5f",
	"USDC->USDe/USDS->ONyc swap":                        "f7bcfbb5eb1efdf6502237d0d0915a2cc6ad46981a412a76a7b3a7ac163f843d",
	"USDS->PRIME/USDS->syrupUSDC swap":                  "d70d0abd049609e209781dc4e321c61b8e85deb73a9f340f3be68d01c2b2274e",
	"USDS->USDC/PYUSD->USDG swap":                       "636f9818de50e7aa28af39d73bb747491be9aa730ad3fd54ecf9a444890695f6",
	"USDe->USDC/USDe->USDS swap":                        "fbc99a7c9155252606368f89a1a3ed66cd95387360d4725e007674e9a915e92b",
	"USDe->USDG/USDe->PYUSD swap":                       "d237753c8f25f8dd614aa719958ecfb68daff81b1a8aaf313a8c103b16adcd98",
	"VOLTR_ALLOCATE_TO_SQUADS":                          "ad63de4894be9e8612fa268391564c592ebb8321c5b981a1aa28b3f93bbc2d39",
	"VOLTR_RESTORE_IDLE":                                "47bd258cbdc84ef68c3552df4c55aff4160083e797b95afa3f254b218af5cd08",
	"wYLDS->AUTO/AUTO->wYLDS/USDC->wYLDS hastra":        "fe05baac5801373f6efbb6397ec267d778b98e6e5b89c0020f5690a1827e9881",
}

func TestPolicyLiteralsAreTheInstalledOnes(t *testing.T) {
	t.Parallel()
	literals, err := backyardPolicies(rwa)
	if err != nil {
		t.Fatal(err)
	}
	if len(literals) != len(installedLiteralSHA256) {
		t.Fatalf("%d literals, %d installed", len(literals), len(installedLiteralSHA256))
	}
	for key, literal := range literals {
		raw, err := json.Marshal(literal)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != installedLiteralSHA256[key.String()] {
			t.Errorf("%s changed: %s", key, got)
		}
	}
}

// An AUTO lane's policy pins the vault's lane obligation and its associated
// token accounts of the two mints (policy.LoopConstraints derives them); its
// route sends exactly those.
func TestAutoRoutesAreTheirPolicysAccounts(t *testing.T) {
	t.Parallel()
	vault := kaminoKey(bridgeVault)
	for _, route := range lanesWithPolicy(rwa, policyAuto) {
		k := route.Kamino
		obligation, err := kamino.ObligationAddress(1, 0, vault, kaminoKey(k.Market), kaminoKey(k.CollateralMint), kaminoKey(k.DebtMint))
		if err != nil {
			t.Fatal(err)
		}
		collateral, err := spl.AssociatedTokenAddress(vault, kaminoKey(k.CollateralMint), kaminoKey(route.CollateralTokenProgram))
		if err != nil {
			t.Fatal(err)
		}
		debt, err := spl.AssociatedTokenAddress(vault, kaminoKey(k.DebtMint), kaminoKey(route.DebtTokenProgram))
		if err != nil {
			t.Fatal(err)
		}
		for name, pair := range map[string][2]solana.PublicKey{"obligation": {obligation, kaminoKey(k.Obligation)},
			"collateral custody": {collateral, kaminoKey(route.CollateralCustody)}, "debt custody": {debt, kaminoKey(route.DebtCustody)}} {
			if pair[0] != pair[1] {
				t.Errorf("%s %s: route %s, policy %s", route.Lane, name, pair[1], pair[0])
			}
		}
	}
}
