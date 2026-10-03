package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRealKLendProxyIdleDeposit(t *testing.T) {
	path := os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	if path == "" {
		t.Skip("requires compiled Rust proxy")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewKLendProxy(path, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := routeFixture(t)
	reference, err := proxy.Build(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, mint := range earnStableMints {
		t.Run(mint, func(t *testing.T) {
			r := KaminoIdleDepositRequest{Vault: base.Vault, Target: base.Target, DepositLiquidityAmount: base.DepositLiquidityAmount}
			r.Target.ObligationBorrowReserves = []string{}
			r.Target.Obligation = reference.Protected[1].Accounts[1].Address
			r.Target.MarketAuthority = reference.Protected[1].Accounts[3].Address
			r.Target.LiquidityMint, r.Target.LiquidityTokenProgram = mint, mustStableProgram(mint)
			r.Target.VaultLiquidityATA, err = deriveATA(r.Vault, mint, r.Target.LiquidityTokenProgram)
			if err != nil {
				t.Fatal(err)
			}
			for _, deposits := range [][]string{nil, {r.Target.Reserve}} {
				r.Target.ObligationDepositReserves = deposits
				route, err := proxy.BuildIdleDeposit(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				if len(route.Protected) != 1 || !bytes.Equal(route.Protected[0].Data, reference.Protected[1].Data) {
					t.Fatal("idle is not exactly the official deposit instruction")
				}
				// Mutate every account and privilege in the real response. Validation
				// must bind the complete instruction, not just owner or amount bytes.
				instructions := []*RouteInstruction{&route.Public[0], &route.Public[1], &route.Protected[0]}
				for _, ix := range instructions {
					for i := range ix.Accounts {
						original := ix.Accounts[i]
						for _, mutate := range []func(*InstructionAccount){func(a *InstructionAccount) { a.Address = "" }, func(a *InstructionAccount) { a.Writable = !a.Writable }, func(a *InstructionAccount) { a.Signer = !a.Signer }} {
							mutate(&ix.Accounts[i])
							if validateIdleProxyRoute(route, r) == nil {
								t.Fatal("idle account mutation accepted")
							}
							ix.Accounts[i] = original
						}
					}
					for i := range ix.Data {
						ix.Data[i] ^= 1
						if validateIdleProxyRoute(route, r) == nil {
							t.Fatal("idle instruction mutation accepted")
						}
						ix.Data[i] ^= 1
					}
				}
			}
			for _, mutate := range []func(*KaminoIdleDepositRequest){
				func(v *KaminoIdleDepositRequest) { v.DepositLiquidityAmount = 0 },
				func(v *KaminoIdleDepositRequest) { v.Target.ObligationDepositReserves = []string{base.Source.Reserve} },
				func(v *KaminoIdleDepositRequest) { v.Target.ObligationBorrowReserves = []string{base.Source.Reserve} },
				func(v *KaminoIdleDepositRequest) { v.Target.Obligation = "" },
				func(v *KaminoIdleDepositRequest) { v.Target.MarketAuthority = base.Source.Reserve },
			} {
				bad := r
				mutate(&bad)
				if _, err := proxy.BuildIdleDeposit(ctx, bad); err == nil {
					t.Fatal("invalid idle request accepted")
				}
				// Bypass Go checks: Rust must independently reject the bad request.
				input, err := json.Marshal(proxyRequest{1, "buildIdleDeposit", bad})
				if err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(ctx, path)
				command.Env = []string{"LC_ALL=C"}
				command.Stdin = bytes.NewReader(input)
				if err := command.Run(); err == nil {
					t.Fatal("Rust accepted invalid idle request")
				}
			}
			bad := r
			bad.Target.VaultLiquidityATA = base.Source.Reserve
			if _, err := proxy.BuildIdleDeposit(ctx, bad); err == nil {
				t.Fatal("unowned idle token account accepted")
			}
			validJSON, err := json.Marshal(proxyRequest{1, "buildIdleDeposit", r})
			if err != nil {
				t.Fatal(err)
			}
			validCommand := exec.CommandContext(ctx, path)
			validCommand.Env = []string{"LC_ALL=C"}
			validCommand.Stdin = bytes.NewReader(validJSON)
			if err := validCommand.Run(); err != nil {
				t.Fatal("direct Rust idle control failed")
			}
			duplicateJSON := bytes.Replace(validJSON, []byte(`"depositLiquidityAmount":`), []byte(`"depositLiquidityAmount":1,"depositLiquidityAmount":`), 1)
			duplicateCommand := exec.CommandContext(ctx, path)
			duplicateCommand.Env = []string{"LC_ALL=C"}
			duplicateCommand.Stdin = bytes.NewReader(duplicateJSON)
			if err := duplicateCommand.Run(); err == nil {
				t.Fatal("Rust accepted duplicate idle amount fields")
			}
			injection := map[string]any{"vault": r.Vault, "target": r.Target, "depositLiquidityAmount": r.DepositLiquidityAmount, "source": base.Source, "withdrawCollateralAmount": 1}
			input, _ := json.Marshal(proxyRequest{1, "buildIdleDeposit", injection})
			command := exec.CommandContext(ctx, path)
			command.Env = []string{"LC_ALL=C"}
			command.Stdin = bytes.NewReader(input)
			if err := command.Run(); err == nil {
				t.Fatal("Rust idle builder accepted withdrawal injection")
			}
		})
	}
}
