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

// budgetReserveRefreshInstructions pins the closed instruction set: the lane's
// two refreshes plus the USDC reference reserve's, deduplicated.
func budgetReserveRefreshInstructions(v *Vault, lane string) ([]compiledInstruction, error) {
	if _, err := v.route(lane); err != nil {
		return nil, err
	}
	seen := map[publicKey]bool{}
	var instructions []compiledInstruction
	for _, id := range []string{lane, PhaseOneLaneID} {
		prefix := kaminoPrimeUSDCRefreshInstructionsForRoute(v, id)
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

func budgetReserveRefreshInstructionsForLanes(v *Vault, lanes []string) ([]compiledInstruction, error) {
	seen := map[publicKey]bool{}
	var instructions []compiledInstruction
	for _, lane := range lanes {
		laneInstructions, err := budgetReserveRefreshInstructions(v, lane)
		if err != nil {
			return nil, err
		}
		for _, instruction := range laneInstructions {
			reserve := instruction.accounts[0].key
			if !seen[reserve] {
				seen[reserve] = true
				instructions = append(instructions, instruction)
			}
		}
	}
	return instructions, nil
}

func simulateLanesReserveRefreshOptional(ctx context.Context, v *Vault, c *chain.Client, lanes []string, addresses, optional []string, minimumSlot int64) (int64, []ConfirmedAccount, error) {
	instructions, err := budgetReserveRefreshInstructionsForLanes(v, lanes)
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
	message, err := compileV1Message(feePayer, hash, instructions, v1Budget{units: maxComputeUnits}, capture...)
	if err != nil {
		return 0, nil, err
	}
	if _, err = checkedUnsignedMessage(message); err != nil {
		return 0, nil, err
	}
	// All-zero placeholder signature; sigVerify is explicitly false. These
	// bytes cannot be broadcast successfully and are never persisted as signed.
	wire := signedWire(message, make([]byte, 64))
	keys, err := publicKeys(addresses)
	if err != nil {
		return 0, nil, err
	}
	minimum := uint64(minimumSlot)
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimum, ReplaceRecentBlockhash: true, Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: keys}})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		transactionError, _ := json.Marshal(failure.Err)
		return 0, nil, &BudgetHold{Reason: holdStale, Details: map[string]string{"cause": "price_refresh_simulation_failed", "transactionError": string(transactionError)}}
	}
	if err != nil {
		return 0, nil, budgetHoldFrom(err, "price_refresh_simulation_unavailable")
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, a := range simulated.Accounts {
		if a == nil {
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
