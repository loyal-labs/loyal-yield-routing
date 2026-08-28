package kamino

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Verification struct {
	Reserve      string    `json:"reserve"`
	AccountHash  string    `json:"account_data_hash"`
	VerifiedSlot int64     `json:"verified_slot"`
	VerifiedAt   time.Time `json:"verified_at"`
	Commitment   string    `json:"commitment"`
	Source       string    `json:"verification_source"`
	StateValid   bool      `json:"state_valid"`
}
type VerificationOutcome struct{ Matched, Deferred map[string]struct{} }

func (s *Store) VerifyStates(ctx context.Context, values []Verification) (VerificationOutcome, error) {
	payload, err := json.Marshal(values)
	if err != nil {
		return VerificationOutcome{}, err
	}
	rows, err := s.pool.Query(ctx, s.verifyStatesSQL(), payload)
	if err != nil {
		return VerificationOutcome{}, fmt.Errorf("verify confirmed Kamino states: %w", err)
	}
	defer rows.Close()
	outcome := VerificationOutcome{Matched: make(map[string]struct{}), Deferred: make(map[string]struct{})}
	for rows.Next() {
		var reserve, classification string
		if err := rows.Scan(&reserve, &classification); err != nil {
			return VerificationOutcome{}, err
		}
		if classification == "matched" {
			outcome.Matched[reserve] = struct{}{}
		} else if classification == "deferred" {
			outcome.Deferred[reserve] = struct{}{}
		}
	}
	return outcome, rows.Err()
}

