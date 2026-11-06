package backyard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func (d *Database) reserveWithdrawalCost(ctx context.Context, operationID string, p *withdrawalCostPreparation, message []byte) error {
	if err := p.revalidate(ctx, message); err != nil {
		return err
	}
	return d.reserveWithdrawalCostRecord(ctx, operationID, p)
}

func (d *Database) reserveWithdrawalCostRecord(ctx context.Context, operationID string, p *withdrawalCostPreparation) error {
	if err := p.validateAccounting(); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	digest := sha256Bytes(body)
	var networkValue, networkUnknown any
	if p.NetworkUSDC != nil {
		networkValue = int64(*p.NetworkUSDC)
	} else {
		networkUnknown = p.NetworkUnknownReason
		if networkUnknown == "" {
			networkUnknown = "operator_fee_price_unavailable"
		}
	}
	return d.inTx(ctx, func(tx pgx.Tx) error {
		for _, r := range p.Participants {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.Consent.WithdrawalGeneration.Key(r.Consent.Authority)); err != nil {
				return err
			}
		}
		var prior, priorState string
		err := tx.QueryRow(ctx, `SELECT evidence_digest,state FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, operationID).Scan(&prior, &priorState)
		if err == nil {
			if prior != digest || priorState != "reserved" {
				return staleHold("withdrawal_reservation_conflict")
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM loyal_yield.multiply_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&status); err != nil {
			return err
		}
		if status != string(Decided) {
			return staleHold("withdrawal_operation_changed")
		}
		for _, r := range p.Participants {
			var id string
			if err := tx.QueryRow(ctx, `SELECT consent_id FROM loyal_yield.earn_max_withdrawal_consents WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3 ORDER BY version DESC LIMIT 1`, r.Consent.Authority, r.Consent.RequestSignature, r.Consent.RequestInstruction).Scan(&id); err != nil {
				return err
			}
			if id != r.ID {
				return staleHold("withdrawal_consent_changed")
			}
			var spent, reserved, incremental int64
			var unknown bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN o.state='settled' THEN r.actual_incremental_cost_usdc ELSE 0 END),0)::bigint,COALESCE(sum(CASE WHEN o.state IN ('reserved','unknown') THEN r.incremental_cost_usdc ELSE 0 END),0)::bigint,COALESCE(sum(CASE WHEN o.state IN ('reserved','unknown') THEN r.incremental_cost_usdc WHEN o.state='settled' AND o.finalized_slot>$5 THEN r.actual_incremental_cost_usdc ELSE 0 END),0)::bigint,COALESCE(bool_or(o.state='unknown'),false) FROM loyal_yield.backyard_withdrawal_cost_reservations r JOIN loyal_yield.backyard_withdrawal_cost_operations o USING(operation_id) WHERE r.authority=$1 AND r.request_signature=$2 AND r.request_instruction=$3 AND r.operation_id<>$4`, r.Consent.Authority, r.Consent.RequestSignature, r.Consent.RequestInstruction, operationID, p.Slot).Scan(&spent, &reserved, &incremental, &unknown); err != nil {
				return err
			}
			if unknown {
				return withdrawalCostWait(earnmax.WithdrawalUnknownCost)
			}
			if err := withdrawalCostLimits(r.Consent, earnmax.USDCRaw(spent), earnmax.USDCRaw(reserved), r.IncrementalCost); err != nil {
				return err
			}
			reason, err := r.Consent.Admit(earnmax.WithdrawalAdmission{ProjectedPostFeePayable: &r.Payable, IncrementalReservedCost: earnmax.USDCRaw(incremental), NewIncrementalCost: r.IncrementalCost}, time.Now())
			if err != nil {
				return err
			}
			if reason != "" {
				return withdrawalCostWait(reason)
			}
		}
		if prior != "" {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_withdrawal_cost_operations(operation_id,evidence_digest,message_sha256,effective_lp,max_cost_usdc,other_holders_usdc,network_usdc,network_lamports,evidence,max_incremental_cost_usdc,incremental_other_holders_usdc,network_unknown_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, operationID, digest, p.MessageSHA256, strconv.FormatUint(p.EffectiveLP, 10), int64(p.MaxCost), int64(p.OtherHolders), networkValue, strconv.FormatUint(p.NetworkLamports, 10), body, int64(p.IncrementalCost), int64(p.IncrementalOtherHolders), networkUnknown)
		if err != nil {
			return err
		}
		for _, r := range p.Participants {
			_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.backyard_withdrawal_cost_reservations(operation_id,consent_id,authority,request_signature,request_instruction,escrow_lp,max_cost_usdc,incremental_cost_usdc,projected_payable_usdc) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, operationID, r.ID, r.Consent.Authority, r.Consent.RequestSignature, r.Consent.RequestInstruction, strconv.FormatUint(r.EscrowLP, 10), int64(r.MaxCost), int64(r.IncrementalCost), int64(r.Payable))
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *Database) bindWithdrawalCostWire(ctx context.Context, operationID string, signed SignedTransaction, effects []byte) error {
	if len(signed.signedWire) == 0 || !json.Valid(effects) {
		return fmt.Errorf("signed withdrawal journal evidence unavailable")
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations SET signed_wire_sha256=$2,signed_wire=$4,transaction_signature=$5,recent_blockhash=$6,last_valid_block_height=$7,expected_effects=$8 WHERE operation_id=$1 AND state='reserved' AND message_sha256=$3 AND (signed_wire_sha256 IS NULL OR signed_wire_sha256=$2)`, operationID, signed.signedWireSHA256, signed.messageSHA256, signed.signedWire, signed.transactionSignature, signed.recentBlockhash, signed.lastValidBlockHeight, effects)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return staleHold("withdrawal_signed_wire_journal_conflict")
	}
	return nil
}

// An unsigned failure or finalized absence may release; consent updates,
// cancellation and time alone never prove that a signed wire cannot land.
func (d *Database) releaseWithdrawalCost(ctx context.Context, operationID string) error {
	var available bool
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.backyard_withdrawal_cost_operations') IS NOT NULL`).Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations c SET state='released' FROM loyal_yield.multiply_operations o WHERE c.operation_id=$1 AND o.operation_id=c.operation_id AND c.state='reserved' AND o.status='failed' AND ((o.signed_wire IS NULL AND c.signed_wire_sha256 IS NULL) OR o.recovery_reason='signature_absent_after_blockhash_expiry')`, operationID)
	return err
}

func (d *Database) releaseUnsignedWithdrawalCost(ctx context.Context, operationID string) error {
	_, err := d.pool.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations c SET state='released' FROM loyal_yield.multiply_operations o WHERE c.operation_id=$1 AND o.operation_id=c.operation_id AND c.state='reserved' AND c.signed_wire_sha256 IS NULL AND o.status='decided' AND o.signed_wire IS NULL`, operationID)
	return err
}

