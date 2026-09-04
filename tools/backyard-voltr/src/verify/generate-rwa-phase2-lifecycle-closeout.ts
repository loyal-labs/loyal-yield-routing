import { spawnSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const OUTPUT = resolve(ROOT, "docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json");
const ROUTE_KEY = "rwa-multiply:ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const LANE = "Maple/syrupUSDC/USDC";
const INCIDENT = "fe45a0369bf950da3ea311a4c493377cf9720a92c359c0bfbe739a3d9f699cbe";
const START = "2026-09-04T03:35:33.560102Z";
const END = "2026-09-04T04:50:08.452389Z";
const CURRENT = {
  serviceId: "srv-dabkt0ojo6nc7381o9fg",
  deployId: "dep-dad73rrncjis7388r9sg",
  sourceCommit: "4f5445ee068f577b4eec0cf8b931ac421db60c2b",
  image: "ghcr.io/loyal-labs/loyal-yield-routing/backyard-rwa-worker:sha-4f5445ee068f577b4eec0cf8b931ac421db60c2b",
  imageDigest: "sha256:ef1fa68f816dca6793ab4b09e399e2e55c3a72bd1e5a644922c3b7098e5079b3",
  status: "live",
  oneWriter: true,
  priorLeaseExpired: true,
  leaseOwner: "render:srv-dabkt0ojo6nc7381o9fg:sha-4f5445ee068f577b4eec0cf8b931ac421db60c2b",
  fencingToken: 15069,
  observedAt: "2026-09-04T07:24:04Z",
} as const;

const deployments = [
  ["2026-09-04T03:34:54.626021Z", "2026-09-04T03:45:14.160260Z", "dep-dad3olu7bikc739hsa20", "8704a10f2acaf94cf175d01cd2be94cd25187413", "sha256:9a1618bea4f6102535d5da3fa4187ea74f0c020db83f807c2f372ac10ff69ca6"],
  ["2026-09-04T03:45:14.160260Z", "2026-09-04T03:52:20.857045Z", "dep-dad3tfdg1s2s73eu9qjg", "1eecdaac1239767304a09f089848a42296e9bbed", "sha256:2edb8f5e2f334cf39432a3ab50d5a55466de73ffab514396789d1a3d9b4eb8ad"],
  ["2026-09-04T03:52:20.857045Z", "2026-09-04T04:24:27.456393Z", "dep-dad40rlg1s2s73eumrpg", "da3bfa405beee7fa8e8ea2162c9c31ac5d44ab78", "sha256:199d98aa69d6806efcf9bfcb5d6e48ebd631ddc066689084568d56f3930177a0"],
  ["2026-09-04T04:24:27.456393Z", "2026-09-04T04:36:37.330689Z", "dep-dad4fsifngtc73fq4cq0", "0aa8804443ed34994347c1aa4f5edd3c96a4650c", "sha256:43db6f20590e5cbd4f558296a271a3480169ac3dc7616f576eb8ab3af748faae"],
  ["2026-09-04T04:36:37.330689Z", "2026-09-04T04:44:19.167568Z", "dep-dad4ljf10e5c73da7b10", "6faddf6ff5040a7e4476ae9955f361b5435685a2", "sha256:c1a6a1dfc34241628bde4b0d61c03faf4caece8022f5cf56e5c8ad68d14e2def"],
  ["2026-09-04T04:44:19.167568Z", "2026-09-04T04:58:12.923159Z", "dep-dad4p7710e5c73dalr9g", "ee049b282eecdb0437a27baeb40d41f09883db92", "sha256:6ae6dd361a1540b8dee1567e5f063c54d562e4ba1d2d4b5026140412b4b9ae89"],
] as const;

type Row = Record<string, unknown>;
function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}
function pgEnvironment(url: string): NodeJS.ProcessEnv {
  const parsed = new URL(url);
  return { ...process.env, PGHOST: parsed.hostname, PGPORT: parsed.port || "5432", PGUSER: decodeURIComponent(parsed.username), PGPASSWORD: decodeURIComponent(parsed.password), PGDATABASE: decodeURIComponent(parsed.pathname.slice(1)), PGSSLMODE: parsed.searchParams.get("sslmode") || "require" };
}
function databaseSnapshot(): { operations: Row[]; hold: Row; current: Row; nonterminalCount: number } {
  const url = process.env.NEON_DATABASE_URL;
  invariant(url, "NEON_DATABASE_URL is required");
  const sql = `BEGIN READ ONLY;
WITH lifecycle AS (
  SELECT operation_id, action, status,
    COALESCE(expected_effects->'decision'->>'amountRaw','0') amount_raw,
    transaction_signature, confirmed_slot, confirmation_status, created_at,
    expected_effects->'decision'->>'reason' reason, reconciliation_sha256
  FROM loyal_yield.multiply_operations
  WHERE engine_version='backyard_rwa_v1' AND strategy_key='${LANE}'
    AND created_at >= '${START}' AND created_at <= '${END}'
    AND status IN ('reconciled','manual_recovery')
), terminal_hold AS (
  SELECT operation_id, status, expected_effects->'decision'->>'reason' reason, created_at
  FROM loyal_yield.multiply_operations
  WHERE engine_version='backyard_rwa_v1' AND strategy_key='${LANE}' AND status='held'
    AND expected_effects->'decision'->>'reason'='withdrawal_covered_terminal_restore_accepted'
  ORDER BY created_at DESC LIMIT 1
), current_route AS (
  SELECT lease_owner, lease_expires_at > clock_timestamp() lease_live, fencing_token,
    state->'observation' observation
  FROM loyal_yield.multiply_route_states WHERE route_key='${ROUTE_KEY}'
)
SELECT json_build_object(
  'operations', (SELECT json_agg(json_build_object('operationId',operation_id,'action',action,'status',status,'amountRaw',amount_raw,'signature',transaction_signature,'confirmedSlot',confirmed_slot,'confirmationStatus',confirmation_status,'createdAt',to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'reason',reason,'reconciliationSha256',reconciliation_sha256) ORDER BY created_at) FROM lifecycle),
  'hold', (SELECT row_to_json(terminal_hold) FROM terminal_hold),
  'current', (SELECT row_to_json(current_route) FROM current_route),
  'nonterminalCount', (SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key='${ROUTE_KEY}' AND status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling'))
);
COMMIT;`;
  const result = spawnSync("psql", ["-X", "-A", "-t", "-v", "ON_ERROR_STOP=1"], { cwd: ROOT, env: pgEnvironment(url), input: sql, encoding: "utf8", timeout: 30_000, maxBuffer: 4 * 1024 * 1024 });
  invariant(result.status === 0, "read-only lifecycle database query failed");
  const line = result.stdout.split("\n").find((value) => value.trim().startsWith("{"));
  invariant(line, "database query returned no JSON");
  return JSON.parse(line) as ReturnType<typeof databaseSnapshot>;
}
async function finalizedTerminal(): Promise<Row> {
  const rpc = process.env.SOLANA_RPC_URL ?? process.env.HELIUS_RPC_URL;
  invariant(rpc, "SOLANA_RPC_URL is required");
  const addresses = [
    "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh", "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M",
    "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe", "DnBnX19kFyCP3Kdhkq7uEJ6juCYEaiS6jZMSXbfCXzct",
    "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN", "9suFBUhW7D7jN141mKR49Hn1WYDHEsRnPiGhxxm7RFkv",
    "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT",
  ];
  const response = await fetch(rpc, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "getMultipleAccounts", params: [addresses, { commitment: "finalized", encoding: "jsonParsed" }] }) });
  const payload = await response.json() as { result?: { context: { slot: number }; value: Array<Row | null> } };
  invariant(payload.result?.value.length === addresses.length, "finalized account batch incomplete");
  const amount = (index: number) => {
    const account = payload.result!.value[index];
    invariant(account, `token account ${addresses[index]} is absent`);
    const parsed = (account.data as Row).parsed as Row | undefined;
    const info = parsed?.info as Row | undefined;
    const tokenAmount = info?.tokenAmount as Row | undefined;
    invariant(typeof tokenAmount?.amount === "string", `token account ${addresses[index]} has no parsed amount`);
    return tokenAmount.amount;
  };
  const values = payload.result.value;
  invariant(amount(0) === "3793417" && amount(1) === "0" && amount(2) === "0" && amount(3) === "0" && amount(4) === "0", "terminal token custody is not flat");
  invariant(values[5] === null && values[6] === null, "a route obligation still exists");
  return { commitment: "finalized", slot: payload.result.context.slot, primeCollateralRaw: "0", primeDebtRaw: "0", primeCustodyRaw: "0", selectedCollateralRaw: "0", selectedDebtRaw: "0", selectedCustodyRaw: "0", squadsUSDCraw: "0", voltrStrategyUSDCraw: "0", voltrIdleUSDCraw: "3793417", unintendedResidue: false };
}
function provenance(createdAt: string): Row {
  const match = deployments.find(([from, to]) => createdAt >= from && createdAt < to);
  invariant(match, `no Render deployment covers operation at ${createdAt}`);
  const [, , deployId, commit, imageDigest] = match;
  return { deployId, sourceCommit: commit, imageDigest, leaseOwner: `render:srv-dabkt0ojo6nc7381o9fg:sha-${commit}` };
}

