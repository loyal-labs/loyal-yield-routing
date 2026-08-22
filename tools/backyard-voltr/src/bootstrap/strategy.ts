import { createHash } from "node:crypto";
import { Reserve } from "@kamino-finance/klend-sdk";
import { createNoopSigner } from "@solana/kit";
import { getVaultDecoder, getVaultEncoder } from "@voltr/vault-sdk";
import {
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";

import {
  assertIntentForRoute,
  intentSha256,
  type SetupIntent,
} from "../domain/execution-intent.js";
import {
  loadBootstrapExecutionAuthorization,
  operationAuthorization,
} from "../domain/bootstrap-execution-authorization.js";
import {
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyGraphSha256,
  partnerStrategyIdentity,
  routeSpecSha256,
  type PartnerStrategyId,
} from "../domain/route-spec.js";
import {
  confirmedSnapshots,
  loadDeploymentIdentities,
  loadReserveGraphs,
  prepareSignedV0Transaction,
  sendPreparedConfirmedOnce,
} from "../integrations/solana-compat.js";
import {
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import { createVoltrRouteBuilder, deriveVoltrAccounts } from "../integrations/voltr.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  verifyVaultCurrentState,
  type Gate,
} from "../verify/current.js";

const MAX_STRATEGY_BOOTSTRAP_LAMPORTS = 75_000_000;

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function sameJson(left: unknown, right: unknown): boolean {
  return JSON.stringify(left, (_key, value) => typeof value === "bigint" ? value.toString() : value)
    === JSON.stringify(right, (_key, value) => typeof value === "bigint" ? value.toString() : value);
}

function accountFingerprint(account: { owner: string; lamports: number; executable: boolean; data: Uint8Array } | null): unknown {
  return account === null ? null : {
    owner: account.owner,
    lamports: account.lamports,
    executable: account.executable,
    dataSha256: createHash("sha256").update(account.data).digest("hex"),
  };
}

type ExpectedInstructionMeta = Readonly<{
  label: string;
  address: string;
  signer: boolean;
  writable: boolean;
}>;

function expectedBootstrapAccountVectors(input: Readonly<{
  route: ReturnType<typeof partnerBuilderRoute>;
  accounts: Awaited<ReturnType<typeof deriveVoltrAccounts>>;
  graph: Readonly<{
    reserve: string;
    userMetadata: string;
    obligation: string;
    lendingMarketAuthority: string;
    reserveFarmState: string;
    obligationFarm: string;
    lendingMarket: string;
  }>;
}>): readonly (readonly ExpectedInstructionMeta[])[] {
  const { route, accounts, graph } = input;
  const update = [
    { label: "admin", address: route.setupAdmin, signer: true, writable: false },
    { label: "protocol", address: accounts.protocol, signer: false, writable: false },
    { label: "vault", address: route.vault, signer: false, writable: true },
    { label: "rent", address: "SysvarRent111111111111111111111111111111111", signer: false, writable: false },
  ] as const;
  const initialize = [
    { label: "payer", address: route.setupAdmin, signer: true, writable: true },
    { label: "manager", address: route.setupAdmin, signer: true, writable: false },
    { label: "protocol", address: accounts.protocol, signer: false, writable: false },
    { label: "vault", address: route.vault, signer: false, writable: false },
    { label: "strategy", address: graph.reserve, signer: false, writable: false },
    { label: "adaptorAddReceipt", address: accounts.adaptorAddReceipt, signer: false, writable: false },
    { label: "strategyInitReceipt", address: accounts.strategyInitReceipt, signer: false, writable: true },
    { label: "vaultStrategyAuth", address: accounts.strategyAuth, signer: false, writable: true },
    { label: "adaptorProgram", address: route.programs.kaminoAdaptor, signer: false, writable: false },
    { label: "systemProgram", address: route.programs.system, signer: false, writable: false },
    { label: "kaminoUserMetadata", address: graph.userMetadata, signer: false, writable: true },
    { label: "kaminoObligation", address: graph.obligation, signer: false, writable: true },
    { label: "lendingMarketAuthority", address: graph.lendingMarketAuthority, signer: false, writable: false },
    { label: "reserve", address: graph.reserve, signer: false, writable: true },
    { label: "reserveFarmState", address: graph.reserveFarmState, signer: false, writable: true },
    { label: "obligationFarm", address: graph.obligationFarm, signer: false, writable: true },
    { label: "lendingMarket", address: graph.lendingMarket, signer: false, writable: false },
    { label: "farmsProgram", address: route.programs.farms, signer: false, writable: false },
    { label: "rentSysvar", address: "SysvarRent111111111111111111111111111111111", signer: false, writable: false },
    { label: "klendProgram", address: route.programs.klend, signer: false, writable: false },
  ] as const;
  return [update, initialize, update];
}

function exactBootstrapPacket(input: Readonly<{
  route: ReturnType<typeof partnerBuilderRoute>;
  instructions: readonly Readonly<{
    canonical: Readonly<{
      programId: string;
      data: Uint8Array;
      accounts: readonly ExpectedInstructionMeta[];
    }>;
  }>[];
  expectedAccounts: readonly (readonly ExpectedInstructionMeta[])[];
  prepared: Awaited<ReturnType<typeof prepareSignedV0Transaction>>;
}>) {
  const { route, instructions, expectedAccounts, prepared } = input;
  const observedAccounts = instructions.map(({ canonical }) => canonical.accounts.map((meta) => ({
    label: meta.label,
    address: meta.address,
    signer: meta.signer,
    writable: meta.writable,
  })));
  const canonicalAccountsExact = observedAccounts.every((accounts, index) =>
    sameJson(accounts, expectedAccounts[index]));
  const expectedInstructions = instructions.map((instruction, index) => new TransactionInstruction({
    programId: new PublicKey(route.programs.voltrVault),
    keys: expectedAccounts[index]!.map((meta) => ({
      pubkey: new PublicKey(meta.address),
      isSigner: meta.signer,
      isWritable: meta.writable,
    })),
    data: Buffer.from(instruction.canonical.data),
  }));
  const expectedMessage = new TransactionMessage({
    payerKey: new PublicKey(route.setupAdmin),
    recentBlockhash: prepared.latestBlockhash.blockhash,
    instructions: expectedInstructions,
  }).compileToV0Message();
  const observedTransaction = VersionedTransaction.deserialize(prepared.serializedTransaction);
  return {
    canonicalAccountsExact,
    expectedAccounts,
    observedAccounts,
    exactSerializedMessage: Buffer.from(expectedMessage.serialize()).equals(Buffer.from(prepared.serializedMessage)),
    exactTransactionMessage: Buffer.from(observedTransaction.message.serialize()).equals(Buffer.from(expectedMessage.serialize())),
    noLookupTables: expectedMessage.addressTableLookups.length === 0
      && observedTransaction.message.addressTableLookups.length === 0,
    requiredSignatureCount: observedTransaction.message.header.numRequiredSignatures,
    signatureCount: observedTransaction.signatures.length,
    staticAccountKeys: observedTransaction.message.staticAccountKeys.map((key) => key.toBase58()),
    expectedStaticAccountKeys: expectedMessage.staticAccountKeys.map((key) => key.toBase58()),
    header: observedTransaction.message.header,
    expectedHeader: expectedMessage.header,
  } as const;
}

function byteDifferences(
  before: Uint8Array | undefined,
  after: Uint8Array | undefined,
) {
  const left = before ?? new Uint8Array();
  const right = after ?? new Uint8Array();
  const length = Math.max(left.length, right.length);
  const differences = [];
  for (let offset = 0; offset < length; offset += 1) {
    if (left[offset] !== right[offset]) {
      differences.push({ offset, before: left[offset] ?? null, after: right[offset] ?? null });
    }
  }
  return differences;
}

function vaultUpdateTransition(
  before: Uint8Array | undefined,
  after: Uint8Array | undefined,
) {
  if (!before || !after) return null;
  try {
    const beforeVault = getVaultDecoder().decode(before);
    const afterVault = getVaultDecoder().decode(after);
    const beforeLastUpdatedTs = beforeVault.lastUpdatedTs;
    const afterLastUpdatedTs = afterVault.lastUpdatedTs;
    const normalizedBefore = getVaultEncoder().encode({
      ...beforeVault,
      lastUpdatedTs: 0n,
    });
    const normalizedAfter = getVaultEncoder().encode({
      ...afterVault,
      lastUpdatedTs: 0n,
    });
    return {
      onlyLastUpdatedTsChanged: Buffer.from(normalizedBefore).equals(Buffer.from(normalizedAfter)),
      timestampAdvanced: afterLastUpdatedTs >= beforeLastUpdatedTs,
      beforeLastUpdatedTs,
      afterLastUpdatedTs,
      byteDifferences: byteDifferences(before, after),
    } as const;
  } catch {
    return null;
  }
}

function reserveRefreshTransition(
  before: Uint8Array | undefined,
  after: Uint8Array | undefined,
) {
  if (!before || !after) return null;
  try {
    const beforeReserve = Reserve.decode(Buffer.from(before));
    const afterReserve = Reserve.decode(Buffer.from(after));
    const beforeJson = beforeReserve.toJSON();
    const afterJson = afterReserve.toJSON();
    const beforeLastUpdateSlot = BigInt(beforeJson.lastUpdate.slot);
    const afterLastUpdateSlot = BigInt(afterJson.lastUpdate.slot);
    const fractionValue = (value: readonly string[]) => value.reduce(
      (total, limb, index) => total + (BigInt(limb) << (64n * BigInt(index))),
      0n,
    );
    const beforeBorrowedAmount = BigInt(beforeJson.liquidity.borrowedAmountSf);
    const afterBorrowedAmount = BigInt(afterJson.liquidity.borrowedAmountSf);
    const beforeCumulativeBorrowRate = fractionValue(beforeJson.liquidity.cumulativeBorrowRateBsf.value);
    const afterCumulativeBorrowRate = fractionValue(afterJson.liquidity.cumulativeBorrowRateBsf.value);
    const beforeProtocolFees = BigInt(beforeJson.liquidity.accumulatedProtocolFeesSf);
    const afterProtocolFees = BigInt(afterJson.liquidity.accumulatedProtocolFeesSf);
    const beforeMarketPrice = BigInt(beforeJson.liquidity.marketPriceSf);
    const afterMarketPrice = BigInt(afterJson.liquidity.marketPriceSf);
    const beforeMarketPriceTs = BigInt(beforeJson.liquidity.marketPriceLastUpdatedTs);
    const afterMarketPriceTs = BigInt(afterJson.liquidity.marketPriceLastUpdatedTs);
    const beforeBorrowedOutsideElevation = BigInt(beforeJson.borrowedAmountOutsideElevationGroup);
    const afterBorrowedOutsideElevation = BigInt(afterJson.borrowedAmountOutsideElevationGroup);
    const placeholderTimestamp = (value: readonly number[]) => value.length === 6
      ? BigInt(value[2]!)
        | (BigInt(value[3]!) << 8n)
        | (BigInt(value[4]!) << 16n)
        | (BigInt(value[5]!) << 24n)
      : -1n;
    const beforePlaceholderTimestamp = placeholderTimestamp(beforeJson.lastUpdate.placeholder);
    const afterPlaceholderTimestamp = placeholderTimestamp(afterJson.lastUpdate.placeholder);
    const changedLiquidityFields = Object.keys(beforeJson.liquidity).filter((key) =>
      !sameJson(
        beforeJson.liquidity[key as keyof typeof beforeJson.liquidity],
        afterJson.liquidity[key as keyof typeof afterJson.liquidity],
      ));
    const changedTopLevelFields = Object.keys(beforeJson).filter((key) =>
      !sameJson(
        beforeJson[key as keyof typeof beforeJson],
        afterJson[key as keyof typeof afterJson],
      ));
    const normalizedBefore = {
      ...beforeJson,
      lastUpdate: {
        ...beforeJson.lastUpdate,
        slot: "0",
        placeholder: beforeJson.lastUpdate.placeholder.map((value, index) => index < 2 ? value : 0),
      },
      liquidity: {
        ...beforeJson.liquidity,
        borrowedAmountSf: "0",
        cumulativeBorrowRateBsf: {
          ...beforeJson.liquidity.cumulativeBorrowRateBsf,
          value: beforeJson.liquidity.cumulativeBorrowRateBsf.value.map(() => "0"),
        },
        accumulatedProtocolFeesSf: "0",
        marketPriceSf: "0",
        marketPriceLastUpdatedTs: "0",
      },
      borrowedAmountOutsideElevationGroup: "0",
    };
    const normalizedAfter = {
      ...afterJson,
      lastUpdate: {
        ...afterJson.lastUpdate,
        slot: "0",
        placeholder: afterJson.lastUpdate.placeholder.map((value, index) => index < 2 ? value : 0),
      },
      liquidity: {
        ...afterJson.liquidity,
        borrowedAmountSf: "0",
        cumulativeBorrowRateBsf: {
          ...afterJson.liquidity.cumulativeBorrowRateBsf,
          value: afterJson.liquidity.cumulativeBorrowRateBsf.value.map(() => "0"),
        },
        accumulatedProtocolFeesSf: "0",
        marketPriceSf: "0",
        marketPriceLastUpdatedTs: "0",
      },
      borrowedAmountOutsideElevationGroup: "0",
    };
    const markerSemanticsExact = beforeJson.lastUpdate.stale === afterJson.lastUpdate.stale
      && beforeJson.lastUpdate.priceStatus === afterJson.lastUpdate.priceStatus
      && beforeJson.lastUpdate.placeholder.length === 6
      && afterJson.lastUpdate.placeholder.length === 6
      && beforeJson.lastUpdate.placeholder[0] === 0
      && beforeJson.lastUpdate.placeholder[1] === 0
      && afterJson.lastUpdate.placeholder[0] === 0
      && afterJson.lastUpdate.placeholder[1] === 0
      && afterPlaceholderTimestamp >= beforePlaceholderTimestamp
      && afterPlaceholderTimestamp - beforePlaceholderTimestamp <= 86_400n;
    const accrualsMonotonic = afterBorrowedAmount >= beforeBorrowedAmount
      && afterCumulativeBorrowRate >= beforeCumulativeBorrowRate
      && afterProtocolFees >= beforeProtocolFees
      && afterBorrowedOutsideElevation >= beforeBorrowedOutsideElevation;
    const priceDelta = afterMarketPrice >= beforeMarketPrice
      ? afterMarketPrice - beforeMarketPrice
      : beforeMarketPrice - afterMarketPrice;
    const oracleRefreshSafe = beforeMarketPrice > 0n
      && afterMarketPrice > 0n
      && afterMarketPriceTs >= beforeMarketPriceTs
      // USDC oracle movement during one setup preflight is bounded to 1%.
      && priceDelta * 10_000n <= beforeMarketPrice * 100n;
    return {
      onlyApprovedRefreshFieldsChanged: sameJson(normalizedBefore, normalizedAfter),
      lastUpdateSlotAdvanced: afterLastUpdateSlot >= beforeLastUpdateSlot,
      markerSemanticsExact,
      accrualsMonotonic,
      oracleRefreshSafe,
      accruals: {
        borrowedAmountSf: { before: beforeBorrowedAmount, after: afterBorrowedAmount },
        cumulativeBorrowRate: { before: beforeCumulativeBorrowRate, after: afterCumulativeBorrowRate },
        accumulatedProtocolFeesSf: { before: beforeProtocolFees, after: afterProtocolFees },
        borrowedAmountOutsideElevationGroup: { before: beforeBorrowedOutsideElevation, after: afterBorrowedOutsideElevation },
        marketPriceSf: { before: beforeMarketPrice, after: afterMarketPrice, absoluteDelta: priceDelta },
        marketPriceLastUpdatedTs: { before: beforeMarketPriceTs, after: afterMarketPriceTs },
      },
      beforeLastUpdate: beforeJson.lastUpdate,
      afterLastUpdate: afterJson.lastUpdate,
      placeholderTimestamp: {
        before: beforePlaceholderTimestamp,
        after: afterPlaceholderTimestamp,
      },
      changedTopLevelFields,
      changedLiquidityFields,
      changedLiquidity: Object.fromEntries(changedLiquidityFields.map((key) => [
        key,
        {
          before: beforeJson.liquidity[key as keyof typeof beforeJson.liquidity],
          after: afterJson.liquidity[key as keyof typeof afterJson.liquidity],
        },
      ])),
      byteDifferences: byteDifferences(before, after),
    } as const;
  } catch {
    return null;
  }
}

export type StrategyBootstrapExecutionConfirmation = Readonly<{
  strategyId: PartnerStrategyId;
  authorizationPath: string | null;
  confirmAuthorizationSha256: string | null;
  confirmStrategyId: string | null;
  confirmReserve: string | null;
  confirmVault: string | null;
  confirmFourMarketRouteSpecSha256: string | null;
  confirmBuilderRouteSpecSha256: string | null;
  confirmSetManagerDataSha256: string | null;
  confirmInitializeStrategyDataSha256: string | null;
  confirmRestoreManagerDataSha256: string | null;
  confirmMaxTotalLamports: string | null;
}>;

async function loadSelectedReserve(
  strategyId: PartnerStrategyId,
  minimumContextSlot?: number,
) {
  const identity = partnerStrategyIdentity(strategyId);
  const route = partnerBuilderRoute(strategyId);
  const accounts = await deriveVoltrAccounts(route);
  if (
    accounts.strategyAuth !== identity.voltr.strategyAuth
    || accounts.strategyInitReceipt !== identity.voltr.strategyInitReceipt
  ) {
    throw new Error(`${strategyId} derived Voltr accounts do not match the frozen catalog`);
  }
  const loaded = await loadReserveGraphs(
    rpcUrl(),
    route,
    [{ candidate: { id: strategyId, reserve: identity.reserve }, vaultStrategyAuth: accounts.strategyAuth }],
    "confirmed",
    minimumContextSlot,
  );
  const row = loaded.rows[0];
  if (!row?.observation) {
    throw new Error(row?.error ?? `${strategyId} reserve graph did not decode`);
  }
  const observation = row.observation;
  const expectedGraph = { reserve: identity.reserve, ...identity.graph };
  if (
    observation.reserveStatus !== 0
    || observation.liquidityMint !== route.asset.mint
    || observation.liquidityTokenProgram !== route.asset.tokenProgram
    || observation.liquidityMintDecimals !== route.asset.decimals
    || !observation.hasCollateralFarm
    || !sameJson(observation.graph, expectedGraph)
  ) {
    throw new Error(`${strategyId} confirmed reserve state escaped the frozen active-USDC graph`);
  }
  return { route, identity, accounts, reserve: observation } as const;
}

async function context(strategyId: PartnerStrategyId) {
  const selected = await loadSelectedReserve(strategyId);
  const { route, accounts, reserve } = selected;
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== route.setupAdmin) {
    throw new Error(`SOLANA_TESTING_PK ${admin.signer.address} is not RouteSpec setup admin`);
  }
  // Strategy initialization never marks the vault as a signer. Keep the
  // builder's setup shape explicit without deriving or loading a vault key.
  const vault = createNoopSigner(route.vault);
  const builder = await createVoltrRouteBuilder(route, reserve.graph);
  return { ...selected, admin, vault, builder } as const;
}

