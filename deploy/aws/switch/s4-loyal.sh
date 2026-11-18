#!/usr/bin/env bash
# Switch S4: Loyal (loyal-workers and the database box loyal-db-primary) to loyal-main. Runs from the operator's Mac.
# The switch itself is `deploy/aws/db/cutover.sh loyal`; clusters/loyal.conf calls handover, handback and dbcheck.
set -uo pipefail

DIR=$(cd "$(dirname "$0")/.." && pwd)
DB=$DIR/db
# S4_OLD and S4_NEW point the steps at stub hosts; the handback deletes on NEW, so they count only with S4_TEST set.
if [[ -n ${S4_OLD:-}${S4_NEW:-} && -z ${S4_TEST:-} ]]; then echo "s4-loyal: S4_OLD and S4_NEW are for stubs; set S4_TEST=1 to use them" >&2; exit 2; fi
OLD=${S4_OLD:-loyal-workers}
NEW=${S4_NEW:-loyal-main}
ART=/Users/user/loyal/artifacts
SRC_ADMIN=${PGMOVE_SRC_ADMIN:-$ART/orchestrate/aws-migration/proof/loyal-sub/bin/dbbox}
AGENT=${PGMOVE_AGENT:-/tmp/pgmove-agent.sock}
UNITS_TSV=${S4_UNITS_TSV:-$ART/aws-migration-2026-11-17/units.tsv}
TUNNEL_SEALED=/etc/credstore.encrypted/cloudflared/loyal-workers.json
TUNNEL_PLAIN=/etc/cloudflared/loyal-workers.json
CADDY_DATA=/var/lib/caddy/.local/share/caddy
CERT_DIR=$CADDY_DATA/certificates/acme-v02.api.letsencrypt.org-directory/realtime.askloyal.com
OLD_IP=5.161.121.229
NEW_IP=100.57.84.125
APPLY="BOX_PUBKEY=~/.ssh/id_ed25519.pub deploy/aws/box.sh loyal-main"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)

