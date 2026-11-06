// Package releasecontrol owns the durable per-vault admission switch for the
// four new money paths. A vault/action without a row is held.
package releasecontrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
)

type Action string

const (
	OptionalAddition Action = "optional_addition"
	OrdinaryFunding  Action = "ordinary_funding"
	OfficialDeposit  Action = "official_deposit"
	ClaimAttempt     Action = "claim_attempt"
)

// Actions maps each action to its one admitting call site and the canary
// limits that site can evaluate. Status reads, consent inspection and cancel,
// exact-wire recovery, settlement, the manual-recovery latch and emergency
// reductions (hard LTV, term, operator exit) are never gated.
var Actions = []struct {
	Action        Action
	Site          string
	Owners, Limit bool
}{
	{OptionalAddition, "backyard.Database.RecordDecisionOnManifest (requiresNAVForAdd)", false, false},
	{OrdinaryFunding, "backyard.Database.RecordDecisionOnManifest (cost-bearing ordinary withdrawal funding)", true, true},
	{OfficialDeposit, "webapp POST /api/earn-max/deposit-quote", true, true},
	{ClaimAttempt, "earnmax.WithdrawalAttemptStore.reserveValidated", true, true},
}

func ParseAction(raw string) (Action, error) {
	for _, a := range Actions {
		if string(a.Action) == raw {
			return a.Action, nil
		}
	}
	return "", fmt.Errorf("unknown release action %q", raw)
}

func limits(action Action) (owners, limit bool) {
	for _, a := range Actions {
		if a.Action == action {
			return a.Owners, a.Limit
		}
	}
	return false, false
}

type State string

const (
	Open State = "open"
	Held State = "held"
)

// Refusal reasons are API contract names.
const (
	RefusedHeld  = "release_control_held"
	RefusedOwner = "owner_not_admitted"
	RefusedLimit = "amount_over_canary_limit"
)

type Control struct {
	Vault         string    `json:"vault"`
	Action        Action    `json:"action"`
	State         State     `json:"state"`
	Version       int64     `json:"version"`
	Reason        string    `json:"reason"`
	Operator      string    `json:"operator"`
	AllowedOwners []string  `json:"allowedOwners"`
	MaxAmountRaw  *uint64   `json:"maxAmountRaw"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Admission is the control version a decision was admitted under.
type Admission struct {
	Vault   string
	Action  Action
	Version int64
}

type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Check reads the control FOR SHARE inside the admitting transaction, so a
// concurrent Set waits for this admission to commit or roll back.
func Check(ctx context.Context, tx pgx.Tx, vault string, action Action, owners []string, amount *uint64) (Admission, error) {
	var state State
	var version int64
	var allowed []string
	var max *int64
	err := tx.QueryRow(ctx, `SELECT state,version,allowed_owners,max_amount_raw FROM loyal_yield.vault_release_controls WHERE vault=$1 AND action=$2 FOR SHARE`, vault, string(action)).Scan(&state, &version, &allowed, &max)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, &Refusal{RefusedHeld}
	}
	if err != nil {
		return Admission{}, err
	}
	if reason := refusal(Control{State: state, AllowedOwners: allowed, MaxAmountRaw: uint64Ptr(max)}, owners, amount); reason != "" {
		return Admission{}, &Refusal{reason}
	}
	return Admission{Vault: vault, Action: action, Version: version}, nil
}

func refusal(c Control, owners []string, amount *uint64) string {
	if c.State != Open {
		return RefusedHeld
	}
	if c.AllowedOwners != nil {
		if len(owners) == 0 {
			return RefusedOwner
		}
		for _, o := range owners {
			if !slices.Contains(c.AllowedOwners, o) {
				return RefusedOwner
			}
		}
	}
	if c.MaxAmountRaw != nil && (amount == nil || *amount > *c.MaxAmountRaw) {
		return RefusedLimit
	}
	return ""
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Capability is what the owner would be told now; it admits nothing.
func Capability(ctx context.Context, db Querier, vault string, action Action, owner string, amount *uint64) (Control, string, error) {
	c, err := read(ctx, db, vault, action)
	if errors.Is(err, pgx.ErrNoRows) {
		return Control{Vault: vault, Action: action, State: Held}, RefusedHeld, nil
	}
	if err != nil {
		return Control{}, "", err
	}
	return c, refusal(c, []string{owner}, amount), nil
}

func read(ctx context.Context, db Querier, vault string, action Action) (Control, error) {
	c := Control{Vault: vault, Action: action}
	var max *int64
	err := db.QueryRow(ctx, `SELECT state,version,reason,operator,allowed_owners,max_amount_raw,updated_at FROM loyal_yield.vault_release_controls WHERE vault=$1 AND action=$2`, vault, string(action)).Scan(&c.State, &c.Version, &c.Reason, &c.Operator, &c.AllowedOwners, &max, &c.UpdatedAt)
	c.MaxAmountRaw = uint64Ptr(max)
	return c, err
}

func uint64Ptr(v *int64) *uint64 {
	if v == nil {
		return nil
	}
	u := uint64(*v)
	return &u
}

type Change struct {
	Vault         string
	Action        Action
	State         State
	ExpectVersion int64
	Reason        string
	Operator      string
	AllowedOwners []string
	MaxAmountRaw  *uint64
}

var ErrVersionConflict = errors.New("release control version changed; re-read with show")

type Beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// Set moves a control by compare-and-swap on its version and appends the
// new row to history in the same transaction. ExpectVersion 0 creates it.
func Set(ctx context.Context, db Beginner, c Change) (Control, error) {
	if err := c.validate(); err != nil {
		return Control{}, err
	}
	var max any
	if c.MaxAmountRaw != nil {
		max = int64(*c.MaxAmountRaw)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return Control{}, err
	}
	defer tx.Rollback(ctx)
	var tag string
	if c.ExpectVersion == 0 {
		tag = `INSERT INTO loyal_yield.vault_release_controls(vault,action,state,version,reason,operator,allowed_owners,max_amount_raw) VALUES($1,$2,$3,1,$4,$5,$6,$7) ON CONFLICT DO NOTHING`
	} else {
		tag = `UPDATE loyal_yield.vault_release_controls SET state=$3,version=version+1,reason=$4,operator=$5,allowed_owners=$6,max_amount_raw=$7,updated_at=now() WHERE vault=$1 AND action=$2 AND version=$8`
	}
	args := []any{c.Vault, string(c.Action), string(c.State), c.Reason, c.Operator, c.AllowedOwners, max}
	if c.ExpectVersion != 0 {
		args = append(args, c.ExpectVersion)
	}
	result, err := tx.Exec(ctx, tag, args...)
	if err != nil {
		return Control{}, err
	}
	if result.RowsAffected() != 1 {
		return Control{}, ErrVersionConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_release_control_history(vault,action,state,version,reason,operator,allowed_owners,max_amount_raw,updated_at) SELECT vault,action,state,version,reason,operator,allowed_owners,max_amount_raw,updated_at FROM loyal_yield.vault_release_controls WHERE vault=$1 AND action=$2`, c.Vault, string(c.Action)); err != nil {
		return Control{}, err
	}
	out, err := read(ctx, tx, c.Vault, c.Action)
	if err != nil {
		return Control{}, err
	}
	return out, tx.Commit(ctx)
}

