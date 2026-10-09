-- Policy setup funding reservations were written only by the retired Rust
-- executor (0033). The Go engine never reads or writes either table.
DROP TABLE IF EXISTS loyal_yield.route_policy_setup_funding_reservations;
DROP TABLE IF EXISTS loyal_yield.route_policy_setup_funding_payers;
