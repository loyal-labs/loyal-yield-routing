"""Byte pipe for deploy/aws/loyal-main-config.sh and robinx-main-config.sh; it runs as
`python3 -I -c <this file> <mode> <base64 json>`.

  send   on an old host as root: read the listed files, trees, links and credentials, write frames to stdout
  local  on the Mac: the same for release files and generated drop-ins, skipping what the new box already has
  have   on the new box as root: print "sha256 mode owner path" for each path on stdin that exists
  recv   on the new box as root: read frames, apply the named rewrites (per path, and cred_rules to every
         credential), write only what differs, print each change
  creds  on any host as root: per sealed credential, the decrypt exit code and the host:port of every
         postgres URL inside it
  sums   on any host as root: "label sha256" for the env keys, files, matching lines and credentials named in
         the params, so two hosts' secrets compare without either printing one

A frame is one JSON header line, then exactly header["size"] bytes. No mode prints file or credential contents;
recv prints paths, rewrite names and counts only.
"""
import base64, glob, grp, hashlib, json, os, pwd, re, stat, subprocess, sys

CLUTTER = re.compile(r"(\.(bak|prev[0-9]*|slow)(-|$)|\.pre-|\.dpkg-|[.-]before$|\.before\.|(^|/)\._)")
CRED_ROOT = "/etc/credstore.encrypted"
URL = re.compile(rb"postgres(?:ql)?://[^\s\"'<>\\]+")


def die(msg):
    sys.exit(f"box-copy: {msg}")


def arg(i):
    return json.loads(base64.b64decode(sys.argv[i])) if len(sys.argv) > i else {}


def owner(st):
    return f"{pwd.getpwuid(st.st_uid).pw_name}:{grp.getgrgid(st.st_gid).gr_name}"


def frame(out, head, data=b""):
    head["size"] = len(data)
    out.write(json.dumps(head, sort_keys=True).encode() + b"\n")
    out.write(data)


def run(args, data=None):
    r = subprocess.run(args, input=data, capture_output=True)
    return r.returncode, r.stdout


def cred_ids():
    """Credential path -> ID, from the LoadCredentialEncrypted= lines of the units on this host."""
    ids = {}
    for path in glob.glob("/etc/systemd/system/**", recursive=True):
        if not os.path.isfile(path) or os.path.islink(path):
            continue
        with open(path, errors="replace") as f:
            for line in f:
                key, _, val = line.strip().partition("=")
                if key == "LoadCredentialEncrypted":
                    name, _, p = val.partition(":")
                    ids[os.path.realpath(p or os.path.join(CRED_ROOT, name))] = name
    return ids


def cred_id(path, ids, named=None):
    """The ID a credential was sealed under: the caller's `ids` entry for its file name, a unit's name for it,
    else its file name, else the ID of the referenced credential whose path it extends (a rotated copy such as
    name.a347)."""
    path = os.path.realpath(path)
    if named and os.path.basename(path) in named:
        return [named[os.path.basename(path)]]
    if path in ids:
        return [ids[path]]
    base = os.path.join(os.path.dirname(path), os.path.basename(path).split(".")[0])
    return [os.path.basename(path)] + ([ids[base]] if base in ids else [])


def decrypt(path, names):
    for name in names:
        rc, out = run(["systemd-creds", "decrypt", f"--name={name}", path, "-"])
        if rc == 0:
            return name, out
    return None, None


