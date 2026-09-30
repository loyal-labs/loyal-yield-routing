#!/usr/bin/env python3
"""Offline rejection checks; never connects, requests, or writes."""
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[2]
base = {'PATH': os.environ['PATH'], 'REALTIME_FIXTURE_ONLY': 'true',
        'NEON_DATABASE_URL': 'postgres://fixture@127.0.0.1:55439/realtime_fixture',
        'REALTIME_URL': 'http://127.0.0.1:55440'}
cases = [
    {'REALTIME_FIXTURE_ONLY': ''},
    {'NEON_DATABASE_URL': 'postgres://fixture@production.example/realtime_fixture'},
    {'NEON_DATABASE_URL': 'postgres://fixture@127.0.0.1/production'},
    {'NEON_DATABASE_URL': 'postgres://fixture@127.0.0.1/realtime_fixture?host=production.example'},
    {'NEON_DATABASE_URL': 'https://fixture@127.0.0.1/realtime_fixture'},
    {'REALTIME_URL': 'https://production.example'},
    {'REALTIME_URL': 'http://fixture@127.0.0.1:55440'},
    {'REALTIME_URL': 'http://127.0.0.1:55440?redirect=denied'},
    {'NEON_DATABASE_URL': 'postgres://fixture@127.0.0.1/realtime_fixture'},
    {'NEON_DATABASE_URL': 'postgres://fixture@127.0.0.1:0/realtime_fixture'},
    {'REALTIME_URL': 'http://127.0.0.1'},
    {'REALTIME_URL': 'http://127.0.0.1:0'},
]
for case in cases:
    result = subprocess.run(['bun', 'scripts/hetzner-realtime/verify-sse-fixture.ts'],
                            cwd=root, env={**base, **case}, capture_output=True, text=True, timeout=5)
    assert result.returncode != 0 and 'Requires REALTIME_FIXTURE_ONLY' in result.stderr
print(f'PASS: {len(cases)} unsafe inputs rejected before transport initialization')
