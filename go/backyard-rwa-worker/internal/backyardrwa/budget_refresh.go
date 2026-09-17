package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

const routeValuationLookupTable = "HSmmBwB7ZRWEsWf4q47w65hXfmqNrfP67KDtpuVrHK7T"

// budgetReserveRefreshInstructions pins the closed instruction set: the lane's
// two refreshes plus the Prime USDC reference refresh, deduplicated.
func budgetReserveRefreshInstructions(lane string) ([]compiledInstruction, error) {
	if _, err := runtimeRoute(lane); err != nil {
		return nil, err
	}
	seen := map[publicKey]bool{}
	var instructions []compiledInstruction
	for _, id := range []string{lane, RouteID} {
		prefix := kaminoPrimeUSDCRefreshInstructionsForRoute(kaminoLegDeposit, id)
		if len(prefix) != 3 {
			return nil, budgetHold("price_refresh_binding_unavailable")
		}
		for _, instruction := range prefix[:2] {
			reserve := instruction.accounts[0].key
			if !seen[reserve] {
				seen[reserve] = true
				instructions = append(instructions, instruction)
			}
		}
	}
	return instructions, nil
}

// This price-only simulation has no signer or send boundary. Its instruction
// set is closed over the same permissionless reserve refreshes already in the
// production transaction prefix. Account overrides are not used.
func (c *RPCClient) simulateBudgetReserveRefresh(ctx context.Context, lane string, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	instructions, err := budgetReserveRefreshInstructions(lane)
	if err != nil {
		return 0, nil, err
	}
	return c.simulateBudgetRefreshInstructions(ctx, instructions, addresses, minimumSlot)
}

// simulateBudgetReserveRefreshOptional mirrors simulateBudgetReserveRefresh for
// batch observers whose pinned address set explicitly allows absent accounts,
// such as an unopened obligation or farm user state. A null capture for such an
// address stays absent — the same zero-value shape GetMultipleAccountsWithOptional
// returns — instead of failing the whole capture; every other address must
// still resolve, and the simulated snapshot remains unsigned evidence only.
func (c *RPCClient) simulateBudgetReserveRefreshOptional(ctx context.Context, lane string, addresses, optional []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	instructions, err := budgetReserveRefreshInstructions(lane)
	if err != nil {
		return 0, nil, err
	}
	allowed := make(map[string]struct{}, len(optional))
	for _, address := range optional {
		allowed[address] = struct{}{}
	}
	return c.simulateBudgetRefreshInstructionsWithOptional(ctx, instructions, addresses, allowed, minimumSlot)
}

func (c *RPCClient) simulateBudgetRefreshInstructions(ctx context.Context, instructions []compiledInstruction, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	return c.simulateBudgetRefreshInstructionsWithOptional(ctx, instructions, addresses, nil, minimumSlot)
}

func (c *RPCClient) simulateBudgetRefreshInstructionsWithOptional(ctx context.Context, instructions []compiledInstruction, addresses []string, optional map[string]struct{}, minimumSlot int64) (int64, []ConfirmedAccount, error) {
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
	// The RPC only returns accounts named in the message, so every requested
	// capture address rides along as a read-only static key. The fee payer is
	// already a message key; capture adds no signer, no write permission, and
	// no new instruction.
	feePayer := mustKey(bridgeDelegate)
	capture := make([]publicKey, 0, len(addresses))
	seenCapture := map[publicKey]bool{}
	for _, address := range addresses {
		key, err := decodeKey(address)
		if err != nil {
			return 0, nil, err
		}
		if key == feePayer || seenCapture[key] {
			continue
		}
		seenCapture[key] = true
		capture = append(capture, key)
	}
	message, err := encodeLegacyMessage(feePayer, hash, instructions, capture...)
	if err != nil {
		return 0, nil, err
	}
	if _, err = checkedUnsignedMessage(message); err != nil {
		// Reuse the existing partner lookup table only as an address encoding
		// hint. The compiler resolves exact keys from this closed refresh and
		// capture set; table contents cannot introduce an instruction or signer.
		tables, _, lookupErr := observeJupiterLookupTables(ctx, c, []string{routeValuationLookupTable}, minimumSlot)
		if lookupErr != nil {
			return 0, nil, budgetHold("price_refresh_lookup_unavailable")
		}
		message, err = compileV0Message(feePayer, hash, instructions, tables, capture...)
		if err != nil {
			return 0, nil, err
		}
		if _, err = checkedUnsignedMessage(message); err != nil {
			return 0, nil, err
		}
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
		if a == nil {
			// Only an explicitly optional pinned address may stay absent.
			if _, permitted := optional[addresses[i]]; permitted {
				accounts[i] = ConfirmedAccount{Address: addresses[i]}
				continue
			}
			return 0, nil, budgetHold("price_refresh_capture_incomplete")
		}
		if len(a.Data) != 2 || a.Data[1] != "base64" {
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
