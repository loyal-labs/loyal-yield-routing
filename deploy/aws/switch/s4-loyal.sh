#!/usr/bin/env bash
# Switch S4: Loyal (loyal-workers and the database box loyal-db-primary) to loyal-main. Runs from the operator's Mac.
# The switch itself is `deploy/aws/db/cutover.sh loyal`; clusters/loyal.conf calls handover, handback and dbcheck.
set -uo pipefail

DIR=$(cd "$(dirname "$0")/.." && pwd)
DB=$DIR/db
OLD=${S4_OLD:-loyal-workers}
NEW=${S4_NEW:-loyal-main}
ART=/Users/user/loyal/artifacts
SRC_ADMIN=${PGMOVE_SRC_ADMIN:-$ART/orchestrate/aws-migration/proof/loyal-sub/bin/dbbox}
AGENT=${PGMOVE_AGENT:-/tmp/pgmove-agent.sock}
UNITS_TSV=${S4_UNITS_TSV:-$ART/aws-migration-2026-11-17/units.tsv}
TUNNEL_SEALED=/etc/credstore.encrypted/cloudflared/loyal-workers.json
TUNNEL_PLAIN=/etc/cloudflared/loyal-workers.json
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)

usage() {
  cat >&2 <<EOF
usage: $0 preflight|handover|handback|dbcheck
preflight  Read-only. Runs s4-services-preflight.sh, pgmove preflight loyal, cutover.sh loyal preflight and the S4 checks,
           one line per check as ok, BLOCKER or PARKED, then "<n> ok, <m> blocker, <k> parked". Exit 0 only with 0 blockers.
handover   CUT_HANDOVER, in freeze once the old units are proven stopped: the ClickStack final copy, the receivers' state
           copy, then the tunnel credential placed on $NEW. Fails closed; cutover.sh bounds it.
handback   CUT_HANDBACK, in abort and rollback once $NEW is stopped: removes the placed tunnel credential.
dbcheck    CUT_DBCHECK, before any app starts: each app database credential on $NEW logs in through 127.0.0.1:6432.
Env: PGMOVE_SRC_ADMIN (runs its one argument as root on loyal-db-primary), PGMOVE_AGENT, S4_UNITS_TSV.
EOF
  exit 2
}

# shellcheck disable=SC2029
old() { ssh "${SSH_OPTS[@]}" "$OLD" "sudo -n bash -c $(printf '%q' "$*")"; }
# shellcheck disable=SC2029
new() { ssh "${SSH_OPTS[@]}" "$NEW" "sudo -n bash -c $(printf '%q' "$*")"; }
newsh() { ssh "${SSH_OPTS[@]}" "$NEW" 'sudo -n bash -s'; }
src() { "$SRC_ADMIN" "$*"; }
conf() { bash -c 'source "$1"; eval "echo \"\${$2[*]}\""' _ "$DB/clusters/loyal.conf" "$1"; }
fail() { echo "s4-loyal: $*" >&2; exit 1; }

