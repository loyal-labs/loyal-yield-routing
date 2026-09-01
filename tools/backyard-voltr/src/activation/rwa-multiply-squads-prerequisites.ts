import { createHash } from "node:crypto";
import {
  chmodSync,
  existsSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { dirname, resolve } from "node:path";

import {
  initObligation,
  Obligation,
  userMetadataPda,
} from "@kamino-finance/klend-sdk";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { executeTransactionSyncV2 } from "@loyal-labs/loyal-smart-accounts-core/internal";
import {
  AccountLayout,
  ASSOCIATED_TOKEN_PROGRAM_ID,
  createAssociatedTokenAccountIdempotentInstruction,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} from "@solana/spl-token";
import {
  AccountRole,
  address,
  createNoopSigner,
  type Instruction,
} from "@solana/kit";
import { getRevokeInstruction } from "@solana-program/token";
import {
  Connection,
  ComputeBudgetProgram,
  Keypair,
  PublicKey,
  SystemProgram,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type AccountInfo,
} from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

type Json = Record<string, unknown>;
type SettingsState = Readonly<{
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{
    key: PublicKey;
    permissions: Readonly<{ mask: number }>;
  }>[];
}>;

const PACKET_LIMIT = 1_232;
const OBLIGATION_BYTES = 3_344;
const RENT_SYSVAR = address("SysvarRent111111111111111111111111111111111");
const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: AccountInfo<Buffer>): readonly [SettingsState, number] };
}).Settings;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function loadAdmin(): Keypair {
  const encoded = process.env.SOLANA_TESTING_PK?.trim();
  invariant(encoded, "SOLANA_TESTING_PK is required");
  let bytes: Uint8Array;
  try {
    bytes = encoded.startsWith("[")
      ? Uint8Array.from(JSON.parse(encoded) as number[])
      : bs58.decode(encoded);
  } catch {
    throw new Error("SOLANA_TESTING_PK is not parseable");
  }
  invariant(bytes.length === 64, "SOLANA_TESTING_PK is not a 64-byte keypair");
  const signer = Keypair.fromSecretKey(bytes);
  invariant(signer.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin, "SOLANA_TESTING_PK is not the fixed setup admin");
  return signer;
}

function kitToWeb3(instruction: Instruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(instruction.programAddress),
    data: Buffer.from(instruction.data ?? []),
    keys: (instruction.accounts ?? []).map((account) => ({
      pubkey: new PublicKey(account.address),
      isSigner: account.role === AccountRole.READONLY_SIGNER
        || account.role === AccountRole.WRITABLE_SIGNER,
      isWritable: account.role === AccountRole.WRITABLE
        || account.role === AccountRole.WRITABLE_SIGNER,
    })),
  });
}

function compileSquadsInner(instructions: readonly TransactionInstruction[]) {
  const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean): number => {
    const found = accounts.findIndex((account) => account.pubkey.equals(pubkey));
    if (found >= 0) {
      accounts[found]!.isWritable ||= writable;
      return found;
    }
    invariant(accounts.length < 255, "Squads inner account table exceeds u8");
    accounts.push({ pubkey, isWritable: writable, isSigner: false });
    return accounts.length - 1;
  };
  const chunks: Buffer[] = [Buffer.from([instructions.length])];
  for (const instruction of instructions) {
    for (const key of instruction.keys) {
      invariant(!key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault, "inner instruction has a signer other than the fixed Squads vault");
    }
    const accountIndexes = instruction.keys.map((key) => indexOf(key.pubkey, key.isWritable));
    const programIndex = indexOf(instruction.programId, false);
    invariant(instruction.data.length <= 65_535, "inner instruction data exceeds u16");
    const dataLength = Buffer.alloc(2);
    dataLength.writeUInt16LE(instruction.data.length);
    chunks.push(
      Buffer.from([programIndex, accountIndexes.length, ...accountIndexes]),
      dataLength,
      instruction.data,
    );
  }
  return { instructions: Uint8Array.from(Buffer.concat(chunks)), accounts };
}

