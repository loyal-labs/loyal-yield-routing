// Older API releases omit these filenames; previews and staggered releases use the bundled artwork.
const BRIDGE_ICONS: Record<string, string> = {
  dai: "dai.png",
  usde: "usde.png",
  fdusd: "fdusd.png",
  rlusd: "rlusd.png",
  usd1: "usd1.jpg",
  usdt0: "usdt0.jpg",
  optimism: "optimism.png",
  monad: "monad.png",
  tempo: "tempo.png",
  robinhood: "robinhood.webp",
  megaeth: "megaeth.png",
  plasma: "plasma.png",
};

export function bridgeIconFile(key: string, file: string | null) {
  return file || (Object.hasOwn(BRIDGE_ICONS, key) ? BRIDGE_ICONS[key] : null);
}