# stopped <where> <unit>...: every unit answers inactive or failed on old, new or src; a failed read fails.
stopped() {
  local where=$1 out n
  shift
  out=$("$where" "for u in $*; do echo \$u \$(systemctl show -p ActiveState --value \$u); done") || { echo "$where did not answer" >&2; return 1; }
  n=$(grep -cE '^[^ ]+ (inactive|failed)$' <<<"$out")
  ((n == $#)) || { echo "$where: not all stopped: $(grep -vE ' (inactive|failed)$' <<<"$out" | paste -sd' ' -)" >&2; return 1; }
}

handover() {
  local out
  stopped old loyal-clickstack loyal-clickstack-relay cloudflared || fail "$OLD: ClickStack or cloudflared runs; nothing copied"
  stopped src loyal-observer-private-receiver loyal-squads-watch-receiver loyal-squads-watch-tick.timer loyal-squads-watch-tick.service loyal-observer-push-check.timer ||
    fail "loyal-db-primary: a receiver runs; nothing copied"
  stopped new loyal-clickstack loyal-clickstack-relay loyal-observer-private-receiver loyal-squads-watch-receiver cloudflared ||
    fail "$NEW: a unit that takes the copies runs; nothing copied"
  bash "$DIR/loyal-clickstack-copy.sh" final || fail "ClickStack final copy failed"
  out=$(bash "$DIR/loyal-receivers-repin.sh" state) || fail "receiver state copy failed: $out"
  echo "$out"
  grep -qx 'final copy: the old receivers and tick timer are stopped' <<<"$out" || fail "the receiver state copy was not final"
  new "test -s $TUNNEL_SEALED && umask 077 && systemd-creds decrypt --name=loyal-workers.json $TUNNEL_SEALED $TUNNEL_PLAIN.new && chmod 0600 $TUNNEL_PLAIN.new && mv -f $TUNNEL_PLAIN.new $TUNNEL_PLAIN && sync /etc/cloudflared" ||
    fail "the tunnel credential was not placed on $NEW"
  echo "$NEW: tunnel credential placed at $TUNNEL_PLAIN"
}

handback() {
  stopped new cloudflared || fail "cloudflared runs on $NEW; the credential stays"
  new "rm -f $TUNNEL_PLAIN $TUNNEL_PLAIN.new && sync /etc/cloudflared && test ! -e $TUNNEL_PLAIN" || fail "the tunnel credential is still on $NEW"
  echo "$NEW: tunnel credential removed"
}

# The app database credentials of every unit on the box (the loyal_yield_prod ones go straight to Postgres on 5432, the
# rest through PgBouncer on 6432), as the units load them: LoadCredentialEncrypted ID:path, decrypted
# with --name=ID. Only postgres URLs at host 127.0.0.1 count. Prints "<unit>/<id> <user> <db> <port>" and never a password.
CREDS_PY='
import os, re, subprocess, sys, urllib.parse as up
for u in sys.argv[1].split():
    cat = subprocess.run(["systemctl", "cat", u + ".service"], capture_output=True, text=True).stdout
    for cid, path in re.findall(r"^LoadCredentialEncrypted=([^:\s]+):(\S+)$", cat, re.M):
        r = subprocess.run(["systemd-creds", "decrypt", "--name=" + cid, path, "-"], capture_output=True, text=True)
        if r.returncode:
            print(u + "/" + cid, "DECRYPT-FAILED - -")
            continue
        p = up.urlsplit(r.stdout.strip())
        if p.scheme not in ("postgres", "postgresql") or p.hostname != "127.0.0.1":
            continue
        user, db = up.unquote(p.username or "-"), p.path.lstrip("/") or "-"
        print(u + "/" + cid, user, db, p.port)
        if os.environ.get("CRED_EXEC"):
            mode = up.parse_qs(p.query).get("sslmode", ["require"])[0]
            env = dict(os.environ, PGPASSWORD=up.unquote(p.password or ""), PGSSLMODE=mode, PGCONNECT_TIMEOUT="10")
            c = subprocess.run(["psql", "-XAtq", "-h", "127.0.0.1", "-p", str(p.port), "-U", user, "-d", db, "-c", "select current_user"],
                               env=env, capture_output=True, text=True)
            err = (c.stderr.strip().splitlines() or ["no error text"])[-1]
            print("  login ok" if c.returncode == 0 and c.stdout.strip() == user else "  login FAILED: " + err)
'
APPS="loyal-web loyal-admin loyal-stats loyal-telegram loyal-observer loyal-realtime loyal-retail loyal-backyard loyal-earn-max loyal-squads-watch-receiver loyal-observer-private-receiver"

dbcheck() {
  local out n bad via
  out=$(new "systemctl is-active loyal-pgbouncer >/dev/null && ss -Hltn 'sport = :6432' | grep -q . && cd /tmp && CRED_EXEC=1 python3 -I -c $(printf '%q' "$CREDS_PY") $(printf '%q' "$APPS")") ||
    fail "$NEW: PgBouncer is not up on 6432, or the credentials did not read"
  echo "$out"
  n=$(grep -c '^  login ok$' <<<"$out")
  bad=$(grep -cE 'FAILED' <<<"$out")
  via=$(grep -cE '^[^ ]+ [^ ]+ [^ ]+ 6432$' <<<"$out")
  ((n > 0 && via > 0 && bad == 0)) || fail "$n login(s) ok ($via through 6432), $bad failed"
  grep -E '^[^ ]+ ' <<<"$out" | awk '$4 != 6432 && $4 != 5432 { bad = 1 } END { exit bad }' || fail "a credential reaches 127.0.0.1 on neither 6432 nor 5432"
  echo "dbcheck: $n app credential(s) log in, $via of them through PgBouncer on 127.0.0.1:6432"
}

OK=0 BLOCK=0 PARK=0
# check <name> <fn>: fn prints its detail; exit 0 is ok, 3 is PARKED, anything else is a BLOCKER.
check() {
  local name=$1 detail rc
  detail=$("$2" 2>&1)
  rc=$?
  detail=$(tr '\n' ' ' <<<"${detail:-no output}" | sed 's/  */ /g; s/ *$//')
  case $rc in
    0) OK=$((OK + 1)); echo "ok $name: $detail" ;;
    3) PARK=$((PARK + 1)); echo "PARKED $name: $detail" ;;
    *) BLOCK=$((BLOCK + 1)); echo "BLOCKER $name: $detail" ;;
  esac
}

