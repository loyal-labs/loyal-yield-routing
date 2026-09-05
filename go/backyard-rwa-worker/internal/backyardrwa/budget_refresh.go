package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

// This price-only simulation has no signer or send boundary. Its instruction
// set is closed over the same permissionless reserve refreshes already in the
// production transaction prefix. Account overrides are not used.
func (c *RPCClient) simulateBudgetReserveRefresh(ctx context.Context, lane string, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	if _, err := runtimeRoute(lane); err != nil {
		return 0, nil, err
	}
	seen := map[publicKey]bool{}
	var instructions []compiledInstruction
	for _, id := range []string{lane, RouteID} {
		prefix := kaminoPrimeUSDCRefreshInstructionsForRoute(kaminoLegDeposit, id)
		if len(prefix) != 3 {
			return 0, nil, budgetHold("price_refresh_binding_unavailable")
		}
		for _, instruction := range prefix[:2] {
			reserve := instruction.accounts[0].key
			if !seen[reserve] {
				seen[reserve] = true
				instructions = append(instructions, instruction)
			}
		}
	}
	return c.simulateBudgetRefreshInstructions(ctx, instructions, addresses, minimumSlot)
}

func (c *RPCClient) simulateBudgetRefreshInstructions(ctx context.Context, instructions []compiledInstruction, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	for _, instruction := range instructions {
		if instruction.program != mustKey(kaminoProgram) || !bytesEqual(instruction.data, kaminoRefreshReserve) || len(instruction.accounts) != 6 {
			return 0, nil, budgetHold("invalid_price_refresh_instruction")
		}
	}
	blockhash, err := c.LatestBlockhash(ctx)
	if err != nil {
		return 0, nil, budgetHold("price_refresh_blockhash_unavailable")
	}
	hash, err := decodeKey(blockhash.Blockhash)
	if err != nil {
		return 0, nil, err
	}
	message, err := encodeLegacyMessage(mustKey(bridgeDelegate), hash, instructions)
	if err != nil {
		return 0, nil, err
	}
	if _, err = checkedUnsignedMessage(message); err != nil {
		return 0, nil, err
	}
	// All-zero placeholder signature; sigVerify is explicitly false. These
	// bytes cannot be broadcast successfully and are never persisted as signed.
	wire := append([]byte{1}, make([]byte, 64)...)
	wire = append(wire, message...)
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Err      json.RawMessage `json:"err"`
			Accounts []*struct {
				Owner      string   `json:"owner"`
				Lamports   uint64   `json:"lamports"`
				Executable bool     `json:"executable"`
				Data       []string `json:"data"`
			} `json:"accounts"`
		} `json:"value"`
	}
	if err = c.call(ctx, "simulateTransaction", []any{base64.StdEncoding.EncodeToString(wire), map[string]any{"encoding": "base64", "commitment": "confirmed", "sigVerify": false, "minContextSlot": minimumSlot, "accounts": map[string]any{"encoding": "base64", "addresses": addresses}}}, &result); err != nil {
		return 0, nil, budgetHold("price_refresh_simulation_unavailable")
	}
	if len(result.Value.Err) > 0 && string(result.Value.Err) != "null" {
		return 0, nil, &BudgetHold{Reason: "price_refresh_simulation_failed", Details: map[string]string{"transactionError": string(result.Value.Err)}}
	}
	if result.Context.Slot < minimumSlot || len(result.Value.Accounts) != len(addresses) {
		return 0, nil, budgetHold("price_refresh_simulation_failed")
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, a := range result.Value.Accounts {
		if a == nil || len(a.Data) != 2 || a.Data[1] != "base64" {
			return 0, nil, budgetHold("price_refresh_capture_incomplete")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(a.Data[0])
		if err != nil {
			return 0, nil, budgetHold("price_refresh_capture_invalid")
		}
		accounts[i] = ConfirmedAccount{Address: addresses[i], Owner: a.Owner, Lamports: a.Lamports, Executable: a.Executable, Data: data}
	}
	return result.Context.Slot, accounts, nil
}
