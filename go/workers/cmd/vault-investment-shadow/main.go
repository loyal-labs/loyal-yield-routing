// vault-investment-shadow can read and simulate, but has no submit or signing path.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
)

type readOnlyTransport struct{ base http.RoundTripper }

func (t readOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		if r.Body == nil {
			return nil, fmt.Errorf("shadow rejects empty POST")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
		if err != nil {
			return nil, err
		}
		if len(body) > 8<<20 {
			return nil, fmt.Errorf("shadow rejects oversized POST")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &call) != nil {
			return nil, fmt.Errorf("shadow rejects invalid POST")
		}
		allowed := false
		switch call.Method {
		case "getAccountInfo", "getMultipleAccounts", "getProgramAccounts", "getSlot", "getBlockTime", "getLatestBlockhash", "getBlockHeight", "getEpochInfo", "getMinimumBalanceForRentExemption", "getFeeForMessage", "getRecentPrioritizationFees", "simulateTransaction", "simulateBundle", "getVersion", "getRecentPerformanceSamples":
			allowed = true
		case "":
			allowed = r.URL.Host == "api.jup.ag" && r.URL.Path == "/swap/v1/swap-instructions"
		}
		if !allowed {
			return nil, fmt.Errorf("shadow rejects RPC method %q", call.Method)
		}
	} else if r.Method != http.MethodGet {
		return nil, fmt.Errorf("shadow rejects HTTP method")
	}
	return t.base.RoundTrip(r)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	vault := flag.String("vault", "", "required vault name (rwa or looping)")
	lane := flag.String("lane", "", "required catalog lane")
	flag.Parse()
	if *vault == "" || *lane == "" || flag.NArg() != 0 {
		return fmt.Errorf("usage: vault-investment-shadow --vault <vault> --lane <lane>; stdout contains an observation-only snapshot")
	}
	// Only these read credentials are accessed. Never load RuntimeConfig, wallet
	// material or DB credentials, even if they exist in the caller's environment.
	rpc, key := os.Getenv("BACKYARD_SOLANA_RPC_URL"), os.Getenv("JUPITER_API_KEY")
	http.DefaultTransport = readOnlyTransport{base: http.DefaultTransport}
	http.DefaultClient = &http.Client{Transport: http.DefaultTransport, Timeout: 40 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var report bytes.Buffer
	err := backyard.CaptureInvestmentShadow(ctx, &report, backyard.InvestmentShadowConfig{RPCURL: rpc, JupiterKey: key, Vault: *vault, Lane: *lane})
	if err != nil {
		return scrub(err, rpc, key)
	}
	data := report.String()
	for _, secret := range []string{rpc, key} {
		if secret != "" {
			data = strings.ReplaceAll(data, secret, "[redacted]")
		}
	}
	_, err = io.WriteString(os.Stdout, data)
	return err
}
func scrub(err error, secrets ...string) error {
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return fmt.Errorf("%s", message)
}
