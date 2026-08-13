#!/usr/bin/env bun

import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import {
  decodePublicKey,
  encodePublicKey,
  MAINNET_GENESIS_HASH,
  REQUIRED_CLONE_ROOTS,
  type MainnetCloneFixtureManifest,
  verifyFixture,
} from "./fleet-local-chain-e2e/fixture";

const LOADER = "BPFLoaderUpgradeab1e11111111111111111111111";
const SYSTEM = "11111111111111111111111111111111";

function digest(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function deterministicAddress(label: string): string {
  return encodePublicKey(createHash("sha256").update(label).digest());
}

function programState(programData: string): Uint8Array {
  return Uint8Array.from([2, 0, 0, 0, ...decodePublicKey(programData)]);
}

function programDataState(label: string): Uint8Array {
  const bytes = new Uint8Array(45 + 32);
  bytes.set([3, 0, 0, 0], 0);
  bytes.set(createHash("sha256").update(`elf:${label}`).digest(), 45);
  return bytes;
}

async function buildFixture(directory: string): Promise<{ manifestPath: string; manifest: MainnetCloneFixtureManifest; accountPath: string; accountRaw: string }> {
  const accountsDirectory = join(directory, "accounts");
  await mkdir(accountsDirectory, { recursive: true });
  const programRoots = new Set([
    REQUIRED_CLONE_ROOTS.squadsProgram,
    REQUIRED_CLONE_ROOTS.kaminoLendProgram,
    REQUIRED_CLONE_ROOTS.kaminoFarmsProgram,
  ]);
  const programData = new Map<string, string>(
    [...programRoots].map((address) => [address, deterministicAddress(`program-data:${address}`)]),
  );
  const addresses = [...Object.values(REQUIRED_CLONE_ROOTS), ...programData.values()];
  const accounts: MainnetCloneFixtureManifest["accounts"] = [];
  let firstAccountPath = "";
  let firstAccountRaw = "";

  for (const address of addresses) {
    const isProgram = programRoots.has(address);
    const isProgramData = [...programData.values()].includes(address);
    const data = isProgram
      ? programState(programData.get(address)!)
      : isProgramData
        ? programDataState(address)
        : Uint8Array.from(Buffer.from(`clone:${address}`));
    const file = `accounts/${address}.json`;
    const value = {
      pubkey: address,
      account: {
        lamports: 1_000_000,
        data: [Buffer.from(data).toString("base64"), "base64"],
        owner: isProgram || isProgramData ? LOADER : SYSTEM,
        executable: isProgram,
        rentEpoch: 0,
        space: data.length,
      },
    };
    const raw = `${JSON.stringify(value)}\n`;
    const path = join(directory, file);
    await writeFile(path, raw);
    if (!firstAccountPath) {
      firstAccountPath = path;
      firstAccountRaw = raw;
    }
    accounts.push({
      address,
      file,
      contextSlot: 350_000_001,
      owner: value.account.owner,
      executable: value.account.executable,
      lamports: String(value.account.lamports),
      dataLength: data.length,
      dataSha256: digest(data),
      fileSha256: digest(Buffer.from(raw)),
      purposes: [isProgram ? "program" : isProgramData ? "program-data" : "required-root"],
    });
  }

  const manifest: MainnetCloneFixtureManifest = {
    schemaVersion: 1,
    kind: "loyal-fleet-mainnet-clone",
    source: {
      cluster: "mainnet-beta",
      genesisHash: MAINNET_GENESIS_HASH,
      commitment: "finalized",
      minimumContextSlot: 350_000_000,
      capturedAtUtc: "2026-08-13T00:00:00Z",
    },
    roots: { ...REQUIRED_CLONE_ROOTS },
    accounts,
    localOnlyAccounts: {
      createdAfterValidatorStart: ["settings", "policies", "vault", "vault user metadata", "obligations", "lookup tables"],
      fabricatedAtGenesis: ["ephemeral wallet USDC token account"],
    },
  };
  const manifestPath = join(directory, "manifest.json");
  await writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
  return { manifestPath, manifest, accountPath: firstAccountPath, accountRaw: firstAccountRaw };
}

async function expectFailure(name: string, run: () => Promise<unknown>): Promise<void> {
  try {
    await run();
  } catch {
    console.log(`PASS negative control: ${name}`);
    return;
  }
  throw new Error(`negative control unexpectedly passed: ${name}`);
}

const directory = await mkdtemp(join(tmpdir(), "fleet-mainnet-clone-fixture."));
const fixture = await buildFixture(directory);
await verifyFixture(fixture.manifestPath);
console.log("PASS positive control: finalized closure and hashes");

const writeManifest = async (value: unknown): Promise<void> => {
  await writeFile(fixture.manifestPath, `${JSON.stringify(value, null, 2)}\n`);
};

await expectFailure("non-Mainnet genesis", async () => {
  await writeManifest({ ...fixture.manifest, source: { ...fixture.manifest.source, genesisHash: SYSTEM } });
  await verifyFixture(fixture.manifestPath);
});

await expectFailure("source RPC endpoint recorded", async () => {
  await writeManifest({ ...fixture.manifest, source: { ...fixture.manifest.source, rpcUrl: "https://example.invalid" } });
  await verifyFixture(fixture.manifestPath);
});

await expectFailure("required clone root omitted", async () => {
  await writeManifest({ ...fixture.manifest, accounts: fixture.manifest.accounts.slice(1) });
  await verifyFixture(fixture.manifestPath);
});

await expectFailure("account file hash changed", async () => {
  await writeManifest(fixture.manifest);
  await writeFile(fixture.accountPath, `${fixture.accountRaw} `);
  await verifyFixture(fixture.manifestPath);
});
await writeFile(fixture.accountPath, fixture.accountRaw);

await expectFailure("account path escapes fixture", async () => {
  const accounts = structuredClone(fixture.manifest.accounts);
  accounts[0].file = "../outside.json";
  await writeManifest({ ...fixture.manifest, accounts });
  await verifyFixture(fixture.manifestPath);
});

await expectFailure("ProgramData clone omitted", async () => {
  const programDataAddress = deterministicAddress(`program-data:${REQUIRED_CLONE_ROOTS.squadsProgram}`);
  await writeManifest({
    ...fixture.manifest,
    accounts: fixture.manifest.accounts.filter((account) => account.address !== programDataAddress),
  });
  await verifyFixture(fixture.manifestPath);
});

await expectFailure("ProgramData state malformed", async () => {
  const programDataAddress = deterministicAddress(`program-data:${REQUIRED_CLONE_ROOTS.squadsProgram}`);
  const accounts = structuredClone(fixture.manifest.accounts);
  const account = accounts.find((candidate) => candidate.address === programDataAddress)!;
  const path = join(directory, account.file);
  const originalRaw = await readFile(path, "utf8");
  const value = JSON.parse(originalRaw);
  const malformed = Uint8Array.from([0, 0, 0, 0]);
  value.account.data[0] = Buffer.from(malformed).toString("base64");
  value.account.space = malformed.length;
  const raw = `${JSON.stringify(value)}\n`;
  await writeFile(path, raw);
  account.dataLength = malformed.length;
  account.dataSha256 = digest(malformed);
  account.fileSha256 = digest(Buffer.from(raw));
  await writeManifest({ ...fixture.manifest, accounts });
  try {
    await verifyFixture(fixture.manifestPath);
  } finally {
    await writeFile(path, originalRaw);
  }
});

await writeManifest(fixture.manifest);
const verified = await verifyFixture(fixture.manifestPath);
const raw = await readFile(fixture.manifestPath, "utf8");
if (raw.includes("rpcUrl") || raw.includes("keypair")) {
  throw new Error("positive manifest unexpectedly contains endpoint or signer material");
}
console.log(`PASS: ${verified.accountFiles.length} account files form a validator-loadable clone contract`);
