import { createHash } from "node:crypto";

import { AccountLayout, TOKEN_PROGRAM_ID } from "@solana/spl-token";
import { Obligation, Reserve } from "@kamino-finance/klend-sdk";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { address, createNoopSigner, type Instruction } from "@solana/kit";
import {
  getProtocolDecoder,
  getVaultDecoder,
} from "@voltr/vault-sdk";
import {
  Connection,
  PublicKey,
  TransactionInstruction,
} from "@solana/web3.js";

import {
  RWA_MULTIPLY_ROUTE,
  rwaMultiplyRouteSpecSha256,
} from "../domain/rwa-multiply-route-spec.js";
import {
  prepareSignedV0Transaction,
  toWeb3Instruction,
} from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  deriveRwaMultiplyVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import {
  buildRwaMultiplyManagerInstructions,
  buildRwaMultiplyBridgeApprovalInstruction,
  buildRwaMultiplyVoltrSetup,
  deriveRwaMultiplyVoltrAccounts,
} from "../integrations/rwa-multiply-voltr.js";
import { verifyInstalledCustomPolicies } from "../policies/rwa-multiply-custom.js";

type Gate = Readonly<{
  name: string;
  pass: boolean;
  observed: unknown;
  expected: unknown;
}>;

type JupiterJson = Record<string, unknown>;
type SquadsSettingsState = Readonly<{ policySeed: { toString(): string } | null }>;
type SquadsPolicyState = Readonly<{
  settings: PublicKey;
  seed: { toString(): string };
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  policyState: Readonly<{
    __kind: string;
    fields?: readonly Readonly<{
      preHook?: unknown;
      postHook?: unknown;
      instructionsConstraints?: readonly Readonly<{ programId: PublicKey }>[];
    }>[];
  }>;
}>;

const SHARED_ACCOUNTS_ROUTE = Buffer.from("c1209b3341d69c81", "hex");
const PACKET_LIMIT = 1_232;
const SquadsSettings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [SquadsSettingsState, number] };
}).Settings;
const SquadsPolicy = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [SquadsPolicyState, number] };
}).Policy;

function add(
  gates: Gate[],
  name: string,
  pass: boolean,
  observed: unknown,
  expected: unknown,
): void {
  gates.push({ name, pass, observed, expected });
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function instructionAddresses(instruction: Instruction): readonly string[] {
  return (instruction.accounts ?? []).map(({ address: account }) => account);
}

function squadsPolicyAddress(seed: bigint): PublicKey {
  const bytes = Buffer.alloc(8);
  bytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"),
    Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(),
    bytes,
  ], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0];
}

function squadsV4VaultAddress(multisig: string, vaultIndex: number): PublicKey {
  return PublicKey.findProgramAddressSync([
    Buffer.from("multisig"),
    new PublicKey(multisig).toBuffer(),
    Buffer.from("vault"),
    Buffer.from([vaultIndex]),
  ], new PublicKey(RWA_MULTIPLY_ROUTE.voltrAdmission.squadsV4Program))[0];
}

async function scanDelegatedPolicies(
  connection: Connection,
  settingsInfo: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>,
) {
  const [settings] = SquadsSettings.fromAccountInfo(settingsInfo);
  const currentSeed = BigInt(settings.policySeed?.toString() ?? "0");
  const seeds = Array.from({ length: Number(currentSeed) }, (_, index) => BigInt(index + 1));
  const rows: Array<Readonly<{ seed: string; address: string; programs: readonly string[] }>> = [];
  for (let offset = 0; offset < seeds.length; offset += 90) {
    const chunk = seeds.slice(offset, offset + 90);
    const infos = await connection.getMultipleAccountsInfo(chunk.map(squadsPolicyAddress), "confirmed");
    for (const [index, info] of infos.entries()) {
      if (!info?.owner.equals(new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))) continue;
      let policy: SquadsPolicyState;
      try { [policy] = SquadsPolicy.fromAccountInfo(info); } catch { continue; }
      const body = policy.policyState.fields?.[0];
      const delegated = policy.settings.equals(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings))
        && policy.threshold === 1
        && policy.timeLock === 0
        && policy.signers.length === 1
        && policy.signers[0]!.key.equals(new PublicKey(RWA_MULTIPLY_ROUTE.squads.delegatedExecutor))
        && policy.signers[0]!.permissions.mask === 7
        && policy.policyState.__kind === "ProgramInteraction"
        && body?.preHook == null
        && body?.postHook == null;
      if (!delegated) continue;
      rows.push({
        seed: chunk[index]!.toString(),
        address: squadsPolicyAddress(chunk[index]!).toBase58(),
        programs: body?.instructionsConstraints?.map(({ programId }) => programId.toBase58()) ?? [],
      });
    }
  }
  return { currentSeed: currentSeed.toString(), rows };
}