function tokenState(info: AccountInfo<Buffer> | null, expectedMint: string) {
  invariant(info !== null, "fixed Squads token account is absent");
  invariant(info.owner.equals(TOKEN_PROGRAM_ID) && info.data.length === AccountLayout.span, "fixed Squads token account is malformed");
  const decoded = AccountLayout.decode(info.data);
  invariant(new PublicKey(decoded.owner).toBase58() === RWA_MULTIPLY_ROUTE.squads.vault, "fixed Squads token account owner drifted");
  invariant(new PublicKey(decoded.mint).toBase58() === expectedMint, "fixed Squads token account mint drifted");
  return {
    amountRaw: decoded.amount.toString(),
    delegate: decoded.delegateOption === 0 ? null : new PublicKey(decoded.delegate).toBase58(),
    delegatedAmountRaw: decoded.delegatedAmount.toString(),
    dataSha256: sha256(info.data),
  };
}

function obligationState(info: AccountInfo<Buffer> | null) {
  if (!info) return null;
  invariant(info.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program && info.data.length === OBLIGATION_BYTES, "fixed obligation owner or size drifted");
  const obligation = Obligation.decode(info.data);
  invariant(obligation.tag.toNumber() === 1, "fixed obligation is not a Multiply obligation");
  invariant(String(obligation.owner) === RWA_MULTIPLY_ROUTE.squads.vault, "fixed obligation owner drifted");
  invariant(String(obligation.lendingMarket) === RWA_MULTIPLY_ROUTE.kamino.market, "fixed obligation market drifted");
  invariant(obligation.deposits.every((entry) => entry.depositedAmount.isZero()), "fixed obligation already has collateral");
  invariant(obligation.borrows.every((entry) => entry.borrowedAmountSf.isZero()), "fixed obligation already has debt");
  return {
    tag: obligation.tag.toNumber(),
    owner: String(obligation.owner),
    market: String(obligation.lendingMarket),
    dataSha256: sha256(info.data),
  };
}

async function readState(connection: Connection, metadata: PublicKey, minContextSlot?: number) {
  const route = RWA_MULTIPLY_ROUTE;
  const keys = [
    new PublicKey(route.squads.settings),
    new PublicKey(route.squads.vault),
    new PublicKey(route.squads.assetAta),
    new PublicKey(route.squads.collateralAta),
    new PublicKey(route.kamino.obligation),
    metadata,
  ];
  const response = await connection.getMultipleAccountsInfoAndContext(keys, {
    commitment: "finalized",
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  });
  const [settingsInfo, vaultInfo, assetInfo, collateralInfo, obligationInfo, metadataInfo] = response.value;
  invariant(settingsInfo?.owner.toBase58() === route.squads.program, "Squads Settings is absent or owned by the wrong program");
  invariant(vaultInfo !== null && vaultInfo !== undefined, "Squads vault PDA is absent");
  invariant(vaultInfo.owner.equals(SystemProgram.programId) && vaultInfo.data.length === 0, "Squads vault PDA is not a system wallet");
  invariant(metadataInfo?.owner.toBase58() === route.kamino.program, "Kamino user metadata is absent or owned by the wrong program");
  const [settings] = Settings.fromAccountInfo(settingsInfo);
  invariant(settings.threshold === 1 && settings.timeLock === 0, "Squads Settings threshold or time lock drifted");
  invariant(settings.signers.length === 1 && settings.signers[0]!.key.toBase58() === route.setupAdmin && settings.signers[0]!.permissions.mask === 7, "Squads Settings admin boundary drifted");
  const asset = tokenState(assetInfo ?? null, route.assets.assetMint);
  const collateral = collateralInfo
    ? tokenState(collateralInfo, route.assets.collateralMint)
    : null;
  return {
    slot: response.context.slot,
    vaultLamports: vaultInfo.lamports,
    asset,
    collateral,
    obligation: obligationState(obligationInfo ?? null),
    stateSha256: sha256(Buffer.concat(response.value.map((info) => info?.data ?? Buffer.alloc(0)))),
  };
}

