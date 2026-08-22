import { createHmac } from "node:crypto";

import {
  createKeyPairSignerFromBytes,
  createKeyPairSignerFromPrivateKeyBytes,
  type KeyPairSigner,
} from "@solana/kit";
import bs58 from "bs58";

import type { PartnerRouteSpec } from "../domain/route-spec.js";

export type SigningMaterial = Readonly<{
  signer: KeyPairSigner;
  privateSeed: Uint8Array;
  secretKey: Uint8Array;
}>;

function decodeSecret(value: string, label: string): Uint8Array {
  try {
    const trimmed = value.trim();
    const bytes = trimmed.startsWith("[")
      ? Uint8Array.from(JSON.parse(trimmed) as number[])
      : /^[0-9a-fA-F]+$/.test(trimmed) && (trimmed.length === 64 || trimmed.length === 128)
        ? Uint8Array.from(Buffer.from(trimmed, "hex"))
        : bs58.decode(trimmed);
    if (bytes.length !== 32 && bytes.length !== 64) {
      throw new Error(`decoded to ${bytes.length} bytes`);
    }
    return bytes;
  } catch (error) {
    throw new Error(`${label} is not a valid 32-byte seed or 64-byte Solana keypair`, {
      cause: error,
    });
  }
}

export async function signingMaterialFromEnvironment(
  name: string,
): Promise<SigningMaterial> {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for this signing operation`);
  const decoded = decodeSecret(value, name);
  const privateSeed = decoded.subarray(0, 32);
  const signer = decoded.length === 32
    ? await createKeyPairSignerFromPrivateKeyBytes(decoded)
    : await createKeyPairSignerFromBytes(decoded);
  return { signer, privateSeed, secretKey: decoded };
}

export async function derivePartnerVaultSigningMaterial(
  admin: SigningMaterial,
  route: PartnerRouteSpec,
): Promise<SigningMaterial> {
  const privateSeed = createHmac("sha256", admin.privateSeed)
    .update(`loyal-backyard-voltr-partner-mainnet-v1:${route.squads.manager}:${route.strategy.reserve}`)
    .digest();
  const signer = await createKeyPairSignerFromPrivateKeyBytes(privateSeed);
  if (signer.address !== route.vault) {
    throw new Error(`derived partner vault ${signer.address} does not match RouteSpec ${route.vault}`);
  }
  return { signer, privateSeed, secretKey: privateSeed };
}
