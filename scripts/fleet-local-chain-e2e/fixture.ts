#!/usr/bin/env bun

import { createHash } from "node:crypto";
import { mkdir, readFile, realpath, stat, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join, relative, resolve } from "node:path";

const BASE58_ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const BASE58_INDEX = new Map([...BASE58_ALPHABET].map((character, index) => [character, index]));

export const MAINNET_GENESIS_HASH = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const UPGRADEABLE_LOADER_ID = "BPFLoaderUpgradeab1e11111111111111111111111";
const LEGACY_BPF_LOADER_ID = "BPFLoader2111111111111111111111111111111111";
const PROGRAMDATA_METADATA_LENGTH = 45;
const REQUIRED_PROGRAM_ROOTS: CloneRootName[] = ["squadsProgram", "kaminoLendProgram", "kaminoFarmsProgram"];

export const REQUIRED_CLONE_ROOTS = {
  squadsProgram: "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG",
  kaminoLendProgram: "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",
  kaminoFarmsProgram: "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr",
  mainMarket: "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
  primeMarket: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
  mainUsdcReserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
  primeUsdcReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
  usdcMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
} as const;

type CloneRootName = keyof typeof REQUIRED_CLONE_ROOTS;

export type MainnetCloneAccount = {
  address: string;
  file: string;
  contextSlot: number;
  owner: string;
  executable: boolean;
  lamports: string;
  dataLength: number;
  dataSha256: string;
  fileSha256: string;
  purposes: string[];
};

export type MainnetCloneFixtureManifest = {
  schemaVersion: 1;
  kind: "loyal-fleet-mainnet-clone";
  source: {
    cluster: "mainnet-beta";
    genesisHash: string;
    commitment: "finalized";
    minimumContextSlot: number;
    capturedAtUtc: string;
  };
  roots: Record<CloneRootName, string>;
  accounts: MainnetCloneAccount[];
  localOnlyAccounts: {
    createdAfterValidatorStart: string[];
    fabricatedAtGenesis: string[];
  };
};

type SolanaCliAccountFile = {
  pubkey: string;
  account: {
    lamports: number | string;
    data: [string, "base64"];
    owner: string;
    executable: boolean;
    rentEpoch: number | string;
    space?: number;
  };
};

export type VerifiedFixture = {
  manifestPath: string;
  fixtureDirectory: string;
  manifest: MainnetCloneFixtureManifest;
  accountFiles: Array<{ address: string; path: string }>;
};

type ValidatorProgramDeployment = {
  program: string;
  kind: "legacy-bpf-relocated-by-validator" | "upgradeable-bpf";
  sourceAccount: string;
  programData?: string;
  codeSha256: string;
  codeLength: number;
};

type RpcEnvelope<T> = { result?: T; error?: { code: number; message: string } };

function fail(message: string): never {
  throw new Error(`mainnet clone fixture: ${message}`);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function requireExactKeys(value: Record<string, unknown>, expected: readonly string[], field: string): void {
  const expectedSet = new Set(expected);
  const extras = Object.keys(value).filter((key) => !expectedSet.has(key));
  const missing = expected.filter((key) => !(key in value));
  if (extras.length > 0 || missing.length > 0) {
    fail(`${field} keys differ (missing: ${missing.join(",") || "none"}; extra: ${extras.join(",") || "none"})`);
  }
}

function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

export function decodePublicKey(value: string): Uint8Array {
  if (value.length === 0) fail("public key must not be empty");
  const bytes = [0];
  for (const character of value) {
    const digit = BASE58_INDEX.get(character);
    if (digit === undefined) fail("public key contains a non-base58 character");
    let carry = digit;
    for (let index = 0; index < bytes.length; index += 1) {
      carry += bytes[index] * 58;
      bytes[index] = carry & 0xff;
      carry >>= 8;
    }
    while (carry > 0) {
      bytes.push(carry & 0xff);
      carry >>= 8;
    }
  }
  for (let index = 0; index < value.length - 1 && value[index] === "1"; index += 1) {
    bytes.push(0);
  }
  return Uint8Array.from(bytes.reverse());
}

export function encodePublicKey(bytes: Uint8Array): string {
  if (bytes.length !== 32) fail("public key bytes must be 32 bytes");
  const digits = [0];
  for (const byte of bytes) {
    let carry = byte;
    for (let index = 0; index < digits.length; index += 1) {
      carry += digits[index] << 8;
      digits[index] = carry % 58;
      carry = Math.floor(carry / 58);
    }
    while (carry > 0) {
      digits.push(carry % 58);
      carry = Math.floor(carry / 58);
    }
  }
  let output = "";
  for (let index = 0; index < bytes.length - 1 && bytes[index] === 0; index += 1) {
    output += "1";
  }
  for (let index = digits.length - 1; index >= 0; index -= 1) {
    output += BASE58_ALPHABET[digits[index]];
  }
  return output;
}

function parsePublicKey(value: unknown, field: string): string {
  if (typeof value !== "string") fail(`${field} must be a public key string`);
  try {
    const bytes = decodePublicKey(value);
    if (bytes.length !== 32) fail(`${field} is not 32 bytes`);
    const canonical = encodePublicKey(bytes);
    if (canonical !== value) fail(`${field} is not canonically encoded`);
    return canonical;
  } catch {
    return fail(`${field} is not a valid public key`);
  }
}

function parseUnsignedInteger(value: unknown, field: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    fail(`${field} must be a safe unsigned integer`);
  }
  return value;
}

