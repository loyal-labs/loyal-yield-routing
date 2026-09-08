package backyardrwa

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

// Test-only, zero-signature bridge compiler used by the connected deployed-SBF
// experiment. It has no RPC, signer, persistence or production admission path.
// Discovery wires identify accounts only; execution requires raw local prestate.
func TestExportOnReBridgeProbe(t *testing.T) {
	input := os.Getenv("PHASE3_ONRE_BRIDGE_INPUT")
	if input == "" {
		t.Skip("explicit local bridge request required")
	}
	var in struct {
		Discover bool   `json:"discover"`
		Action   Action `json:"action"`
		Slot     int64  `json:"slot"`
		Amount   uint64 `json:"amountRaw"`
		Accounts []struct {
			Address string `json:"address"`
			Owner   string `json:"owner"`
			Present bool   `json:"present"`
			Data    string `json:"dataBase64"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]string{}
	for _, p := range manifest.RuntimeBindings.BridgePolicies {
		policies[p.Account] = *p.DataSHA256
	}
	actions := []Action{in.Action}
	if in.Discover {
		actions = []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV}
		in.Slot = 1
	}
	addresses := map[string]bool{}
	rows := []any{}
	for _, action := range actions {
		amountRaw := in.Amount
		if in.Discover && action != ReportNAV {
			amountRaw = 101000
		}
		nav := NAVReportInput{SnapshotDigest: sha256Bytes([]byte("DISCOVERY_ONLY_NOT_NAV_PROOF"))}
		if !in.Discover {
			components := []NAVComponent{}
			balances := map[string]uint64{}
			for address, authority := range map[string]string{bridgeIdleATA: bridgeIdleAuthority, bridgeStrategyATA: bridgeStrategyAuth, bridgeSquadsATA: bridgeVault} {
				matches := 0
				for _, a := range in.Accounts {
					if a.Address != address {
						continue
					}
					matches++
					data, e := base64.StdEncoding.DecodeString(a.Data)
					if e != nil || !a.Present {
						t.Fatal("missing local custody")
					}
					custody, e := DecodeTokenCustody(a.Owner, data, mustKey(bridgeUSDC), mustKey(authority))
					if e != nil || custody.Raw > bridgeMaxNAV {
						t.Fatal("invalid local custody")
					}
					balances[address] = custody.Raw
					if address != bridgeIdleATA {
						components = append(components, NAVComponent{Account: address, Owner: authority, Raw: int64(custody.Raw), Slot: in.Slot, Known: true})
					}
				}
				if matches != 1 {
					t.Fatal("missing or duplicate custody")
				}
			}
			nav, err = ComputeNAV(NAVSnapshotContext{Slot: in.Slot, ReceiptFingerprint: sha256Bytes([]byte(input)), ManifestSHA256: manifest.SHA256, PolicyCatalogSHA256: *manifest.PolicyCatalog.SHA256}, components)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case VoltrAllocateToSquads:
				if nav.Raw != 0 || balances[bridgeIdleATA] < amountRaw {
					t.Fatal("allocation requires flat funded bridge")
				}
				nav.Raw += int64(amountRaw)
			case StageSquadsToVoltr:
				if amountRaw != balances[bridgeSquadsATA] || balances[bridgeStrategyATA] != 0 {
					t.Fatal("stage complete actual cash only")
				}
			case VoltrRestoreIdle:
				if amountRaw != balances[bridgeStrategyATA] || balances[bridgeSquadsATA] != 0 {
					t.Fatal("restore complete staged cash only")
				}
				nav.Raw -= int64(amountRaw)
			case ReportNAV:
				if amountRaw != 0 {
					t.Fatal("NAV must not move capital")
				}
			default:
				t.Fatal("unsupported bridge action")
			}
		}
		request := BridgeBuildRequest{Action: action, AmountRaw: amountRaw, Report: BridgeReport{Sequence: uint64(in.Slot), ObservedSlot: uint64(in.Slot), NAVAfterRaw: uint64(nav.Raw), SnapshotDigest: nav.SnapshotDigest}, AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
		message, e := CompileBridgeMessage(request)
		if e != nil {
			t.Fatal(e)
		}
		inner, policy, _, e := ticketedBridgeInstructions(request)
		if e != nil {
			t.Fatal(e)
		}
		addresses[encodeBase58(policy[:])] = true
		addresses[bridgeDelegate] = true
		for _, ix := range inner {
			addresses[encodeBase58(ix.program[:])] = true
			for _, a := range ix.accounts {
				addresses[encodeBase58(a.key[:])] = true
			}
		}
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		rows = append(rows, map[string]any{"request": request, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)})
	}
	keys := []string{}
	for a := range addresses {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	data, err := json.Marshal(map[string]any{"broadcast": false, "signatureProof": false, "discoveryOnly": in.Discover, "addresses": keys, "policies": policies, "steps": rows})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ONRE_BRIDGE_JSON=" + string(data))
}
