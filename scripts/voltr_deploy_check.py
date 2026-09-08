import base64
import json
import time
import urllib.request

RPC = "https://api.mainnet-beta.solana.com"


def rpc(m, p, tries=5):
    for i in range(tries):
        try:
            r = urllib.request.Request(
                RPC,
                data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": m, "params": p}).encode(),
                headers={"Content-Type": "application/json"},
            )
            out = json.load(urllib.request.urlopen(r, timeout=40))
            if "error" in out:
                raise RuntimeError(out["error"])
            return out["result"]
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(2 + 2 * i)
    raise last


def b58(b):
    A = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    n = int.from_bytes(b, "big")
    s = ""
    while n:
        n, r = divmod(n, 58)
        s = A[r] + s
    return "1" * (len(b) - len(b.lstrip(b"\0"))) + s


VOLTR = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
acc = rpc("getAccountInfo", [VOLTR, {"encoding": "base64"}])["value"]
data = base64.b64decode(acc["data"][0])
pd = b58(data[4:36])
pdacc = rpc("getAccountInfo", [pd, {"encoding": "base64"}])["value"]
d = base64.b64decode(pdacc["data"][0])
slot = int.from_bytes(d[4:12], "little")
auth = b58(d[13:45]) if d[12] else None
print("Voltr programdata", pd, "last_deploy_slot", slot, "upgrade_authority", auth, "len", len(d))
print("incident slots: hnQZ9v9i=443586075  46UBvSw1=444157954  litesvm_dump=445235325")
# deploy history: signatures touching programdata account
sigs = rpc("getSignaturesForAddress", [pd, {"limit": 20}])
for s in sigs:
    print("programdata tx", s["slot"], s["signature"][:16], s.get("blockTime"), "err" if s.get("err") else "ok")

# receipt version bytes: strategy 1 receipt on mainnet
r = rpc("getAccountInfo", ["3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6", {"encoding": "base64"}])["value"]
rb = base64.b64decode(r["data"][0])
print(
    "receipt1 len",
    len(rb),
    "positionValue",
    int.from_bytes(rb[104:112], "little"),
    "lastUpdatedTs",
    int.from_bytes(rb[112:120], "little"),
    "version",
    rb[120],
    "bump",
    rb[121],
    "authBump",
    rb[122],
    "reserved_nonzero",
    any(rb[128:192]),
)
# vault account: version-ish bytes near the top and total len
v = rpc("getAccountInfo", ["HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA", {"encoding": "base64"}])["value"]
vb = base64.b64decode(v["data"][0])
print("vault len", len(vb), "first 16 bytes after disc", vb[8:24].hex())
# Adw vault Kamino receipts for comparison of version byte
for rec in ["BjBuUPgR"]:
    pass