function parseDecimal(value: unknown, field: string): string {
  const normalized = typeof value === "number" ? String(value) : value;
  if (typeof normalized !== "string" || !/^\d+$/u.test(normalized)) {
    fail(`${field} must be an unsigned decimal string`);
  }
  return normalized;
}

function parseSha256(value: unknown, field: string): string {
  if (typeof value !== "string" || !/^[a-f0-9]{64}$/u.test(value)) {
    fail(`${field} must be a lowercase SHA-256 digest`);
  }
  return value;
}

function parseUtc(value: unknown): string {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/u.test(value)) {
    fail("source.capturedAtUtc must be an ISO-8601 UTC timestamp");
  }
  if (!Number.isFinite(Date.parse(value))) fail("source.capturedAtUtc is invalid");
  return value;
}

function parseRelativeFile(value: unknown, field: string): string {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    [...value].some((character) => character.charCodeAt(0) < 0x20 || character.charCodeAt(0) === 0x7f)
  ) {
    fail(`${field} must be a nonempty relative path`);
  }
  if (isAbsolute(value) || value.split(/[\\/]/u).includes("..")) {
    fail(`${field} must stay inside the fixture directory`);
  }
  return value;
}

function parseStringList(value: unknown, field: string): string[] {
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === "string" && entry.length > 0)) {
    fail(`${field} must be an array of nonempty strings`);
  }
  return [...new Set(value)].sort();
}

