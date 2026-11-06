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
	"strings"
	"testing"
	"time"
)

const withdrawalClaimTSGolden = `{"consent":{"authority":"213wZCPaVWK6Jz37Mi5um7p6ZxgKkXWX6VZyVBtRA2cq","cluster":"mainnet-beta","destination":"GmaDrppBC7P5ARKV8g3djiwP89vz1jLK23V2GBjuAEGB","expiresAt":"4102444800","feeBasisHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","lpRaw":"100","maxUserFeeLamports":"1000000","minClaimUsdcRaw":"100","nonce":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","owner":"GmaDrppBC7P5ARKV8g3djiwP89vz1jLK23V2GBjuAEGB","ownerSignature":"+2ax3TGZqNOI2kLeq4bKZOIgkEqek6iFrwoaShveodw8Bwgi3LDg9fttuk0KgEKNZwt/LKE/RcGN7iIDlGgQCA==","program":"vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8","referenceSlot":"1","referenceUsdcRaw":"100","requestInstruction":0,"requestSignature":"2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6ijwfYmfZYsKRxboQMPh3R4kUhXRVdtSXFXMheka4Rc4P2","settings":"6YiPBScD6WZKkM4YRp9ui33W79zynP2hNsGQhMY8z9it","vault":"HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA","version":"1","policySeed":"0"},"wireBase64":"ARDI1DMzSdFi4yDP0IYaICyKyVJ+Uk0jjGBWlrFWz2ESef6LFgd8EmVLCCzLM2NFIP2Sb6NHUtV4SSc/CCPrgw6AAQAGEupKbGPinFIKvvVQexMuxfmVR3auvr57kkIe6mkURtIsZzAuSRiU10kupr5PkU6k9F+hQuZFhnyRZKJ21d128HZSajsgEPJwglRkW4qijaf+bRqk+oyuAgD41+rpoYgNVRFDmykluUiEtYwLkfDYx680inQ0lN5uX5RaAJVt31MbDt8DCodzgqE1zmF57M5R8bfWTiqnWWrAkZQnT3tBHYpThcWDQNJsQdqgsRwcO8Y1Z/boA3W5V5EzxciBTfxooozRKNWuHT9T2TYHXn5gFeX/9nV8+QCRI9HTJzOgyl9e9aTrVhjQn9co+5P34M1i0nEE2OPK2RXEfVtWWBF9b1dXc5fSYxDUGLiYgrLVbzXVS4pv7s5FXJiPvPE+drOLmI5+z+cVwY/CKTsqen5y0hlVl1KQl2P8ghS4f8mAVhE6TzMr6ndpNHXxHDbLP9p2iwdJNbim8AoYRL2/6IgLFizNA9n7PDD1Nf7t181z3fvHlP9uIIsG7dGiMYgeBKYinoyXJY9OJInxuz0QKRSODYMLWhOZ2v8QhASOe9jb6fhZxvp6877brTo9ZfNqq8l0MbG75MLS9uDkfKYCA0UvXWEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAZ+nHraWi0mg8kxgywyiNpakUsc8wgWYUL7AQFv9BxBDbRYjAAoLjkGvJD3kNRwaBHozCSynMqZG01w2zqljTk5oh0F2kwCL+pRFs/3QjknvqkcNItlC3+yI6WGsXR+4QMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDBQwGAAEADQ4SAQEPBgIADg8AA+cBitFAo09D6UwBAQAAAAcBAAAAAAAAAAMCAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAIBAQAAAEgAAAAAAAAABSAAAABkAAAAAAAAAAAAAAAAAGQAAAAAAAAAAAABAAAAAAAAAAABDbRYjAAoLjkGvJD3kNRwaBHozCSynMqZG01w2zqljTkBAAAAAAAAAAAAAAAFDAAAAAIAAAAAAAAAAAAAAAAAAAAAAAABAAAA6kpsY+KcUgq+9VB7Ey7F+ZVHdq6+vnuSQh7qaRRG0iwHAQAAAAAAAAAADwcDDwAEAA4FLFpRu1EnRoBOAgEBAQEBAAAAAAECFAAAAAECAwABAwwAAgAAAAAAAAAAAAAADxICDwAEBgEHCAkKCwUSDQwOEBFfWlG7USdGgE4CAQBQAAAABQkEAQoCAAoADPQBAAAAAAAABgkDAQAAAQAJCwYAAQAKDAkAAA0NAA4DCgQFBgcBCAkJDAgAhwfteJVeXwcJBAEKAgAKAAxkAAAAAAAAAAYPBgIADg8AAy+K0UCjT0PpTAEBAAAACRFDmykluUiEtYwLkfDYx680inQ0lN5uX5RaAJVt31MbAAHiTz4qllcehnay2i5IVBAMTSdPPOjIS+Dc4oaPyRqp7QABBQ==","receiptBase64":"y1Hfja9sZXL1pOtWGNCf1yj7k/fgzWLScQTY48rZFcR9W1ZYEX1vVw7fAwqHc4KhNc5heezOUfG31k4qp1lqwJGUJ097QR2KZAAAAAAAAAAAAAAAAABkAAAAAAAAAAAAAQAAAAAAAAD8AAAAAAAAAA==","signature":"LTsqmRgCv1HyHmhkaSUnDyzby8jHrkPYLX8mHgPW4t5B9Uhgzfp7G4F1ZqUqKZrjx4zB1Nt5wpoWWBhUsHnbmSM","blockhash":"CktRuQ2mttgRGkXJtyksdKHjUdc2C4TgDzyB98oEzy8","lastValidBlockHeight":100000,"feeLamports":"5000","lookupTables":[{"address":"GERDuMq2KXct84sG9JUxuRguqfcPvn71gYXg2p3LLHv4","metadataBase64":"AQAAAP//////////Tn/qFQAAAAAsAX0Oo0JAUwU7u9hg90wy8EvD3Icq35pPTtFwwk7g9D7TAAA=","entries":[{"index":5,"address":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}]}]}`

