import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey } from "@solana/web3.js";

import { PARTNER_ROUTE, routeSpecSha256 } from "../domain/route-spec.js";
import type { Gate } from "./current.js";

type SettingsState = Readonly<{
  threshold: number;
  timeLock: number;
  policySeed: { toString(): string } | null;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
}>;

type PolicyState = Readonly<{
  settings: PublicKey;
  seed: { toString(): string };
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  policyState: Readonly<{
    __kind: string;
    fields?: readonly Readonly<{
      accountIndex?: number;
      instructionsConstraints?: readonly Readonly<{ programId: PublicKey }>[];
    }>[];
  }>;
}>;

type Web3Account = NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>;
const Settings = (squadsGenerated as unknown as { Settings: { fromAccountInfo(account: Web3Account): readonly [SettingsState, number] } }).Settings;
const Policy = (squadsGenerated as unknown as { Policy: { fromAccountInfo(account: Web3Account): readonly [PolicyState, number] } }).Policy;

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function policyAddress(seed: bigint): PublicKey {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(), seedBytes],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0];
}

function decodeSettings(account: Web3Account | null): SettingsState | null {
  try {
    return account?.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))
      ? Settings.fromAccountInfo(account)[0]
      : null;
  } catch {
    return null;
  }
}

/**
 * The Squads Settings account is shared with Loyal's other policy-bound routes,
 * so its monotonically increasing policy seed is not a Voltr ownership lock.
 * Prove instead that every live policy outside the exact Voltr catalog cannot
 * authorize a direct Voltr instruction. Missing/closed policy PDAs are safe.
 */