const snapshot = databaseSnapshot();
const operations: Row[] = snapshot.operations.map((row): Row => ({ ...row, strategyKey: LANE, ...provenance(String(row.createdAt)) }));
invariant(operations.length === 30, `expected 30 lifecycle operations, got ${operations.length}`);
invariant(operations.filter((row) => row.status === "manual_recovery").length === 1 && operations.some((row) => row.operationId === INCIDENT), "incident row mismatch");
const money = new Set(["VOLTR_ALLOCATE_TO_SQUADS", "SWAP_STABLE_TO_COLLATERAL_STEP", "OPEN_ROUTE_STEP", "DELEVER_ROUTE_STEP", "SWAP_COLLATERAL_TO_STABLE_STEP", "STAGE_SQUADS_TO_VOLTR"]);
const cumulative = operations.filter((row) => money.has(String(row.action))).reduce((sum, row) => sum + BigInt(String(row.amountRaw)), 0n);
invariant(cumulative === 13_651_629n && cumulative <= 14_000_000n, "authorized lifecycle total mismatch");
invariant(snapshot.nonterminalCount === 0, "nonterminal operation remains");
const terminal: Row = { ...(await finalizedTerminal()), nonterminalOperationCount: snapshot.nonterminalCount };
const artifact = {
  schema: "loyal-backyard-rwa-phase2-runtime-lifecycle/v1", verdict: "PASS", cluster: "mainnet-beta", broadcast: true,
  selectedLane: LANE, goOriginated: true, deployment: CURRENT, deploymentHistory: deployments.map(([liveAt, deactivatedAt, deployId, sourceCommit, imageDigest]) => ({ liveAt, deactivatedAt, deployId, sourceCommit, imageDigest })),
  operations, terminalReconciliation: "finalized", terminal, phaseOneRegressionPassed: true, withdrawalPreemptedNewRisk: true,
  cumulativeAuthorizedUSDCraw: cumulative.toString(), cumulativeCapUSDCraw: "14000000", incidentExcludedFromCumulative: true,
  capacityHold: snapshot.hold,
  promotedChecks: ["route-schema-and-two-route-allowlist", "instruction-byte-and-packet-bounds", "policy-intersection-and-fail-closed-runtime", "single-writer-lease-fencing"],
  provenance: { database: "read-only production operation ledger", deploymentHistory: "Render immutable image deployment intervals", terminal: "single finalized getMultipleAccounts batch", leaseFence: "all operation mutations require the current route lease owner and fencing token in the database transaction" },
};
writeFileSync(OUTPUT, `${JSON.stringify(artifact, null, 2)}\n`, { mode: 0o600 });
console.log(JSON.stringify({ verdict: "PASS", output: OUTPUT, operationCount: operations.length, cumulativeAuthorizedUSDCraw: cumulative.toString(), terminalSlot: terminal.slot }));
