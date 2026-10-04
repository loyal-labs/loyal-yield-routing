#!/usr/bin/env python3
"""Own subprocess signal tests. No service, container, DB or provider effects."""
import json, pathlib, selectors, signal, subprocess, time

ROOT = pathlib.Path(__file__).resolve().parent
BINARY = ROOT / 'target/debug/ata-monitor-shutdown-probe'
out = {'scope': 'actual_posix_shutdown_components', 'cases': [],
       'production_entrypoint_verified': False, 'database_settlement_verified': False,
       'migration_acceptance': False}

def test(mode, timeout, expected_return, marker):
    with subprocess.Popen([str(BINARY), mode], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          env={'PATH': '/usr/bin:/bin', 'LANG': 'C'}) as child:
        try:
            started = time.monotonic()
            if mode != 'abort-wrapper':
                with selectors.DefaultSelector() as selector:
                    selector.register(child.stdout, selectors.EVENT_READ)
                    assert selector.select(timeout=3), 'probe not ready'
                    assert child.stdout.readline() == b'READY\n'
                assert child.poll() is None
                child.send_signal(signal.SIGTERM)
                started = time.monotonic()
            stdout, stderr = child.communicate(timeout=timeout)
            elapsed = time.monotonic() - started
            assert len(stdout) + len(stderr) <= 16384
            assert child.returncode == expected_return
            if marker is not None:
                assert marker in (stderr if expected_return else stdout)
            if mode == 'blocked-runtime':
                assert 49.5 <= elapsed < 55, 'watchdog outside measured bound'
                assert not stdout and not stderr, 'watchdog unexpectedly used blocked output'
            out['cases'].append({'mode': mode, 'exit': child.returncode,
                                 'elapsed_seconds': round(elapsed, 3), 'passed': True})
        finally:
            if child.poll() is None:
                child.kill()
                child.wait(timeout=3)

test('abort-wrapper', 4, 0, b'INNER_ABORTED')
test('signal', 4, 0, b'STOPPED')
test('blocked-runtime', 55, 1, None)
out['verdict'] = 'PASS'
print(json.dumps(out, sort_keys=True))
