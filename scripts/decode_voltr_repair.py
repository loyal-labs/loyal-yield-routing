#!/usr/bin/env python3
import base64, json, os, sys, urllib.request, time

OUT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "crates",
                      "squads-test-harness", "fixtures", "voltr-repair"))
RPC = os.environ.get("RPC_URL", "https://api.mainnet-beta.solana.com")
_B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
def b58(b):
    n = int.from_bytes(b, "big"); out = ""
    while n > 0:
        n, r = divmod(n, 58); out = _B58[r] + out
    pad = len(b) - len(b.lstrip(b"\0"))
    return "1"*pad + out

def load(addr):
    with open(os.path.join(OUT, addr + ".json")) as f:
        r = json.load(f)
    return base64.b64decode(r["dataBase64"]), r

def u64(b, o): return int.from_bytes(b[o:o+8], "little")
def u16(b, o): return int.from_bytes(b[o:o+2], "little")
def u128(b, o): return int.from_bytes(b[o:o+16], "little")

def rpc(method, params):
    body = json.dumps({"jsonrpc":"2.0","id":1,"method":method,"params":params}).encode()
    for a in range(5):
        try:
            req = urllib.request.Request(RPC, data=body, headers={"Content-Type":"application/json"})
            with urllib.request.urlopen(req, timeout=40) as r:
                return json.loads(r.read())["result"]
        except Exception as e:
            print("retry", e, file=sys.stderr); time.sleep(2+a*2)
    raise RuntimeError("rpc fail")

vault, _ = load("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA")
print("== VAULT ==")
print(" space", len(vault))
lp_mint = b58(vault[272:304])
print(" asset.mint      ", b58(vault[104:136]))
print(" asset.idleAta   ", b58(vault[136:168]))
print(" asset.totalValue", u64(vault, 168))
print(" lp.mint         ", lp_mint)
print(" pendingAdmin    ", b58(vault[336:368]))
print(" manager         ", b58(vault[368:400]))
print(" admin           ", b58(vault[400:432]))
print(" maxCap          ", u64(vault, 432))
print(" withdrawWaitSecs", u64(vault, 456))
print(" disabledOps     ", u16(vault, 464))
print(" adminPerfFeeBps  (feeConfig off528? adminPerformanceFee@514)", u16(vault, 514))
print(" managerPerfFee@512", u16(vault,512), "adminPerf@514", u16(vault,514))
print(" feeState.accLpManager", u64(vault,576),"admin",u64(vault,584),"protocol",u64(vault,592))
print(" HWM.highestAssetPerLpBits", u128(vault, 624))
print(" version", vault[664], "allowAnyAdaptor", vault[665])

rec, _ = load("3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6")
print("== RECEIPT ==")
print(" vault", b58(rec[8:40]), "strategy", b58(rec[40:72]))
print(" adaptorProgram", b58(rec[72:104]))
print(" positionValue", u64(rec, 104), "lastUpdatedTs", u64(rec,112), "version", rec[120])

for label, addr in [("idle_ata","6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"),
                    ("custody_ata","FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M"),
                    ("squads_usdc_ata","EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe")]:
    d, _ = load(addr)
    print(f"== {label} ==  mint {b58(d[0:32])} owner {b58(d[32:64])} amount {u64(d,64)} state {d[108]} delegateTag {u16(d,72) if False else int.from_bytes(d[72:76],'little')}")

mint, _ = load("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
print("== USDC MINT == decimals", mint[44], "supplyLo", u64(mint,36))

# v2 config decode (worktree layout: disc8, ver1, idx1, 6 zero, 12 pubkeys, 5 u64, digest32)
cfg, _ = load("9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj")
print("== V2 CONFIG ==  space", len(cfg), "disc", list(cfg[:8]), "ver", cfg[8], "idx", cfg[9])
names=["voltr_program","voltr_vault","strategy","vault_strategy_auth","squads_program",
       "squads_settings","squads_settings_signer","squads_vault","asset_mint",
       "asset_token_program","squads_asset_ata","(reserved_pubkey)"]
for i,nm in enumerate(names):
    print(f"   {nm:24s}", b58(cfg[16+i*32:16+i*32+32]))
o=16+12*32
print("   max_report_nav_raw   ", u64(cfg,o)); print("   max_report_age_slots ", u64(cfg,o+8))
print("   last_sequence", u64(cfg,o+16), "last_observed_slot", u64(cfg,o+24), "last_nav_raw", u64(cfg,o+32))

tk, _ = load("C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5")
print("== REPORT TICKET ==  space", len(tk), "disc", list(tk[:8]), "ver", tk[8], "bump", tk[9],
      "armed", tk[10], "config", b58(tk[16:48]), "last_consumed_seq", u64(tk,48),
      "active_seq", u64(tk,56))

sett, _ = load("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6")
print("== SQUADS SETTINGS ==  space", len(sett), "disc", list(sett[:8]))

# Fetch LP mint + largest holders
print("== FETCH LP MINT", lp_mint, "==")
res = rpc("getAccountInfo", [lp_mint, {"encoding":"base64","commitment":"confirmed"}])
if res and res.get("value"):
    v = res["value"]; raw = base64.b64decode(v["data"][0])
    rec2 = {"address":lp_mint,"label":"lp_mint","owner":v["owner"],"lamports":v["lamports"],
            "executable":v["executable"],"rentEpoch":v.get("rentEpoch"),
            "space":len(raw),"dataBase64":v["data"][0]}
    with open(os.path.join(OUT, lp_mint+".json"),"w") as f: json.dump(rec2,f)
    print(" LP mint decimals", raw[44], "supply", u64(raw,36), "mintAuthTag", int.from_bytes(raw[0:4],'little'),
          "mintAuth", b58(raw[4:36]))
largest = rpc("getTokenLargestAccounts", [lp_mint, {"commitment":"confirmed"}])
print(" LP largest holders:")
holders=[]
for h in (largest or {}).get("value", []):
    print("   ", h["address"], h["amount"])
    holders.append(h["address"])
# fetch + save each holder token account
for h in holders:
    res = rpc("getAccountInfo", [h, {"encoding":"base64","commitment":"confirmed"}])
    if res and res.get("value"):
        v=res["value"]; raw=base64.b64decode(v["data"][0])
        rec3={"address":h,"label":"lp_holder","owner":v["owner"],"lamports":v["lamports"],
              "executable":v["executable"],"rentEpoch":v.get("rentEpoch"),"space":len(raw),"dataBase64":v["data"][0]}
        with open(os.path.join(OUT,h+".json"),"w") as f: json.dump(rec3,f)
        print("    holder",h,"owner",b58(raw[32:64]),"amount",u64(raw,64))
print("DONE")
