import type { BackyardStrategyId } from "./types";

export const BACKYARD_VAULT = {
  name: "Backyard Loyal USDC",
  address: "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK",
  lpMint: "dbQkLsUYE7ADHHv8XEottANAa773K4xM4nyPjVdutka",
  idleAta: "9LHpTxtFDYb8xJAruX9uTrceohFms2KyRvkXREj3iV9P",
  idleAuthority: "C8geyt5kKSDoXYPrSvDee6Rv9ooBzXLiQLmCSUjamcfo",
  manager: "DMPn3d7G2rcVVhvRbpSyEeq3cBW7bygiGjSgrLci5FYK",
  assetMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  assetDecimals: 6,
  capRaw: 1_000_000_000_000n,
  withdrawalWaitSeconds: 600,
  performanceFeeBps: 500,
  programs: {
    voltr: "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8",
    kaminoAdaptor: "to6Eti9CsC5FGkAtqiPphvKD2hiQiLsS8zWiDBqBPKR",
    token: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
  },
  strategies: [
    {
      id: "main",
      label: "Main",
      reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
      receipt: "8TrCAoobPV9cygRG59LforafAmVE5QLa9HBS76GEG2gh",
    },
    {
      id: "onre",
      label: "OnRe",
      reserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
      receipt: "2dhBPLc2s69FBck4Kdz5PzGanV4YsRL5M4nY3KdVUJpo",
    },
    {
      id: "prime",
      label: "Prime",
      reserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
      receipt: "Gyhy9cX1fyjxhYKbRbt3rpswRg8JZRPvMgGorMWdHJ9U",
    },
    {
      id: "maple",
      label: "Maple",
      reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
      receipt: "9MajR4HSgdRkWiFsAum83P5F6EYNGjgscLGk97eEN6dC",
    },
  ] as const satisfies readonly {
    id: BackyardStrategyId;
    label: string;
    reserve: string;
    receipt: string;
  }[],
} as const;