function parseManifest(value: unknown): MainnetCloneFixtureManifest {
  if (!isRecord(value)) fail("manifest must be a JSON object");
  requireExactKeys(value, ["schemaVersion", "kind", "source", "roots", "accounts", "localOnlyAccounts"], "manifest");
  if (value.schemaVersion !== 1) fail("schemaVersion must equal 1");
  if (value.kind !== "loyal-fleet-mainnet-clone") fail("kind is invalid");

  if (!isRecord(value.source)) fail("source must be an object");
  requireExactKeys(
    value.source,
    ["cluster", "genesisHash", "commitment", "minimumContextSlot", "capturedAtUtc"],
    "source",
  );
  if (value.source.cluster !== "mainnet-beta") fail("source.cluster must be mainnet-beta");
  if (value.source.genesisHash !== MAINNET_GENESIS_HASH) fail("source.genesisHash is not canonical Mainnet");
  if (value.source.commitment !== "finalized") fail("source.commitment must be finalized");
  const minimumContextSlot = parseUnsignedInteger(value.source.minimumContextSlot, "source.minimumContextSlot");
  if (minimumContextSlot === 0) fail("source.minimumContextSlot must be positive");

  if (!isRecord(value.roots)) fail("roots must be an object");
  requireExactKeys(value.roots, Object.keys(REQUIRED_CLONE_ROOTS), "roots");
  const roots = {} as Record<CloneRootName, string>;
  for (const [name, expected] of Object.entries(REQUIRED_CLONE_ROOTS) as Array<[CloneRootName, string]>) {
    const address = parsePublicKey(value.roots[name], `roots.${name}`);
    if (address !== expected) fail(`roots.${name} must equal ${expected}`);
    roots[name] = address;
  }

  if (!Array.isArray(value.accounts) || value.accounts.length === 0) {
    fail("accounts must contain the finalized clone closure");
  }
  const seen = new Set<string>();
  const accounts = value.accounts.map((raw, index): MainnetCloneAccount => {
    if (!isRecord(raw)) fail(`accounts[${index}] must be an object`);
    requireExactKeys(
      raw,
      ["address", "file", "contextSlot", "owner", "executable", "lamports", "dataLength", "dataSha256", "fileSha256", "purposes"],
      `accounts[${index}]`,
    );
    const address = parsePublicKey(raw.address, `accounts[${index}].address`);
    if (seen.has(address)) fail(`accounts contains duplicate address ${address}`);
    seen.add(address);
    const contextSlot = parseUnsignedInteger(raw.contextSlot, `accounts[${index}].contextSlot`);
    if (contextSlot < minimumContextSlot) {
      fail(`account ${address} predates source.minimumContextSlot`);
    }
    const purposes = parseStringList(raw.purposes, `accounts[${index}].purposes`);
    if (purposes.length === 0) fail(`account ${address} must explain at least one purpose`);
    return {
      address,
      file: parseRelativeFile(raw.file, `accounts[${index}].file`),
      contextSlot,
      owner: parsePublicKey(raw.owner, `accounts[${index}].owner`),
      executable: raw.executable === true,
      lamports: parseDecimal(raw.lamports, `accounts[${index}].lamports`),
      dataLength: parseUnsignedInteger(raw.dataLength, `accounts[${index}].dataLength`),
      dataSha256: parseSha256(raw.dataSha256, `accounts[${index}].dataSha256`),
      fileSha256: parseSha256(raw.fileSha256, `accounts[${index}].fileSha256`),
      purposes,
    };
  });
  for (const [name, address] of Object.entries(roots)) {
    if (!seen.has(address)) fail(`clone closure is missing root ${name} (${address})`);
  }

  if (!isRecord(value.localOnlyAccounts)) fail("localOnlyAccounts must be an object");
  requireExactKeys(
    value.localOnlyAccounts,
    ["createdAfterValidatorStart", "fabricatedAtGenesis"],
    "localOnlyAccounts",
  );
  return {
    schemaVersion: 1,
    kind: "loyal-fleet-mainnet-clone",
    source: {
      cluster: "mainnet-beta",
      genesisHash: MAINNET_GENESIS_HASH,
      commitment: "finalized",
      minimumContextSlot,
      capturedAtUtc: parseUtc(value.source.capturedAtUtc),
    },
    roots,
    accounts,
    localOnlyAccounts: {
      createdAfterValidatorStart: parseStringList(
        value.localOnlyAccounts.createdAfterValidatorStart,
        "localOnlyAccounts.createdAfterValidatorStart",
      ),
      fabricatedAtGenesis: parseStringList(
        value.localOnlyAccounts.fabricatedAtGenesis,
        "localOnlyAccounts.fabricatedAtGenesis",
      ),
    },
  };
}

async function readAccountFile(path: string): Promise<{ raw: Uint8Array; value: SolanaCliAccountFile }> {
  const raw = new Uint8Array(await readFile(path));
  let value: unknown;
  try {
    value = JSON.parse(new TextDecoder().decode(raw));
  } catch {
    return fail(`account file is not valid JSON: ${path}`);
  }
  if (!isRecord(value) || typeof value.pubkey !== "string" || !isRecord(value.account)) {
    fail(`account file has no Solana CLI pubkey/account envelope: ${path}`);
  }
  const account = value.account;
  if (
    !Array.isArray(account.data) ||
    account.data.length !== 2 ||
    typeof account.data[0] !== "string" ||
    account.data[1] !== "base64" ||
    typeof account.owner !== "string" ||
    typeof account.executable !== "boolean"
  ) {
    fail(`account file has an unsupported account encoding: ${path}`);
  }
  return { raw, value: value as unknown as SolanaCliAccountFile };
}

