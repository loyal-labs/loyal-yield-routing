#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/hetzner/db/remote.sh
source "$HERE/remote.sh"
CUTOVER_PROOF=${CUTOVER_PROOF:-/Users/user/loyal/artifacts/orchestrate/loyal-hetzner/proof/hz-db5-cutover}
INVENTORY_HOSTS=(loyal-workers loyal-eu-1 root@5.161.126.23 root@5.78.147.233)
ENGINES=(loyal-retail loyal-backyard)
SERVICES=(loyal-observer loyal-realtime loyal-telegram loyal-web loyal-admin loyal-stats)
TIMERS=(loyal-web-cron-smart-account-reconcile.timer loyal-web-cron-earn-yield-joy.timer)
CRONS=(loyal-web-cron@smart-account-reconcile.service loyal-web-cron@earn-yield-joy.service)
OWNERS=(loyal_app_owner loyal_yield_prod_owner kamino_timescale_owner)
NEON_ENDPOINTS=(curly-glade-19640752/ep-dry-forest-ah27inp9 purple-wave-56227231/ep-ancient-grass-aqb5aalu)
CRED_ROOT=/etc/loyal-cutover
STATE=/var/lib/loyal-db-replica/cutover
SILENCE_COMMENT='hz-db5 cutover: replica subscriptions disabled at switch, dropped at pause-neon'
TIMES=()

workers() { ssh -o BatchMode=yes loyal-workers "$@"; }
db() { box "loyal-db-replica $*"; }
die() { echo "cutover: $*" >&2; exit 1; }
now() { perl -MTime::HiRes=time -e 'printf "%.1f", time'; }

timed() {
  local label=$1 t rc
  shift
  t=$(now)
  printf '\n== %s (%s)\n' "$label" "$(date -u +%H:%M:%SZ)"
  "$@"
  rc=$?
  TIMES+=("$label $(perl -e "printf '%.1f', $(now) - $t")")
  echo "-- $label: ${TIMES[${#TIMES[@]} - 1]#* }s$( ((rc == 0)) || echo ", FAILED exit $rc")"
  return $rc
}

report_times() {
  echo
  echo "== step timings"
  printf '%s\n' "${TIMES[@]}" | awk '{ printf "  %-22s %7.1fs\n", $1, $2; t += $2 } END { printf "  %-22s %7.1fs\n", "total", t }'
}

health_path() {
  case $1 in
    loyal-web) echo 10080/readyz ;;
    loyal-admin) echo 10081/readyz ;;
    loyal-stats) echo 10082/readyz ;;
    loyal-telegram) echo 10083/healthz ;;
    loyal-realtime) echo 10000/readyz ;;
    loyal-observer) echo 9101/metrics ;;
    loyal-retail) echo 9102/metrics ;;
    loyal-backyard) echo 9103/metrics ;;
    *) die "no health path for $1" ;;
  esac
}