export async function verifyNonCatalogSquadsPoliciesIsolated(
  rpcUrl: string,
  catalogFirstSeed = 17n,
  catalogLastSeed = 24n,
  minContextSlot?: number,
  commitment: "confirmed" | "finalized" = "confirmed",
  additionalCatalogRanges: readonly Readonly<{ firstSeed: bigint; lastSeed: bigint }>[] = [],
  requireCompleteCatalog = true,
) {
  if (catalogFirstSeed <= 0n || catalogLastSeed < catalogFirstSeed) {
    throw new Error("invalid Squads catalog seed range");
  }
  if (minContextSlot !== undefined && (!Number.isSafeInteger(minContextSlot) || minContextSlot < 0)) {
    throw new Error(`Squads isolation minimum context slot must be a non-negative safe integer: ${minContextSlot}`);
  }
  const connection = new Connection(rpcUrl, commitment);
  const settingsAddress = new PublicKey(PARTNER_ROUTE.squads.settings);
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const first = await connection.getAccountInfoAndContext(settingsAddress, {
      commitment,
      ...(minContextSlot === undefined ? {} : { minContextSlot }),
    });
    const firstSettings = decodeSettings(first.value);
    const currentSeed = BigInt(firstSettings?.policySeed?.toString() ?? "0");
    const allowedRanges = [{ firstSeed: catalogFirstSeed, lastSeed: catalogLastSeed }, ...additionalCatalogRanges];
    if (allowedRanges.some(({ firstSeed, lastSeed }) => firstSeed <= 0n || lastSeed < firstSeed)) {
      throw new Error("invalid additional Squads catalog seed range");
    }
    const nonCatalogSeeds = currentSeed >= 1n
      ? Array.from({ length: Number(currentSeed) }, (_, index) => BigInt(index + 1))
        .filter((seed) => !allowedRanges.some(({ firstSeed, lastSeed }) => seed >= firstSeed && seed <= lastSeed))
      : [];
    const accounts = new Map<bigint, Web3Account | null>();
    let contextSlot = first.context.slot;
    for (let offset = 0; offset < nonCatalogSeeds.length; offset += 90) {
      const seeds = nonCatalogSeeds.slice(offset, offset + 90);
      const response = await connection.getMultipleAccountsInfoAndContext(seeds.map(policyAddress), {
        commitment,
        minContextSlot: contextSlot,
      });
      contextSlot = Math.max(contextSlot, response.context.slot);
      seeds.forEach((seed, index) => accounts.set(seed, response.value[index] ?? null));
    }
    const final = await connection.getAccountInfoAndContext(settingsAddress, {
      commitment,
      minContextSlot: contextSlot,
    });
    const finalSettings = decodeSettings(final.value);
    const finalSeed = BigInt(finalSettings?.policySeed?.toString() ?? "0");
    if (finalSeed !== currentSeed) {
      if (attempt === 4) throw new Error(`Squads policy seed changed during isolation scan: ${currentSeed} -> ${finalSeed}`);
      continue;
    }

    const gates: Gate[] = [];
    add(gates, "shared Settings owner and decoder", firstSettings !== null && finalSettings !== null, first.value?.owner.toBase58() ?? null, PARTNER_ROUTE.squads.program);
    const settingsSigners = finalSettings?.signers.map((signer) => ({ address: signer.key.toBase58(), permissionsMask: signer.permissions.mask })) ?? [];
    add(gates, "shared Settings admin boundary", finalSettings?.threshold === 1 && finalSettings.timeLock === 0 && settingsSigners.length === 1 && settingsSigners[0]?.address === PARTNER_ROUTE.setupAdmin && settingsSigners[0].permissionsMask === 7, finalSettings ? { threshold: finalSettings.threshold, timeLock: finalSettings.timeLock, signers: settingsSigners } : null, { threshold: 1, timeLock: 0, signers: [{ address: PARTNER_ROUTE.setupAdmin, permissionsMask: 7 }] });
    add(gates, requireCompleteCatalog ? "shared Settings includes the complete Voltr catalog" : "shared Settings has not advanced beyond the installing catalog", requireCompleteCatalog ? currentSeed >= catalogLastSeed : currentSeed <= catalogLastSeed, currentSeed, requireCompleteCatalog ? `>=${catalogLastSeed}` : `<=${catalogLastSeed}`);
    add(gates, "shared Settings policy seed stable during isolation scan", finalSeed === currentSeed, { before: currentSeed, after: finalSeed }, "unchanged");
    const policies = nonCatalogSeeds.map((seed) => {
      const account = accounts.get(seed) ?? null;
      if (account === null) {
        add(gates, `non-catalog policy ${seed} is absent or isolated from Voltr`, true, null, "absent or constrained away from Voltr");
        return { seed, policy: policyAddress(seed).toBase58(), exists: false, dataSha256: null, programs: [] as string[] };
      }
      let decoded: PolicyState | null = null;
      try {
        if (account.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))) decoded = Policy.fromAccountInfo(account)[0];
      } catch {
        decoded = null;
      }
      const graphs = decoded?.policyState.fields ?? [];
      const programs = graphs.flatMap((graph) => graph.instructionsConstraints?.map(({ programId }) => programId.toBase58()) ?? []);
      const isolated = decoded?.policyState.__kind === "ProgramInteraction"
        && decoded.settings.equals(settingsAddress)
        && BigInt(decoded.seed.toString()) === seed
        && policyAddress(seed).equals(policyAddress(BigInt(decoded.seed.toString())))
        && decoded.threshold === 1
        && decoded.timeLock === 0
        && decoded.signers.length > 0
        && decoded.signers.every((signer) => signer.permissions.mask > 0 && !signer.key.equals(new PublicKey(PARTNER_ROUTE.squads.guardian)))
        && graphs.length > 0
        && graphs.every((graph) => (graph.instructionsConstraints?.length ?? 0) > 0)
        && !programs.includes(PARTNER_ROUTE.programs.voltrVault);
      add(
        gates,
        `non-catalog policy ${seed} is absent or isolated from Voltr`,
        isolated,
        decoded ? { owner: account.owner.toBase58(), settings: decoded.settings.toBase58(), seed: decoded.seed.toString(), threshold: decoded.threshold, timeLock: decoded.timeLock, signers: decoded.signers.map((signer) => ({ address: signer.key.toBase58(), permissionsMask: signer.permissions.mask })), kind: decoded.policyState.__kind, programs } : { owner: account.owner.toBase58(), decoded: false },
        { owner: PARTNER_ROUTE.squads.program, settings: PARTNER_ROUTE.squads.settings, seed, threshold: 1, timeLock: 0, signer: `not ${PARTNER_ROUTE.squads.guardian}`, kind: "ProgramInteraction", excludesProgram: PARTNER_ROUTE.programs.voltrVault },
      );
      return {
        seed,
        policy: policyAddress(seed).toBase58(),
        exists: true,
        dataSha256: createHash("sha256").update(account.data).digest("hex"),
        programs,
      };
    });
    const failedGateCount = gates.filter(({ pass }) => !pass).length;
    return {
      verdict: failedGateCount === 0 ? "PARTNER_NON_CATALOG_SQUADS_ISOLATION_PASS" : "PARTNER_NON_CATALOG_SQUADS_ISOLATION_FAIL",
      broadcast: false,
      commitment,
      contextSlot: Math.max(contextSlot, final.context.slot),
      currentSeed,
      catalogFirstSeed,
      catalogLastSeed,
      policies,
      failedGateCount,
      gates,
    } as const;
  }
  throw new Error("unreachable Squads isolation retry state");
}

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const LEGACY_POLICY_ARTIFACT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v1.json");
const LEGACY_POLICY_ARTIFACT_FILE_SHA256 = "09179904c9a7eb9f8cecf022c3fa035993efc63d712345f1033ab94a5cfe4afc";
const LEGACY_POLICY_ARTIFACT_SHA256 = "32d8da34831dc6c609c1d52475f01b32296d3bfce047f7ecde0f767d6e2659e5";
const LEGACY_VOLTR_POLICIES = [
  { seed: 17n, policy: "7HyFp3UepFCxQsPwUgsZ2pcPH23PQma77ofPJpBA8zWV", originSignature: "4Mq12JSDVUDEnxYJcy9mSBMsrMajtkd6UbFW2LcrrCfQrre3VGFaYc2jWd4siCBNuTJRXH7NwGwnp9x2CgHa63p3", createDataSha256: "ad753d89fd635e5c9a986ce7be7dcde4cc4102317da1fdd20168a4457693946c" },
  { seed: 18n, policy: "BwqBwUZEF5kuwTWXAL7ot5edBG53Q54Rc4quvcAkwqXn", originSignature: "51R7RX6fJndysPuJ48X8GfhjwPWPGXMEThp6tQRog7bHFLFp15QhuG7msmoVs6cPBaGBusNBjumnK6ELeKyhdKwj", createDataSha256: "d9306eb42c634d28e5c4f3606aa9115be1c78c678e06e7541f51f7c99cd3fdff" },
  { seed: 19n, policy: "E1tzGGnMT3sxnV1UQo3ZtGXAU1Kjc4p1vBGvF8yAWy9h", originSignature: "5PmSiQR8UK8a9avhCRPrcgzKh3YLk2b5xjntvotpUuak56cgXU9MP67cdyvj4cGcdoZFY7rc4xAxpEU3aoighbzC", createDataSha256: "94d98d3cf196b72b9d946b333b1336fca4a8aae6b36dd55839ec58d448463638" },
  { seed: 20n, policy: "FtXWYrvg3zJx2qi1LU4NgcERhuKAcim9H711PAaKeHA6", originSignature: "3ALas5ErCaAjnt5DHwu4NqikYh4WZgrM9E2uCSjrVzrg2ATVe22m1htu4pdhLeiuW5EPzGwcAFCo1AyU7h4TvC3z", createDataSha256: "0da65e8d955caaa030b1b7db45f8f818ed0f6023c6d094c4e11a8f22b20bd3fa" },
  { seed: 21n, policy: "9CHuKjLbGs1WkX1H6TAwZFBfv9YuxqcR6EMRF6Yh4fqk", originSignature: "3x2dyopwf2yeRetEy3KpDhqfhn8e2nmngQctWqULkLPmxmnFyXykZySSvDFG6LPQTt5JBc9PAJ2wtK1ypz3fU2FW", createDataSha256: "301c7786c1217a5e20e282140519e312e831a80197f1da8da09534b180490a47" },
  { seed: 22n, policy: "3XwSrs1b9xN3aXXumd5RFPftGVnEtkPGLQTp4PHQXaLb", originSignature: "2FZUD4UsTQWqo1wmy1wsCVu5nJwmNeqNUNE3TXECBhaQdbUBGX5Ajrt68ZzSD9rrFGnvQtgiBNPMYUFWTRU1wmRK", createDataSha256: "5539c45987b8a43fdd271c3865626f4cf87b4fe9bdde528dc3483a176139d1fd" },
  { seed: 23n, policy: "5mHpGWE9G2Dn9dyoEcBqYfLRUdw14uTaDE3GCXGmLxk9", originSignature: "5xdyxCLPCxA1mztX1W3atjdi99wS54vg3QSrypiMrqDzRAA1LATK9bExmErGYUpyUUmc1jNueedYAsSNeTPHABZr", createDataSha256: "5d8f428575c61a1709cbab308770c6fea75cfd7099d1825c7a9d49ae8fd25efb" },
  { seed: 24n, policy: "FABT2KhKHcVoUZ1VJsENBnzJmDKEZDZdTqKGTAPkV81i", originSignature: "4y1Zvqvnd9TDxRdtjYA3XsgPjXvsDm6UzfsirWXxavVSCJ2GhdMfs2pBRqtz9WkD4DyhPWEk6nubppRUeicCwFrs", createDataSha256: "3d84ad07363b6c0ed3fa0eb525f0ee67775dd672ca04a24b14277f2078b01499" },
] as const;