function writePrivate(path: string, value: Json, flag: "w" | "wx") {
  const content = `${JSON.stringify(value, null, 2)}\n`;
  writeFileSync(path, content, { flag, mode: 0o600 });
  chmodSync(path, 0o600);
}

async function main() {
  const execute = process.argv.includes("--execute");
  const reconcile = process.argv.includes("--reconcile");
  const journalIndex = process.argv.indexOf("--journal");
  const journal = journalIndex >= 0 ? resolve(process.argv[journalIndex + 1] ?? "") : "";
  invariant(!(execute && reconcile), "--execute and --reconcile are mutually exclusive");
  invariant(!execute || process.env.CONFIRM_MAINNET === "1", "--execute requires CONFIRM_MAINNET=1");
  invariant(!(execute || reconcile) || (journal.endsWith(".json") && existsSync(dirname(journal))), "--execute/--reconcile requires --journal PATH under an existing directory");
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)), "journal replay barrier already exists");
  invariant(!reconcile || (!existsSync(journal) && existsSync(`${journal}.pending`)), "--reconcile requires one pending journal and no finalized journal");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const admin = loadAdmin();
  const [metadataAddress] = await userMetadataPda(address(RWA_MULTIPLY_ROUTE.squads.vault), address(RWA_MULTIPLY_ROUTE.kamino.program));
  const metadata = new PublicKey(metadataAddress);
  const collateralAta = getAssociatedTokenAddressSync(
    new PublicKey(RWA_MULTIPLY_ROUTE.assets.collateralMint),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault),
    true,
    TOKEN_PROGRAM_ID,
    ASSOCIATED_TOKEN_PROGRAM_ID,
  );
  invariant(collateralAta.toBase58() === RWA_MULTIPLY_ROUTE.squads.collateralAta,
    "derived PRIME ATA does not match the fixed route");
  const [obligation] = PublicKey.findProgramAddressSync([
    Buffer.from([1]),
    Buffer.from([0]),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault).toBuffer(),
    new PublicKey(RWA_MULTIPLY_ROUTE.kamino.market).toBuffer(),
    new PublicKey(RWA_MULTIPLY_ROUTE.assets.collateralMint).toBuffer(),
    new PublicKey(RWA_MULTIPLY_ROUTE.assets.assetMint).toBuffer(),
  ], new PublicKey(RWA_MULTIPLY_ROUTE.kamino.program));
  invariant(obligation.toBase58() === RWA_MULTIPLY_ROUTE.kamino.obligation,
    "derived PRIME/USDC Multiply obligation does not match the fixed route");
  const before = await readState(connection, metadata);
  if (reconcile) {
    invariant(before.obligation !== null
      && before.asset.delegate === null
      && before.asset.delegatedAmountRaw === "0"
      && before.collateral?.delegate === null
      && before.collateral.delegatedAmountRaw === "0",
      "pending prerequisite wire is not reflected in finalized state");
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as {
      transaction?: { expectedSignature?: unknown };
    };
    const signature = String(pending.transaction?.expectedSignature ?? "");
    invariant(signature.length > 0, "pending journal lacks the expected signature");
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized", "pending signature is not finalized successfully");
    writePrivate(journal, {
      ...pending,
      verdict: "FINALIZED_RECONCILED",
      signature,
      finalizedSlot: landed.slot,
      finalizedContextSlot: status.context.slot,
      after: before,
    }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature, finalizedSlot: landed.slot, journal }, null, 2));
    return;
  }
  if (before.obligation
    && before.asset.delegate === null
    && before.asset.delegatedAmountRaw === "0"
    && before.collateral?.delegate === null
    && before.collateral.delegatedAmountRaw === "0") {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, before }, null, 2));
    return;
  }

  const vaultSigner = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const inner: TransactionInstruction[] = [];
  if (!before.obligation) {
    inner.push(kitToWeb3(initObligation({ args: { tag: 1, id: 0 } }, {
      obligationOwner: vaultSigner,
      feePayer: vaultSigner,
      obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      lendingMarket: RWA_MULTIPLY_ROUTE.kamino.market,
      seed1Account: RWA_MULTIPLY_ROUTE.assets.collateralMint,
      seed2Account: RWA_MULTIPLY_ROUTE.assets.assetMint,
      ownerUserMetadata: address(metadata.toBase58()),
      rent: RENT_SYSVAR,
      systemProgram: RWA_MULTIPLY_ROUTE.programs.system,
    }, [], RWA_MULTIPLY_ROUTE.kamino.program)));
  }
  if (before.asset.delegate !== null) {
    inner.push(kitToWeb3(getRevokeInstruction({
      source: RWA_MULTIPLY_ROUTE.squads.assetAta,
      owner: vaultSigner,
    }, { programAddress: RWA_MULTIPLY_ROUTE.assets.tokenProgram })));
  }
  if (before.collateral?.delegate) {
    inner.push(kitToWeb3(getRevokeInstruction({
      source: RWA_MULTIPLY_ROUTE.squads.collateralAta,
      owner: vaultSigner,
    }, { programAddress: RWA_MULTIPLY_ROUTE.assets.tokenProgram })));
  }
  let executeSquads: TransactionInstruction | null = null;
  if (inner.length > 0) {
    const compiled = compileSquadsInner(inner);
    executeSquads = executeTransactionSyncV2({
      feePayer: admin.publicKey,
      settingsPda: new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings),
      accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
      numSigners: 1,
      instructions: compiled.instructions,
      instruction_accounts: [
        { pubkey: admin.publicKey, isSigner: true, isWritable: false },
        ...compiled.accounts,
      ],
    });
  }
  const obligationRent = before.obligation
    ? 0
    : await connection.getMinimumBalanceForRentExemption(OBLIGATION_BYTES, "finalized");
  const fundingLamports = Math.max(0, obligationRent - before.vaultLamports);
  const outer: TransactionInstruction[] = [
    ComputeBudgetProgram.setComputeUnitLimit({ units: 100_000 }),
    ComputeBudgetProgram.setComputeUnitPrice({ microLamports: 50_000 }),
    ...(fundingLamports > 0 ? [SystemProgram.transfer({
      fromPubkey: admin.publicKey,
      toPubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault),
      lamports: fundingLamports,
    })] : []),
    ...(before.collateral === null ? [createAssociatedTokenAccountIdempotentInstruction(
      admin.publicKey,
      collateralAta,
      new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault),
      new PublicKey(RWA_MULTIPLY_ROUTE.assets.collateralMint),
      TOKEN_PROGRAM_ID,
      ASSOCIATED_TOKEN_PROGRAM_ID,
    )] : []),
    ...(executeSquads ? [executeSquads] : []),
  ];
  const latest = await connection.getLatestBlockhashAndContext({ commitment: "confirmed", minContextSlot: before.slot });
  const message = new TransactionMessage({
    payerKey: admin.publicKey,
    recentBlockhash: latest.value.blockhash,
    instructions: outer,
  }).compileToV0Message();
  const transaction = new VersionedTransaction(message);
  transaction.sign([admin]);
  const wire = transaction.serialize();
  invariant(wire.length <= PACKET_LIMIT, `prerequisite transaction is ${wire.length} bytes`);
  const expectedSignature = bs58.encode(transaction.signatures[0]!);
  const simulation = await connection.simulateTransaction(VersionedTransaction.deserialize(wire), {
    commitment: "confirmed",
    sigVerify: true,
    replaceRecentBlockhash: false,
    minContextSlot: before.slot,
    accounts: {
      encoding: "base64",
      addresses: [
        RWA_MULTIPLY_ROUTE.kamino.obligation,
        RWA_MULTIPLY_ROUTE.squads.assetAta,
        RWA_MULTIPLY_ROUTE.squads.collateralAta,
      ],
    },
  });
  invariant(simulation.value.err === null, `signed prerequisite simulation failed: ${JSON.stringify(simulation.value.err)}`);
  const plan = {
    schema: "loyal-rwa-multiply-squads-prerequisites/v3",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    identities: {
      settings: RWA_MULTIPLY_ROUTE.squads.settings,
      vault: RWA_MULTIPLY_ROUTE.squads.vault,
      market: RWA_MULTIPLY_ROUTE.kamino.market,
      obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      assetAta: RWA_MULTIPLY_ROUTE.squads.assetAta,
      collateralAta: RWA_MULTIPLY_ROUTE.squads.collateralAta,
      collateralReserve: RWA_MULTIPLY_ROUTE.kamino.collateralReserve,
      debtReserve: RWA_MULTIPLY_ROUTE.kamino.debtReserve,
      assetMint: RWA_MULTIPLY_ROUTE.assets.assetMint,
      collateralMint: RWA_MULTIPLY_ROUTE.assets.collateralMint,
    },
    before,
    transaction: {
      packetBytes: wire.length,
      innerInstructionCount: inner.length,
      outerInstructionCount: outer.length,
      fundingLamports,
      obligationRent,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      expectedSignature,
      wireSha256: sha256(wire),
    },
  };
  if (!execute) {
    console.log(JSON.stringify(plan, null, 2));
    return;
  }
  const pending = `${journal}.pending`;
  writePrivate(pending, { ...plan, signedWireBase64: Buffer.from(wire).toString("base64") }, "wx");
  const sent = await connection.sendRawTransaction(wire, { skipPreflight: true, maxRetries: 3 });
  invariant(sent === expectedSignature, "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction({
    signature: sent,
    blockhash: latest.value.blockhash,
    lastValidBlockHeight: latest.value.lastValidBlockHeight,
  }, "finalized");
  invariant(confirmation.value.err === null, `prerequisite transaction finalized with an error: ${JSON.stringify(confirmation.value.err)}`);
  const after = await readState(connection, metadata, confirmation.context.slot);
  invariant(after.obligation !== null, "fixed obligation is absent after finalization");
  invariant(after.collateral !== null, "fixed PRIME ATA is absent after finalization");
  invariant(after.collateral.delegate === null
    && after.collateral.delegatedAmountRaw === "0",
  "PRIME ATA delegate was not removed exactly");
  if (before.collateral === null) {
    invariant(after.collateral.amountRaw === "0", "new PRIME ATA is not empty");
  } else {
    invariant(after.collateral.amountRaw === before.collateral.amountRaw,
      "existing PRIME ATA balance changed during prerequisite activation");
    if (before.collateral.delegate === null) {
      invariant(after.collateral.dataSha256 === before.collateral.dataSha256,
        "authority-clean PRIME ATA changed during prerequisite activation");
    }
  }
  invariant(after.asset.delegate === null && after.asset.delegatedAmountRaw === "0",
    "Squads USDC delegate was not removed exactly");
  invariant(after.asset.amountRaw === before.asset.amountRaw,
    "Squads USDC balance changed during prerequisite activation");
  writePrivate(journal, {
    ...plan,
    verdict: "FINALIZED_RECONCILED",
    signature: sent,
    finalizedContextSlot: confirmation.context.slot,
    after,
  }, "wx");
  renameSync(pending, `${journal}.sent-wire`);
  console.log(JSON.stringify({
    verdict: "FINALIZED_RECONCILED",
    signature: sent,
    finalizedContextSlot: confirmation.context.slot,
    journal,
    obligation: after.obligation,
    delegates: {
      asset: { address: after.asset.delegate, allowanceRaw: after.asset.delegatedAmountRaw },
      collateral: { address: after.collateral.delegate, allowanceRaw: after.collateral.delegatedAmountRaw },
    },
    collateralAta: RWA_MULTIPLY_ROUTE.squads.collateralAta,
  }, null, 2));
}

try {
  await main();
} catch (error) {
  console.error(JSON.stringify({
    verdict: "BLOCKED",
    blocker: error instanceof Error ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>") : String(error),
  }));
  process.exitCode = 1;
}
