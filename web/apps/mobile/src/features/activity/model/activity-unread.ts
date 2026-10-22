import { mmkv } from "@/lib/storage";

export type ActivitySection = "wallet" | "earn" | "earn-max";

const lastSeenKey = (section: ActivitySection): string =>
  `activity:lastSeen:${section}`;

export function getActivityLastSeen(
  section: ActivitySection
): number | undefined {
  return mmkv.getNumber(lastSeenKey(section));
}

export function setActivityLastSeen(
  section: ActivitySection,
  timestamp: number
): void {
  mmkv.setNumber(lastSeenKey(section), timestamp);
}
