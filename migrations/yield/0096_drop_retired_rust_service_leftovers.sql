-- The Go observer serializes health projection with a transaction advisory
-- lock; the Rust projector's lease row (0034) has no reader.
DROP TABLE IF EXISTS loyal_yield.fleet_health_projection_leases;

-- Ad hoc RWA pilot inspection views, created outside the migrations and read
-- by nothing in the repositories.
DROP VIEW IF EXISTS loyal_yield.pilot_operations;
DROP VIEW IF EXISTS loyal_yield.pilot_report_locators;
DROP VIEW IF EXISTS loyal_yield.pilot_route_observation;
