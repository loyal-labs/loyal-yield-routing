export type TokenDetailChartPoint = {
  timestamp: number;
  priceUsd: number;
};

export type TokenDetailResponse = {
  mint: string;
  token: {
    decimals: number | null;
    logoUrl: string | null;
    name: string | null;
    symbol: string | null;
  };
  links: {
    website: string | null;
    twitter: string | null;
    explorer: string | null;
    discord: string | null;
    telegram: string | null;
  };
  market: {
    fdvUsd: number | null;
    holderCount: number | null;
    liquidityUsd: number | null;
    marketCapUsd: number | null;
    priceChange24hPercent: number | null;
    priceUsd: number | null;
    updatedAt: string | null;
    volume24hUsd: number | null;
  };
  info: {
    description: string | null;
    gtScore: number | null;
    gtVerified: boolean;
    mintAuthority: string | null;
    freezeAuthority: string | null;
    holderDistribution: {
      top10: string;
      rest: string;
    } | null;
  };
  chart: TokenDetailChartPoint[];
};
