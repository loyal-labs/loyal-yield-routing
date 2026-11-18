#!/usr/bin/env bash
# Fails when a switch script can compare two remote reads without proof that both answered: round 4 of the AWS move found
# preflights that printed "identical" for two failed checksum reads and "both (no answer)" for an architecture read that failed.
# Usage: scripts/ci-switch-fail-closed.sh [script...]   (default: the S1 and S3 switch scripts; run it on the xbots S2 scripts by path)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
files=("$@")
[ ${#files[@]} -gt 0 ] || files=(deploy/aws/switch/s1-mnemonic.sh deploy/aws/s3-weather.sh)
exec python3 -I - "${files[@]}" <<'PY'
import re, sys, pathlib

bad = []

def flag(f, n, fn, what, use):
    bad.append(f"{f}:{n} ({fn or 'top level'}): {what}; instead {use}")

for f in sys.argv[1:]:
    p = pathlib.Path(f)
    fn = ""
    for n, line in enumerate(p.read_text().split("\n"), 1):
        m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if m:
            fn = m.group(1)
        elif line.startswith("}"):
            fn = ""
        if line.lstrip().startswith("#"):
            continue
        if re.search(r"\$\([^()]*\)\"?\s*(==|!=|=)\s*\"?\$\(", line):
            flag(f, n, fn, "two command substitutions compared: two failed reads are two empty strings and compare equal",
                 "read each into a variable under `|| fail`, then compare the variables (rd and sha_set in s3-weather.sh, same_read in s2-lib.sh)")
        if re.search(r"(sha|hash|digest)\w*\(\)", line) and re.search(r"\|\|\s*(true|:|echo)\b", line):
            flag(f, n, fn, "a hash read that ends in `|| true`: a failed read becomes an empty hash", "let the failure through (hex64 in s2-lib.sh rejects an empty answer)")
        if fn in ("preflight_checks", "cmd_preflight") and re.search(r"\b(oldsh|newsh)\b", line) and "rd " not in line:
            flag(f, n, fn, "a preflight check reads a box without rd: its failure or empty answer can pass", "rd old|new COMMAND, then block when it fails")
        if f.endswith("s1-mnemonic.sh") and re.search(r"\$\{\w+:-0\}", line):
            flag(f, n, fn, "a missing statistic defaults to zero", "rsync_stats, which fails on a missing line")
for b in bad:
    print(b)
print(f"switch fail-closed: {len(bad)} finding(s) in {len(sys.argv) - 1} script(s)")
sys.exit(1 if bad else 0)
PY
