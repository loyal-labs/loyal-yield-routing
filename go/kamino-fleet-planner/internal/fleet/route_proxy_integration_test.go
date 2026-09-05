package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This test invokes the actual compiled Rust proxy, not fixtureProxy's canned
// output. The local verification gate requires its execution without a skip.
func TestRealKLendProxyCrossMintLegs(t *testing.T) {
	path := os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	if path == "" {
		t.Skip("requires compiled Rust proxy")
	}
	binaryBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewKLendProxy(path, fmt.Sprintf("%x", sha256.Sum256(binaryBytes)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sameMint := routeFixture(t)
	if _, err := proxy.Build(ctx, sameMint); err != nil {
		t.Fatalf("same-mint regression: %v", err)
	}
	if _, err := proxy.BuildCrossMintLegs(ctx, sameMint); err == nil {
		t.Fatal("cross-mint lane accepted a same-mint request")
	}
	for _, mint := range earnStableMints {
		t.Run(mint, func(t *testing.T) {
			request := sameMint
			request.Target.LiquidityMint = mint
			request.Target.LiquidityTokenProgram = mustStableProgram(mint)
			request.Target.VaultLiquidityATA, err = deriveATA(request.Vault, mint, request.Target.LiquidityTokenProgram)
			if err != nil {
				t.Fatal(err)
			}
			route, err := proxy.BuildCrossMintLegs(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(route.Protected) != 2 {
				t.Fatal("expected independent withdrawal and deposit instructions")
			}
			for i, expected := range []struct {
				amount    uint64
				mint, ata string
			}{{request.WithdrawCollateralAmount, request.Source.LiquidityMint, request.Source.VaultLiquidityATA}, {request.DepositLiquidityAmount, request.Target.LiquidityMint, request.Target.VaultLiquidityATA}} {
				ix := route.Protected[i]
				if ix.Program != KLendProgram || len(ix.Data) != 16 || binary.LittleEndian.Uint64(ix.Data[8:]) != expected.amount {
					t.Fatalf("leg %d amount/program drifted", i)
				}
				foundMint, foundATA := false, false
				for _, account := range ix.Accounts {
					foundMint = foundMint || account.Address == expected.mint
					foundATA = foundATA || account.Address == expected.ata && account.Writable
				}
				if !foundMint || !foundATA {
					t.Fatalf("leg %d lost its own mint or writable token account", i)
				}
			}
			if _, err := proxy.Build(ctx, request); err == nil {
				t.Fatal("Go same-mint lane accepted different mints")
			}
			// Bypass Go validation to verify the Rust boundary independently.
			for _, invalid := range []proxyRequest{
				{1, "buildSameMintRoute", request},
				{1, "buildCrossMintLegs", sameMint},
				{1, "unknownOperation", request},
			} {
				raw, err := json.Marshal(invalid)
				if err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(ctx, path)
				command.Env = []string{"LC_ALL=C"}
				command.Stdin = bytes.NewReader(raw)
				if output, err := command.CombinedOutput(); err == nil {
					t.Fatalf("Rust accepted invalid lane %s: %s", invalid.Operation, output)
				}
			}
		})
	}
}
