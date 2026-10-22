import { type GoServedCall, goServedMobileCalls } from "./routes-census";
import { mintMobileSession, register, scrub, walletKeyFromEnv } from "./wallet-session";

const CONTRACT = "earn-max-v4";
const TEST_SETTINGS_PDA = "EEA8aso4Vky11KYsL66jsBujUW8quseSr9GinxAPpmvw";

const HELP = `Usage: bun --tsconfig-override web/apps/mobile/tsconfig.json scripts/web/mobile-go-check.ts [flags]

Mobile against Go, with the test wallet. Read-only: nothing is signed or sent.
  1. mint   POST /api/smart-accounts/mobile/earn/session, signed by POLICY_KEYPAIR
  2. preflight  every mobile client path the routes census lists as Go-served (needs the Go
                toolchain) answers with X-Loyal-API-Source: go and not 404; POSTs send {} and are refused
  3. read   GET Earn MAX summary, activity and invite with the bearer
  4. prepare  prepareEarnMaxVoltr builds a Voltr deposit (the app's own call)
  5. simulate  simulateTransaction on the unsigned deposit (sigVerify off)
Each read asserts HTTP 200, x-loyal-earn-max-contract: ${CONTRACT} and X-Loyal-API-Source: go.

Flags
  --base <origin>      server under test (default https://next.askloyal.com)
  --rpc-url <url>      Solana RPC for prepare and simulate (default: SOLANA_RPC_URL)
  --amount-raw <n>     deposit in USDC raw units, 6 decimals (default 1000000 = 1 USDC,
                       the app's own minimum; the Voltr vault code sets none)
  --settings-pda <pk>  expected smart-account settings PDA (default ${TEST_SETTINGS_PDA})
  --help

Environment
  POLICY_KEYPAIR       test wallet key; run under: op run --env-file=.env.1password -- ...
  Needs: cd web && bun install --frozen-lockfile
`;

type Step = { name: string; ok: boolean; detail: string };
const steps: Step[] = [];
const report = (name: string, ok: boolean, detail: string) => {
  steps.push({ name, ok, detail });
  console.log(scrub(`${ok ? "PASS" : "FAIL"} ${name}: ${detail}`));
  return ok;
};

function parse(argv: string[]) {
  const flags: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (arg === "--help" || arg === "-h") flags.help = "1";
    else if (arg.startsWith("--")) flags[arg.slice(2)] = argv[++i] ?? "";
    else throw new Error(`Unexpected argument ${arg}. See --help.`);
  }
  for (const key of Object.keys(flags)) {
    if (!["help", "base", "rpc-url", "amount-raw", "settings-pda"].includes(key)) {
      throw new Error(`Unknown flag --${key}. See --help.`);
    }
  }
  return flags;
}

async function readJson(base: string, path: string, bearer: string) {
  const response = await fetch(new URL(path, base), {
    redirect: "error",
    signal: AbortSignal.timeout(20_000),
    headers: { Authorization: `Bearer ${bearer}` },
  });
  const body = (await response.json().catch(() => null)) as any;
  return { response, body };
}

const SAMPLE_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

