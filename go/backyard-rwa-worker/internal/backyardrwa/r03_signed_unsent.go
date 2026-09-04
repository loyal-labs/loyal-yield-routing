package backyardrwa

// This file is deliberately a lower-level evidence boundary.  It accepts only
// wires produced by the canonical Go builders; it never builds a caller-
// supplied instruction and it has no send/broadcast path.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

const r03EvidenceSchema = "loyal-backyard-rwa-phase2-runtime-signed-unsent/v1"

type R03LifecycleTransaction struct {
	Role              string `json:"role"`
	Phase             string `json:"phase"`
	Signature         string `json:"signature"`
	PacketBytes       int    `json:"packetBytes"`
	TransactionBase64 string `json:"transactionBase64"`
	TransactionSHA256 string `json:"transactionSha256"`
	SimulationErr     any    `json:"simulationErr"`
	SimulationPassed  bool   `json:"simulationPassed"`
}

type R03HoldRecord struct {
	Action        string  `json:"action"`
	Reason        string  `json:"reason"`
	ObservationID string  `json:"observationId"`
	Slot          int64   `json:"slot"`
	Broadcast     bool    `json:"broadcast"`
	Signature     *string `json:"signature"`
}

// R03LifecyclePlan is the JSON handoff from the canonical Go lifecycle
// builders to the simulation command.  The handoff is intentionally explicit:
// an absent obligation requires the first wire to be the setup-authority init
// prelude, and the HOLD record is not disguised as a transaction.
type R03LifecyclePlan struct {
	Schema             string                    `json:"schema"`
	Lane               string                    `json:"lane"`
	ProtectedAddresses []string                  `json:"protectedAddresses"`
	ObligationAddress  string                    `json:"obligationAddress"`
	ObligationAbsent   bool                      `json:"obligationAbsent"`
	Transactions       []R03LifecycleTransaction `json:"transactions"`
	Hold               R03HoldRecord             `json:"hold"`
}

type R03BundleResult struct {
	Provider               any    `json:"provider"`
	ContextSlot            int64  `json:"contextSlot"`
	SignatureAbsentOnChain bool   `json:"signatureAbsentOnChain"`
	ChainPreStateSHA256    string `json:"chainPreStateSha256"`
	ChainPostStateSHA256   string `json:"chainPostStateSha256"`
}

type r03HeliusResult struct {
	Kind               string `json:"kind"`
	Summary            string `json:"summary"`
	ContextSlot        int64  `json:"contextSlot"`
	TransactionResults []struct {
		Err                         any    `json:"err"`
		CapturedPre                 bool   `json:"capturedPre"`
		CapturedPost                bool   `json:"capturedPost"`
		PreExecutionAccountsSha256  string `json:"preExecutionAccountsSha256"`
		PostExecutionAccountsSha256 string `json:"postExecutionAccountsSha256"`
	} `json:"transactionResults"`
}

func (p R03LifecyclePlan) validate() error {
	if p.Schema != "" && p.Schema != r03EvidenceSchema {
		return fmt.Errorf("R03 plan schema is not %s", r03EvidenceSchema)
	}
	if p.Lane != SelectedRouteID {
		return fmt.Errorf("R03 plan lane is not the installed Maple lane")
	}
	if len(p.ProtectedAddresses) == 0 || len(p.Transactions) == 0 {
		return fmt.Errorf("R03 plan requires protected accounts and signed transactions")
	}
	if p.Hold.Action != "HOLD" || p.Hold.Reason == "" || p.Hold.ObservationID == "" || p.Hold.Slot <= 0 || p.Hold.Broadcast || p.Hold.Signature != nil {
		return fmt.Errorf("R03 plan HOLD record is not explicit and unsigned")
	}
	seen := make(map[string]bool, len(p.Transactions))
	phases := make(map[string]bool, len(p.Transactions))
	for index := range p.Transactions {
		row := &p.Transactions[index]
		if row.Role == "" || row.Phase == "" || seen[row.Role] {
			return fmt.Errorf("R03 plan transaction roles must be non-empty and unique")
		}
		seen[row.Role] = true
		phases[row.Phase] = true
		if row.PacketBytes <= 0 || row.PacketBytes > solanaPacketBytes || row.TransactionBase64 == "" {
			return fmt.Errorf("R03 plan transaction %s is not a bounded signed wire", row.Role)
		}
		if _, err := base64.StdEncoding.Strict().DecodeString(row.TransactionBase64); err != nil {
			return fmt.Errorf("R03 plan transaction %s has invalid base64", row.Role)
		}
	}
	for _, phase := range []string{"entry", "unwind", "return", "nav"} {
		if !phases[phase] {
			return fmt.Errorf("R03 plan is missing the %s lifecycle phase", phase)
		}
	}
	if p.ObligationAbsent {
		if p.ObligationAddress == "" || p.Transactions[0].Phase != "setup-authority" || p.Transactions[0].Role != "init-obligation" {
			return fmt.Errorf("absent obligation requires the first setup-authority init-obligation prelude")
		}
	}
	return nil
}