c_services() {
  local out rc
  out=$(bash "$DIR/s4-services-preflight.sh" 2>&1)
  rc=$?
  grep -E '^FAIL ' <<<"$out"
  tail -1 <<<"$out"
  return "$rc"
}

c_pgmove() {
  local out rc
  out=$(PGMOVE_SRC_ADMIN=$SRC_ADMIN PGMOVE_AGENT=$AGENT bash "$DB/remote.sh" run preflight loyal 2>&1)
  rc=$?
  grep -E 'DIFF' <<<"$out" | grep -vE '(^| )0 DIFF$' | head -5
  grep -E ' DIFF$' <<<"$out" | tail -1
  return "$rc"
}

c_cutover() {
  local out rc
  out=$(PGMOVE_SRC_ADMIN=$SRC_ADMIN PGMOVE_AGENT=$AGENT bash "$DB/cutover.sh" loyal preflight 2>&1)
  rc=$?
  grep -E '^(preflight:|cutover:)|total' <<<"$out"
  return "$rc"
}

# Every loyal-main row of units.tsv is in clusters/loyal.conf (the database itself moves by pgmove), and every unit of
# the conf is loaded, disabled (oneshots static) and inactive on loyal-main.
c_units() {
  local want listed u miss='' states bad
  want=$(awk -F'\t' '$3 == "loyal-main" && $4 == "move" { print $1 }' "$UNITS_TSV" | sed 's/\.service$//') || return 1
  [[ $(wc -l <<<"$want") -eq 28 ]] || { echo "units.tsv lists $(wc -l <<<"$want") loyal-main rows, not 28"; return 1; }
  listed=" $(conf CUT_TIMERS) $(conf CUT_ENGINES) $(conf CUT_SERVICES) $(conf CUT_FIRST) "
  for u in $want; do
    [[ $u == postgresql@18-main || $listed == *" $u "* ]] || miss+=" $u"
  done
  [[ -z $miss ]] || { echo "in units.tsv, not in loyal.conf:$miss"; return 1; }
  states=$(new "for u in $listed $(conf CUT_ONESHOTS); do echo \$u \$(systemctl show -p LoadState --value \$u) \$(systemctl show -p ActiveState --value \$u) \$(systemctl show -p UnitFileState --value \$u); done") || { echo "$NEW did not answer"; return 1; }
  bad=$(grep -vE '^[^ ]+ loaded inactive (disabled|static)$' <<<"$states")
  [[ -z $bad ]] || { echo "on $NEW: $bad"; return 1; }
  echo "27 of 28 rows in loyal.conf (postgresql@18-main moves by pgmove), plus loyal-squads-watch-firewall; $(wc -l <<<"$states") units loaded, inactive and disabled or static on $NEW"
}

# The sides freeze records: abort and rollback restore exactly these.
c_old_sides() {
  local w d oldu srcu
  oldu=$(bash -c 'source "$1"; source /dev/stdin' _ "$DB/clusters/loyal.conf" <<<'for u in ${CUT_TIMERS[*]} ${CUT_ENGINES[*]} ${CUT_SERVICES[*]}; do [[ " ${CUT_SRCSTOP[*]} " == *" $u "* ]] || echo $u; done' | paste -sd' ' -)
  srcu="$(conf CUT_SRCSTOP) loyal-pgbouncer loyal-squads-watch-firewall postgresql@18-main"
  w=$(old "for u in $oldu; do echo \$u \$(systemctl is-enabled \$u 2>/dev/null) \$(systemctl is-active \$u 2>/dev/null); done") || { echo "$OLD did not answer"; return 1; }
  d=$(src "for u in $srcu; do echo \$u \$(systemctl is-enabled \$u 2>/dev/null) \$(systemctl is-active \$u 2>/dev/null); done") || { echo "loyal-db-primary did not answer"; return 1; }
  echo "$OLD: $(grep -c ' enabled active$' <<<"$w") enabled and running; not both: $(grep -v ' enabled active$' <<<"$w" | paste -sd',' -)."
  echo "loyal-db-primary: $(grep -cE ' (enabled|static) active$' <<<"$d") running; others: $(grep -vE ' (enabled|static) active$' <<<"$d" | paste -sd',' -)."
  [[ $(grep -c '' <<<"$w") -eq $(wc -w <<<"$oldu") && $(grep -c '' <<<"$d") -eq $(wc -w <<<"$srcu") ]]
}