export async function strategyBootstrapAuthorizationFacts(strategyId: PartnerStrategyId) {
  const route = partnerBuilderRoute(strategyId);
  const identity = partnerStrategyIdentity(strategyId);
  const accounts = await deriveVoltrAccounts(route);
  if (
    accounts.strategyAuth !== identity.voltr.strategyAuth
    || accounts.strategyInitReceipt !== identity.voltr.strategyInitReceipt
  ) throw new Error(`${strategyId} statically derived Voltr accounts do not match the frozen catalog`);
  const graph = { reserve: identity.reserve, ...identity.graph } as const;
  const builder = await createVoltrRouteBuilder(route, graph);
  const admin = createNoopSigner(route.setupAdmin);
  const vault = createNoopSigner(route.vault);
  const signers = { payer: admin, admin, vault };
  const setManager = await builder.setup.setManagerToAdmin(signers);
  const initialize = await builder.setup.initializeStrategyAsAdmin(signers);
  const restoreManager = await builder.setup.restoreManager(signers);
  return {
    route,
    identity,
    accounts,
    graph,
    instructionDataSha256: [
      setManager.canonical.dataSha256,
      initialize.canonical.dataSha256,
      restoreManager.canonical.dataSha256,
    ] as const,
  } as const;
}

async function prepareStrategyBootstrap(strategyId: PartnerStrategyId) {
  const { route, identity, admin, vault, accounts, reserve, builder } = await context(strategyId);
  const signers = { payer: admin.signer, admin: admin.signer, vault };
  const setManager = await builder.setup.setManagerToAdmin(signers);
  const initialize = await builder.setup.initializeStrategyAsAdmin(signers);
  const restoreManager = await builder.setup.restoreManager(signers);
  const inspectedAddresses = [
    route.vault,
    accounts.lpMint,
    accounts.idleAta,
    route.asset.mint,
    accounts.adaptorAddReceipt,
    accounts.strategyInitReceipt,
    reserve.graph.userMetadata,
    reserve.graph.obligation,
    reserve.graph.obligationFarm,
    route.strategy.reserve,
    route.setupAdmin,
  ];
  const before = await confirmedSnapshots(rpcUrl(), inspectedAddresses, reserve.contextSlot);
  const deployments = await loadDeploymentIdentities(rpcUrl(), route, before.contextSlot, "confirmed");
  const vaultBefore = verifyVaultCurrentState({
    route,
    accounts,
    vault: before.accounts[0] ?? null,
    lpMint: before.accounts[1] ?? null,
    idleAta: before.accounts[2] ?? null,
    assetMint: before.accounts[3] ?? null,
  });
  const adaptorBefore = verifyAdaptorReceipt(route, accounts.adaptorAddReceipt, before.accounts[4] ?? null);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: admin,
    instructions: [setManager.raw, initialize.raw, restoreManager.raw],
    inspectedAddresses,
    commitment: "confirmed",
  });
  const deploymentsAfter = await loadDeploymentIdentities(rpcUrl(), route, prepared.simulationSlot, "confirmed");
  const post = new Map(inspectedAddresses.map((account, index) => [account, prepared.simulation.postAccounts[index] ?? null]));
  const vaultAfter = verifyVaultCurrentState({
    route,
    accounts,
    vault: post.get(route.vault) ?? null,
    lpMint: post.get(accounts.lpMint) ?? null,
    idleAta: post.get(accounts.idleAta) ?? null,
    assetMint: post.get(route.asset.mint) ?? null,
  });
  const strategyGates = verifyStrategyBootstrap({
    route,
    accounts,
    graph: reserve.graph,
    strategyReceipt: post.get(accounts.strategyInitReceipt) ?? null,
    userMetadata: post.get(reserve.graph.userMetadata) ?? null,
    obligation: post.get(reserve.graph.obligation) ?? null,
    obligationFarm: post.get(reserve.graph.obligationFarm) ?? null,
  });
  const gates: Gate[] = [];
  gates.push(...verifyDeploymentIdentities(route, deployments.identities).map((gate) => ({ ...gate, name: `strategy deployment: ${gate.name}` })));
  gates.push(...verifyDeploymentIdentities(route, deploymentsAfter.identities).map((gate) => ({ ...gate, name: `simulated deployment: ${gate.name}` })));
  add(gates, "deployment identities unchanged across simulation", sameJson(deployments.identities, deploymentsAfter.identities), deploymentsAfter.identities, deployments.identities);
  add(gates, "confirmed vault is exact and Squads-managed", vaultBefore.verdict === "PARTNER_VAULT_CURRENT_PASS", vaultBefore.verdict, "PARTNER_VAULT_CURRENT_PASS");
  add(gates, "confirmed adaptor receipt is exact", adaptorBefore.every(({ pass }) => pass), adaptorBefore.filter(({ pass }) => !pass), []);
  add(gates, "strategy accounts are absent", before.accounts.slice(5, 9).every((account) => account === null), before.accounts.slice(5, 9).map((account) => account?.address ?? null), [null, null, null, null]);
  add(gates, "exact three-instruction atomic bootstrap", [setManager, initialize, restoreManager].every(({ canonical }) => canonical.programId === route.programs.voltrVault), [setManager.canonical.programId, initialize.canonical.programId, restoreManager.canonical.programId], [route.programs.voltrVault, route.programs.voltrVault, route.programs.voltrVault]);
  add(gates, "simulation succeeded", prepared.simulation.err === null, prepared.simulation.err, null);
  add(gates, "bootstrap packet within Solana limit", prepared.packetBytes <= 1_232, prepared.packetBytes, "<=1232");
  const bootstrapInstructions = [setManager, initialize, restoreManager] as const;
  const signerAddresses = bootstrapInstructions.flatMap(({ canonical }) => canonical.accounts.filter(({ signer }) => signer).map(({ address }) => address));
  add(gates, "only setup admin is an instruction signer", new Set(signerAddresses).size === 1 && signerAddresses[0] === route.setupAdmin, signerAddresses, [route.setupAdmin]);
  const expectedManagerTransitionOrder = [["admin", "protocol", "vault", "rent"], ["payer", "manager", "protocol", "vault", "strategy", "adaptorAddReceipt", "strategyInitReceipt", "vaultStrategyAuth", "adaptorProgram", "systemProgram", "kaminoUserMetadata", "kaminoObligation", "lendingMarketAuthority", "reserve", "reserveFarmState", "obligationFarm", "lendingMarket", "farmsProgram", "rentSysvar", "klendProgram"], ["admin", "protocol", "vault", "rent"]];
  const managerTransitionOrder = bootstrapInstructions.map(({ canonical }) => canonical.accounts.map(({ label }) => label));
  add(gates, "manager transition instruction order is exact", sameJson(managerTransitionOrder, expectedManagerTransitionOrder), managerTransitionOrder, expectedManagerTransitionOrder);
  const expectedAccountVectors = expectedBootstrapAccountVectors({
    route,
    accounts,
    graph: reserve.graph,
  });
  const packetProof = exactBootstrapPacket({
    route,
    instructions: bootstrapInstructions,
    expectedAccounts: expectedAccountVectors,
    prepared,
  });
  add(gates, "all bootstrap instruction account addresses and roles are exact", packetProof.canonicalAccountsExact, packetProof.observedAccounts, packetProof.expectedAccounts);
  add(gates, "compiled bootstrap v0 message is exact", packetProof.exactSerializedMessage && packetProof.exactTransactionMessage, { exactSerializedMessage: packetProof.exactSerializedMessage, exactTransactionMessage: packetProof.exactTransactionMessage, staticAccountKeys: packetProof.staticAccountKeys, header: packetProof.header }, { staticAccountKeys: packetProof.expectedStaticAccountKeys, header: packetProof.expectedHeader });
  add(gates, "bootstrap has one signature and no lookup tables", packetProof.noLookupTables && packetProof.requiredSignatureCount === 1 && packetProof.signatureCount === 1, { noLookupTables: packetProof.noLookupTables, requiredSignatureCount: packetProof.requiredSignatureCount, signatureCount: packetProof.signatureCount }, { noLookupTables: true, requiredSignatureCount: 1, signatureCount: 1 });
  add(gates, "manager restored and vault remains exact", vaultAfter.verdict === "PARTNER_VAULT_CURRENT_PASS", vaultAfter.verdict, "PARTNER_VAULT_CURRENT_PASS");
  const vaultTransition = vaultUpdateTransition(
    before.accounts[0]?.data,
    post.get(route.vault)?.data,
  );
  add(
    gates,
    "atomic bootstrap changes only the vault last-updated timestamp",
    vaultTransition?.onlyLastUpdatedTsChanged === true
      && vaultTransition.timestampAdvanced,
    vaultTransition,
    "all decoded vault fields exact; lastUpdatedTs may only advance",
  );
  add(gates, "idle USDC and vault value unchanged", vaultAfter.state?.idleRaw === vaultBefore.state?.idleRaw && vaultAfter.state?.totalValueRaw === vaultBefore.state?.totalValueRaw, vaultAfter.state ? { idleRaw: vaultAfter.state.idleRaw, totalValueRaw: vaultAfter.state.totalValueRaw } : null, vaultBefore.state ? { idleRaw: vaultBefore.state.idleRaw, totalValueRaw: vaultBefore.state.totalValueRaw } : null);
  const reserveTransition = reserveRefreshTransition(
    before.accounts[9]?.data,
    post.get(route.strategy.reserve)?.data,
  );
  add(
    gates,
    "selected reserve changes only its refresh marker",
    reserveTransition?.onlyApprovedRefreshFieldsChanged === true
      && reserveTransition.lastUpdateSlotAdvanced
      && reserveTransition.markerSemanticsExact
      && reserveTransition.accrualsMonotonic
      && reserveTransition.oracleRefreshSafe,
    reserveTransition,
    "all decoded reserve fields exact except monotonic Kamino refresh accruals and lastUpdate",
  );
  gates.push(...strategyGates.map((gate) => ({ ...gate, name: `simulated strategy: ${gate.name}` })));
  const adminSpend = before.accounts[10] && post.get(route.setupAdmin)
    ? before.accounts[10].lamports - post.get(route.setupAdmin)!.lamports
    : null;
  const expectedCreatedRentLamports = [
    accounts.strategyInitReceipt,
    reserve.graph.userMetadata,
    reserve.graph.obligation,
    reserve.graph.obligationFarm,
  ].reduce((total, account) => total + (post.get(account)?.lamports ?? 0), 0);
  add(gates, "bootstrap SOL spend is exact fee plus created-account rent", adminSpend === prepared.feeLamports + expectedCreatedRentLamports, adminSpend, prepared.feeLamports + expectedCreatedRentLamports);
  add(gates, "bootstrap SOL spend bounded", adminSpend !== null && adminSpend >= prepared.feeLamports && adminSpend <= MAX_STRATEGY_BOOTSTRAP_LAMPORTS, adminSpend, `${prepared.feeLamports}..${MAX_STRATEGY_BOOTSTRAP_LAMPORTS}`);
  const canonicalMessageSha256 = createHash("sha256").update(prepared.serializedMessage).digest("hex");
  const intent: SetupIntent = {
    schemaVersion: 1,
    kind: "setup",
    operation: "initialize-strategy",
    routeId: route.id,
    routeSpecSha256: routeSpecSha256(route),
    signer: route.setupAdmin,
    nonce: `initialize-strategy:${accounts.strategyInitReceipt}`,
    prestateSlot: BigInt(prepared.prestateSlot),
    expiresAtUnix: BigInt(Math.floor(Date.now() / 1_000) + 300),
    canonicalMessageSha256,
  };
  assertIntentForRoute(intent, route);
  const intentDigest = intentSha256(intent);
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    route,
    identity,
    accounts,
    reserve,
    intent,
    intentSha256: intentDigest,
    prepared,
    report: {
      verdict: failedGateCount === 0 ? "PARTNER_STRATEGY_BOOTSTRAP_SIMULATION_PASS" : "PARTNER_STRATEGY_BOOTSTRAP_SIMULATION_FAIL",
      broadcast: false,
      readyForBroadcast: failedGateCount === 0,
      strategyId,
      fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
      strategyGraphSha256: partnerStrategyGraphSha256(strategyId),
      builderRouteSpecSha256: routeSpecSha256(route),
      intentSha256: intentDigest,
      transaction: {
        cluster: "mainnet-beta",
        operation: "initialize-strategy",
        instructionSequence: ["manager=setup-admin", `initialize ${strategyId} USDC strategy`, "manager=Squads PDA"],
        vault: route.vault,
        finalManager: route.squads.manager,
        strategyId,
        reserve: identity.reserve,
        lendingMarket: route.strategy.lendingMarket,
        collateralFarm: route.strategy.collateralFarm,
        strategyReceipt: accounts.strategyInitReceipt,
        expectedAssetMovementRaw: "0",
        packetBytes: prepared.packetBytes,
        feeLamports: prepared.feeLamports,
        expectedCreatedRentLamports,
        maxTotalLamports: MAX_STRATEGY_BOOTSTRAP_LAMPORTS,
        expectedSignature: prepared.expectedSignature,
        canonicalMessageSha256,
        instructionDataSha256: [setManager.canonical.dataSha256, initialize.canonical.dataSha256, restoreManager.canonical.dataSha256],
      },
      simulation: { prestateSlot: prepared.prestateSlot, contextSlot: prepared.simulationSlot, err: prepared.simulation.err, unitsConsumed: prepared.simulation.unitsConsumed },
      deployments: { before: deployments.identities, after: deploymentsAfter.identities },
      failedGateCount,
      gates,
    },
    prestate: { addresses: inspectedAddresses, accounts: before.accounts, contextSlot: before.contextSlot },
    deploymentsBefore: deployments,
    deploymentsAfter,
    expectedCreatedRentLamports,
  } as const;
}

