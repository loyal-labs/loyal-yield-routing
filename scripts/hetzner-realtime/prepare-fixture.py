#!/usr/bin/env python3
"""Print minimal fixture DDL using the pinned existing event functions.

Pipe only into the disposable database created in README; no full store
migrations or financial tables are required for this SSE fixture.
"""
from pathlib import Path
import re

root = Path(__file__).resolve().parents[2]
base = (root / 'crates/loyal-yield-store/migrations/0010_realtime_events.sql').read_text()
identity = (root / 'crates/loyal-yield-store/migrations/0013_earn_realtime_events.sql').read_text()
protocol = (root / 'crates/loyal-yield-store/migrations/0015_realtime_web_mobile_protocol.sql').read_text()

def function(source, name):
    match = re.search(r'CREATE OR REPLACE FUNCTION loyal_yield\.' + name + r'\([\s\S]*?\n\$\$;', source)
    if not match:
        raise RuntimeError(f'missing existing function {name}')
    return match.group()

print("CREATE SCHEMA loyal_yield;")
print(base.split('CREATE OR REPLACE FUNCTION')[0])
print("""ALTER TABLE loyal_yield.realtime_events
  ADD COLUMN schema_version SMALLINT NOT NULL DEFAULT 1,
  ADD COLUMN earn_vault_address TEXT,
  ADD COLUMN failure_code TEXT,
  ADD COLUMN deliverable BOOLEAN NOT NULL DEFAULT FALSE;""")
print(function(identity, 'realtime_private_scope_requires_identity'))
print(function(protocol, 'emit_realtime_event'))