probe_py() {
  cat <<'PY'
import glob, json, os, re, subprocess, sys, urllib.parse as u

URL = re.compile(r"postgres(?:ql)?://[^\s\"'<>\\]+")
LOYAL = ("ep-dry-forest", "ep-ancient-grass", "10.92.1.1")
ROLES = {"neondb_owner", "loyal_web_stats_reader", "loyal_vault_demo_observation",
         "loyal_app_owner", "loyal_yield_prod_owner", "kamino_timescale_owner"}
only_loyal = sys.argv[1] == "loyal-only"
host = os.uname().nodename
seen, skipped = set(), 0

def run(*a):
    return subprocess.run(a, capture_output=True, text=True).stdout

def source(h, db):
    if h.startswith("ep-dry-forest"):
        return "app"
    if h.startswith("ep-ancient-grass"):
        return "timescale" if db == "kamino_timescale" else "yield"
    if h == "10.92.1.1":
        return {"loyal_app": "app", "loyal_yield_prod": "yield", "kamino_timescale": "timescale"}.get(db, "hetzner")
    return "other"

def emit(unit, state, cred, where, text):
    global skipped
    for m in URL.findall(text or ""):
        p = u.urlparse(m)
        h = p.hostname or "-"
        if only_loyal and not h.startswith(LOYAL):
            skipped += 1
            continue
        db = p.path.lstrip("/") or "-"
        role = u.unquote(p.username or "")
        role = role if role in ROLES else ("other" if role else "-")
        ep = h.split(".")[0].removesuffix("-pooler") if h.endswith("neon.tech") else "-"
        mode = "pooler" if "-pooler" in h or p.port == 6432 else "direct"
        row = (host, unit, state, cred, where, source(h, db), ep, h, db, role, mode)
        if row not in seen:
            seen.add(row)
            print("\t".join(row))

def creds_in(text, key):
    try:
        doc = json.loads(text)
    except ValueError:
        return [(key, text)]
    out = []
    def walk(v, path):
        if isinstance(v, dict):
            for k, x in v.items():
                walk(x, f"{path}.{k}")
        elif isinstance(v, list):
            for i, x in enumerate(v):
                walk(x, f"{path}[{i}]")
        elif isinstance(v, str):
            out.append((path, v))
    walk(doc, key)
    return out

def decrypt(path):
    r = subprocess.run(["systemd-creds", "decrypt", "--name=", path, "-"], capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else ""

def resolve(path):
    if path.startswith("/"):
        return path
    for d in ("/etc/credstore.encrypted", "/run/credstore.encrypted", "/etc/credstore"):
        if os.path.exists(f"{d}/{path}"):
            return f"{d}/{path}"
    return path

units = sorted({os.path.basename(p).removesuffix(".d") for p in
                glob.glob("/etc/systemd/system/*.service") + glob.glob("/etc/systemd/system/*.service.d")})
referenced = set()
for unit in units:
    state = run("systemctl", "is-active", unit).strip() or "-"
    if unit.endswith("@.service"):
        state = "template"
    for line in run("systemctl", "cat", unit).splitlines():
        key, _, val = line.partition("=")
        if key in ("LoadCredentialEncrypted", "LoadCredential"):
            name, _, path = val.partition(":")
            path = resolve(path or name)
            referenced.add(os.path.realpath(path))
            text = decrypt(path) if key == "LoadCredentialEncrypted" else open(path, errors="replace").read() if os.path.isfile(path) else ""
            for cred, v in creds_in(text, name):
                emit(unit, state, cred, path, v)
        elif key == "Environment":
            emit(unit, state, "Environment", "unit", val)
        elif key == "EnvironmentFile":
            path = val.lstrip("-")
            if os.path.isfile(path):
                emit(unit, state, "EnvironmentFile", path, open(path, errors="replace").read())

for path in sorted(glob.glob("/etc/credstore.encrypted/**", recursive=True)):
    if os.path.isfile(path) and os.path.realpath(path) not in referenced:
        for cred, v in creds_in(decrypt(path), os.path.basename(path)):
            emit("unreferenced", "-", cred, path, v)

if os.path.exists("/usr/bin/docker"):
    for cid in run("docker", "ps", "-q").split():
        name = run("docker", "inspect", "--format", "{{.Name}}", cid).strip().lstrip("/")
        emit(f"docker:{name}", "running", "Config.Env", "docker", run("docker", "inspect", "--format", "{{json .Config.Env}}", cid))

for unit in units:
    if unit.startswith("loyal-") and not any(r[1] == unit for r in seen):
        print("\t".join((host, unit, run("systemctl", "is-active", unit).strip() or "-", "-", "-", "none", "-", "-", "-", "-", "-")))

grep = subprocess.run(["timeout", "120", "grep", "-rIlE", "--exclude-dir=node_modules", "--exclude-dir=.git",
                       "--exclude-dir=proc", "-e", "ep-dry-forest|ep-ancient-grass",
                       "/etc", "/opt", "/srv", "/root", "/home"], capture_output=True, text=True).stdout
for path in grep.split():
    emit("plaintext-file", "-", "-", path, open(path, errors="replace").read())
print(f"# {host}: {len(seen)} rows, {skipped} non-Loyal URLs skipped, {len(units)} units read", file=sys.stderr)
PY
}

cmd_inventory() {
  local out=$CUTOVER_PROOF/inventory.tsv h mode
  mkdir -p "$CUTOVER_PROOF"
  {
    printf 'host\tunit\tstate\tcredential\tfile\tsource\tneon_endpoint\tdb_host\tdb\trole\tconn\n'
    for h in "${INVENTORY_HOSTS[@]}"; do
      mode=loyal-only
      [[ $h == loyal-workers ]] && mode=all
      probe_py | ssh -o BatchMode=yes -o ConnectTimeout=10 "$h" "python3 -I - $mode"
    done
  } >"$out.tmp"
  mv -f "$out.tmp" "$out"
  column -t -s$'\t' "$out"
}

creds_py() {
  cat <<'PY'
import json, os, shutil, subprocess, sys, urllib.parse as u

mode, expect = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else "")
ROOT, HZ = "/etc/loyal-cutover", "10.92.1.1"
TARGET = {"app": ("loyal_app", "loyal_app_owner"), "yield": ("loyal_yield_prod", "loyal_yield_prod_owner"),
          "timescale": ("kamino_timescale", "kamino_timescale_owner")}
PORT = {"direct": 5432, "pooler": 6432}
READ = {"app": "select count(*) from public.app_users",
        "yield": "select count(*) from loyal_yield.user_yield_positions",
        "timescale": "select max(observed_at) from kamino.reserve_updates"}

pw, creds = {}, {}
for line in sys.stdin:
    f = line.rstrip("\n").split("\t")
    if f[0] == "pw":
        pw[f[1]] = f[2]
    elif f[0] == "cred":
        unit, cred, path, src, conn = f[1:6]
        name, _, key = cred.partition(".")
        c = creds.setdefault(path, {"name": name, "key": key, "src": src, "conn": conn, "units": set()})
        if (c["name"], c["key"], c["src"], c["conn"]) != (name, key, src, conn):
            sys.exit(f"{path}: two units read it as different credentials")
        c["units"].add(unit)
if not creds:
    sys.exit("no Loyal database credentials in the inventory")

def run(args, data=None):
    r = subprocess.run(args, input=data, capture_output=True)
    if r.returncode:
        sys.exit(f"{' '.join(args[:2])} {args[-2]}: {r.stderr.decode(errors='replace').strip()[:200]}")
    return r.stdout

def decrypt(path, name):
    return run(["systemd-creds", "decrypt", f"--name={name}", path, "-"]).decode()

def encrypt(text, name, path):
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    run(["systemd-creds", "encrypt", "--with-key=host", f"--name={name}", "-", path + ".tmp"], text.encode())
    os.chmod(path + ".tmp", 0o600)
    os.replace(path + ".tmp", path)

def publish(src, dst):
    os.makedirs(os.path.dirname(dst), mode=0o700, exist_ok=True)
    shutil.copy2(src, dst + ".cutover")
    with open(dst + ".cutover", "rb") as f:
        os.fsync(f.fileno())
    os.replace(dst + ".cutover", dst)
    d = os.open(os.path.dirname(dst), os.O_RDONLY)
    os.fsync(d)
    os.close(d)

def neon_copy(path, name, key):
    r = subprocess.run(["systemd-creds", "decrypt", f"--name={name}", saved(path), "-"], capture_output=True, text=True)
    try:
        return r.returncode == 0 and on_neon(url(r.stdout, key))
    except (ValueError, KeyError, TypeError):
        return False

def staged(p):
    return ROOT + "/stage" + p

def saved(p):
    return ROOT + "/neon" + p

def url(text, key):
    if not key:
        return text.strip()
    v = json.loads(text)
    for k in key.split("."):
        v = v[k]
    return v

def with_url(text, key, new):
    tail = text[len(text.rstrip()):]
    if not key:
        return new + tail
    doc = json.loads(text)
    node = doc
    *parents, last = key.split(".")
    for k in parents:
        node = node[k]
    node[last] = new
    return json.dumps(doc) + tail

def where(v):
    p = u.urlparse(v)
    return f"{p.hostname}:{p.port or 5432}/{p.path.lstrip('/')}"

def on_neon(v):
    return (u.urlparse(v).hostname or "").endswith(".neon.tech")

def psql(v, sql):
    p = u.urlparse(v)
    env = dict(os.environ, PGHOST=p.hostname, PGPORT=str(p.port or 5432), PGDATABASE=p.path.lstrip("/"),
               PGUSER=u.unquote(p.username or ""), PGPASSWORD=u.unquote(p.password or ""),
               PGSSLMODE="require", PGCONNECT_TIMEOUT="10", PGAPPNAME="loyal-cutover-check")
    r = subprocess.run(["psql", "-XAtq", "-v", "ON_ERROR_STOP=1", "-c", sql], env=env, capture_output=True, text=True)
    return r.returncode == 0, r.stdout.strip() if r.returncode == 0 else (r.stderr.strip().splitlines() or ["?"])[-1][:200]

bad = 0
for path, c in sorted(creds.items()):
    name, key = c["name"], c["key"]
    label = f"{path} [{name}{'.' + key if key else ''}; {','.join(sorted(c['units']))}]"
    if mode == "stage":
        base = decrypt(saved(path) if os.path.exists(saved(path)) else path, name)
        old = url(base, key)
        if not on_neon(old):
            sys.exit(f"{label}: live value is {where(old)}, not Neon, and there is no saved Neon copy")
        db, role = TARGET[c["src"]]
        if role not in pw:
            sys.exit(f"{label}: no password for {role}")
        new = f"postgresql://{role}:{u.quote(pw[role], safe='')}@{HZ}:{PORT[c['conn']]}/{db}?sslmode=require"
        want = with_url(base, key, new)
        same = os.path.exists(staged(path)) and decrypt(staged(path), name) == want
        if not same:
            encrypt(want, name, staged(path))
        dropped = sorted(set(u.parse_qs(u.urlparse(old).query)) - {"sslmode"})
        print(f"{'unchanged' if same else 'staged'}: {label} {where(old)} -> {where(new)}, URL params dropped: {','.join(dropped) or 'none'}")
    elif mode in ("test-staged", "test-live"):
        p = staged(path) if mode == "test-staged" else path
        if not os.path.exists(p):
            print(f"FAIL {label}: {p} missing")
            bad += 1
            continue
        v = url(decrypt(p, name), key)
        if on_neon(v):
            print(f"FAIL {label}: points at Neon")
            bad += 1
            continue
        ok, out = psql(v, f"select current_user || ' ' || current_database() || ' read=' || coalesce(({READ[c['src']]})::text, 'null')")
        print(f"{'ok' if ok else 'FAIL'} {label} via {where(v)}: {out}")
        bad += not ok
    elif mode == "switch":
        if not os.path.exists(staged(path)):
            sys.exit(f"{label}: not staged; run preflight")
        live, want = decrypt(path, name), decrypt(staged(path), name)
        if not neon_copy(path, name, key):
            if not on_neon(url(live, key)):
                sys.exit(f"{label}: live value is not Neon and there is no valid saved Neon copy")
            publish(path, saved(path))
            if not neon_copy(path, name, key):
                sys.exit(f"{label}: saved Neon copy {saved(path)} does not decrypt to a Neon URL")
        if live == want:
            print(f"unchanged: {label} already on {where(url(want, key))}")
            continue
        publish(staged(path), path)
        print(f"switched: {label} -> {where(url(want, key))}, Neon copy in {saved(path)}")
    elif mode == "rollback":
        if not os.path.exists(saved(path)):
            print(f"unchanged: {label} was never switched")
            continue
        if decrypt(path, name) == decrypt(saved(path), name):
            print(f"unchanged: {label} already on Neon")
            continue
        publish(saved(path), path)
        print(f"rolled back: {label} -> {where(url(decrypt(path, name), key))}")
    elif mode == "status":
        live = url(decrypt(path, name), key)
        side = "neon" if on_neon(live) else "hetzner"
        ready = os.path.exists(staged(path))
        print(f"{side}: {label} live {where(live)}, staged={ready}, neon copy={os.path.exists(saved(path))}")
        bad += (expect in ("neon", "hetzner") and side != expect) or (expect == "staged" and not ready)
    else:
        sys.exit(f"unknown mode {mode}")
sys.exit(1 if bad else 0)
PY
}