# PgBouncer on loyal-main, read-only: its config fronts the local Postgres on 127.0.0.1, the Postgres certificate verifies
# for 127.0.0.1, every app credential at 127.0.0.1 targets 6432 with a user in the auth file whose SCRAM secret equals the
# role's, and the password in the credential matches that secret. Nothing logs in; the roles stay NOLOGIN until switch.
c_pgbouncer() {
  newsh <<REMOTE
set -u
ini=/etc/pgbouncer/loyal.ini users=/etc/pgbouncer/loyal-users.txt
grep -qxE 'listen_addr = 127.0.0.1' \$ini && grep -qxE 'listen_port = 6432' \$ini || { echo "\$ini does not listen on 127.0.0.1:6432"; exit 1; }
dbs=\$(awk '/^\[databases\]/ { s = 1; next } /^\[/ { s = 0 } s && /=/' \$ini)
[ -n "\$dbs" ] && ! grep -vE 'host=127\.0\.0\.1 port=5432' <<<"\$dbs" | grep -q . || { echo "a [databases] entry is not host=127.0.0.1 port=5432"; exit 1; }
openssl verify -CAfile /etc/loyal-db/tls/ca.crt -verify_ip 127.0.0.1 /etc/loyal-db/tls/server.crt >/dev/null 2>&1 || { echo "server.crt does not verify for 127.0.0.1"; exit 1; }
st=\$(systemctl is-enabled loyal-pgbouncer 2>&1) at=\$(systemctl is-active loyal-pgbouncer 2>&1)
ss -Hltn 'sport = :6432' | grep -q . && { echo "something listens on 6432 before switch"; exit 1; }
cd /tmp
secrets=\$(sudo -n -u postgres psql -XAtq -F ' ' -c "select rolname, rolcanlogin, rolpassword from pg_authid where rolpassword like 'SCRAM%'") || { echo "pg_authid did not read"; exit 1; }
python3 -I -c $(printf '%q' "$CREDS_PY") $(printf '%q' "$APPS") >/tmp/.s4-creds.\$\$ || exit 1
export SECRETS="\$secrets"
python3 -I - "\$users" /tmp/.s4-creds.\$\$ <<'PY'
import os, re, sys
auth = dict(re.findall(r'^"([^"]+)"\s+"([^"]+)"', open(sys.argv[1]).read(), re.M))
roles = {l.split(" ")[0]: (l.split(" ")[1], l.split(" ", 2)[2]) for l in os.environ["SECRETS"].splitlines() if l.strip()}
rows = [l.split() for l in open(sys.argv[2]) if l.strip()]
os.unlink(sys.argv[2])
bad = 0
for ref, user, db, port in rows:
    why = []
    if port not in ("6432", "5432"): why.append("port " + port)
    elif port == "5432": pass
    elif user not in auth: why.append("not in auth_file")
    elif user not in roles: why.append("no SCRAM role")
    elif auth[user] != roles[user][1]: why.append("auth_file secret differs from the role")
    if why: bad += 1
    print(f"{ref} {user}@{db}:{port}: {'ok' if not why else ', '.join(why)}")
via = sum(r[3] == "6432" for r in rows)
print(f"{len(rows)} credential(s), {via} through PgBouncer, {len(rows) - via} direct to Postgres 5432, {bad} bad")
sys.exit(1 if bad or not via else 0)
PY
rc=\$?
echo "loyal-pgbouncer \$st \$at; nothing on 6432"
[ \$rc = 0 ] && [ "\$st \$at" = "disabled inactive" ]
REMOTE
}