export async function preflight(base: string, bearer: string, calls: GoServedCall[]): Promise<string[]> {
  const failures: string[] = [];
  for (const call of calls) {
    const label = `${call.method} ${call.path} (${call.at})`;
    try {
      const response = await fetch(new URL(call.path.replaceAll("{}", SAMPLE_MINT), base), {
        method: call.method,
        redirect: "error",
        signal: AbortSignal.timeout(20_000),
        headers: { Authorization: `Bearer ${bearer}`, "Content-Type": "application/json" },
        body: call.method === "GET" ? undefined : "{}",
      });
      const source = response.headers.get("X-Loyal-API-Source");
      if (response.status === 404) failures.push(`${label} answered 404: its Go route is not registered (credential or dependency missing)`);
      else if (source !== "go") failures.push(`${label} was not served by Go (X-Loyal-API-Source is ${source ?? "absent"}, HTTP ${response.status}): it proxies to Next`);
    } catch (error) {
      failures.push(`${label} failed: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  return failures;
}

function readStep(name: string, path: string, res: Awaited<ReturnType<typeof readJson>>, extra: (body: any) => string | null) {
  const { response, body } = res;
  if (!response.ok) {
    const code = typeof body?.error?.code === "string" ? ` ${body.error.code}` : "";
    return report(name, false, `GET ${path} answered HTTP ${response.status}${code}`);
  }
  if (response.headers.get("X-Loyal-API-Source") !== "go") {
    return report(name, false, `GET ${path} was not served by Go (X-Loyal-API-Source is ${response.headers.get("X-Loyal-API-Source") ?? "absent"})`);
  }
  const contract = response.headers.get("x-loyal-earn-max-contract");
  if (contract !== CONTRACT) return report(name, false, `x-loyal-earn-max-contract is ${contract ?? "absent"}, need ${CONTRACT}`);
  const problem = extra(body);
  return problem ? report(name, false, problem) : report(name, true, `GET ${path} 200, served by Go, ${CONTRACT}`);
}

async function main(): Promise<number> {
  const flags = parse(process.argv.slice(2));
  if (flags.help) {
    console.log(HELP);
    return 0;
  }
  const base = flags.base || "https://next.askloyal.com";
  const rpcUrl = flags["rpc-url"] || process.env.SOLANA_RPC_URL;
  if (!rpcUrl) throw new Error("Pass --rpc-url (or set SOLANA_RPC_URL). See --help.");
  register(rpcUrl);
  const expectedSettings = flags["settings-pda"] || TEST_SETTINGS_PDA;
  const amountRaw = BigInt(flags["amount-raw"] || "1000000");
  if (amountRaw <= 0n) throw new Error("--amount-raw must be positive");

  const key = walletKeyFromEnv();
  let session: { token: string; expiresAt: string };
  try {
    session = await mintMobileSession(base, key);
  } catch (error) {
    report("mint", false, error instanceof Error ? error.message : String(error));
    return 1;
  }
  report("mint", true, `session for ${key.address} minted at ${base}; expires ${session.expiresAt}`);

  const calls = goServedMobileCalls();
  const refused = await preflight(base, session.token, calls);
  for (const problem of refused) report("preflight", false, problem);
  if (refused.length > 0) return 1;
  report("preflight", true, `${calls.length} Go-served mobile calls answered by Go at ${base}`);

  const summaryPath = "/api/smart-accounts/earn-max/summary";
  const summary = await readJson(base, summaryPath, session.token);
  const summaryOk = readStep("summary", summaryPath, summary, (body) => {
    if (typeof body?.config?.programId !== "string") return "summary has no config.programId";
    if (body.config.walletAddress !== undefined && body.config.walletAddress !== key.address) {
      return "summary answered for a different wallet";
    }
    if (body.config.settingsPda !== undefined && body.config.settingsPda !== expectedSettings) {
      return `summary settingsPda is ${body.config.settingsPda}, expected ${expectedSettings}`;
    }
    return null;
  });
  const activityPath = "/api/smart-accounts/earn-max/activity";
  readStep("activity", activityPath, await readJson(base, activityPath, session.token), (body) =>
    Array.isArray(body?.operations) ? null : "activity has no operations array",
  );
  const invitePath = "/api/smart-accounts/earn-max/invite";
  readStep("invite", invitePath, await readJson(base, invitePath, session.token), (body) =>
    body?.redeemed === true ? null : "the test wallet has not redeemed an Earn MAX invite",
  );
  if (!summaryOk) return 1;

  let sdk: typeof import("@loyal-labs/smart-account-vaults");
  let web3: typeof import("../../web/node_modules/@solana/web3.js");
  try {
    sdk = await import("@loyal-labs/smart-account-vaults");
    web3 = await import("../../web/node_modules/@solana/web3.js");
  } catch (error) {
    throw new Error(
      `Cannot load the SDK (${error instanceof Error ? error.message : error}). Run: cd web && bun install --frozen-lockfile, then bun --tsconfig-override web/apps/mobile/tsconfig.json scripts/web/mobile-go-check.ts`,
    );
  }
  const { Connection, PublicKey, TransactionMessage, VersionedTransaction } = web3;
  const connection = new Connection(rpcUrl, "confirmed");
  const programId = new PublicKey(summary.body.config.programId);
  const settingsPda = new PublicKey(summary.body.config.settingsPda ?? expectedSettings);
  const owner = new PublicKey(key.address);

  let prepared: Awaited<ReturnType<typeof sdk.prepareEarnMaxVoltr>>;
  try {
    prepared = await sdk.prepareEarnMaxVoltr({
      build: async (context) => sdk.voltrDepositPlan(context, amountRaw),
      client: sdk.createSmartAccountVaultsClient({ connection, programId }),
      owner,
      programId,
      settingsPda,
    });
  } catch (error) {
    report("prepare", false, `prepareEarnMaxVoltr threw: ${error instanceof Error ? error.message : String(error)}`);
    return 1;
  }
  report("prepare", true, `deposit of ${amountRaw} raw built: ${prepared.instructions.length} instructions, settings ${settingsPda.toBase58()}`);

  const { blockhash } = await connection.getLatestBlockhash("confirmed");
  const transaction = new VersionedTransaction(
    new TransactionMessage({
      payerKey: prepared.payer,
      recentBlockhash: blockhash,
      instructions: [...prepared.instructions],
    }).compileToV0Message([...(prepared.lookupTableAccounts ?? [])]),
  );
  const simulation = await connection.simulateTransaction(transaction, {
    sigVerify: false,
    replaceRecentBlockhash: true,
    commitment: "confirmed",
  });
  if (simulation.value.err) {
    const logs = (simulation.value.logs ?? []).slice(-4).join(" | ");
    report("simulate", false, `simulateTransaction failed: ${JSON.stringify(simulation.value.err)}; last logs: ${logs}`);
    return 1;
  }
  report("simulate", true, `simulates OK, ${simulation.value.unitsConsumed ?? "?"} compute units, ${transaction.serialize().length} bytes, unsigned and unsent`);
  return steps.every((step) => step.ok) ? 0 : 1;
}

if (import.meta.main) process.exitCode = await main()
  .catch((error) => {
    console.error(scrub(error instanceof Error ? error.message : String(error)));
    return 1;
  })
  .then((code) => {
    if (steps.length) console.log(`${steps.filter((s) => s.ok).length} passed, ${steps.filter((s) => !s.ok).length} failed`);
    return code;
  });
