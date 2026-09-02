import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { LendingMarket, Reserve } from "@kamino-finance/klend-sdk";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { AccountLayout, getAssociatedTokenAddressSync } from "@solana/spl-token";
import { Connection, PublicKey, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE, rwaMultiplyRouteSpecSha256 } from "../domain/rwa-multiply-route-spec.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CATALOG_PATH = resolve(REPOSITORY_ROOT,
  "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
const INSTRUCTIONS_SYSVAR = "Sysvar1nstructions1111111111111111111111111";

type Json = Record<string, unknown>;
const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [{
    policySeed: { toString(): string } | null;
    threshold: number;
    timeLock: number;
    signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  }, number] };
}).Settings;
export type CandidateIdentity = Readonly<{
  evidence: string;
  finalizedSlot: number;
  market: string;
  collateralReserve: string;
  collateralMint: string;
  debtReserve: string;
  debtMint: string;
  collateralTokenProgram: string;
  debtTokenProgram: string;
}>;
export type CatalogLane = Readonly<{
  market: string;
  collateral: string;
  debt: string;
  candidateIdentity: CandidateIdentity;
}>;
export type Catalog = Readonly<{
  schema: string;
  lanes: readonly CatalogLane[];
  swapEdges: readonly Readonly<{ from: string; to: string }>[];
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function publicKey(value: string, label: string): PublicKey {
  try {
    return new PublicKey(value);
  } catch {
    throw new Error(`${label} is not a public key`);
  }
}

function laneKey(lane: CatalogLane): string {
  return `${lane.market}/${lane.collateral}/${lane.debt}`;
}

function unique(values: readonly string[]): string[] {
  return [...new Set(values)];
}

export function readRwaMultiplyCatalog(): Catalog {
  const raw = readFileSync(CATALOG_PATH);
  const value = JSON.parse(raw.toString("utf8")) as Partial<Catalog>;
  invariant(value.schema === "loyal-backyard-rwa-policy-catalog/v1"
    && Array.isArray(value.lanes) && value.lanes.length === 11
    && Array.isArray(value.swapEdges) && value.swapEdges.length === 52,
  "policy catalog does not have the frozen 11-lane/52-edge shape");
  const catalog = value as Catalog;
  const expectedLanes = [
    "OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS",
    "Prime/PRIME/USDC", "Prime/PRIME/PYUSD", "Prime/PRIME/USDS",
    "Maple/syrupUSDC/USDC", "Maple/syrupUSDC/USDG", "Maple/syrupUSDC/PYUSD",
    "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD",
  ];
  invariant(JSON.stringify(catalog.lanes.map(laneKey)) === JSON.stringify(expectedLanes),
    "policy catalog lane order or identity drifted");
  const stable = ["USDC", "USDG", "USDS", "PYUSD"];
  const rwa = ["ONyc", "PRIME", "syrupUSDC", "AUTO", "USDe"];
  const expectedEdges = [
    ...stable.flatMap((from) => rwa.map((to) => `${from}->${to}`)),
    ...rwa.flatMap((from) => stable.map((to) => `${from}->${to}`)),
    ...stable.flatMap((from) => stable.filter((to) => to !== from).map((to) => `${from}->${to}`)),
  ].sort();
  const observedEdges = catalog.swapEdges.map(({ from, to }) => `${from}->${to}`).sort();
  invariant(JSON.stringify(observedEdges) === JSON.stringify(expectedEdges),
    "policy catalog swap graph is not the exact 52 directed edges");
  return catalog;
}

function multiplyObligation(lane: CatalogLane): PublicKey {
  const candidate = lane.candidateIdentity;
  return PublicKey.findProgramAddressSync([
    Buffer.from([1]),
    Buffer.from([0]),
    publicKey(RWA_MULTIPLY_ROUTE.squads.vault, "Squads vault").toBuffer(),
    publicKey(candidate.market, `${laneKey(lane)} market`).toBuffer(),
    publicKey(candidate.collateralMint, `${laneKey(lane)} collateral mint`).toBuffer(),
    publicKey(candidate.debtMint, `${laneKey(lane)} debt mint`).toBuffer(),
  ], publicKey(RWA_MULTIPLY_ROUTE.kamino.program, "KLend program"))[0];
}

function lendingMarketAuthority(market: string): PublicKey {
  return PublicKey.findProgramAddressSync([
    Buffer.from("lma"),
    publicKey(market, "lending market").toBuffer(),
  ], publicKey(RWA_MULTIPLY_ROUTE.kamino.program, "KLend program"))[0];
}

export function rwaMultiplyVaultAta(mint: string, tokenProgram: string): PublicKey {
  return getAssociatedTokenAddressSync(
    publicKey(mint, "custody mint"),
    publicKey(RWA_MULTIPLY_ROUTE.squads.vault, "Squads vault"),
    true,
    publicKey(tokenProgram, "custody token program"),
    publicKey(RWA_MULTIPLY_ROUTE.assets.associatedTokenProgram, "associated token program"),
  );
}

function tokenAccountBoundary(
  account: AccountInfo<Buffer> | null,
  address: PublicKey,
  mint: string,
  tokenProgram: string,
) {
  if (account === null) return {
    address: address.toBase58(),
    initialized: false,
    exact: false,
    blocker: "derived vault ATA is absent and must be initialized before policy simulation",
  };
  let decoded: ReturnType<typeof AccountLayout.decode>;
  try {
    decoded = AccountLayout.decode(account.data);
  } catch {
    return { address: address.toBase58(), initialized: true, exact: false, blocker: "vault ATA is undecodable" };
  }
  const observedMint = new PublicKey(decoded.mint).toBase58();
  const observedOwner = new PublicKey(decoded.owner).toBase58();
  const exact = account.owner.equals(publicKey(tokenProgram, "token program"))
    && observedMint === mint
    && observedOwner === RWA_MULTIPLY_ROUTE.squads.vault;
  return {
    address: address.toBase58(),
    initialized: true,
    exact,
    ownerProgram: account.owner.toBase58(),
    mint: observedMint,
    authority: observedOwner,
    dataSha256: sha256(account.data),
    ...(exact ? {} : { blocker: "vault ATA owner/mint/authority boundary drifted" }),
  };
}

function decodedReserveBoundary(
  lane: CatalogLane,
  side: "collateral" | "debt",
  account: AccountInfo<Buffer> | null,
) {
  const candidate = lane.candidateIdentity;
  const expectedAddress = side === "collateral" ? candidate.collateralReserve : candidate.debtReserve;
  const expectedMint = side === "collateral" ? candidate.collateralMint : candidate.debtMint;
  const expectedTokenProgram = side === "collateral"
    ? candidate.collateralTokenProgram : candidate.debtTokenProgram;
  invariant(account !== null, `${laneKey(lane)} ${side} reserve is absent`);
  invariant(account.owner.equals(publicKey(RWA_MULTIPLY_ROUTE.kamino.program, "KLend program")),
    `${laneKey(lane)} ${side} reserve has the wrong owner`);
  const reserve = Reserve.decode(account.data);
  const observed = {
    address: expectedAddress,
    lendingMarket: new PublicKey(reserve.lendingMarket).toBase58(),
    liquidityMint: new PublicKey(reserve.liquidity.mintPubkey).toBase58(),
    liquidityTokenProgram: new PublicKey(reserve.liquidity.tokenProgram).toBase58(),
    liquiditySupply: new PublicKey(reserve.liquidity.supplyVault).toBase58(),
    liquidityFeeReceiver: new PublicKey(reserve.liquidity.feeVault).toBase58(),
    collateralMint: new PublicKey(reserve.collateral.mintPubkey).toBase58(),
    collateralSupply: new PublicKey(reserve.collateral.supplyVault).toBase58(),
    status: reserve.config.status,
    lastUpdateSlot: reserve.lastUpdate.slot.toString(),
    lastUpdateStale: reserve.lastUpdate.stale,
    dataSha256: sha256(account.data),
  };
  invariant(observed.lendingMarket === candidate.market,
    `${laneKey(lane)} ${side} reserve lending market drifted`);
  invariant(observed.liquidityMint === expectedMint,
    `${laneKey(lane)} ${side} reserve liquidity mint drifted`);
  invariant(observed.liquidityTokenProgram === expectedTokenProgram,
    `${laneKey(lane)} ${side} reserve token program drifted`);
  invariant(Number(observed.status) === 0, `${laneKey(lane)} ${side} reserve is not active`);
  return observed;
}

export async function resolveCurrentRwaMultiplyCatalog(connection: Connection) {
  const catalog = readRwaMultiplyCatalog();
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not mainnet-beta");
  const baseAddresses = unique(catalog.lanes.flatMap((lane) => [
    lane.candidateIdentity.market,
    lane.candidateIdentity.collateralReserve,
    lane.candidateIdentity.debtReserve,
    lane.candidateIdentity.collateralMint,
    lane.candidateIdentity.debtMint,
  ]));
  const baseRead = await connection.getMultipleAccountsInfoAndContext(
    baseAddresses.map((value) => publicKey(value, "candidate graph address")),
    { commitment: "confirmed" },
  );
  const byAddress = new Map(baseAddresses.map((value, index) => [value, baseRead.value[index] ?? null]));
  const klend = publicKey(RWA_MULTIPLY_ROUTE.kamino.program, "KLend program");
  for (const lane of catalog.lanes) {
    const market = byAddress.get(lane.candidateIdentity.market) ?? null;
    invariant(market !== null && market.owner.equals(klend), `${laneKey(lane)} market is absent or not KLend-owned`);
    LendingMarket.decode(market.data);
    for (const [mint, tokenProgram] of [
      [lane.candidateIdentity.collateralMint, lane.candidateIdentity.collateralTokenProgram],
      [lane.candidateIdentity.debtMint, lane.candidateIdentity.debtTokenProgram],
    ] as const) {
      const mintAccount = byAddress.get(mint) ?? null;
      invariant(mintAccount !== null && mintAccount.owner.equals(publicKey(tokenProgram, "candidate token program")),
        `${laneKey(lane)} mint ${mint} is absent or has the wrong token program`);
    }
  }

  const custodyAddresses = unique(catalog.lanes.flatMap((lane) => [
    rwaMultiplyVaultAta(lane.candidateIdentity.collateralMint, lane.candidateIdentity.collateralTokenProgram).toBase58(),
    rwaMultiplyVaultAta(lane.candidateIdentity.debtMint, lane.candidateIdentity.debtTokenProgram).toBase58(),
  ]));
  const custodyRead = await connection.getMultipleAccountsInfoAndContext(
    custodyAddresses.map((value) => new PublicKey(value)),
    { commitment: "confirmed", minContextSlot: baseRead.context.slot },
  );
  const custodyByAddress = new Map(custodyAddresses.map((value, index) => [value, custodyRead.value[index] ?? null]));

  const lanes = catalog.lanes.map((lane) => {
    const candidate = lane.candidateIdentity;
    const collateralReserve = decodedReserveBoundary(lane, "collateral",
      byAddress.get(candidate.collateralReserve) ?? null);
    const debtReserve = decodedReserveBoundary(lane, "debt", byAddress.get(candidate.debtReserve) ?? null);
    const collateralCustody = rwaMultiplyVaultAta(candidate.collateralMint, candidate.collateralTokenProgram);
    const debtCustody = rwaMultiplyVaultAta(candidate.debtMint, candidate.debtTokenProgram);
    const collateralCustodyBoundary = tokenAccountBoundary(
      custodyByAddress.get(collateralCustody.toBase58()) ?? null,
      collateralCustody,
      candidate.collateralMint,
      candidate.collateralTokenProgram,
    );
    const debtCustodyBoundary = tokenAccountBoundary(
      custodyByAddress.get(debtCustody.toBase58()) ?? null,
      debtCustody,
      candidate.debtMint,
      candidate.debtTokenProgram,
    );
    return {
      key: laneKey(lane),
      candidateIdentity: candidate,
      resolved: {
        klendProgram: RWA_MULTIPLY_ROUTE.kamino.program,
        vault: RWA_MULTIPLY_ROUTE.squads.vault,
        lendingMarket: candidate.market,
        lendingMarketAuthority: lendingMarketAuthority(candidate.market).toBase58(),
        collateralReserve,
        debtReserve,
        obligation: multiplyObligation(lane).toBase58(),
        collateralCustody: collateralCustodyBoundary,
        debtCustody: debtCustodyBoundary,
        instructionSysvar: INSTRUCTIONS_SYSVAR,
      },
      exact: collateralCustodyBoundary.exact && debtCustodyBoundary.exact,
    };
  });
  const routeAccountsExact = lanes.every((lane) => lane.exact);
  const assetPrograms = new Map<string, string>();
  for (const [mint, tokenProgram] of catalog.lanes.flatMap((lane) => [
    [lane.candidateIdentity.collateralMint, lane.candidateIdentity.collateralTokenProgram] as const,
    [lane.candidateIdentity.debtMint, lane.candidateIdentity.debtTokenProgram] as const,
  ])) {
    invariant(!assetPrograms.has(mint) || assetPrograms.get(mint) === tokenProgram,
      `mint ${mint} has conflicting token programs across lanes`);
    assetPrograms.set(mint, tokenProgram);
  }
  invariant(assetPrograms.size === 9, "resolved swap universe is not exactly four stable and five RWA mints");
  const settingsRead = await connection.getAccountInfoAndContext(
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings),
    { commitment: "confirmed", minContextSlot: custodyRead.context.slot },
  );
  invariant(settingsRead.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "Squads Settings is absent or has the wrong owner");
  const [settings] = Settings.fromAccountInfo(settingsRead.value);
  invariant(settings.threshold === 1 && settings.timeLock === 0 && settings.signers.length === 1
    && settings.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin
    && settings.signers[0]?.permissions.mask === 7, "Squads Settings authority boundary drifted");
  const policySeedBefore = BigInt(settings.policySeed?.toString() ?? "0");
  invariant(policySeedBefore >= 66n, "Squads Settings policy seed predates the installed Phase 1 policies");
  const swapCustodies = Object.fromEntries([...assetPrograms].map(([mint, tokenProgram]) => [mint, {
    tokenProgram,
    custody: rwaMultiplyVaultAta(mint, tokenProgram).toBase58(),
  }]));
  return {
    schema: "loyal-backyard-rwa-policy-resolution/v1",
    verdict: routeAccountsExact ? "PASS_LANE_ACCOUNTS_RESOLVED" : "FAIL_LANE_ACCOUNTS_UNRESOLVED",
    broadcast: false,
    cluster: "mainnet-beta",
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    commitment: "confirmed",
    contextSlot: settingsRead.context.slot,
    catalogSha256: sha256(readFileSync(CATALOG_PATH)),
    routeSpecSha256: rwaMultiplyRouteSpecSha256(),
    policySeedBefore: policySeedBefore.toString(),
    laneGraphExact: routeAccountsExact,
    lanes,
    swap: {
      jupiterProgram: RWA_MULTIPLY_ROUTE.programs.jupiter,
      edges: catalog.swapEdges,
      custodies: swapCustodies,
      structuralSlices: ["stable-to-rwa", "rwa-to-stable", "stable-to-stable"],
      headerEvidence: "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json",
    },
    addressesResolved: routeAccountsExact,
    resumeCondition: routeAccountsExact
      ? null
      : "Initialize or repair every derived Squads vault ATA and rerun this confirmed resolver.",
  } as const;
}

export async function resolveCurrentRwaMultiplyCatalogFromEnvironment() {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required for read-only catalog resolution");
  return resolveCurrentRwaMultiplyCatalog(new Connection(rpcUrl, "confirmed"));
}
