package autodeposit

import (
	"errors"
	"testing"
)

func TestDepositReserveIsTheLargestLiveReserveInTheDepositMint(t *testing.T) {
	const other = "other-mint"
	for _, tc := range []struct {
		name      string
		mint      string
		positions []LiveVaultPosition
		want      string
		refused   bool
	}{
		{"largest wins", USDCMint, []LiveVaultPosition{{"a", "m", USDCMint, 3}, {"b", "m", USDCMint, 4}}, "b", false},
		{"tie breaks on reserve address", USDCMint, []LiveVaultPosition{{"z", "m", USDCMint, 4}, {"c", "m", USDCMint, 4}}, "c", false},
		{"other mints and empty rows ignored", USDCMint, []LiveVaultPosition{{"big", "m", other, 99}, {"empty", "m", USDCMint, 0}, {"a", "m", USDCMint, 1}}, "a", false},
		{"an empty vault takes the default", USDCMint, []LiveVaultPosition{{"drained", "m", USDCMint, 0}}, defaultEarnReserve, false},
		{"live reserves in other mints only refuse", USDCMint, []LiveVaultPosition{{"big", "m", other, 99}}, "", true},
		{"no default outside USDC", other, nil, "", true},
		{"a row without its market refuses", USDCMint, []LiveVaultPosition{{"a", "", USDCMint, 1}}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDepositReserve(tc.mint, tc.positions)
			if tc.refused {
				if !errors.Is(err, ErrUnresolvedCurrentReserve) {
					t.Fatalf("want a refusal, got %+v %v", got, err)
				}
				return
			}
			if err != nil || got.Reserve != tc.want {
				t.Fatalf("got %+v %v, want %s", got, err, tc.want)
			}
		})
	}
}
