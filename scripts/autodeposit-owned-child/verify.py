#!/usr/bin/env python3
"""Real synthetic process proof; no DB/RPC, signer, credentials or journal fixture."""
import os
import json
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import tempfile
import time

binary = Path(__file__).parent / 'target/debug/autodeposit-owned-child-probe'

def proc(pid):
    # Read only explicitly owned synthetic process identities, never a host scan.
    path = Path(f'/proc/{pid}/stat')
    try:
        tail = path.read_text().rsplit(')', 1)[1].split()
        status = Path(f'/proc/{pid}/status').read_text()
    except FileNotFoundError:
        return None
    euid = int(next(line for line in status.splitlines() if line.startswith('Uid:')).split()[2])
    return {'state': tail[0], 'ppid': int(tail[1]), 'pgid': int(tail[2]), 'sid': int(tail[3]), 'euid': euid}

def running(pid):
    identity = proc(pid)
    return identity is not None and identity['state'] != 'Z'

if sys.platform != 'linux':
    raise SystemExit('UNTESTED: this ownership/subreaper probe requires actual Linux /proc')

with tempfile.TemporaryDirectory(prefix='autodeposit-child-') as directory:
    root = Path(directory)
    forbidden = root / 'post-stop-spawn'
    command = 'touch ' + shlex.quote(str(forbidden))
    subprocess.run([str(binary), '--stop-before-spawn', command], check=True, timeout=3)
    if forbidden.exists(): raise RuntimeError('new executor admitted after stop')
    print('pre-admission SIGTERM: actual handler rejected spawn, marker absent', flush=True)
    unrelated = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'], start_new_session=True)
    try:
        for name, resistant, early_exit in [('term', False, False), ('kill', True, False), ('shell_exit', True, True)]:
            marker = root / name
            code = ('import os,signal,time,json; '
                    + ('signal.signal(signal.SIGTERM,signal.SIG_IGN); ' if resistant else '')
                    + f'open({str(marker)+".tmp"!r},"w").write(json.dumps(dict(pid=os.getpid(),ppid=os.getppid(),euid=os.geteuid(),sid=os.getsid(0),pgid=os.getpgrp()))); os.rename({str(marker)+".tmp"!r},{str(marker)!r}); time.sleep(120)' )
            command = ('trap "" TERM; ' if resistant else '') + shlex.quote(sys.executable) + ' -c ' + shlex.quote(code) + ' & '
            if early_exit:
                command += f'while [ ! -s {shlex.quote(str(marker)+".release")} ]; do sleep 0.01; done; exit 7'
            else:
                command += 'wait'
            probe = subprocess.Popen([str(binary), command])
            child_pid = None
            try:
                deadline = time.monotonic() + 5
                while not marker.exists() or not marker.read_text():
                    if time.monotonic() >= deadline: raise RuntimeError('child did not start')
                    time.sleep(.01)
                identity = json.loads(marker.read_text())
                child_pid = identity['pid']
                leader = identity['ppid']
                parent = proc(probe.pid)
                if parent is None or identity['euid'] != os.geteuid() or parent['euid'] != os.geteuid():
                    raise RuntimeError('unexpected effective UID')
                if identity['pgid'] != leader or identity['sid'] != parent['sid']:
                    raise RuntimeError('child PGID/session is not exact owned shell identity')
                if identity['pgid'] in (os.getpgrp(), os.getpgid(probe.pid), os.getpgid(unrelated.pid)):
                    raise RuntimeError('owned group overlaps parent or unrelated process')
                print(json.dumps({'case': name, 'probe_pid': probe.pid, 'leader': leader, 'child': identity, 'parent': parent}), flush=True)
                shell = proc(leader)
                if shell is None or shell['ppid'] != probe.pid or shell['pgid'] != leader or shell['euid'] != os.geteuid() or shell['sid'] != parent['sid']:
                    raise RuntimeError('shell identity/ownership does not match probe')
                before = time.monotonic()
                if early_exit:
                    Path(str(marker)+'.release').write_text('exit')
                if not early_exit:
                    pgid = os.getpgid(child_pid)
                    if pgid == os.getpgid(os.getpid()) or pgid == os.getpgid(unrelated.pid):
                        raise RuntimeError('executor group not isolated')
                    probe.send_signal(signal.SIGTERM)
                rc = probe.wait(timeout=25)
                elapsed = time.monotonic() - before
                if rc != (7 if early_exit else 0): raise RuntimeError(f'{name}: unexpected exit {rc}')
                deadline = time.monotonic() + 2
                while running(child_pid) and time.monotonic() < deadline: time.sleep(.02)
                if proc(child_pid) is not None or proc(leader) is not None:
                    raise RuntimeError('owned leader/descendant remains present; Linux reaping unproved')
                if unrelated.poll() is not None: raise RuntimeError('unrelated process was signaled')
                if resistant and not early_exit and elapsed < 19: raise RuntimeError('TERM grace not exercised')
                print(f'{name}: real descendant stopped, unrelated process survived, {elapsed:.3f}s')
            finally:
                if probe.poll() is None:
                    probe.send_signal(signal.SIGTERM)
                    probe.wait(timeout=25)
                if child_pid:
                    try:
                        if running(child_pid):
                            os.kill(child_pid, signal.SIGKILL)
                    except PermissionError:
                        print('Local process observation denied; no closure claim', file=sys.stderr)
        rc = subprocess.run([str(binary), 'exit 9'], timeout=3).returncode
        if rc != 9: raise RuntimeError('normal executor exit classification changed')
        print('normal exit: original exit code 9 retained')
    finally:
        unrelated.terminate()
        unrelated.wait(timeout=3)
