import {expect,test} from "bun:test";
import {readFileSync} from "node:fs";
import {gunzipSync} from "node:zlib";
import {createHash} from "node:crypto";
import {onreBridgeEntryProof} from "./onre-bridge-entry-proof.js";

test("fresh real bridge failures remain failures and retain the raw accounting discrepancy",()=>{
  const bytes=readFileSync(new URL("../../../../docs/evidence/backyard-rwa-go/phase3/onre-voltr-entry-blocker-2026-09-06.json.gz",import.meta.url));
  expect(createHash("sha256").update(bytes).digest("hex")).toBe("635785b5642b978fce2f3f45a14990ac95bf7b448849fb757098843616912be1");
  const e=JSON.parse(gunzipSync(bytes).toString()).preflight.onreBridgeEntry.data.execution;
  const manifest=JSON.parse(readFileSync(new URL("../../../../docs/manifests/backyard-rwa-v1.json",import.meta.url),"utf8"));
  const result=onreBridgeEntryProof(e,manifest);
  expect(result.validObservation).toBe(true);
  expect(result.pass).toBe(false);
  expect(result.receiptExceedsVaultTotalRaw).toBe("119");
  expect(result.errors).toEqual([
    {action:"REPORT_NAV",error:{InstructionError:[0,{Custom:6004}]}},
    {action:"VOLTR_ALLOCATE_TO_SQUADS",error:{InstructionError:[0,{Custom:6004}]}},
  ]);
  for(const mutate of [
    (v:any)=>v.broadcast=true,
    (v:any)=>v.signatureProof=true,
    (v:any)=>v.sequentialProof=true,
    (v:any)=>v.rows.pop(),
    (v:any)=>v.rows.reverse(),
    (v:any)=>v.slot++,
    (v:any)=>v.vaultTotalValueRaw="2793417",
    (v:any)=>v.strategyReceiptValueRaw="0",
    (v:any)=>v.accounts.push(v.accounts[0]),
    (v:any)=>v.accounts.find((a:any)=>a.present).dataSha256="forged",
    (v:any)=>v.rows[0].input.accounts[0].lamports++,
    (v:any)=>v.rows[0].compiled.broadcast=true,
    (v:any)=>v.rows[0].compiled.steps[0].request.Report.NAVAfterRaw=119,
    (v:any)=>v.rows[0].compiled.steps[0].request.Settings="forged",
    (v:any)=>v.rows[0].compiled.steps[0].wireSha256="forged",
    (v:any)=>v.rows[1].compiled.steps[0].wireBase64=v.rows[0].compiled.steps[0].wireBase64,
    (v:any)=>v.rows[0].simulation.context.slot=v.slot-1,
    (v:any)=>v.rows[0].simulation.value.logs=[],
    (v:any)=>v.rows[0].simulation.value.err=null,
    (v:any)=>delete v.rows[0].simulation.value.err,
  ]) {
    const changed=structuredClone(e);mutate(changed);
    const proof=onreBridgeEntryProof(changed,manifest);
    expect(proof.validObservation).toBe(false);
    expect(proof.pass).toBe(false);
  }
});
