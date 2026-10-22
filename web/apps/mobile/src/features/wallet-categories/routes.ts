import type { Href } from "expo-router";

/** On-screen rectangle of a wallet card, in window coordinates. */
export type CardRect = { x: number; y: number; width: number; height: number };

export type CardSourceRect = CardRect & { usd: number };

function withRect(path: string, rect?: CardSourceRect): Href {
  if (!rect) return path as Href;
  const q = [
    `sx=${rect.x.toFixed(3)}`,
    `sy=${rect.y.toFixed(3)}`,
    `sw=${rect.width.toFixed(3)}`,
    `sh=${rect.height.toFixed(3)}`,
    `su=${rect.usd.toFixed(2)}`,
  ].join("&");
  return `${path}?${q}` as Href;
}

export function buildStablecoinsHref(rect?: CardSourceRect): Href {
  return withRect("/wallet/stablecoins", rect);
}

export function buildCryptoHref(rect?: CardSourceRect): Href {
  return withRect("/wallet/crypto", rect);
}

export function buildEarnHref(): Href {
  return "/(tabs)/earn" as Href;
}
