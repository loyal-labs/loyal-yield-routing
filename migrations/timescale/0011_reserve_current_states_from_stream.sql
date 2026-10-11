-- The observer advances each reserve's newest stored row from its stream too.
ALTER TABLE kamino.reserve_current_states DROP CONSTRAINT IF EXISTS reserve_current_states_state_source_check;
ALTER TABLE kamino.reserve_current_states ADD CONSTRAINT reserve_current_states_state_source_check
    CHECK (state_source IN ('laserstream_grpc', 'http_snapshot', 'http_confirmed_refresh'));