/**
 * Policies 17..24 are immutable, already-used one-raw POC policies. Squads has
 * no policy-close instruction, so classify their exact creation origins rather
 * than pretending they can be deleted or are outside the Voltr surface.
 */
export async function verifyLegacyVoltrPolicyCatalog(
  rpcUrl: string,
  minContextSlot?: number,
  commitment: "confirmed" | "finalized" = "confirmed",
) {
  const connection = new Connection(rpcUrl, commitment);
  const gates: Gate[] = [];
  const artifactBytes = readFileSync(LEGACY_POLICY_ARTIFACT);
  const artifactFileSha256 = createHash("sha256").update(artifactBytes).digest("hex");
  let artifactSha256: string | null = null;
  try { artifactSha256 = String((JSON.parse(artifactBytes.toString("utf8")) as { artifactSha256?: unknown }).artifactSha256 ?? ""); } catch { artifactSha256 = null; }
  add(gates, "legacy catalog file hash exact", artifactFileSha256 === LEGACY_POLICY_ARTIFACT_FILE_SHA256, artifactFileSha256, LEGACY_POLICY_ARTIFACT_FILE_SHA256);
  add(gates, "legacy catalog semantic hash exact", artifactSha256 === LEGACY_POLICY_ARTIFACT_SHA256, artifactSha256, LEGACY_POLICY_ARTIFACT_SHA256);
  const response = await connection.getMultipleAccountsInfoAndContext(LEGACY_VOLTR_POLICIES.map(({ policy }) => new PublicKey(policy)), {
    commitment,
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  });
  const policies = [];
  for (const [index, expected] of LEGACY_VOLTR_POLICIES.entries()) {
    const account = response.value[index] ?? null;
    let decoded: PolicyState | null = null;
    try { if (account?.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))) decoded = Policy.fromAccountInfo(account)[0]; } catch { decoded = null; }
    const programs = decoded?.policyState.fields?.flatMap((graph) => graph.instructionsConstraints?.map(({ programId }) => programId.toBase58()) ?? []) ?? [];
    const currentExact = account !== null
      && decoded !== null
      && decoded.settings.equals(new PublicKey(PARTNER_ROUTE.squads.settings))
      && BigInt(decoded.seed.toString()) === expected.seed
      && decoded.threshold === 1
      && decoded.timeLock === 0
      && decoded.signers.length === 1
      && decoded.signers[0]!.key.equals(new PublicKey(PARTNER_ROUTE.squads.guardian))
      && decoded.signers[0]!.permissions.mask === 7
      && decoded.policyState.__kind === "ProgramInteraction"
      && programs.includes(PARTNER_ROUTE.programs.voltrVault);
    add(gates, `legacy policy ${expected.seed} account and guardian boundary exact`, currentExact, decoded ? { policy: expected.policy, seed: decoded.seed.toString(), threshold: decoded.threshold, timeLock: decoded.timeLock, signers: decoded.signers.map((signer) => ({ address: signer.key.toBase58(), permissionsMask: signer.permissions.mask })), kind: decoded.policyState.__kind, programs } : null, { policy: expected.policy, seed: expected.seed, guardian: PARTNER_ROUTE.squads.guardian, permissionsMask: 7, kind: "ProgramInteraction", includesProgram: PARTNER_ROUTE.programs.voltrVault });
    const origin = await connection.getTransaction(expected.originSignature, { commitment, maxSupportedTransactionVersion: 0 });
    const message = origin?.transaction.message;
    const keys = message?.staticAccountKeys.map((key) => key.toBase58()) ?? [];
    const instruction = message?.compiledInstructions[0];
    const instructionAccounts = instruction ? [...instruction.accountKeyIndexes].map((accountIndex) => keys[accountIndex] ?? "<missing>") : [];
    const expectedAccounts = [PARTNER_ROUTE.squads.settings, PARTNER_ROUTE.setupAdmin, PARTNER_ROUTE.programs.system, PARTNER_ROUTE.squads.program, PARTNER_ROUTE.setupAdmin, expected.policy];
    const originExact = origin !== null
      && origin.meta?.err === null
      && origin.transaction.signatures.length === 1
      && message?.header.numRequiredSignatures === 1
      && keys[0] === PARTNER_ROUTE.setupAdmin
      && message.compiledInstructions.length === 1
      && instruction !== undefined
      && keys[instruction.programIdIndex] === PARTNER_ROUTE.squads.program
      && createHash("sha256").update(instruction.data).digest("hex") === expected.createDataSha256
      && instructionAccounts.join(",") === expectedAccounts.join(",");
    add(gates, `legacy policy ${expected.seed} immutable creation origin exact`, originExact, origin ? { signature: expected.originSignature, slot: origin.slot, signer: keys[0] ?? null, programId: instruction ? keys[instruction.programIdIndex] ?? null : null, dataSha256: instruction ? createHash("sha256").update(instruction.data).digest("hex") : null, accounts: instructionAccounts } : null, { signature: expected.originSignature, signer: PARTNER_ROUTE.setupAdmin, programId: PARTNER_ROUTE.squads.program, dataSha256: expected.createDataSha256, accounts: expectedAccounts });
    policies.push({ ...expected, exists: account !== null, programs, originSlot: origin?.slot ?? null });
  }
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { verdict: failedGateCount === 0 ? "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS" : "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_FAIL", broadcast: false, commitment, contextSlot: response.context.slot, artifactFileSha256, artifactSha256, policies, failedGateCount, gates } as const;
}