func (d *Database) settleWithdrawalCost(ctx context.Context, client *chain.Client, operationID string) error {
	var available bool
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.backyard_withdrawal_cost_operations') IS NOT NULL`).Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	var raw, wire, expected []byte
	var signature, state, wireDigest, route string
	err := d.pool.QueryRow(ctx, `SELECT c.evidence,c.state,o.signed_wire,o.transaction_signature,o.signed_wire_sha256,o.route_key,o.expected_effects FROM loyal_yield.backyard_withdrawal_cost_operations c JOIN loyal_yield.multiply_operations o USING(operation_id) WHERE operation_id=$1`, operationID).Scan(&raw, &state, &wire, &signature, &wireDigest, &route, &expected)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "settled" || state == "released" {
		return nil
	}
	var p withdrawalCostPreparation
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return err
	}
	receipt, err := client.Execution(ctx, sig, rpc.CommitmentFinalized)
	if err != nil {
		return err
	}
	if receipt.Slot == 0 || sha256Bytes(wire) != wireDigest || !bytes.Equal(receipt.Wire, wire) {
		return fmt.Errorf("withdrawal settlement wire mismatch")
	}
	actual, err := withdrawalReceiptCost(p, receipt.Fee, receipt.Err, expected, route, receipt.Receipt)
	actualIncremental := earnmax.USDCRaw(0)
	if err == nil {
		actualIncremental, err = withdrawalActualIncremental(p, actual, receipt.Fee)
	}
	unknown := ""
	if err != nil {
		unknown = err.Error()
	}
	var actualNetwork, actualNetworkUnknown any
	if network, err := withdrawalNetworkCost(p, receipt.Fee); err == nil {
		actualNetwork = int64(network)
	} else {
		actualNetworkUnknown = err.Error()
	}
	return d.inTx(ctx, func(tx pgx.Tx) error {
		for _, r := range p.Participants {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.Consent.WithdrawalGeneration.Key(r.Consent.Authority)); err != nil {
				return err
			}
		}
		var current string
		if err := tx.QueryRow(ctx, `SELECT state FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&current); err != nil {
			return err
		}
		if current == "settled" || current == "released" {
			return nil
		}
		if unknown != "" {
			_, err := tx.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations SET state='unknown',unknown_reason=$2,receipt_signature=$3,receipt_instruction=0,finalized_slot=$4,actual_fee_lamports=$5 WHERE operation_id=$1`, operationID, unknown, signature, receipt.Slot, strconv.FormatUint(receipt.Fee, 10))
			return err
		}
		if err := p.allocateSettlement(actual, actualIncremental); err != nil {
			return err
		}
		for _, r := range p.Participants {
			if _, err := tx.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_reservations SET actual_cost_usdc=$3,actual_incremental_cost_usdc=$4 WHERE operation_id=$1 AND consent_id=$2`, operationID, r.ID, int64(r.MaxCost), int64(r.IncrementalCost)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations SET state='settled',receipt_signature=$2,receipt_instruction=0,finalized_slot=$3,actual_cost_usdc=$4,actual_other_holders_usdc=$5,actual_fee_lamports=$6,actual_incremental_cost_usdc=$7,actual_incremental_other_holders_usdc=$8,actual_network_usdc=$9,actual_network_unknown_reason=$10,unknown_reason=NULL WHERE operation_id=$1`, operationID, signature, receipt.Slot, int64(actual), int64(p.OtherHolders), strconv.FormatUint(receipt.Fee, 10), int64(actualIncremental), int64(p.IncrementalOtherHolders), actualNetwork, actualNetworkUnknown)
		return err
	})
}

