CREATE INDEX CONCURRENTLY IF NOT EXISTS multiply_operations_route_created_idx
    ON loyal_yield.multiply_operations (route_key, created_at, operation_id);