type withdrawalClaimGolden struct {
	Consent                             WithdrawalConsent `json:"consent"`
	Wire, Receipt, Signature, Blockhash string
	LastValidBlockHeight                uint64 `json:"lastValidBlockHeight"`
	Fee                                 string `json:"feeLamports"`
	Tables                              []struct {
		Address  string `json:"address"`
		Metadata string `json:"metadataBase64"`
		Entries  []struct {
			Index   int    `json:"index"`
			Address string `json:"address"`
		} `json:"entries"`
	} `json:"lookupTables"`
}

func loadWithdrawalClaimGolden(t *testing.T, lowRent ...bool) (withdrawalClaimGolden, *solana.Transaction, *chain.Account, *chain.Account, []*chain.Account) {
	t.Helper()
	var fields struct {
		withdrawalClaimGolden
		Wire    string `json:"wireBase64"`
		Receipt string `json:"receiptBase64"`
	}
	if err := json.Unmarshal([]byte(withdrawalClaimTSGolden), &fields); err != nil {
		t.Fatal(err)
	}
	g := fields.withdrawalClaimGolden
	g.Wire = fields.Wire
	g.Receipt = fields.Receipt
	if len(lowRent) > 0 && lowRent[0] {
		var variant struct {
			Wire      string `json:"wireBase64"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal([]byte(withdrawalClaimTSLowRent), &variant); err != nil {
			t.Fatal(err)
		}
		g.Wire = variant.Wire
		g.Signature = variant.Signature
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
	token := make([]byte, 165)
	copy(token, AssetMint[:])
	copy(token[32:], authority.key[:])
	binary.LittleEndian.PutUint64(token[64:], 500)
	token[108] = 1
	asset := &chain.Account{Key: source, Owner: solana.TokenProgramID, Lamports: 2_039_280, Data: token}
	var tables []*chain.Account
	for _, table := range g.Tables {
		metadata, err := base64.StdEncoding.DecodeString(table.Metadata)
		if err != nil {
			t.Fatal(err)
		}
		maxIndex := 0
		for _, entry := range table.Entries {
			maxIndex = max(maxIndex, entry.Index)
		}
		raw := make([]byte, 56+(maxIndex+1)*32)
		copy(raw, metadata)
		for _, entry := range table.Entries {
			key := solana.MustPublicKeyFromBase58(entry.Address)
			copy(raw[56+entry.Index*32:], key[:])
		}
		tables = append(tables, &chain.Account{Key: solana.MustPublicKeyFromBase58(table.Address), Owner: solana.AddressLookupTableProgramID, Lamports: 1_000_000, Data: raw})
	}
	if err = resolveWithdrawalClaimLookups(tx, 1_000_000_000, tables); err != nil {
		t.Fatal(err)
	}
	return g, tx, receipt, asset, tables
}

func TestWithdrawalClaimProductionTSWireParity(t *testing.T) {
	g, tx, receipt, asset, _ := loadWithdrawalClaimGolden(t)
	if err := checkProtectedWithdrawalClaim(tx, g.Consent, receipt, asset, 3_000_000, 2_039_280); err != nil {
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
			g.Consent.MinClaimUSDC = "99"
		},
		"old cash subsidy": func(_ *withdrawalClaimGolden, _ *solana.Transaction, _, a *chain.Account) {
			binary.LittleEndian.PutUint64(a.Data[64:], 501)
		},
		"extra top-level instruction": func(_ *withdrawalClaimGolden, tx *solana.Transaction, _, _ *chain.Account) {
			tx.Message.Instructions = append(tx.Message.Instructions, tx.Message.Instructions[0])
		},
		"missing cleanup": func(_ *withdrawalClaimGolden, tx *solana.Transaction, _, _ *chain.Account) {
			tx.Message.Instructions = tx.Message.Instructions[:len(tx.Message.Instructions)-1]
		},
		"inner floor": func(_ *withdrawalClaimGolden, tx *solana.Transaction, _, _ *chain.Account) {
			last := len(tx.Message.Instructions) - 2
			data := tx.Message.Instructions[last].Data
			data[len(data)-2] ^= 1
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			g, tx, r, a, _ := loadWithdrawalClaimGolden(t)
			mutate(&g, tx, r, a)
			if err := checkProtectedWithdrawalClaim(tx, g.Consent, r, a, 3_000_000, 2_039_280); err == nil {
				t.Fatal("mutated protected operation accepted")
			}
		})
	}
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
				tables[0].Data[len(tables[0].Data)-1] ^= 1
			}
			err := resolveWithdrawalClaimLookups(tx, 1_000_000_000, tables)
			if kind == "wrong loaded key" && err == nil {
				_, _, r, a, _ := loadWithdrawalClaimGolden(t)
				err = checkProtectedWithdrawalClaim(tx, g.Consent, r, a, 3_000_000, 2_039_280)
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

const withdrawalClaimTSLowRent = `{"wireBase64":"AcN3SfhFoPy5Fv3d6TFW5Y3XWKNTXFRTpdOzuF2s9oCK4DVfAHZPiLuuLBKOQ70fT6T0HyB9Lc/WgWKY4CoFNQGAAQAGEupKbGPinFIKvvVQexMuxfmVR3auvr57kkIe6mkURtIsZzAuSRiU10kupr5PkU6k9F+hQuZFhnyRZKJ21d128HYO3wMKh3OCoTXOYXnszlHxt9ZOKqdZasCRlCdPe0EdilJqOyAQ8nCCVGRbiqKNp/5tGqT6jK4CAPjX6umhiA1VEUObKSW5SIS1jAuR8NjHrzSKdDSU3m5flFoAlW3fUxtThcWDQNJsQdqgsRwcO8Y1Z/boA3W5V5EzxciBTfxooozRKNWuHT9T2TYHXn5gFeX/9nV8+QCRI9HTJzOgyl9e9aTrVhjQn9co+5P34M1i0nEE2OPK2RXEfVtWWBF9b1dXc5fSYxDUGLiYgrLVbzXVS4pv7s5FXJiPvPE+drOLmI5+z+cVwY/CKTsqen5y0hlVl1KQl2P8ghS4f8mAVhE6TzMr6ndpNHXxHDbLP9p2iwdJNbim8AoYRL2/6IgLFizNA9n7PDD1Nf7t181z3fvHlP9uIIsG7dGiMYgeBKYinoyXJY9OJInxuz0QKRSODYMLWhOZ2v8QhASOe9jb6fhZxvp6877brTo9ZfNqq8l0MbG75MLS9uDkfKYCA0UvXWEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAZ+nHraWi0mg8kxgywyiNpakUsc8wgWYUL7AQFv9BxBDbRYjAAoLjkGvJD3kNRwaBHozCSynMqZG01w2zqljTk5oh0F2kwCL+pRFs/3QjknvqkcNItlC3+yI6WGsXR+4QMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDBgwGAAEADQ4SAQEOAgACDAIAAADwHR8AAAAAAA8GAwAODwAE5wGK0UCjT0PpTAEBAAAABwEAAAAAAAAAAwIBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAAAAgEBAAAASAAAAAAAAAAFIAAAAGQAAAAAAAAAAAAAAAAAZAAAAAAAAAAAAAEAAAAAAAAAAAENtFiMACguOQa8kPeQ1HBoEejMJLKcypkbTXDbOqWNOQEAAAAAAAAAAAAAAAUMAAAAAgAAAAAAAAAAAAAAAAAAAAAAAAEAAADqSmxj4pxSCr71UHsTLsX5lUd2rr6+e5JCHuppFEbSLAcBAAAAAAAAAAAPBwQPAAIADgUsWlG7USdGgE4CAQEBAQEAAAAAAQIUAAAAAQIDAAEDDAACAAAAAAAAAAAAAAAPEgMPAAIGAQcICQoLBRINDA4QEV9aUbtRJ0aATgIBAFAAAAAFCQQBCgIACgAM9AEAAAAAAAAGCQMBAAABAAkLBgABAAoMCQAADQ0ADgMKBAUGBwEICQkMCACHB+14lV5fBwkEAQoCAAoADGQAAAAAAAAABg8GAwAODwAEL4rRQKNPQ+lMAQEAAAAJEUObKSW5SIS1jAuR8NjHrzSKdDSU3m5flFoAlW3fUxsAAeJPPiqWVx6GdrLaLkhUEAxNJ0886MhL4Nziho/JGqntAAEF","signature":"4ufUuv5sVCq9NNBMpawbXpZYpdL1m2JZU8qYy8NeNbPzVNgyg2A7XuDqnWBcHMTSyHTeYhJPjHdjh1PgDsechHjn"}`

func TestWithdrawalClaimProductionTSRentTopupParity(t *testing.T) {
	g, tx, receipt, asset, _ := loadWithdrawalClaimGolden(t, true)
	if err := checkProtectedWithdrawalClaim(tx, g.Consent, receipt, asset, 0, 2_039_280); err != nil {
		t.Fatal(err)
	}
	if err := checkProtectedWithdrawalClaim(tx, g.Consent, receipt, asset, 1, 2_039_280); err == nil {
		t.Fatal("excess rent top-up accepted")
	}
}
