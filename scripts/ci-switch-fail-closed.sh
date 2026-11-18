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

# Two command substitutions compared where the comparison cannot pass on two failed reads (exact repo path, the whole stripped line, why).
TWO_SUBS_OK = [
    ('deploy/aws/loyal-receivers-repin.sh', r'''[ "$(openssl x509 -in "$t/server.pem" -noout -pubkey)" = "$(cat "$t/pub.pem")" ]''', 'pub.pem fed `openssl x509 -force_pubkey` two lines up, so it is not empty'),
    ('deploy/aws/db/pgmove.sh', r'''if [[ $(catch_sum src "$table" "$floor" "$cutoff") == "$(catch_sum "$FROM" "$table" "$floor" "$cutoff")" ]]; then state "$f" "$ceiling"; else need catchup "$table" "$floor"; fi''', 'catch_sum is q, and a failed q kills the run (die signals the main shell)'),
    ('deploy/aws/db/pgmove.sh', r'''if [[ -s $(fence_id) && $(cat "$WORK/drained-$sub" 2>/dev/null) == "$(cat "$(fence_id)")" ]]; then''', 'the test `-s` on the id file guards the comparison, so the right side is not empty'),
    ('deploy/aws/db/pgmove.sh', r'''if ((DRY == 0 && REVERSE == 0)) && [[ -s $(fence_ok) ]] && [[ $(cut -s -d' ' -f2- "$WORK/hash-ok" 2>/dev/null) != "$(cat "$(fence_ok)")" ]]; then''', 'the test `-s` on the ok file guards the comparison, so the right side is not empty'),
    ('deploy/aws/db/pgmove.sh', r'''if [[ $(on "$DST_SSH" "cat $f" 2>/dev/null || true) != "$(volume_dropin)" ]]; then''', 'volume_dropin prints a fixed unit text, so the right side is never empty'),
    ('deploy/aws/db/pgmove.sh', r'''[[ $(on "$DST_SSH" "cat $f" 2>/dev/null || true) == "$(volume_dropin)" ]] || { diff_row "volume: no $f; run init $CLUSTER (writes it and reloads systemd, no restart)"; return 0; }''', 'volume_dropin prints a fixed unit text, so the right side is never empty'),
]

def flag(f, n, fn, what, use):
    bad.append(f"{f}:{n} ({fn or 'top level'}): {what}; instead {use}")

# Pipe stages that stop reading early (exact repo path, the whole stripped line, why the writer ends on its own or the cut is harmless).
EARLY_OK = []

# Words that put a command before the one that matters: `sudo -n grep`, `LC_ALL=C grep`, `command grep`, `\grep`.
WRAP = {"{", "(", "!", "if", "then", "else", "elif", "do", "while", "until", "time", "command", "builtin", "exec", "env", "nice", "sudo", "timeout"}
WRAP_ARG = {"sudo": {"-u", "-g", "-h", "-p", "-C", "-D", "-R", "-T", "-U"}, "timeout": {"-s", "-k"}, "nice": {"-n"}, "env": {"-u"}}
GREPS = {"grep", "egrep", "fgrep", "zgrep", "rg"}
GREP_ARG = set("efmABCdDgtTj")

def words(s):
    out, cur, has, q, i = [], "", False, None, 0
    while i < len(s):
        c = s[i]
        if q == "'":
            if c == "'":
                q = None
            else:
                cur += c
        elif q == '"':
            if c == '"':
                q = None
            elif c == "\\" and i + 1 < len(s):
                i += 1
                cur += s[i]
            else:
                cur += c
        elif c in " \t":
            if has:
                out.append(cur)
                cur, has = "", False
        elif c in "'\"":
            q, has = c, True
        elif c == "\\" and i + 1 < len(s):
            i += 1
            cur += s[i]
            has = True
        elif c == "#" and not has:
            break
        elif c in ");" or s.startswith("&&", i) or s.startswith("||", i):
            break
        else:
            cur += c
            has = True
        i += 1
    if has:
        out.append(cur)
    return out

def command(ws):
    i = 0
    while i < len(ws):
        w = ws[i]
        if re.match(r"[A-Za-z_]\w*=", w):
            i += 1
        elif w in WRAP:
            i += 1
            while i < len(ws) and ws[i].startswith("-") and w in WRAP_ARG:
                i += 2 if ws[i] in WRAP_ARG[w] else 1
            if w == "timeout" and i < len(ws):
                i += 1
        else:
            return w.rsplit("/", 1)[-1], ws[i + 1:]
    return "", []

def grep_early(args):
    pos = False
    i = 0
    while i < len(args):
        a = args[i]
        if pos or not a.startswith("-") or a == "-":
            i += 1
            continue
        if a == "--":
            pos = True
        elif a.startswith("--"):
            n = a.split("=")[0]
            if n in ("--quiet", "--silent"):
                return "q"
            if n in ("--max-count", "--files-with-matches", "--files-without-match"):
                return "m"
            if n in ("--regexp", "--file", "--after-context", "--before-context", "--context", "--exclude", "--include", "--directories", "--devices", "--glob", "--type") and "=" not in a:
                i += 1
        else:
            for j, c in enumerate(a[1:]):
                if c == "q":
                    return "q"
                if c in "mlL":
                    return "m"
                if c in GREP_ARG:
                    if j == len(a) - 2:
                        i += 1
                    break
        i += 1
    return ""

def sed_quits(args):
    for a in args:
        a = re.sub(r"(?<![\w\\])[sy](.).*?\1.*?\1\w*", "", a)
        a = re.sub(r"/[^/]*/", "", a)
        if re.search(r"(?:^|[\s;{}!$\d,])q\d*\s*(?:$|[;}])", a):
            return True
    return False

