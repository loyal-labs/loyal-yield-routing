package webapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type wprepTestPolicies struct{ pair RoutePolicyPair }

func (p wprepTestPolicies) activePolicyPair(context.Context, EarnAccount) (*RoutePolicyPair, error) {
	return &p.pair, nil
}
func (wprepTestPolicies) inactivePolicy(context.Context, EarnAccount) (bool, error) {
	return false, nil
}

type wprepTestChain struct {
	accounts map[solana.PublicKey]*chain.Account
	simulate func([]byte) (chain.Simulated, error)
}

func (c wprepTestChain) Accounts(_ context.Context, keys []solana.PublicKey, _ rpc.CommitmentType, _ uint64) (uint64, []*chain.Account, error) {
	out := make([]*chain.Account, len(keys))
	for i, key := range keys {
		out[i] = c.accounts[key]
	}
	return 100, out, nil
}
func (wprepTestChain) Blockhash(context.Context, rpc.CommitmentType, uint64) (solana.Hash, uint64, uint64, error) {
	return solana.Hash{1}, 200, 100, nil
}
func (c wprepTestChain) Simulate(_ context.Context, wire []byte, _ rpc.SimulateTransactionOpts) (chain.Simulated, error) {
	return c.simulate(wire)
}

func TestWprepMaxRedeemsAllCollateralWithoutChangingPartialWithdrawals(t *testing.T) {
	for _, tc := range []struct {
		name, request                                            string
		quotedCollateral, expectedCollateral, redeemed, transfer uint64
	}{
		{"max", "max", 99_999_999, math.MaxUint64, 100_050_250, 100_050_250},
		{"partial", "50000000", 49_975_013, 49_975_013, 50_000_001, 50_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			wallet, settings := solana.PublicKey{1}, solana.PublicKey{2}
			vault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
			if err != nil {
				t.Fatal(err)
			}
			market := solana.MustPublicKeyFromBase58("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8")
			mint := solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
			reserve, collateralMint := solana.PublicKey{3}, solana.PublicKey{4}
			vaultATA, err := spl.AssociatedTokenAddress(vault, mint, solana.TokenProgramID)
			if err != nil {
				t.Fatal(err)
			}
			obligation, err := kamino.VanillaObligation(vault, market)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := kamino.LendingMarketAuthority(market)
			if err != nil {
				t.Fatal(err)
			}
			accounts := kamino.CollateralAccounts{
				Owner: vault, Obligation: obligation, LendingMarket: market, LendingMarketAuthority: authority,
				Reserve: reserve, LiquidityMint: mint, LiquiditySupply: solana.PublicKey{5},
				CollateralMint: collateralMint, CollateralSupply: solana.PublicKey{6},
				UserLiquidity: vaultATA, LiquidityTokenProgram: solana.TokenProgramID,
			}
			quoted := wprepIx(kamino.WithdrawV2(accounts, tc.quotedCollateral))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				keys := make([]map[string]string, len(quoted.Accounts))
				for i, a := range quoted.Accounts {
					role := "READONLY"
					if a.IsWritable {
						role = "WRITABLE"
					}
					if a.IsSigner {
						role += "_SIGNER"
					}
					keys[i] = map[string]string{"address": a.PublicKey.String(), "role": role}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"instructions": []any{map[string]any{
					"programAddress": kamino.ProgramID.String(), "accounts": keys,
					"data": base64.StdEncoding.EncodeToString(quoted.Data),
				}}})
			}))
			t.Cleanup(server.Close)

			// The quote and holdings snapshot precede interest accrued during simulation.
			reserveData := make([]byte, 2632)
			copy(reserveData, kamino.ReserveDiscriminator[:])
			for offset, key := range map[int]solana.PublicKey{32: market, 128: mint, 160: accounts.LiquiditySupply, 408: solana.TokenProgramID, 2560: collateralMint, 2600: accounts.CollateralSupply} {
				copy(reserveData[offset:], key[:])
			}
			binary.LittleEndian.PutUint64(reserveData[224:], 1_000_500_000)
			binary.LittleEndian.PutUint64(reserveData[2592:], 1_000_000_000)
			obligationData := make([]byte, 96+8*136)
			copy(obligationData, kamino.ObligationDiscriminator[:])
			copy(obligationData[32:], market[:])
			copy(obligationData[64:], vault[:])
			copy(obligationData[96:], reserve[:])
			binary.LittleEndian.PutUint64(obligationData[128:], 100_000_000)
			idle := make([]byte, 165)
			copy(idle, mint[:])
			copy(idle[32:], vault[:])
			binary.LittleEndian.PutUint64(idle[64:], 1_000)
			c := wprepTestChain{accounts: map[solana.PublicKey]*chain.Account{
				reserve:    {Owner: kamino.ProgramID, Data: reserveData},
				obligation: {Owner: kamino.ProgramID, Data: obligationData},
				vaultATA:   {Owner: solana.TokenProgramID, Data: idle},
			}}
			simulations := 0
			c.simulate = func(wire []byte) (chain.Simulated, error) {
				simulations++
				withdraw := wprepWithAmount(quoted, tc.expectedCollateral)
				if !bytes.Contains(wire, withdraw.Data) {
					t.Fatalf("compiled withdrawal lacks expected collateral amount %d", tc.expectedCollateral)
				}
				post := bytes.Clone(idle)
				binary.LittleEndian.PutUint64(post[64:], 1_000+tc.redeemed)
				return chain.Simulated{Slot: 101, Accounts: []*chain.Account{{Owner: solana.TokenProgramID, Data: post}}}, nil
			}
			policy := RoutePolicy{PolicyAccount: (solana.PublicKey{7}).String(), PolicySeed: 1,
				StableMints: []string{mint.String()}, KaminoLiquidityMints: []string{mint.String()}, KaminoMarkets: []string{market.String()}}
			account := EarnAccount{Principal: Principal{Wallet: wallet, Settings: settings}, Vault: vault, Cluster: LoyalMainnetBeta}
			body, err := json.Marshal(map[string]string{"amountRaw": tc.request, "sourceId": "reserve:" + reserve.String()})
			if err != nil {
				t.Fatal(err)
			}
			request, err := parseWprepBody(body)
			if err != nil {
				t.Fatal(err)
			}
			input, err := wprepResolve(ctx, wprepTestPolicies{RoutePolicyPair{Route: policy}}, c, account, solana.PublicKey{8}, request)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := input.prepareReserve(ctx, wprepDeps{Chain: c, HTTP: server.Client(), KaminoURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			want := strconv.FormatUint(tc.transfer, 10)
			if prepared.AmountRaw != want || prepared.Persistence.WithdrawnAmountRaw != want || prepared.Persistence.WalletTransferAmountRaw != want || simulations != 1 {
				t.Fatalf("amount=%s withdrawn=%s transfer=%s simulations=%d; want %s in one simulation", prepared.AmountRaw, prepared.Persistence.WithdrawnAmountRaw, prepared.Persistence.WalletTransferAmountRaw, simulations, want)
			}
			walletATA, err := spl.AssociatedTokenAddress(wallet, mint, solana.TokenProgramID)
			if err != nil {
				t.Fatal(err)
			}
			transfer := wprepVaultTransfer(solana.TokenProgramID, vaultATA, mint, walletATA, vault, tc.transfer)
			last := prepared.WithdrawSteps[0].Prepared.Instructions
			data, err := base64.StdEncoding.DecodeString(last[len(last)-1].DataBase64)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, transfer.Data) {
				t.Fatal("member transaction does not transfer the expected redeemed amount")
			}
			confirmation := struct {
				EarnWithdrawPersistence
				ConfirmedSlot       string `json:"confirmedSlot"`
				WithdrawalSignature string `json:"withdrawalSignature"`
				SmartAccountAddress string `json:"smartAccountAddress"`
			}{prepared.WithdrawSteps[0].Persistence, "101", (solana.Signature{1}).String(), vault.String()}
			confirmationBody, err := json.Marshal(confirmation)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseWithdrawConfirmBody(confirmationBody)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.WithdrawnAmountRaw.String() != want || len(parsed.ReserveWithdrawals) != 1 || parsed.ReserveWithdrawals[0].KaminoWithdrawAmountRaw != strconv.FormatUint(tc.expectedCollateral, 10) {
				t.Fatal("confirmation parser changed the transferred liquidity or collateral amount")
			}
		})
	}
}