export async function verifyFixture(manifestPathInput: string): Promise<VerifiedFixture> {
  const manifestPath = resolve(manifestPathInput);
  const fixtureDirectory = dirname(manifestPath);
  const manifest = parseManifest(JSON.parse(await readFile(manifestPath, "utf8")) as unknown);
  const fixtureReal = await realpath(fixtureDirectory);
  const accountFiles: Array<{ address: string; path: string }> = [];
  const accountData = new Map<string, Uint8Array>();

  for (const expected of manifest.accounts) {
    const path = join(fixtureDirectory, expected.file);
    const fileInfo = await stat(path);
    if (!fileInfo.isFile()) fail(`account path is not a file: ${expected.file}`);
    const fileReal = await realpath(path);
    const escape = relative(fixtureReal, fileReal);
    if (escape.startsWith("..") || isAbsolute(escape)) fail(`account file escapes fixture directory: ${expected.file}`);

    const { raw, value } = await readAccountFile(fileReal);
    const data = Buffer.from(value.account.data[0], "base64");
    const observed = {
      address: parsePublicKey(value.pubkey, `${expected.file}.pubkey`),
      owner: parsePublicKey(value.account.owner, `${expected.file}.account.owner`),
      executable: value.account.executable,
      lamports: parseDecimal(value.account.lamports, `${expected.file}.account.lamports`),
      dataLength: data.length,
      dataSha256: sha256(data),
      fileSha256: sha256(raw),
    };
    for (const field of ["address", "owner", "executable", "lamports", "dataLength", "dataSha256", "fileSha256"] as const) {
      if (observed[field] !== expected[field]) {
        fail(`${expected.file} ${field} does not match its finalized manifest record`);
      }
    }
    accountFiles.push({ address: expected.address, path: fileReal });
    accountData.set(expected.address, data);
  }

  const accountsByAddress = new Map(manifest.accounts.map((account) => [account.address, account]));
  for (const name of REQUIRED_PROGRAM_ROOTS) {
    const programAddress = manifest.roots[name];
    const program = accountsByAddress.get(programAddress);
    const data = accountData.get(programAddress);
    if (!program || !data) fail(`program root ${name} is absent after file verification`);
    if (!program.executable || program.owner !== UPGRADEABLE_LOADER_ID) {
      fail(`program root ${name} is not an executable upgradeable-loader account`);
    }
    if (data.length !== 36 || data[0] !== 2 || data[1] !== 0 || data[2] !== 0 || data[3] !== 0) {
      fail(`program root ${name} does not encode an upgradeable Program state`);
    }
    const programDataAddress = encodePublicKey(data.subarray(4));
    const programData = accountsByAddress.get(programDataAddress);
    if (!programData || !accountData.has(programDataAddress)) {
      fail(`program root ${name} is missing ProgramData clone ${programDataAddress}`);
    }
    if (programData.owner !== UPGRADEABLE_LOADER_ID || programData.executable) {
      fail(`ProgramData clone ${programDataAddress} has the wrong owner/executable state`);
    }
    const programDataBytes = accountData.get(programDataAddress)!;
    if (
      programDataBytes.length < 45 ||
      programDataBytes[0] !== 3 ||
      programDataBytes[1] !== 0 ||
      programDataBytes[2] !== 0 ||
      programDataBytes[3] !== 0
    ) {
      fail(`ProgramData clone ${programDataAddress} does not encode upgradeable ProgramData state`);
    }
  }

  return { manifestPath, fixtureDirectory, manifest, accountFiles };
}

async function fixtureAccountData(verified: VerifiedFixture): Promise<Map<string, Uint8Array>> {
  const data = new Map<string, Uint8Array>();
  for (const { address, path } of verified.accountFiles) {
    const account = await readAccountFile(path);
    data.set(address, Buffer.from(account.value.account.data[0], "base64"));
  }
  return data;
}

function programDataAuthority(data: Uint8Array, address: string): string {
  if (data.length < PROGRAMDATA_METADATA_LENGTH) fail(`ProgramData ${address} is shorter than its metadata`);
  if (data[12] === 0) return "none";
  if (data[12] !== 1) fail(`ProgramData ${address} has an invalid authority option`);
  const authority = encodePublicKey(data.subarray(13, PROGRAMDATA_METADATA_LENGTH));
  // solana-test-validator represents CLI `none` as Some(Pubkey::default()) at
  // genesis. Both are intentionally unupgradeable because the default key has
  // no signing key, so normalize that local encoding to the captured None.
  return authority === "11111111111111111111111111111111" ? "none" : authority;
}