cred_rows() {
  [[ -s $CUTOVER_PROOF/inventory.tsv ]] || die "no inventory; run $0 inventory"
  awk -F'\t' 'NR > 1 && $1 ~ /^loyal-workers/ && $2 != "unreferenced" && $6 ~ /^(app|yield|timescale)$/ { print "cred\t" $2 "\t" $4 "\t" $5 "\t" $6 "\t" $11 }' "$CUTOVER_PROOF/inventory.tsv"
}

install_creds() {
  creds_py | workers "install -d -m 0700 $CRED_ROOT && install -m 0700 /dev/stdin $CRED_ROOT/creds.py"
}

creds() {
  install_creds
  cred_rows | workers "python3 -I $CRED_ROOT/creds.py $*"
}

stage_creds() {
  install_creds
  {
    box "for r in ${OWNERS[*]}; do printf 'pw\t%s\t%s\n' \$r \"\$(cat /etc/loyal-db/replica/\$r.password)\"; done"
    cred_rows
  } | workers "python3 -I $CRED_ROOT/creds.py stage"
}

check_coverage() {
  local known=" ${ENGINES[*]} ${SERVICES[*]} " unit missing=0
  while read -r unit; do
    [[ $unit == loyal-web-cron@.service || $known == *" ${unit%.service} "* ]] && continue
    echo "coverage: $unit reads a Loyal database but is not in ENGINES or SERVICES in $0" >&2
    missing=1
  done < <(awk -F'\t' 'NR > 1 && $1 ~ /^loyal-workers/ && $2 != "unreferenced" && $6 ~ /^(app|yield|timescale)$/ { print $2 }' "$CUTOVER_PROOF/inventory.tsv" | sort -u)
  for unit in $(workers "systemctl list-units --type=service --state=active --no-legend --plain 'loyal-*'" | awk '{ print $1 }'); do
    unit=${unit/@*./@.}
    grep -q "	$unit	" "$CUTOVER_PROOF/inventory.tsv" && continue
    echo "coverage: running $unit is missing from the inventory" >&2
    missing=1
  done
  ((missing == 0)) || die "inventory and unit lists disagree"
  echo "coverage: every running loyal-* unit is in the inventory; every database reader is in ENGINES, SERVICES or the web cron template"
}

