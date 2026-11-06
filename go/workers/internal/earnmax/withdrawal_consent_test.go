package earnmax

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func signedConsent(t *testing.T) (WithdrawalConsent, solana.PublicKey, solana.PublicKey, time.Time) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	owner := solana.PublicKeyFromBytes(pub)
	settings := solana.NewWallet().PublicKey()
	a, err := AuthorityOf(settings)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	c := WithdrawalConsent{WithdrawalGeneration: WithdrawalGeneration{RequestSignature: solana.Signature{1}.String(), RequestInstruction: 2}, Version: "1", PolicySeed: "0", Owner: owner.String(), Cluster: Cluster, Program: voltr.ProgramID.String(), Vault: Vault.String(), Settings: settings.String(), Authority: a.Key().String(), Destination: owner.String(), LPRaw: "100", ReferenceUSDC: "1000", ReferenceSlot: "123", FeeBasisHash: strings.Repeat("a", 64), MinClaimUSDC: "990", MaxUserFeeLamports: "5000", ExpiresAt: "2000", Nonce: strings.Repeat("b", 64)}
	message, err := c.CanonicalMessage()
	if err != nil {
		t.Fatal(err)
	}
	c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, message))
	return c, owner, settings, now
}

func TestWithdrawalConsentSignatureCannotWidenOrCrossGeneration(t *testing.T) {
	c, owner, settings, now := signedConsent(t)
	if err := c.Validate(owner, settings, now); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*WithdrawalConsent){"floor": func(c *WithdrawalConsent) { c.MinClaimUSDC = "980" }, "generation": func(c *WithdrawalConsent) { c.RequestInstruction++ }, "network": func(c *WithdrawalConsent) { c.Cluster = "devnet" }, "fees": func(c *WithdrawalConsent) { c.MaxUserFeeLamports = "6000" }, "destination": func(c *WithdrawalConsent) { c.Destination = solana.NewWallet().PublicKey().String() }} {
		t.Run(name, func(t *testing.T) {
			changed := c
			change(&changed)
			if changed.Validate(owner, settings, now) == nil {
				t.Fatal("changed signed consent accepted")
			}
		})
	}
	if c.Validate(owner, settings, time.Unix(2000, 0)) == nil {
		t.Fatal("expired approval accepted")
	}
	if c.Validate(solana.NewWallet().PublicKey(), settings, now) == nil {
		t.Fatal("wrong authenticated owner accepted")
	}
}

func TestWithdrawalAdmissionPartialFundingAndUnknown(t *testing.T) {
	c, _, _, now := signedConsent(t)
	payable := USDCRaw(1000)
	a := WithdrawalAdmission{ProjectedPostFeePayable: &payable, IncrementalReservedCost: 4, NewIncrementalCost: 6, ConsumedUserFeeLamports: 1000, ReservedUserFeeLamports: 2000, NewUserFeeLamports: 2000}
	if reason, err := c.Admit(a, now); err != nil || reason != "" {
		t.Fatalf("exact floor: %q %v", reason, err)
	}
	a.NewIncrementalCost = 7
	if reason, _ := c.Admit(a, now); reason != WithdrawalWaitingPrice {
		t.Fatalf("under floor: %q", reason)
	}
	a.NewIncrementalCost = 6
	a.NewUserFeeLamports = 2001
	if reason, _ := c.Admit(a, now); reason != WithdrawalFeeBudget {
		t.Fatalf("fee budget: %q", reason)
	}
	a.ProjectedPostFeePayable = nil
	if reason, _ := c.Admit(a, now); reason != WithdrawalUnknownCost {
		t.Fatalf("unknown: %q", reason)
	}
}

func TestWithdrawalCostAllocationSumsAndOrderStable(t *testing.T) {
	for cost := USDCRaw(0); cost < 100; cost++ {
		participants := []WithdrawalCostParticipant{{"b", 2}, {"a", 3}}
		got, err := AllocateWithdrawalCost(cost, 7, participants)
		if err != nil {
			t.Fatal(err)
		}
		sum := got.OtherHolders
		for _, s := range got.Requests {
			sum += s.Cost
		}
		if sum != cost {
			t.Fatal("allocation loses cost", cost, got)
		}
		participants[0], participants[1] = participants[1], participants[0]
		reverse, err := AllocateWithdrawalCost(cost, 7, participants)
		if err != nil || !reflect.DeepEqual(got, reverse) {
			t.Fatal("allocation depends on observation order", strconv.Itoa(int(cost)))
		}
	}
	if _, err := AllocateWithdrawalCost(10, 7, []WithdrawalCostParticipant{{"a", 4}, {"b", 4}}); err == nil {
		t.Fatal("overlapping LP accepted")
	}
	if _, err := AllocateWithdrawalCost(10, 7, []WithdrawalCostParticipant{{"a", 1}, {"a", 1}}); err == nil {
		t.Fatal("duplicate participant accepted")
	}
}