// SimulateR03Lifecycle performs fresh confirmed reads, signature-absence
// checks, and one stateful Helius simulateBundle call.  `skipSigVerify:false`
// is intentional: Helius names the signature-verification switch negatively.
// This function never calls sendTransaction.
func SimulateR03Lifecycle(ctx context.Context, rpcURL string, plan R03LifecyclePlan) (R03BundleResult, error) {
	if err := plan.validate(); err != nil {
		return R03BundleResult{}, err
	}
	rpc, err := NewRPCClient(rpcURL)
	if err != nil {
		return R03BundleResult{}, err
	}
	decoded := make([][]byte, len(plan.Transactions))
	signatures := make([]string, len(plan.Transactions))
	seenSignatures := make(map[string]bool, len(plan.Transactions))
	for index := range plan.Transactions {
		row := &plan.Transactions[index]
		wire, err := base64.StdEncoding.Strict().DecodeString(row.TransactionBase64)
		if err != nil || len(wire) < 1+ed25519SignatureBytes {
			return R03BundleResult{}, fmt.Errorf("R03 transaction %s is not a signed Solana wire", row.Role)
		}
		if wire[0] != 1 {
			return R03BundleResult{}, fmt.Errorf("R03 transaction %s must have exactly one signature", row.Role)
		}
		decoded[index] = wire
		signatures[index] = encodeBase58(wire[1 : 1+ed25519SignatureBytes])
		if seenSignatures[signatures[index]] {
			return R03BundleResult{}, fmt.Errorf("R03 bundle reuses a transaction signature")
		}
		seenSignatures[signatures[index]] = true
		row.Signature = signatures[index]
		row.PacketBytes = len(wire)
		digest := sha256.Sum256(wire)
		row.TransactionSHA256 = hex.EncodeToString(digest[:])
	}

	minimumSlot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return R03BundleResult{}, err
	}
	preSlot, preAccounts, err := rpc.readR03Accounts(ctx, plan.ProtectedAddresses, minimumSlot, plan.ObligationAddress, plan.ObligationAbsent)
	if err != nil {
		return R03BundleResult{}, err
	}
	if plan.ObligationAbsent && accountAt(preAccounts, plan.ObligationAddress).Lamports != 0 {
		return R03BundleResult{}, fmt.Errorf("obligation %s is present; refusing an init prelude", plan.ObligationAddress)
	}
	before, err := rpc.SignatureStatuses(ctx, signatures)
	if err != nil {
		return R03BundleResult{}, err
	}
	if !allSignaturesAbsent(before) {
		return R03BundleResult{}, fmt.Errorf("R03 signed-unsent wire already has an on-chain signature")
	}
	provider, err := rpc.simulateR03Bundle(ctx, decoded, plan.ProtectedAddresses)
	if err != nil {
		return R03BundleResult{}, err
	}
	for index := range plan.Transactions {
		plan.Transactions[index].SimulationPassed = true
	}
	afterStatuses, err := rpc.SignatureStatuses(ctx, signatures)
	if err != nil {
		return R03BundleResult{}, err
	}
	if !allSignaturesAbsent(afterStatuses) {
		return R03BundleResult{}, fmt.Errorf("Helius simulation landed an R03 signed wire")
	}
	postSlot, postAccounts, err := rpc.readR03Accounts(ctx, plan.ProtectedAddresses, preSlot, plan.ObligationAddress, plan.ObligationAbsent)
	if err != nil {
		return R03BundleResult{}, err
	}
	preHash := hashConfirmedAccounts(preAccounts)
	postHash := hashConfirmedAccounts(postAccounts)
	if preHash != postHash {
		return R03BundleResult{}, fmt.Errorf("confirmed protected state changed across signed-unsent simulation")
	}
	if provider.ContextSlot > postSlot {
		postSlot = provider.ContextSlot
	}
	return R03BundleResult{Provider: provider, ContextSlot: postSlot, SignatureAbsentOnChain: true, ChainPreStateSHA256: preHash, ChainPostStateSHA256: postHash}, nil
}

const ed25519SignatureBytes = 64

func allSignaturesAbsent(statuses []SignatureObservation) bool {
	for _, status := range statuses {
		if status.Found {
			return false
		}
	}
	return true
}

