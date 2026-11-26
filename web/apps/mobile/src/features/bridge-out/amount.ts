import { toRaw } from "@loyal-labs/api";
import { formatUnits } from "viem/utils";

const usdcInput = /^(?:\d{1,20}(?:\.\d{0,6})?|\.\d{1,6})$/;
const maxRaw = BigInt("18446744073709551615");

export function usdcRaw(text: string): bigint {
  if (!usdcInput.test(text)) return BigInt(0);
  const raw = toRaw(text, 6);
  return raw <= maxRaw ? raw : BigInt(0);
}

export function usdcForDollars(text: string, priceUsd: number | null): string {
  if (
    !usdcInput.test(text) ||
    priceUsd === null ||
    !Number.isFinite(priceUsd) ||
    priceUsd <= 0
  )
    return "";
  const price = String(priceUsd);
  if (!/^\d+(?:\.\d+)?$/.test(price)) return "";
  const [whole, fraction = ""] = price.split(".");
  const divisor = BigInt(whole + fraction);
  const numerator = toRaw(text, 6) * BigInt(10) ** BigInt(fraction.length);
  const raw = (numerator + divisor / BigInt(2)) / divisor;
  return raw <= maxRaw ? formatUnits(raw, 6) : "";
}
