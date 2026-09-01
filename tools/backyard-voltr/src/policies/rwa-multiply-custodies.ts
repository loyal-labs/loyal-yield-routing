import { createHash } from "node:crypto";

import { AccountLayout } from "@solana/spl-token";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  readRwaMultiplyCatalog,
  rwaMultiplyVaultAta,
} from "./rwa-multiply-catalog-resolver.js";

export type CatalogCustody = Readonly<{
  symbol: string;
  mint: string;
  tokenProgram: string;
  ata: string;
  lanes: readonly string[];
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

export function catalogCustodies(): readonly CatalogCustody[] {
  const catalog = readRwaMultiplyCatalog();
  const bySymbol = new Map<string, { mint: string; tokenProgram: string; lanes: Set<string> }>();
  for (const lane of catalog.lanes) {
    const laneName = `${lane.market}/${lane.collateral}/${lane.debt}`;
    for (const [symbol, mint, tokenProgram] of [
      [lane.collateral, lane.candidateIdentity.collateralMint, lane.candidateIdentity.collateralTokenProgram],
      [lane.debt, lane.candidateIdentity.debtMint, lane.candidateIdentity.debtTokenProgram],
    ] as const) {
      const prior = bySymbol.get(symbol);
      invariant(!prior || (prior.mint === mint && prior.tokenProgram === tokenProgram),
        `${symbol} mint or token program conflicts across lanes`);
      const current = prior ?? { mint, tokenProgram, lanes: new Set<string>() };
      current.lanes.add(laneName);
      bySymbol.set(symbol, current);
    }
  }
  invariant(bySymbol.size === 9, "catalog does not resolve to exactly nine custody assets");
  return [...bySymbol].map(([symbol, value]) => ({
    symbol,
    mint: value.mint,
    tokenProgram: value.tokenProgram,
    ata: rwaMultiplyVaultAta(value.mint, value.tokenProgram).toBase58(),
    lanes: [...value.lanes],
  }));
}

export async function inspectCatalogCustodies(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not mainnet-beta");
  const custodies = catalogCustodies();
  const response = await connection.getMultipleAccountsInfoAndContext(
    custodies.map(({ ata }) => new PublicKey(ata)),
    { commitment: "finalized" },
  );
  const accounts = custodies.map((custody, index) => {
    const account = response.value[index] ?? null;
    if (!account) return { ...custody, status: "absent" as const };
    let decoded: ReturnType<typeof AccountLayout.decode>;
    try {
      decoded = AccountLayout.decode(account.data);
    } catch {
      return { ...custody, status: "drift" as const, blocker: "account data is not a token account" };
    }
    const observed = {
      ownerProgram: account.owner.toBase58(),
      mint: new PublicKey(decoded.mint).toBase58(),
      authority: new PublicKey(decoded.owner).toBase58(),
      dataSha256: sha256(account.data),
    };
    const exact = observed.ownerProgram === custody.tokenProgram
      && observed.mint === custody.mint
      && observed.authority === RWA_MULTIPLY_ROUTE.squads.vault;
    return {
      ...custody,
      status: exact ? "exact" as const : "drift" as const,
      observed,
      ...(exact ? {} : { blocker: "ATA owner program, mint, or authority drifted" }),
    };
  });
  invariant(accounts.filter(({ status }) => status === "drift").length === 0,
    "one or more derived custody ATAs exist with a conflicting boundary");
  return {
    schema: "loyal-backyard-rwa-custody-readback/v1",
    broadcast: false,
    commitment: "finalized",
    contextSlot: response.context.slot,
    accounts,
    exactCount: accounts.filter(({ status }) => status === "exact").length,
    absentCount: accounts.filter(({ status }) => status === "absent").length,
  } as const;
}
