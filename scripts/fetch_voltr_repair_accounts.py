#!/usr/bin/env python3
"""Read-only mainnet dump for the Voltr repair LiteSVM tests.

Fetches getAccountInfo (base64) for the programs + state accounts the crank
needs, follows BPF upgradeable programs to their programdata, and writes one
JSON per account into fixtures/voltr-repair/. Never signs or sends anything.
"""
import base64, json, os, sys, time, urllib.request

RPC = os.environ.get("RPC_URL", "https://api.mainnet-beta.solana.com")
OUT = os.path.join(os.path.dirname(__file__), "..", "crates", "squads-test-harness",
                   "fixtures", "voltr-repair")
OUT = os.path.abspath(OUT)
os.makedirs(OUT, exist_ok=True)

def rpc(method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    for attempt in range(6):
        try:
            req = urllib.request.Request(RPC, data=body, headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=40) as r:
                out = json.loads(r.read())
            if "error" in out:
                raise RuntimeError(out["error"])
            return out["result"]
        except Exception as e:
            print(f"  retry {attempt} {method}: {e}", file=sys.stderr)
            time.sleep(2 + attempt * 2)
    raise RuntimeError(f"rpc failed: {method} {params}")

def get_account(addr):
    return rpc("getAccountInfo", [addr, {"encoding": "base64", "commitment": "confirmed"}])

def save(addr, res, label=""):
    if res is None or res.get("value") is None:
        print(f"  MISSING {addr} {label}")
        return None
    v = res["value"]
    data_b64 = v["data"][0]
    rec = {
        "address": addr, "label": label, "owner": v["owner"],
        "lamports": v["lamports"], "executable": v["executable"],
        "rentEpoch": v.get("rentEpoch"), "space": len(base64.b64decode(data_b64)),
        "dataBase64": data_b64,
    }
    with open(os.path.join(OUT, addr + ".json"), "w") as f:
        json.dump(rec, f)
    print(f"  saved {addr} {label} space={rec['space']} owner={rec['owner']} exec={rec['executable']}")
    return rec

BPF_LOADER = "BPFLoaderUpgradeab1e11111111111111111111111"

ACCOUNTS = {
    "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8": "voltr_program",
    "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW": "adaptor_program",
    "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG": "squads_smart_account_program",
    "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA": "vault",
    "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6": "strategy_init_receipt",
    "9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj": "v2_strategy_config",
    "ABYbbc7ms3FdB9BDe8HPYMKvuU9iEj8a55rVuyuZeNSs": "v1_strategy_config",
    "C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5": "report_ticket",
    "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh": "idle_ata",
    "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M": "custody_strategy_asset_ata",
    "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe": "squads_usdc_ata",
    "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6": "squads_settings",
    "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh": "squads_vault",
    "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "usdc_mint",
    # installed v2 bridge policies (Squads action accounts)
    "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz": "policy_voltr_allocate",
    "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP": "policy_report_nav",
    "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY": "policy_stage_squads_to_voltr",
    "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t": "policy_voltr_restore_idle",
}

def main():
    slot = rpc("getSlot", [{"commitment": "confirmed"}])
    genesis = rpc("getGenesisHash", [])
    print(f"slot={slot} genesis={genesis}")
    manifest = {"slot": slot, "genesis": genesis, "rpc": RPC, "accounts": {}, "programdata": {}}
    for addr, label in ACCOUNTS.items():
        rec = save(addr, get_account(addr), label)
        if rec is None:
            continue
        manifest["accounts"][addr] = label
        if rec["owner"] == BPF_LOADER and rec["executable"]:
            raw = base64.b64decode(rec["dataBase64"])
            # UpgradeableLoaderState::Program { programdata_address }
            assert len(raw) >= 36 and int.from_bytes(raw[0:4], "little") == 2, "unexpected program account"
            pd_addr = b58encode(raw[4:36])
            print(f"  {label} programdata -> {pd_addr}")
            pdrec = save(pd_addr, get_account(pd_addr), label + "_programdata")
            if pdrec:
                manifest["programdata"][addr] = pd_addr
    with open(os.path.join(OUT, "_manifest.json"), "w") as f:
        json.dump(manifest, f, indent=1)
    print("DONE")

# minimal base58 encode (Bitcoin alphabet)
_B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
def b58encode(b: bytes) -> str:
    n = int.from_bytes(b, "big")
    out = ""
    while n > 0:
        n, r = divmod(n, 58)
        out = _B58[r] + out
    pad = 0
    for c in b:
        if c == 0:
            pad += 1
        else:
            break
    return "1" * pad + out

if __name__ == "__main__":
    main()