unit_history() {
  workers "for u in ${ENGINES[*]} ${SERVICES[*]}; do journalctl -u \$u.service --since -14days -o short-iso-precise --no-pager | grep -E 'systemd\[1\]: (Stopping|Stopped|Starting|Started) ' | awk -v u=\$u '{ print u, \$1, \$4 }'; done" |
    python3 -I -c '
import sys, datetime as d, collections
last, out = {}, collections.defaultdict(lambda: {"stop": [], "start": []})
for line in sys.stdin:
    unit, ts, act = line.split()[:3]
    t = d.datetime.fromisoformat(ts)
    if act in ("Stopping", "Starting"):
        last[(unit, act)] = t
    elif act in ("Stopped", "Started") and (unit, act.replace("ped", "ping").replace("ted", "ting")) in last:
        out[unit]["stop" if act == "Stopped" else "start"].append((t - last.pop((unit, act.replace("ped", "ping").replace("ted", "ting")))).total_seconds())
for unit, v in out.items():
    s, a = sorted(v["stop"]), sorted(v["start"])
    f = lambda xs: f"n={len(xs)} max={max(xs):.2f}s" if xs else "n=0"
    print(f"  {unit:16} stop {f(s):20} start {f(a)}")
    print(f"history {unit} {max(s, default=0):.2f} {max(a, default=0):.2f}", file=sys.stderr)' 2>"$CUTOVER_PROOF/.unit-history"
}

