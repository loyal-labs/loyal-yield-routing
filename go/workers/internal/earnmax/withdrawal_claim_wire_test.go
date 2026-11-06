package earnmax

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"os"
	"strings"
	"testing"
	"time"
)

// The official claim wire, signed by the production TS builders over captured
// mainnet state; regenerate with web/packages/smart-account-vaults/scripts/voltr-wire-fixtures.ts.
const withdrawalClaimFixtures = "../../../../crates/squads-test-harness/fixtures/voltr/"

// The superseded close-and-recreate claim the first qualifier accepted, signed
// for the same consent; it must no longer qualify.
const withdrawalClaimCloseRecreateWire = "ARDI1DMzSdFi4yDP0IYaICyKyVJ+Uk0jjGBWlrFWz2ESef6LFgd8EmVLCCzLM2NFIP2Sb6NHUtV4SSc/CCPrgw6AAQAGEupKbGPinFIKvvVQexMuxfmVR3auvr57kkIe6mkURtIsZzAuSRiU10kupr5PkU6k9F+hQuZFhnyRZKJ21d128HZSajsgEPJwglRkW4qijaf+bRqk+oyuAgD41+rpoYgNVRFDmykluUiEtYwLkfDYx680inQ0lN5uX5RaAJVt31MbDt8DCodzgqE1zmF57M5R8bfWTiqnWWrAkZQnT3tBHYpThcWDQNJsQdqgsRwcO8Y1Z/boA3W5V5EzxciBTfxooozRKNWuHT9T2TYHXn5gFeX/9nV8+QCRI9HTJzOgyl9e9aTrVhjQn9co+5P34M1i0nEE2OPK2RXEfVtWWBF9b1dXc5fSYxDUGLiYgrLVbzXVS4pv7s5FXJiPvPE+drOLmI5+z+cVwY/CKTsqen5y0hlVl1KQl2P8ghS4f8mAVhE6TzMr6ndpNHXxHDbLP9p2iwdJNbim8AoYRL2/6IgLFizNA9n7PDD1Nf7t181z3fvHlP9uIIsG7dGiMYgeBKYinoyXJY9OJInxuz0QKRSODYMLWhOZ2v8QhASOe9jb6fhZxvp6877brTo9ZfNqq8l0MbG75MLS9uDkfKYCA0UvXWEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAZ+nHraWi0mg8kxgywyiNpakUsc8wgWYUL7AQFv9BxBDbRYjAAoLjkGvJD3kNRwaBHozCSynMqZG01w2zqljTk5oh0F2kwCL+pRFs/3QjknvqkcNItlC3+yI6WGsXR+4QMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDBQwGAAEADQ4SAQEPBgIADg8AA+cBitFAo09D6UwBAQAAAAcBAAAAAAAAAAMCAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAIBAQAAAEgAAAAAAAAABSAAAABkAAAAAAAAAAAAAAAAAGQAAAAAAAAAAAABAAAAAAAAAAABDbRYjAAoLjkGvJD3kNRwaBHozCSynMqZG01w2zqljTkBAAAAAAAAAAAAAAAFDAAAAAIAAAAAAAAAAAAAAAAAAAAAAAABAAAA6kpsY+KcUgq+9VB7Ey7F+ZVHdq6+vnuSQh7qaRRG0iwHAQAAAAAAAAAADwcDDwAEAA4FLFpRu1EnRoBOAgEBAQEBAAAAAAECFAAAAAECAwABAwwAAgAAAAAAAAAAAAAADxICDwAEBgEHCAkKCwUSDQwOEBFfWlG7USdGgE4CAQBQAAAABQkEAQoCAAoADPQBAAAAAAAABgkDAQAAAQAJCwYAAQAKDAkAAA0NAA4DCgQFBgcBCAkJDAgAhwfteJVeXwcJBAEKAgAKAAxkAAAAAAAAAAYPBgIADg8AAy+K0UCjT0PpTAEBAAAACRFDmykluUiEtYwLkfDYx680inQ0lN5uX5RaAJVt31MbAAHiTz4qllcehnay2i5IVBAMTSdPPOjIS+Dc4oaPyRqp7QABBQ=="

