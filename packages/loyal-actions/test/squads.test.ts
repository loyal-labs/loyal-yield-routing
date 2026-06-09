import { describe, expect, test } from "bun:test";
import { PublicKey, SystemProgram } from "@solana/web3.js";
import { LOYAL_CLUSTER_CONFIGS, LoyalCluster } from "../src/index.js";
import {
  createSquadsSmartAccountInstruction,
  decodeSquadsProgramConfig,
  deriveSquadsProgramConfig,
  deriveSquadsSettings,
  deriveSquadsVault,
} from "../src/internal/squads.js";

const config = LOYAL_CLUSTER_CONFIGS[LoyalCluster.MainnetBeta];

describe("Squads smart-account helpers", () => {
  test("builds the Squads create smart account instruction", () => {
    const payer = new PublicKey("11111111111111111111111111111113");
    const verifier = new PublicKey("11111111111111111111111111111114");
    const treasury = new PublicKey("11111111111111111111111111111115");
    const seed = 1n;
    const settings = deriveSquadsSettings(config, seed);

    const instruction = createSquadsSmartAccountInstruction(config, {
      payer,
      verifier,
      seed,
      treasury,
    });

    expect(instruction.programId).toEqual(config.squadsSmartAccountProgramId);
    expect(instruction.keys.map((key) => [key.pubkey.toBase58(), key.isSigner, key.isWritable])).toEqual([
      [deriveSquadsProgramConfig(config).toBase58(), false, true],
      [treasury.toBase58(), false, true],
      [payer.toBase58(), true, true],
      [SystemProgram.programId.toBase58(), false, false],
      [config.squadsSmartAccountProgramId.toBase58(), false, false],
      [settings.toBase58(), false, true],
    ]);
    expect([...instruction.data.subarray(0, 8)]).toEqual([197, 102, 253, 231, 77, 84, 50, 17]);
    expect(instruction.data.length).toBe(54);
    expect(instruction.data[8]).toBe(0);
    expect([...instruction.data.subarray(9, 11)]).toEqual([1, 0]);
    expect([...instruction.data.subarray(11, 15)]).toEqual([1, 0, 0, 0]);
    expect(new PublicKey(instruction.data.subarray(15, 47))).toEqual(verifier);
    expect(instruction.data[47]).toBe(7);
    expect([...instruction.data.subarray(48, 52)]).toEqual([0, 0, 0, 0]);
    expect([...instruction.data.subarray(52, 54)]).toEqual([0, 0]);
  });

  test("derives a vault for the settings account", () => {
    const settings = deriveSquadsSettings(config, 2n);
    const vault = deriveSquadsVault(config, settings, 0);

    expect(vault).toBeInstanceOf(PublicKey);
    expect(vault.equals(settings)).toBe(false);
  });

  test("decodes the Squads program config account layout", () => {
    const authority = new PublicKey("11111111111111111111111111111116");
    const treasury = new PublicKey("11111111111111111111111111111117");
    const data = new Uint8Array(160);
    data.set([196, 210, 90, 231, 144, 149, 140, 63], 0);
    writeLittleEndian(data, 8, 42n, 16);
    data.set(authority.toBytes(), 24);
    writeLittleEndian(data, 56, 5_000n, 8);
    data.set(treasury.toBytes(), 64);

    expect(decodeSquadsProgramConfig(data)).toEqual({
      smartAccountIndex: 42n,
      authority,
      smartAccountCreationFeeLamports: 5_000n,
      treasury,
    });
  });
});

function writeLittleEndian(data: Uint8Array, offset: number, value: bigint, byteLength: number): void {
  let remaining = value;
  for (let index = 0; index < byteLength; index += 1) {
    data[offset + index] = Number(remaining & 0xffn);
    remaining >>= 8n;
  }
}
