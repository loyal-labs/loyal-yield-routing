package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/loyal-labs/loyal-yield-routing/go/kamino-fleet-planner/internal/fleet"
)

type outputInstruction struct {
	Step     string                     `json:"step"`
	Program  string                     `json:"program"`
	Accounts []fleet.InstructionAccount `json:"accounts"`
	DataHex  string                     `json:"dataHex"`
}
type output struct {
	Public    []outputInstruction `json:"public"`
	Protected []outputInstruction `json:"protected"`
}

func main() {
	var request fleet.KaminoSameMintRouteRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fatal(err)
	}
	proxy, err := fleet.NewKLendProxy(os.Getenv("KLEND_PROXY_PATH"), os.Getenv("KLEND_PROXY_SHA256"))
	if err != nil {
		fatal(err)
	}
	route, err := proxy.Build(context.Background(), request)
	if err != nil {
		fatal(err)
	}
	convert := func(values []fleet.RouteInstruction) []outputInstruction {
		result := make([]outputInstruction, 0, len(values))
		for _, v := range values {
			result = append(result, outputInstruction{v.Step, v.Program, v.Accounts, hex.EncodeToString(v.Data)})
		}
		return result
	}
	if err := json.NewEncoder(os.Stdout).Encode(output{convert(route.Public), convert(route.Protected)}); err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
