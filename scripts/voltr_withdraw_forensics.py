import base64
import json
import sys
import time
import urllib.request

RPC = "https://api.mainnet-beta.solana.com"
RECEIPT = "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6"
WINDOWS = [(443583800, 443586300), (444157400, 444158100)]
RESULTS = "/Users/user/loyal/loyal-yield-routing/.claude/worktrees/voltr-reset-proof/docs/evidence/voltr-reset-litesvm-2026-09-08.results.json"


def rpc(m, p, tries=6):
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
            time.sleep(2 + 2 * i)
            last = e
    raise last


def u64(b, o):
    return int.from_bytes(b[o : o + 8], "little")


def decode_event(b64):
    b = base64.b64decode(b64)
    if len(b) != 312:
        return None
    o = 8 + 6 * 32
    return {
        "amountArgOrMeasured": u64(b, o),
        "tvBefore": u64(b, o + 8),
        "tvAfter": u64(b, o + 16),
        "lpBefore": u64(b, o + 24),
        "lpAfter": u64(b, o + 32),
        "idleBefore": u64(b, o + 72),
        "idleAfter": u64(b, o + 80),
        "posBefore": u64(b, o + 88),
        "posAfter": u64(b, o + 96),
    }


def events_from_logs(logs):
    out = []
    kind = None
    for line in logs:
        if "Instruction: DepositStrategy" in line:
            kind = "DepositStrategy"
        elif "Instruction: WithdrawStrategy" in line:
            kind = "WithdrawStrategy"
        elif line.startswith("Program data: "):
            ev = decode_event(line[len("Program data: ") :])
            if ev:
                ev["kind"] = kind
                out.append(ev)
    return out


def main():
    # 1. LiteSVM proof: R5a withdraw + hazards events
    try:
        d = json.load(open(RESULTS))
        s = json.dumps(d)
        print("== LiteSVM reset-proof: WithdrawStrategy events found in results JSON")
        import re

        for m in re.finditer(r'"Program data: ([A-Za-z0-9+/=]+)"', s):
            ev = decode_event(m.group(1))
            if ev:
                print(json.dumps(ev))
    except Exception as e:  # noqa: BLE001
        print("results JSON read failed:", e)

    # 2. mainnet signatures on receipt in the two windows
    lo = min(w[0] for w in WINDOWS)
    sigs = []
    before = None
    while True:
        page = rpc("getSignaturesForAddress", [RECEIPT, {"limit": 1000, "before": before}])
        if not page:
            break
        sigs.extend(page)
        before = page[-1]["signature"]
        if page[-1]["slot"] < lo:
            break
        time.sleep(0.5)
    print("== total signatures paged:", len(sigs))
    for (a, b) in WINDOWS:
        print(f"== window {a}-{b}")
        rows = [x for x in sigs if a <= x["slot"] <= b]
        rows.sort(key=lambda x: x["slot"])
        for x in rows:
            tx = rpc(
                "getTransaction",
                [x["signature"], {"encoding": "json", "maxSupportedTransactionVersion": 0}],
            )
            time.sleep(0.4)
            logs = (tx or {}).get("meta", {}).get("logMessages", []) or []
            kinds = sorted({l.split("Instruction: ")[1] for l in logs if "Instruction: " in l})
            evs = events_from_logs(logs)
            err = (tx or {}).get("meta", {}).get("err")
            print(x["slot"], x["signature"][:12], "err" if err else "ok", kinds)
            for ev in evs:
                print("    ", json.dumps(ev))


if __name__ == "__main__":
    main()
