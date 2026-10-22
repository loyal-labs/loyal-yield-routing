export function splitUsd(value: number): { whole: string; cents: string } {
  const safe = Number.isFinite(value) ? value : 0;
  const [whole, cents] = safe.toFixed(2).split(".");
  return {
    whole: `$${Number(whole).toLocaleString("en-US")}`,
    cents: `.${cents}`,
  };
}

export function formatApyBps(bps: number): string {
  const pct = bps / 100;
  return `${pct.toLocaleString("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}%`;
}
