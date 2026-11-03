-- The bridge.RouteSet a Privy deposit account was created with. Privy cannot add
-- sources to an existing account, so each address converts only its own set.
-- Every account made before sets existed was created with legacy-13. A
-- relay_signed wallet has no Privy account and bridges whatever Relay offers now.
ALTER TABLE loyal_yield.evm_deposit_routes ADD COLUMN route_set text;
UPDATE loyal_yield.evm_deposit_routes SET route_set = 'legacy-13' WHERE kind = 'privy_autobridge';
ALTER TABLE loyal_yield.evm_deposit_routes ADD CONSTRAINT evm_deposit_routes_route_set
    CHECK ((kind = 'privy_autobridge') = (route_set IS NOT NULL));