# The password inside each credential against the role's SCRAM secret, computed here (no login).
c_passwords() {
  newsh <<REMOTE
set -u
cd /tmp
secrets=\$(sudo -n -u postgres psql -XAtq -F ' ' -c "select rolname, rolpassword from pg_authid where rolpassword like 'SCRAM%'") || { echo "pg_authid did not read"; exit 1; }
export SECRETS="\$secrets" APPS=$(printf '%q' "$APPS")
python3 -I - <<'PY'
import base64, hashlib, hmac, os, re, subprocess, urllib.parse
roles = dict(l.split(" ", 1) for l in os.environ["SECRETS"].splitlines() if l.strip())
n = bad = 0
for u in os.environ["APPS"].split():
    cat = subprocess.run(["systemctl", "cat", u + ".service"], capture_output=True, text=True).stdout
    for cid, path in re.findall(r"^LoadCredentialEncrypted=([^:\s]+):(\S+)$", cat, re.M):
        r = subprocess.run(["systemd-creds", "decrypt", "--name=" + cid, path, "-"], capture_output=True, text=True)
        v = r.stdout.strip()
        if r.returncode or not re.match(r"postgres(ql)?://", v): continue
        p = urllib.parse.urlsplit(v)
        if p.hostname != "127.0.0.1": continue
        n += 1
        user, pw = urllib.parse.unquote(p.username or ""), urllib.parse.unquote(p.password or "")
        m = re.match(r"SCRAM-SHA-256\\\$(\d+):([^\$]+)\\\$([^:]+):", roles.get(user, ""))
        ok = False
        if m:
            salted = hashlib.pbkdf2_hmac("sha256", pw.encode(), base64.b64decode(m.group(2)), int(m.group(1)))
            stored = hashlib.sha256(hmac.new(salted, b"Client Key", "sha256").digest()).digest()
            ok = hmac.compare_digest(stored, base64.b64decode(m.group(3)))
        bad += not ok
        print(f"{u}/{cid} {user}: password {'matches' if ok else 'DOES NOT MATCH'} the role")
print(f"{n} credential(s), {bad} mismatch")
raise SystemExit(1 if bad or not n else 0)
PY
REMOTE
}

c_tunnel() {
  local out
  out=$(new "f=\$(awk '\$1 == \"credentials-file:\" { print \$2 }' /etc/cloudflared/config.yml); t=\$(awk '\$1 == \"tunnel:\" { print \$2 }' /etc/cloudflared/config.yml); echo config \$t \$f; test -s $TUNNEL_SEALED && echo sealed yes; test -e $TUNNEL_PLAIN && echo plain yes; systemd-creds decrypt --name=loyal-workers.json $TUNNEL_SEALED - | python3 -I -c 'import json,sys; print(\"decrypts\", json.load(sys.stdin)[\"TunnelID\"])'; echo unit \$(systemctl is-enabled cloudflared 2>&1) \$(systemctl is-active cloudflared 2>&1)") || { echo "$NEW: $out"; return 1; }
  local oldt
  oldt=$(old "awk '\$1 == \"tunnel:\" { print \$2 }' /etc/cloudflared/config.yml; systemctl is-active cloudflared") || { echo "$OLD did not answer"; return 1; }
  echo "$out" "| $OLD:" "$oldt"
  local t
  t=$(awk '$1 == "config" { print $2 }' <<<"$out")
  [[ -n $t ]] && grep -qx "config $t $TUNNEL_PLAIN" <<<"$out" && grep -qx 'sealed yes' <<<"$out" && ! grep -qx 'plain yes' <<<"$out" &&
    grep -qx "decrypts $t" <<<"$out" && grep -qx 'unit disabled inactive' <<<"$out" && [[ $(head -1 <<<"$oldt") == "$t" ]]
}

# The public routes that do not go through the tunnel: Caddy on loyal-workers serves realtime.askloyal.com (proxy to
# 127.0.0.1:10000) by DNS A and ACME HTTP. After switch loyal-realtime runs on loyal-main, which takes only tcp 22.
c_public() {
  local dns ing
  dns=$(dig +short realtime.askloyal.com A | paste -sd' ' -)
  ing=$(grep -c '^INGRESS' "$DIR/boxes/loyal-main.conf")
  echo "realtime.askloyal.com A ${dns:-none}; loyal-main.conf INGRESS lines: $ing; loyal-main caddy $(new 'systemctl is-enabled caddy 2>&1') (not in units.tsv)"
  [[ $ing -gt 0 && $dns == *100.57.84.125* ]]
}

# The production deploy reaches the box through WireGuard loyal-ci on udp 51820 (scripts/ci-worker-network.sh).
c_deploy() {
  local w
  w=$(new "test -e /etc/wireguard/loyal-ci.conf && echo conf; systemctl is-enabled wg-quick@loyal-ci 2>&1") || true
  echo "loyal-main wireguard: ${w//$'\n'/ }; LOYAL_WORKERS_HOST stays loyal-workers until the owner repoints it after switch"
  grep -qx conf <<<"$w"
}

preflight() {
  check services c_services
  check pgmove-preflight c_pgmove
  check cutover-preflight c_cutover
  check units-28 c_units
  check old-sides c_old_sides
  check pgbouncer-path c_pgbouncer
  check pgbouncer-passwords c_passwords
  check tunnel c_tunnel
  check public-route c_public
  check deploy-route c_deploy
  echo "$OK ok, $BLOCK blocker, $PARK parked"
  ((BLOCK == 0))
}

case ${1:-} in
  preflight) preflight ;;
  handover) handover ;;
  handback) handback ;;
  dbcheck) dbcheck ;;
  *) usage ;;
esac