stop_writers() {
  workers "systemctl stop ${TIMERS[*]}"
  echo "stopped timers: ${TIMERS[*]}"
  workers "for i in \$(seq 300); do systemctl is-active ${CRONS[*]} | grep -qvxE 'inactive|failed' || exit 0; sleep 1; done; echo 'a web cron run is still running after 300s' >&2; exit 1"
  local u
  for u in "${ENGINES[@]}"; do
    workers "systemctl stop $u; [ \"\$(systemctl show -p Result --value $u)\" = success ] || { echo '$u did not stop cleanly' >&2; exit 1; }"
    echo "stopped $u cleanly"
  done
  workers "systemctl stop ${SERVICES[*]}"
  echo "stopped ${SERVICES[*]}"
  writers_stopped || die "a writer is still active"
}

writers_stopped() {
  local states
  states=$(workers "systemctl is-active ${ENGINES[*]} ${SERVICES[*]} ${CRONS[*]} ${TIMERS[*]} || true")
  [[ -n $states ]] && ! grep -qvxE 'inactive|failed' <<<"$states"
}

start_healthy() {
  local u path
  for u in "$@"; do
    path=$(health_path "$u")
    workers "systemctl start $u; for i in \$(seq 90); do systemctl is-active --quiet $u && curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:$path && { echo \"$u: active, $path answers after \${i}s\"; exit 0; }; sleep 1; done; echo '$u: not healthy after 90s' >&2; exit 1"
  done
}

