import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { AddressLookupTableAccount, PublicKey } from "@solana/web3.js";

const LOOKUP_TABLE_PROGRAM_ID = new PublicKey("AddressLookupTab1e1111111111111111111111111");
const LOOKUP_TABLE_AUTHORITY_OFFSET = 22;
const DEFAULT_RPC_URL = "https://api.mainnet-beta.solana.com";
const DEFAULT_AUTHORITY = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const U64_MAX = 18_446_744_073_709_551_615n;

type Options = {
  authorities: string[];
  recipient?: string;
  authorityKeyEnv: string;
  rpcUrl: string;
  limit: number;
  pageSize: number;
  bundleSize: number;
  execute: boolean;
  simulateBeforeSubmit: boolean;
  traceTiming: boolean;
  verbose: boolean;
  maxPages?: number;
  report?: string;
};

type ProgramAccountsV2Account = {
  pubkey: string;
  account: {
    data: unknown;
  };
};

type ProgramAccountsV2Result = {
  accounts: ProgramAccountsV2Account[];
  paginationKey?: string;
};

type DiscoveredTable = {
  table: string;
  authority: string;
  addressCount: number;
  deactivationSlot: string;
  status: "active" | "deactivated";
};

type CleanupReport = {
  candidates?: Array<{
    action?: string;
    execution?: {
      signature?: string;
      kind?: string;
    } | null;
  }>;
  plannedExecutionCount?: number;
  totalReclaimableLamports?: string;
  totalReclaimedLamports?: string;
};

type JsonObject = Record<string, unknown>;

const options = parseArgs(process.argv.slice(2));
await main(options);

async function main(options: Options): Promise<void> {
  if (options.limit < 1) {
    throw new Error("--limit must be at least 1");
  }
  if (options.pageSize < 1) {
    throw new Error("--page-size must be at least 1");
  }
  if (options.bundleSize < 1) {
    throw new Error("--bundle-size must be at least 1");
  }
  if (options.execute && !options.recipient) {
    if (options.authorities.length !== 1) {
      throw new Error("--recipient is required for --execute with multiple authorities");
    }
    options.recipient = options.authorities[0];
  }

  if (options.report) {
    mkdirSync(dirname(options.report), { recursive: true });
    writeFileSync(options.report, "");
  }

  emit({
    event: "paginated_cleanup_start",
    execute: options.execute,
    rpcUrl: redactedRpcUrl(options.rpcUrl),
    authorities: options.authorities,
    limit: options.limit,
    pageSize: options.pageSize,
    bundleSize: options.bundleSize,
  });

  const discovered = await discoverLookupTables(options);
  const batches = chunk(discovered, options.limit);
  emit({
    event: "paginated_cleanup_discovery_complete",
    discoveredCount: discovered.length,
    activeCount: discovered.filter((table) => table.status === "active").length,
    deactivatedCount: discovered.filter((table) => table.status === "deactivated").length,
    batchCount: batches.length,
  });

  let plannedExecutionCount = 0;
  let totalReclaimedLamports = 0n;
  const signatures = new Set<string>();
  const actionCounts: Record<string, number> = {};

  for (const [batchIndex, batch] of batches.entries()) {
    const report = await runCleanupBatch(options, batch, batchIndex);
    plannedExecutionCount += report.plannedExecutionCount ?? 0;
    totalReclaimedLamports += BigInt(report.totalReclaimedLamports ?? "0");
    for (const candidate of report.candidates ?? []) {
      if (candidate.action) {
        actionCounts[candidate.action] = (actionCounts[candidate.action] ?? 0) + 1;
      }
      const signature = candidate.execution?.signature;
      if (signature) {
        signatures.add(signature);
      }
    }
  }

  emit({
    event: "paginated_cleanup_complete",
    discoveredCount: discovered.length,
    batchCount: batches.length,
    plannedExecutionCount,
    totalReclaimedLamports: totalReclaimedLamports.toString(),
    actionCounts,
    signatures: [...signatures],
  });
}

async function discoverLookupTables(options: Options): Promise<DiscoveredTable[]> {
  const discovered = new Map<string, DiscoveredTable>();

  for (const authority of options.authorities) {
    let paginationKey: string | undefined;
    let pageIndex = 0;

    while (true) {
      if (options.maxPages !== undefined && pageIndex >= options.maxPages) {
        break;
      }

      const config: Record<string, unknown> = {
        encoding: "base64",
        limit: options.pageSize,
        filters: [
          {
            memcmp: {
              offset: LOOKUP_TABLE_AUTHORITY_OFFSET,
              bytes: authority,
            },
          },
        ],
      };
      if (paginationKey) {
        config.paginationKey = paginationKey;
      }

      const result = await rpcCall<ProgramAccountsV2Result>(
        options.rpcUrl,
        "getProgramAccountsV2",
        [LOOKUP_TABLE_PROGRAM_ID.toBase58(), config],
      );

      let matchedCount = 0;
      let decodeSkippedCount = 0;
      let authorityMismatchCount = 0;
      for (const account of result.accounts) {
        const table = decodeLookupTable(account);
        if (!table) {
          decodeSkippedCount += 1;
          continue;
        }
        if (table.authority !== authority) {
          authorityMismatchCount += 1;
          continue;
        }
        matchedCount += 1;
        discovered.set(table.table, table);
      }

      emit({
        event: "paginated_cleanup_page",
        authority,
        pageIndex,
        accountCount: result.accounts.length,
        matchedCount,
        decodeSkippedCount,
        authorityMismatchCount,
        discoveredCount: discovered.size,
        hasNextPage: Boolean(result.paginationKey),
      });

      paginationKey = result.paginationKey;
      pageIndex += 1;
      if (!paginationKey || result.accounts.length === 0) {
        break;
      }
    }
  }

  return [...discovered.values()];
}

