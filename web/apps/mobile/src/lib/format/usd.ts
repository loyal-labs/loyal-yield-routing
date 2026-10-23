// Round to integer cents BEFORE splitting: Kamino's floor-division values a
// fresh $4 deposit at 3.999999, and trunc-then-round-cents rendered that as
// whole "3" + cents "100" — "$3.100" on screen, read as a drop to "3.1".
export function splitUsdCents(value: number): { whole: string; cents: string } {
  const safe = Number.isFinite(value) ? value : 0;
  const totalCents = Math.round(safe * 100);
  const whole = Math.trunc(totalCents / 100);
  const cents = totalCents - whole * 100;
  return {
    whole: `$${whole.toLocaleString("en-US")}`,
    cents: `.${cents.toString().padStart(2, "0")}`,
  };
}
