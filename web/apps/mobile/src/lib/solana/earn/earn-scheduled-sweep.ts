import {
  parseUnsignedRawTokenAmount,
  type LoadedEarnAutodepositScheduledSweep,
} from "@loyal-labs/shared";

const USDC_RAW_SCALE = BigInt(1_000_000);

export function formatScheduledSweepAmount(rawAmount: string): string {
  const raw = parseUnsignedRawTokenAmount(rawAmount);
  if (raw === null) {
    return "0.00 USDC";
  }
  const whole = raw / USDC_RAW_SCALE;
  const cents = (raw % USDC_RAW_SCALE) / BigInt(10_000);
  return `${Number(whole).toLocaleString("en-US")}.${cents
    .toString()
    .padStart(2, "0")} USDC`;
}

export function formatScheduledSweepTime(eligibleAfter: string): string {
  const date = new Date(eligibleAfter);
  if (Number.isNaN(date.getTime())) {
    return "Scheduled";
  }
  const hh = date.getHours().toString().padStart(2, "0");
  const mm = date.getMinutes().toString().padStart(2, "0");
  return `${relativeDayLabel(date)} at ${hh}:${mm}`;
}

// Subtitle + button copy for a sweep's in-flight/retry states, mirroring the
// web pane's `getAutodepositProgressLabel`/`getAutodepositProgressButtonLabel`
// exactly. Prefers the granular progress-poll state; falls back to the slot
// status so labels still upgrade ("Queued", "Preparing…") against a backend
// without the progress endpoint. Null fields mean "nothing special" — the row
// falls back to the scheduled time / "Executing…" / "Execute now".
export function getScheduledSweepStatusLabels(
  sweep: LoadedEarnAutodepositScheduledSweep,
  progressState: string | null | undefined,
): { subtitle: string | null; button: string | null } {
  switch (progressState) {
    case "requested":
      return { subtitle: "Queued", button: "Queued" };
    case "selected":
      return { subtitle: "Preparing deposit", button: "Preparing…" };
    case "pull_confirmed":
      return { subtitle: "Funds moved — depositing", button: "Depositing…" };
    case "completed":
      return { subtitle: "Autodeposit complete", button: "Complete" };
    case "failed":
      return { subtitle: "Autodeposit failed", button: "Try again" };
    case "canceled":
      return { subtitle: "Autodeposit canceled", button: "Try again" };
    case "released":
      return { subtitle: "Ready to retry", button: "Try again" };
    default:
      break;
  }
  switch (sweep.status) {
    case "requested":
      return { subtitle: "Queued", button: "Queued" };
    case "selected":
      return { subtitle: "Preparing deposit", button: "Preparing…" };
    case "failed":
    case "released":
      return { subtitle: "Retry needed", button: "Try again" };
    default:
      return { subtitle: null, button: null };
  }
}

export function isScheduledSweepDue(
  sweep: LoadedEarnAutodepositScheduledSweep,
): boolean {
  const eligibleAt = new Date(sweep.eligibleAfter).getTime();
  return !Number.isNaN(eligibleAt) && eligibleAt <= Date.now();
}

function relativeDayLabel(date: Date): string {
  const startOfDay = (d: Date) =>
    new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const dayMs = 24 * 60 * 60 * 1000;
  const diffDays = Math.round((startOfDay(date) - startOfDay(new Date())) / dayMs);
  if (diffDays <= 0) {
    return "Today";
  }
  if (diffDays === 1) {
    return "Tomorrow";
  }
  return date.toLocaleDateString("en-US", { day: "numeric", month: "short" });
}
