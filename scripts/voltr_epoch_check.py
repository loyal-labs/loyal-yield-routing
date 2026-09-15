import base64
import json
import time
import urllib.request
from collections import defaultdict

RPC = "https://api.mainnet-beta.solana.com"
VOLTR = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
UPGRADE_SLOT = 445223838
SLOTS_PER_EPOCH = 432000


def rpc(m, p, tries=6):
    last = None
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


def decode_event(b64):
    b = base64.b64decode(b64)
    if len(b) != 312:
        return None
    o = 8
    vault = b58(b[o + 32 : o + 64])
    adaptor = b58(b[o + 128 : o + 160])
    o2 = 8 + 6 * 32
    return {
        "vault": vault,
        "adaptor": adaptor,
        "amount": int.from_bytes(b[o2 : o2 + 8], "little"),
        "tvBefore": int.from_bytes(b[o2 + 8 : o2 + 16], "little"),
        "tvAfter": int.from_bytes(b[o2 + 16 : o2 + 24], "little"),
        "idleBefore": int.from_bytes(b[o2 + 72 : o2 + 80], "little"),
        "idleAfter": int.from_bytes(b[o2 + 80 : o2 + 88], "little"),
        "posBefore": int.from_bytes(b[o2 + 88 : o2 + 96], "little"),
        "posAfter": int.from_bytes(b[o2 + 96 : o2 + 104], "little"),
    }


def main():
    epoch_now = None
    slot_now = rpc("getSlot", [])
    epoch_now = slot_now // SLOTS_PER_EPOCH
    print("current slot", slot_now, "epoch", epoch_now, "upgrade epoch", UPGRADE_SLOT // SLOTS_PER_EPOCH)
    sigs = []
    before = None
    while len(sigs) < 400:
        page = rpc("getSignaturesForAddress", [VOLTR, {"limit": 200, "before": before}])
        if not page:
            break
        sigs.extend(page)
        before = page[-1]["signature"]
        if page[-1]["slot"] < UPGRADE_SLOT:
            break
        time.sleep(0.5)
    ok = [s for s in sigs if not s.get("err") and s["slot"] > UPGRADE_SLOT]
    print("post-upgrade ok signatures fetched:", len(ok), "slot range", min(s["slot"] for s in ok), max(s["slot"] for s in ok))
    groups = defaultdict(list)
    kinds_seen = defaultdict(int)
    n = 0
    for s in ok:
        tx = rpc("getTransaction", [s["signature"], {"encoding": "json", "maxSupportedTransactionVersion": 0}])
        time.sleep(0.35)
        n += 1
        logs = (tx or {}).get("meta", {}).get("logMessages", []) or []
        kind = None
        for line in logs:
            if "Instruction: " in line:
                k = line.split("Instruction: ")[1]
                if k in ("DepositStrategy", "WithdrawStrategy", "InstantWithdrawStrategy", "DirectWithdrawStrategy"):
                    kind = k
                kinds_seen[k] += 1
            elif line.startswith("Program data: ") and kind:
                ev = decode_event(line[len("Program data: ") :])
                if ev:
                    ev["kind"] = kind
                    ev["slot"] = s["slot"]
                    ev["sig"] = s["signature"][:12]
                    groups[(ev["vault"], ev["adaptor"])].append(ev)
    print("instruction kinds seen:", dict(kinds_seen))
    for (vault, adaptor), evs in groups.items():
        evs.sort(key=lambda e: e["slot"])
        epochs = defaultdict(int)
        for e in evs:
            epochs[e["slot"] // SLOTS_PER_EPOCH] += 1
        print(f"vault {vault[:8]} adaptor {adaptor[:8]} strategy-ops post-upgrade: {len(evs)} per-epoch {dict(epochs)}")
        for e in evs[:6]:
            credit = e["tvAfter"] - e["tvBefore"] - (e["posAfter"] - e["posBefore"])
            print("   ", e["kind"], e["slot"], e["sig"], "amount", e["amount"], "dTV", e["tvAfter"] - e["tvBefore"], "dPos", e["posAfter"] - e["posBefore"], "dIdle", e["idleAfter"] - e["idleBefore"], "impliedCredit", credit)


if __name__ == "__main__":
    main()