function decodeLookupTable(account: ProgramAccountsV2Account): DiscoveredTable | undefined {
  const data = account.account.data;
  if (!Array.isArray(data) || typeof data[0] !== "string") {
    return undefined;
  }
  const encoding = typeof data[1] === "string" ? data[1] : "base64";
  if (encoding !== "base64") {
    throw new Error(`unsupported getProgramAccountsV2 account encoding ${encoding}`);
  }

  let state: ReturnType<typeof AddressLookupTableAccount.deserialize>;
  try {
    state = AddressLookupTableAccount.deserialize(Buffer.from(data[0], "base64"));
  } catch {
    return undefined;
  }
  const authority = state.authority?.toBase58();
  if (!authority) {
    return undefined;
  }
  const deactivationSlot = BigInt(state.deactivationSlot.toString());
  return {
    table: account.pubkey,
    authority,
    addressCount: state.addresses.length,
    deactivationSlot: deactivationSlot.toString(),
    status: deactivationSlot === U64_MAX ? "active" : "deactivated",
  };
}

async function runCleanupBatch(
  options: Options,
  batch: DiscoveredTable[],
  batchIndex: number,
): Promise<CleanupReport> {
  const args = [
    "run",
    "same-mint:alt-cleanup",
    "--",
    ...options.authorities.flatMap((authority) => ["--authority", authority]),
    ...batch.flatMap((table) => ["--table", table.table]),
    options.execute ? "--execute" : "--dry-run",
    "--limit",
    String(options.limit),
    "--authority-key-env",
    options.authorityKeyEnv,
    "--bundle-size",
    String(options.bundleSize),
  ];

  if (options.recipient) {
    args.push("--recipient", options.recipient);
  }
  if (options.simulateBeforeSubmit) {
    args.push("--simulate-before-submit");
  }
  if (options.traceTiming) {
    args.push("--trace-timing");
  }

  emit({
    event: "paginated_cleanup_batch_start",
    batchIndex,
    tableCount: batch.length,
    firstTable: batch[0]?.table,
    lastTable: batch.at(-1)?.table,
  });

  const subprocess = Bun.spawn(["bun", ...args], {
    env: process.env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(subprocess.stdout).text(),
    new Response(subprocess.stderr).text(),
    subprocess.exited,
  ]);

  if (options.verbose && stderr.trim()) {
    process.stderr.write(stderr);
    if (!stderr.endsWith("\n")) {
      process.stderr.write("\n");
    }
  }

  if (exitCode !== 0) {
    process.stderr.write(stderr);
    if (stderr && !stderr.endsWith("\n")) {
      process.stderr.write("\n");
    }
    process.stderr.write(stdout);
    if (!stdout.endsWith("\n")) {
      process.stderr.write("\n");
    }
    throw new Error(`cleanup batch ${batchIndex} failed with exit code ${exitCode}`);
  }

  const report = parseCleanupReport(stdout);
  const actionCounts = countActions(report);
  const signatures = uniqueSignatures(report);
  emit({
    event: "paginated_cleanup_batch_complete",
    batchIndex,
    tableCount: batch.length,
    plannedExecutionCount: report.plannedExecutionCount ?? 0,
    totalReclaimableLamports: report.totalReclaimableLamports ?? "0",
    totalReclaimedLamports: report.totalReclaimedLamports ?? "0",
    actionCounts,
    signatures,
  });
  return report;
}

function parseCleanupReport(rawOutput: string): CleanupReport {
  const start = rawOutput.indexOf("{");
  const end = rawOutput.lastIndexOf("}");
  if (start === -1 || end === -1 || end <= start) {
    throw new Error("cleanup command did not emit a JSON object on stdout");
  }
  return JSON.parse(rawOutput.slice(start, end + 1)) as CleanupReport;
}

async function rpcCall<T>(rpcUrl: string, method: string, params: unknown[]): Promise<T> {
  const response = await fetch(rpcUrl, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: "route-lookup-table-cleanup-paginated",
      method,
      params,
    }),
  });
  if (!response.ok) {
    throw new Error(`${method} returned HTTP ${response.status}: ${await response.text()}`);
  }
  const body = (await response.json()) as {
    result?: T;
    error?: { code: number; message: string };
  };
  if (body.error) {
    throw new Error(`${method} returned ${body.error.code}: ${body.error.message}`);
  }
  if (body.result === undefined) {
    throw new Error(`${method} response did not include result`);
  }
  return body.result;
}

