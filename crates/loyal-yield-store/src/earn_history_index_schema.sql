SELECT EXISTS (
    SELECT 1
    FROM pg_index AS index
    JOIN pg_class AS relation ON relation.oid = index.indrelid
    JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
    JOIN pg_class AS index_relation ON index_relation.oid = index.indexrelid
    JOIN pg_am AS method ON method.oid = index_relation.relam
    WHERE namespace.nspname = 'loyal_yield'
      AND relation.relname = 'vault_position_snapshots'
      AND index_relation.relname = 'vault_position_snapshots_complete_history_idx'
      AND method.amname = 'btree'
      AND index.indisvalid AND index.indisready
      AND NOT index.indisunique
      AND index.indnkeyatts = 4 AND index.indnatts = 4
      AND index.indexprs IS NULL
      AND index.indoption[0] = 0
      AND index.indoption[1] = 3
      AND index.indoption[2] = 3
      AND index.indoption[3] = 3
      AND pg_get_indexdef(index.indexrelid, 1, false) = 'vault_id'
      AND pg_get_indexdef(index.indexrelid, 2, false) = 'observed_at'
      AND pg_get_indexdef(index.indexrelid, 3, false) = 'observed_slot'
      AND pg_get_indexdef(index.indexrelid, 4, false) = 'id'
      AND regexp_replace(pg_get_expr(index.indpred, index.indrelid), '[[:space:]()]', '', 'g')
          = 'context->>''publication_scope''::text=''complete_product_vault''::text'
) AS valid;
