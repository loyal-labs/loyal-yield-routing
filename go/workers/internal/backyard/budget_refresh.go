package backyard

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
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
func simulateBudgetReserveRefresh(ctx context.Context, c *chain.Client, lane string, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	instructions, err := budgetReserveRefreshInstructions(lane)
	if err != nil {
		return 0, nil, err
	}
	return simulateBudgetRefreshInstructions(ctx, c, instructions, addresses, minimumSlot)
}

// simulateBudgetReserveRefreshOptional mirrors simulateBudgetReserveRefresh for
// batch observers whose pinned address set explicitly allows absent accounts,
// such as an unopened obligation or farm user state. A null capture for such an
// address stays absent — the same zero-value shape confirmedAccounts returns
// for an optional address — instead of failing the whole capture; every other address must
// still resolve, and the simulated snapshot remains unsigned evidence only.
func simulateBudgetReserveRefreshOptional(ctx context.Context, c *chain.Client, lane string, addresses, optional []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	instructions, err := budgetReserveRefreshInstructions(lane)
	if err != nil {
		return 0, nil, err
	}
	allowed := make(map[string]struct{}, len(optional))
	for _, address := range optional {
		allowed[address] = struct{}{}
	}
	return simulateBudgetRefreshInstructionsWithOptional(ctx, c, instructions, addresses, allowed, minimumSlot)
}

func simulateBudgetRefreshInstructions(ctx context.Context, c *chain.Client, instructions []compiledInstruction, addresses []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	return simulateBudgetRefreshInstructionsWithOptional(ctx, c, instructions, addresses, nil, minimumSlot)
}

func simulateBudgetRefreshInstructionsWithOptional(ctx context.Context, c *chain.Client, instructions []compiledInstruction, addresses []string, optional map[string]struct{}, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	for _, instruction := range instructions {
		if instruction.program != publicKey(kamino.ProgramID) || !bytesEqual(instruction.data, kamino.RefreshReserveDiscriminator[:]) || len(instruction.accounts) != 6 {
			return 0, nil, budgetHold("invalid_price_refresh_instruction")
		}
	}
	// The simulating node replaces the zero blockhash with its own recent one,
	// so the message never names a blockhash that node has not seen yet.
	var hash publicKey
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
		tables, lookupErr := observeJupiterLookupTables(ctx, c, []string{routeValuationLookupTable}, minimumSlot)
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
	keys, err := publicKeys(addresses)
	if err != nil {
		return 0, nil, err
	}
	minimum := uint64(minimumSlot)
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimum, ReplaceRecentBlockhash: true, Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: keys}})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		transactionError, _ := json.Marshal(failure.Err)
		return 0, nil, &BudgetHold{Reason: "price_refresh_simulation_failed", Details: map[string]string{"transactionError": string(transactionError)}}
	}
	if err != nil {
		return 0, nil, budgetHold("price_refresh_simulation_unavailable")
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, a := range simulated.Accounts {
		if a == nil {
			// Only an explicitly optional pinned address may stay absent.
			if _, permitted := optional[addresses[i]]; permitted {
				accounts[i] = ConfirmedAccount{Address: addresses[i]}
				continue
			}
			return 0, nil, budgetHold("price_refresh_capture_incomplete")
		}
		accounts[i] = confirmedAccount(addresses[i], a)
	}
	return int64(simulated.Slot), accounts, nil
}
