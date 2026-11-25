package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type feeCapitalResolution struct {
	Signature string `json:"signature"`
	Slot      int64  `json:"finalizedSlot"`
	Principal int64  `json:"principalUsdcRaw"`
	Supply    int64  `json:"effectiveLpRaw"`
}

func (r feeCapitalResolution) valid() bool {
	sig, err := solana.SignatureFromBase58(r.Signature)
	return err == nil && !sig.IsZero() && r.Slot > 0 && r.Principal >= 0 && r.Principal <= int64(bridgeMaxNAV) && r.Supply > 0
}

const resolutionTable = "loyal_yield.backyard_looping_fee_capital_resolutions"

func (d *Database) recordFeeResolution(ctx context.Context, r feeCapitalResolution, by, evidence string, apply bool) (string, error) {
	if !r.valid() || strings.TrimSpace(by) == "" || len(by) > 128 || strings.TrimSpace(evidence) == "" || len(evidence) > 1024 {
		return "", fmt.Errorf("invalid Looping capital resolution")
	}
	status := "dry-run"
	err := d.inTx(ctx, func(tx pgx.Tx) error {
		var checkpoint []byte
		if err := tx.QueryRow(ctx, `SELECT state->'performanceFee' FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, looping.RouteKey).Scan(&checkpoint); err != nil {
			return err
		}
		var same, applied bool
		err := tx.QueryRow(ctx, `SELECT (finalized_slot,principal_usdc_raw,effective_lp_raw,approved_by,evidence)=($2,$3,$4,$5,$6),applied_operation_id IS NOT NULL FROM `+resolutionTable+` WHERE signature=$1`, r.Signature, r.Slot, r.Principal, r.Supply, by, evidence).Scan(&same, &applied)
		if err == nil {
			if !same {
				return fmt.Errorf("a different resolution already exists for this receipt")
			}
			status = "already-recorded"
			if applied {
				status = "already-applied"
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var f performanceFeeState
		if json.Unmarshal(checkpoint, &f) != nil || !f.valid() || f.CapitalHold != r.Signature || r.Slot < f.CapitalSlot {
			return fmt.Errorf("the finalized receipt is not the current Looping capital hold")
		}
		if !apply {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO `+resolutionTable+`(signature,finalized_slot,principal_usdc_raw,effective_lp_raw,approved_by,evidence,approved_version) VALUES($1,$2,$3,$4,$5,$6,$7)`, r.Signature, r.Slot, r.Principal, r.Supply, by, evidence, f.Version)
		status = "recorded"
		return err
	})
	return status, err
}

// ResolvePerformanceFeeCapital records an operator's basis for one finalized
// external transaction. The running worker alone can consume it in a net NAV.
func ResolvePerformanceFeeCapital(ctx context.Context, out io.Writer, endpoint, dsn, signature string, principal, supply int64, by, evidence string, apply bool) error {
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil || sig.IsZero() {
		return fmt.Errorf("invalid held transaction signature")
	}
	c, err := chain.New(endpoint, 15*time.Second)
	if err != nil {
		return err
	}
	genesis, err := c.GenesisHash(ctx)
	if err != nil {
		return err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return fmt.Errorf("Looping capital resolution requires mainnet")
	}
	x, err := c.Execution(ctx, sig, rpc.CommitmentFinalized)
	if err != nil {
		return err
	}
	if x.Err != nil || x.Transaction == nil || len(x.Transaction.Signatures) == 0 || x.Transaction.Signatures[0] != sig || neutralFeeCapital(x, feeCapitalAccounts()) {
		return fmt.Errorf("receipt does not prove the held external Looping activity")
	}
	r := feeCapitalResolution{signature, int64(x.Slot), principal, supply}
	d, err := openOperatorDatabase(ctx, dsn)
	if err != nil {
		return err
	}
	defer d.Close()
	status, err := d.recordFeeResolution(ctx, r, by, evidence, apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"status": status, "vault": looping.VoltrVault, "resolution": r})
}
