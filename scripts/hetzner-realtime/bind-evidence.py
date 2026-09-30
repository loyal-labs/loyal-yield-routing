#!/usr/bin/env python3
"""Bind local supporting measurements to the exact inputs and binary tested."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import time

root = Path(__file__).resolve().parents[2]
output = Path(sys.argv[2]).resolve()
inputs = [root/'Cargo.toml', root/'Cargo.lock']
for relative in ['crates/loyal-yield-realtime', 'crates/loyal-yield-realtime-core', 'crates/loyal-observability']:
    inputs.extend(p for p in (root/relative).rglob('*') if p.is_file())
inputs.extend(p for p in (root/'scripts/hetzner-realtime').iterdir() if p.is_file())
inputs.extend(root/'crates/loyal-yield-store/migrations'/name for name in [
    '0010_realtime_events.sql', '0013_earn_realtime_events.sql', '0015_realtime_web_mobile_protocol.sql'])

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

snapshot = dict(source_commit=subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip(),
                files={str(p.relative_to(root)): digest(p) for p in sorted(set(inputs))})
if sys.argv[1] == 'snapshot':
    (output/'source-inputs.json').write_text(json.dumps(snapshot, indent=2)+'\n')
elif sys.argv[1] == 'binary':
    (output/'binary-input.json').write_text(json.dumps(dict(
        binary_sha256=digest(root/'target/debug/loyal-yield-realtime')), indent=2)+'\n')
else:
    binary_before = json.loads((output/'binary-input.json').read_text())
    if binary_before['binary_sha256'] != digest(root/'target/debug/loyal-yield-realtime'):
        raise RuntimeError('runtime binary changed during tests; evidence is invalid')
    before = json.loads((output/'source-inputs.json').read_text())
    if before != snapshot:
        raise RuntimeError('source inputs changed during build/test; evidence is invalid')
    runtime = json.loads((output/'runtime.json').read_text())
    result = dict(runtime)
    result['source_identity'] = before
    result['target_identity'] = dict(binary_sha256=digest(root/'target/debug/loyal-yield-realtime'),
                                     platform=subprocess.check_output(['uname', '-sm'], text=True).strip())
    result['collected_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
    result['artifacts'] = {p.name: digest(p) for p in sorted(output.iterdir())
                           if p.is_file() and p.name != 'evidence.json'}
    result['measurements']['cleanup'] = json.loads((output/'cleanup.json').read_text())
    (output/'evidence.json').write_text(json.dumps(result, indent=2)+'\n')