export async function prepareValidatorPrograms(
  verified: VerifiedFixture,
  outputDirectoryInput: string,
): Promise<{ args: string[]; deployments: ValidatorProgramDeployment[] }> {
  const outputDirectory = resolve(outputDirectoryInput);
  await mkdir(outputDirectory, { recursive: true });
  const data = await fixtureAccountData(verified);
  const accounts = new Map(verified.manifest.accounts.map((account) => [account.address, account]));
  const programDataAddresses = new Set<string>();
  const args: string[] = [];
  const deployments: ValidatorProgramDeployment[] = [];

  for (const program of verified.manifest.accounts.filter((account) => account.executable)) {
    const programBytes = data.get(program.address)!;
    if (program.owner === UPGRADEABLE_LOADER_ID) {
      if (programBytes.length !== 36 || programBytes[0] !== 2) {
        fail(`upgradeable program ${program.address} has invalid Program state`);
      }
      const programDataAddress = encodePublicKey(programBytes.subarray(4));
      const programData = accounts.get(programDataAddress);
      const programDataBytes = data.get(programDataAddress);
      if (!programData || !programDataBytes || programData.owner !== UPGRADEABLE_LOADER_ID) {
        fail(`upgradeable program ${program.address} has no verified ProgramData`);
      }
      programDataAddresses.add(programDataAddress);
      const code = programDataBytes.subarray(PROGRAMDATA_METADATA_LENGTH);
      const path = join(outputDirectory, `${program.address}.so`);
      await writeFile(path, code);
      args.push("--upgradeable-program", program.address, path, programDataAuthority(programDataBytes, programDataAddress));
      deployments.push({
        program: program.address,
        kind: "upgradeable-bpf",
        sourceAccount: programDataAddress,
        programData: programDataAddress,
        codeSha256: sha256(code),
        codeLength: code.length,
      });
    } else if (program.owner === LEGACY_BPF_LOADER_ID) {
      const path = join(outputDirectory, `${program.address}.so`);
      await writeFile(path, programBytes);
      args.push("--bpf-program", program.address, path);
      deployments.push({
        program: program.address,
        kind: "legacy-bpf-relocated-by-validator",
        sourceAccount: program.address,
        codeSha256: sha256(programBytes),
        codeLength: programBytes.length,
      });
    } else {
      fail(`executable fixture account ${program.address} uses unsupported loader ${program.owner}`);
    }
  }

  for (const { address, path } of verified.accountFiles) {
    const account = accounts.get(address)!;
    if (!account.executable && !programDataAddresses.has(address)) args.push("--account", address, path);
  }
  return { args, deployments };
}