function parseArgs(values: string[]): Options {
  const options: Options = {
    authorities: [],
    authorityKeyEnv: "POLICY_KEYPAIR",
    rpcUrl: process.env.SOLANA_RPC_URL ?? DEFAULT_RPC_URL,
    limit: 25,
    pageSize: 1000,
    bundleSize: 25,
    execute: false,
    simulateBeforeSubmit: false,
    traceTiming: false,
    verbose: false,
  };

  for (let index = 0; index < values.length; index += 1) {
    const value = values[index];
    switch (value) {
      case "--authority":
        options.authorities.push(requireValue(values, ++index, value));
        break;
      case "--recipient":
        options.recipient = requireValue(values, ++index, value);
        break;
      case "--authority-key-env":
        options.authorityKeyEnv = requireValue(values, ++index, value);
        break;
      case "--rpc-url":
        options.rpcUrl = requireValue(values, ++index, value);
        break;
      case "--limit":
        options.limit = parsePositiveInteger(requireValue(values, ++index, value), value);
        break;
      case "--page-size":
        options.pageSize = parsePositiveInteger(requireValue(values, ++index, value), value);
        break;
      case "--bundle-size":
        options.bundleSize = parsePositiveInteger(requireValue(values, ++index, value), value);
        break;
      case "--max-pages":
        options.maxPages = parsePositiveInteger(requireValue(values, ++index, value), value);
        break;
      case "--report":
        options.report = requireValue(values, ++index, value);
        break;
      case "--execute":
        options.execute = true;
        break;
      case "--dry-run":
        options.execute = false;
        break;
      case "--simulate-before-submit":
        options.simulateBeforeSubmit = true;
        break;
      case "--trace-timing":
        options.traceTiming = true;
        break;
      case "--verbose":
        options.verbose = true;
        break;
      case "--help":
      case "-h":
        printUsage();
        process.exit(0);
      default:
        throw new Error(`unknown argument: ${value}`);
    }
  }

  if (options.authorities.length === 0) {
    options.authorities.push(DEFAULT_AUTHORITY);
  }
  options.authorities = [...new Set(options.authorities.map((authority) => new PublicKey(authority).toBase58()))];
  if (options.recipient) {
    options.recipient = new PublicKey(options.recipient).toBase58();
  }
  return options;
}

function requireValue(values: string[], index: number, flag: string): string {
  const value = values[index];
  if (!value) {
    throw new Error(`${flag} requires a value`);
  }
  return value;
}

function parsePositiveInteger(value: string, flag: string): number {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isSafeInteger(parsed) || parsed < 1) {
    throw new Error(`${flag} must be a positive integer`);
  }
  return parsed;
}

function countActions(report: CleanupReport): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const candidate of report.candidates ?? []) {
    if (candidate.action) {
      counts[candidate.action] = (counts[candidate.action] ?? 0) + 1;
    }
  }
  return counts;
}

function uniqueSignatures(report: CleanupReport): string[] {
  const signatures = new Set<string>();
  for (const candidate of report.candidates ?? []) {
    const signature = candidate.execution?.signature;
    if (signature) {
      signatures.add(signature);
    }
  }
  return [...signatures];
}

function chunk<T>(values: T[], size: number): T[][] {
  const chunks: T[][] = [];
  for (let index = 0; index < values.length; index += size) {
    chunks.push(values.slice(index, index + size));
  }
  return chunks;
}

function emit(value: JsonObject): void {
  const line = `${JSON.stringify(value)}\n`;
  process.stdout.write(line);
  if (options.report) {
    appendFileSync(options.report, line);
  }
}

function redactedRpcUrl(rpcUrl: string): string {
  const [prefix, query] = rpcUrl.split("?");
  return query === undefined ? rpcUrl : `${prefix}?<redacted>`;
}

function printUsage(): void {
  console.log(`Usage: bun run same-mint:alt-cleanup-paginated -- [options]

Discovers all address lookup tables for one or more authorities using
paginated getProgramAccountsV2, then feeds explicit --table batches into
same-mint:alt-cleanup.

Options:
  --authority <PUBKEY>          Repeatable. Defaults to ${DEFAULT_AUTHORITY}.
  --recipient <PUBKEY>          Close recipient. Defaults to the sole authority in --execute mode.
  --authority-key-env <NAME>    Signing env var. Defaults to POLICY_KEYPAIR.
  --rpc-url <URL>               Defaults to SOLANA_RPC_URL, then mainnet RPC.
  --limit <N>                   Tables per cleanup batch. Defaults to 25.
  --page-size <N>               RPC accounts per getProgramAccountsV2 page. Defaults to 1000.
  --bundle-size <N>             Cleanup instructions per transaction. Defaults to 25.
  --execute                     Execute cleanup. Default is dry-run.
  --dry-run                     Do not execute cleanup.
  --simulate-before-submit      Ask the cleanup binary to simulate signed txs before submit.
  --trace-timing                Forward trace timing to cleanup child runs.
  --verbose                     Print child cleanup command stderr.
  --max-pages <N>               Stop discovery after N RPC pages.
  --report <PATH>               Also write JSONL progress to PATH.
`);
}