def send(spec, local):
    out = sys.stdout.buffer
    have = {}
    if local:
        for line in open(spec["have"]) if spec.get("have") else []:
            sha, mode, own, path = line.rstrip("\n").split(" ", 3)
            have[path] = (sha, mode, own)
    ids = cred_ids() if not local else {}
    skip = re.compile(spec["skip"]) if spec.get("skip") else None

    def emit_file(src, dst, mode=None, own=None, data=None):
        if data is None:
            with open(src, "rb") as f:
                data = f.read()
        if local and have.get(dst) == (hashlib.sha256(data).hexdigest(), mode, own):
            return
        frame(out, {"k": "file", "dst": dst, "mode": mode, "own": own}, data)

    def walk(src, dst):
        st = os.lstat(src)
        if stat.S_ISLNK(st.st_mode):
            die(f"{src} is a symlink; list its target instead")
        if CLUTTER.search(src) or (skip and skip.search(src)):
            return
        mode = "%04o" % stat.S_IMODE(st.st_mode)
        if stat.S_ISDIR(st.st_mode):
            frame(out, {"k": "dir", "dst": dst, "mode": mode, "own": owner(st)})
            for name in sorted(os.listdir(src)):
                walk(os.path.join(src, name), os.path.join(dst, name))
        elif stat.S_ISREG(st.st_mode):
            emit_file(src, dst, mode, owner(st))

    for e in spec["entries"]:
        k = e.get("k", "path")
        if k == "glob":
            for src in sorted(glob.glob(e["src"])):
                walk(src, src)
        elif k == "path":
            if not os.path.lexists(e["src"]):
                die(f"{e['src']} is missing")
            walk(e["src"], e.get("dst", e["src"]))
        elif k == "creds":
            only = re.compile(e["only"]) if e.get("only") else None
            for src in sorted(glob.glob(os.path.join(CRED_ROOT, "**"), recursive=True)):
                if not os.path.isfile(src) or CLUTTER.search(src) or (skip and skip.search(src)):
                    continue
                if only and not only.search(os.path.basename(src)):
                    continue
                name, data = decrypt(src, cred_id(src, ids, e.get("ids")))
                if name is None:
                    die(f"{src} does not decrypt here under any known ID")
                st = os.stat(src)
                frame(out, {"k": "cred", "dst": src, "name": name, "mode": "%04o" % stat.S_IMODE(st.st_mode),
                            "own": owner(st)}, data)
        elif k == "seal":
            st = os.stat(e["src"])
            with open(e["src"], "rb") as f:
                frame(out, {"k": "cred", "dst": e["dst"], "name": e["name"], "mode": "0600", "own": "root:root"},
                      f.read())
        elif k == "dir":
            frame(out, {"k": "dir", "dst": e["dst"], "mode": e["mode"], "own": e["own"]})
        elif k == "link":
            frame(out, {"k": "link", "dst": e["dst"], "mode": "0777", "own": e["own"]}, e["target"].encode())
        elif k == "file":
            data = base64.b64decode(e["data"]) if "data" in e else None
            emit_file(e.get("src"), e["dst"], e["mode"], e["own"], data)
        elif k == "tree":
            for root, dirs, files in os.walk(e["src"]):
                dirs.sort()
                rel = os.path.relpath(root, e["src"])
                d = os.path.normpath(os.path.join(e["dst"], rel))
                frame(out, {"k": "dir", "dst": d, "mode": e["dmode"], "own": e["own"]})
                for name in sorted(files):
                    emit_file(os.path.join(root, name), os.path.join(d, name), e["fmode"], e["own"])
        else:
            die(f"unknown entry kind {k}")
    frame(out, {"k": "end", "dst": "", "mode": "", "own": ""})


def rewrite(dst, data, rules, args):
    notes = []
    for rule in rules:
        if rule == "db":
            n = data.count(b"10.92.1.1")
            data = data.replace(b"10.92.1.1", b"127.0.0.1")
        elif rule == "listen":
            n = data.count(b",10.92.1.1")
            data = data.replace(b",10.92.1.1", b"")
            n == 1 or die(f"{dst}: rule listen matched {n} times, want 1")
        elif rule in ("pubip", "launcher"):
            old, new = args[f"old_{rule}"].encode(), args[f"new_{rule}"].encode()
            n = data.count(old)
            data = data.replace(old, new)
            n >= 1 or die(f"{dst}: rule {rule} matched nothing")
        elif rule.startswith("sub:"):
            name = rule[4:]
            old, new = args[f"old_{name}"].encode(), args[f"new_{name}"].encode()
            n = data.count(old)
            data = data.replace(old, new)
            n >= 1 or die(f"{dst}: rule {rule} matched nothing")
        else:
            die(f"unknown rule {rule}")
        notes.append(f"{rule} x{n}")
    return data, notes


def same_meta(path, mode, own):
    st = os.lstat(path)
    return "%04o" % stat.S_IMODE(st.st_mode) == mode and owner(st) == own


def place(dst, mode, own, write):
    parent = os.path.dirname(dst)
    if not os.path.isdir(parent):
        os.makedirs(parent, mode=0o700)
    tmp = os.path.join(parent, f".{os.path.basename(dst)}.box-copy")
    write(tmp)
    user, group = own.split(":")
    os.chown(tmp, pwd.getpwnam(user).pw_uid, grp.getgrnam(group).gr_gid)
    os.chmod(tmp, int(mode, 8))
    os.replace(tmp, dst)


