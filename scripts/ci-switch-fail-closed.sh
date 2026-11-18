#!/usr/bin/env bash
# Fails when a switch script can compare two remote reads without proof that both answered: round 4 of the AWS move found
# preflights that printed "identical" for two failed checksum reads and "both (no answer)" for an architecture read that failed.
# Round 8 added the quiet-grep pipe: xstock's S2 switch aborted on a healthy Prometheus because `curl | grep -qx` under pipefail fails when grep
# exits at the first match and curl dies on the closed pipe.
# Usage: scripts/ci-switch-fail-closed.sh [script...]   (default: every shell script under deploy/aws that a switch runs; run it on the xbots S2 scripts by path)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
files=("$@")
[ ${#files[@]} -gt 0 ] || files=(deploy/aws/*.sh deploy/aws/switch/*.sh deploy/aws/db/*.sh)
exec python3 -I - "${files[@]}" <<'PY'
import re, sys, pathlib

bad = []

# Two command substitutions compared where the comparison cannot pass on two failed reads (file, text on the line, why).
TWO_SUBS_OK = [
    ("loyal-receivers-repin.sh", '[ "$(openssl x509 -in "$t/server.pem" -noout -pubkey)" = "$(cat "$t/pub.pem")" ]', "pub.pem fed `openssl x509 -force_pubkey` two lines up, so it is not empty"),
    ("pgmove.sh", "catch_sum src", "catch_sum is q, and a failed q kills the run (die signals the main shell)"),
    ("pgmove.sh", "-s $(fence_id) &&", "the test `-s` on the id file guards the comparison, so the right side is not empty"),
    ("pgmove.sh", "-s $(fence_ok) ]] &&", "the test `-s` on the ok file guards the comparison, so the right side is not empty"),
    ("pgmove.sh", '"$(volume_dropin)"', "volume_dropin prints a fixed unit text, so the right side is never empty"),
]

def flag(f, n, fn, what, use):
    bad.append(f"{f}:{n} ({fn or 'top level'}): {what}; instead {use}")

QUIET = r"(?:\S*/)?e?grep\s+(?:-\S+\s+)*?(?:-[A-Za-z]*q[A-Za-z]*|--quiet)(?=[\s\"'\\]|$)"
QUIET_PIPE = re.compile(r"(?<![|\\])\|(?!\|)\s*" + QUIET)
QUIET_START = re.compile(r"^\s*" + QUIET)

for f in sys.argv[1:]:
    p = pathlib.Path(f)
    fn = ""
    prev = ""
    for n, line in enumerate(p.read_text().split("\n"), 1):
        before, prev = prev, line.rstrip()
        m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if m:
            fn = m.group(1)
        elif line.startswith("}"):
            fn = ""
        if line.lstrip().startswith("#"):
            continue
        if re.search(r"\$\([^()]*\)\"?\s*(==|!=|=)\s*\"?\$\(", line) and not any(f.endswith(a) and b in line for a, b, _ in TWO_SUBS_OK):
            flag(f, n, fn, "two command substitutions compared: two failed reads are two empty strings and compare equal",
                 "read each into a variable under `|| fail`, then compare the variables (rd and sha_set in s3-weather.sh, same_read in s2-lib.sh)")
        if re.search(r"(sha|hash|digest)\w*\(\)", line) and re.search(r"\|\|\s*(true|:|echo)\b", line):
            flag(f, n, fn, "a hash read that ends in `|| true`: a failed read becomes an empty hash", "let the failure through (hex64 in s2-lib.sh rejects an empty answer)")
        if fn in ("preflight_checks", "cmd_preflight") and re.search(r"\b(oldsh|newsh)\b", line) and "rd " not in line:
            flag(f, n, fn, "a preflight check reads a box without rd: its failure or empty answer can pass", "rd old|new COMMAND, then block when it fails")
        if re.search(r"\|\s*head\b[^()]*\)\s*\|\|", line) or re.search(r"\|\s*grep\s+-\w*m\s*\d[^()]*\)\s*\|\|", line):
            flag(f, n, fn, "a pipe cut short by head or grep -m inside a read that falls back with `||`: under pipefail the cut pipe reads as a failed read, so a long answer passes as an empty one",
                 "read the whole answer into a variable first, then grep it (push_log_errors in s1-mnemonic.sh)")
        if QUIET_PIPE.search(line) or (QUIET_START.search(line) and re.search(r"(?<!\|)\|$", before)):
            flag(f, n, fn, "a pipe into a quiet grep: grep -q exits at the first match, the writer then dies on the closed pipe, and under pipefail (here or on the remote side) the pipeline reports failure for a match",
                 "read the output into a variable first and test it with a here-string (`grep -q ... <<<\"$var\"`), count with `grep -c ... || true` and compare the number, or drop the q and send the output away (`grep ... >/dev/null`), which reads the whole pipe and keeps the pipeline's failure")
        if f.endswith("s1-mnemonic.sh") and re.search(r"\$\{\w+:-0\}", line):
            flag(f, n, fn, "a missing statistic defaults to zero", "rsync_stats, which fails on a missing line")
for b in bad:
    print(b)
print(f"switch fail-closed: {len(bad)} finding(s) in {len(sys.argv) - 1} script(s)")
sys.exit(1 if bad else 0)
PY