async function rpcCall<T>(rpcUrl: string, method: string, params: unknown[] = []): Promise<T> {
  const response = await fetch(rpcUrl, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  if (!response.ok) fail(`local RPC ${method} returned HTTP ${response.status}`);
  const envelope = await response.json() as RpcEnvelope<T>;
  if (envelope.error || envelope.result === undefined) {
    fail(`local RPC ${method} failed: ${envelope.error?.message ?? "missing result"}`);
  }
  return envelope.result;
}

export async function verifyLiveFixture(verified: VerifiedFixture, rpcUrlInput: string): Promise<void> {
  const rpcUrl = new URL(rpcUrlInput);
  if (rpcUrl.protocol !== "http:" || rpcUrl.hostname !== "127.0.0.1" || rpcUrl.username || rpcUrl.password) {
    fail("live verification permits only an unauthenticated http://127.0.0.1 RPC URL");
  }
  const liveGenesisHash = await rpcCall<string>(rpcUrl.href, "getGenesisHash");
  if (liveGenesisHash === MAINNET_GENESIS_HASH) {
    fail("live RPC returned the Mainnet genesis hash instead of a disposable local genesis");
  }

  const expectedByAddress = new Map(verified.manifest.accounts.map((account) => [account.address, account]));
  const expectedData = await fixtureAccountData(verified);
  const semanticProgramData = new Set(
    verified.manifest.accounts
      .filter((account) => account.purposes.some((purpose) => purpose.startsWith("program-data:")))
      .map((account) => account.address),
  );
  for (let offset = 0; offset < verified.manifest.accounts.length; offset += 100) {
    const addresses = verified.manifest.accounts.slice(offset, offset + 100).map((account) => account.address);
    const result = await rpcCall<{
      context: { slot: number };
      value: Array<null | { lamports: number; owner: string; executable: boolean; data: [string, "base64"] }>;
    }>(rpcUrl.href, "getMultipleAccounts", [addresses, { commitment: "confirmed", encoding: "base64" }]);
    if (!Array.isArray(result.value) || result.value.length !== addresses.length) {
      fail("live RPC returned an invalid getMultipleAccounts result");
    }
    for (let index = 0; index < addresses.length; index += 1) {
      const address = addresses[index];
      const live = result.value[index];
      const expected = expectedByAddress.get(address)!;
      if (!live) fail(`local validator is missing cloned account ${address}`);
      const data = Buffer.from(live.data[0], "base64");
      if (semanticProgramData.has(address)) {
        const captured = expectedData.get(address)!;
        if (
          live.owner !== expected.owner || live.executable ||
          data.length !== captured.length ||
          sha256(data.subarray(PROGRAMDATA_METADATA_LENGTH)) !== sha256(captured.subarray(PROGRAMDATA_METADATA_LENGTH)) ||
          sha256(data.subarray(0, 4)) !== sha256(captured.subarray(0, 4)) ||
          programDataAuthority(data, address) !== programDataAuthority(captured, address)
        ) {
          fail(
            `local validator ProgramData ${address} differs outside its local deployment slot/lamports ` +
            `(length ${data.length}/${captured.length}, code ${sha256(data.subarray(PROGRAMDATA_METADATA_LENGTH))}/` +
            `${sha256(captured.subarray(PROGRAMDATA_METADATA_LENGTH))}, authority ` +
            `${programDataAuthority(data, address)}/${programDataAuthority(captured, address)})`,
          );
        }
      } else if (expected.executable) {
        if (expected.owner === LEGACY_BPF_LOADER_ID && live.owner === UPGRADEABLE_LOADER_ID) {
          if (!live.executable || data.length !== 36 || data[0] !== 2) {
            fail(`local validator legacy program relocation ${address} has invalid Program state`);
          }
          const localProgramDataAddress = encodePublicKey(data.subarray(4));
          const localProgramData = await rpcCall<{
            value: null | { owner: string; executable: boolean; data: [string, "base64"] };
          }>(rpcUrl.href, "getAccountInfo", [localProgramDataAddress, { commitment: "confirmed", encoding: "base64" }]);
          if (!localProgramData.value) fail(`local validator legacy relocation lacks ProgramData ${localProgramDataAddress}`);
          const localProgramDataBytes = Buffer.from(localProgramData.value.data[0], "base64");
          if (
            localProgramData.value.owner !== UPGRADEABLE_LOADER_ID || localProgramData.value.executable ||
            sha256(localProgramDataBytes.subarray(PROGRAMDATA_METADATA_LENGTH)) !== expected.dataSha256
          ) {
            fail(`local validator relocated legacy program ${address} does not retain the captured ELF`);
          }
        } else if (
          live.owner !== expected.owner || !live.executable ||
          data.length !== expected.dataLength || sha256(data) !== expected.dataSha256
        ) {
          fail(`local validator executable program ${address} differs in owner or bytecode`);
        }
      } else if (
        String(live.lamports) !== expected.lamports || live.owner !== expected.owner ||
        live.executable !== expected.executable || data.length !== expected.dataLength ||
        sha256(data) !== expected.dataSha256
      ) {
        fail(`local validator account ${address} differs from its finalized clone`);
      }
    }
  }
}

async function main(): Promise<void> {
  const [command, manifestPath, thirdArgument] = process.argv.slice(2);
  if (!manifestPath || !["verify", "prepare-validator", "verify-live"].includes(command)) {
    fail("usage: fixture.ts <verify|prepare-validator|verify-live> <manifest.json> [output-dir|local-rpc-url]");
  }
  const verified = await verifyFixture(manifestPath);
  if (command === "verify-live") {
    if (!thirdArgument) fail("verify-live requires a local RPC URL");
    await verifyLiveFixture(verified, thirdArgument);
    const semanticProgramDataClones = verified.manifest.accounts.filter((account) =>
      account.purposes.some((purpose) => purpose.startsWith("program-data:"))
    ).length;
    const semanticExecutablePrograms = verified.manifest.accounts.filter((account) => account.executable).length;
    console.log(JSON.stringify({
      status: "PASS",
      localRpc: true,
      exactCloneAccounts: verified.accountFiles.length - semanticProgramDataClones - semanticExecutablePrograms,
      semanticExecutablePrograms,
      semanticProgramDataClones,
      totalFixtureAccounts: verified.accountFiles.length,
      sourceSlot: verified.manifest.source.minimumContextSlot,
    }));
    return;
  }
  if (command === "verify") {
    console.log(JSON.stringify({
      status: "PASS",
      kind: verified.manifest.kind,
      sourceSlot: verified.manifest.source.minimumContextSlot,
      accountCount: verified.accountFiles.length,
      localOnlyAccounts: verified.manifest.localOnlyAccounts,
    }));
    return;
  }
  if (!thirdArgument) fail("prepare-validator requires an output directory");
  console.log(JSON.stringify({
    kind: "loyal-fleet-local-validator-program-deployments",
    ...(await prepareValidatorPrograms(verified, thirdArgument)),
  }));
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  });
}