type withdrawalClaimGolden struct {
	Consent              WithdrawalConsent `json:"consent"`
	Wire                 string            `json:"wireBase64"`
	Receipt              string            `json:"receiptBase64"`
	Signature            string            `json:"signature"`
	Blockhash            string            `json:"blockhash"`
	LastValidBlockHeight uint64            `json:"lastValidBlockHeight"`
	Fee                  string            `json:"feeLamports"`
	Baseline             string            `json:"preexistingUsdcRaw"`
	Tables               []string          `json:"lookupTables"`
}

func capturedLookupTables(t *testing.T, want []string) []*chain.Account {
	t.Helper()
	raw, err := os.ReadFile(withdrawalClaimFixtures + "lookup-tables.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Addresses []string `json:"addresses"`
		Result    struct {
			Value []struct {
				Data []string `json:"data"`
			} `json:"value"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	var tables []*chain.Account
	for _, address := range want {
		for i, candidate := range captured.Addresses {
			if candidate != address {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(captured.Result.Value[i].Data[0])
			if err != nil {
				t.Fatal(err)
			}
			tables = append(tables, &chain.Account{Key: solana.MustPublicKeyFromBase58(address), Owner: solana.AddressLookupTableProgramID, Lamports: 1_000_000, Data: data})
		}
	}
	return tables
}

func loadWithdrawalClaimGolden(t *testing.T) (withdrawalClaimGolden, *solana.Transaction, *chain.Account, *chain.Account, []*chain.Account) {
	t.Helper()
	raw, err := os.ReadFile(withdrawalClaimFixtures + "claim-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var g withdrawalClaimGolden
	if err = json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	wire, err := base64.StdEncoding.DecodeString(g.Wire)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := canonicalUint(g.Fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = validateWithdrawalAttemptWire(g.Consent, wire, g.Signature, g.Blockhash, g.LastValidBlockHeight, fee); err != nil {
		t.Fatal(err)
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	owner := solana.MustPublicKeyFromBase58(g.Consent.Owner)
	settings := solana.MustPublicKeyFromBase58(g.Consent.Settings)
	if err = g.Consent.Validate(owner, settings, time.Unix(1_793_920_731, 0)); err != nil {
		t.Fatal(err)
	}
	authority, err := AuthorityOf(settings)
	if err != nil {
		t.Fatal(err)
	}
	receiptKey, _, err := voltr.WithdrawalReceiptAddress(Vault, authority.key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(g.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &chain.Account{Key: receiptKey, Owner: voltr.ProgramID, Lamports: 10_000_000, Data: data}
	source, err := spl.AssociatedTokenAddress(authority.key, AssetMint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := canonicalUint(g.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 165)
	copy(token, AssetMint[:])
	copy(token[32:], authority.key[:])
	binary.LittleEndian.PutUint64(token[64:], baseline)
	token[108] = 1
	asset := &chain.Account{Key: source, Owner: solana.TokenProgramID, Lamports: 2_039_280, Data: token}
	tables := capturedLookupTables(t, g.Tables)
	if err = resolveWithdrawalClaimLookups(tx, 1_000_000_000, tables); err != nil {
		t.Fatal(err)
	}
	return g, tx, receipt, asset, tables
}

func TestWithdrawalClaimProductionTSWireParity(t *testing.T) {
	g, tx, receipt, asset, _ := loadWithdrawalClaimGolden(t)
	if err := checkProtectedWithdrawalClaim(tx, g.Consent, receipt, asset); err != nil {
		t.Fatal(err)
	}
}
func TestWithdrawalClaimRejectsConsentStateAndWireTampering(t *testing.T) {
	mutations := map[string]func(*withdrawalClaimGolden, *solana.Transaction, *chain.Account, *chain.Account){
		"new receipt economics": func(_ *withdrawalClaimGolden, _ *solana.Transaction, r, _ *chain.Account) { r.Data[80] ^= 1 },
		"old receipt LP":        func(_ *withdrawalClaimGolden, _ *solana.Transaction, r, _ *chain.Account) { r.Data[72] ^= 1 },
		"deadline":              func(_ *withdrawalClaimGolden, _ *solana.Transaction, r, _ *chain.Account) { r.Data[96] ^= 1 },
		"receipt owner": func(_ *withdrawalClaimGolden, _ *solana.Transaction, r, _ *chain.Account) {
			r.Owner = solana.SystemProgramID
		},
		"receipt identity": func(_ *withdrawalClaimGolden, _ *solana.Transaction, r, _ *chain.Account) {
			r.Key = solana.PublicKey{31: 9}
		},
		"policy generation": func(g *withdrawalClaimGolden, _ *solana.Transaction, _, _ *chain.Account) { g.Consent.PolicySeed = "1" },
		"approved floor": func(g *withdrawalClaimGolden, _ *solana.Transaction, _, _ *chain.Account) {
			g.Consent.MinClaimUSDC = "101"
		},
		"baseline moved since signing": func(_ *withdrawalClaimGolden, _ *solana.Transaction, _, a *chain.Account) {
			binary.LittleEndian.PutUint64(a.Data[64:], 501)
		},
		"extra top-level instruction": func(_ *withdrawalClaimGolden, tx *solana.Transaction, _, _ *chain.Account) {
			tx.Message.Instructions = append(tx.Message.Instructions, tx.Message.Instructions[0])
		},
		"missing cleanup": func(_ *withdrawalClaimGolden, tx *solana.Transaction, _, _ *chain.Account) {
			tx.Message.Instructions = tx.Message.Instructions[:len(tx.Message.Instructions)-1]
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			g, tx, r, a, _ := loadWithdrawalClaimGolden(t)
			mutate(&g, tx, r, a)
			if err := checkProtectedWithdrawalClaim(tx, g.Consent, r, a); err == nil {
				t.Fatal("mutated protected operation accepted")
			}
		})
	}
	t.Run("finalized balance below the client's read", func(t *testing.T) {
		g, tx, r, a, _ := loadWithdrawalClaimGolden(t)
		binary.LittleEndian.PutUint64(a.Data[64:], 493)
		if err := checkProtectedWithdrawalClaim(tx, g.Consent, r, a); err != nil {
			t.Fatalf("claim moving more than the evidenced balance plus floor refused: %v", err)
		}
	})
	t.Run("superseded close and recreate wire", func(t *testing.T) {
		g, _, r, a, tables := loadWithdrawalClaimGolden(t)
		wire, err := base64.StdEncoding.DecodeString(withdrawalClaimCloseRecreateWire)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := solana.TransactionFromBytes(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err = resolveWithdrawalClaimLookups(tx, 1_000_000_000, tables); err != nil {
			t.Fatal(err)
		}
		if err = checkProtectedWithdrawalClaim(tx, g.Consent, r, a); err == nil {
			t.Fatal("superseded close-and-recreate claim accepted")
		}
	})
}
func TestWithdrawalClaimLookupAndSignatureTampering(t *testing.T) {
	for _, kind := range []string{"owner", "deactivation", "unwarmed", "wrong loaded key"} {
		t.Run(kind, func(t *testing.T) {
			g, _, _, _, tables := loadWithdrawalClaimGolden(t)
			wire, _ := base64.StdEncoding.DecodeString(g.Wire)
			tx, _ := solana.TransactionFromBytes(wire)
			switch kind {
			case "owner":
				tables[0].Owner = solana.SystemProgramID
			case "deactivation":
				binary.LittleEndian.PutUint64(tables[0].Data[4:], 0)
			case "unwarmed":
				binary.LittleEndian.PutUint64(tables[0].Data[12:], 1_000_000_000)
			case "wrong loaded key":
				used := tx.Message.AddressTableLookups[0].ReadonlyIndexes
				if len(used) == 0 {
					used = tx.Message.AddressTableLookups[0].WritableIndexes
				}
				tables[0].Data[56+int(used[0])*32] ^= 1
			}
			err := resolveWithdrawalClaimLookups(tx, 1_000_000_000, tables)
			if kind == "wrong loaded key" && err == nil {
				_, _, r, a, _ := loadWithdrawalClaimGolden(t)
				err = checkProtectedWithdrawalClaim(tx, g.Consent, r, a)
			}
			if err == nil {
				t.Fatal("tampered ALT accepted")
			}
		})
	}
	g, _, _, _, _ := loadWithdrawalClaimGolden(t)
	wire, _ := base64.StdEncoding.DecodeString(g.Wire)
	wire[1] ^= 1
	fee, _ := canonicalUint(g.Fee)
	if _, err := validateWithdrawalAttemptWire(g.Consent, wire, g.Signature, g.Blockhash, g.LastValidBlockHeight, fee); err == nil {
		t.Fatal("invalid signature accepted")
	}
	if _, err := validateWithdrawalAttemptWire(g.Consent, []byte(strings.Repeat("x", 1233)), g.Signature, g.Blockhash, g.LastValidBlockHeight, fee); err == nil {
		t.Fatal("oversized wire accepted")
	}
}

func TestWithdrawalClaimGenerationNeedsOnlyNewestReceiptRequest(t *testing.T) {
	for _, kind := range []string{"full older history", "latest cancellation", "later request same transaction", "wrong invocation ordinal"} {
		t.Run(kind, func(t *testing.T) {
			g, _, _, _, _ := loadWithdrawalClaimGolden(t)
			owner := solana.MustPublicKeyFromBase58(g.Consent.Owner)
			settings := solana.MustPublicKeyFromBase58(g.Consent.Settings)
			authority, _ := AuthorityOf(settings)
			user, _ := voltr.UserAccounts(Vault, LPMint, authority.key)
			escrow, _ := spl.AssociatedTokenAddress(user.Receipt, LPMint, solana.TokenProgramID)
			data := append([]byte{}, voltr.RequestWithdrawVaultDiscriminator[:]...)
			data = binary.LittleEndian.AppendUint64(data, 100)
			data = append(data, 1, 0)
			request := claimInstruction(voltr.ProgramID, data, owner, authority.key, solana.PublicKey{31: 44}, Vault, LPMint, user.LPATA, escrow, user.Receipt, solana.TokenProgramID, solana.SystemProgramID)
			metas := make([]*solana.AccountMeta, len(request.Accounts))
			for i, a := range request.Accounts {
				value := a
				metas[i] = &value
			}
			metas[0].IsSigner = true
			metas[1].IsSigner = true
			instructions := []solana.Instruction{solana.NewInstruction(request.ProgramID, metas, request.Data)}
			signature := solana.MustSignatureFromBase58(g.Consent.RequestSignature)
			if kind == "latest cancellation" {
				instructions = []solana.Instruction{solana.NewInstruction(voltr.ProgramID, []*solana.AccountMeta{{PublicKey: user.Receipt}}, voltr.CancelRequestWithdrawVaultDiscriminator[:])}
				signature = solana.Signature{31: 99}
			}
			if kind == "later request same transaction" {
				instructions = append(instructions, instructions[0])
			}
			if kind == "wrong invocation ordinal" {
				g.Consent.RequestInstruction = 1
			}
			transaction, err := solana.NewTransaction(instructions, solana.Hash{31: 88}, solana.TransactionPayer(owner))
			if err != nil {
				t.Fatal(err)
			}
			transaction.Signatures = make([]solana.Signature, transaction.Message.Header.NumRequiredSignatures)
			transaction.Signatures[0] = signature
			source := &claimGenerationFixtureSource{execution: chain.Execution{Receipt: chain.Receipt{Keys: transaction.Message.AccountKeys, Slot: 1}, Transaction: transaction}}
			source.rows = append(source.rows, chain.Signed{Signature: signature, Slot: 1})
			for i := 1; i < historyLimit; i++ {
				source.rows = append(source.rows, chain.Signed{Signature: solana.Signature{0: byte(i), 1: 1}, Slot: 1})
			}
			err = verifyProtectedWithdrawalGeneration(context.Background(), source, g.Consent, 1000)

			if kind == "full older history" {
				if err != nil {
					t.Fatal(err)
				}
				if source.calls != 1 {
					t.Fatalf("fetched %d transactions, want newest request only", source.calls)
				}
			} else if err == nil {
				t.Fatal("superseded or incorrect generation accepted")
			}
		})
	}
}

type claimGenerationFixtureSource struct {
	rows      []chain.Signed
	execution chain.Execution
	calls     int
}

func (f *claimGenerationFixtureSource) History(context.Context, solana.PublicKey, int, solana.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error) {
	return f.rows, nil
}
func (f *claimGenerationFixtureSource) Execution(context.Context, solana.Signature, rpc.CommitmentType) (chain.Execution, error) {
	f.calls++
	return f.execution, nil
}