health() {
  local u path bad=0
  for u in "$@"; do
    path=$(health_path "$u")
    if workers "systemctl is-active --quiet $u && curl -fsS -o /dev/null --max-time 5 http://127.0.0.1:$path"; then
      echo "health: $u active, $path 200, restarts $(workers "systemctl show -p NRestarts --value $u")"
    else
      echo "health: FAIL $u ($(workers "systemctl is-active $u" || true), $path)"
      bad=1
    fi
  done
  return $bad
}

silence_add() {
  workers "f=$CRED_ROOT/silence.id; am='amtool --alertmanager.url=http://127.0.0.1:9093'; if [ -s \$f ] && \$am silence query -q | grep -qxF \"\$(cat \$f)\"; then echo \"silence \$(cat \$f) already active\"; else \$am silence add --duration=168h --author=hz-db5-cutover --comment='$SILENCE_COMMENT' 'alertname=~\"LoyalDbReplica.*|LoyalDbNeon.*\"' >\$f && echo \"silence \$(cat \$f) added for 7 days on LoyalDbReplica.* and LoyalDbNeon.*\"; fi"
}

neon_api() {
  curl -fsS -H "Authorization: Bearer $NEON_KEY" -H 'Content-Type: application/json' "https://console.neon.tech/api/v2$1" "${@:2}"
}

neon_endpoints() {
  local pe
  for pe in "${NEON_ENDPOINTS[@]}"; do
    neon_api "/projects/${pe%/*}/endpoints/${pe#*/}" | python3 -I -c 'import json, sys; e = json.load(sys.stdin)["endpoint"]; print("neon: %s %s state=%s disabled=%s" % (e["project_id"], e["id"], e["current_state"], e.get("disabled")))'
  done
}

neon_writes_total() { awk '$2 == "neon" && $4 == "total" { t += $5 } END { print t + 0 }' <<<"$1"; }
hz_writes_total() { awk '$2 == "hz" && $4 == "total" { t += $5 } END { print t + 0 }' <<<"$1"; }

cmd_preflight() {
  local verify_out
  verify_out=$CUTOVER_PROOF/preflight-verify-$(date -u +%Y%m%dT%H%MZ).txt
  timed inventory cmd_inventory
  timed coverage check_coverage
  timed install install
  timed replica-check db check
  timed access db access
  timed grants db grants
  timed schema db schema
  timed pgbouncer db pgbouncer-test
  timed creds-stage stage_creds
  timed creds-test creds test-staged
  timed neon-sessions db sessions
  timed lag-wait db lag-wait 600
  timed catchup db catchup
  timed sequences db sequences
  timed writes-save db writes save preflight
  timed replica-verify db verify >"$verify_out"
  echo "replica verify: $(grep -c ' DIFF$' "$verify_out" || true) DIFF line(s) while Neon takes writes; full output $verify_out"
  NEON_KEY=$(op read 'op://Engineering/f4adqjr3dxnqofikunbwaajumy/credential')
  timed neon-api neon_endpoints
  timed unit-history unit_history
  timed ssh-roundtrip workers true
  timed writes-diff db writes diff preflight
  report_times
  estimate
}

