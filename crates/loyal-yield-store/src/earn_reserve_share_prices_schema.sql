-- Read-only contract used by both migration entrypoints and release verification.
WITH expected_columns(name, data_type) AS (
    VALUES ('id', 'bigint'), ('cluster', 'text'), ('reserve', 'text'),
           ('market', 'text'), ('liquidity_mint', 'text'),
           ('observed_hour', 'timestamp with time zone'),
           ('observed_at', 'timestamp with time zone'),
           ('slot', 'bigint'), ('share_price', 'double precision')
), columns_valid AS (
    SELECT count(*) = 9 AS valid
    FROM expected_columns expected
    JOIN pg_attribute attribute
      ON attribute.attrelid = to_regclass('loyal_yield.earn_reserve_share_prices')
     AND attribute.attname = expected.name
     AND NOT attribute.attisdropped AND attribute.attnotnull
     AND format_type(attribute.atttypid, attribute.atttypmod) = expected.data_type
), indexes_valid AS (
    SELECT count(*) = 3 AS valid
    FROM pg_index index
    JOIN pg_class relation ON relation.oid = index.indexrelid
    JOIN pg_am method ON method.oid = relation.relam
    WHERE index.indrelid = to_regclass('loyal_yield.earn_reserve_share_prices')
      AND index.indisready AND index.indisvalid AND method.amname = 'btree'
      AND index.indpred IS NULL AND index.indexprs IS NULL
      AND index.indnatts = index.indnkeyatts
      AND (
        (index.indisprimary AND index.indisunique AND index.indnkeyatts = 1
          AND pg_get_indexdef(index.indexrelid, 1, true) = 'id')
        OR (relation.relname = 'earn_reserve_share_prices_hour_uidx'
          AND index.indisunique AND index.indnkeyatts = 3
          AND pg_get_indexdef(index.indexrelid, 1, true) = 'cluster'
          AND pg_get_indexdef(index.indexrelid, 2, true) = 'reserve'
          AND pg_get_indexdef(index.indexrelid, 3, true) = 'observed_hour')
        OR (relation.relname = 'earn_reserve_share_prices_observed_idx'
          AND index.indnkeyatts = 2
          AND pg_get_indexdef(index.indexrelid, 1, true) = 'cluster'
          AND pg_get_indexdef(index.indexrelid, 2, true) = 'observed_at')
      )
)
SELECT columns_valid.valid AND indexes_valid.valid
  AND EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = to_regclass('loyal_yield.earn_reserve_share_prices')
      AND contype = 'c' AND convalidated
      AND pg_get_expr(conbin, conrelid) =
        '((share_price > (0)::double precision) AND (share_price < ''Infinity''::double precision))'
  )
  AND EXISTS (
    SELECT 1 FROM pg_attrdef
    WHERE adrelid = to_regclass('loyal_yield.earn_reserve_share_prices')
      AND adnum = (SELECT attnum FROM pg_attribute
        WHERE attrelid = adrelid AND attname = 'id')
      AND pg_get_expr(adbin, adrelid) LIKE 'nextval(%'
  ) AS valid
FROM columns_valid CROSS JOIN indexes_valid;