function jsonRecord(value: unknown, label: string): JupiterJson {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} is not an object`);
  }
  return value as JupiterJson;
}

function jsonString(value: unknown, label: string): string {
  if (typeof value !== "string") throw new Error(`${label} is not a string`);
  return value;
}

function jsonArray(value: unknown, label: string): readonly unknown[] {
  if (!Array.isArray(value)) throw new Error(`${label} is not an array`);
  return value;
}

async function fetchJupiterLeg(input: Readonly<{
  connection: Connection;
  inputMint: PublicKey;
  outputMint: PublicKey;
  sourceAta: PublicKey;
  destinationAta: PublicKey;
  amountRaw: bigint;
  label: "forward" | "reverse";
}>): Promise<Readonly<{
  label: "forward" | "reverse";
  quote: Readonly<{
    inAmountRaw: string;
    outAmountRaw: string;
    otherAmountThresholdRaw: string;
    routePlanLength: number;
  }>;
  instruction: TransactionInstruction;
  instructionSha256: string;
  setupInstructionCount: number;
  cleanupInstructionPresent: boolean;
  otherInstructionCount: number;
  lookupTables: readonly string[];
  lookupTablesResolved: boolean;
  fixedHeaderPass: boolean;
}>> {
  const route = RWA_MULTIPLY_ROUTE;
  const params = new URLSearchParams({
    inputMint: input.inputMint.toBase58(),
    outputMint: input.outputMint.toBase58(),
    amount: input.amountRaw.toString(),
    slippageBps: String(route.assets.maxSlippageBps),
    swapMode: "ExactIn",
    maxAccounts: "64",
  });
  const quoteResponse = await fetch(
    `https://lite-api.jup.ag/swap/v1/quote?${params}`,
    { signal: AbortSignal.timeout(20_000) },
  );
  const quote = jsonRecord(await quoteResponse.json(), `${input.label} quote`);
  if (!quoteResponse.ok) throw new Error(`${input.label} Jupiter quote failed`);
  const inAmount = jsonString(quote.inAmount, `${input.label} quote.inAmount`);
  const outAmount = jsonString(quote.outAmount, `${input.label} quote.outAmount`);
  const threshold = jsonString(
    quote.otherAmountThreshold,
    `${input.label} quote.otherAmountThreshold`,
  );
  const routePlan = jsonArray(quote.routePlan, `${input.label} quote.routePlan`);
  if (
    quote.inputMint !== input.inputMint.toBase58()
    || quote.outputMint !== input.outputMint.toBase58()
    || inAmount !== input.amountRaw.toString()
    || quote.swapMode !== "ExactIn"
    || BigInt(outAmount) <= 0n
    || BigInt(threshold) <= 0n
    || routePlan.length < 1
  ) {
    throw new Error(`${input.label} Jupiter quote identity/economics drifted`);
  }
  const instructionResponse = await fetch(
    "https://lite-api.jup.ag/swap/v1/swap-instructions",
    {
      method: "POST",
      headers: { "content-type": "application/json" },
      signal: AbortSignal.timeout(20_000),
      body: JSON.stringify({
        userPublicKey: route.squads.vault,
        quoteResponse: quote,
        wrapAndUnwrapSol: false,
        useSharedAccounts: true,
        dynamicComputeUnitLimit: false,
      }),
    },
  );
  const body = jsonRecord(
    await instructionResponse.json(),
    `${input.label} swap-instructions`,
  );
  if (!instructionResponse.ok) {
    throw new Error(`${input.label} Jupiter swap-instructions failed`);
  }
  const raw = jsonRecord(body.swapInstruction, `${input.label} swapInstruction`);
  const rawAccounts = jsonArray(raw.accounts, `${input.label} swapInstruction.accounts`);
  const instruction = new TransactionInstruction({
    programId: new PublicKey(jsonString(raw.programId, `${input.label} programId`)),
    data: Buffer.from(jsonString(raw.data, `${input.label} data`), "base64"),
    keys: rawAccounts.map((entry, index) => {
      const account = jsonRecord(entry, `${input.label} account ${index}`);
      if (typeof account.isSigner !== "boolean" || typeof account.isWritable !== "boolean") {
        throw new Error(`${input.label} account ${index} flags are malformed`);
      }
      return {
        pubkey: new PublicKey(jsonString(account.pubkey, `${input.label} account ${index}`)),
        isSigner: account.isSigner,
        isWritable: account.isWritable,
      };
    }),
  });
  const [programAuthority] = PublicKey.findProgramAddressSync(
    [Buffer.from("authority"), Buffer.from([instruction.data[8] ?? 0])],
    new PublicKey(route.programs.jupiter),
  );
  const fixedHeaderPass =
    instruction.programId.equals(new PublicKey(route.programs.jupiter))
    && instruction.data.subarray(0, 8).equals(SHARED_ACCOUNTS_ROUTE)
    && instruction.data.length >= 37
    && instruction.keys[0]?.pubkey.equals(TOKEN_PROGRAM_ID) === true
    && instruction.keys[1]?.pubkey.equals(programAuthority) === true
    && instruction.keys[2]?.pubkey.equals(new PublicKey(route.squads.vault)) === true
    && instruction.keys[2]?.isSigner === true
    && instruction.keys[3]?.pubkey.equals(input.sourceAta) === true
    && instruction.keys[3]?.isWritable === true
    && instruction.keys[6]?.pubkey.equals(input.destinationAta) === true
    && instruction.keys[6]?.isWritable === true
    && instruction.keys[7]?.pubkey.equals(input.inputMint) === true
    && instruction.keys[8]?.pubkey.equals(input.outputMint) === true
    && instruction.keys[9]?.pubkey.equals(new PublicKey(route.programs.jupiter)) === true
    && instruction.keys[10]?.pubkey.equals(new PublicKey(route.programs.jupiter)) === true
    && instruction.data.readBigUInt64LE(instruction.data.length - 19) === input.amountRaw
    && instruction.data.readBigUInt64LE(instruction.data.length - 11) === BigInt(outAmount)
    && instruction.data.readUInt16LE(instruction.data.length - 3) <= route.assets.maxSlippageBps
    && instruction.data[instruction.data.length - 1] === 0
    && instruction.keys.every((key, index) => !key.isSigner || index === 2)
    && !instruction.keys.some((key) => key.pubkey.equals(new PublicKey(route.previousBackyardVault)));
  const lookupTables = Array.isArray(body.addressLookupTableAddresses)
    ? body.addressLookupTableAddresses.map((value, index) =>
        jsonString(value, `${input.label} ALT ${index}`))
    : [];
  const resolved = await Promise.all(
    lookupTables.map((value) =>
      input.connection.getAddressLookupTable(new PublicKey(value), { commitment: "confirmed" })),
  );
  return {
    label: input.label,
    quote: {
      inAmountRaw: inAmount,
      outAmountRaw: outAmount,
      otherAmountThresholdRaw: threshold,
      routePlanLength: routePlan.length,
    },
    instruction,
    instructionSha256: sha256(Buffer.concat([
      instruction.programId.toBuffer(),
      Buffer.from(instruction.data),
      ...instruction.keys.flatMap((key) => [
        key.pubkey.toBuffer(),
        Buffer.from([Number(key.isSigner), Number(key.isWritable)]),
      ]),
    ])),
    setupInstructionCount: Array.isArray(body.setupInstructions)
      ? body.setupInstructions.length
      : 0,
    cleanupInstructionPresent: body.cleanupInstruction !== null
      && body.cleanupInstruction !== undefined,
    otherInstructionCount: Array.isArray(body.otherInstructions)
      ? body.otherInstructions.length
      : 0,
    lookupTables,
    lookupTablesResolved: resolved.every(({ value }) => value !== null),
    fixedHeaderPass,
  };
}