estimate() {
  { printf 'step %s\n' "${TIMES[@]}"; cat "$CUTOVER_PROOF/.unit-history"; } | awk -v engines=" ${ENGINES[*]} " '
    $1 == "step" { s[$2] = $3 }
    $1 == "history" { if (index(engines, " " $2 " ")) { estop += $3; estart += $4 } else { if ($3 > sstop) sstop = $3; sstart += $4 } }
    END {
      r = s["ssh-roundtrip"]
      stop = estop + sstop + 6 * r
      freeze = stop + s["neon-sessions"] + s["lag-wait"] + s["catchup"] + s["sequences"] + s["writes-save"] + s["replica-verify"]
      sw = s["lag-wait"] + r + s["catchup"] + s["sequences"] + s["writes-save"] + s["creds-test"] + sstart + 6 * r
      vf = s["creds-test"] + s["writes-diff"] + 12 * r
      un = estart + 2 * r + 60
      printf "\n== freeze estimate (measured: preflight steps above, unit stop/start maxima from 14 days of journal, ssh round trip %.1fs)\n", r
      printf "  freeze   %6.0fs  stop units (engines %.2fs, services %.2fs) + Neon session check + lag-wait + catchup + sequences + write counters + replica verify\n", freeze, estop, sstop
      printf "  switch   %6.0fs  lag-wait + disable + catchup + sequences + write counters + credential install + service starts (%.2fs)\n", sw, sstart
      printf "  verify   %6.0fs  live credential reads + write counters + health\n", vf
      printf "  unfreeze %6.0fs  engine starts (%.2fs) + 60s restart watch\n", un, estart
      printf "  writes stop for about %.0fs (%.1f min) from freeze to unfreeze; loyal-web is down from freeze until switch starts it (about %.0fs)\n", freeze + sw + vf + un, (freeze + sw + vf + un) / 60, freeze + sw
    }'
}

cmd_freeze() {
  local verify_out
  verify_out=$CUTOVER_PROOF/freeze-verify-$(date -u +%Y%m%dT%H%MZ).txt
  creds status staged >/dev/null || die "credentials are not staged; run preflight"
  creds status neon >/dev/null || die "a credential already points at Hetzner; freeze runs before switch, rerun switch instead"
  timed install install
  db subscriptions enabled || die "a subscription is disabled, so the replica is not following Neon; rerun switch, or rebuild the replica"
  timed silence silence_add
  timed stop stop_writers
  timed neon-sessions db sessions
  timed lag-wait db lag-wait 600
  timed catchup db catchup
  timed sequences db sequences
  timed writes-save db writes save freeze neon
  timed replica-verify db verify >"$verify_out"
  grep ' DIFF$' "$verify_out" && die "replica differs from Neon at the freeze point ($verify_out); rerun freeze"
  echo "replica verify: no DIFF at the freeze point; $verify_out"
  report_times
}

cmd_switch() {
  if creds status hetzner >/dev/null; then
    echo "switch: credentials already on Hetzner; ensuring services run"
  else
    writers_stopped || die "writers are running; run freeze first"
    timed install install
    timed lag-wait db lag-wait 600
    timed disable db subscriptions disable
    timed catchup db catchup
    timed sequences db sequences
    timed writes-save db writes save switch hz
    timed creds-switch creds switch
  fi
  timed start start_healthy "${SERVICES[@]}"
  report_times
}

neon_writes_since_freeze() {
  local out
  if out=$(db writes diff freeze neon) && (($(neon_writes_total "$out") == 0)); then
    echo "$out"
    return 0
  fi
  echo "$out"
  echo "cutover: Neon took writes after the freeze snapshot, or its counters cannot be read; those rows may not be on Hetzner (tables above)"
  [[ ${ACCEPT_NEON_WRITES:-} == 1 ]] && { echo "cutover: ACCEPT_NEON_WRITES=1, a human has checked them; continuing"; return 0; }
  return 1
}

