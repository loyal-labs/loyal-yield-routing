import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import bs58 from "bs58";
import { createTransferInstruction } from "@solana/spl-token";
import {
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type AccountInfo,
} from "@solana/web3.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  buildExactJupiterSquadsExecution,
  signExactJupiterSquadsExecution,
} from "./rwa-phase2-jupiter-execution.js";
type J = Record<string, unknown>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const sha = (x: Uint8Array | string) =>
  createHash("sha256").update(x).digest("hex");
const inv: (x: unknown, m: string) => asserts x = (x, m) => {
  if (!x) throw Error(m);
};
const obj = (x: unknown, m: string) => {
  inv(x && typeof x === "object" && !Array.isArray(x), m);
  return x as J;
};
const OUT = (e: string) =>
  resolve(
    ROOT,
    `docs/evidence/backyard-rwa-go/policy-helius-prefix-${e.replaceAll(">", "-")}-${process.env.RWA_PHASE2_PREFIX_VERSION?.trim() || "v1"}.json`,
  );
function ci(x: unknown) {
  const r = obj(x, "create");
  const d = Buffer.from(String(r.dataBase64), "base64");
  return new TransactionInstruction({
    programId: new PublicKey(String(r.programId)),
    data: d,
    keys: (r.accounts as J[]).map((a) => ({
      pubkey: new PublicKey(String(a.address)),
      isSigner: a.signer === true,
      isWritable: a.writable === true,
    })),
  });
}
function state(x: readonly (AccountInfo<Buffer> | null)[]) {
  return sha(
    JSON.stringify(
      x.map((a) =>
        a
          ? {
              o: a.owner.toBase58(),
              d: a.data.toString("base64"),
              l: a.lamports,
            }
          : null,
      ),
    ),
  );
}
async function main() {
  const edge = process.argv[2];
  inv(
    edge === "USDC->PRIME" ||
      edge === "USDC->ONyc" ||
      edge === "ONyc->USDC" ||
      edge === "PRIME->USDC" ||
      edge === "USDC->USDG" ||
      edge === "USDC->PYUSD",
    "edge",
  );
  const out = OUT(edge);
  inv(!existsSync(out), "output exists");
  const rpc = process.env.SOLANA_RPC_URL;
  inv(rpc, "rpc");
  const a = obj(
    JSON.parse(
      readFileSync(
        resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json"),
        "utf8",
      ),
    ),
    "artifact",
  );
  const h = obj(
    JSON.parse(
      readFileSync(
        resolve(
          ROOT,
          "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json",
        ),
        "utf8",
      ),
    ),
    "headers",
  );
  const ps = a.policies as J[];
  const i = ps.findIndex((p) =>
    (p.swapEdges as J[]).some((e) => `${e.from}->${e.to}` === edge),
  );
  inv(i >= 0, "policy");
  const p = ps[i]!,
    raw = (h.rows as J[]).find((r) => r.key === edge)!;
  const row = JSON.parse(JSON.stringify(raw)) as J,
    ix = obj(row.instruction, "ix"),
    data = Buffer.from(String(ix.dataBase64), "base64"),
    hi = obj(obj(row.header, "header").indexes, "idx");
  const reverseToUsdc = edge.endsWith("->USDC");
  if (reverseToUsdc) {
    // The preceding USDC->PRIME funding leg currently yields about 946k raw;
    // keep the reverse leg below that simulated balance.
    data.writeBigUInt64LE(edge === "PRIME->USDC" ? 900_000n : 1_000_000n, data.length - 19);
    data.writeBigUInt64LE(1n, data.length - 11);
  }
  ix.dataBase64 = data.toString("base64");
  ix.dataSha256 = sha(data);
  const admin = Keypair.fromSecretKey(
      (await signingMaterialFromEnvironment("SOLANA_TESTING_PK")).secretKey,
    ),
    del = Keypair.fromSecretKey(
      (await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey,
    ),
    c = new Connection(rpc, "confirmed"),
    bh = await c.getLatestBlockhashAndContext("confirmed");
  const ex = await buildExactJupiterSquadsExecution({
    connection: c,
    compiledPolicy: p,
    headerRow: row,
    delegatedSigner: del.publicKey,
  });
  const primeFunding = edge === "PRIME->USDC"
    ? await (async () => {
        const fundingEdge = "USDC->PRIME";
        const fundingPolicy = ps.find((candidate) =>
          (candidate.swapEdges as J[]).some(
            (candidateEdge) => `${candidateEdge.from}->${candidateEdge.to}` === fundingEdge,
          ),
        );
        const fundingHeader = (h.rows as J[]).find((candidate) => candidate.key === fundingEdge);
        inv(fundingPolicy && fundingHeader, "USDC->PRIME funding route is absent");
        return buildExactJupiterSquadsExecution({
          connection: c,
          compiledPolicy: fundingPolicy,
          headerRow: fundingHeader,
          delegatedSigner: del.publicKey,
        });
      })()
    : null;
  const wire = (payer: Keypair, ins: TransactionInstruction[]) => {
    const t = new VersionedTransaction(
      new TransactionMessage({
        payerKey: payer.publicKey,
        recentBlockhash: bh.value.blockhash,
        instructions: ins,
      }).compileToV0Message(),
    );
    t.sign([payer]);
    const w = t.serialize();
    inv(w.length <= 1232, "packet");
    return { w, s: bs58.encode(t.signatures[0]!) };
  };
  const ws: any[] = [];
  if (edge === "ONyc->USDC") {
    const source = obj(row.source, "source"),
      src = new PublicKey(String(source.ata)),
      dst = ex.innerInstruction.keys[3]!.pubkey;
    ws.push(
      wire(admin, [
        createTransferInstruction(src, dst, admin.publicKey, 1_000_000),
      ]),
    );
  }
  for (const q of ps.slice(0, i + 1))
    ws.push(wire(admin, [ci(q.createInstruction)]));
  if (primeFunding) {
    const funding = signExactJupiterSquadsExecution({
      execution: primeFunding,
      payer: del,
      recentBlockhash: bh.value.blockhash,
    });
    ws.push({
      w: funding.wire,
      s: bs58.encode(funding.transaction.signatures[0]!),
    });
  }
  const sx = signExactJupiterSquadsExecution({
    execution: ex,
    payer: del,
    recentBlockhash: bh.value.blockhash,
  });
  ws.push({ w: sx.wire, s: bs58.encode(sx.transaction.signatures[0]!) });
  inv(ws.length <= 20, "bundle cap");
  const inspect = [
    RWA_MULTIPLY_ROUTE.squads.settings,
    RWA_MULTIPLY_ROUTE.squads.vault,
    ex.policy,
    ex.innerInstruction.keys[3]!.pubkey.toBase58(),
    ex.innerInstruction.keys[6]!.pubkey.toBase58(),
  ];
  const before = await c.getMultipleAccountsInfoAndContext(
    inspect.map((x) => new PublicKey(x)),
    { commitment: "confirmed", minContextSlot: bh.context.slot },
  );
  const pre = await c.getSignatureStatuses(
    ws.map((x) => x.s),
    { searchTransactionHistory: true },
  );
  const res = (await (
    await fetch(rpc, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: "prefix",
        method: "simulateBundle",
        params: [
          {
            encodedTransactions: ws.map((x) =>
              Buffer.from(x.w).toString("base64"),
            ),
          },
          {
            preExecutionAccountsConfigs: ws.map(() => ({
              addresses: inspect,
              encoding: "base64",
            })),
            postExecutionAccountsConfigs: ws.map(() => ({
              addresses: inspect,
              encoding: "base64",
            })),
            skipSigVerify: false,
            simulationBank: { commitment: { commitment: "confirmed" } },
            transactionEncoding: "base64",
            replaceRecentBlockhash: false,
          },
        ],
      }),
    })
  ).json()) as any;
  const post = await c.getSignatureStatuses(
    ws.map((x) => x.s),
    { searchTransactionHistory: true },
  );
  const after = await c.getMultipleAccountsInfoAndContext(
    inspect.map((x) => new PublicKey(x)),
    { commitment: "confirmed", minContextSlot: before.context.slot },
  );
  inv(state(before.value) === state(after.value), "state");
  const r = res.result?.value,
    pass =
      r?.summary === "succeeded" &&
      r.transactionResults?.every((x: any) => x.err === null);
  writeFileSync(
    out,
    JSON.stringify(
      {
        schema: "prefix-jupiter/v1",
        verdict: pass ? "PASS" : "REJECTED",
        edge,
        broadcast: false,
        signedUnsent: true,
        compiledArtifactSha256: sha(
          readFileSync(
            resolve(
              ROOT,
              "docs/evidence/backyard-rwa-go/policy-compiled-v1.json",
            ),
          ),
        ),
        transactions: ws.map((x) => ({
          transactionBase64: Buffer.from(x.w).toString("base64"),
          transactionSha256: sha(x.w),
          signature: x.s,
          packetBytes: x.w.length,
        })),
        simulation: {
          err: pass ? null : r,
          contextSlot: res.result?.context?.slot,
        },
        signatureAbsentOnChain:
          pre.value.every((x: any) => x === null) &&
          post.value.every((x: any) => x === null),
        chainPreStateSha256: state(before.value),
        chainPostStateSha256: state(after.value),
        confirmedReadbackSlot: after.context.slot,
      },
      null,
      2,
    ),
  );
  console.log(JSON.stringify({ edge, pass, out, tx: ws.length }));
}
main();
