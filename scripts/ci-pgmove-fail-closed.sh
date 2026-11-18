#!/usr/bin/env bash
# Fails when a query in the database move tool can fail and the run carry on as if it answered:
# round 1 of the AWS move found checks that printed "0 DIFF" and "verify: ok" with the source down.
# Usage: scripts/ci-pgmove-fail-closed.sh [dir]   (default deploy/aws/db)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
dir=${1:-deploy/aws/db}
exec python3 -I - "$dir" <<'PY'
import re, sys, pathlib

d = pathlib.Path(sys.argv[1])
FILES = ["pgmove.sh", "cutover.sh", "remote.sh"]
# Words that run a query or a step that runs queries; q and dump stop the run themselves.
QUERY = r'(?:q|dump|sql|psql|pg_dump|src_dump|pq|pg|pg_counted|box_pgmove|pgmove_on|forward_loop|src_run|"?\$admin"?)'
RAW = r'(?:sql|psql|pg_dump|src_dump|pq|pg|box_pgmove|pgmove_on|forward_loop|src_run|"?\$admin"?)'
CMDPOS = r'(?:^|[|;&(]|\$\(|\|\||&&|\bthen\b|\bdo\b|\belse\b|!)\s*'
FILTERS = {"q", "hash_meta", "only_in", "sort", "awk", "grep", "sed", "printf", "comm", "join", "cut", "echo", "cat", "tr", "head", "tail", "paste"}
# Raw sql whose status is not lost: the subcommand's own exit status, and the dry-run probe of a target that may not exist yet.
SQL_OK = {("pgmove.sh", "q"), ("pgmove.sh", "cmd_catchout"), ("pgmove.sh", "")}
bad = []

def flag(f, n, fn, what, use):
    bad.append(f"{f}:{n} ({fn or 'top level'}): {what}; instead {use}")

def statements(path):
    lines = path.read_text().split("\n")
    fn, i, term = "", 0, None
    while i < len(lines):
        line, n = lines[i], i + 1
        i += 1
        if term is not None:
            if line.strip() == term:
                term = None
            continue
        m = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if m:
            fn = m.group(1)
        elif line.startswith("}"):
            fn = ""
        text = line
        while re.search(r"(\||\\|&&|\|\|)\s*$", text) and i < len(lines) and not text.lstrip().startswith("#"):
            text += "\n" + lines[i]
            i += 1
        h = re.search(r"<<-?\s*'?([A-Za-z_]+)'?", text)
        if h and "<<<" not in text[h.start() - 1:h.start() + 3]:
            term = h.group(1)
        if text.lstrip().startswith("#"):
            continue
        yield n, fn, re.sub(r"'[^'\n]*'", "''", text), text

for f in FILES:
    p = d / f
    if not p.exists():
        continue
    src = p.read_text()
    if not re.search(r"^set -euo pipefail$", src, re.M):
        flag(f, 1, "", "no `set -euo pipefail`", "start the script with it, so a failed query in a pipeline fails the pipeline")
    for n, fn, s, raw in statements(p):
        m = re.search(CMDPOS + "(" + QUERY + r")\b([^\n]*?)\|\|\s*(true|:|return 0)\b", s)
        # `[[ -n $(q ...) ]] || return 0` tests an answer: q itself stops the run when it has none.
        if m and not (m.group(1) in ("q", "dump") and "]]" in m.group(2)):
            flag(f, n, fn, "`|| true`, `|| :` or `|| return 0` after a query drops its failure",
                 "q or dump in pgmove.sh (a failed query stops the run), pg_counted or `|| die` in cutover.sh and remote.sh")
        if re.search(r"^\s*(echo|printf)\b[^\n]*\$\(\s*" + RAW + r"\b", s):
            flag(f, n, fn, "a query inside an echo argument: echo's status replaces the query's",
                 "assign first, `x=$(...) || die \"...\"`, then echo $x")
        for m in re.finditer(r"<\(\s*([A-Za-z_\"$][A-Za-z0-9_\"$]*)", s):
            w = m.group(1)
            if w not in FILTERS:
                flag(f, n, fn, f"process substitution of `{w}`: bash never reads its exit status",
                     "q inside it (q stops the run), or write the output to a file under `|| die` and read the file")
        if re.search(r"\blocal\s+[A-Za-z_]\w*=\$\((?!\()", s):
            flag(f, n, fn, "`local x=$(...)` returns local's status, not the command's", "`local x; x=$(...) || die \"...\"`")
        if re.search(r"^\s*(?:local\s+)?[A-Za-z_]\w*=[^\n]*\$\(\s*\(\([^)]*\)\)\s*&&(?:(?!\|\|)[^)])*\)", s):
            flag(f, n, fn, "an assignment from `$( ((x)) && ... )`: when x is 0 it returns 1 and set -e ends the run with no message",
                 "an if, or `x=a; ((cond)) || x=b`")
        if f == "pgmove.sh" and (f, fn) not in SQL_OK and re.search(CMDPOS + r"sql\s+(src|dst|\"?\$)", s) \
                and not re.search(r"\|\|\s*(query_failed|die|return 1)\b", s):
            flag(f, n, fn, "a direct `sql` call: its failure is the caller's to notice", "q, or end the statement with `|| query_failed \"...\"`")
        if f != "pgmove.sh" and fn != "pg_counted" and re.search(CMDPOS + r"pg\s+writes\s+diff\b", s):
            flag(f, n, fn, "`pg writes diff` read without its total line", "pg_counted, which fails unless the diff answered")

    if f == "pgmove.sh":
        def body(name):
            m = re.search(r"^" + name + r"\(\)\s*\{(.*?)^\}", src, re.M | re.S)
            return m.group(1) if m else None
        qb, db, fb = body("q"), body("dump"), body("query_failed")
        if qb is None or "query_failed" not in qb or re.search(r"return\s+\"?\$rc", qb):
            flag(f, 1, "q", "q must end every failure in query_failed and never `return $rc`",
                 "a q whose failure no caller can turn into a pass: `query_failed \"$msg\"` on every failing path")
        if db is None or "query_failed" not in db:
            flag(f, 1, "dump", "pg_dump must run through dump(), which calls query_failed", "dump <side> <db> <flags>")
        if fb is None or "kill -USR1" not in fb or not re.search(r"^trap .*USR1", src, re.M):
            flag(f, 1, "query_failed", "query_failed must signal the top shell (kill -USR1 \"$PGMOVE_PID\") and the top shell trap USR1",
                 "so a failure inside $(...), a pipeline or a background job still stops the run")

for b in bad:
    print(b)
print(f"pgmove fail-closed: {len(bad)} finding(s) in {sys.argv[1]}")
sys.exit(1 if bad else 0)
PY