def recv(params):
    rules, args, cred_rules = params.get("rules", {}), params.get("args", {}), params.get("cred_rules", [])
    seen, changes, ended = set(), 0, False
    inp = sys.stdin.buffer
    while True:
        line = inp.readline()
        if not line:
            break
        h = json.loads(line)
        if h["k"] == "end":
            ended = True
            break
        data = inp.read(h["size"])
        len(data) == h["size"] or die(f"{h['dst']}: short frame")
        dst, mode, own = h["dst"], h["mode"], h["own"]
        if h["k"] == "dir":
            if os.path.isdir(dst) and same_meta(dst, mode, own):
                continue
            os.makedirs(dst, mode=0o700, exist_ok=True)
            user, group = own.split(":")
            os.chown(dst, pwd.getpwnam(user).pw_uid, grp.getgrnam(group).gr_gid)
            os.chmod(dst, int(mode, 8))
            changes += 1
            print(f"changed: {dst} dir {mode} {own}")
            continue
        if h["k"] == "link":
            seen.add(dst)
            user, group = own.split(":")
            if os.path.islink(dst) and os.readlink(dst) == data.decode() and same_meta(dst, mode, own):
                continue
            os.makedirs(os.path.dirname(dst), mode=0o755, exist_ok=True)
            tmp = os.path.join(os.path.dirname(dst), f".{os.path.basename(dst)}.box-copy")
            if os.path.lexists(tmp):
                os.remove(tmp)
            os.symlink(data.decode(), tmp)
            os.lchown(tmp, pwd.getpwnam(user).pw_uid, grp.getgrnam(group).gr_gid)
            os.replace(tmp, dst)
            changes += 1
            print(f"changed: {dst} link -> {data.decode()} {own}")
            continue
        seen.add(dst)
        data, notes = rewrite(dst, data, rules.get(dst, []) + (cred_rules if h["k"] == "cred" else []), args)
        note = f" ({', '.join(notes)})" if notes else ""
        if h["k"] == "file":
            if os.path.isfile(dst) and same_meta(dst, mode, own) and open(dst, "rb").read() == data:
                continue

            def write(tmp, data=data):
                with open(tmp, "wb") as f:
                    f.write(data)
            place(dst, mode, own, write)
        elif h["k"] == "cred":
            name = h["name"]
            if os.path.isfile(dst) and same_meta(dst, mode, own):
                rc, have = run(["systemd-creds", "decrypt", f"--name={name}", dst, "-"])
                if rc == 0 and have == data:
                    continue

            def write(tmp, data=data, name=name):
                rc, _ = run(["systemd-creds", "encrypt", "--with-key=host", f"--name={name}", "-", tmp], data)
                rc == 0 or die(f"{dst}: systemd-creds encrypt failed")
            place(dst, mode, own, write)
        changes += 1
        print(f"changed: {dst} {h['k']} {mode} {own}{note}")
    ended or die("the sender stopped before its end frame; nothing after the last change was written")
    missing = sorted(set(rules) - seen)
    missing and die(f"rewrite targets never arrived: {' '.join(missing)}")
    print(f"recv: {changes} change(s)")


def have():
    for line in sys.stdin:
        path = line.rstrip("\n")
        if os.path.isfile(path):
            with open(path, "rb") as f:
                sha = hashlib.sha256(f.read()).hexdigest()
            st = os.lstat(path)
            print(sha, "%04o" % stat.S_IMODE(st.st_mode), owner(st), path)


def creds(params):
    ids, n, ok = cred_ids(), 0, 0
    skip = re.compile(params["skip"]) if params.get("skip") else None
    for src in sorted(glob.glob(os.path.join(CRED_ROOT, "**"), recursive=True)):
        if not os.path.isfile(src) or CLUTTER.search(src) or (skip and skip.search(src)):
            continue
        n += 1
        name, data = decrypt(src, cred_id(src, ids))
        ok += name is not None
        hosts = []
        for m in URL.findall(data or b""):
            rest = m.split(b"://", 1)[1].split(b"/", 1)[0].rsplit(b"@", 1)[-1].decode(errors="replace")
            hosts.append(rest if ":" in rest else rest + ":5432")
        print(f"{'rc=0' if name else 'rc=1'} {src}{' db ' + ','.join(hosts) if hosts else ''}")
    print(f"creds: {ok} of {n} decrypt")
    sys.exit(0 if ok == n else 1)


def sums(params):
    def sha(b):
        return hashlib.sha256(b).hexdigest()

    for path in params.get("env", []):
        last = {}
        for line in open(path, "rb").read().split(b"\n"):
            m = re.match(rb"\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$", line)
            if m:
                last[m.group(1).decode()] = m.group(2)
        for key in sorted(last):
            print(f"{path}:{key} {sha(last[key])}")
    for path in params.get("files", []):
        print(f"{path} {sha(open(path, 'rb').read()) if os.path.isfile(path) else 'MISSING'}")
    for path, pattern in params.get("lines", []):
        hit = [ln for ln in open(path, "rb").read().split(b"\n") if re.search(pattern.encode(), ln)]
        digest = sha(b"\n".join(hit)) if hit else "MISSING"
        print(f"{path}:{pattern} {digest}")
    c = params.get("creds")
    if c:
        ids, only = cred_ids(), re.compile(c["only"])
        for src in sorted(glob.glob(os.path.join(CRED_ROOT, "*"))):
            if not os.path.isfile(src) or CLUTTER.search(src) or not only.search(os.path.basename(src)):
                continue
            name, data = decrypt(src, cred_id(src, ids, c.get("ids")))
            print(f"{src} {sha(data) if name else 'UNDECRYPTABLE'}")


mode = sys.argv[1] if len(sys.argv) > 1 else ""
if mode in ("send", "local"):
    send(arg(2), mode == "local")
elif mode == "recv":
    recv(arg(2))
elif mode == "have":
    have()
elif mode == "creds":
    creds(arg(2))
elif mode == "sums":
    sums(arg(2))
else:
    die("usage: send|local|recv|have|creds|sums <base64 json>")
