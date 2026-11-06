package voltr

import (
	"testing"
	"time"
)

// Vectors are calculateLpForDepositAmount from @voltr/vault-sdk at the same
// inputs, so the quote matches what the program mints.
func TestDepositLPMatchesSDKVectors(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		name   string
		vault  Vault
		supply uint64
		dec    [2]uint8
		amount uint64
		want   uint64
	}{
		{"first deposit pays dead weight", Vault{}, 0, [2]uint8{6, 9}, 1_000_000, 999_999_000},
		{"accrued management fee and issuance fee", Vault{TotalValueRaw: 1_000_000_000, FeeAccumulatorAdminRaw: 20, FeeAccumulatorManagerRaw: 10, FeeAccumulatorProtocolRaw: 30, LPSupplyDeadWeightRaw: 1000, IssuanceFeeBPS: 25, ManagerManagementFeeBPS: 50, AdminManagementFeeBPS: 30, ProtocolManagementFeeBPS: 20, LastManagementFeeUpdateUnix: 1_800_000_000 - 15_768_000}, 900_000_000, [2]uint8{6, 6}, 10_000_000, 9_022_398},
		{"floors the mint", Vault{TotalValueRaw: 123_456_789_012, FeeAccumulatorAdminRaw: 7, FeeAccumulatorManagerRaw: 3, LPSupplyDeadWeightRaw: 1000, IssuanceFeeBPS: 10, ManagerManagementFeeBPS: 37, LastManagementFeeUpdateUnix: 1_800_000_000 - 3_601}, 98_765_432_109, [2]uint8{6, 6}, 333_333, 266_399},
	} {
		got, err := c.vault.DepositLP(c.supply, c.dec[0], c.dec[1], c.amount, now)
		if err != nil || got != c.want {
			t.Fatalf("%s: got %d want %d err %v", c.name, got, c.want, err)
		}
	}
	if _, err := (Vault{}).DepositLP(0, 6, 6, 999, now); err == nil {
		t.Fatal("first deposit below dead weight quoted")
	}
}
