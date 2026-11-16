package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
)

type earnMaxScanTransport func(*http.Request) (*http.Response, error)

func (f earnMaxScanTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEarnMaxScanMintHistory(t *testing.T) {
	for _, vault := range []string{"loyal", "backyard"} {
		for _, scenario := range []string{"complete", "known reports", "missing deposit", "missing transaction", "stuck pagination"} {
			t.Run(vault+"/"+scenario, func(t *testing.T) {
				cfg, err := earnMaxConfigFor(vault)
				if err != nil {
					t.Fatal(err)
				}
				oldCache := earnMaxCaches[vault]
				earnMaxCaches[vault] = &earnMaxVaultCache{flows: map[string]EarnMaxLpFlow{}}
				t.Cleanup(func() { earnMaxCaches[vault] = oldCache })
				t.Setenv("LOYAL_ADMIN_SOLANA_RPC_URL", "https://rpc.test")
				sig := func(n uint64) solana.Signature {
					var s solana.Signature
					binary.LittleEndian.PutUint64(s[:8], n)
					return s
				}
				var deps *Deps
				if scenario == "known reports" {
					dsn := os.Getenv("ADMIN_TEST_DATABASE_URL")
					if dsn == "" {
						t.Skip("requires disposable ADMIN_TEST_DATABASE_URL")
					}
					config, err := pgxpool.ParseConfig(dsn)
					if err != nil {
						t.Fatal(err)
					}
					if config.ConnConfig.Host != "127.0.0.1" || config.ConnConfig.Database != "admin_vault_test" {
						t.Fatal("requires local disposable admin_vault_test")
					}
					pool, err := pgxpool.NewWithConfig(context.Background(), config)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(pool.Close)
					_, err = pool.Exec(context.Background(), `CREATE SCHEMA loyal_yield; CREATE TABLE loyal_yield.multiply_operations (route_key text, action text, status text, transaction_signature text)`)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA loyal_yield CASCADE`) })
					reports := []string{}
					for n := uint64(1001); n < 2000; n++ {
						reports = append(reports, sig(n).String())
					}
					_, err = pool.Exec(context.Background(), `INSERT INTO loyal_yield.multiply_operations SELECT $1,'REPORT_NAV','reconciled',unnest($2::text[]);
`, cfg.RouteKey, reports)
					if err != nil {
						t.Fatal(err)
					}
					_, err = pool.Exec(context.Background(), `INSERT INTO loyal_yield.multiply_operations VALUES ('other-route','REPORT_NAV','reconciled',$1),($2,'REPORT_NAV','failed',$3),($2,'DEPOSIT','reconciled',$4)`, sig(1).String(), cfg.RouteKey, sig(2).String(), sig(3).String())
					if err != nil {
						t.Fatal(err)
					}
					deps = &Deps{Yield: pool}
				}
				row := func(n, slot uint64, failed bool) map[string]any {
					r := map[string]any{"signature": sig(n).String(), "slot": slot, "err": nil, "blockTime": 1000}
					if failed {
						r["err"] = "failed"
					}
					return r
				}
				// Twenty-five deposits, then a full withdrawal closing one holder's account.
				transactions := map[string]any{}
				for n := uint64(1); n <= 26; n++ {
					holder, payer := solana.PublicKey{byte(n)}, solana.PublicKey{99}
					if n == 26 {
						holder = solana.PublicKey{1}
					}
					tx, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(solana.TokenProgramID, solana.AccountMetaSlice{solana.Meta(holder).WRITE(), solana.Meta(cfg.Accounts.VaultAssetIdleATA).WRITE()}, nil)}, solana.Hash{1}, solana.TransactionPayer(payer))
					if err != nil {
						t.Fatal(err)
					}
					wire, err := tx.MarshalBinary()
					if err != nil {
						t.Fatal(err)
					}
					balance := func(key, mint solana.PublicKey, amount string) any {
						index := -1
						for i, k := range tx.Message.AccountKeys {
							if k == key {
								index = i
							}
						}
						if index < 0 {
							t.Fatal("fixture account missing")
						}
						return map[string]any{"accountIndex": index, "mint": mint.String(), "owner": payer.String(), "programId": solana.TokenProgramID.String(), "uiTokenAmount": map[string]any{"amount": amount, "decimals": 6}}
					}
					pre := []any{balance(cfg.Accounts.VaultAssetIdleATA, solana.PublicKey{88}, "0")}
					post := []any{balance(holder, cfg.Accounts.LPMint, "100000000"), balance(cfg.Accounts.VaultAssetIdleATA, solana.PublicKey{88}, "100000000")}
					if n == 26 {
						pre, post = post, pre
					}
					transactions[sig(n).String()] = map[string]any{"slot": 100, "blockTime": 1000 + int(n), "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"}, "meta": map[string]any{"err": nil, "preTokenBalances": pre, "postTokenBalances": post}}
				}
				first := []any{row(2001, 201, false)} // Arrived after the mint snapshot; exclude it.
				for n := uint64(1001); n < 2000; n++ {
					first = append(first, row(n, 200, scenario != "known reports"))
				}
				second := []any{}
				for n := uint64(1); n <= 26; n++ {
					if scenario != "missing deposit" || n != 1 {
						second = append(second, row(n, 100, false))
					}
				}
				pages := 0
				oldTransport := http.DefaultTransport
				http.DefaultTransport = earnMaxScanTransport(func(r *http.Request) (*http.Response, error) {
					var body any
					if r.URL.Host == "api.voltr.xyz" {
						body = map[string]any{"data": map[string]any{"sharePrice": 1000}}
					} else {
						var req struct {
							ID     any
							Method string
							Params []json.RawMessage
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							return nil, err
						}
						var result any
						switch req.Method {
						case "getMultipleAccounts":
							mint := make([]byte, 82)
							binary.LittleEndian.PutUint64(mint[36:44], 2_400_000_000)
							result = map[string]any{"context": map[string]any{"slot": 200}, "value": []any{map[string]any{"lamports": 1, "owner": solana.TokenProgramID.String(), "data": []string{base64.StdEncoding.EncodeToString(mint), "base64"}}}}
						case "getSignaturesForAddress":
							var address string
							_ = json.Unmarshal(req.Params[0], &address)
							var opts struct {
								Before         string
								MinContextSlot uint64
							}
							_ = json.Unmarshal(req.Params[1], &opts)
							if address != cfg.Accounts.LPMint.String() || opts.MinContextSlot != 200 {
								t.Error("history must use the mint and its snapshot slot")
							}
							pages++
							if pages == 1 || scenario == "stuck pagination" {
								result = first
							} else {
								if opts.Before != sig(1999).String() {
									t.Error("incorrect pagination cursor")
								}
								result = second
							}
						case "getTransaction":
							var signature string
							_ = json.Unmarshal(req.Params[0], &signature)
							result = transactions[signature]
							if scenario == "missing transaction" && signature == sig(1).String() {
								result = nil
							}
						default:
							return nil, fmt.Errorf("unexpected RPC method: %s", req.Method)
						}
						body = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
					}
					raw, _ := json.Marshal(body)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
				})
				t.Cleanup(func() { http.DefaultTransport = oldTransport })
				scan, err := earnMaxScanVault(context.Background(), deps, cfg)
				if scenario != "complete" && scenario != "known reports" {
					if err == nil || scan.valueUsd != 0 || len(scan.flows) != 0 {
						t.Fatalf("incomplete history produced earnings: %+v, %v", scan, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if pages != 2 || len(scan.flows) != 26 || scan.valueUsd != 2400 {
					t.Fatalf("scan = %+v, pages = %d", scan, pages)
				}
				var deposits, withdrawals float64
				for _, flow := range scan.flows {
					if flow.LpNet > 0 {
						deposits += flow.IdleDelta / 1e6
					} else {
						withdrawals -= flow.IdleDelta / 1e6
					}
				}
				if deposits != 2500 || withdrawals != 100 || scan.valueUsd-deposits+withdrawals != 0 {
					t.Fatalf("deposits=%v withdrawals=%v", deposits, withdrawals)
				}
			})
		}
	}
}