cmd_verify() {
  local out bad=0
  timed creds-live creds test-live || bad=1
  timed health health "${SERVICES[@]}" || bad=1
  if ! out=$(db writes diff switch hz) || (($(hz_writes_total "$out") == 0)); then
    echo "$out"
    echo "verify: FAIL no write has landed on Hetzner since switch, or its counters cannot be read"
    bad=1
  else
    echo "$out"
  fi
  neon_writes_since_freeze || { echo "verify: FAIL copy those rows to Hetzner or roll back; rerun with ACCEPT_NEON_WRITES=1 once a human has checked"; bad=1; }
  report_times
  ((bad == 0)) || die "verify failed"
  echo "verify: ok"
}

cmd_unfreeze() {
  creds status hetzner >/dev/null || die "credentials still point at Neon; run switch"
  cmd_verify
  workers "touch $CRED_ROOT/unfrozen && sync $CRED_ROOT"
  echo "unfreeze: $CRED_ROOT/unfrozen written; rollback now refuses without --force"
  timed start-timers workers "systemctl start ${TIMERS[*]}"
  timed start-engines start_healthy "${ENGINES[@]}"
  sleep 60
  timed health health "${ENGINES[@]}" "${SERVICES[@]}"
  report_times
}

cmd_rollback() {
  local out snap enable=1
  box "test ! -e $STATE/paused" || die "Neon is paused and its slots are dropped; rolling back means re-enabling the Neon endpoints and losing every write since switch"
  if [[ $(workers "test -e $CRED_ROOT/unfrozen && echo yes || echo no") == yes && ${1:-} != --force ]]; then
    die "unfreeze has run, so the engines have written to Hetzner. Rollback would discard every Hetzner write since switch and the replica could not resume. Fix forward. 'rollback --force' discards them and lists the tables first"
  fi
  timed stop stop_writers
  snap=$(box "test -s $STATE/writes-switch-hz-app && echo yes || echo no")
  if [[ $snap == yes ]]; then
    if out=$(db writes diff switch hz) && (($(hz_writes_total "$out") == 0)); then
      echo "$out"
    else
      echo "$out"
      enable=0
      echo "rollback: DISCARDING the Hetzner row changes above (or ones the counters cannot count). They stay on Hetzner only. Subscriptions stay disabled; rebuild the replica with remote.sh setup from empty databases."
    fi
  fi
  timed creds-rollback creds rollback
  ((enable == 0)) || timed enable db subscriptions enable
  workers "rm -f $CRED_ROOT/unfrozen"
  timed start start_healthy "${SERVICES[@]}"
  timed start-timers workers "systemctl start ${TIMERS[*]}"
  timed start-engines start_healthy "${ENGINES[@]}"
  report_times
}

cmd_pause_neon() {
  local pe
  creds status hetzner >/dev/null || die "credentials still point at Neon"
  neon_writes_since_freeze || die "copy those rows to Hetzner, or rerun with ACCEPT_NEON_WRITES=1 to drop them"
  timed install install
  timed drop db subscriptions drop
  NEON_KEY=$(op read 'op://Engineering/f4adqjr3dxnqofikunbwaajumy/credential')
  for pe in "${NEON_ENDPOINTS[@]}"; do
    neon_api "/projects/${pe%/*}/endpoints/${pe#*/}" -X PATCH -d '{"endpoint": {"disabled": true}}' >/dev/null
    if neon_api "/projects/${pe%/*}/endpoints/${pe#*/}" | grep -q '"current_state": *"active"'; then
      neon_api "/projects/${pe%/*}/endpoints/${pe#*/}/suspend" -X POST >/dev/null
    fi
  done
  timed neon-api neon_endpoints
  box "mkdir -p $STATE && touch $STATE/paused"
  report_times
}

case ${1:-} in
  inventory) cmd_inventory ;;
  preflight) cmd_preflight ;;
  freeze) cmd_freeze ;;
  switch) cmd_switch ;;
  verify) cmd_verify ;;
  unfreeze) cmd_unfreeze ;;
  rollback) cmd_rollback "${2:-}" ;;
  pause-neon) cmd_pause_neon ;;
  status) creds status ;;
  *) echo "usage: $0 inventory|preflight|freeze|switch|verify|unfreeze|rollback [--force]|pause-neon|status" >&2; exit 2 ;;
esac