func (s *Store) verifyStatesSQL() string {
	return fmt.Sprintf(`
WITH input AS (
 SELECT * FROM jsonb_to_recordset($1::jsonb) AS row(reserve text,account_data_hash text,verified_slot bigint,verified_at timestamptz,commitment text,verification_source text,state_valid boolean)
), eligible_input AS MATERIALIZED (
 SELECT * FROM input WHERE commitment='confirmed' AND verification_source IN('http_snapshot','http_confirmed_refresh')
), locked_existing_floors AS MATERIALIZED (
 SELECT existing.reserve,existing.floor_slot,existing.account_data_hash,existing.state_valid,existing.source,existing.source_rank,existing.observed_at
 FROM %[1]s.reserve_confirmed_observation_floors existing JOIN(SELECT DISTINCT reserve FROM eligible_input) requested ON requested.reserve=existing.reserve
 ORDER BY existing.reserve FOR UPDATE OF existing
), input_with_prior_floor AS MATERIALIZED (
 SELECT input.*,prior.floor_slot AS prior_floor_slot,prior.account_data_hash AS prior_floor_account_data_hash,prior.state_valid AS prior_floor_state_valid
 FROM eligible_input input LEFT JOIN locked_existing_floors prior ON prior.reserve=input.reserve
), advanced_floors AS (
 INSERT INTO %[1]s.reserve_confirmed_observation_floors AS current(reserve,floor_slot,account_data_hash,state_valid,source,source_rank,observed_at)
 SELECT reserve,verified_slot,CASE WHEN state_valid THEN account_data_hash ELSE NULL END,state_valid,verification_source,2,verified_at FROM input_with_prior_floor
 ON CONFLICT(reserve) DO UPDATE SET floor_slot=EXCLUDED.floor_slot,account_data_hash=EXCLUDED.account_data_hash,state_valid=EXCLUDED.state_valid,source=EXCLUDED.source,source_rank=EXCLUDED.source_rank,observation_id=EXCLUDED.observation_id,observed_at=GREATEST(current.observed_at,EXCLUDED.observed_at),updated_at=now()
 WHERE EXCLUDED.floor_slot>current.floor_slot OR(EXCLUDED.floor_slot=current.floor_slot AND EXCLUDED.source_rank>=current.source_rank)
 RETURNING reserve,floor_slot,account_data_hash,state_valid,source_rank
), effective_floors AS MATERIALIZED (
 SELECT input.reserve,COALESCE(input.prior_floor_slot,advanced.floor_slot) AS floor_slot,
 CASE WHEN input.prior_floor_slot IS NOT NULL THEN input.prior_floor_account_data_hash ELSE advanced.account_data_hash END AS floor_account_data_hash,
 CASE WHEN input.prior_floor_slot IS NOT NULL THEN input.prior_floor_state_valid ELSE advanced.state_valid END AS floor_state_valid
 FROM input_with_prior_floor input LEFT JOIN advanced_floors advanced ON advanced.reserve=input.reserve
), deferred AS MATERIALIZED (
 SELECT input.reserve FROM input_with_prior_floor input WHERE input.state_valid=true AND input.prior_floor_slot IS NOT NULL AND(
 input.prior_floor_slot-input.verified_slot>%[2]s OR(input.verified_slot<input.prior_floor_slot AND input.prior_floor_state_valid=false) OR
 (input.verified_slot=input.prior_floor_slot AND(input.prior_floor_state_valid=false OR input.prior_floor_account_data_hash IS DISTINCT FROM input.account_data_hash)))
), locked_current AS MATERIALIZED (
 SELECT input.reserve,input.account_data_hash AS confirmed_account_data_hash,input.verified_slot,input.verified_at,input.commitment,input.verification_source,input.state_valid,
 current_state.state_event_id,current_state.account_data_hash AS current_account_data_hash,current_state.state_slot,current_state.state_source,
 COALESCE((current_update.snapshot->>'observation_schema_version')::integer,0) AS current_observation_schema_version,
 floor.floor_slot,floor.floor_account_data_hash,floor.floor_state_valid
 FROM eligible_input input JOIN effective_floors floor ON floor.reserve=input.reserve JOIN %[1]s.reserve_current_states current_state ON current_state.reserve=input.reserve
 JOIN %[1]s.reserve_updates current_update ON current_update.reserve=current_state.reserve AND current_update.event_id=current_state.state_event_id AND current_update.account_data_hash=current_state.account_data_hash AND current_update.slot=current_state.state_slot
 FOR UPDATE OF current_state
), invalidated AS (
 DELETE FROM %[1]s.reserve_confirmed_verifications verification USING locked_current state WHERE verification.reserve=state.reserve AND(
 state.confirmed_account_data_hash<>state.current_account_data_hash OR state.state_valid=false OR state.floor_state_valid=false OR state.floor_account_data_hash IS DISTINCT FROM state.current_account_data_hash)
 AND state.verified_slot>=state.state_slot AND state.verified_slot>=state.floor_slot AND state.verified_slot>=verification.verified_slot RETURNING verification.reserve
), matching AS MATERIALIZED (
 SELECT state.reserve,state.state_event_id,state.confirmed_account_data_hash AS account_data_hash,state.verified_slot,state.verified_at,state.commitment,state.verification_source
 FROM locked_current state WHERE state.state_valid=true AND NOT EXISTS(SELECT 1 FROM deferred WHERE deferred.reserve=state.reserve)
 AND state.confirmed_account_data_hash=state.current_account_data_hash AND state.verified_slot>=state.state_slot
 AND state.state_source IN('http_snapshot','http_confirmed_refresh') AND state.current_observation_schema_version=2 AND(
 state.verified_slot>state.floor_slot OR(state.verified_slot=state.floor_slot AND state.floor_state_valid AND state.floor_account_data_hash=state.current_account_data_hash))
), advanced AS (
 INSERT INTO %[1]s.reserve_confirmed_verifications AS current(reserve,state_event_id,account_data_hash,verified_slot,verified_at,commitment,verification_source)
 SELECT reserve,state_event_id,account_data_hash,verified_slot,verified_at,commitment,verification_source FROM matching
 ON CONFLICT(reserve) DO UPDATE SET state_event_id=EXCLUDED.state_event_id,account_data_hash=EXCLUDED.account_data_hash,verified_slot=EXCLUDED.verified_slot,verified_at=EXCLUDED.verified_at,commitment=EXCLUDED.commitment,verification_source=EXCLUDED.verification_source,updated_at=now()
 WHERE EXCLUDED.verified_slot>current.verified_slot OR(EXCLUDED.verified_slot=current.verified_slot AND EXCLUDED.state_event_id>current.state_event_id) OR(EXCLUDED.verified_slot=current.verified_slot AND EXCLUDED.state_event_id=current.state_event_id AND EXCLUDED.verified_at>current.verified_at)
 RETURNING reserve
)
SELECT reserve,'matched'::text AS classification FROM matching UNION ALL SELECT reserve,'deferred'::text FROM deferred ORDER BY reserve,classification
`, s.schema, s.tolerance())
}