func withdrawalNetworkCost(p withdrawalCostPreparation, fee uint64) (earnmax.USDCRaw, error) {
	if p.NetworkLamports == 0 || p.NetworkUSDC == nil || *p.NetworkUSDC < 0 {
		return 0, fmt.Errorf("withdrawal network attribution unavailable")
	}
	n := new(big.Int).Mul(big.NewInt(int64(*p.NetworkUSDC)), new(big.Int).SetUint64(fee))
	n.Add(n, new(big.Int).SetUint64(p.NetworkLamports-1))
	n.Quo(n, new(big.Int).SetUint64(p.NetworkLamports))
	if !n.IsInt64() || n.Sign() < 0 {
		return 0, fmt.Errorf("withdrawal network attribution overflow")
	}
	return earnmax.USDCRaw(n.Int64()), nil
}

func withdrawalActualIncremental(p withdrawalCostPreparation, actual earnmax.USDCRaw, _ uint64) (earnmax.USDCRaw, error) {
	if actual < 0 || p.IncrementalCost < 0 || p.IncrementalCost > p.MaxCost {
		return 0, fmt.Errorf("withdrawal NAV allowance basis unavailable")
	}
	incremental := actual - min(actual, p.MaxCost-p.IncrementalCost)
	if incremental > p.IncrementalCost {
		return 0, fmt.Errorf("finalized incremental cost exceeds reservation")
	}
	return incremental, nil
}

func withdrawalReceiptCost(p withdrawalCostPreparation, _ uint64, failure any, expected []byte, route string, receipt chain.Receipt) (earnmax.USDCRaw, error) {
	cost := earnmax.USDCRaw(0)
	if failure == nil {
		v, err := vaultOfRoute(route)
		if err != nil {
			return 0, err
		}
		effects, err := DecodeExpectedEffects(v, expected)
		if err != nil {
			return 0, err
		}
		if effects.Sale != nil {
			pre, err := transactionTokenBalances(receipt.Pre)
			if err != nil {
				return 0, err
			}
			post, err := transactionTokenBalances(receipt.Post)
			if err != nil {
				return 0, err
			}
			preMap, err := transactionBalancesByAddress(pre)
			if err != nil {
				return 0, err
			}
			postMap, err := transactionBalancesByAddress(post)
			if err != nil {
				return 0, err
			}
			returned, err := effects.Sale.returnedRaw(v, effects, preMap, postMap)
			if err != nil {
				return 0, err
			}
			value, err := effects.Sale.proceedsValue(returned)
			if err != nil {
				return 0, err
			}
			loss := effects.Sale.MarkValueRaw - min(value, effects.Sale.MarkValueRaw)
			if loss > math.MaxInt64-uint64(cost) {
				return 0, fmt.Errorf("withdrawal cost overflow")
			}
			cost += earnmax.USDCRaw(loss)
		}
	}
	if cost > p.MaxCost {
		return 0, fmt.Errorf("finalized withdrawal cost exceeds reservation")
	}
	return cost, nil
}