def awk_exits(args):
    for a in args:
        a = re.sub(r'"(?:[^"\\]|\\.)*"', '""', a)
        while True:
            m = re.search(r"\bEND\s*\{", a)
            if not m:
                break
            depth, k = 1, m.end()
            while k < len(a) and depth:
                depth += (a[k] == "{") - (a[k] == "}")
                k += 1
            a = a[:m.start()] + a[k:]
        if re.search(r"\bexit\b", a):
            return True
    return False

def early_reader(stage):
    cmd, args = command(words(stage))
    if cmd in GREPS:
        return grep_early(args)
    if cmd == "head":
        return "m"
    if cmd == "sed" and sed_quits(args):
        return "m"
    if cmd in ("awk", "gawk", "mawk") and awk_exits(args):
        return "m"
    return ""

# A test (if, elif, while, until, !, &&) on a pipeline whose first stage reads a box: the filter's "no" and a dead ssh look the same.
BOX_READERS = r"(?:ssh|on|old|new|oldsh|newsh|rd|mnem|remote|as_root|box_pgmove)"
BOX_TEST = re.compile(r"(?:\b(?:if|elif|while|until)\s+(?:!\s*)?(?:\[\[.*?\]\]\s*&&\s*)*|!\s+|&&\s*)(" + BOX_READERS + r")\s")

def box_pipe(line):
    for m in BOX_TEST.finditer(line):
        q, i = None, m.start(1)
        while i < len(line):
            c = line[i]
            if q:
                if c == "\\" and q == '"':
                    i += 1
                elif c == q:
                    q = None
            elif c in "'\"":
                q = c
            elif c == "|" and not line.startswith("||", i) and line[i - 1] not in "|\\":
                return True
            elif c in ";)" or line.startswith("&&", i) or line.startswith("||", i):
                break
            i += 1
    return False

PIPE = re.compile(r"(?<![|\\])\|(?!\|)&?")

def allowed(table, rel, stripped):
    return any(rel == a and stripped == b for a, b, _ in table)

top = pathlib.Path.cwd().resolve()

for f in sys.argv[1:]:
    p = pathlib.Path(f)
    try:
        rel = str(p.resolve().relative_to(top))
    except ValueError:
        rel = str(p)
    fn = ""
    open_pipe = False
    for n, line in enumerate(p.read_text().split("\n"), 1):
        stripped = line.strip()
        carry = open_pipe
        m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if m:
            fn = m.group(1)
        elif line.startswith("}"):
            fn = ""
        if not stripped or stripped.startswith("#"):
            continue
        open_pipe = bool(re.search(r"(?<![|\\])\|&?$", re.sub(r"\\$", "", line.rstrip()).rstrip()))
        if re.search(r"\$\([^()]*\)\"?\s*(==|!=|=)\s*\"?\$\(", line) and not allowed(TWO_SUBS_OK, rel, stripped):
            flag(f, n, fn, "two command substitutions compared: two failed reads are two empty strings and compare equal",
                 "read each into a variable under `|| fail`, then compare the variables (rd and sha_set in s3-weather.sh, same_read in s2-lib.sh)")
        if re.search(r"(sha|hash|digest)\w*\(\)", line) and re.search(r"\|\|\s*(true|:|echo)\b", line):
            flag(f, n, fn, "a hash read that ends in `|| true`: a failed read becomes an empty hash", "let the failure through (hex64 in s2-lib.sh rejects an empty answer)")
        if fn in ("preflight_checks", "cmd_preflight") and re.search(r"\b(oldsh|newsh)\b", line) and "rd " not in line:
            flag(f, n, fn, "a preflight check reads a box without rd: its failure or empty answer can pass", "rd old|new COMMAND, then block when it fails")
        kinds = {early_reader(st) for k, st in enumerate(PIPE.split(line)) if k or carry}
        if "q" in kinds:
            flag(f, n, fn, "a pipe into a quiet grep: grep -q exits at the first match, the writer then dies on the closed pipe, and under pipefail (here or on the remote side) the pipeline reports failure for a match",
                 "read the output into a variable first and test it with a here-string (`grep -q ... <<<\"$var\"`), count with `grep -c ... || true` and compare the number, or drop the q and send the output away (`grep ... >/dev/null`), which reads the whole pipe and keeps the pipeline's failure")
        elif "m" in kinds and not allowed(EARLY_OK, rel, stripped):
            flag(f, n, fn, "a pipe into a reader that stops early (head, grep -m or -l, sed with q, awk with exit): the writer dies on the closed pipe and under pipefail (here or on the remote side) the pipeline reports failure for a good answer",
                 "use a reader that drains: `sed -n '1,Np'`, `awk 'NR<=N'`, awk setting a flag in place of exit, or read the whole answer into a variable first; a writer that never ends goes on EARLY_OK with that reason")
        if box_pipe(line):
            flag(f, n, fn, "a test on a pipeline that starts with a read of a box: when ssh or docker fails, the filter sees nothing and the test reads it as the answer no",
                 "read the answer into a variable under `|| { echo ...; exit 1; }` (loyal-clickstack-copy.sh final does), then test the variable")
        if f.endswith("s1-mnemonic.sh") and re.search(r"\$\{\w+:-0\}", line):
            flag(f, n, fn, "a missing statistic defaults to zero", "rsync_stats, which fails on a missing line")
for b in bad:
    print(b)
print(f"switch fail-closed: {len(bad)} finding(s) in {len(sys.argv) - 1} script(s)")
sys.exit(1 if bad else 0)
PY