export async function simulateStrategyBootstrap(strategyId: PartnerStrategyId) {
  return (await prepareStrategyBootstrap(strategyId)).report;
}

export async function executeStrategyBootstrap(input: StrategyBootstrapExecutionConfirmation) {
  const route = partnerBuilderRoute(input.strategyId);
  const identity = partnerStrategyIdentity(input.strategyId);
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("execute initialize-strategy requires CONFIRM_MAINNET=1");
  if (input.strategyId === "main") throw new Error("Main strategy is already initialized and is not in the six-operation bootstrap authorization");
  if (!input.authorizationPath) throw new Error("execute initialize-strategy requires --authorization");
  const authorization = loadBootstrapExecutionAuthorization(
    input.authorizationPath,
    input.confirmAuthorizationSha256,
  );
  if (authorization.routeId !== "loyal-backyard-four-market-usdc-v1" || authorization.genesisHash !== PARTNER_ROUTE.genesisHash) throw new Error("bootstrap authorization route or mainnet genesis is not exact");
  const approved = operationAuthorization(authorization, "initialize-strategy", input.strategyId);
  const staticFacts = await strategyBootstrapAuthorizationFacts(input.strategyId);
  const expectedApproved = {
    reserve: identity.reserve,
    vault: route.vault,
    setupAdmin: route.setupAdmin,
    strategyAuth: staticFacts.accounts.strategyAuth,
    strategyInitReceipt: staticFacts.accounts.strategyInitReceipt,
    strategyAssetAta: identity.voltr.strategyAssetAta,
    fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
    strategyGraphSha256: partnerStrategyGraphSha256(input.strategyId),
    builderRouteSpecSha256: routeSpecSha256(route),
    instructionDataSha256: {
      setManager: staticFacts.instructionDataSha256[0],
      initializeStrategy: staticFacts.instructionDataSha256[1],
      restoreManager: staticFacts.instructionDataSha256[2],
    },
    maxTotalLamports: MAX_STRATEGY_BOOTSTRAP_LAMPORTS.toString(),
  };
  if (!sameJson({ ...approved, operation: undefined, strategyId: undefined }, expectedApproved)) throw new Error(`bootstrap authorization semantics do not match initialize-strategy:${input.strategyId}`);
  if (input.confirmStrategyId !== input.strategyId) throw new Error(`execute initialize-strategy requires --confirm-strategy-id ${input.strategyId}`);
  if (input.confirmReserve !== identity.reserve) throw new Error(`execute initialize-strategy requires --confirm-reserve ${identity.reserve}`);
  if (input.confirmVault !== PARTNER_ROUTE.vault) throw new Error(`execute initialize-strategy requires --confirm-vault ${PARTNER_ROUTE.vault}`);
  if (input.confirmFourMarketRouteSpecSha256 !== fourMarketRouteSpecSha256()) throw new Error(`execute initialize-strategy requires --confirm-four-market-route-spec-sha256 ${fourMarketRouteSpecSha256()}`);
  if (input.confirmBuilderRouteSpecSha256 !== routeSpecSha256(route)) throw new Error(`execute initialize-strategy requires --confirm-builder-route-spec-sha256 ${routeSpecSha256(route)}`);
  if (input.confirmMaxTotalLamports !== MAX_STRATEGY_BOOTSTRAP_LAMPORTS.toString()) throw new Error(`execute initialize-strategy requires --confirm-max-total-lamports ${MAX_STRATEGY_BOOTSTRAP_LAMPORTS}`);

  // Derive every graph/instruction approval without loading signer material.
  const unsigned = staticFacts;
  const unsignedHashes = unsigned.instructionDataSha256;
  if (input.confirmSetManagerDataSha256 !== unsignedHashes[0]) throw new Error(`execute initialize-strategy requires --confirm-set-manager-data-sha256 ${unsignedHashes[0]}`);
  if (input.confirmInitializeStrategyDataSha256 !== unsignedHashes[1]) throw new Error(`execute initialize-strategy requires --confirm-initialize-strategy-data-sha256 ${unsignedHashes[1]}`);
  if (input.confirmRestoreManagerDataSha256 !== unsignedHashes[2]) throw new Error(`execute initialize-strategy requires --confirm-restore-manager-data-sha256 ${unsignedHashes[2]}`);

  const preparation = await prepareStrategyBootstrap(input.strategyId);
  const expectedInstructionHashes = preparation.report.transaction.instructionDataSha256;
  if (input.confirmSetManagerDataSha256 !== expectedInstructionHashes[0]) throw new Error(`execute initialize-strategy requires --confirm-set-manager-data-sha256 ${expectedInstructionHashes[0]}`);
  if (input.confirmInitializeStrategyDataSha256 !== expectedInstructionHashes[1]) throw new Error(`execute initialize-strategy requires --confirm-initialize-strategy-data-sha256 ${expectedInstructionHashes[1]}`);
  if (input.confirmRestoreManagerDataSha256 !== expectedInstructionHashes[2]) throw new Error(`execute initialize-strategy requires --confirm-restore-manager-data-sha256 ${expectedInstructionHashes[2]}`);
  if (!preparation.report.readyForBroadcast || preparation.report.failedGateCount !== 0) throw new Error(`strategy bootstrap preflight failed with ${preparation.report.verdict}`);

  const protectedAddresses = [...preparation.prestate.addresses];
  const refreshed = await confirmedSnapshots(rpcUrl(), protectedAddresses, preparation.prepared.simulationSlot);
  const refreshedReserve = await loadSelectedReserve(input.strategyId, refreshed.contextSlot);
  const deployments = await loadDeploymentIdentities(rpcUrl(), route, refreshedReserve.reserve.contextSlot, "confirmed");
  const vault = verifyVaultCurrentState({ route, accounts: preparation.accounts, vault: refreshed.accounts[0] ?? null, lpMint: refreshed.accounts[1] ?? null, idleAta: refreshed.accounts[2] ?? null, assetMint: refreshed.accounts[3] ?? null });
  const refreshedReserveTransition = reserveRefreshTransition(
    preparation.prestate.accounts[9]?.data,
    refreshed.accounts[9]?.data,
  );
  // Kamino reserve bytes are shared mutable state; every other protected
  // account must be byte-for-byte identical to the signed preflight snapshot.
  const semanticallyRefreshedAddresses = new Set<string>([
    preparation.reserve.graph.reserve,
    route.asset.mint,
  ]);
  const changedProtectedAddresses = preparation.prestate.addresses.filter((addressValue, index) =>
    !semanticallyRefreshedAddresses.has(addressValue) && !sameJson(
      accountFingerprint(preparation.prestate.accounts[index] ?? null),
      accountFingerprint(refreshed.accounts[index] ?? null),
    ));
  const prestateMatches = changedProtectedAddresses.length === 0;
  if (
    !prestateMatches
    || refreshedReserveTransition?.onlyApprovedRefreshFieldsChanged !== true
    || !refreshedReserveTransition.lastUpdateSlotAdvanced
    || !refreshedReserveTransition.markerSemanticsExact
    || !refreshedReserveTransition.accrualsMonotonic
    || !refreshedReserveTransition.oracleRefreshSafe
    || vault.verdict !== "PARTNER_VAULT_CURRENT_PASS"
    || !verifyAdaptorReceipt(route, preparation.accounts.adaptorAddReceipt, refreshed.accounts[4] ?? null).every(({ pass }) => pass)
    || refreshed.accounts.slice(5, 9).some((account) => account !== null)
  ) {
    throw new Error(`strategy bootstrap protected state changed after simulation (${changedProtectedAddresses.join(", ") || "semantic refresh gate"}); refusing send`);
  }
  if (
    !sameJson(refreshedReserve.reserve.graph, preparation.reserve.graph)
    || !verifyDeploymentIdentities(route, deployments.identities).every(({ pass }) => pass)
    || !sameJson(preparation.deploymentsBefore.identities, deployments.identities)
  ) {
    throw new Error("strategy graph or deployment identity changed after simulation; refusing send");
  }
  const authorizationContextSlot = Math.max(
    preparation.prepared.simulationSlot,
    refreshed.contextSlot,
    refreshedReserve.reserve.contextSlot,
    deployments.contextSlot,
  );
  let confirmed: Awaited<ReturnType<typeof sendPreparedConfirmedOnce>> | null = null;
  try {
    confirmed = await sendPreparedConfirmedOnce(
      rpcUrl(),
      preparation.prepared,
      authorizationContextSlot,
    );
    if (confirmed.err !== null) {
      return { verdict: "PARTNER_STRATEGY_BOOTSTRAP_CONFIRMED_WITH_ERROR", broadcast: true, authorizationContextSlot, preflight: preparation.report, confirmed } as const;
    }
    const stateSnapshot = await confirmedSnapshots(rpcUrl(), protectedAddresses, confirmed.confirmedSlot);
    const state = { ...stateSnapshot, addresses: protectedAddresses };
    const vaultReadback = verifyVaultCurrentState({ route, accounts: preparation.accounts, vault: state.accounts[0] ?? null, lpMint: state.accounts[1] ?? null, idleAta: state.accounts[2] ?? null, assetMint: state.accounts[3] ?? null });
    const adaptorGates = verifyAdaptorReceipt(route, preparation.accounts.adaptorAddReceipt, state.accounts[4] ?? null);
    const strategyGates = verifyStrategyBootstrap({ route, accounts: preparation.accounts, graph: preparation.reserve.graph, strategyReceipt: state.accounts[5] ?? null, userMetadata: state.accounts[6] ?? null, obligation: state.accounts[7] ?? null, obligationFarm: state.accounts[8] ?? null });
    const finalReserve = await loadSelectedReserve(input.strategyId, state.contextSlot);
    const finalDeployments = await loadDeploymentIdentities(rpcUrl(), route, finalReserve.reserve.contextSlot, "confirmed");
    const readbackGates: Gate[] = [];
    const createdAddresses = [
      preparation.accounts.strategyInitReceipt,
      preparation.reserve.graph.userMetadata,
      preparation.reserve.graph.obligation,
      preparation.reserve.graph.obligationFarm,
    ] as const;
    const lamportDeltaByAddress = new Map(confirmed.lamportDeltas.map((row) => [row.address, BigInt(row.deltaRaw)]));
    const expectedPayerDebit = BigInt(confirmed.feeLamports ?? 0) + BigInt(preparation.expectedCreatedRentLamports);
    add(readbackGates, "confirmed state context is at or after transaction", state.contextSlot >= confirmed.confirmedSlot, state.contextSlot, `>=${confirmed.confirmedSlot}`);
    add(readbackGates, "confirmed fee matches the signed quote", confirmed.feeLamports === preparation.prepared.feeLamports, confirmed.feeLamports, preparation.prepared.feeLamports);
    add(readbackGates, "confirmed setup-admin debit is exact fee plus rent", lamportDeltaByAddress.get(route.setupAdmin) === -expectedPayerDebit, lamportDeltaByAddress.get(route.setupAdmin) ?? null, -expectedPayerDebit);
    add(readbackGates, "confirmed created-account rent deltas are exact", createdAddresses.every((account) => {
      const index = protectedAddresses.indexOf(account);
      return index >= 0 && lamportDeltaByAddress.get(account) === BigInt(state.accounts[index]?.lamports ?? 0);
    }), createdAddresses.map((account) => ({ account, delta: lamportDeltaByAddress.get(account) ?? null, readbackLamports: state.accounts[protectedAddresses.indexOf(account)]?.lamports ?? null })), "each created account delta equals its confirmed rent-exempt lamports");
    add(readbackGates, "confirmed bootstrap has no token movement", confirmed.tokenDeltas.every(({ deltaRaw }) => deltaRaw === "0"), confirmed.tokenDeltas, []);
    const allowedLamportChanges = new Set<string>([route.setupAdmin, ...createdAddresses]);
    add(readbackGates, "confirmed bootstrap has no unrelated lamport movement", confirmed.lamportDeltas.every(({ address: account, deltaRaw }) => deltaRaw === "0" || allowedLamportChanges.has(account)), confirmed.lamportDeltas.filter(({ deltaRaw }) => deltaRaw !== "0"), [...allowedLamportChanges]);
    add(readbackGates, "confirmed selected reserve graph identities unchanged", sameJson(finalReserve.reserve.graph, preparation.reserve.graph), finalReserve.reserve.graph, preparation.reserve.graph);
    add(readbackGates, "confirmed deployment identities unchanged", sameJson(preparation.deploymentsBefore.identities, finalDeployments.identities), finalDeployments.identities, preparation.deploymentsBefore.identities);
    const confirmedVaultTransition = vaultUpdateTransition(
      refreshed.accounts[0]?.data,
      state.accounts[0]?.data,
    );
    add(
      readbackGates,
      "confirmed vault changes only its last-updated timestamp",
      confirmedVaultTransition?.onlyLastUpdatedTsChanged === true
        && confirmedVaultTransition.timestampAdvanced,
      confirmedVaultTransition,
      "all encoded vault bytes exact after normalizing lastUpdatedTs",
    );
    const confirmedReserveTransition = reserveRefreshTransition(
      refreshed.accounts[9]?.data,
      state.accounts[9]?.data,
    );
    add(
      readbackGates,
      "confirmed reserve changes only its refresh marker",
      confirmedReserveTransition?.onlyApprovedRefreshFieldsChanged === true
        && confirmedReserveTransition.lastUpdateSlotAdvanced
        && confirmedReserveTransition.markerSemanticsExact
        && confirmedReserveTransition.accrualsMonotonic
        && confirmedReserveTransition.oracleRefreshSafe,
      confirmedReserveTransition,
      "all decoded reserve fields exact except monotonic Kamino refresh accruals and lastUpdate",
    );
    const failedGateCount = vaultReadback.failedGateCount
      + adaptorGates.filter(({ pass }) => !pass).length
      + strategyGates.filter(({ pass }) => !pass).length
      + readbackGates.filter(({ pass }) => !pass).length;
    return {
      verdict: failedGateCount === 0
        ? "PARTNER_STRATEGY_BOOTSTRAP_CONFIRMED_AND_VERIFIED"
        : "PARTNER_STRATEGY_BOOTSTRAP_CONFIRMED_READBACK_FAIL",
      broadcast: true,
      authorizationContextSlot,
      intent: preparation.intent,
      intentSha256: preparation.intentSha256,
      preflight: preparation.report,
      confirmed,
      readbackContextSlot: state.contextSlot,
      readback: { vault: vaultReadback, adaptor: adaptorGates, strategy: strategyGates, gates: readbackGates, failedGateCount },
    } as const;
  } catch (error) {
    if (confirmed) {
      return { verdict: "PARTNER_STRATEGY_BOOTSTRAP_CONFIRMED_READBACK_ERROR", broadcast: true, authorizationContextSlot, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, confirmed, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. The transaction is confirmed; rerun read-only strategy reconciliation." } as const;
    }
    return { verdict: "PARTNER_STRATEGY_BOOTSTRAP_BROADCAST_STATUS_UNKNOWN", broadcast: null, authorizationContextSlot, expectedSignature: preparation.prepared.expectedSignature, intent: preparation.intent, intentSha256: preparation.intentSha256, preflight: preparation.report, error: error instanceof Error ? error.message : String(error), recoveryInstruction: "Do not resend. Verify this exact signature and reload the strategy accounts." } as const;
  }
}