func (c Change) validate() error {
	if _, err := ParseAction(string(c.Action)); err != nil {
		return err
	}
	owners, limit := limits(c.Action)
	switch {
	case c.State != Open && c.State != Held:
		return fmt.Errorf("state must be open or held")
	case c.ExpectVersion < 0:
		return fmt.Errorf("expected version must not be negative")
	case c.Reason == "" || c.Operator == "":
		return fmt.Errorf("reason and operator are required")
	case c.AllowedOwners != nil && !owners:
		return fmt.Errorf("%s has no owner to compare against an allowlist", c.Action)
	case c.MaxAmountRaw != nil && !limit:
		return fmt.Errorf("%s has no amount to compare against a cap", c.Action)
	case c.MaxAmountRaw != nil && *c.MaxAmountRaw > 1<<63-1:
		return fmt.Errorf("max amount exceeds bigint")
	}
	if _, err := solana.PublicKeyFromBase58(c.Vault); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	for _, o := range c.AllowedOwners {
		if _, err := solana.PublicKeyFromBase58(o); err != nil {
			return fmt.Errorf("allowed owner %q: %w", o, err)
		}
	}
	return nil
}

// InFlight is work admitted under an older version that a hold does not
// revoke: exact signed wires and issued quotes run to their own end.
type InFlight struct {
	Action  Action `json:"action"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Status  string `json:"status"`
}

type Report struct {
	Controls []Control  `json:"controls"`
	InFlight []InFlight `json:"inFlight"`
}

func Show(ctx context.Context, db Querier, vault string) (Report, error) {
	var r Report
	for _, a := range Actions {
		c, err := read(ctx, db, vault, a.Action)
		if errors.Is(err, pgx.ErrNoRows) {
			c = Control{Vault: vault, Action: a.Action, State: Held, Reason: "no control row"}
		} else if err != nil {
			return Report{}, err
		}
		r.Controls = append(r.Controls, c)
	}
	rows, err := db.Query(ctx, `
SELECT o.release_control_action,'operation',o.operation_id,o.release_control_version,o.status
  FROM loyal_yield.multiply_operations o JOIN loyal_yield.vault_release_controls c ON c.vault=o.release_control_vault AND c.action=o.release_control_action
 WHERE o.release_control_vault=$1 AND o.release_control_version<c.version AND o.status IN ('decided','built','simulated','signed','broadcast_intent','confirmed','reconciling')
UNION ALL
SELECT 'claim_attempt','claim_attempt',a.attempt_id,a.release_control_version,a.state
  FROM loyal_yield.earn_max_withdrawal_claim_attempts a JOIN loyal_yield.vault_release_controls c ON c.vault=a.release_control_vault AND c.action='claim_attempt'
 WHERE a.release_control_vault=$1 AND a.release_control_version<c.version AND a.state='reserved'
UNION ALL
SELECT 'official_deposit','deposit_quote',q.certificate_id,q.release_control_version,'issued'
  FROM loyal_yield.earn_max_deposit_quotes q JOIN loyal_yield.vault_release_controls c ON c.vault=q.vault AND c.action='official_deposit'
 WHERE q.vault=$1 AND q.release_control_version<c.version AND q.expires_at>now()
 ORDER BY 1,3`, vault)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var f InFlight
		if err := rows.Scan(&f.Action, &f.Kind, &f.ID, &f.Version, &f.Status); err != nil {
			return Report{}, err
		}
		r.InFlight = append(r.InFlight, f)
	}
	return r, rows.Err()
}