async function run(): Promise<Readonly<Record<string, unknown>>> {
  const route = RWA_MULTIPLY_ROUTE;
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) {
    return {
      verdict: "BLOCKED",
      blocker: "SOLANA_RPC_URL is absent",
      resumeCondition: "Mount .env.1password and rerun through op run.",
    };
  }
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (admin.signer.address !== route.setupAdmin) {
    return {
      verdict: "FAIL",
      failedCheck: "setup admin identity",
      observed: admin.signer.address,
      expected: route.setupAdmin,
    };
  }
  const vault = await deriveRwaMultiplyVaultSigningMaterial(admin, route);
  const strategy = await deriveRwaMultiplyStrategySigningMaterial(admin, route);
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: vault.signer,
    strategyConfig: strategy.signer,
  }, route);
  const manager = createNoopSigner(route.squads.vault);
  const managerInstructions = await buildRwaMultiplyManagerInstructions(
    manager,
    route.vault.proofAmountRaw,
    {
      sequence: 1n,
      observedSlot: 1n,
      navAfterRaw: 0n,
      snapshotDigest: new Uint8Array(32).fill(1),
    },
    route,
  );
  const bridgeApproval = await buildRwaMultiplyBridgeApprovalInstruction(route);
  const connection = new Connection(rpcUrl, { commitment: "confirmed" });
  const genesisHash = await connection.getGenesisHash();
  const protectedAddresses = [
    route.vault.address,
    route.previousBackyardVault,
    route.customAdaptor.strategyConfig,
    accounts.adaptorAddReceipt,
    accounts.strategyInitReceipt,
    route.squads.settings,
    route.squads.vault,
    route.squads.assetAta,
    route.squads.collateralAta,
    route.kamino.obligation,
    route.kamino.market,
    route.kamino.collateralReserve,
    route.kamino.debtReserve,
    route.customAdaptor.program,
    accounts.protocol,
    route.voltrAdmission.multisig,
  ];
  const live = await connection.getMultipleAccountsInfoAndContext(
    protectedAddresses.map((value) => new PublicKey(value)),
    { commitment: "confirmed" },
  );
  const [
    newVaultInfo,
    previousBackyardInfo,
    strategyInfo,
    adaptorReceiptInfo,
    strategyReceiptInfo,
    settingsInfo,
    squadsVaultInfo,
    squadsAssetInfo,
    squadsCollateralInfo,
    obligationInfo,
    marketInfo,
    collateralReserveInfo,
    debtReserveInfo,
    adaptorInfo,
    protocolInfo,
    voltrAdmissionMultisigInfo,
  ] = live.value;
  const allBuiltInstructions = [
    ...Object.values(setup.instructions),
    managerInstructions.deposit,
    managerInstructions.withdraw,
    bridgeApproval,
  ];
  const gates: Gate[] = [];
  const activationGates: Gate[] = [];
  add(gates, "mainnet genesis exact", genesisHash === route.genesisHash, genesisHash, route.genesisHash);
  add(gates, "new vault differs from prior Backyard vault", String(route.vault.address) !== String(route.previousBackyardVault), route.vault.address, `not ${route.previousBackyardVault}`);
  add(gates, "new deterministic vault signer exact", vault.signer.address === route.vault.address, vault.signer.address, route.vault.address);
  add(gates, "new deterministic strategy signer exact", strategy.signer.address === route.customAdaptor.strategyConfig, strategy.signer.address, route.customAdaptor.strategyConfig);
  add(gates, "all setup/runtime instructions exclude prior Backyard vault", allBuiltInstructions.every((instruction) => !instructionAddresses(instruction).includes(route.previousBackyardVault)), allBuiltInstructions.flatMap(instructionAddresses).filter((value) => value === route.previousBackyardVault), []);
  add(gates, "custom adaptor setup ABI exact", (setup.instructions.initializeConfig.accounts?.length ?? 0) === 13 && (setup.instructions.initializeConfig.data?.length ?? 0) === 25 && (setup.instructions.initializeStrategy.accounts?.length ?? 0) === 16, { initializeConfigAccounts: setup.instructions.initializeConfig.accounts?.length ?? 0, initializeConfigDataBytes: setup.instructions.initializeConfig.data?.length ?? 0, initializeStrategyAccounts: setup.instructions.initializeStrategy.accounts?.length ?? 0 }, { initializeConfigAccounts: 13, initializeConfigDataBytes: 25, initializeStrategyAccounts: 16 });
  add(gates, "custom adaptor runtime ABI exact", (managerInstructions.deposit.accounts?.length ?? 0) === 17 && (managerInstructions.withdraw.accounts?.length ?? 0) === 17 && managerInstructions.deposit.programAddress === route.programs.voltr && managerInstructions.withdraw.programAddress === route.programs.voltr, { depositAccounts: managerInstructions.deposit.accounts?.length ?? 0, withdrawAccounts: managerInstructions.withdraw.accounts?.length ?? 0, depositProgram: managerInstructions.deposit.programAddress, withdrawProgram: managerInstructions.withdraw.programAddress }, { depositAccounts: 17, withdrawAccounts: 17, program: route.programs.voltr });
  add(gates, "one-time bridge delegate approval exact", bridgeApproval.programAddress === route.assets.tokenProgram && (bridgeApproval.accounts?.length ?? 0) === 4 && bridgeApproval.accounts?.[0]?.address === route.squads.assetAta && bridgeApproval.accounts?.[2]?.address === accounts.strategyAuth && bridgeApproval.accounts?.[3]?.address === route.squads.vault, { program: bridgeApproval.programAddress, accounts: bridgeApproval.accounts?.map(({ address: account }) => account) ?? [] }, { program: route.assets.tokenProgram, source: route.squads.assetAta, delegate: accounts.strategyAuth, owner: route.squads.vault });
  add(gates, "previous Backyard vault remains present and read-only", previousBackyardInfo?.owner.equals(new PublicKey(route.programs.voltr)) === true, previousBackyardInfo ? { owner: previousBackyardInfo.owner.toBase58(), dataSha256: sha256(previousBackyardInfo.data) } : null, { owner: route.programs.voltr });
  add(gates, "custom adaptor deployed executable", adaptorInfo?.executable === true, adaptorInfo ? { owner: adaptorInfo.owner.toBase58(), executable: adaptorInfo.executable } : null, { executable: true });
  add(gates, "Voltr protocol account identity", protocolInfo?.owner.equals(new PublicKey(route.programs.voltr)) === true, protocolInfo?.owner.toBase58() ?? null, route.programs.voltr);
  const decodedProtocol = protocolInfo ? getProtocolDecoder().decode(protocolInfo.data) : null;
  const derivedVoltrAdmissionVault = squadsV4VaultAddress(
    route.voltrAdmission.multisig,
    route.voltrAdmission.vaultIndex,
  );
  add(gates, "Voltr protocol admin is exact independent Squads v4 vault", decodedProtocol?.admin === route.voltrAdmission.protocolAdmin
    && derivedVoltrAdmissionVault.equals(new PublicKey(route.voltrAdmission.protocolAdmin))
    && PublicKey.isOnCurve(new PublicKey(route.voltrAdmission.protocolAdmin).toBytes()) === false
    && voltrAdmissionMultisigInfo?.owner.equals(new PublicKey(route.voltrAdmission.squadsV4Program)) === true
    && String(route.voltrAdmission.squadsV4Program) !== String(route.squads.program)
    && String(route.voltrAdmission.protocolAdmin) !== String(route.squads.vault),
  {
    protocolAdmin: decodedProtocol?.admin ?? null,
    derivedVault: derivedVoltrAdmissionVault.toBase58(),
    vaultIndex: route.voltrAdmission.vaultIndex,
    multisigOwner: voltrAdmissionMultisigInfo?.owner.toBase58() ?? null,
    loyalSmartAccountsProgram: route.squads.program,
    loyalCapitalVault: route.squads.vault,
  }, {
    protocolAdmin: route.voltrAdmission.protocolAdmin,
    derivedVault: route.voltrAdmission.protocolAdmin,
    multisigOwner: route.voltrAdmission.squadsV4Program,
    distinctFromLoyalSmartAccount: true,
  });
  add(gates, "Squads Settings identity", settingsInfo?.owner.equals(new PublicKey(route.squads.program)) === true, settingsInfo?.owner.toBase58() ?? null, route.squads.program);
  add(gates, "Squads vault is PDA wallet", squadsVaultInfo?.owner.equals(new PublicKey(route.programs.system)) === true && squadsVaultInfo.data.length === 0, squadsVaultInfo ? { owner: squadsVaultInfo.owner.toBase58(), dataBytes: squadsVaultInfo.data.length } : null, { owner: route.programs.system, dataBytes: 0 });
  const collateralReserve = collateralReserveInfo ? Reserve.decode(collateralReserveInfo.data) : null;
  const debtReserve = debtReserveInfo ? Reserve.decode(debtReserveInfo.data) : null;
  add(gates, "exact Kamino market and reserve graph deployed", marketInfo?.owner.equals(new PublicKey(route.kamino.program)) === true
    && collateralReserveInfo?.owner.equals(new PublicKey(route.kamino.program)) === true
    && debtReserveInfo?.owner.equals(new PublicKey(route.kamino.program)) === true
    && String(collateralReserve?.lendingMarket) === route.kamino.market
    && String(debtReserve?.lendingMarket) === route.kamino.market
    && String(collateralReserve?.liquidity.mintPubkey) === route.assets.collateralMint
    && String(debtReserve?.liquidity.mintPubkey) === route.assets.assetMint,
  {
    market: { address: route.kamino.market, owner: marketInfo?.owner.toBase58() ?? null },
    collateralReserve: {
      address: route.kamino.collateralReserve,
      owner: collateralReserveInfo?.owner.toBase58() ?? null,
      lendingMarket: collateralReserve ? String(collateralReserve.lendingMarket) : null,
      liquidityMint: collateralReserve ? String(collateralReserve.liquidity.mintPubkey) : null,
    },
    debtReserve: {
      address: route.kamino.debtReserve,
      owner: debtReserveInfo?.owner.toBase58() ?? null,
      lendingMarket: debtReserve ? String(debtReserve.lendingMarket) : null,
      liquidityMint: debtReserve ? String(debtReserve.liquidity.mintPubkey) : null,
    },
  }, {
    owner: route.kamino.program,
    market: route.kamino.market,
    collateralReserve: route.kamino.collateralReserve,
    collateralMint: route.assets.collateralMint,
    debtReserve: route.kamino.debtReserve,
    debtMint: route.assets.assetMint,
  });
  const decodedObligation = obligationInfo ? Obligation.decode(obligationInfo.data) : null;
  add(gates, "fixed empty Kamino obligation identity exact", decodedObligation !== null
    && obligationInfo?.owner.equals(new PublicKey(route.kamino.program)) === true
    && decodedObligation.tag.toNumber() === 1
    && String(decodedObligation.owner) === route.squads.vault
    && String(decodedObligation.lendingMarket) === route.kamino.market
    && decodedObligation.deposits.every((entry) => entry.depositedAmount.isZero())
    && decodedObligation.borrows.every((entry) => entry.borrowedAmountSf.isZero()),
  decodedObligation ? {
    address: route.kamino.obligation,
    accountOwner: obligationInfo?.owner.toBase58() ?? null,
    positionOwner: String(decodedObligation.owner),
    lendingMarket: String(decodedObligation.lendingMarket),
    depositsEmpty: decodedObligation.deposits.every((entry) => entry.depositedAmount.isZero()),
    borrowsEmpty: decodedObligation.borrows.every((entry) => entry.borrowedAmountSf.isZero()),
  } : null, {
    address: route.kamino.obligation,
    accountOwner: route.kamino.program,
    positionOwner: route.squads.vault,
    lendingMarket: route.kamino.market,
    depositsEmpty: true,
    borrowsEmpty: true,
  });
  const lifecycleStateCoherent = newVaultInfo == null
    ? strategyInfo == null && adaptorReceiptInfo == null && strategyReceiptInfo == null
    : newVaultInfo.owner.equals(new PublicKey(route.programs.voltr));
  add(gates, "new vault activation state coherent", lifecycleStateCoherent, {
    newVaultOwner: newVaultInfo?.owner.toBase58() ?? null,
    strategyConfigOwner: strategyInfo?.owner.toBase58() ?? null,
    adaptorReceiptOwner: adaptorReceiptInfo?.owner.toBase58() ?? null,
    strategyReceiptOwner: strategyReceiptInfo?.owner.toBase58() ?? null,
  }, newVaultInfo == null ? "all activation accounts absent" : { newVaultOwner: route.programs.voltr });

  let initializeSimulation: Awaited<ReturnType<typeof prepareSignedV0Transaction>> | null = null;
  let admissionSimulation: Awaited<ReturnType<typeof prepareSignedV0Transaction>> | null = null;
  let protocolAdminHandoff: Readonly<Record<string, unknown>> | null = null;
  if (newVaultInfo == null) {
    initializeSimulation = await prepareSignedV0Transaction({
      rpcUrl,
      feePayer: admin,
      additionalSigners: [vault],
      instructions: [setup.instructions.initializeVault],
      inspectedAddresses: [
        route.vault.address,
        accounts.lpMint,
        accounts.idleAta,
        route.previousBackyardVault,
      ],
    });
    add(gates, "new vault signed-unsent initialization succeeds", initializeSimulation.simulation.err === null, initializeSimulation.simulation.err, null);
    add(gates, "new vault initialization packet fits", initializeSimulation.packetBytes <= PACKET_LIMIT, initializeSimulation.packetBytes, `<= ${PACKET_LIMIT}`);
    const simulatedPrevious = initializeSimulation.simulation.postAccounts[3] ?? null;
    const currentPrevious = previousBackyardInfo ?? null;
    add(gates, "new vault simulation leaves prior Backyard vault byte-exact", simulatedPrevious !== null && currentPrevious !== null && sha256(simulatedPrevious.data) === sha256(currentPrevious.data), simulatedPrevious ? sha256(simulatedPrevious.data) : null, currentPrevious ? sha256(currentPrevious.data) : null);
    admissionSimulation = await prepareSignedV0Transaction({
      rpcUrl,
      feePayer: admin,
      additionalSigners: [vault],
      instructions: [setup.instructions.initializeVault, setup.instructions.addAdaptor],
      inspectedAddresses: [
        route.vault.address,
        accounts.adaptorAddReceipt,
        route.previousBackyardVault,
      ],
    });
    add(activationGates, "new vault plus allowlisted custom-adaptor admission signed-unsent simulation succeeds", admissionSimulation.simulation.err === null, admissionSimulation.simulation.err, null);
    add(gates, "new vault plus custom-adaptor admission packet fits", admissionSimulation.packetBytes <= PACKET_LIMIT, admissionSimulation.packetBytes, `<= ${PACKET_LIMIT}`);
  } else {
    const decodedVault = getVaultDecoder().decode(newVaultInfo.data);
    add(gates, "active vault immutable identity exact", decodedVault.asset.mint === route.assets.assetMint
      && decodedVault.vaultConfiguration.maxCap === route.vault.capRaw
      && decodedVault.vaultConfiguration.withdrawalWaitingPeriod === route.vault.withdrawalWaitingPeriodSeconds
      && decodedVault.allowAnyAdaptor === 0,
    {
      assetMint: decodedVault.asset.mint,
      maxCap: decodedVault.vaultConfiguration.maxCap,
      withdrawalWaitingPeriod: decodedVault.vaultConfiguration.withdrawalWaitingPeriod,
      allowAnyAdaptor: decodedVault.allowAnyAdaptor,
    }, {
      assetMint: route.assets.assetMint,
      maxCap: route.vault.capRaw,
      withdrawalWaitingPeriod: route.vault.withdrawalWaitingPeriodSeconds,
      allowAnyAdaptor: 0,
    });
    if (!adaptorReceiptInfo) {
      admissionSimulation = await prepareSignedV0Transaction({
        rpcUrl,
        feePayer: admin,
        instructions: [setup.instructions.addAdaptor],
        inspectedAddresses: [
          route.vault.address,
          accounts.adaptorAddReceipt,
          route.previousBackyardVault,
        ],
      });
      add(activationGates, "active vault custom-adaptor admission signed-unsent simulation succeeds", admissionSimulation.simulation.err === null, admissionSimulation.simulation.err, null);
      add(gates, "active vault custom-adaptor admission packet fits", admissionSimulation.packetBytes <= PACKET_LIMIT, admissionSimulation.packetBytes, `<= ${PACKET_LIMIT}`);
      protocolAdminHandoff = {
        action: "make the exact custom adaptor program accepted by the deployed Voltr add_adaptor gate",
        adaptorProgram: route.customAdaptor.program,
        currentFailure: "AdaptorProgramNotWhitelisted (6016)",
        invariant: "this vault's allowAnyAdaptor remains 0",
        loyalFollowUp: "rerun the sole verifier, then execute the separately signed Loyal add_adaptor transaction exactly once",
      };
    }
  }

  const forward = await fetchJupiterLeg({
    connection,
    inputMint: new PublicKey(route.assets.assetMint),
    outputMint: new PublicKey(route.assets.collateralMint),
    sourceAta: new PublicKey(route.squads.assetAta),
    destinationAta: new PublicKey(route.squads.collateralAta),
    amountRaw: route.assets.jupiterProofInputRaw,
    label: "forward",
  });
  const reverse = await fetchJupiterLeg({
    connection,
    inputMint: new PublicKey(route.assets.collateralMint),
    outputMint: new PublicKey(route.assets.assetMint),
    sourceAta: new PublicKey(route.squads.collateralAta),
    destinationAta: new PublicKey(route.squads.assetAta),
    amountRaw: route.assets.jupiterProofInputRaw,
    label: "reverse",
  });
  add(gates, "Jupiter forward fixed custody header", forward.fixedHeaderPass, forward.instructionSha256, "SharedAccountsRoute with Squads vault and exact USDC/syrupUSDC ATAs");
  add(gates, "Jupiter reverse fixed custody header", reverse.fixedHeaderPass, reverse.instructionSha256, "SharedAccountsRoute with Squads vault and exact syrupUSDC/USDC ATAs");
  add(gates, "Jupiter ALTs resolve", forward.lookupTablesResolved && reverse.lookupTablesResolved, { forward: forward.lookupTables, reverse: reverse.lookupTables }, "all current quote ALTs resolve");
  const delegatedPolicyScan = settingsInfo
    ? await scanDelegatedPolicies(connection, settingsInfo)
    : { currentSeed: "0", rows: [] };
  const delegatedPolicyPrograms = new Set(delegatedPolicyScan.rows.flatMap(({ programs }) => programs));
  const customPolicies = await verifyInstalledCustomPolicies(connection);
  const delegatedProtocolsReady = delegatedPolicyPrograms.has(route.kamino.program)
    && delegatedPolicyPrograms.has(route.programs.jupiter);
  const delegatedPoliciesReady = delegatedProtocolsReady && customPolicies.pass;

  const failed = gates.find(({ pass }) => !pass);
  if (failed) {
    return {
      schema: "loyal-voltr-rwa-multiply-new-vault-verifier/v1",
      verdict: "FAIL",
      broadcast: false,
      failedCheck: failed.name,
      routeSpecSha256: rwaMultiplyRouteSpecSha256(route),
      contextSlot: live.context.slot,
      gates,
    };
  }
  const externalBlockers: string[] = [];
  let bridgeDelegateReady = false;
  if (!squadsAssetInfo || !squadsCollateralInfo) {
    externalBlockers.push("the Squads USDC and syrupUSDC ATAs must be created before the Jupiter and adaptor runtime can simulate without setup instructions");
  } else {
    const asset = AccountLayout.decode(squadsAssetInfo.data);
    const collateral = AccountLayout.decode(squadsCollateralInfo.data);
    if (
      !squadsAssetInfo.owner.equals(TOKEN_PROGRAM_ID)
      || !squadsCollateralInfo.owner.equals(TOKEN_PROGRAM_ID)
      || !new PublicKey(asset.owner).equals(new PublicKey(route.squads.vault))
      || !new PublicKey(collateral.owner).equals(new PublicKey(route.squads.vault))
    ) {
      return {
        schema: "loyal-voltr-rwa-multiply-new-vault-verifier/v1",
        verdict: "FAIL",
        broadcast: false,
        failedCheck: "Squads token custody identity",
      };
    }
    bridgeDelegateReady = asset.delegateOption === 1
      && new PublicKey(asset.delegate).equals(new PublicKey(accounts.strategyAuth))
      && asset.delegatedAmount.toString() === "18446744073709551615";
  }
  if (!obligationInfo) {
    externalBlockers.push("the fixed syrupUSDC/USDC Kamino obligation must be initialized for the Squads vault");
  } else {
    const obligationExact = decodedObligation !== null
      && obligationInfo.owner.equals(new PublicKey(route.kamino.program))
      && decodedObligation.tag.toNumber() === 1
      && String(decodedObligation.owner) === route.squads.vault
      && String(decodedObligation.lendingMarket) === route.kamino.market
      && decodedObligation.deposits.every((entry) => entry.depositedAmount.isZero())
      && decodedObligation.borrows.every((entry) => entry.borrowedAmountSf.isZero());
    if (!obligationExact) {
      return {
        schema: "loyal-voltr-rwa-multiply-new-vault-verifier/v1",
        verdict: "FAIL",
        broadcast: false,
        failedCheck: "fixed empty Multiply obligation identity",
      };
    }
  }
  if (forward.setupInstructionCount !== 0 || reverse.setupInstructionCount !== 0) {
    externalBlockers.push("Jupiter currently requires setup instructions; precreate both canonical Squads ATAs and refetch the routes");
  }
  if (forward.cleanupInstructionPresent || reverse.cleanupInstructionPresent || forward.otherInstructionCount !== 0 || reverse.otherInstructionCount !== 0) {
    externalBlockers.push("Jupiter returned cleanup/other instructions; freeze a clean no-setup/no-cleanup route before policy installation");
  }
  if (!adaptorReceiptInfo && admissionSimulation?.simulation.err !== null) {
    externalBlockers.push("the Voltr team must make the exact deployed custom adaptor program pass add_adaptor admission while allowAnyAdaptor remains 0");
  }
  if (!strategyInfo) externalBlockers.push("the immutable custom-adaptor strategy config must be initialized at its fixed address");
  if (!strategyReceiptInfo) externalBlockers.push("the Voltr strategy receipt must be initialized after custom-adaptor admission");
  if (!bridgeDelegateReady) externalBlockers.push("the one-time strategy-authority USDC delegate approval must be installed and read back exactly");
  if (!delegatedPoliciesReady) externalBlockers.push("delegated-key hookless KLend, Jupiter, and four exact custom Voltr bridge policies must be installed and read back at their final addresses");
  return {
    schema: "loyal-voltr-rwa-multiply-new-vault-verifier/v1",
    verdict: externalBlockers.length === 0 ? "PASS" : "BLOCKED",
    broadcast: false,
    routeSpecSha256: rwaMultiplyRouteSpecSha256(route),
    contextSlot: live.context.slot,
    identities: {
      newVault: route.vault.address,
      strategyConfig: route.customAdaptor.strategyConfig,
      previousBackyardVault: route.previousBackyardVault,
      squadsSettings: route.squads.settings,
      squadsVault: route.squads.vault,
      delegatedExecutor: route.squads.delegatedExecutor,
      strategyAuth: accounts.strategyAuth,
    },
    signedUnsentInitialization: initializeSimulation === null ? null : {
      packetBytes: initializeSimulation.packetBytes,
      unitsConsumed: initializeSimulation.simulation.unitsConsumed,
      expectedSignature: initializeSimulation.expectedSignature,
      wireStored: false,
    },
    signedUnsentAdmission: admissionSimulation === null ? null : {
      packetBytes: admissionSimulation.packetBytes,
      unitsConsumed: admissionSimulation.simulation.unitsConsumed,
      err: admissionSimulation.simulation.err,
      logs: admissionSimulation.simulation.logs,
      expectedSignature: admissionSimulation.expectedSignature,
      wireStored: false,
    },
    jupiter: {
      forward: { ...forward, instruction: undefined },
      reverse: { ...reverse, instruction: undefined },
    },
    setupInstructionHashes: Object.fromEntries(
      Object.entries(setup.instructions).map(([name, instruction]) => [
        name,
        sha256(toWeb3Instruction(instruction).data),
      ]),
    ),
    managerInstructionHashes: {
      deposit: sha256(toWeb3Instruction(managerInstructions.deposit).data),
      withdraw: sha256(toWeb3Instruction(managerInstructions.withdraw).data),
      bridgeApproval: sha256(toWeb3Instruction(bridgeApproval).data),
    },
    squadsActivation: {
      bridgeDelegateReady,
      delegatedPoliciesReady,
      delegatedProtocolsReady,
      delegatedPolicyScan,
      customPolicies: {
        pass: customPolicies.pass,
        sourceSha256: customPolicies.sourceSha256,
        rows: customPolicies.rows,
      },
    },
    protocolAdminHandoff,
    gates,
    activationGates,
    blockers: externalBlockers,
    resumeCondition: externalBlockers.length === 0
      ? null
      : "Have the Voltr team make FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW pass the deployed add_adaptor gate without changing this vault's allowAnyAdaptor=0. Rerun the sole verifier, execute Loyal add_adaptor once, initialize the strategy receipt, hand manager control to Squads, and run the dust lifecycle.",
  };
}

try {
  const report = await run();
  process.stdout.write(`${JSON.stringify(report, (_key, value) =>
    typeof value === "bigint" ? value.toString() : value, 2)}\n`);
  process.exitCode = report.verdict === "FAIL" ? 1 : 0;
} catch (error) {
  process.stdout.write(`${JSON.stringify({
    schema: "loyal-voltr-rwa-multiply-new-vault-verifier/v1",
    verdict: "BLOCKED",
    broadcast: false,
    blocker: error instanceof Error ? error.message : String(error),
    resumeCondition: "Restore the named RPC, signer, Jupiter, or current-chain prerequisite and rerun the same command.",
  }, (_key, value) => typeof value === "bigint" ? value.toString() : value, 2)}\n`);
  process.exitCode = 2;
}
