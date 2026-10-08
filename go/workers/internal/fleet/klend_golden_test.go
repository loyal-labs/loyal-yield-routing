package fleet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// testdata/klend/golden.json is the recorded byte-parity output of the retired
// Rust loyal-klend-proxy (c22e1094, klend-interface 23b9f2b). The generator was
// deleted with the Rust workers; the record is fixed.
const klendGoldenPath = "../../testdata/klend/golden.json"

type klendGoldenCase struct {
	Name      string          `json:"name"`
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type klendGoldenInstruction struct {
	Step     string               `json:"step"`
	Program  string               `json:"program"`
	Accounts []InstructionAccount `json:"accounts"`
	DataHex  string               `json:"dataHex"`
}

type klendGoldenOutput struct {
	SchemaVersion int    `json:"schemaVersion"`
	Operation     string `json:"operation"`
	Route         struct {
		Public    []klendGoldenInstruction `json:"public"`
		Protected []klendGoldenInstruction `json:"protected"`
	} `json:"route"`
}

func klendGoldenRoute(operation string, route KaminoSameMintRoute) klendGoldenOutput {
	encode := func(list []RouteInstruction) []klendGoldenInstruction {
		out := []klendGoldenInstruction{}
		for _, ix := range list {
			out = append(out, klendGoldenInstruction{ix.Step, ix.Program, ix.Accounts, hex.EncodeToString(ix.Data)})
		}
		return out
	}
	var out klendGoldenOutput
	out.SchemaVersion, out.Operation = 1, operation
	out.Route.Public, out.Route.Protected = encode(route.Public), encode(route.Protected)
	return out
}

func buildKLendGolden(operation string, raw json.RawMessage) (KaminoSameMintRoute, error) {
	decode := func(out any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		return decoder.Decode(out)
	}
	switch operation {
	case "buildCrossMintLegs":
		var r KaminoSameMintRouteRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildCrossMintLegs(r)
	case "buildIdleDeposit":
		var r KaminoIdleDepositRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildIdleDeposit(r)
	case "buildDestinationSetup":
		var r DestinationSetupRequest
		if err := decode(&r); err != nil {
			return KaminoSameMintRoute{}, err
		}
		return BuildDestinationSetup(r)
	}
	panic("unknown golden operation " + operation)
}

func TestKLendGoldenParityWithRustProxy(t *testing.T) {
	raw, err := os.ReadFile(klendGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []klendGoldenCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 30 {
		t.Fatalf("golden record has only %d cases", len(cases))
	}
	for _, c := range cases {
		if c.Operation == "buildCanonicalSubscriptionPolicy" {
			continue // autodeposit owns this builder; see its golden test.
		}
		t.Run(c.Name, func(t *testing.T) {
			route, err := buildKLendGolden(c.Operation, c.Request)
			if c.Error != "" {
				// Idle preconditions are checked before the official builder
				// and may refuse earlier, never later, than Rust.
				if err == nil {
					t.Fatalf("Rust refused (%s); Go built %+v", c.Error, route)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rust built the route; Go refused: %v", err)
			}
			got, err := json.Marshal(klendGoldenRoute(c.Operation, route))
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err = json.Compact(&want, c.Output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("Go wire differs from Rust proxy\n go: %s\nrust: %s", got, want.Bytes())
			}
		})
	}
}