export async function verifyPrecreatedSquadsIsolation(
  rpcUrl: string,
  expectedCurrentSeed: bigint = PARTNER_ROUTE.squads.policySeedBefore,
  minContextSlot?: number,
  commitment: "confirmed" | "finalized" = "finalized",
) {
  const connection = new Connection(rpcUrl, commitment);
  const policySeeds = Array.from({ length: Number(PARTNER_ROUTE.squads.policySeedBefore) }, (_, index) => BigInt(index + 1));
  const addresses = [new PublicKey(PARTNER_ROUTE.squads.settings), ...policySeeds.map(policyAddress)];
  const response = await connection.getMultipleAccountsInfoAndContext(addresses, {
    commitment,
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  });
  const gates: Gate[] = [];
  const settingsAccount = response.value[0] ?? null;
  let settings: SettingsState | null = null;
  try {
    if (settingsAccount && settingsAccount.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))) settings = Settings.fromAccountInfo(settingsAccount)[0];
  } catch {
    settings = null;
  }
  const signers = settings?.signers.map((signer) => ({ address: signer.key.toBase58(), permissionsMask: signer.permissions.mask })) ?? [];
  add(gates, "pre-created Settings owner and decoder", settings !== null, settingsAccount?.owner.toBase58() ?? null, PARTNER_ROUTE.squads.program);
  add(gates, "pre-created Settings admin boundary", settings?.threshold === 1 && settings.timeLock === 0 && signers.length === 1 && signers[0]?.address === PARTNER_ROUTE.setupAdmin && signers[0].permissionsMask === 7, settings ? { threshold: settings.threshold, timeLock: settings.timeLock, signers } : null, { threshold: 1, timeLock: 0, signers: [{ address: PARTNER_ROUTE.setupAdmin, permissionsMask: 7 }] });
  add(gates, "pre-created Settings current policy seed", BigInt(settings?.policySeed?.toString() ?? "0") === expectedCurrentSeed, settings?.policySeed?.toString() ?? "0", expectedCurrentSeed);
  add(gates, "Squads isolation context is fresh", minContextSlot === undefined || response.context.slot >= minContextSlot, response.context.slot, minContextSlot === undefined ? "current finalized" : `>=${minContextSlot}`);
  const active = [];
  for (let index = 0; index < policySeeds.length; index += 1) {
    const seed = policySeeds[index]!;
    const account = response.value[index + 1] ?? null;
    if (seed < 15n) {
      add(gates, `retired policy ${seed} remains absent`, account === null, account?.owner.toBase58() ?? null, null);
      continue;
    }
    let decoded: PolicyState | null = null;
    try {
      if (account && account.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))) decoded = Policy.fromAccountInfo(account)[0];
    } catch {
      decoded = null;
    }
    const graph = decoded?.policyState.fields?.[0];
    const programs = graph?.instructionsConstraints?.map(({ programId }) => programId.toBase58()) ?? [];
    const isolated = decoded?.policyState.__kind === "ProgramInteraction"
      && graph?.accountIndex === 0
      && !programs.includes(PARTNER_ROUTE.programs.voltrVault);
    add(gates, `legacy policy ${seed} is isolated from Voltr manager index`, isolated, decoded ? { kind: decoded.policyState.__kind, accountIndex: graph?.accountIndex ?? null, programs } : null, { kind: "ProgramInteraction", accountIndex: 0, excludesProgram: PARTNER_ROUTE.programs.voltrVault });
    active.push({ seed, policy: addresses[index + 1]!.toBase58(), dataSha256: account ? createHash("sha256").update(account.data).digest("hex") : null, programs, accountIndex: graph?.accountIndex ?? null });
  }
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    verdict: failedGateCount === 0 ? "PARTNER_PRECREATED_SQUADS_ISOLATION_PASS" : "PARTNER_PRECREATED_SQUADS_ISOLATION_FAIL",
    broadcast: false,
    commitment,
    routeSpecSha256: routeSpecSha256(PARTNER_ROUTE),
    contextSlot: response.context.slot,
    expectedCurrentSeed,
    activeLegacyPolicies: active,
    failedGateCount,
    gates,
  } as const;
}