func (c *RPCClient) SignatureStatuses(ctx context.Context, signatures []string) ([]SignatureObservation, error) {
	if c == nil || len(signatures) == 0 {
		return nil, fmt.Errorf("signatures are required")
	}
	var result struct {
		Value []*struct {
			Slot               int64           `json:"slot"`
			Err                json.RawMessage `json:"err"`
			ConfirmationStatus string          `json:"confirmationStatus"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getSignatureStatuses", []any{signatures, map[string]bool{"searchTransactionHistory": true}}, &result); err != nil {
		return nil, err
	}
	if len(result.Value) != len(signatures) {
		return nil, fmt.Errorf("signature status count does not match R03 bundle")
	}
	out := make([]SignatureObservation, len(signatures))
	for index, row := range result.Value {
		if row == nil {
			continue
		}
		failed := len(row.Err) > 0 && string(row.Err) != "null"
		out[index] = SignatureObservation{Found: true, Confirmed: !failed && row.Slot > 0 && (row.ConfirmationStatus == "confirmed" || row.ConfirmationStatus == "finalized"), ConfirmationSlot: row.Slot, Failed: failed}
	}
	return out, nil
}

func (c *RPCClient) readR03Accounts(ctx context.Context, addresses []string, minSlot int64, obligation string, absent bool) (int64, []ConfirmedAccount, error) {
	if absent && obligation != "" {
		return c.GetMultipleAccountsWithOptional(ctx, addresses, minSlot, obligation)
	}
	return c.GetMultipleAccounts(ctx, addresses, minSlot)
}

func hashConfirmedAccounts(accounts []ConfirmedAccount) string {
	hash := sha256.New()
	for _, account := range accounts {
		hash.Write([]byte(account.Address))
		hash.Write([]byte{0})
		hash.Write([]byte(account.Owner))
		hash.Write([]byte{0})
		var lamports [8]byte
		for index := range lamports {
			lamports[index] = byte(account.Lamports >> (8 * index))
		}
		hash.Write(lamports[:])
		hash.Write([]byte{0})
		hash.Write(account.Data)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *RPCClient) simulateR03Bundle(ctx context.Context, wires [][]byte, addresses []string) (r03HeliusResult, error) {
	encoded := make([]string, len(wires))
	for index, wire := range wires {
		encoded[index] = base64.StdEncoding.EncodeToString(wire)
	}
	configs := make([]map[string]any, len(wires))
	for index := range configs {
		configs[index] = map[string]any{"addresses": addresses, "encoding": "base64"}
	}
	params := []any{map[string]any{"encodedTransactions": encoded}, map[string]any{
		"preExecutionAccountsConfigs": configs, "postExecutionAccountsConfigs": configs,
		"skipSigVerify": false, "simulationBank": map[string]any{"commitment": map[string]string{"commitment": "confirmed"}},
		"transactionEncoding": "base64", "replaceRecentBlockhash": false,
	}}
	var provider r03HeliusResult
	if err := c.call(ctx, "simulateBundle", params, &provider); err != nil {
		return r03HeliusResult{}, err
	}
	if provider.Kind != "result" || provider.Summary != "succeeded" || len(provider.TransactionResults) != len(wires) {
		return provider, fmt.Errorf("Helius rejected the exact R03 signed-unsent bundle: summary=%q transactions=%d", provider.Summary, len(provider.TransactionResults))
	}
	for index, row := range provider.TransactionResults {
		if row.Err != nil && fmt.Sprint(row.Err) != "<nil>" && fmt.Sprint(row.Err) != "null" || !row.CapturedPre || !row.CapturedPost {
			return provider, fmt.Errorf("Helius R03 transaction %d failed or lacked account captures", index)
		}
		if index > 0 && provider.TransactionResults[index-1].PostExecutionAccountsSha256 != row.PreExecutionAccountsSha256 {
			return provider, fmt.Errorf("Helius R03 bundle is not stateful at transaction %d", index)
		}
	}
	return provider, nil
}

func WriteR03Evidence(path string, plan R03LifecyclePlan, result R03BundleResult) error {
	if path == "" {
		return fmt.Errorf("R03 evidence path is required")
	}
	if err := plan.validate(); err != nil {
		return err
	}
	payload := struct {
		Schema                 string                    `json:"schema"`
		Verdict                string                    `json:"verdict"`
		Broadcast              bool                      `json:"broadcast"`
		SignedUnsent           bool                      `json:"signedUnsent"`
		Cluster                string                    `json:"cluster"`
		Commitment             string                    `json:"commitment"`
		SelectedLane           string                    `json:"selectedLane"`
		Transactions           []R03LifecycleTransaction `json:"transactions"`
		Hold                   R03HoldRecord             `json:"hold"`
		SignatureAbsentOnChain bool                      `json:"signatureAbsentOnChain"`
		ChainPreStateSHA256    string                    `json:"chainPreStateSha256"`
		ChainPostStateSHA256   string                    `json:"chainPostStateSha256"`
		ConfirmedReadbackSlot  int64                     `json:"confirmedReadbackSlot"`
		Simulation             R03BundleResult           `json:"simulation"`
	}{r03EvidenceSchema, "PASS", false, true, "mainnet-beta", "confirmed", plan.Lane, plan.Transactions, plan.Hold,
		result.SignatureAbsentOnChain, result.ChainPreStateSHA256, result.ChainPostStateSHA256, result.ContextSlot, result}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Chmod(0o600)
}
