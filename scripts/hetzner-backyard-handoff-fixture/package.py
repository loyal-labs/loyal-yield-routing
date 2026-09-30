#!/usr/bin/env python3
"""Allowlisted source archive; no traversal of env, git, deps or credentials."""
import argparse
import hashlib
import gzip
import json
from pathlib import Path
import subprocess
import tarfile
ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
parser = argparse.ArgumentParser()
parser.add_argument('archive', type=Path)
parser.add_argument('--checkout-commit', help='optional exact full HEAD SHA required by caller')
args = parser.parse_args()
out = args.archive.resolve()
deployed_source_commit = 'f821a78f6a5c0507eb1dada10fd300df1277f639'
checkout_commit = subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip()
if args.checkout_commit is not None and args.checkout_commit != checkout_commit:
    raise SystemExit('caller checkout commit does not match HEAD')
ancestry = subprocess.run(['git','merge-base','--is-ancestor',deployed_source_commit,checkout_commit],cwd=ROOT)
if ancestry.returncode != 0:
    raise SystemExit('checkout is not a verified descendant of deployed source commit')
files = [ROOT/'go/backyard-rwa-worker/go.mod',ROOT/'go/backyard-rwa-worker/go.sum']
files += sorted((ROOT/'go/backyard-rwa-worker/internal').rglob('*.go'))
files += sorted((ROOT/'go/backyard-rwa-worker/internal/backyardrwa/manifest').glob('*.json'))
files += [HERE/n for n in ('Dockerfile','run.py','schema.sql','README.md','package.py','linux-parent-run.sh')]
# Required schema provenance only; the test does not replay these migrations.
for prefix in ('0051','0053','0070','0071'):
    files += sorted((ROOT/'crates/loyal-yield-store/migrations').glob(prefix+'*.sql'))
file_hashes = {str(f.relative_to(ROOT)):hashlib.sha256(f.read_bytes()).hexdigest() for f in files}
manifest = {'deployed_source_commit':deployed_source_commit,
            'checkout_commit':checkout_commit,
            'scope':'Backyard synthetic handoff only',
            'source_identity':'allowlisted working-tree bytes; checkout commit does not attest uncommitted patches',
            'code_hashes':{name:digest for name,digest in file_hashes.items() if name.startswith('go/')},
            'files':file_hashes}
with out.open('wb') as destination:
    with gzip.GzipFile(filename='', mode='wb', fileobj=destination, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for f in files:
                if f.is_symlink() or not f.is_file(): raise SystemExit('regular files only')
                info = archive.gettarinfo(str(f), arcname=str(f.relative_to(ROOT)))
                info.uid = info.gid = info.mtime = 0
                info.uname = info.gname = ''
                info.mode = 0o644
                with f.open('rb') as source:
                    archive.addfile(info, source)
out.with_suffix(out.suffix+'.manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
print(json.dumps({'archive':str(out),'sha256':hashlib.sha256(out.read_bytes()).hexdigest(),'file_count':len(files),'manifest':str(out.with_suffix(out.suffix+'.manifest.json'))}))
