import type { EarnTransactionItem } from "./earn-api";

export function getEarnTransactionAmountColor(
  kind: EarnTransactionItem["kind"],
): string {
  return kind === "deposit" || kind === "balance_sweep" ? "#34C759" : "#000000";
}

export function earnTransactionTimestampMs(
  item: Pick<EarnTransactionItem, "confirmedAt" | "sortTimestamp">,
): number {
  const raw = item.confirmedAt ?? item.sortTimestamp;
  if (!raw) return 0;
  const ms = Date.parse(raw);
  return Number.isNaN(ms) ? 0 : ms;
}

// The backend's `timestamp`/`dateGroup` strings are formatted in the server's
// timezone; the Wallet feed renders in device-local time, so format Earn rows
// the same way from the raw `confirmedAt` (ISO). Falls back to the server string
// if the raw timestamp is missing/unparseable.
export function formatEarnRowTime(
  item: Pick<EarnTransactionItem, "confirmedAt" | "sortTimestamp" | "timestamp">,
): string {
  const ms = earnTransactionTimestampMs(item);
  if (ms === 0) return item.timestamp;
  return new Date(ms).toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit",
  });
}

function earnRowDateGroup(
  item: Pick<
    EarnTransactionItem,
    "confirmedAt" | "sortTimestamp" | "dateGroup"
  >,
): string {
  const ms = earnTransactionTimestampMs(item);
  if (ms === 0) return item.dateGroup;
  return new Date(ms).toLocaleDateString("en-US", {
    month: "long",
    day: "numeric",
  });
}

export type EarnTransactionGroup = {
  date: string;
  items: EarnTransactionItem[];
};

// Groups a pre-sorted (newest-first) list into consecutive date sections. Uses a
// device-local date label (not the server `dateGroup`) so the headers line up
// with the local per-row times — otherwise a tx near midnight could sit under a
// header a day off from its displayed time.
export function groupEarnTransactions(
  items: EarnTransactionItem[],
): EarnTransactionGroup[] {
  const groups: EarnTransactionGroup[] = [];
  for (const item of items) {
    const date = earnRowDateGroup(item);
    const last = groups[groups.length - 1];
    if (last && last.date === date) {
      last.items.push(item);
    } else {
      groups.push({ date, items: [item] });
    }
  }
  return groups;
}
