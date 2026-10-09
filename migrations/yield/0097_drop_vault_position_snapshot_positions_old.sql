-- The pre-September copy of vault_position_snapshot_positions, left behind by
-- a manual table rewrite (no migration created it), and the bookkeeping schema
-- of that copy. Nothing reads either: no view, foreign key or code references
-- them, and the copy's last write was 2026-09-19.
DROP TABLE IF EXISTS loyal_yield.vault_position_snapshot_positions_old;
DROP TABLE IF EXISTS loyal_yield_maint.vpsp_copy_progress;
DROP SCHEMA IF EXISTS loyal_yield_maint;
