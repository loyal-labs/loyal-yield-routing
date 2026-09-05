import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { Reserve } from "@kamino-finance/klend-sdk";
import { ExtensionType, getExtensionTypes, getTransferFeeConfig, getTransferHook, unpackMint } from "@solana/spl-token";
import { PublicKey } from "@solana/web3.js";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CONTRACT = "docs/plans/backyard-rwa-phase3-family-activation-verifier.md";
const ROUTE = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const SERVICE = "srv-dabkt0ojo6nc7381o9fg";
const GOAL = "01a06b6c-8023-72b1-ad5d-c97c0662820e";
export const EXPECTED_LANES = [
  "Prime/PRIME/USDC", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS",
  "Maple/syrupUSDC/USDC", "Maple/syrupUSDC/USDG", "Maple/syrupUSDC/PYUSD",
  "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS",
  "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD",
].sort();
type Json = Record<string, any>;
type Observation = { status: "OBSERVED" | "BLOCKED"; source: string; data?: Json; reason?: string };
const read = (p: string) => readFileSync(resolve(ROOT, p), "utf8");
const json = (p: string): Json => JSON.parse(read(p));
const sha = (s: string | Uint8Array) => createHash("sha256").update(s).digest("hex");
export function exactSet(actual: unknown, expected: string[]): boolean {
  return Array.isArray(actual) && actual.every((x) => typeof x === "string") &&
    actual.length === expected.length && new Set(actual).size === actual.length &&
    [...actual].sort().every((x, i) => x === [...expected].sort()[i]);
}
async function rpc(method: string, params: unknown[] = []): Promise<any> {
  const endpoint = process.env.SOLANA_RPC_URL;
  if (!endpoint) throw new Error("RPC_CREDENTIAL_MISSING");
  const response = await fetch(endpoint, {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    signal: AbortSignal.timeout(30_000),
  });
  if (!response.ok) throw new Error("RPC_HTTP_" + response.status);
  const payload = await response.json() as Json;
  if (payload.error || payload.result === undefined) throw new Error("RPC_RESPONSE_FAILED");
  return payload.result;
}
async function chainObservation(catalog: Json, manifest: Json): Promise<Observation> {
  try {
    const genesis = await rpc("getGenesisHash");
    if (genesis !== manifest.genesisHash) return {status:"BLOCKED", source:"Solana RPC", reason:"GENESIS_MISMATCH"};
    const mintAddresses = [...new Set<string>(catalog.lanes.flatMap((l: Json) =>
      [l.candidateIdentity.collateralMint, l.candidateIdentity.debtMint]))];
    const addresses = [...new Set<string>([
      ...mintAddresses, manifest.identities.squadsSettings,
      manifest.identities.voltrVault, manifest.identities.v2StrategyConfig,
      manifest.identities.squadsUsdcAta, manifest.identities.delegatedExecutor,
      ...catalog.lanes.flatMap((l: Json) => [l.candidateIdentity.collateralReserve,l.candidateIdentity.debtReserve]),
    ])];
    const result = await rpc("getMultipleAccounts", [addresses, {commitment:"finalized",encoding:"base64"}]);
    if (result.value.length !== addresses.length) throw new Error("RPC_ACCOUNT_COUNT");
    const reserveAddresses = catalog.lanes.flatMap((l: Json) => [l.candidateIdentity.collateralReserve,l.candidateIdentity.debtReserve]);
    const accounts = result.value.map((account: Json | null, i: number) => {
      const address = addresses[i];
      if (!address) throw new Error("RPC_ACCOUNT_ADDRESS_MISSING");
      if (!account) return {address:addresses[i], present:false};
      const bytes = Buffer.from(account.data[0], "base64");
      let mint: Json = {};
      let reserve: Json = {};
      if (mintAddresses.includes(address)) {
        const decoded = unpackMint(new PublicKey(address), {
          data:bytes,owner:new PublicKey(account.owner),executable:account.executable,lamports:account.lamports,
        },new PublicKey(account.owner));
        const hook = getTransferHook(decoded);
        const fees = getTransferFeeConfig(decoded);
        const feeSummary = (fee: NonNullable<typeof fees>["olderTransferFee"]) => ({
          epoch:fee.epoch.toString(),maximumFeeRaw:fee.maximumFee.toString(),basisPoints:fee.transferFeeBasisPoints,
        });
        mint = {mintDecimals:decoded.decimals,mintInitialized:decoded.isInitialized,
          extensions:getExtensionTypes(decoded.tlvData).map(type => ({type,name:ExtensionType[type] ?? "UNMAPPED_SDK_ENUM"})),
          transferHook:hook ? {authority:hook.authority.toBase58(),programId:hook.programId.toBase58(),
            enabled:!hook.programId.equals(PublicKey.default)} : null,
          transferFees:fees ? {older:feeSummary(fees.olderTransferFee),newer:feeSummary(fees.newerTransferFee)} : null};
      }
      if (reserveAddresses.includes(addresses[i])) {
        if (account.owner !== "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD") throw new Error("RESERVE_OWNER_MISMATCH");
        const decoded = Reserve.decode(bytes);
        const sf = 1n << 60n;
        const borrowed = (BigInt(decoded.liquidity.borrowedAmountSf.toString())+sf-1n)/sf;
        const available = BigInt(decoded.liquidity.availableAmount.toString());
        const depositLimit = BigInt(decoded.config.depositLimit.toString());
        const borrowLimit = BigInt(decoded.config.borrowLimit.toString());
        reserve = {reserve:{status:Number(decoded.config.status),mint:String(decoded.liquidity.mintPubkey),
          mintDecimals:String(decoded.liquidity.mintDecimals),marketPriceSf:decoded.liquidity.marketPriceSf.toString(),
          availableRaw:available.toString(),borrowedRawCeil:borrowed.toString(),
          depositHeadroomRaw:(depositLimit>available+borrowed?depositLimit-available-borrowed:0n).toString(),
          borrowHeadroomRaw:(borrowLimit>borrowed?borrowLimit-borrowed:0n).toString(),
          loanToValuePct:Number(decoded.config.loanToValuePct),liquidationThresholdPct:Number(decoded.config.liquidationThresholdPct),
          utilizationLimitBlockBorrowingAbovePct:Number(decoded.config.utilizationLimitBlockBorrowingAbovePct)}};
      }
      return {
        address:addresses[i], present:true, owner:account.owner, executable:account.executable,
        lamports:String(account.lamports), dataLength:bytes.length, dataSha256:sha(bytes),
        ...mint,...reserve,
        ...(addresses[i] === manifest.identities.squadsUsdcAta && bytes.length >= 165
          ? {tokenAmountRaw:bytes.readBigUInt64LE(64).toString()} : {}),
      };
    });
    return {status:"OBSERVED",source:"fresh finalized Solana accounts",data:{genesis,slot:result.context.slot,accounts}};
  } catch {
    // Fetch/driver errors can contain credential-bearing URLs; never print them.
    return {status:"BLOCKED",source:"Solana RPC",reason:"RPC_READ_UNAVAILABLE"};
  }
}
async function databaseObservation(): Promise<Observation> {
  const connection = process.env.NEON_DATABASE_URL;
  if (!connection) return {status:"BLOCKED",source:"Postgres",reason:"DATABASE_CREDENTIAL_MISSING"};
  try {
    const u = new URL(connection);
    const sql = `SELECT json_build_object(
      'route', (SELECT json_build_object('routeKey',route_key,'leaseOwner',lease_owner,
        'leaseLive',lease_expires_at>clock_timestamp(),'fencingToken',fencing_token,
        'stateVersion',state_version,'phase3',state->'phase3')
        FROM loyal_yield.multiply_route_states WHERE route_key='${ROUTE}'),
      'nonterminal', (SELECT COALESCE(json_agg(json_build_object(
        'operationId',operation_id,'status',status,'action',action,'strategyKey',strategy_key,
        'signature',transaction_signature,'createdAt',created_at)), '[]'::json)
        FROM loyal_yield.multiply_operations WHERE route_key='${ROUTE}'
        AND status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling')),
      'phase3OperationCount', (SELECT count(*) FROM loyal_yield.multiply_operations
        WHERE route_key='${ROUTE}' AND expected_effects->'phase3'->>'goalId'='${GOAL}'),
      'latestOperation', (SELECT json_build_object('operationId',operation_id,
        'status',status,'action',action,'createdAt',created_at)
        FROM loyal_yield.multiply_operations WHERE route_key='${ROUTE}'
        ORDER BY created_at DESC LIMIT 1)
    )`;
    const child = spawn("psql",["-X","-q","-A","-t","-v","ON_ERROR_STOP=1","-c",sql], {
      cwd:ROOT, stdio:["ignore","pipe","pipe"],
      env:{...process.env,PGHOST:u.hostname,PGPORT:u.port||"5432",PGUSER:decodeURIComponent(u.username),
        PGPASSWORD:decodeURIComponent(u.password),PGDATABASE:u.pathname.slice(1),
        PGSSLMODE:"require",PGCONNECT_TIMEOUT:"10",
        PGOPTIONS:"-c default_transaction_read_only=on -c statement_timeout=30000"},
    });
    let output = "";
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", chunk => { output += chunk; });
    // Drain but never expose credential-bearing driver errors.
    child.stderr.resume();
    const deadline = setTimeout(() => child.kill(), 35_000);
    let code: number | null;
    try {
      code = await new Promise<number | null>((resolve, reject) => {
        child.once("error", reject);
        child.once("close", resolve);
      });
    } finally { clearTimeout(deadline); }
    if (code !== 0) throw new Error("DATABASE_READ_FAILED");
    return {status:"OBSERVED",source:"fresh read-only Postgres snapshot",data:JSON.parse(output)};
  } catch {
    return {status:"BLOCKED",source:"Postgres",reason:"DATABASE_READ_UNAVAILABLE"};
  }
}
async function deploymentObservation(): Promise<Observation> {
  const key = process.env.RENDER_API_KEY;
  if (!key) return {status:"BLOCKED",source:"Render",reason:"RENDER_CREDENTIAL_MISSING"};
  try {
    const r = await fetch("https://api.render.com/v1/services/"+SERVICE+"/deploys?limit=5", {
      headers:{Authorization:"Bearer "+key},signal:AbortSignal.timeout(30_000),
    });
    if (!r.ok) throw new Error("RENDER_READ_FAILED");
    const payload = await r.json() as Json[];
    return {status:"OBSERVED",source:"Render deploy API",data:{serviceId:SERVICE,
      deploys:payload.map((row) => {const d=row.deploy ?? row;return {
        id:d.id,status:d.status,createdAt:d.createdAt,finishedAt:d.finishedAt,
        commit:d.commit?.id,image:d.image?.ref,digest:d.image?.digest,
      };}),
    }};
  } catch {
    return {status:"BLOCKED",source:"Render",reason:"RENDER_READ_UNAVAILABLE"};
  }
}
export async function verify() {
  const manifest = json("docs/manifests/backyard-rwa-v1.json");
  const catalog = json("crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
  const catalogLanes = catalog.lanes.map((l: Json) => [l.market,l.collateral,l.debt].join("/"));
  const active = manifest.runtimeActivation?.runtimeRoutes ?? [];
  const offline = process.argv.includes("--offline");
  const [chain,database,deployment] = offline
    ? ["Solana RPC","Postgres","Render"].map(source => ({status:"BLOCKED" as const,source,reason:"OFFLINE_DIAGNOSTIC"}))
    : await Promise.all([chainObservation(catalog,manifest),databaseObservation(),deploymentObservation()]);
  const conditions = [
    ["R01","Production admission/send cap enforcement, durable exit reservations and negative behavior",
      {governorSource:existsSync(resolve(ROOT,"go/backyard-rwa-worker/internal/backyardrwa/budget.go")),capsMicros:[1000000,20000000,60000000]},
      "Implement and execute production-path reservation/sign/send/restart/exit witnesses; reconcile live budget state."],
    ["R02","Exact eleven-lane runtime allowlist and frozen three-family queue",
      {expectedLanes:EXPECTED_LANES,catalogLanes,runtimeRoutes:active,catalogExact:exactSet(catalogLanes,EXPECTED_LANES),runtimeExact:exactSet(active,EXPECTED_LANES)},
      "Resolve current bindings, validate compiled runtime capability and persist the approved queue."],
    ["R03","Shared debt/token/valuation/exit runtime with exact existing policy authority",
      {operationCount:catalog.operations.length,edgeCount:catalog.swapEdges.length,chain},
      "Execute each lane through the shared production paths and reconcile installed policy intersections."],
    ["R04","Batched all-lane positive/negative and full stateful lifecycle evidence",
      {lanes:EXPECTED_LANES,simulationTimeoutMs:180000},
      "Run current-chain and explicitly controlled-state production-program evidence with exact per-lane coverage."],
    ["R05","Immutable deployment, one fenced writer, flat sequential transitions",
      {deployment,database},
      "Bind deployed image to verified source and prove durable independent queue advancement under one lease."],
    ["R06","Three Go-originated LIVE_VALIDATED or proven CAPACITY_PENDING outcomes",
      {canaries:["OnRe/ONyc/USDC","AUTO/AUTO/PYUSD","Ethena/USDe/PYUSD"],database},
      "Complete each approved canary proof and independently reconcile finalized terminal custody."],
    ["R07","Prior-route, withdrawal, NAV and recovery behavior including reserved exits",
      {retainedPhase2:json("docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json").verdict},
      "Run affected production-path regressions and verify current shared custody/recovery invariants."],
    ["R08","Deployed eleven-lane readiness agrees with operational handoff and standing coverage",
      {runtimeRoutes:active},
      "Reconcile truthful lane statuses and retained/promoted proof with the deployed runtime."],
  ].map(([id,condition,evidence,resumeCondition]) => ({
    id,condition,verdict:"FAIL",measurementStatus:"NOT_IMPLEMENTED",evidence,resumeCondition,
  }));
  // Bootstrap intentionally cannot PASS: observed infrastructure and static
  // membership do not substitute for the contract's behavioral measurements.
  return {
    schema:"loyal-backyard-rwa-phase3-family-activation-verifier/v2",
    verdict:"FAIL",implementationStatus:"NOT_IMPLEMENTED",broadcast:false,readOnly:true,
    generatedAt:new Date().toISOString(),goalId:GOAL,
    contract:{path:CONTRACT,sha256:sha(read(CONTRACT))},
    preflight:{chain,database,deployment},
    conditions,
  };
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { console.log(JSON.stringify(await verify(),null,2)); process.exitCode=1; }
  catch { console.log(JSON.stringify({verdict:"FAIL",reason:"VERIFIER_INPUT_INVALID",broadcast:false})); process.exitCode=1; }
}