usage() {
  cat >&2 <<EOF
usage: $0 preflight|handover|handback|dbcheck
preflight  Read-only. Runs s4-services-preflight.sh, pgmove preflight loyal, cutover.sh loyal preflight and the S4 checks,
           one line per check as ok, BLOCKER or PARKED, then "<n> ok, <m> blocker, <k> parked". Exit 0 only with 0 blockers.
handover   CUT_HANDOVER, in freeze once the old units are proven stopped: the ClickStack final copy, the receivers' state
           copy, Caddy's realtime.askloyal.com certificate, the loyal-ci-production root key, then the tunnel credential placed on $NEW. Fails closed; cutover.sh bounds it.
handback   CUT_HANDBACK, in abort and rollback once $NEW is stopped: removes the placed tunnel credential, the copied certificate and the
           loyal-ci-production key from root's authorized_keys.
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
b64() { base64 | tr -d '\n'; }
j64() { python3 -I -c 'import base64, json, sys; print(base64.b64encode(json.dumps(json.load(sys.stdin)).encode()).decode())'; }
# bcopy <send|recv> <params json>: the box-copy.py command to run as root on a host, its code carried in the command.
# shellcheck disable=SC2016 # expanded on the remote host
bcopy() { printf 'python3 -I -c "$(echo %s | base64 -d)" %s %s' "$(b64 <"$DIR/box-copy.py")" "$1" "$(j64 <<<"$2")"; }
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

# caddy_cert_copy: the realtime.askloyal.com certificate, its key and its metadata, from the stopped Caddy on $OLD into the same
# path on $NEW (owner caddy, 0600; the five directories above it are made caddy's too), through one pipe: nothing lands on this
# machine. The ACME account and the sslip name's certificate stay: $NEW's Caddy makes its own for the sslip name on start.
caddy_cert_copy() {
  local d spec='{"entries":[' out rc
  for d in /var/lib/caddy/.local /var/lib/caddy/.local/share $CADDY_DATA $CADDY_DATA/certificates "$(dirname "$CERT_DIR")"; do
    spec+="{\"k\":\"dir\",\"dst\":\"$d\",\"mode\":\"0700\",\"own\":\"caddy:caddy\"},"
  done
  spec+="{\"src\":\"$CERT_DIR\"}]}"
  # shellcheck disable=SC2029 # the commands are built here on purpose
  out=$(set -o pipefail; ssh "${SSH_OPTS[@]}" "$OLD" "sudo -n $(bcopy send "$spec")" | ssh "${SSH_OPTS[@]}" "$NEW" "sudo -n $(bcopy recv '{}')")
  rc=$?
  echo "caddy: ${out//$'\n'/$'\ncaddy: '}"
  ((rc == 0)) || return 1
  new "d=$CERT_DIR; for f in crt key json; do test -s \$d/realtime.askloyal.com.\$f || exit 1; done
    [ \"\$(stat -c %U:%a \$d/realtime.askloyal.com.key)\" = caddy:600 ] &&
    [ \"\$(openssl x509 -in \$d/realtime.askloyal.com.crt -noout -pubkey)\" = \"\$(openssl pkey -in \$d/realtime.askloyal.com.key -pubout)\" ] &&
    runuser -u caddy -- test -r \$d/realtime.askloyal.com.key" || return 1
  local so sn
  so=$(old "sha256sum <$CERT_DIR/realtime.askloyal.com.crt") || return 1
  sn=$(new "sha256sum <$CERT_DIR/realtime.askloyal.com.crt") || return 1
  [[ -n $so && $so == "$sn" ]]
}

# The production deploy's root login (scripts/ci-worker-network.sh brings the tunnel, then ssh root@10.203.0.1): loyal-workers
# trusts the loyal-ci-production key as one restrict line. Root login on loyal-main is granted at the switch, never before: the
# handover appends that one public line and the handback removes it, so abort and rollback leave loyal-main without it.
CI_KEY_RE=' loyal-ci-production-[0-9]+$'
ci_key_line() {
  local line
  line=$(old "grep -E '$CI_KEY_RE' /root/.ssh/authorized_keys") || return 1
  [[ $(grep -c '' <<<"$line") -eq 1 && $line =~ ^restrict\ ssh-ed25519\ [A-Za-z0-9+/]+\ loyal-ci-production-[0-9]+$ ]] || return 1
  printf '%s\n' "$line"
}
ci_key_count() { new "grep -cE '$CI_KEY_RE' /root/.ssh/authorized_keys || true"; }
# Idempotent: one line already there that is the one on $OLD counts as installed; none is appended; any other state fails.
ci_key_install() {
  local line n have
  line=$(ci_key_line) || return 1
  n=$(ci_key_count) || return 1
  if [[ $n == 1 ]]; then
    have=$(new "grep -E '$CI_KEY_RE' /root/.ssh/authorized_keys") || return 1
    [[ $have == "$line" ]]
    return
  fi
  [[ $n == 0 ]] || return 1
  printf '%s\n' "$line" | new "f=/root/.ssh/authorized_keys; [ -z \"\$(tail -c1 \$f)\" ] || echo >>\$f; cat >>\$f" || return 1
  [[ $(ci_key_count) == 1 ]]
}
ci_key_remove() {
  new "f=/root/.ssh/authorized_keys; grep -vE '$CI_KEY_RE' \$f >\$f.new; chmod --reference=\$f \$f.new && chown --reference=\$f \$f.new && mv \$f.new \$f" || return 1
  [[ $(ci_key_count) == 0 ]]
}

handover() {
  local out
  stopped old loyal-clickstack loyal-clickstack-relay cloudflared caddy wg-quick@loyal-ci || fail "$OLD: ClickStack, cloudflared, Caddy or the loyal-ci tunnel runs; nothing copied"
  stopped src loyal-observer-private-receiver loyal-squads-watch-receiver loyal-squads-watch-tick.timer loyal-squads-watch-tick.service loyal-observer-push-check.timer ||
    fail "loyal-db-primary: a receiver runs; nothing copied"
  stopped new loyal-clickstack loyal-clickstack-relay loyal-observer-private-receiver loyal-squads-watch-receiver cloudflared caddy wg-quick@loyal-ci ||
    fail "$NEW: a unit that takes the copies runs; nothing copied"
  bash "$DIR/loyal-clickstack-copy.sh" final || fail "ClickStack final copy failed"
  out=$(bash "$DIR/loyal-receivers-repin.sh" state) || fail "receiver state copy failed: $out"
  echo "$out"
  grep -qx 'final copy: the old receivers and tick timer are stopped' <<<"$out" || fail "the receiver state copy was not final"
  caddy_cert_copy || fail "the realtime certificate was not copied to $NEW"
  echo "$NEW: realtime.askloyal.com certificate placed at $CERT_DIR"
  ci_key_install || fail "the loyal-ci-production key was not installed for root on $NEW"
  echo "$NEW: loyal-ci-production key installed for root"
  new "test -s $TUNNEL_SEALED && umask 077 && systemd-creds decrypt --name=loyal-workers.json $TUNNEL_SEALED $TUNNEL_PLAIN.new && chmod 0600 $TUNNEL_PLAIN.new && mv -f $TUNNEL_PLAIN.new $TUNNEL_PLAIN && sync /etc/cloudflared" ||
    fail "the tunnel credential was not placed on $NEW"
  echo "$NEW: tunnel credential placed at $TUNNEL_PLAIN"
}

handback() {
  stopped new cloudflared caddy wg-quick@loyal-ci || fail "cloudflared, Caddy or the loyal-ci tunnel runs on $NEW; the credential and the certificate stay"
  new "rm -f $TUNNEL_PLAIN $TUNNEL_PLAIN.new && sync /etc/cloudflared && test ! -e $TUNNEL_PLAIN" || fail "the tunnel credential is still on $NEW"
  echo "$NEW: tunnel credential removed"
  new "rm -rf $CERT_DIR && sync $CADDY_DATA && test ! -e $CERT_DIR" || fail "the realtime certificate is still on $NEW"
  echo "$NEW: realtime.askloyal.com certificate removed"
  ci_key_remove || fail "the loyal-ci-production key is still in root's authorized_keys on $NEW"
  echo "$NEW: loyal-ci-production key removed"
}

# The app database credentials of every unit on the box (the loyal_yield_prod ones go straight to Postgres on 5432, the
# rest through PgBouncer on 6432), as the units load them: LoadCredentialEncrypted ID:path, decrypted
# with --name=ID; a JSON envelope (loyal-realtime) is read for the postgres URLs among its values. Only postgres URLs at host 127.0.0.1 count. Prints "<unit>/<id> <user> <db> <port>" and never a password.
CREDS_PY='
import json, os, re, subprocess, sys, urllib.parse as up
for u in sys.argv[1].split():
    cat = subprocess.run(["systemctl", "cat", u + ".service"], capture_output=True, text=True).stdout
    for cid, path in re.findall(r"^LoadCredentialEncrypted=([^:\s]+):(\S+)$", cat, re.M):
        r = subprocess.run(["systemd-creds", "decrypt", "--name=" + cid, path, "-"], capture_output=True, text=True)
        if r.returncode:
            print(u + "/" + cid, "DECRYPT-FAILED - -")
            continue
        raw, cands = r.stdout.strip(), None
        if raw.startswith("{"):
            try:
                cands = [(cid + ":" + k, v) for k, v in json.loads(raw)["values"].items() if isinstance(v, str)]
            except (ValueError, KeyError, AttributeError):
                print(u + "/" + cid, "ENVELOPE-FAILED - -")
                continue
        for name, url in cands or [(cid, raw)]:
            p = up.urlsplit(url)
            if p.scheme not in ("postgres", "postgresql") or p.hostname != "127.0.0.1":
                continue
            user, db = up.unquote(p.username or "-"), p.path.lstrip("/") or "-"
            print(u + "/" + name, user, db, p.port)
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
  grep -q '^loyal-realtime/' <<<"$out" || fail "loyal-realtime: no database login was tested; its realtime-envelope holds no postgres URL at 127.0.0.1"
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
  [[ $(wc -l <<<"$want") -eq 29 ]] || { echo "units.tsv lists $(wc -l <<<"$want") loyal-main rows, not 29"; return 1; }
  listed=" $(conf CUT_TIMERS) $(conf CUT_ENGINES) $(conf CUT_SERVICES) $(conf CUT_FIRST) "
  for u in $want; do
    [[ $u == postgresql@18-main || $listed == *" $u "* ]] || miss+=" $u"
  done
  [[ -z $miss ]] || { echo "in units.tsv, not in loyal.conf:$miss"; return 1; }
  states=$(new "for u in $listed $(conf CUT_ONESHOTS); do echo \$u \$(systemctl show -p LoadState --value \$u) \$(systemctl show -p ActiveState --value \$u) \$(systemctl show -p UnitFileState --value \$u); done") || { echo "$NEW did not answer"; return 1; }
  bad=$(grep -vE '^[^ ]+ loaded inactive (disabled|static)$' <<<"$states")
  [[ -z $bad ]] || { echo "on $NEW: $bad"; return 1; }
  echo "28 of 29 rows in loyal.conf (postgresql@18-main moves by pgmove), plus loyal-squads-watch-firewall and wg-quick@loyal-ci; $(wc -l <<<"$states") units loaded, inactive and disabled or static on $NEW"
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

# shellcheck disable=SC2016 # expanded on loyal-main
new_ip() { new 't=$(curl -fsS -m 5 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 60" http://169.254.169.254/latest/api/token) && curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/public-ipv4'; }

# The public routes that do not go through the tunnel. Caddy on loyal-workers serves realtime.askloyal.com (proxy to
# 127.0.0.1:10000, ACME HTTP-01 only) and the ClickStack sslip name by DNS A. The unit is the package's on both boxes; the
# Caddyfile differs only in the sslip name's address; nothing runs or listens on loyal-main.
c_caddy_staged() {
  local a b ip st
  local sum="systemctl cat caddy | sha256sum | cut -c1-16; caddy version | cut -d' ' -f1; sed -E 's/observability[.][0-9.]+[.]sslip[.]io/observability.X.sslip.io/' /etc/caddy/Caddyfile | sha256sum | cut -c1-16"
  a=$(old "$sum") || { echo "$OLD did not answer"; return 1; }
  b=$(new "$sum") || { echo "$NEW did not answer"; return 1; }
  [[ $a == "$b" ]] || { echo "unit, version or Caddyfile differ: $OLD $(paste -sd' ' - <<<"$a"), $NEW $(paste -sd' ' - <<<"$b")"; return 1; }
  ip=$(new_ip) || { echo "no public address on $NEW"; return 1; }
  [[ $ip == "$NEW_IP" ]] || { echo "$NEW's public address is $ip, not $NEW_IP"; return 1; }
  st=$(new "caddy adapt --config /etc/caddy/Caddyfile >/dev/null 2>&1 && echo adapt-ok; grep -c 'observability.$ip.sslip.io {' /etc/caddy/Caddyfile; grep -c '^realtime.askloyal.com {' /etc/caddy/Caddyfile; echo \$(systemctl is-enabled caddy 2>&1) \$(systemctl is-active caddy 2>&1); ss -Hltn 'sport = :80 or sport = :443 or sport = :2019' | wc -l") || { echo "$NEW did not answer"; return 1; }
  st=$(paste -sd' ' - <<<"$st" | sed 's/  */ /g')
  echo "unit, caddy $(awk '{print $1}' <<<"$b" | sed -n 2p) and Caddyfile (sslip name aside) equal on both; $NEW: $st (want adapt-ok 1 1 disabled inactive 0: Caddyfile parses, names observability.$ip.sslip.io once and realtime.askloyal.com once, disabled, inactive, nothing on 80, 443 or 2019)"
  [[ $st == "adapt-ok 1 1 disabled inactive 0" ]]
}

# TLS: the certificate for realtime.askloyal.com is copied in freeze with Caddy stopped on both boxes (caddy_cert_copy), so the
# first request after the DNS change is served with a valid certificate. This reads what would be copied.
c_caddy_tls() {
  local o
  o=$(old "d=$CERT_DIR; test -s \$d/realtime.askloyal.com.crt && test -s \$d/realtime.askloyal.com.key && openssl x509 -checkend 1209600 -noout -in \$d/realtime.askloyal.com.crt >/dev/null && echo valid-over-14-days; [ \"\$(openssl x509 -in \$d/realtime.askloyal.com.crt -noout -pubkey)\" = \"\$(openssl pkey -in \$d/realtime.askloyal.com.key -pubout)\" ] && echo key-matches; openssl x509 -noout -enddate -in \$d/realtime.askloyal.com.crt; stat -c '%U:%a' \$d/realtime.askloyal.com.key") || { echo "$OLD: no usable realtime certificate under $CERT_DIR"; return 1; }
  o=$(paste -sd' ' - <<<"$o")
  echo "$OLD $o; copied to $NEW in freeze, removed again by abort and rollback; $NEW: $(new "getent passwd caddy | cut -d: -f1; stat -c '%U:%a' /var/lib/caddy; test -e $CERT_DIR && echo 'certificate already present' || echo 'no certificate yet'" | paste -sd' ' -)"
  [[ $o == *valid-over-14-days*key-matches*notAfter=*caddy:600 ]] && new "getent passwd caddy >/dev/null && test -d /var/lib/caddy" >/dev/null 2>&1
}

c_ingress_conf() {
  local r w miss=''
  r=$(bash -c 'source "$1"; echo "${INGRESS[*]-}"' _ "$DIR/boxes/loyal-main.conf")
  for w in udp:51820:0.0.0.0/0 tcp:80:0.0.0.0/0 tcp:443:0.0.0.0/0; do [[ " $r " == *" $w "* ]] || miss+=" $w"; done
  echo "loyal-main.conf INGRESS: ${r:-none}${miss:+; missing$miss}"
  [[ -z $miss ]]
}

# The security group cannot be read without an AWS session (expired). A TCP connect to the Elastic IP tells tcp 80 and 443 apart:
# refused or open means the group lets the port in (nothing listens on loyal-main yet), a timeout means it does not. UDP gives no
# answer either way; box.sh adds the rules in conf order, udp 51820 first, so tcp 443 reaching the box means all three are in.
c_ingress_applied() {
  local p r out='' miss=0
  for p in 80 443; do
    r=$(python3 -I -c 'import socket, sys
s = socket.socket(); s.settimeout(6)
try:
    s.connect((sys.argv[1], int(sys.argv[2]))); print("open")
except ConnectionRefusedError:
    print("refused")
except OSError:
    print("timeout")' "$NEW_IP" "$p")
    out+=" tcp $p $r;"
    [[ $r == refused || $r == open ]] || miss=1
  done
  if ((miss)); then
    echo "cannot read the security group (AWS session expired); probes of $NEW_IP:$out so the conf's INGRESS is not applied. Owner, after aws login --profile mgmt: $APPLY"
    return 3
  fi
  echo "probes of $NEW_IP:$out the group lets 80 and 443 in; udp 51820 cannot be read (inferred in, box.sh adds it first)"
}

c_dns_realtime() {
  local dns ns ttl
  dns=$(dig +short realtime.askloyal.com A | paste -sd' ' -)
  ns=$(dig +short NS askloyal.com | head -1)
  ttl=$([[ -n $ns ]] && dig +noall +answer @"$ns" realtime.askloyal.com A | awk 'NR == 1 { print $2 }')
  echo "realtime.askloyal.com A ${dns:-none}, record TTL ${ttl:-unread} s; the owner's switch-time step O2 moves it to $NEW_IP (lower the TTL the day before)"
  [[ $dns == "$NEW_IP" ]] && return 0
  [[ $dns == "$OLD_IP" ]] && return 3
  return 1
}

# The production deploy reaches the box through WireGuard loyal-ci on udp 51820 (scripts/ci-worker-network.sh), with the same
# server key: LOYAL_WORKERS_WIREGUARD_PUBLIC_KEY stays valid. The sealed key on loyal-main must be the key that runs on
# loyal-workers (compared by public key; no private key is printed), with the same conf and nft file, and nothing up.
c_wg_staged() {
  local sum="sha256sum /etc/wireguard/loyal-ci.conf /etc/wireguard/loyal-ci.nft /etc/systemd/system/wg-quick@loyal-ci.service.d/credentials.conf | cut -c1-16"
  local a b pub_old pub_new st
  a=$(old "$sum") || { echo "$OLD did not answer"; return 1; }
  b=$(new "$sum") || { echo "$NEW has no loyal-ci conf, nft file or drop-in"; return 1; }
  [[ $a == "$b" ]] || { echo "the loyal-ci conf, nft file or credential drop-in differ between $OLD and $NEW"; return 1; }
  pub_old=$(old "wg show loyal-ci public-key") || { echo "wg-quick@loyal-ci does not run on $OLD"; return 1; }
  pub_new=$(new "systemd-creds decrypt --name=private-key /etc/credstore.encrypted/loyal-ci-wireguard.cred - | wg pubkey") || { echo "the sealed key on $NEW does not decrypt"; return 1; }
  st=$(new "echo \$(systemctl is-enabled wg-quick@loyal-ci 2>&1) \$(systemctl is-active wg-quick@loyal-ci 2>&1); ip link show loyal-ci >/dev/null 2>&1 && echo interface-up || echo no-interface; ss -Hlun 'sport = :51820' | wc -l" | paste -sd' ' -) || { echo "$NEW did not answer"; return 1; }
  echo "conf, nft file and drop-in equal on both; public key ${pub_new:0:8}... of the key sealed on $NEW $([[ $pub_old == "$pub_new" ]] && echo equals || echo DIFFERS from) the one up on $OLD; $NEW: $st (want disabled inactive no-interface 0)"
  [[ $pub_old == "$pub_new" && $st == "disabled inactive no-interface 0" ]]
}

# Before the switch root login on loyal-main stays closed: the key is absent there (the handover installs it, the handback removes
# it), and the line it will install is readable on loyal-workers.
c_deploy_ssh() {
  local line n
  line=$(ci_key_line) || { echo "$OLD does not trust exactly one loyal-ci-production key as a restrict line; the handover has nothing to install"; return 1; }
  n=$(ci_key_count) || { echo "$NEW did not answer"; return 1; }
  echo "source: one restrict line on $OLD ($(ssh-keygen -lf - <<<"${line#restrict }" | awk '{print $2}' | cut -c8-15)...); $NEW root authorized_keys holds it $n time(s), want 0 before the switch; sshd: $(new "sshd -T | grep -i '^permitrootlogin '")"
  [[ $n == 0 ]]
}

# The owner's O4: the repo variable names the host the deploy dials (endpoint host:51820, ssh HostKeyAlias). It is read, never set.
c_ci_variable() {
  local v
  v=$(gh variable get LOYAL_WORKERS_HOST --env production -R loyal-labs/loyal-yield-routing 2>&1) || { echo "cannot read LOYAL_WORKERS_HOST (gh: ${v:0:60}); O4 sets it to $NEW_IP after unfreeze"; return 3; }
  echo "LOYAL_WORKERS_HOST=$v; O4 sets it to $NEW_IP after unfreeze (and LOYAL_WORKERS_KNOWN_HOSTS)"
  [[ $v == "$NEW_IP" ]] && return 0
  [[ $v == "$OLD_IP" ]] && return 3
  return 1
}

preflight() {
  check services c_services
  check pgmove-preflight c_pgmove
  check cutover-preflight c_cutover
  check units-29 c_units
  check old-sides c_old_sides
  check pgbouncer-path c_pgbouncer
  check pgbouncer-passwords c_passwords
  check tunnel c_tunnel
  check caddy-staged c_caddy_staged
  check caddy-tls c_caddy_tls
  check ingress-conf c_ingress_conf
  check ingress-applied c_ingress_applied
  check dns-realtime c_dns_realtime
  check wg-staged c_wg_staged
  check deploy-ssh c_deploy_ssh
  check ci-variable c_ci_variable
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
