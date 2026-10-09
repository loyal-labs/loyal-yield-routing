package earn

// Parity with the retired Rust implementation. The golden files are the
// recorded output of loyal-actions and the Rust policy monitor; their
// generator was deleted with the Rust workers.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

type goldenInstruction struct {
	ProgramID string `json:"program_id"`
	Accounts  []struct {
		Pubkey     string `json:"pubkey"`
		IsSigner   bool   `json:"is_signer"`
		IsWritable bool   `json:"is_writable"`
	} `json:"accounts"`
	Data string `json:"data"`
}

func (g goldenInstruction) decode(t *testing.T) squads.Instruction {
	t.Helper()
	data, err := hex.DecodeString(g.Data)
	if err != nil {
		t.Fatal(err)
	}
	out := squads.Instruction{ProgramID: solana.MustPublicKeyFromBase58(g.ProgramID), Data: data}
	for _, account := range g.Accounts {
		out.Accounts = append(out.Accounts, solana.AccountMeta{PublicKey: solana.MustPublicKeyFromBase58(account.Pubkey), IsSigner: account.IsSigner, IsWritable: account.IsWritable})
	}
	return out
}

func canonicalJSON(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPolicyDetectionMatchesRustMonitor(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/earn/policy-detection.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Name        string            `json:"name"`
			Instruction goldenInstruction `json:"instruction"`
			Events      json.RawMessage   `json:"events"`
		} `json:"cases"`
		EarnMax []struct {
			Settings       string `json:"settings"`
			PolicySeedBase uint64 `json:"policy_seed_base"`
			Delegate       string `json:"delegate"`
			Families       []struct {
				Family string            `json:"family"`
				Create goldenInstruction `json:"create"`
				Update goldenInstruction `json:"update"`
			} `json:"families"`
		} `json:"earn_max"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	monitor := &PolicyMonitor{cluster: "mainnet-beta"}
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var events []map[string]any
			for _, event := range monitor.events("golden-signature", 77, c.Instruction.decode(t)) {
				switch {
				case event.route != nil:
					events = append(events, map[string]any{"kind": "route", "input": event.route})
				case event.setup != nil:
					events = append(events, map[string]any{"kind": "setup", "input": event.setup})
				case event.sweep != nil:
					events = append(events, map[string]any{"kind": "sweep", "input": event.sweep})
				case event.crossMint != nil:
					events = append(events, map[string]any{"kind": "cross_mint", "input": event.crossMint})
				default:
					events = append(events, map[string]any{"kind": "removal", "input": event.removal})
				}
			}
			if events == nil {
				events = []map[string]any{}
			}
			var want any
			decoder := json.NewDecoder(bytes.NewReader(c.Events))
			decoder.UseNumber()
			if err := decoder.Decode(&want); err != nil {
				t.Fatal(err)
			}
			if got := canonicalJSON(t, events); !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				t.Fatalf("policy events differ from Rust\n got %s\nwant %s", gotJSON, c.Events)
			}
		})
	}
	for _, set := range golden.EarnMax {
		settings, delegate := solana.MustPublicKeyFromBase58(set.Settings), solana.MustPublicKeyFromBase58(set.Delegate)
		topology, err := multiply.DeriveEarnMaxTopology(settings, set.PolicySeedBase)
		if err != nil {
			t.Fatal(err)
		}
		strategy, err := topology.Strategy(multiply.SyrupUsdcUsdc)
		if err != nil {
			t.Fatal(err)
		}
		monitor := &PolicyMonitor{cluster: "mainnet-beta", delegate: delegate}
		for index, family := range set.Families {
			if string(earnMaxFamilies[index]) != family.Family {
				t.Fatalf("family order %s differs from Rust %s", earnMaxFamilies[index], family.Family)
			}
			constraints, err := multiply.CanonicalConstraints(topology, earnMaxFamilies[index])
			if err != nil {
				t.Fatal(err)
			}
			policy := familyPolicy(strategy, earnMaxFamilies[index])
			update, err := squads.EncodeCompactPolicyUpdate(policy.Account, delegate, 0, constraints)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(update, family.Update.decode(t).Data) {
				t.Fatalf("%s %s canonical update bytes differ from Rust", set.Settings, family.Family)
			}
			actions, err := squads.DecodeSettingsActions(family.Create.decode(t))
			if err != nil || len(actions) != 1 {
				t.Fatalf("decode canonical create: %v %d", err, len(actions))
			}
			base, ok, err := monitor.earnMaxSeedBase(actions[0])
			if err != nil || !ok || base != set.PolicySeedBase {
				t.Fatalf("%s create seed base = %d %v %v, want %d", family.Family, base, ok, err, set.PolicySeedBase)
			}
		}
	}
}
